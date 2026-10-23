package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"slices"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/arjunvlama/toli/internal/a2aserver"
	"github.com/arjunvlama/toli/internal/api"
	"github.com/arjunvlama/toli/internal/backend"
	"github.com/arjunvlama/toli/internal/hook"
	"github.com/arjunvlama/toli/internal/mcp"
	"github.com/arjunvlama/toli/internal/store"
	"github.com/arjunvlama/toli/internal/token"
)

// daemonFlags are `toli daemon`'s flags.
type daemonFlags struct {
	mcpListen, adminListen, a2aListen, publicURL, logFile, path *string
}

func newDaemonFlags() (*flag.FlagSet, daemonFlags) {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	return fs, daemonFlags{
		mcpListen:   fs.String("mcp-listen", mcpAddr(), "address for the MCP server (agents)"),
		adminListen: fs.String("admin-listen", adminAddr(), "address for the control plane (TUI)"),
		a2aListen:   fs.String("a2a-listen", a2aAddr(), `address for the A2A server (peers); "" to only delegate, never receive`),
		publicURL: fs.String("public-url", os.Getenv("TOLI_PUBLIC_URL"),
			"URL peers use to reach the A2A server (default http://<a2a-listen>)"),
		logFile: fs.String("log-file", "", "append logs to this file instead of stderr"),
		path:    fs.String("path", "", "the PATH to find programs such as codex on (toli daemon install sets it on Windows)"),
	}
}

// runDaemon runs the long-lived toli process: opens the store, serves the
// agent API (/mcp), the human API (/v1/), and the A2A server peers call
// (/a2a/), each on its own port, and shuts down cleanly on SIGINT/SIGTERM.
// `toli daemon install|...` manages it as an always-on service instead.
func runDaemon(args []string) error {
	if len(args) > 0 && slices.Contains(serviceVerbs, args[0]) {
		return runService(args[0], args[1:])
	}
	fs, f := newDaemonFlags()
	if err := fs.Parse(args); errors.Is(err, flag.ErrHelp) {
		return nil
	} else if err != nil {
		return err
	}
	if *f.path != "" {
		os.Setenv("PATH", *f.path)
	}
	mcpListen, adminListen, a2aListen, publicURL := f.mcpListen, f.adminListen, f.a2aListen, f.publicURL
	if *publicURL == "" && *a2aListen != "" {
		*publicURL = "http://" + *a2aListen
	}
	if *f.logFile != "" {
		lf, err := os.OpenFile(*f.logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer lf.Close()
		log.SetOutput(lf) // slog's default handler writes through log
		detachConsole()
	}

	if daemonAlreadyRunning(*adminListen) {
		return fmt.Errorf("a toli daemon is already running (%s); `toli daemon status` shows it and `toli daemon restart` restarts it", *adminListen)
	}

	home, err := homeDir()
	if err != nil {
		return err
	}

	st, err := store.Open(context.Background(), filepath.Join(home, "toli.db"))
	if err != nil {
		return err
	}
	defer st.Close() // runs after g.Wait: servers and background work have stopped

	tok, err := loadOrCreateAdminToken(home)
	if err != nil {
		return err
	}
	adminHash := token.Hash(tok)

	b, err := backend.New(context.Background(), st, backend.Options{})
	if err != nil {
		return err
	}

	mcpMux := http.NewServeMux()
	h := mcp.New(b)
	mcpMux.Handle("/mcp", h)
	mcpMux.Handle("/mcp/", h)
	mcpMux.Handle("/hook/", hook.Handler(b))

	adminMux := http.NewServeMux()
	adminMux.Handle("/v1/", api.New(b, adminHash, *publicURL))

	// Requests run under reqCtx, canceled at shutdown: Shutdown alone waits for
	// in-flight requests, and a get_delegated_task_updates long poll can wait 2 minutes.
	reqCtx, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	base := func(net.Listener) context.Context { return reqCtx }
	const readHeader = 10 * time.Second
	mcpSrv := &http.Server{Addr: *mcpListen, Handler: rejectBrowsers(mcpMux), BaseContext: base, ReadHeaderTimeout: readHeader}
	adminSrv := &http.Server{Addr: *adminListen, Handler: rejectBrowsers(adminMux), BaseContext: base, ReadHeaderTimeout: readHeader}
	servers := []*http.Server{mcpSrv, adminSrv}
	serving := []string{"agents (MCP)", "TUI (admin)"}
	if *a2aListen != "" {
		// No rejectBrowsers: Agent Cards are meant to be fetchable from anywhere,
		// and the A2A endpoints are protected by peer tokens instead.
		servers = append(servers, &http.Server{
			Addr: *a2aListen, Handler: a2aserver.New(b, *publicURL), BaseContext: base,
			ReadHeaderTimeout: readHeader, ReadTimeout: time.Minute, IdleTimeout: 2 * time.Minute,
		})
		serving = append(serving, "peers (A2A), public URL "+*publicURL)
	}

	agents, err := b.ListAgents(context.Background())
	if err != nil {
		return err
	}
	slog.Info("toli daemon starting", "version", version(), "home", home, "agents", len(agents))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	g, ctx := errgroup.WithContext(ctx)
	// Bind the listeners before starting background work, so a bind failure
	// stops with a clear error instead of racing peer checks that log warnings.
	listeners := make([]net.Listener, len(servers))
	for i, srv := range servers {
		ln, err := net.Listen("tcp", srv.Addr)
		if err != nil {
			return fmt.Errorf("listen on %s: %w", srv.Addr, err)
		}
		listeners[i] = ln
		slog.Info("listening", "for", serving[i], "addr", ln.Addr().String())
	}
	for i, srv := range servers {
		srv, ln := srv, listeners[i]
		g.Go(func() error { return serveOn(srv, ln) })
	}
	g.Go(func() error {
		return b.Run(ctx)
	})
	g.Go(func() error {
		<-ctx.Done() // wait for sigterm, or a server failing
		slog.Info("stopping")
		cancelRequests()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var errs []error
		for _, srv := range servers {
			errs = append(errs, srv.Shutdown(shutdownCtx))
		}
		return errors.Join(errs...)
	})
	if err := g.Wait(); err != nil {
		return err
	}
	slog.Info("stopped")
	return nil
}

// serveOn serves srv on an already-bound listener.
func serveOn(srv *http.Server, ln net.Listener) error {
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// daemonAlreadyRunning reports whether something already answers on the admin
// address, so a second `toli daemon` can stop with a clear message instead of
// failing to bind and printing peer-check warnings.
func daemonAlreadyRunning(adminListen string) bool {
	c, err := net.DialTimeout("tcp", adminListen, 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// version is toli's module version ("(devel)" when built from a checkout).
func version() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		return info.Main.Version
	}
	return "unknown"
}

// rejectBrowsers refuses requests carrying an Origin header. Browsers always
// send one on cross-origin requests; agents and the CLI/TUI don't. This blocks
// DNS-rebinding and drive-by requests from web pages to localhost.
func rejectBrowsers(next http.Handler) http.Handler {

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" {
			http.Error(w, "browser requests are not allowed", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

package main

import (
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/arjunvlama/toli/internal/service"
)

// serviceVerbs are the `toli daemon` subcommands that manage it as a service.
var serviceVerbs = []string{"install", "uninstall", "start", "stop", "restart", "status"}

// runService is `toli daemon install [daemon flags]`, `uninstall`, `start`,
// `stop`, `restart` and `status`: the daemon as an always-on service for
// this user (see internal/service).
func runService(verb string, args []string) error {
	if verb != "install" && len(args) > 0 {
		return fmt.Errorf("`toli daemon %s` takes no arguments", verb)
	}
	if verb == "install" {
		// Check them now, not when the service starts.
		if err := func() error { fs, _ := newDaemonFlags(); return fs.Parse(args) }(); err != nil {
			return err
		}
	}
	m, err := serviceManager(args)
	if err != nil {
		return err
	}

	switch verb {
	case "install":
		if err := m.Install(); err != nil {
			return err
		}
		ok("Installed the toli daemon. It starts at login and restarts if it stops.")
		fmt.Println("Logs: " + m.Logs())
	case "uninstall":
		if err := m.Uninstall(); err != nil {
			return err
		}
		ok("Uninstalled the toli daemon.")
	case "start":
		if err := m.Start(); err != nil {
			return err
		}
		ok("Started the toli daemon.")
	case "stop":
		if err := m.Stop(); err != nil {
			return err
		}
		ok("Stopped the toli daemon.")
	case "restart":
		if err := m.Restart(); err != nil {
			return err
		}
		ok("Restarted the toli daemon.")
	case "status":
		installed, where := m.Installed()
		inst := "no (run `toli daemon install`)"
		if installed {
			inst = "yes (" + where + ")"
		}
		fmt.Printf("installed  %s\nrunning    %s\nlogs       %s\n", inst, listening(), m.Logs())
	}
	return nil
}

// serviceManager returns the service for this OS, running toli daemon with
// args, and this user's PATH (the daemon runs commands such as codex and
// openclaw to deliver notes) and toli settings.
func serviceManager(args []string) (service.Manager, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if strings.Contains(exe, "go-build") {
		return nil, fmt.Errorf("%s is a temporary build (`go run`); install from a built toli binary", exe)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	env := map[string]string{"PATH": os.Getenv("PATH")}
	for _, kv := range os.Environ() {
		if k, v, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "TOLI_") {
			env[k] = v
		}
	}
	c := service.Config{
		Exe:     exe,
		Args:    append([]string{"daemon"}, args...),
		Env:     env,
		Home:    home,
		LogDir:  filepath.Join(home, "Library", "Logs", "toli"),
		UID:     os.Getuid(),
		TempDir: os.TempDir(),
		Run:     service.Run,
	}
	if runtime.GOOS == "windows" {
		cache, err := os.UserCacheDir() // %LocalAppData%
		if err != nil {
			return nil, err
		}
		c.LogDir = filepath.Join(cache, "toli")
		u, err := user.Current()
		if err != nil {
			return nil, err
		}
		c.User = u.Username
	}
	return service.For(runtime.GOOS, c)
}

// listening says whether a daemon answers on the MCP and admin addresses.
func listening() string {
	var down []string
	for _, addr := range []string{mcpAddr(), adminAddr()} {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			down = append(down, addr)
			continue
		}
		conn.Close()
	}
	if len(down) > 0 {
		return "no (nothing on " + strings.Join(down, ", ") + ")"
	}
	return "yes (" + mcpAddr() + ", " + adminAddr() + ")"
}

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// Default daemon addresses. MCP and admin are localhost only: never bind
// them to a public interface. They're separate ports so an agent sandbox can
// allow MCP and block the control plane. A2A is what peers call; it's also
// localhost by default, and people expose it themselves (Tailscale, a tunnel).
const (
	defaultMCPAddr   = "127.0.0.1:7420" // agents: /mcp, agent tokens
	defaultAdminAddr = "127.0.0.1:7421" // humans: /v1/, admin token
	defaultA2AAddr   = "127.0.0.1:7422" // peers: /a2a/<agent>/, peer tokens
)

// mcpAddr is the daemon's MCP address: $TOLI_MCP_ADDR or defaultMCPAddr.
func mcpAddr() string {
	addr := os.Getenv("TOLI_MCP_ADDR")
	if addr == "" {
		return defaultMCPAddr
	}
	return addr
}

// adminAddr is the daemon's control-plane address: $TOLI_ADMIN_ADDR or
// defaultAdminAddr.
func adminAddr() string {
	addr := os.Getenv("TOLI_ADMIN_ADDR")
	if addr == "" {
		return defaultAdminAddr
	}
	return addr
}

// a2aAddr is the daemon's A2A address: $TOLI_A2A_ADDR or defaultA2AAddr.
func a2aAddr() string {
	addr := os.Getenv("TOLI_A2A_ADDR")
	if addr == "" {
		return defaultA2AAddr
	}
	return addr
}

func main() {
	var err error
	switch {
	case len(os.Args) < 2:
		err = runTUI()
	case os.Args[1] == "daemon":
		err = runDaemon(os.Args[2:])
	case os.Args[1] == "hook":
		err = runHook(os.Args[2:])
	case os.Args[1] == "connect":
		err = runConnect(os.Args[2:], false)
	case os.Args[1] == "disconnect":
		err = runConnect(os.Args[2:], true)
	case os.Args[1] == "agent":
		err = runAgent(os.Args[2:])
	case os.Args[1] == "peer":
		err = runPeer(os.Args[2:])
	case os.Args[1] == "task":
		err = runTask(os.Args[2:])
	case os.Args[1] == "help" || os.Args[1] == "--help" || os.Args[1] == "-h":
		usage(os.Stdout)
	case os.Args[1] == "version" || os.Args[1] == "--version":
		fmt.Println("toli", version())
	default:
		usage(os.Stderr)
		os.Exit(2)
	}
	if err != nil {
		if !errors.Is(err, errReported) {
			fmt.Fprintln(os.Stderr, "toli:", err)
		}
		os.Exit(1)
	}
}

// isHelp reports whether args ask for a command's help.
func isHelp(args []string) bool {
	return len(args) == 1 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help")
}

// usage lists the commands: on stdout when asked for, on stderr after a
// mistake.
func usage(w io.Writer) {
	fmt.Fprintln(w, `usage:
  toli                                                    open the TUI (daemon must be running)
  toli daemon [--mcp-listen ADDR] [--admin-listen ADDR]   run the daemon (in the foreground)
              [--a2a-listen ADDR] [--public-url URL]
  toli daemon install [daemon flags]                      run it always: at login, restarted if it stops
  toli daemon uninstall|start|stop|restart|status         manage that
  toli connect HARNESS AGENT [--scope project|user]       set a coding agent up as AGENT (makes a new MCP URL)
  toli connect HARNESS --mcp-url URL [--scope ...]        the same, with an MCP URL you have
  toli disconnect HARNESS [--scope project|user]          undo toli connect
  toli agent add AGENT [--description TEXT] [--skills FILE] [--connect HARNESS]
                                                          add one of your agents; with --connect, also connect a harness to it here
  toli agent edit AGENT [--description TEXT] [--skills FILE]
                                                          change its description or skills
  toli agent new-token AGENT                              print a new MCP URL for AGENT (the old one stops working)
  toli agent notify AGENT --command PATH | --webhook URL | --none
                                                          push AGENT's notifications when it has no harness session
  toli agent disable|enable|remove AGENT                  pause it (it and its peers are refused), resume it, or delete it
  toli agent list                                         your agents
  toli peer invite AGENT PEER                             an invite for another agent to pair with AGENT as PEER
  toli peer accept AGENT INVITE [--name NAME]             pair AGENT with the agent that made INVITE
  toli peer add AGENT PEER CARD_URL [--token TOKEN]       let AGENT work with another agent; prints the token to give it
  toli peer token AGENT PEER TOKEN                        set the token the peer gave you
  toli peer new-token AGENT PEER                          a new token to give PEER (the old one stops working)
  toli peer remove AGENT PEER                             AGENT stops working with PEER; its running tasks are canceled
  toli peer list AGENT                                    AGENT's peers, and whether they're connected
  toli task list AGENT [--assigned]                       AGENT's delegated tasks (or the ones peers gave it)
  toli task show AGENT ID [--assigned]                    a task's state and conversation
  toli task cancel AGENT ID                               cancel a task AGENT delegated
  toli hook HARNESS EVENT MCP_URL                         run by a coding agent's hooks (see toli connect)
  toli version                                            toli's version`)
}

// errReported: the command failed and has already said how.
var errReported = errors.New("reported")

// failed reports a step that failed after others succeeded, as gh does: an
// X line and what to run once it's fixed, on stderr.
func failed(next, format string, args ...any) error {
	fmt.Fprintf(os.Stderr, "X "+format+"\n", args...)
	fmt.Fprintf(os.Stderr, "Next: %s\n", next)
	return errReported
}

// ok reports that a command did what was asked: one line, starting with ✓.
func ok(format string, args ...any) {
	fmt.Printf("✓ "+format+"\n", args...)
}

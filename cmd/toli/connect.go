package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/arjunvlama/toli/internal/connect"
	"github.com/arjunvlama/toli/internal/hook"
)

// runConnect is `toli connect <harness> <agent>` (a new MCP URL for the
// agent), or `toli connect <harness> --mcp-url <url>`; with disconnect,
// `toli disconnect <harness>`. A URL in place of the agent still works.
func runConnect(args []string, disconnect bool) error {
	cmd := "connect"
	if disconnect {
		cmd = "disconnect"
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	scope := fs.String("scope", connect.Project, "project (sessions started in this directory) or user (all sessions)")
	mcpURL := fs.String("mcp-url", "", "the agent's MCP URL, instead of making a new one")
	if isHelp(args) {
		fmt.Println(connectUsage)
		return nil
	}
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return errors.New(connectUsage)
	}
	harness, rest, agent := args[0], args[1:], ""
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		agent, rest = rest[0], rest[1:]
	}
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return errors.New(connectUsage)
	}
	if strings.Contains(agent, "://") { // the older form: an MCP URL
		agent, *mcpURL = "", agent
	}
	if agent != "" && *mcpURL != "" {
		return errors.New("give an agent or `--mcp-url`, not both")
	}
	if !slices.Contains(connect.Harnesses(), harness) {
		return fmt.Errorf("unknown harness %q (want one of: %s); for other agents, see the docs", harness, strings.Join(connect.Harnesses(), ", "))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	replaced := false // a running session used the agent's old MCP URL
	switch {
	case *mcpURL != "":
		a, err := hook.Agent(ctx, *mcpURL)
		if err != nil && !disconnect {
			return fmt.Errorf("check the MCP URL: %w", err)
		}
		if err != nil {
			// The agent may be gone; its harness config should still go.
			fmt.Fprintf(os.Stderr, "toli: check the MCP URL: %v (disconnecting anyway)\n", err)
		}
		agent = a
	case agent != "" && !disconnect:
		var err error
		if *mcpURL, replaced, err = newMCPURL(ctx, agent); err != nil {
			return err
		}
	case !disconnect:
		return errors.New(connectUsage)
	}
	return doConnect(ctx, harness, agent, *mcpURL, *scope, disconnect, replaced)
}

// doConnect connects (or disconnects) harness in scope to the agent behind
// mcpURL, and prints what it did. replaced: the agent's old MCP URL was in use.
func doConnect(ctx context.Context, harness, agent, mcpURL, scope string, disconnect, replaced bool) error {
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}

	register := func(h, session string) error {
		_, err := hook.Register(ctx, mcpURL, h, session)
		return err
	}
	unregister := func(session string) error {
		if mcpURL == "" {
			return nil // the daemon drops sessions that are gone by itself
		}
		return hook.Unregister(ctx, mcpURL, session)
	}
	opts := connect.Options{
		MCPURL: mcpURL, Scope: scope, Dir: dir, Home: home, Toli: self, Run: run, Register: register, Unregister: unregister,
	}
	do, verb := connect.Connect, "Connected %s to %s"
	if disconnect {
		do, verb = connect.Disconnect, "Disconnected %s from %s"
	}
	done, err := do(harness, opts)
	if err != nil {
		return err
	}
	summary := fmt.Sprintf(verb, harnessName(harness), "agent '"+agent+"'")
	if agent == "" {
		summary = strings.TrimSuffix(summary, " from agent ''")
	}
	if replaced {
		done = append([]connect.Done{{What: fmt.Sprintf("Made a new MCP URL for agent '%s': sessions using the old one are disconnected.", agent)}}, done...)
	}
	printConnect(summary, harness, scope, dir, home, done)
	return nil
}

const connectUsage = `usage:
  toli connect HARNESS AGENT [--scope project|user]
  toli connect HARNESS --mcp-url URL [--scope project|user]
  toli disconnect HARNESS [--scope project|user]`

// newMCPURL makes a new MCP URL for agent, and says whether a session was
// registered with the old one.
func newMCPURL(ctx context.Context, agent string) (string, bool, error) {
	c, err := adminClient()
	if err != nil {
		return "", false, err
	}
	a, err := findAgent(ctx, c, agent)
	if err != nil {
		return "", false, err
	}
	rotated, err := c.RotateAgentToken(ctx, agent)
	if err != nil {
		return "", false, err
	}
	return "http://" + mcpAddr() + "/mcp/" + rotated.Token, a.Harness != "", nil
}

// printConnect prints what connect or disconnect did: one line naming what
// changed, then any problems, then the step left for the user.
func printConnect(summary, harness, scope, dir, home string, done []connect.Done) {
	if harness != "openclaw" { // OpenClaw's config is per user: no scope
		if scope == connect.User {
			summary += " for all your sessions"
		} else {
			summary += " in " + shortPath(dir, "", home)
		}
	}
	var files []string
	for _, d := range done {
		if f := shortPath(d.File, dir, home); f != "" && !slices.Contains(files, f) {
			files = append(files, f)
		}
	}
	if len(files) > 0 {
		summary += " (" + strings.Join(files, ", ") + ")"
	}
	ok("%s.", summary)
	for _, d := range done {
		if d.What != "" {
			fmt.Println("! " + d.What)
		}
		if d.Manual != "" {
			fmt.Println("  Run: " + d.Manual)
		}
	}
	for _, d := range done {
		if d.Next != "" {
			fmt.Println("Next: " + d.Next)
		}
	}
}

// harnessName is a harness's name as people write it.
func harnessName(harness string) string {
	switch harness {
	case "claude-code":
		return "Claude Code"
	case "codex":
		return "Codex"
	case "opencode":
		return "OpenCode"
	case "openclaw":
		return "OpenClaw"
	case "pi":
		return "Pi"
	}
	return harness
}

// shortPath shows path relative to dir if it's inside it, else with ~ for
// home.
func shortPath(path, dir, home string) string {
	if path == "" || strings.HasPrefix(path, "~") {
		return path
	}
	if rel, err := filepath.Rel(dir, path); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	if rel, err := filepath.Rel(home, path); err == nil && !strings.HasPrefix(rel, "..") {
		return "~/" + rel
	}
	return path
}

// run runs a command in dir, quietly.
func run(dir, name string, args ...string) error {
	path, err := exec.LookPath(name)
	if err != nil {
		return err
	}
	cmd := exec.Command(path, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(out) > 0 {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return err
}

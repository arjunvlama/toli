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

// runConnect is `toli connect <harness> <MCP URL> [--scope project|user]`,
// or with disconnect, `toli disconnect` (same arguments).
func runConnect(args []string, disconnect bool) error {
	cmd := "connect"
	if disconnect {
		cmd = "disconnect"
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	scope := fs.String("scope", connect.Project, "project (sessions started in this directory) or user (all sessions)")
	if len(args) < 2 {
		return fmt.Errorf("usage: toli %s <%s> <MCP URL> [--scope project|user]", cmd, strings.Join(connect.Harnesses(), "|"))
	}
	harness, mcpURL := args[0], args[1]
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	agent, err := hook.Agent(ctx, mcpURL)
	if err != nil && !disconnect {
		return fmt.Errorf("check the MCP URL: %w", err)
	}
	if err != nil {
		// The agent may be gone; its harness config should still go.
		fmt.Fprintf(os.Stderr, "toli: check the MCP URL: %v (disconnecting anyway)\n", err)
		agent = "?"
	}
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
		return hook.Unregister(ctx, mcpURL, session)
	}
	opts := connect.Options{
		MCPURL: mcpURL, Scope: *scope, Dir: dir, Home: home, Toli: self, Run: run, Register: register, Unregister: unregister,
	}
	do, verb := connect.Connect, "Connected %s to %s"
	if disconnect {
		do, verb = connect.Disconnect, "Disconnected %s from %s"
	}
	done, err := do(harness, opts)
	if err != nil {
		return err
	}
	summary := fmt.Sprintf(verb, harnessName(harness), agent)
	if agent == "?" { // the MCP URL didn't name an agent (disconnecting anyway)
		summary = strings.TrimSuffix(summary, " from ?")
	}
	printConnect(summary, harness, *scope, dir, home, done)
	return nil
}

// printConnect prints what connect or disconnect did: one line naming what
// changed, then any problems, then the step left for the user.
func printConnect(summary, harness, scope, dir, home string, done []connect.Done) {
	if harness != "openclaw" { // OpenClaw's config is per user: no scope
		if scope == connect.User {
			summary += " for all your sessions"
		} else {
			summary += " in this folder"
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

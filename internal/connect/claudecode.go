package connect

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
)

// claudeCode is Claude Code.
type claudeCode struct{}

// files: the MCP server's scope and the settings file, for o.Scope.
// Local scope is private to the user, for this directory; settings go to
// .claude/settings.local.json in the project (not committed), or
// ~/.claude/settings.json.
func (claudeCode) files(o Options) (mcpScope, settings string) {
	if o.Scope == User {
		return "user", filepath.Join(o.Home, ".claude", "settings.json")
	}
	return "local", filepath.Join(o.Dir, ".claude", "settings.local.json")
}

// connect adds the MCP server with Claude Code's own CLI, and the
// hooks to Claude Code's settings.
func (c claudeCode) connect(o Options) ([]Done, error) {
	var done []Done
	mcpScope, settings := c.files(o)

	add := []string{"mcp", "add", "--transport", "http", "--scope", mcpScope, "toli", o.MCPURL}
	o.Run(o.Dir, "claude", "mcp", "remove", "--scope", mcpScope, "toli") // replace any earlier one; fails if there's none
	switch err := o.Run(o.Dir, "claude", add...); {
	case errors.Is(err, exec.ErrNotFound):
		return nil, errors.New("claude isn't on your PATH: install Claude Code's CLI (or open a new terminal), then retry")
	case err != nil:
		return nil, fmt.Errorf("claude mcp add: %w", err)
	default:
		done = append(done, Done{File: "~/.claude.json"})
	}

	hooks := map[string]string{
		"SessionStart": shellJoin([]string{o.Toli, "hook", "claude-code", "session-start", o.MCPURL}),
		"SessionEnd":   shellJoin([]string{o.Toli, "hook", "claude-code", "session-end", o.MCPURL}),
	}
	// toli's tools only talk to the daemon, so they run without asking:
	// otherwise an unattended session stops at the first one.
	if err := setHooks(settings, hooks, []string{"mcp__toli"}); err != nil {
		return nil, err
	}
	done = append(done, Done{File: settings})

	if o.Scope == Project {
		ignored, err := gitIgnore(o, settings)
		if err != nil {
			return nil, err
		}
		if ignored {
			done = append(done, Done{File: ".gitignore"})
		}
	}
	return append(done, Done{Next: "start a Claude Code session, or restart a running one (`claude --continue`)."}), nil
}

// disconnect removes the MCP server, toli's hooks and its tools'
// permission.
func (c claudeCode) disconnect(o Options) ([]Done, error) {
	var done []Done
	mcpScope, settings := c.files(o)

	remove := []string{"mcp", "remove", "--scope", mcpScope, "toli"}
	switch err := o.Run(o.Dir, "claude", remove...); {
	case errors.Is(err, exec.ErrNotFound):
		done = append(done, Done{What: "claude isn't on your PATH, so toli couldn't remove its MCP server.", Manual: "claude " + shellJoin(remove)})
	case err != nil:
		done = append(done, Done{What: fmt.Sprintf("Couldn't remove the MCP server (%s scope): %v", mcpScope, err)})
	default:
		done = append(done, Done{File: "~/.claude.json"})
	}

	if err := removeHooks(settings, []string{"mcp__toli"}); err != nil {
		return nil, err
	}
	return append(done,
		Done{File: settings},
		Done{Next: "restart running sessions; they stay connected until then."},
	), nil
}

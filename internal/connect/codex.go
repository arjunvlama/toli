package connect

import (
	"fmt"
	"path/filepath"
)

// codex is OpenAI's Codex CLI.
type codex struct{}

// files: Codex's config and hooks files, for o.Scope: the project's
// .codex/ (Codex loads them only in folders you trust), or ~/.codex/.
func (codex) files(o Options) (config, hooks string) {
	dir := filepath.Join(o.Home, ".codex")
	if o.Scope == Project {
		dir = filepath.Join(o.Dir, ".codex")
	}
	return filepath.Join(dir, "config.toml"), filepath.Join(dir, "hooks.json")
}

// connect writes the MCP server and hooks to Codex's config. Codex has
// no uncommitted project file, so in a project both files are gitignored,
// and connect refuses if git already tracks either (a shared file would
// commit the agent's token).
func (c codex) connect(o Options) ([]Done, error) {
	var done []Done
	config, hooksFile := c.files(o)
	if o.Scope == Project {
		for _, f := range []string{config, hooksFile} {
			if o.Run(o.Dir, "git", "ls-files", "--error-unmatch", f) == nil {
				return nil, fmt.Errorf("git tracks %s, so the agent's token would be committed; use `--scope user`", f)
			}
		}
	}
	// toli's tools only talk to the daemon, so they run without asking:
	// otherwise an unattended session stops at the first one.
	server := fmt.Sprintf("url = %q\ndefault_tools_approval_mode = \"approve\"", o.MCPURL)
	if err := setTOMLTable(config, "mcp_servers.toli", server); err != nil {
		return nil, err
	}
	done = append(done, Done{File: config})

	hooks := map[string]string{
		"SessionStart": shellJoin([]string{o.Toli, "hook", "codex", "session-start", o.MCPURL}),
		"SessionEnd":   shellJoin([]string{o.Toli, "hook", "codex", "session-end", o.MCPURL}),
	}
	if err := setHooks(hooksFile, hooks, nil); err != nil {
		return nil, err
	}
	done = append(done, Done{File: hooksFile})

	if o.Scope == Project {
		for _, f := range []string{config, hooksFile} {
			ignored, err := gitIgnore(o, f)
			if err != nil {
				return nil, err
			}
			if ignored {
				done = append(done, Done{File: ".gitignore"})
			}
		}
	}
	return append(done,
		Done{Next: "start Codex (or restart a running session with `codex resume --last`), and when it asks, trust the folder and toli's hooks."},
	), nil
}

// disconnect removes the MCP server and toli's hooks from Codex's
// config.
func (c codex) disconnect(o Options) ([]Done, error) {
	config, hooksFile := c.files(o)
	if err := setTOMLTable(config, "mcp_servers.toli", ""); err != nil {
		return nil, err
	}
	if err := removeHooks(hooksFile, nil); err != nil {
		return nil, err
	}
	return []Done{
		{File: config},
		{File: hooksFile},
		{Next: "restart running sessions; they stay connected until then."},
	}, nil
}

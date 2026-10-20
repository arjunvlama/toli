package connect

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

//go:embed opencode_plugin.js
var opencodePlugin string

// opencode is OpenCode.
type opencode struct{}

// files: OpenCode's config file and toli's plugin, for o.Scope: the
// project's opencode.json and .opencode/plugins/, or ~/.config/opencode/.
func (opencode) files(o Options) (config, plugin string) {
	if o.Scope == Project {
		return filepath.Join(o.Dir, "opencode.json"), filepath.Join(o.Dir, ".opencode", "plugins", "toli.js")
	}
	dir := filepath.Join(o.Home, ".config", "opencode")
	return filepath.Join(dir, "opencode.json"), filepath.Join(dir, "plugins", "toli.js")
}

// connect writes toli's plugin (it registers the session and
// delivers notes into it; OpenCode has no way in from outside) and the MCP
// server, with toli's tools allowed, to opencode.json. As with Codex, both
// are gitignored in a project, and connect refuses if git already tracks
// either.
func (c opencode) connect(o Options) ([]Done, error) {
	base, tok, err := splitMCPURL(o.MCPURL)
	if err != nil {
		return nil, err
	}
	config, plugin := c.files(o)
	if o.Scope == Project {
		for _, f := range []string{config, plugin} {
			if o.Run(o.Dir, "git", "ls-files", "--error-unmatch", f) == nil {
				return nil, fmt.Errorf("git tracks %s, so the agent's token would be committed; use `--scope user`", f)
			}
		}
	}
	if _, err := os.Stat(strings.TrimSuffix(config, ".json") + ".jsonc"); err == nil {
		return nil, fmt.Errorf("%s exists; `toli connect` only edits opencode.json (add the toli MCP server there by hand)", strings.TrimSuffix(config, ".json")+".jsonc")
	}

	if err := os.MkdirAll(filepath.Dir(plugin), 0o755); err != nil {
		return nil, err
	}
	src := strings.ReplaceAll(opencodePlugin, "{{HOOK_URL}}", base+"/hook/"+tok)
	if err := os.WriteFile(plugin, []byte(src), 0o600); err != nil {
		return nil, err
	}
	done := []Done{{File: plugin}}

	if err := setOpencodeConfig(config, o.MCPURL); err != nil {
		return nil, err
	}
	done = append(done, Done{File: config})

	if o.Scope == Project {
		for _, f := range []string{config, plugin} {
			ignored, err := gitIgnore(o, f)
			if err != nil {
				return nil, err
			}
			if ignored {
				done = append(done, Done{File: ".gitignore"})
			}
		}
	}
	return append(done, Done{Next: "start OpenCode, or restart a running one: plugins load when it starts."}), nil
}

// disconnect deletes toli's plugin and takes the MCP server and its
// tools' permission out of opencode.json.
func (c opencode) disconnect(o Options) ([]Done, error) {
	config, plugin := c.files(o)
	if err := os.Remove(plugin); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := setOpencodeConfig(config, ""); err != nil {
		return nil, err
	}
	return []Done{
		{File: plugin},
		{File: config},
		{Next: "restart running sessions; they stay connected until then."},
	}, nil
}

// setOpencodeConfig adds the toli MCP server and allows its tools in an
// OpenCode config file, keeping everything else; with mcpURL "", it takes
// them out.
func setOpencodeConfig(path, mcpURL string) error {
	cfg, err := readJSON(path)
	if err != nil || cfg == nil && mcpURL == "" {
		return err
	}
	if cfg == nil {
		cfg = map[string]any{"$schema": "https://opencode.ai/config.json"}
	}
	mcp, _ := cfg["mcp"].(map[string]any)
	perm, _ := cfg["permission"].(map[string]any) // a single permission for everything ("allow"/"ask") is the user's call
	if mcpURL == "" {
		delete(mcp, "toli")
		delete(perm, "toli_*")
		return writeJSON(path, cfg)
	}
	if mcp == nil {
		mcp = map[string]any{}
	}
	mcp["toli"] = map[string]any{"type": "remote", "url": mcpURL, "enabled": true}
	cfg["mcp"] = mcp
	// toli's tools only talk to the daemon, so they run without asking.
	switch {
	case perm != nil:
		perm["toli_*"] = "allow"
	case cfg["permission"] == nil:
		cfg["permission"] = map[string]any{"toli_*": "allow"}
	}
	return writeJSON(path, cfg)
}

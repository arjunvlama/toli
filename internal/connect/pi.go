package connect

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

//go:embed pi_extension.js
var piExtension string

// pi is Pi (pi.dev).
type pi struct{}

// extension: toli's extension file, for o.Scope: the project's
// .pi/extensions/ (Pi loads it once the folder is trusted), or
// ~/.pi/agent/extensions/.
func (pi) extension(o Options) string {
	if o.Scope == Project {
		return filepath.Join(o.Dir, ".pi", "extensions", "toli.js")
	}
	return filepath.Join(o.Home, ".pi", "agent", "extensions", "toli.js")
}

// connect writes toli's extension: it adds the MCP server
// (pi.registerMcpServer, so Pi's mcp.json isn't touched), registers the
// session and delivers notes into it. It holds the agent's token, so in a
// project it's gitignored, and connect refuses if git already tracks it.
func (p pi) connect(o Options) ([]Done, error) {
	base, tok, err := splitMCPURL(o.MCPURL)
	if err != nil {
		return nil, err
	}
	extension := p.extension(o)
	if o.Scope == Project && o.Run(o.Dir, "git", "ls-files", "--error-unmatch", extension) == nil {
		return nil, fmt.Errorf("git tracks %s, so the agent's token would be committed; use --scope user", extension)
	}
	if err := os.MkdirAll(filepath.Dir(extension), 0o755); err != nil {
		return nil, err
	}
	src := strings.NewReplacer("{{MCP_URL}}", o.MCPURL, "{{HOOK_URL}}", base+"/hook/"+tok).Replace(piExtension)
	if err := os.WriteFile(extension, []byte(src), 0o600); err != nil {
		return nil, err
	}
	done := []Done{{File: extension}}

	if o.Scope == Project {
		ignored, err := gitIgnore(o, extension)
		if err != nil {
			return nil, err
		}
		if ignored {
			done = append(done, Done{File: ".gitignore"})
		}
	}
	return append(done, Done{Next: "start pi (trust the folder if it asks), or run /reload in a running session."}), nil
}

// disconnect deletes toli's extension.
func (p pi) disconnect(o Options) ([]Done, error) {
	extension := p.extension(o)
	if err := os.Remove(extension); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return []Done{
		{File: extension},
		{Next: "run /reload in running sessions, or restart them; they stay connected until then."},
	}, nil
}

// Package connect is `toli connect` and `toli disconnect`: it sets a coding
// agent (harness) up to use one of our agents, writing its MCP server config
// and its hooks, in the same scope, and takes them out again. One file per
// harness. See ARCHITECTURE.md.
package connect

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Scopes.
const (
	Project = "project" // sessions started in Dir
	User    = "user"    // every session of the harness on this machine
)

// Options says what to connect, and where.
type Options struct {
	MCPURL string // the agent's MCP URL
	Scope  string // Project or User
	Dir    string // the project directory (Project scope)
	Home   string // the user's home directory
	Toli   string // absolute path to the toli binary, for the hooks
	// Run runs a command in dir (e.g. the claude CLI); exec.LookPath's
	// exec.ErrNotFound means it isn't installed.
	Run func(dir, name string, args ...string) error
	// Register registers a session with the daemon directly, for harnesses
	// whose sessions don't run hooks.
	Register func(harness, sessionID string) error
	// Unregister drops a session registered with Register.
	Unregister func(sessionID string) error
}

// Done is one result of connect or disconnect, of one kind: a file it wrote
// or changed (File), the step the user has to take next (Next), or a problem
// the user should know about (What), with the command to run if toli
// couldn't (Manual).
type Done struct {
	File   string
	Next   string
	What   string
	Manual string
}

// harness is how to connect one harness, and disconnect it.
type harness interface {
	connect(Options) ([]Done, error)
	disconnect(Options) ([]Done, error)
}

var harnesses = map[string]harness{
	"claude-code": claudeCode{},
	"codex":       codex{},
	"opencode":    opencode{},
	"openclaw":    openclaw{},
	"pi":          pi{},
}

// Harnesses lists the harnesses connect supports.
func Harnesses() []string {
	var names []string
	for name := range harnesses {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// Connect sets harness up to use the agent behind opts.MCPURL.
func Connect(name string, opts Options) ([]Done, error) {
	h, err := lookup(name, opts)
	if err != nil {
		return nil, err
	}
	return h.connect(opts)
}

// Disconnect undoes Connect, in the same scope.
func Disconnect(name string, opts Options) ([]Done, error) {
	h, err := lookup(name, opts)
	if err != nil {
		return nil, err
	}
	return h.disconnect(opts)
}

func lookup(name string, opts Options) (harness, error) {
	h, ok := harnesses[name]
	if !ok {
		return nil, fmt.Errorf("unknown harness %q (want one of: %s); for other agents, see the docs", name, strings.Join(Harnesses(), ", "))
	}
	if opts.Scope != Project && opts.Scope != User {
		return nil, fmt.Errorf("unknown scope %q (want %s or %s)", opts.Scope, Project, User)
	}
	return h, nil
}

// splitMCPURL splits http://host:port/mcp/<token> into the daemon's base
// URL and the token.
func splitMCPURL(raw string) (base, tok string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("MCP URL: %w", err)
	}
	tok, ok := strings.CutPrefix(u.Path, "/mcp/")
	if !ok || tok == "" {
		return "", "", fmt.Errorf("MCP URL %q: want http://host:port/mcp/<token>", raw)
	}
	return u.Scheme + "://" + u.Host, tok, nil
}

// setTOMLTable writes [name] with body into a TOML file, replacing that table
// if it's there and leaving the rest of the file as it is; with body "", it
// takes the table out.
func setTOMLTable(path, name, body string) error {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) && body == "" {
		return nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var kept []string
	in, found := false, false
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "[") {
			in = t == "["+name+"]"
			found = found || in
		}
		if !in {
			kept = append(kept, line)
		}
	}
	if !found && body == "" {
		return nil
	}
	out := strings.TrimRight(strings.Join(kept, "\n"), "\n")
	if body != "" {
		if out != "" {
			out += "\n\n"
		}
		out += "[" + name + "]\n" + body
	}
	out += "\n"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(out), 0o600)
}

// setHooks puts one command hook per event into a hooks settings file
// (Claude Code's or Codex's), replacing toli's earlier ones and keeping
// everything else. allow adds Claude Code permission rules
// (permissions.allow), if not there yet.
func setHooks(path string, hooks map[string]string, allow []string) error {
	settings, err := readJSON(path)
	if err != nil {
		return err
	}
	if settings == nil {
		settings = map[string]any{}
	}
	all, _ := settings["hooks"].(map[string]any)
	if all == nil {
		all = map[string]any{}
	}
	for event, command := range hooks {
		groups, _ := all[event].([]any)
		var kept []any
		for _, g := range groups {
			if g = withoutToli(g); g != nil {
				kept = append(kept, g)
			}
		}
		all[event] = append(kept, map[string]any{"hooks": []any{map[string]any{"type": "command", "command": command}}})
	}
	settings["hooks"] = all
	if len(allow) > 0 {
		perms, _ := settings["permissions"].(map[string]any)
		if perms == nil {
			perms = map[string]any{}
		}
		rules, _ := perms["allow"].([]any)
		for _, rule := range allow {
			if !slices.Contains(rules, any(rule)) {
				rules = append(rules, rule)
			}
		}
		perms["allow"] = rules
		settings["permissions"] = perms
	}
	return writeJSON(path, settings)
}

// removeHooks takes toli's hooks, and the permission rules in allow, out of a
// hooks settings file.
func removeHooks(path string, allow []string) error {
	settings, err := readJSON(path)
	if err != nil || settings == nil {
		return err
	}
	all, _ := settings["hooks"].(map[string]any)
	for event, groups := range all {
		groups, _ := groups.([]any)
		var kept []any
		for _, g := range groups {
			if g = withoutToli(g); g != nil {
				kept = append(kept, g)
			}
		}
		if len(kept) == 0 {
			delete(all, event)
		} else {
			all[event] = kept
		}
	}
	if perms, _ := settings["permissions"].(map[string]any); perms["allow"] != nil {
		rules, _ := perms["allow"].([]any)
		perms["allow"] = slices.DeleteFunc(rules, func(r any) bool {
			s, _ := r.(string)
			return slices.Contains(allow, s)
		})
	}
	return writeJSON(path, settings)
}

// readJSON reads a JSON object from a config file; nil if there's no file.
func readJSON(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return v, nil
}

// writeJSON writes a config file, readable only by the user (it holds the
// agent's token).
func writeJSON(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}

// withoutToli is a settings hook group with toli's hooks taken out; nil if
// nothing is left.
func withoutToli(group any) any {
	g, ok := group.(map[string]any)
	if !ok {
		return group
	}
	hooks, _ := g["hooks"].([]any)
	var kept []any
	for _, h := range hooks {
		if cmd, _ := h.(map[string]any)["command"].(string); isToliHook(cmd) {
			continue
		}
		kept = append(kept, h)
	}
	if len(kept) == 0 {
		return nil
	}
	g["hooks"] = kept
	return g
}

// isToliHook reports whether a hook command is one toli connect wrote: it
// runs a toli binary's `hook` command.
func isToliHook(cmd string) bool {
	prog, rest, _ := strings.Cut(cmd, " ")
	if strings.HasPrefix(cmd, "'") { // a quoted path, as shellJoin writes it
		end := strings.Index(cmd[1:], "' ")
		if end < 0 {
			return false
		}
		prog, rest = cmd[1:end+1], cmd[end+3:]
	}
	return strings.TrimSuffix(filepath.Base(prog), ".exe") == "toli" && strings.HasPrefix(rest, "hook ") // toli.exe on Windows
}

// gitIgnore adds path to the repo's .gitignore if the project is a git repo
// and git doesn't already ignore it. Reports whether it added it.
func gitIgnore(o Options, path string) (bool, error) {
	if o.Run(o.Dir, "git", "rev-parse", "--is-inside-work-tree") != nil {
		return false, nil // not a git repo (or no git)
	}
	if o.Run(o.Dir, "git", "check-ignore", "-q", path) == nil {
		return false, nil // already ignored
	}
	rel, err := filepath.Rel(o.Dir, path)
	if err != nil {
		return false, err
	}
	f, err := os.OpenFile(filepath.Join(o.Dir, ".gitignore"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "\n%s\n", filepath.ToSlash(rel)); err != nil {
		return false, err
	}
	return true, nil
}

// shellJoin quotes args for a shell command line.
func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		if a != "" && !strings.ContainsAny(a, " '\"\\$`!*?&;|<>()[]{}#~") {
			quoted[i] = a
			continue
		}
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}

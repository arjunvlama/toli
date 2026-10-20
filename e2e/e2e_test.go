// Package e2e runs toli end to end: two real daemons (the built binary), each
// with an agent, paired over A2A; the test plays both coding agents over MCP
// and collects notes as a connected agent would (/hook/). Real coding agents
// (Claude Code, Codex, ...) aren't run here: they need logins, cost money and
// aren't deterministic; they're checked by hand before a release.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/arjunvlama/toli/internal/api"
)

// toli is the binary under test, built once by TestMain.
var toli string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "toli-e2e")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	toli = filepath.Join(dir, "toli")
	if runtime.GOOS == "windows" {
		toli += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", toli, "../cmd/toli").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build toli: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// daemon is a running toli daemon with its own home and ports.
type daemon struct {
	home            string
	mcp, admin, a2a string // addresses
	api             *api.Client
}

// freeAddr returns a localhost address with a port nobody's using.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func startDaemon(t *testing.T) *daemon {
	t.Helper()
	home := t.TempDir()
	d := &daemon{home: home, mcp: freeAddr(t), admin: freeAddr(t), a2a: freeAddr(t)}
	cmd := exec.Command(toli, "daemon", "--mcp-listen", d.mcp, "--admin-listen", d.admin, "--a2a-listen", d.a2a)
	cmd.Env = append(os.Environ(), "TOLI_HOME="+home)
	var log bytes.Buffer
	cmd.Stdout, cmd.Stderr = &log, &log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.Process.Signal(os.Interrupt) != nil { // no interrupt on Windows
			cmd.Process.Kill()
		}
		cmd.Wait()
		if t.Failed() {
			t.Logf("daemon log:\n%s", log.String())
		}
	})
	for i := 0; ; i++ {
		if conn, err := net.Dial("tcp", d.admin); err == nil {
			conn.Close()
			break
		}
		if i == 600 { // 30s: a new binary can be slow to start on Windows (scanned first)
			t.Fatalf("daemon didn't start:\n%s", log.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	tok, err := os.ReadFile(filepath.Join(home, "admin-token"))
	if err != nil {
		t.Fatal(err)
	}
	d.api = api.NewClient("http://"+d.admin, strings.TrimSpace(string(tok)))
	return d
}

func (d *daemon) cardURL(agent string) string {
	return "http://" + d.a2a + "/a2a/" + agent + "/.well-known/agent-card.json"
}

// agent is one coding agent: its MCP session and its notes.
type agent struct {
	t     *testing.T
	name  string
	token string
	mcp   *mcp.ClientSession
	notes chan string
}

func addAgent(t *testing.T, d *daemon, name string) *agent {
	t.Helper()
	ctx := context.Background()
	resp, err := d.api.AddAgent(ctx, api.AddAgentRequest{Name: name, Description: "e2e " + name})
	if err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: "http://" + d.mcp + "/mcp/" + resp.Token}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return &agent{t: t, name: name, token: resp.Token, mcp: session, notes: make(chan string, 20)}
}

// call calls one of toli's tools and returns its structured result, or the
// tool's error text with ok false.
func (a *agent) call(tool string, args map[string]any) (res map[string]any, errText string) {
	a.t.Helper()
	r, err := a.mcp.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		a.t.Fatalf("%s %s: %v", a.name, tool, err)
	}
	if r.IsError {
		var b strings.Builder
		for _, c := range r.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				b.WriteString(tc.Text)
			}
		}
		return nil, b.String()
	}
	raw, _ := json.Marshal(r.StructuredContent)
	json.Unmarshal(raw, &res)
	return res, ""
}

// must is call, failing the test on a tool error.
func (a *agent) must(tool string, args map[string]any) map[string]any {
	a.t.Helper()
	res, errText := a.call(tool, args)
	if errText != "" {
		a.t.Fatalf("%s %s: %s", a.name, tool, errText)
	}
	return res
}

// collectNotes registers a session for the agent and collects its notes in
// the background, as any agent's own integration would (docs/custom-agents.md).
func (a *agent) collectNotes(d *daemon) {
	a.t.Helper()
	hook := "http://" + d.mcp + "/hook/"
	do := func(ctx context.Context, method, path string, body any, out any) (int, error) {
		var r *bytes.Reader
		if body != nil {
			raw, _ := json.Marshal(body)
			r = bytes.NewReader(raw)
		} else {
			r = bytes.NewReader(nil)
		}
		req, _ := http.NewRequestWithContext(ctx, method, hook+path, r)
		req.Header.Set("Authorization", "Bearer "+a.token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		if out != nil {
			json.NewDecoder(resp.Body).Decode(out)
		}
		return resp.StatusCode, nil
	}
	if code, err := do(context.Background(), "POST", "session-start", map[string]string{"harness": "e2e", "session_id": a.name + "-s1"}, nil); err != nil || code != 200 {
		a.t.Fatalf("session-start: %d %v", code, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.t.Cleanup(cancel)
	go func() {
		for ctx.Err() == nil {
			var out struct {
				Notes []string `json:"notifications"`
			}
			code, err := do(ctx, "GET", "notifications?session="+a.name+"-s1", nil, &out)
			if err != nil || code != 200 {
				return
			}
			for _, n := range out.Notes {
				a.notes <- n
			}
		}
	}()
}

// waitNote waits for a note containing want.
func (a *agent) waitNote(want string) {
	a.t.Helper()
	timeout := time.After(30 * time.Second)
	for {
		select {
		case n := <-a.notes:
			if strings.Contains(n, want) {
				return
			}
		case <-timeout:
			a.t.Fatalf("%s: no note containing %q", a.name, want)
		}
	}
}

// waitState waits (on the delegator's side) for a delegated task to reach
// state, and returns the update. The daemon polls peers every 10s.
func (a *agent) waitState(taskID, state string) map[string]any {
	a.t.Helper()
	var version float64
	for range 6 {
		u := a.must("get_delegated_task_updates", map[string]any{"task_id": taskID, "after_version": int64(version), "wait_seconds": 15})
		if u["state"] == state {
			return u
		}
		version = u["version"].(float64)
	}
	a.t.Fatalf("%s: task %s never reached %s", a.name, taskID, state)
	return nil
}

// pair puts each agent on the other's allowlist, exchanging tokens, and
// returns the peer ids (a's peer for b, b's peer for a).
func pair(t *testing.T, da *daemon, a *agent, db *daemon, b *agent) (int64, int64) {
	t.Helper()
	ctx := context.Background()
	onA, err := da.api.AddPeer(ctx, api.AddPeerRequest{Agent: a.name, Name: b.name, URL: db.cardURL(b.name)})
	if err != nil {
		t.Fatal(err)
	}
	onB, err := db.api.AddPeer(ctx, api.AddPeerRequest{Agent: b.name, Name: a.name, URL: da.cardURL(a.name), Token: onA.Token})
	if err != nil {
		t.Fatal(err)
	}
	if err := da.api.SetPeerToken(ctx, onA.Peer.ID, onB.Token); err != nil {
		t.Fatal(err)
	}
	return onA.Peer.ID, onB.Peer.ID
}

func TestDelegateAskAnswerComplete(t *testing.T) {
	da, db := startDaemon(t), startDaemon(t)
	alice, bob := addAgent(t, da, "alice"), addAgent(t, db, "bob")
	pair(t, da, alice, db, bob)
	alice.collectNotes(da)
	bob.collectNotes(db)

	// alice sees bob as a peer, and delegates.
	if peers := alice.must("list_peers", nil); !strings.Contains(fmt.Sprint(peers), "bob") {
		t.Fatalf("list_peers = %v", peers)
	}
	task := alice.must("delegate", map[string]any{"peer": "bob", "message": "Add a /health endpoint"})
	id := task["task_id"].(string)

	// bob is told, finds it, accepts it, and asks a question.
	bob.waitNote("new task from alice")
	if avail := bob.must("list_available_tasks", nil); !strings.Contains(fmt.Sprint(avail), id) {
		t.Fatalf("list_available_tasks = %v", avail)
	}
	bob.must("accept_task", map[string]any{"task_id": id, "message": "On it."})
	bob.must("update_task", map[string]any{"task_id": id, "state": "input-required", "message": "JSON or plain text?"})

	// alice sees the question, and answers.
	alice.waitNote("asked")
	q := alice.waitState(id, "input-required")
	if !strings.Contains(fmt.Sprint(q["messages"]), "JSON or plain text?") {
		t.Fatalf("question not in %v", q["messages"])
	}
	alice.must("reply", map[string]any{"task_id": id, "message": "JSON."})

	// bob gets the answer and finishes with an artifact.
	bob.waitNote("answered your question")
	bob.must("update_task", map[string]any{"task_id": id, "state": "completed", "message": "Done.",
		"artifacts": []map[string]string{{"name": "diff", "text": "+ GET /health"}}})

	// alice sees it completed, with the artifact; the whole conversation is there.
	alice.waitNote("completed")
	done := alice.waitState(id, "completed")
	if !strings.Contains(fmt.Sprint(done["artifacts"]), "+ GET /health") {
		t.Fatalf("artifacts = %v", done["artifacts"])
	}
	full := alice.must("get_task_full", map[string]any{"task_id": id})
	for _, want := range []string{"Add a /health endpoint", "JSON or plain text?", "JSON.", "Done."} {
		if !strings.Contains(fmt.Sprint(full["messages"]), want) {
			t.Errorf("get_task_full lacks %q: %v", want, full["messages"])
		}
	}
}

func TestCancel(t *testing.T) {
	da, db := startDaemon(t), startDaemon(t)
	alice, bob := addAgent(t, da, "alice"), addAgent(t, db, "bob")
	pair(t, da, alice, db, bob)

	id := alice.must("delegate", map[string]any{"peer": "bob", "message": "Long job"})["task_id"].(string)
	bob.must("accept_task", map[string]any{"task_id": id})
	if res := alice.must("cancel_task", map[string]any{"task_id": id}); res["state"] != "canceled" {
		t.Fatalf("cancel_task = %v", res)
	}
	if _, errText := bob.call("update_task", map[string]any{"task_id": id, "state": "completed", "message": "too late"}); errText == "" {
		t.Error("bob completed a canceled task")
	}
}

func TestRotatedPeerToken(t *testing.T) {
	da, db := startDaemon(t), startDaemon(t)
	alice, bob := addAgent(t, da, "alice"), addAgent(t, db, "bob")
	bobOnAlice, aliceOnBob := pair(t, da, alice, db, bob)
	ctx := context.Background()

	// bob's human issues alice a new token: alice's old one stops working...
	rot, err := db.api.RotatePeerToken(ctx, aliceOnBob)
	if err != nil {
		t.Fatal(err)
	}
	if _, errText := alice.call("delegate", map[string]any{"peer": "bob", "message": "Hi"}); !strings.Contains(errText, "401") {
		t.Fatalf("delegate with the old token: %q, want a 401", errText)
	}
	// ...until alice's human saves the new one.
	if err := da.api.SetPeerToken(ctx, bobOnAlice, rot.Token); err != nil {
		t.Fatal(err)
	}
	alice.must("delegate", map[string]any{"peer": "bob", "message": "Hi"})
}

func TestRemovedPeerCancelsTasks(t *testing.T) {
	da, db := startDaemon(t), startDaemon(t)
	alice, bob := addAgent(t, da, "alice"), addAgent(t, db, "bob")
	bobOnAlice, _ := pair(t, da, alice, db, bob)

	id := alice.must("delegate", map[string]any{"peer": "bob", "message": "Long job"})["task_id"].(string)
	if err := da.api.RemovePeer(context.Background(), bobOnAlice); err != nil {
		t.Fatal(err)
	}
	// alice can't see the task any more; bob's side is canceled (in the
	// background, so give it a moment).
	if _, errText := alice.call("get_task_full", map[string]any{"task_id": id}); errText == "" {
		t.Error("alice still sees the removed peer's task")
	}
	for i := 0; ; i++ {
		res := bob.must("get_task_full", map[string]any{"task_id": id})
		if res["state"] == "canceled" {
			break
		}
		if i == 50 {
			t.Fatalf("bob's task = %v, want canceled", res["state"])
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// cli runs a toli command against d, as its human would, and returns its
// output.
func (d *daemon) cli(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command(toli, args...)
	cmd.Env = append(os.Environ(), "TOLI_HOME="+d.home, "TOLI_ADMIN_ADDR="+d.admin, "TOLI_MCP_ADDR="+d.mcp)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("toli %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// cliFails runs a toli command that should fail, and returns its output.
func (d *daemon) cliFails(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command(toli, args...)
	cmd.Env = append(os.Environ(), "TOLI_HOME="+d.home, "TOLI_ADMIN_ADDR="+d.admin, "TOLI_MCP_ADDR="+d.mcp)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("toli %s succeeded:\n%s", strings.Join(args, " "), out)
	}
	return string(out)
}

// waitConnected waits until toli peer list shows agent's peer as connected
// and holding its token.
func (d *daemon) waitConnected(t *testing.T, agent, peer string) {
	t.Helper()
	for i := 0; ; i++ {
		out := d.cli(t, "peer", "list", agent)
		for _, line := range strings.Split(out, "\n") {
			if f := strings.Fields(line); len(f) >= 3 && f[0] == peer && f[1] == "true" && f[2] == "true" {
				return
			}
		}
		if i == 100 {
			t.Fatalf("%s's peer %s isn't connected:\n%s", agent, peer, out)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// field is the value on a "key  value" line of CLI output.
func field(t *testing.T, out, key string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == key {
			return f[1]
		}
	}
	t.Fatalf("no %s in:\n%s", key, out)
	return ""
}

// TestCLI sets up and pairs agents with the CLI, as the README does, then
// checks tasks show in it and that editing an agent keeps what isn't given.
func TestCLI(t *testing.T) {
	da, db := startDaemon(t), startDaemon(t)

	out := da.cli(t, "agent", "add", "web", "--description", "Owns the web UI")
	if !strings.Contains(out, "Next: in the folder where you start it, run `toli connect <harness> web`.") {
		t.Errorf("agent add:\n%s", out)
	}
	if url := field(t, out, "card_url"); url != da.cardURL("web") {
		t.Errorf("card_url = %s, want %s", url, da.cardURL("web"))
	}

	// Pair two agents on different daemons, one side at a time.
	frontend := addAgent(t, da, "frontend")
	addAgent(t, db, "backend")
	tokenForBackend := field(t, da.cli(t, "peer", "add", "frontend", "backend", db.cardURL("backend")), "token")
	tokenForFrontend := field(t, db.cli(t, "peer", "add", "backend", "frontend", da.cardURL("frontend"), "--token", tokenForBackend), "token")
	da.cli(t, "peer", "token", "frontend", "backend", tokenForFrontend)
	da.waitConnected(t, "frontend", "backend")
	db.waitConnected(t, "backend", "frontend")

	// A task shows on both sides, by its short ID too, and can be canceled.
	id := frontend.must("delegate", map[string]any{"peer": "backend", "message": "Add a /stats endpoint"})["task_id"].(string)
	short := id[len(id)-8:]
	if out := da.cli(t, "task", "list", "frontend"); !strings.Contains(out, short) || !strings.Contains(out, "backend") {
		t.Errorf("task list frontend:\n%s", out)
	}
	if out := db.cli(t, "task", "list", "backend", "--assigned"); !strings.Contains(out, short) || !strings.Contains(out, "frontend") {
		t.Errorf("task list backend --assigned:\n%s", out)
	}
	if out := da.cli(t, "task", "show", "frontend", short); !strings.Contains(out, id) || !strings.Contains(out, "Add a /stats endpoint") {
		t.Errorf("task show:\n%s", out)
	}
	if out := da.cli(t, "task", "cancel", "frontend", short); !strings.Contains(out, "✓ Canceled task "+short+".") {
		t.Errorf("task cancel: %s", out)
	}

	// Editing only the description keeps the skills, and the other way round.
	skills := filepath.Join(t.TempDir(), "skills.json")
	if err := os.WriteFile(skills, []byte(`[{"name": "Signups API", "description": "Signup stats"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	db.cli(t, "agent", "edit", "backend", "--skills", skills)
	db.cli(t, "agent", "edit", "backend", "--description", "Owns the API")
	agents, err := db.api.ListAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a := agents[0]; a.Description != "Owns the API" || len(a.Skills) != 1 || a.Skills[0].Name != "Signups API" {
		t.Errorf("after both edits: %+v", a)
	}

	// Push notifications: one target at a time, and the output says what it
	// replaced.
	wake := filepath.Join(t.TempDir(), "wake-backend")
	if err := os.WriteFile(wake, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	out = db.cli(t, "agent", "notify", "backend", "--webhook", "https://example.com/toli")
	if !strings.Contains(out, "✓ Notifications for agent 'backend' now go to webhook https://example.com/toli.") || !strings.HasPrefix(field(t, out, "secret"), "toli_") {
		t.Errorf("notify --webhook:\n%s", out)
	}
	if out := db.cli(t, "agent", "notify", "backend", "--command", wake); !strings.Contains(out, "(replacing webhook https://example.com/…)") {
		t.Errorf("notify --command:\n%s", out)
	}
	if out := db.cli(t, "agent", "list"); !strings.Contains(out, "command "+wake) {
		t.Errorf("agent list:\n%s", out)
	}
	if out := db.cli(t, "agent", "notify", "backend", "--none"); !strings.Contains(out, "(they went to command "+wake+")") {
		t.Errorf("notify --none:\n%s", out)
	}
}

// TestInvite pairs agents with toli peer invite and accept: on two daemons,
// then two agents on one daemon.
func TestInvite(t *testing.T) {
	da, db := startDaemon(t), startDaemon(t)
	frontend, backend := addAgent(t, da, "frontend"), addAgent(t, db, "backend")

	out := da.cli(t, "peer", "invite", "frontend", "backend")
	inv := ""
	for _, f := range strings.Fields(out) {
		if strings.HasPrefix(f, "toli-invite:") {
			inv = f
		}
	}
	if !strings.Contains(out, "✓ Created an invite for peer 'backend'.") || !strings.Contains(out, "toli peer accept <agent> "+inv) || inv == "" {
		t.Fatalf("peer invite:\n%s", out)
	}
	if out := db.cli(t, "peer", "accept", "backend", inv); !strings.Contains(out, "✓ Paired agent 'backend' with peer 'frontend'.") {
		t.Errorf("peer accept: %s", out)
	}
	da.waitConnected(t, "frontend", "backend")
	db.waitConnected(t, "backend", "frontend")

	// Each side's token works: both can delegate.
	frontend.must("delegate", map[string]any{"peer": "backend", "message": "Add a /stats endpoint"})
	backend.must("delegate", map[string]any{"peer": "frontend", "message": "Show the stats"})
	if out := db.cli(t, "task", "list", "backend", "--assigned"); !strings.Contains(out, "frontend") {
		t.Errorf("backend's assigned tasks:\n%s", out)
	}
	if out := da.cli(t, "task", "list", "frontend", "--assigned"); !strings.Contains(out, "backend") {
		t.Errorf("frontend's assigned tasks:\n%s", out)
	}

	// A used invite pairs no one else.
	addAgent(t, db, "other")
	if out := db.cliFails(t, "peer", "accept", "other", inv); !strings.Contains(out, "no such invite") {
		t.Errorf("used invite: %s", out)
	}

	// Two agents on one daemon, with a name of the accepter's choosing.
	addAgent(t, da, "docs")
	out = da.cli(t, "peer", "invite", "frontend", "docs")
	inv = out[strings.Index(out, "toli-invite:"):]
	inv = inv[:strings.IndexAny(inv, " \n")]
	if out := da.cli(t, "peer", "accept", "docs", inv, "--name", "ui"); !strings.Contains(out, "✓ Paired agent 'docs' with peer 'ui'.") {
		t.Errorf("peer accept --name: %s", out)
	}
	da.waitConnected(t, "frontend", "docs")
	da.waitConnected(t, "docs", "ui")
}

// TestConnectByName connects a harness by agent name: it gets a new MCP URL,
// and the old one stops working.
func TestConnectByName(t *testing.T) {
	d := startDaemon(t)
	old := addAgent(t, d, "web").token
	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(toli, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "TOLI_HOME="+d.home, "TOLI_ADMIN_ADDR="+d.admin, "TOLI_MCP_ADDR="+d.mcp)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("toli %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}

	if out := run("connect", "codex", "web"); !strings.Contains(out, "✓ Connected Codex to agent 'web' in ") {
		t.Fatalf("connect by name:\n%s", out)
	}
	config, err := os.ReadFile(filepath.Join(dir, ".codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	url := regexp.MustCompile(`url = "([^"]+)"`).FindStringSubmatch(string(config))
	if url == nil || !strings.HasPrefix(url[1], "http://"+d.mcp+"/mcp/toli_") || strings.HasSuffix(url[1], old) {
		t.Fatalf("config.toml:\n%s", config)
	}
	// The new URL works (connect checks it); the old one doesn't.
	if out := run("connect", "codex", "--mcp-url", url[1]); !strings.Contains(out, "agent 'web'") {
		t.Errorf("connect --mcp-url:\n%s", out)
	}
	if out := d.cliFails(t, "connect", "codex", "--mcp-url", "http://"+d.mcp+"/mcp/"+old); !strings.Contains(out, "check the MCP URL") {
		t.Errorf("old URL: %s", out)
	}

	if out := run("disconnect", "codex"); !strings.Contains(out, "✓ Disconnected Codex in ") {
		t.Errorf("disconnect:\n%s", out)
	}
	if out := d.cliFails(t, "connect", "codex", "nobody"); !strings.Contains(out, "no agent 'nobody'") {
		t.Errorf("unknown agent: %s", out)
	}

	// Adding an agent can connect it too; an unknown harness adds nothing.
	if out := d.cliFails(t, "agent", "add", "api", "--connect", "nope"); !strings.Contains(out, "unknown harness") {
		t.Errorf("agent add --connect nope: %s", out)
	}
	out := run("agent", "add", "api", "--connect", "codex")
	if !strings.Contains(out, "✓ Added agent 'api'.") || !strings.Contains(out, "✓ Connected Codex to agent 'api' in ") {
		t.Errorf("agent add --connect:\n%s", out)
	}
	if config, _ := os.ReadFile(filepath.Join(dir, ".codex", "config.toml")); !strings.Contains(string(config), "/mcp/toli_") {
		t.Errorf("config.toml after agent add --connect:\n%s", config)
	}

	// If connecting fails, the agent stays and the output says how to finish.
	cmd := exec.Command(toli, "agent", "add", "claw", "--connect", "openclaw")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TOLI_HOME="+d.home, "TOLI_ADMIN_ADDR="+d.admin, "TOLI_MCP_ADDR="+d.mcp, "PATH="+t.TempDir())
	out2, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out2), "✓ Added agent 'claw'.") ||
		!strings.Contains(string(out2), "X Couldn't connect OpenClaw: openclaw isn't on your PATH") ||
		!strings.Contains(string(out2), "Next: once that's fixed, run `toli connect openclaw claw` here.") ||
		strings.Contains(string(out2), "toli:") {
		t.Errorf("agent add --connect that fails: %v\n%s", err, out2)
	}
}

// TestPushNotify: an agent with no session, but a webhook set by its human,
// is told about work by the webhook (with its secret).
func TestPushNotify(t *testing.T) {
	da, db := startDaemon(t), startDaemon(t)
	alice, bob := addAgent(t, da, "alice"), addAgent(t, db, "bob")
	pair(t, da, alice, db, bob)

	got := make(chan string, 4)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var n struct{ Agent, Text string }
		json.NewDecoder(r.Body).Decode(&n)
		got <- r.Header.Get("Authorization") + " | " + n.Agent + " | " + n.Text
	}))
	defer hook.Close()
	set, err := db.api.SetAgentNotify(context.Background(), "bob", api.SetNotifyRequest{Webhook: hook.URL})
	if err != nil {
		t.Fatal(err)
	}

	alice.must("delegate", map[string]any{"peer": "bob", "message": "Add a /health endpoint"})
	select {
	case n := <-got:
		if !strings.HasPrefix(n, "Bearer "+set.Secret+" | bob | ") || !strings.Contains(n, "new task from alice") {
			t.Fatalf("webhook got %q", n)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the webhook was never called")
	}
}

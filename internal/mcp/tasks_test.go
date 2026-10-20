package mcp_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/arjunvlama/toli/internal/a2atest"
	"github.com/arjunvlama/toli/internal/backend"
	"github.com/arjunvlama/toli/internal/mcp"
	"github.com/arjunvlama/toli/internal/store"
)

// env is a running backend and MCP server with agent "backend" and its fake
// peer "bob".
type env struct {
	b   *backend.Backend
	url string // the MCP server's base URL
	tok string // agent backend's token
	bob *a2atest.FakePeer
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "toli.db"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := backend.New(ctx, st, backend.Options{PollEvery: 20 * time.Millisecond, CheckEvery: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { b.Run(runCtx); close(done) }()
	t.Cleanup(func() { cancel(); <-done; st.Close() })

	tok, err := b.AddAgent(ctx, "backend", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	bob := a2atest.NewFakePeer(t)
	if _, _, err := b.AddPeer(ctx, "backend", "bob", bob.URL, "bobs-token"); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	h := mcp.New(b)
	mux.Handle("/mcp", h)
	mux.Handle("/mcp/", h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &env{b: b, url: srv.URL, tok: tok, bob: bob}
}

// dial opens an MCP client session with tok in the URL, as coding agents connect.
func (e *env) dial(t *testing.T, tok string) (*sdkmcp.ClientSession, error) {
	t.Helper()
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test", Version: "v0"}, nil)
	cs, err := client.Connect(context.Background(), &sdkmcp.StreamableClientTransport{Endpoint: e.url + "/mcp/" + tok, MaxRetries: -1}, nil)
	if err == nil {
		t.Cleanup(func() { cs.Close() })
	}
	return cs, err
}

// connect returns an MCP session acting as agent "backend", and its peer bob.
func connect(t *testing.T) (*sdkmcp.ClientSession, *a2atest.FakePeer) {
	t.Helper()
	e := newEnv(t)
	cs, err := e.dial(t, e.tok)
	if err != nil {
		t.Fatal(err)
	}
	return cs, e.bob
}

// call runs a tool and decodes its structured result into out. It fails the
// test on a tool error unless wantErr is set, and returns the error text.
func call(t *testing.T, cs *sdkmcp.ClientSession, name string, args map[string]any, out any) (errText string) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		for _, c := range res.Content {
			if tc, ok := c.(*sdkmcp.TextContent); ok {
				return tc.Text
			}
		}
		return "(error with no text)"
	}
	if out != nil {
		raw, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s: decode %s: %v", name, raw, err)
		}
	}
	return ""
}

type summary struct {
	TaskID  string `json:"task_id"`
	State   string `json:"state"`
	Version int64  `json:"version"`
	Answer  *struct {
		From string `json:"from"`
		Text string `json:"text"`
	} `json:"answer"`
}

type update struct {
	TaskID   string   `json:"task_id"`
	Peer     string   `json:"peer"`
	State    string   `json:"state"`
	Messages []string `json:"messages"`
	Version  int64    `json:"version"`
}

// waitFor long-polls get_delegated_task_updates until the task reaches state.
func waitFor(t *testing.T, cs *sdkmcp.ClientSession, id, state string) update {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	args := map[string]any{"task_id": id}
	for time.Now().Before(deadline) {
		var u update
		if e := call(t, cs, "get_delegated_task_updates", args, &u); e != "" {
			t.Fatalf("get_delegated_task_updates: %s", e)
		}
		if u.State == state {
			return u
		}
		args = map[string]any{"task_id": id, "after_version": u.Version, "wait_seconds": 2}
	}
	t.Fatalf("task %s never reached %s", id, state)
	return update{}
}

func TestDelegateAndFollow(t *testing.T) {
	cs, bob := connect(t)

	var peers struct {
		Peers []struct {
			Peer      string `json:"peer"`
			Name      string `json:"name"`
			Reachable *bool  `json:"reachable"`
		} `json:"peers"`
	}
	// reachable appears once the daemon has checked the peer (just after it's added).
	deadline := time.Now().Add(5 * time.Second)
	for {
		call(t, cs, "list_peers", nil, &peers)
		if len(peers.Peers) != 1 {
			t.Fatalf("list_peers = %+v", peers)
		}
		if peers.Peers[0].Reachable != nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if p := peers.Peers[0]; p.Peer != "bob" || p.Name != "Fake peer" || p.Reachable == nil || !*p.Reachable {
		t.Fatalf("list_peers peer = %+v, want bob (card name Fake peer), reachable", p)
	}

	var s summary
	call(t, cs, "delegate", map[string]any{"peer": "bob", "message": "Add a /users endpoint\nwith tests"}, &s)
	if s.TaskID == "" || (s.State != "submitted" && s.State != "working") || s.Version != 1 || s.Answer != nil {
		t.Fatalf("delegate = %+v", s)
	}

	// The version from delegate is what to wait after: the next update is
	// a real change, not the same state again.
	bob.Next(a2a.TaskStateInputRequired, "Should it be paginated?")
	var first update
	call(t, cs, "get_delegated_task_updates", map[string]any{"task_id": s.TaskID, "after_version": s.Version, "wait_seconds": 5}, &first)
	if first.Version <= s.Version {
		t.Fatalf("get_delegated_task_updates after delegate's version returned version %d, want > %d", first.Version, s.Version)
	}

	u := waitFor(t, cs, s.TaskID, "input-required")
	if u.Peer != "bob" || !slices.Equal(u.Messages, []string{"Should it be paginated?"}) {
		t.Fatalf("update = %+v", u)
	}

	var r summary
	call(t, cs, "reply", map[string]any{"task_id": s.TaskID, "message": "yes, cursor"}, &r)
	bob.Next(a2a.TaskStateCompleted, "done: GET /users")
	u = waitFor(t, cs, s.TaskID, "completed")
	// What bob said since our reply. bob is a plain a2a-go server: it moves
	// its question into history only with its next status, after our reply,
	// so the answered question shows up again. (Toli peers record the reply
	// after the question.)
	if !slices.Equal(u.Messages, []string{"Should it be paginated?", "done: GET /users"}) {
		t.Fatalf("final update = %+v", u)
	}

	var full struct {
		Peer     string `json:"peer"`
		State    string `json:"state"`
		Messages []struct {
			From string `json:"from"`
			Text string `json:"text"`
		} `json:"messages"`
	}
	call(t, cs, "get_task_full", map[string]any{"task_id": s.TaskID}, &full)
	if full.State != "completed" || len(full.Messages) < 3 ||
		full.Messages[0].From != "backend" || full.Messages[len(full.Messages)-1].From != "bob" ||
		full.Messages[len(full.Messages)-1].Text != "done: GET /users" {
		t.Fatalf("get_task_full = %+v", full)
	}

	var list struct {
		Tasks []struct {
			TaskID  string `json:"task_id"`
			Peer    string `json:"peer"`
			State   string `json:"state"`
			Summary string `json:"summary"`
		} `json:"tasks"`
	}
	call(t, cs, "list_delegated_tasks", map[string]any{"state": "completed"}, &list)
	if len(list.Tasks) != 1 || list.Tasks[0].Peer != "bob" || list.Tasks[0].Summary != "Add a /users endpoint" {
		t.Fatalf("list_delegated_tasks(completed) = %+v", list)
	}
	call(t, cs, "list_delegated_tasks", map[string]any{"state": "working"}, &list)
	if len(list.Tasks) != 0 {
		t.Fatalf("list_delegated_tasks(working) = %+v", list)
	}
}

func TestDirectAnswerAndCancel(t *testing.T) {
	cs, _ := connect(t)

	var s summary
	call(t, cs, "delegate", map[string]any{"peer": "bob", "message": "ping"}, &s)
	if s.State != "completed" || s.Answer == nil || s.Answer.From != "bob" || s.Answer.Text != "pong" {
		t.Fatalf("direct answer = %+v", s)
	}

	call(t, cs, "delegate", map[string]any{"peer": "bob", "message": "long job"}, &s)
	var c summary
	call(t, cs, "cancel_task", map[string]any{"task_id": s.TaskID}, &c)
	if c.State != "canceled" {
		t.Fatalf("cancel_task = %+v", c)
	}
	// Canceling again reports the final state, not an error.
	if e := call(t, cs, "cancel_task", map[string]any{"task_id": s.TaskID}, &c); e != "" || c.State != "canceled" {
		t.Fatalf("second cancel_task: %q, %+v", e, c)
	}
}

func TestToolErrors(t *testing.T) {
	cs, _ := connect(t)
	for _, c := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"get_delegated_task_updates", map[string]any{"task_id": "nope"}, "task nope not found"},
		{"get_task_full", map[string]any{"task_id": "nope"}, "task nope not found"},
		{"reply", map[string]any{"task_id": "nope", "message": "hi"}, "task nope not found"},
		{"cancel_task", map[string]any{"task_id": "nope"}, "task nope not found"},
		{"delegate", map[string]any{"peer": "nobody", "message": "hi"}, `peer "nobody"`},
		{"list_delegated_tasks", map[string]any{"state": "bogus"}, `unknown state "bogus"`},
	} {
		if e := call(t, cs, c.tool, c.args, nil); !strings.Contains(e, c.want) {
			t.Errorf("%s(%v) error = %q, want it to contain %q", c.tool, c.args, e, c.want)
		}
	}
}

func TestSessions(t *testing.T) {
	cs, bob := connect(t)

	var sess struct {
		SessionID string `json:"session_id"`
		Title     string `json:"title"`
	}
	call(t, cs, "start_session", map[string]any{"title": "users api"}, &sess)
	if sess.SessionID == "" || sess.Title != "users api" {
		t.Fatalf("start_session = %+v", sess)
	}
	if e := call(t, cs, "start_session", map[string]any{"title": "  "}, nil); !strings.Contains(e, "title is required") {
		t.Fatalf("blank title error = %q", e)
	}

	var inSession, standalone summary
	call(t, cs, "delegate", map[string]any{"peer": "bob", "message": "in the session", "session_id": sess.SessionID}, &inSession)
	call(t, cs, "delegate", map[string]any{"peer": "bob", "message": "standalone"}, &standalone)

	var u struct {
		SessionID string `json:"session_id"`
	}
	call(t, cs, "get_delegated_task_updates", map[string]any{"task_id": inSession.TaskID}, &u)
	if u.SessionID != sess.SessionID {
		t.Fatalf("session task's session_id = %q, want %q", u.SessionID, sess.SessionID)
	}
	u.SessionID = "" // omitted when empty, so decoding wouldn't clear it
	call(t, cs, "get_delegated_task_updates", map[string]any{"task_id": standalone.TaskID}, &u)
	if u.SessionID != "" {
		t.Fatalf("standalone task's session_id = %q, want empty (the peer's own contextId isn't a session)", u.SessionID)
	}

	var list struct {
		Tasks []struct {
			TaskID string `json:"task_id"`
		} `json:"tasks"`
	}
	call(t, cs, "list_delegated_tasks", map[string]any{"session_id": sess.SessionID}, &list)
	if len(list.Tasks) != 1 || list.Tasks[0].TaskID != inSession.TaskID {
		t.Fatalf("list_delegated_tasks(session) = %+v", list)
	}

	var del struct {
		Deleted bool `json:"deleted"`
	}
	call(t, cs, "delete_session", map[string]any{"session_id": sess.SessionID}, &del)
	if !del.Deleted {
		t.Fatalf("delete_session = %+v", del)
	}
	// Its running task is canceled on the peer in the background, and gone here.
	deadline := time.Now().Add(5 * time.Second)
	for !slices.Contains(bob.Canceled(), a2a.TaskID(inSession.TaskID)) {
		if time.Now().After(deadline) {
			t.Fatalf("peer never got a cancel for the session's task; got %v", bob.Canceled())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if e := call(t, cs, "get_delegated_task_updates", map[string]any{"task_id": inSession.TaskID}, nil); !strings.Contains(e, "not found") {
		t.Fatalf("deleted session's task: error = %q, want not found", e)
	}
	call(t, cs, "list_delegated_tasks", nil, &list)
	if len(list.Tasks) != 1 || list.Tasks[0].TaskID != standalone.TaskID {
		t.Fatalf("after delete, list_delegated_tasks = %+v, want only the standalone task", list)
	}

	want := "session " + sess.SessionID + " not found"
	if e := call(t, cs, "delete_session", map[string]any{"session_id": sess.SessionID}, nil); !strings.Contains(e, want) {
		t.Errorf("delete_session again: error = %q, want %q", e, want)
	}
	if e := call(t, cs, "delegate", map[string]any{"peer": "bob", "message": "x", "session_id": sess.SessionID}, nil); !strings.Contains(e, "not found") {
		t.Errorf("delegate into deleted session: error = %q", e)
	}
}

func TestDisabledAgentAndRotatedToken(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	cs, err := e.dial(t, e.tok)
	if err != nil {
		t.Fatal(err)
	}

	// Disabling cuts off even a session that's already connected.
	if err := e.b.DisableAgent(ctx, "backend"); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CallTool(ctx, &sdkmcp.CallToolParams{Name: "list_peers"}); err == nil {
		t.Fatal("call from a disabled agent's open session succeeded")
	}
	if _, err := e.dial(t, e.tok); err == nil {
		t.Fatal("connecting as a disabled agent succeeded")
	}
	// The 401 says why (MCP clients don't surface the body, but logs and curl do).
	resp, err := http.Post(e.url+"/mcp/"+e.tok, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(string(body), "agent 'backend': agent is disabled") {
		t.Fatalf("disabled agent: %d %q, want 401 saying it's disabled", resp.StatusCode, body)
	}

	if err := e.b.EnableAgent(ctx, "backend"); err != nil {
		t.Fatal(err)
	}
	cs, err = e.dial(t, e.tok)
	if err != nil {
		t.Fatalf("connecting after re-enable: %v", err)
	}
	call(t, cs, "list_peers", nil, nil)

	// Rotation: the old token stops working, the new one works.
	newTok, err := e.b.RotateAgentToken(ctx, "backend")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.dial(t, e.tok); err == nil {
		t.Fatal("old token still connects after rotation")
	}
	cs, err = e.dial(t, newTok)
	if err != nil {
		t.Fatalf("new token: %v", err)
	}
	call(t, cs, "list_peers", nil, nil)
}

package backend_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/arjunvlama/toli/internal/a2atest"
	"github.com/arjunvlama/toli/internal/backend"
	"github.com/arjunvlama/toli/internal/backend/peers"
	"github.com/arjunvlama/toli/internal/store"
)

func TestCreateAndRedeemInvite(t *testing.T) {
	ctx := context.Background()
	b := setup(t)
	addAgent(t, b, "backend")
	web := a2atest.NewFakePeer(t)
	webCard := web.URL + a2asrv.WellKnownAgentCardPath

	if _, _, err := b.CreateInvite(ctx, "nobody", "web", "http://x"+a2asrv.WellKnownAgentCardPath); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("invite for an unknown agent: %v, want ErrNotFound", err)
	}
	if _, _, err := b.CreateInvite(ctx, "backend", "web", ""); err == nil {
		t.Error("invite with no A2A address: nil error")
	}

	s, _, err := b.CreateInvite(ctx, "backend", "web", "http://127.0.0.1:7422/a2a/backend"+a2asrv.WellKnownAgentCardPath)
	if err != nil || !strings.HasPrefix(s, "toli-invite:") {
		t.Fatalf("CreateInvite: %q, %v", s, err)
	}
	tok := inviteToken(t, s)

	p, err := b.RedeemInvite(ctx, "backend", tok, webCard, "from-web")
	if err != nil || p.Name != "web" || p.OutboundToken != "from-web" {
		t.Fatalf("RedeemInvite: %+v, %v", p, err)
	}
	if caller, err := b.PeerByToken(ctx, tok); err != nil || caller.ID != p.ID {
		t.Errorf("the invite's token doesn't identify the new peer: %+v, %v", caller, err)
	}
	again, err := b.RedeemInvite(ctx, "backend", tok, webCard, "from-web-2")
	if err != nil || again.ID != p.ID || again.OutboundToken != "from-web-2" {
		t.Errorf("retry: %+v, %v; want peer %d with the new token", again, err, p.ID)
	}

	// A name that's taken can't be invited.
	if _, _, err := b.CreateInvite(ctx, "backend", "web", "http://127.0.0.1:7422/a2a/backend"+a2asrv.WellKnownAgentCardPath); !errors.Is(err, store.ErrExists) {
		t.Errorf("invite under a taken name: %v, want ErrExists", err)
	}

	if _, err := b.RedeemInvite(ctx, "backend", "toli_NOPE", webCard, "x"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown token: %v, want ErrNotFound", err)
	}
	s3, _, _ := b.CreateInvite(ctx, "backend", "gone", "http://127.0.0.1:7422/a2a/backend"+a2asrv.WellKnownAgentCardPath)
	if _, err := b.RedeemInvite(ctx, "backend", inviteToken(t, s3), "http://127.0.0.1:1"+a2asrv.WellKnownAgentCardPath, "x"); !errors.Is(err, peers.ErrUnreachable) {
		t.Errorf("unreachable card: %v, want ErrUnreachable", err)
	}
}

func TestAcceptInvite(t *testing.T) {
	ctx := context.Background()
	b := setup(t)
	addAgent(t, b, "backend") // accepts
	own := a2atest.NewFakePeer(t).URL + a2asrv.WellKnownAgentCardPath

	var (
		mu     sync.Mutex
		status = http.StatusOK
		got    []backend.InviteRequest
		auth   []string
	)
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	card := &a2a.AgentCard{Name: "web", Version: "1", Skills: []a2a.AgentSkill{},
		DefaultInputModes: []string{"text/plain"}, DefaultOutputModes: []string{"text/plain"},
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(srv.URL+"/a2a/web/", a2a.TransportProtocolJSONRPC)}}
	mux.Handle("/a2a/web"+a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(card))
	mux.HandleFunc("POST /a2a/web/invite", func(w http.ResponseWriter, r *http.Request) {
		var req backend.InviteRequest
		json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		got, auth = append(got, req), append(auth, r.Header.Get("Authorization"))
		st := status
		mu.Unlock()
		w.WriteHeader(st)
		if st != http.StatusOK {
			json.NewEncoder(w).Encode(map[string]string{"error": "nope"})
		}
	})
	inv := func(tok string) string {
		b, _ := json.Marshal(map[string]any{"v": 1, "agent": "web", "card": srv.URL + "/a2a/web" + a2asrv.WellKnownAgentCardPath, "token": tok})
		return "toli-invite:" + base64.RawURLEncoding.EncodeToString(b)
	}
	peerNamed := func(name string) (store.Peer, bool) {
		list, _ := b.ListPeers(ctx, "backend")
		for _, p := range list {
			if p.Name == name {
				return p, true
			}
		}
		return store.Peer{}, false
	}

	if _, err := b.AcceptInvite(ctx, "not an invite", "backend", "", own); !errors.Is(err, backend.ErrInvalidInvite) {
		t.Errorf("garbage: %v, want ErrInvalidInvite", err)
	}

	// Refused: the new peer goes again.
	status = http.StatusNotFound
	if _, err := b.AcceptInvite(ctx, inv("toli_A1"), "backend", "", own); err == nil || strings.Contains(err.Error(), "run the same accept again") {
		t.Fatalf("refused: %v", err)
	}
	if _, ok := peerNamed("web"); ok {
		t.Fatal("a refused accept left its peer")
	}

	// Unclear: the peer stays, and the error says to retry.
	status = http.StatusInternalServerError
	if _, err := b.AcceptInvite(ctx, inv("toli_A2"), "backend", "", own); err == nil || !strings.Contains(err.Error(), "run the same accept again") {
		t.Fatalf("unclear: %v", err)
	}
	first, ok := peerNamed("web")
	if !ok || first.OutboundToken != "toli_A2" {
		t.Fatalf("after an unclear failure: %+v, %v", first, ok)
	}

	// The retry reuses that peer, and sends a new token for it.
	status = http.StatusOK
	p, err := b.AcceptInvite(ctx, inv("toli_A2"), "backend", "", own)
	if err != nil || p.ID != first.ID {
		t.Fatalf("retry: %+v, %v; want peer %d", p, err, first.ID)
	}
	mu.Lock()
	defer mu.Unlock()
	n := len(got)
	if got[n-1].Card != own || got[n-1].Token == got[n-2].Token || auth[n-1] != "Bearer toli_A2" {
		t.Errorf("call backs: %+v, auth %v", got, auth)
	}
	if caller, err := b.PeerByToken(ctx, got[n-1].Token); err != nil || caller.ID != p.ID {
		t.Errorf("the token sent back doesn't identify the peer: %v", err)
	}
	if _, err := b.PeerByToken(ctx, got[n-2].Token); err == nil {
		t.Error("the token from the unclear try still works")
	}
}

// inviteToken reads the token out of an invite string.
func inviteToken(t *testing.T, s string) string {
	t.Helper()
	var inv struct{ Token string }
	raw, _ := strings.CutPrefix(s, "toli-invite:")
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || json.Unmarshal(b, &inv) != nil || inv.Token == "" {
		t.Fatalf("invite %q: %v", s, err)
	}
	return inv.Token
}

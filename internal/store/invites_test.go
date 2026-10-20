package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestInvites(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	addAgent(t, s, "backend")
	addAgent(t, s, "frontend")
	day := time.Now().Add(24 * time.Hour)
	add := func(token, name string, expires time.Time) {
		t.Helper()
		if err := s.AddInvite(ctx, Invite{Agent: "backend", Name: name, TokenHash: token, ExpiresAt: expires}); err != nil {
			t.Fatal(err)
		}
	}
	bob := Peer{Agent: "backend", URL: "https://bob", OutboundToken: "from-bob"}

	if err := s.AddInvite(ctx, Invite{Agent: "nobody", Name: "x", TokenHash: "y", ExpiresAt: day}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("invite for an unknown agent: %v, want ErrNotFound", err)
	}

	// Redeeming makes the peer, holding the promised token, and uses the invite up.
	add("t1", "bob", day)
	if _, err := s.RedeemInvite(ctx, "wrong", bob); !errors.Is(err, ErrNotFound) {
		t.Errorf("wrong token: %v, want ErrNotFound", err)
	}
	if _, err := s.RedeemInvite(ctx, "t1", Peer{Agent: "frontend", Name: "bob", URL: "x"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("another agent's invite: %v, want ErrNotFound", err)
	}
	redeemed, err := s.RedeemInvite(ctx, "t1", bob)
	if err != nil {
		t.Fatal(err)
	}
	id := redeemed.ID
	if p, err := s.PeerByInboundTokenHash(ctx, "t1"); err != nil || p.ID != id || p.Name != "bob" { // named by the invite
		t.Fatalf("peer for the promised token: %+v, %v", p, err)
	}

	// Accepting again (a retry after a lost reply) is safe: same peer, its
	// new token from them stored. Another peer can't use the invite.
	retry := bob
	retry.OutboundToken = "from-bob-2"
	if again, err := s.RedeemInvite(ctx, "t1", retry); err != nil || again.ID != id || again.OutboundToken != "from-bob-2" {
		t.Fatalf("retry: %+v, %v; want peer %d with the new token", again, err, id)
	}
	if _, err := s.RedeemInvite(ctx, "t1", Peer{Agent: "backend", Name: "mallory", URL: "https://mallory"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("a used invite for another peer: %v, want ErrNotFound", err)
	}

	// Once that peer is removed, the used invite can't bring it back.
	if _, err := s.RemovePeer(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedeemInvite(ctx, "t1", bob); !errors.Is(err, ErrNotFound) {
		t.Errorf("retry after the peer was removed: %v, want ErrNotFound", err)
	}

	// An expired invite can't be used.
	add("t2", "carol", time.Now().Add(-time.Minute))
	if _, err := s.RedeemInvite(ctx, "t2", Peer{Agent: "backend", Name: "carol", URL: "x"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired: %v, want ErrNotFound", err)
	}

	// The invite names the peer: a name that's taken can't be invited, and
	// one taken since makes redeeming fail, leaving the invite usable.
	if _, err := s.AddPeer(ctx, Peer{Agent: "backend", Name: "taken", URL: "https://other"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.AddInvite(ctx, Invite{Agent: "backend", Name: "taken", TokenHash: "tx", ExpiresAt: day}); !errors.Is(err, ErrExists) {
		t.Errorf("invite under a taken name: %v, want ErrExists", err)
	}
	add("t3", "erin", day)
	if _, err := s.AddPeer(ctx, Peer{Agent: "backend", Name: "erin", URL: "https://someone-else"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedeemInvite(ctx, "t3", Peer{Agent: "backend", URL: "https://erin"}); !errors.Is(err, ErrExists) {
		t.Fatalf("name taken since: %v, want ErrExists", err)
	}
	if _, err := s.RemovePeer(ctx, mustPeerID(t, s, "backend", "erin")); err != nil {
		t.Fatal(err)
	}
	if p, err := s.RedeemInvite(ctx, "t3", Peer{Agent: "backend", URL: "https://erin"}); err != nil || p.Name != "erin" {
		t.Fatalf("after the clash: %+v, %v", p, err)
	}

	// Removing the agent removes its invites.
	add("t4", "dave", day)
	if _, err := s.RemoveAgent(ctx, "backend"); err != nil {
		t.Fatal(err)
	}
	var n int
	s.db.QueryRow("SELECT COUNT(*) FROM invites").Scan(&n)
	if n != 0 {
		t.Errorf("%d invites left after removing their agent", n)
	}
}

// mustPeerID is the id of agent's peer called name.
func mustPeerID(t *testing.T, s *Store, agent, name string) int64 {
	t.Helper()
	peers, err := s.ListPeers(context.Background(), agent)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range peers {
		if p.Name == name {
			return p.ID
		}
	}
	t.Fatalf("no peer %s for %s", name, agent)
	return 0
}

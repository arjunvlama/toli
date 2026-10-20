package backend

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/arjunvlama/toli/internal/store"
	"github.com/arjunvlama/toli/internal/token"
)

// toli peer invite gives a string; toli peer accept adds the inviting agent
// as a peer, then calls the inviting daemon back (POST <A2A base>/invite) so
// it adds the accepting agent in turn.

const (
	inviteTTL     = 24 * time.Hour
	invitePrefix  = "toli-invite:"
	inviteTimeout = 30 * time.Second
)

var ErrInvalidInvite = errors.New("invalid invite")

type invite struct {
	V     int    `json:"v"`
	Agent string `json:"agent"`
	Card  string `json:"card"`
	Token string `json:"token"` // to accept, then to call the inviting agent
}

func (inv invite) String() string {
	b, _ := json.Marshal(inv)
	return invitePrefix + base64.RawURLEncoding.EncodeToString(b)
}

func parseInvite(s string) (invite, error) {
	raw, ok := strings.CutPrefix(strings.TrimSpace(s), invitePrefix)
	if !ok {
		return invite{}, fmt.Errorf("%w: it should start with %s", ErrInvalidInvite, invitePrefix)
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	var inv invite
	if err != nil || json.Unmarshal(b, &inv) != nil || inv.V != 1 || inv.Agent == "" || inv.Token == "" ||
		!strings.HasSuffix(inv.Card, a2asrv.WellKnownAgentCardPath) {
		return invite{}, fmt.Errorf("%w: copy the whole string", ErrInvalidInvite)
	}
	return inv, nil
}

// CreateInvite makes a single-use invite to agent (card at cardURL); whoever
// accepts it becomes agent's peer called name.
func (b *Backend) CreateInvite(ctx context.Context, agent, name, cardURL string) (string, time.Time, error) {
	if err := checkName(name); err != nil {
		return "", time.Time{}, err
	}
	if cardURL == "" {
		return "", time.Time{}, errors.New("this daemon has no A2A address (`--a2a-listen`), so no one could reach the agent")
	}
	tok, hash := token.New()
	expires := time.Now().Add(inviteTTL)
	if err := b.store.AddInvite(ctx, store.Invite{Agent: agent, Name: name, TokenHash: hash, ExpiresAt: expires}); err != nil {
		return "", time.Time{}, fmt.Errorf("invite for agent '%s': %w", agent, err)
	}
	return invite{V: 1, Agent: agent, Card: cardURL, Token: tok}.String(), expires, nil
}

// RedeemInvite is the inviting side of an accept: it adds the agent at
// cardURL as agent's peer.
func (b *Backend) RedeemInvite(ctx context.Context, agent, tok, cardURL, peerToken string) (store.Peer, error) {
	return b.peers.AddByInvite(ctx, token.Hash(tok), agent, cardURL, peerToken)
}

// AcceptInvite adds the inviting agent as agent's peer, then calls its
// daemon back. If the call surely failed, the new peer is removed; if it's
// unclear, the peer is kept for a retry.
func (b *Backend) AcceptInvite(ctx context.Context, s, agent, name, cardURL string) (store.Peer, error) {
	inv, err := parseInvite(s)
	if err != nil {
		return store.Peer{}, err
	}
	if cardURL == "" {
		return store.Peer{}, errors.New("this daemon has no A2A address (`--a2a-listen`), so the other side couldn't reach your agent")
	}
	if name == "" {
		name = inv.Agent
	}
	if err := checkName(name); err != nil {
		return store.Peer{}, err
	}

	// A retry reuses the peer the first try made.
	var (
		peer    store.Peer
		ourTok  string
		created bool
	)
	existing, err := b.peers.List(ctx, agent)
	if err != nil {
		return store.Peer{}, err
	}
	for _, p := range existing {
		if p.URL == inv.Card {
			peer = p
		}
	}
	if peer.ID != 0 {
		if _, ourTok, err = b.RotatePeerToken(ctx, peer.ID); err != nil {
			return store.Peer{}, err
		}
		if err := b.peers.SetToken(ctx, peer.ID, inv.Token); err != nil {
			return store.Peer{}, err
		}
	} else {
		if peer, ourTok, err = b.AddPeer(ctx, agent, name, inv.Card, inv.Token); err != nil {
			return store.Peer{}, err
		}
		created = true
	}

	sure, err := callInviter(ctx, inv, cardURL, ourTok)
	switch {
	case err == nil:
		// The first check ran before the inviting daemon had our token.
		b.peers.CheckSoon(peer.ID)
		return peer, nil
	case sure:
		if created {
			if rmErr := b.peers.Remove(context.WithoutCancel(ctx), peer.ID); rmErr != nil {
				return store.Peer{}, fmt.Errorf("%w (and removing peer '%s' again failed: %w)", err, peer.Name, rmErr)
			}
		}
		return store.Peer{}, err
	default:
		return store.Peer{}, fmt.Errorf("couldn't confirm the pairing with agent '%s' (%w); run the same accept again", inv.Agent, err)
	}
}

// InviteRequest is the call back to the inviting daemon.
type InviteRequest struct {
	Card  string `json:"card"`
	Token string `json:"token"`
}

// No redirects: the invite's token goes only to the invite's address.
var inviteClient = &http.Client{
	Timeout:       inviteTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// callInviter reports sure if a failure certainly didn't go through (a 4xx
// or no connection), not maybe (timeout, broken connection, 5xx).
func callInviter(ctx context.Context, inv invite, cardURL, ourTok string) (sure bool, err error) {
	endpoint := strings.TrimSuffix(inv.Card, a2asrv.WellKnownAgentCardPath) + "/invite"
	body, err := json.Marshal(InviteRequest{Card: cardURL, Token: ourTok})
	if err != nil {
		return true, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return true, fmt.Errorf("%w: bad card URL", ErrInvalidInvite)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+inv.Token)
	resp, err := inviteClient.Do(req)
	if err != nil {
		var opErr *net.OpError
		connected := !(errors.As(err, &opErr) && opErr.Op == "dial")
		return !connected, fmt.Errorf("reach the daemon of agent '%s': %w", inv.Agent, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		return false, nil
	}
	var e struct{ Error string }
	json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&e)
	if e.Error == "" {
		e.Error = resp.Status
	}
	return resp.StatusCode/100 == 4, fmt.Errorf("daemon of agent '%s': %s", inv.Agent, e.Error)
}

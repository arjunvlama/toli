// Package peerclient keeps one ready A2A client per peer (Client is the
// taskclient.PeerResolver) and periodically re-fetches each peer's Agent Card
// (Run), which also tells us whether the peer is reachable.
package peerclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	"github.com/a2aproject/a2a-go/v2/a2acompat/a2av0"

	"github.com/arjunvlama/toli/internal/store"
)

// Options configure a Pool.
type Options struct {
	CheckEvery time.Duration // how often Run re-fetches every peer's card (default 30s)
	// OnCardChange saves a peer's new card when a check finds it changed.
	OnCardChange func(ctx context.Context, peerID int64, card *a2a.AgentCard) error
}

// Status is what the last card check found (zero: not checked yet). In
// memory only.
type Status struct {
	LastSeen    time.Time // last successful card fetch; zero if never
	LastError   error     // error from the last check, nil if it worked
	LastErrorAt time.Time // when LastError happened
}

// Pool is safe for concurrent use.
type Pool struct {
	opts Options

	mu      sync.RWMutex
	entries map[int64]entry

	soon chan int64
}

// entry is a peer as last given to the pool, its client, and what the last
// card check found (zero Status: not checked yet).
type entry struct {
	peer   store.Peer
	client *a2aclient.Client
	status Status
}

// New builds a client for each peer from its saved card.
func New(ctx context.Context, peers []store.Peer, opts Options) *Pool {
	if opts.CheckEvery == 0 {
		opts.CheckEvery = 30 * time.Second
	}

	p := &Pool{
		opts:    opts,
		entries: map[int64]entry{},
		soon:    make(chan int64, 16),
	}

	for _, peer := range peers {
		c, err := NewClient(ctx, peer.Card, peer.OutboundToken)
		if err != nil {
			// One bad peer shouldn't stop the daemon; it just has no client until fixed.
			slog.Warn("peerclient: can't build client; skipping", "peer", peer.ID, "name", peer.Name, "err", err)
			continue
		}
		p.entries[peer.ID] = entry{peer: peer, client: c}
	}

	return p
}

// Client returns the ready A2A client for a peer. It's the
// taskclient.PeerResolver. Unknown peer -> store.ErrNotFound.
func (p *Pool) Client(ctx context.Context, peerID int64) (*a2aclient.Client, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.entries[peerID]
	if !ok {
		return nil, store.ErrNotFound
	}
	return e.client, nil
}

// Status returns what the last card check found for a peer.
func (p *Pool) Status(peerID int64) (Status, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.entries[peerID]
	if !ok {
		return Status{}, store.ErrNotFound
	}
	return e.status, nil
}

// Put makes c the client for peer, replacing (and destroying) any existing one.
func (p *Pool) Put(peer store.Peer, c *a2aclient.Client) {
	p.mu.Lock()
	old := p.entries[peer.ID]
	p.entries[peer.ID] = entry{peer: peer, client: c}
	p.mu.Unlock()

	if old.client != c {
		destroy(old.client, peer.ID)
	}
}

// TakeAgent removes the clients of every peer belonging to a local agent
// without destroying them, and returns them keyed by peer ID. The caller must
// Destroy them when done.
func (p *Pool) TakeAgent(agent string) map[int64]*a2aclient.Client {
	p.mu.Lock()
	defer p.mu.Unlock()
	taken := map[int64]*a2aclient.Client{}
	for id, e := range p.entries {
		if e.peer.Agent == agent {
			taken[e.peer.ID] = e.client
			delete(p.entries, id)
		}
	}
	return taken
}

// Drop discards a peer's client.
func (p *Pool) Drop(peerID int64) {
	destroy(p.Take(peerID), peerID)
}

// Take removes a peer's client from the pool without destroying it, and
// returns it (nil if there was none). The caller must Destroy it when done.
func (p *Pool) Take(peerID int64) *a2aclient.Client {
	p.mu.Lock()
	defer p.mu.Unlock()
	old := p.entries[peerID]
	delete(p.entries, peerID)
	return old.client
}

func destroy(c *a2aclient.Client, peerID int64) {
	if c == nil {
		return
	}
	if err := c.Destroy(); err != nil {
		slog.Warn("peerclient: destroy client", "peer", peerID, "err", err)
	}
}

// Run checks every peer now and every Options.CheckEvery until ctx ends.
// Run it in the daemon's errgroup.
func (p *Pool) Run(ctx context.Context) error {
	ticker := time.NewTicker(p.opts.CheckEvery)
	defer ticker.Stop()

	for {
		p.mu.RLock()
		ids := make([]int64, 0, len(p.entries))
		for id := range p.entries {
			ids = append(ids, id)
		}
		p.mu.RUnlock()

		for _, id := range ids {
			if err := p.check(ctx, id); err != nil {
				slog.Warn("peerclient: check", "peer", id, "err", err)
			}
		}
		ticker.Reset(p.opts.CheckEvery)

		for waiting := true; waiting; {
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
				waiting = false
			case id := <-p.soon:
				if err := p.check(ctx, id); err != nil {
					slog.Warn("peerclient: check", "peer", id, "err", err)
				}
			}
		}
	}
}

// ErrNoToken is a peer's status when it requires a token and we have none.
var ErrNoToken = errors.New("no token for this peer")

// CheckSoon asks Run to check a peer now.
func (p *Pool) CheckSoon(peerID int64) {
	select {
	case p.soon <- peerID:
	default:
	}
}

// check re-fetches one peer's card and applies any change.
func (p *Pool) check(ctx context.Context, peerID int64) error {
	p.mu.RLock()
	e, ok := p.entries[peerID]
	p.mu.RUnlock()

	if !ok {
		return nil // peer has been deleted/dropped
	}

	defer func(origE entry) {
		var shouldDestroy *a2aclient.Client
		p.mu.Lock()
		cur, ok := p.entries[peerID]
		if ok && cur.status == origE.status {
			p.entries[peerID] = e
			if cur.client != e.client {
				shouldDestroy = cur.client
			}
		} else if cur.client != e.client && e.client != origE.client {
			shouldDestroy = e.client
		}
		p.mu.Unlock()
		destroy(shouldDestroy, peerID)
	}(e)

	public, err := FetchCard(ctx, e.peer.URL)
	if err != nil {
		err = fmt.Errorf("fetch agent card for peer '%s' from %s: %w", e.peer.Name, e.peer.URL, err)
		e.status.LastError, e.status.LastErrorAt = err, time.Now()
		return err
	}
	if err := e.client.UpdateCard(public); err != nil {
		// The peer's interface changed, so it needs a new client.
		c, err := NewClient(ctx, public, e.peer.OutboundToken)
		if err != nil {
			err = fmt.Errorf("connect to peer '%s' with its new agent card: %w", e.peer.Name, err)
			e.status.LastError, e.status.LastErrorAt = err, time.Now()
			return err
		}
		e.client = c
	}

	// Getting the extended card proves our token works.
	card := public
	switch {
	case public.Capabilities.ExtendedAgentCard && e.peer.OutboundToken != "":
		card, err = e.client.GetExtendedAgentCard(ctx, &a2a.GetExtendedAgentCardRequest{})
		if err != nil {
			err = fmt.Errorf("peer '%s': get extended agent card: %w", e.peer.Name, err)
			e.status.LastError, e.status.LastErrorAt = err, time.Now()
			return err
		}
		if err := e.client.UpdateCard(card); err != nil {
			err = fmt.Errorf("peer '%s': extended agent card: %w", e.peer.Name, err)
			e.status.LastError, e.status.LastErrorAt = err, time.Now()
			return err
		}
	case e.peer.OutboundToken == "" && len(public.SecurityRequirements) > 0:
		e.status.LastError, e.status.LastErrorAt = ErrNoToken, time.Now()
		return fmt.Errorf("peer '%s': %w", e.peer.Name, ErrNoToken)
	}

	if !sameCard(e.peer.Card, card) && p.opts.OnCardChange != nil {
		if err := p.opts.OnCardChange(ctx, peerID, card); err != nil {
			e.status = Status{LastSeen: time.Now()}
			return fmt.Errorf("save new agent card for peer '%s': %w", e.peer.Name, err)
		}
	}
	e.peer.Card = card
	e.status = Status{LastSeen: time.Now()}
	return nil
}

func sameCard(a, b *a2a.AgentCard) bool {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(ja, jb)
}

// cardResolver is the SDK's default, but also reads A2A 0.3 cards (as 1.0
// ones, their interfaces marked 0.3).
var cardResolver = func() *agentcard.Resolver {
	r := *agentcard.DefaultResolver
	r.CardParser = a2av0.NewAgentCardParser()
	return &r
}()

// FetchCard downloads a peer's Agent Card.
func FetchCard(ctx context.Context, url string) (*a2a.AgentCard, error) {
	return cardResolver.Resolve(ctx, url)
}

// NewClient builds an A2A client from a peer's card that sends our token for
// that peer on every call.
func NewClient(ctx context.Context, card *a2a.AgentCard, outboundToken string) (*a2aclient.Client, error) {
	return a2aclient.NewFromCard(ctx, card,
		a2aclient.WithCallInterceptors(bearer{token: outboundToken}),
		a2av0.WithJSONRPCTransport(a2av0.JSONRPCTransportConfig{}), // for peers on A2A 0.3
	)
}

// bearer adds "Authorization: Bearer <token>" to every call. (The SDK's
// AuthInterceptor works from the card's security schemes plus a per-request
// session ID; we just have one token per peer.)
type bearer struct {
	a2aclient.PassthroughInterceptor
	token string
}

func (b bearer) Before(ctx context.Context, req *a2aclient.Request) (context.Context, any, error) {
	if b.token != "" {
		req.ServiceParams["Authorization"] = []string{"Bearer " + b.token}
	}
	return ctx, nil, nil
}

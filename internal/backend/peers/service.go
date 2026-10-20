// Package peers owns a local agent's peers: their rows in the store and their
// A2A clients in the pool. It's the only code that touches the pool.
package peers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"

	"github.com/arjunvlama/toli/internal/peerclient"
	"github.com/arjunvlama/toli/internal/store"
	"github.com/arjunvlama/toli/internal/taskclient"
	"github.com/arjunvlama/toli/internal/token"
)

// ErrUnreachable: a peer's card couldn't be fetched.
var ErrUnreachable = errors.New("peer unreachable")

// orphanCancelTimeout bounds the cancels sent for tasks deleted along with
// their peer or session.
const orphanCancelTimeout = time.Minute

// Service owns peers: their rows and their clients in the pool. Anything
// that changes peers goes through it, under mu, so the store and the pool
// change together. Reads take the read lock; the task client's lookups go
// straight to the pool and never wait on this.
type Service struct {
	store *store.Store
	pool  *peerclient.Pool
	mu    sync.RWMutex
}

// New loads every stored peer and builds its client. checkEvery is how often
// peers' cards are re-fetched (0 for the default).
func New(ctx context.Context, st *store.Store, checkEvery time.Duration) (*Service, error) {
	peers, err := st.ListAllPeers(ctx)
	if err != nil {
		return nil, fmt.Errorf("load peers: %w", err)
	}
	ps := &Service{store: st}
	ps.pool = peerclient.New(ctx, peers, peerclient.Options{CheckEvery: checkEvery, OnCardChange: ps.saveCard})
	return ps, nil
}

// Client returns a peer's A2A client: the taskclient.PeerResolver. It only
// takes the pool's lock, never mu.
func (ps *Service) Client(ctx context.Context, peerID int64) (*a2aclient.Client, error) {
	return ps.pool.Client(ctx, peerID)
}

// Status returns what the pool's last card check found for a peer.
func (ps *Service) Status(peerID int64) (peerclient.Status, error) {
	return ps.pool.Status(peerID)
}

// Run runs the pool's card checker until ctx ends.
func (ps *Service) Run(ctx context.Context) error {
	return ps.pool.Run(ctx)
}

// saveCard persists a card the pool's checker found changed.
func (ps *Service) saveCard(ctx context.Context, peerID int64, card *a2a.AgentCard) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.store.UpdatePeerCard(ctx, peerID, card)
}

// Add fetches a peer's card, builds its client, and stores it on agent's
// allowlist. Returns the peer and the token we issue to them.
func (ps *Service) Add(ctx context.Context, agent, name, url, outboundToken string) (store.Peer, string, error) {
	card, err := peerclient.FetchCard(ctx, url)
	if err != nil {
		return store.Peer{}, "", fmt.Errorf("fetch agent card from %s: %w", url, err)
	}

	client, err := peerclient.NewClient(ctx, card, outboundToken)
	if err != nil {
		return store.Peer{}, "", fmt.Errorf("connect to peer at %s: %w", url, err)
	}

	peer := store.Peer{
		Agent:         agent,
		Name:          name,
		URL:           url,
		Card:          card,
		OutboundToken: outboundToken,
		CreatedAt:     time.Now(),
	}
	inbound, hash := token.New()

	ps.mu.Lock()
	defer ps.mu.Unlock()
	peer.ID, err = ps.store.AddPeer(ctx, peer, hash)
	if err != nil {
		client.Destroy()
		return store.Peer{}, "", fmt.Errorf("add peer '%s': %w", name, err)
	}
	ps.pool.Put(peer, client)
	ps.pool.CheckSoon(peer.ID)
	return peer, inbound, nil
}

// AddByInvite is Add for a peer that accepted one of agent's invites.
func (ps *Service) AddByInvite(ctx context.Context, tokenHash, agent, url, outboundToken string) (store.Peer, error) {
	// Before fetching anything, so a stranger can't make us fetch URLs.
	if err := ps.store.CheckInvite(ctx, tokenHash, agent); err != nil {
		return store.Peer{}, err
	}
	card, err := peerclient.FetchCard(ctx, url)
	if err != nil {
		return store.Peer{}, fmt.Errorf("%w: fetch agent card from %s: %w", ErrUnreachable, url, err)
	}
	client, err := peerclient.NewClient(ctx, card, outboundToken)
	if err != nil {
		return store.Peer{}, fmt.Errorf("connect to peer at %s: %w", url, err)
	}
	peer := store.Peer{Agent: agent, URL: url, Card: card, OutboundToken: outboundToken, CreatedAt: time.Now()}

	ps.mu.Lock()
	defer ps.mu.Unlock()
	peer, err = ps.store.RedeemInvite(ctx, tokenHash, peer)
	if err != nil {
		client.Destroy()
		return store.Peer{}, err
	}
	ps.pool.Put(peer, client)
	ps.pool.CheckSoon(peer.ID)
	return peer, nil
}

// CheckSoon re-checks a peer's connection now, not at the next round.
func (ps *Service) CheckSoon(peerID int64) {
	ps.pool.CheckSoon(peerID)
}

// SetToken stores the token the peer gave us (if it wasn't known when the
// peer was added, or the peer issued a new one) and rebuilds its client with
// it. The token we issued the peer is unchanged.
func (ps *Service) SetToken(ctx context.Context, peerID int64, outboundToken string) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	peer, err := ps.store.GetPeer(ctx, peerID)
	if err != nil {
		return fmt.Errorf("peer %d: %w", peerID, err)
	}
	client, err := peerclient.NewClient(ctx, peer.Card, outboundToken)
	if err != nil {
		return fmt.Errorf("connect to peer '%s': %w", peer.Name, err)
	}
	if err := ps.store.SetPeerOutboundToken(ctx, peerID, outboundToken); err != nil {
		client.Destroy()
		return fmt.Errorf("set token of peer '%s': %w", peer.Name, err)
	}
	peer.OutboundToken = outboundToken
	ps.pool.Put(peer, client)
	ps.pool.CheckSoon(peerID)
	return nil
}

// Remove deletes a peer, then tells it to cancel the tasks that were still
// active, in the background: the delete has already happened, so nothing
// waits on a slow or unreachable peer.
func (ps *Service) Remove(ctx context.Context, peerID int64) error {
	ps.mu.Lock()
	active, err := ps.store.RemovePeer(ctx, peerID)
	if err != nil {
		ps.mu.Unlock()
		return fmt.Errorf("remove peer %d: %w", peerID, err)
	}
	client := ps.pool.Take(peerID)
	ps.mu.Unlock()

	go cancelOrphans(client, peerID, active)
	return nil
}

// cancelOrphans asks a removed peer to cancel tasks we no longer track, then
// destroys its client. Best effort: failures are only logged.
func cancelOrphans(client *a2aclient.Client, peerID int64, tasks []store.ActiveTask) {
	if client == nil {
		if len(tasks) > 0 {
			slog.Warn("peers: removed peer had no client; tasks not canceled", "peer", peerID, "tasks", len(tasks))
		}
		return
	}
	defer func() {
		if err := client.Destroy(); err != nil {
			slog.Warn("peers: destroy client", "peer", peerID, "err", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), orphanCancelTimeout)
	defer cancel()
	for _, t := range tasks {
		if err := taskclient.CancelUntracked(ctx, client, t.ID); err != nil {
			slog.Warn("peers: cancel task on removed peer", "peer", peerID, "task", t.ID, "err", err)
		}
	}
}

// Get returns a peer by id. Returns store.ErrNotFound if there's no such peer.
func (ps *Service) Get(ctx context.Context, peerID int64) (store.Peer, error) {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.store.GetPeer(ctx, peerID)
}

// ByToken returns the peer we issued tok to (when it was added), or
// store.ErrNotFound. It's how an incoming A2A call is identified.
func (ps *Service) ByToken(ctx context.Context, tok string) (store.Peer, error) {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.store.PeerByInboundTokenHash(ctx, token.Hash(tok))
}

// List returns agent's peers.
func (ps *Service) List(ctx context.Context, agent string) ([]store.Peer, error) {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.store.ListPeers(ctx, agent)
}

// RemoveForAgent runs deleteAgent, whose delete cascades to the agent's peers
// and returns the tasks that were still active, and takes those peers'
// clients, under the lock so the store and pool change together. Then, in the
// background, each peer is asked to cancel its tasks and its client is
// destroyed.
func (ps *Service) RemoveForAgent(ctx context.Context, agent string, deleteAgent func(context.Context) ([]store.ActiveTask, error)) error {
	ps.mu.Lock()
	active, err := deleteAgent(ctx)
	if err != nil {
		ps.mu.Unlock()
		return err
	}
	clients := ps.pool.TakeAgent(agent)
	ps.mu.Unlock()

	byPeer := map[int64][]store.ActiveTask{}
	for _, t := range active {
		byPeer[t.PeerID] = append(byPeer[t.PeerID], t)
	}
	for peerID, client := range clients {
		go cancelOrphans(client, peerID, byPeer[peerID]) // also destroys the client
		delete(byPeer, peerID)
	}
	for peerID, tasks := range byPeer {
		go cancelOrphans(nil, peerID, tasks) // no client: logs that they weren't canceled
	}
	return nil
}

// Cancel asks the peers of tasks we no longer track (deleted with their
// session) to cancel them, in the background. The peers themselves remain, so
// their clients stay in the pool. Best effort: failures are only logged.
func (ps *Service) Cancel(tasks []store.ActiveTask) {
	if len(tasks) == 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), orphanCancelTimeout)
		defer cancel()
		for _, t := range tasks {
			client, err := ps.pool.Client(ctx, t.PeerID)
			if err != nil {
				slog.Warn("peers: no client for peer; task not canceled", "peer", t.PeerID, "task", t.ID, "err", err)
				continue
			}
			if err := taskclient.CancelUntracked(ctx, client, t.ID); err != nil {
				slog.Warn("peers: cancel untracked task", "peer", t.PeerID, "task", t.ID, "err", err)
			}
		}
	}()
}

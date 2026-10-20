package backend

import (
	"context"
	"fmt"

	"github.com/arjunvlama/toli/internal/peerclient"
	"github.com/arjunvlama/toli/internal/store"
	"github.com/arjunvlama/toli/internal/token"
)

// AddPeer puts a peer on a local agent's allowlist. It fetches the peer's
// card first, so a wrong URL or unreachable peer fails here rather than
// later. outboundToken is the token the peer gave us; "" if it hasn't yet
// (set it later with SetPeerToken). Returns the peer and
// the token we issue to them, shown once (only its hash is stored).
func (b *Backend) AddPeer(ctx context.Context, agent, name, url, outboundToken string) (store.Peer, string, error) {
	if err := checkName(name); err != nil {
		return store.Peer{}, "", err
	}
	return b.peers.Add(ctx, agent, name, url, outboundToken)
}

// RotatePeerToken issues a peer a new token for calling the agent, shown
// once (only its hash is stored); the one it has stops working at once.
// Returns the peer's name, and store.ErrNotFound if there's no such peer.
func (b *Backend) RotatePeerToken(ctx context.Context, peerID int64) (string, string, error) {
	peer, err := b.GetPeer(ctx, peerID)
	if err != nil {
		return "", "", err
	}
	tok, hash := token.New()
	if err := b.store.SetPeerInboundTokenHash(ctx, peerID, hash); err != nil {
		return "", "", fmt.Errorf("rotate token for peer '%s': %w", peer.Name, err)
	}
	return peer.Name, tok, nil
}

// SetPeerToken stores the token a peer gave us, for calling it. Returns
// store.ErrNotFound if there's no such peer.
func (b *Backend) SetPeerToken(ctx context.Context, peerID int64, outboundToken string) error {
	return b.peers.SetToken(ctx, peerID, outboundToken)
}

// RemovePeer deletes a peer (its outgoing tasks go with it) and drops its
// client. Returns store.ErrNotFound if there's no such peer.
func (b *Backend) RemovePeer(ctx context.Context, peerID int64) error {
	return b.peers.Remove(ctx, peerID)
}

// GetPeer returns a peer by id. Returns store.ErrNotFound if there's no such
// peer.
func (b *Backend) GetPeer(ctx context.Context, peerID int64) (store.Peer, error) {
	return b.peers.Get(ctx, peerID)
}

// PeerByToken identifies an incoming A2A caller: the peer we issued tok to.
// Returns store.ErrNotFound for an unknown token.
func (b *Backend) PeerByToken(ctx context.Context, tok string) (store.Peer, error) {
	return b.peers.ByToken(ctx, tok)
}

func (b *Backend) ListPeers(ctx context.Context, agent string) ([]store.Peer, error) {
	return b.peers.List(ctx, agent)
}

// PeerStatus returns what the last card check found for a peer.
func (b *Backend) PeerStatus(peerID int64) (peerclient.Status, error) {
	return b.peers.Status(peerID)
}

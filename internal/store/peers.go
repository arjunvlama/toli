package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	sqlite3 "modernc.org/sqlite/lib"
)

// Peer is a remote agent on one local agent's allowlist.
type Peer struct {
	ID            int64
	Agent         string         // the local agent whose allowlist this is on
	Name          string         // local alias ("bob"); the key the agent passes to delegate
	URL           string         // base URL we fetch their Agent Card from
	Card          *a2a.AgentCard // cached copy of their card, as published; nil until fetched
	OutboundToken string         // token the peer gave us, sent when we call them; "" if not set
	CreatedAt     time.Time
}

// ListPeers returns the allowlist of one local agent.
func (s *Store) ListPeers(ctx context.Context, agent string) ([]Peer, error) {
	return s.queryPeers(ctx, "SELECT "+peerColumns+" FROM peers WHERE agent = ? ORDER BY name", agent)
}

// ListAllPeers returns every peer of every local agent.
func (s *Store) ListAllPeers(ctx context.Context) ([]Peer, error) {
	return s.queryPeers(ctx, "SELECT "+peerColumns+" FROM peers ORDER BY id")
}

// queryPeers runs a peer query and scans every row.
func (s *Store) queryPeers(ctx context.Context, query string, args ...any) ([]Peer, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var peers []Peer
	for rows.Next() {
		p, err := scanPeer(rows)
		if err != nil {
			return nil, err
		}
		peers = append(peers, p)
	}
	return peers, rows.Err()
}

// GetPeer looks a peer up by id.
func (s *Store) GetPeer(ctx context.Context, id int64) (Peer, error) {
	return onePeer(s.db.QueryRowContext(ctx, "SELECT "+peerColumns+" FROM peers WHERE id = ?", id))
}

// PeerByInboundTokenHash resolves an incoming A2A caller's token to the peer
// it was issued to.
func (s *Store) PeerByInboundTokenHash(ctx context.Context, hash string) (Peer, error) {
	return onePeer(s.db.QueryRowContext(ctx, "SELECT "+peerColumns+" FROM peers WHERE inbound_token_hash = ?", hash))
}

// AddPeer puts a remote agent on a local agent's allowlist and returns its
// id. inboundTokenHash is the hash of the token we issue to the peer ("" for
// none yet). Returns ErrExists if that agent already has a peer by this name,
// and ErrNotFound if the local agent doesn't exist.
func (s *Store) AddPeer(ctx context.Context, p Peer, inboundTokenHash string) (int64, error) {
	return insertPeer(ctx, s.db, p, inboundTokenHash)
}

// execer is what *sql.DB and *sql.Tx have in common for writes.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// insertPeer is AddPeer, on the database or in a transaction.
func insertPeer(ctx context.Context, db execer, p Peer, inboundTokenHash string) (int64, error) {
	var card sql.NullString // NULL until the peer's card has been fetched
	if p.Card != nil {
		b, err := json.Marshal(p.Card)
		if err != nil {
			return 0, fmt.Errorf("encode card: %w", err)
		}
		card = sql.NullString{String: string(b), Valid: true}
	}

	res, err := db.ExecContext(ctx,
		`INSERT INTO peers (agent, name, url, card, outbound_token, inbound_token_hash, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		p.Agent, p.Name, p.URL, card, nullIfEmpty(p.OutboundToken), nullIfEmpty(inboundTokenHash), time.Now().Unix(),
	)
	switch {
	case isConstraint(err, sqlite3.SQLITE_CONSTRAINT_UNIQUE):
		return 0, ErrExists // (agent, name) taken
	case isConstraint(err, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY):
		return 0, ErrNotFound // no local agent by that name
	case err != nil:
		return 0, err
	}
	return res.LastInsertId()
}

// peerColumns is the column list scanPeer expects, in order.
const peerColumns = "id, agent, name, url, card, outbound_token, created_at"

func scanPeer(sc scanner) (Peer, error) {
	var (
		p        Peer
		card     sql.NullString
		outbound sql.NullString
		created  int64
	)
	if err := sc.Scan(&p.ID, &p.Agent, &p.Name, &p.URL, &card, &outbound, &created); err != nil {
		return Peer{}, err
	}
	if card.Valid {
		p.Card = new(a2a.AgentCard)
		if err := json.Unmarshal([]byte(card.String), p.Card); err != nil {
			return Peer{}, fmt.Errorf("decode card for peer '%s': %w", p.Name, err)
		}
	}
	p.OutboundToken = outbound.String
	p.CreatedAt = time.Unix(created, 0)
	return p, nil
}

// onePeer scans a single-row lookup, mapping "no row" to ErrNotFound.
func onePeer(row *sql.Row) (Peer, error) {
	p, err := scanPeer(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Peer{}, ErrNotFound
	}
	return p, err
}

// nullIfEmpty stores "" as SQL NULL (so UNIQUE columns allow many unset rows).
func nullIfEmpty(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

// RemovePeer deletes a peer; its outgoing tasks are deleted with it (cascade).
// It returns the tasks that were still active, read in the same transaction,
// so the caller can cancel them on the peer. Returns ErrNotFound if there's
// no such peer.
func (s *Store) RemovePeer(ctx context.Context, id int64) ([]ActiveTask, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	active, err := activeTasks(ctx, tx, "peer_id = ?", id)
	if err != nil {
		return nil, fmt.Errorf("list tasks of peer %d: %w", id, err)
	}
	if err := oneRow(tx.ExecContext(ctx, "DELETE FROM peers WHERE id = ?", id)); err != nil {
		return nil, err
	}
	return active, tx.Commit()
}

// UpdatePeerCard replaces a peer's cached Agent Card (after a refresh found
// it changed). Returns ErrNotFound if there's no such peer.
func (s *Store) UpdatePeerCard(ctx context.Context, id int64, card *a2a.AgentCard) error {
	b, err := json.Marshal(card)
	if err != nil {
		return fmt.Errorf("encode card: %w", err)
	}
	return oneRow(s.db.ExecContext(ctx, "UPDATE peers SET card = ? WHERE id = ?", string(b), id))
}

// SetPeerOutboundToken stores the token the peer gave us. Returns ErrNotFound
// if there's no such peer.
func (s *Store) SetPeerOutboundToken(ctx context.Context, id int64, tok string) error {
	return oneRow(s.db.ExecContext(ctx, "UPDATE peers SET outbound_token = ? WHERE id = ?", nullIfEmpty(tok), id))
}

// SetPeerInboundTokenHash replaces the token we issued a peer (by its hash);
// the old one stops working. Returns ErrNotFound if there's no such peer.
func (s *Store) SetPeerInboundTokenHash(ctx context.Context, id int64, hash string) error {
	return oneRow(s.db.ExecContext(ctx, "UPDATE peers SET inbound_token_hash = ? WHERE id = ?", hash, id))
}

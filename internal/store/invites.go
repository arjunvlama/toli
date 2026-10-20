package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	sqlite3 "modernc.org/sqlite/lib"
)

// Invite: whoever accepts it becomes Agent's peer called Name.
type Invite struct {
	Agent     string
	Name      string
	TokenHash string
	ExpiresAt time.Time
}

// AddInvite stores an invite and drops expired ones. ErrExists if the name
// is taken.
func (s *Store) AddInvite(ctx context.Context, inv Invite) error {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM invites WHERE expires_at <= ?", time.Now().Unix()); err != nil {
		return err
	}
	var taken bool
	if err := s.db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM peers WHERE agent = ? AND name = ?)", inv.Agent, inv.Name).Scan(&taken); err != nil {
		return err
	}
	if taken {
		return ErrExists
	}
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO invites (token_hash, agent, name, expires_at) VALUES (?, ?, ?, ?)",
		inv.TokenHash, inv.Agent, inv.Name, inv.ExpiresAt.Unix())
	if isConstraint(err, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY) {
		return ErrNotFound
	}
	return err
}

// CheckInvite returns ErrNotFound unless agent has an invite with tokenHash
// that hasn't expired.
func (s *Store) CheckInvite(ctx context.Context, tokenHash, agent string) error {
	var ok bool
	err := s.db.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT 1 FROM invites WHERE token_hash = ? AND agent = ? AND expires_at > ?)",
		tokenHash, agent, time.Now().Unix()).Scan(&ok)
	if err == nil && !ok {
		return ErrNotFound
	}
	return err
}

// RedeemInvite adds p as the invite's peer and returns it. A retry for the
// same p.URL only updates its token.
func (s *Store) RedeemInvite(ctx context.Context, tokenHash string, p Peer) (Peer, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Peer{}, err
	}
	defer tx.Rollback()

	var (
		used   bool
		peerID sql.NullInt64
	)
	err = tx.QueryRowContext(ctx,
		"SELECT name, used, peer_id FROM invites WHERE token_hash = ? AND agent = ? AND expires_at > ?",
		tokenHash, p.Agent, time.Now().Unix()).Scan(&p.Name, &used, &peerID)
	if errors.Is(err, sql.ErrNoRows) {
		return Peer{}, ErrNotFound
	}
	if err != nil {
		return Peer{}, err
	}

	id := peerID.Int64
	if used { // a retry
		if !peerID.Valid {
			return Peer{}, ErrNotFound
		}
		res, err := tx.ExecContext(ctx, "UPDATE peers SET outbound_token = ? WHERE id = ? AND url = ?",
			nullIfEmpty(p.OutboundToken), id, p.URL)
		if err := oneRow(res, err); err != nil {
			return Peer{}, err
		}
	} else {
		if id, err = insertPeer(ctx, tx, p, tokenHash); err != nil {
			return Peer{}, err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE invites SET used = 1, peer_id = ? WHERE token_hash = ?", id, tokenHash); err != nil {
			return Peer{}, err
		}
	}
	stored, err := onePeer(tx.QueryRowContext(ctx, "SELECT "+peerColumns+" FROM peers WHERE id = ?", id))
	if err != nil {
		return Peer{}, err
	}
	return stored, tx.Commit()
}

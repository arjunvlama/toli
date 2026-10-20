// Package store is the only package that talks SQL. It exposes the database
// as plain Go functions named after the domain.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"modernc.org/sqlite"
)

var (
	// ErrNotFound is returned when a lookup matches no row.
	ErrNotFound = errors.New("not found")
	// ErrExists is returned when an insert collides with an existing row
	// (modernc sqlite: error code SQLITE_CONSTRAINT_PRIMARYKEY / _UNIQUE).
	ErrExists = errors.New("already exists")
)

// migrations run in order; PRAGMA user_version records how many have been
// applied.
var migrations = []string{
	`CREATE TABLE agents (
		name       TEXT PRIMARY KEY,
		card       TEXT NOT NULL,          -- a2a.AgentCard JSON
		token_hash  TEXT NOT NULL UNIQUE,
		disabled_at INTEGER,               -- set while the human has paused the agent; NULL = enabled
		created_at  INTEGER NOT NULL)`,

	`CREATE TABLE peers (
		id                 INTEGER PRIMARY KEY AUTOINCREMENT, -- never reused: stale ids can't point at a new peer
		agent              TEXT NOT NULL REFERENCES agents(name) ON DELETE CASCADE, -- whose allowlist
		name               TEXT NOT NULL,          -- local alias for the peer
		url                TEXT NOT NULL,
		card               TEXT,                   -- a2a.AgentCard JSON; NULL until fetched
		outbound_token     TEXT,                   -- token the peer gave us; sent when we call them
		inbound_token_hash TEXT UNIQUE,            -- hash of the token we gave them; checked when they call us
		created_at         INTEGER NOT NULL,
		UNIQUE (agent, name))`,

	// Sessions group an agent's outgoing tasks. The session id is the A2A
	// contextId sent with each task.
	`CREATE TABLE sessions (
		id         TEXT PRIMARY KEY,
		agent      TEXT NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
		title      TEXT NOT NULL,
		created_at INTEGER NOT NULL)`,

	// Outgoing tasks: the taskstore.Store. Times are unix milliseconds.
	`CREATE TABLE outgoing_tasks (
		id           TEXT PRIMARY KEY,             -- A2A task id
		peer_id      INTEGER NOT NULL REFERENCES peers(id) ON DELETE CASCADE,
		owner        TEXT NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
		context_id   TEXT REFERENCES sessions(id) ON DELETE CASCADE, -- the session id (a client-supplied contextId); NULL for standalone
		state        TEXT NOT NULL,                -- copy of a2a.Status.State, for filtering
		a2a          TEXT NOT NULL,                -- the peer's a2a.Task JSON, as last reported
		version      INTEGER NOT NULL,             -- +1 per update; optimistic concurrency
		saved_at     INTEGER NOT NULL,
		deadline     INTEGER NOT NULL,
		direct       INTEGER NOT NULL DEFAULT 0,   -- 1 = local record of a direct answer
		created_at   INTEGER NOT NULL)`,
	`CREATE INDEX outgoing_tasks_state ON outgoing_tasks (state)`,
	`CREATE INDEX outgoing_tasks_owner ON outgoing_tasks (owner, context_id)`,
	`CREATE INDEX outgoing_tasks_context ON outgoing_tasks (context_id)`, // for the session delete cascade

	// Incoming tasks: tasks peers sent our agents, stored for a2asrv (the
	// a2asrv taskstore.Store). Each belongs to the peer that created it.
	`CREATE TABLE incoming_tasks (
		id         TEXT PRIMARY KEY,                -- A2A task id
		agent      TEXT NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
		peer_id    INTEGER NOT NULL REFERENCES peers(id) ON DELETE CASCADE,
		context_id TEXT NOT NULL,                   -- the peer's contextId
		state      TEXT NOT NULL,                   -- copy of a2a.Status.State, for filtering
		a2a        TEXT NOT NULL,                   -- the a2a.Task JSON
		version    INTEGER NOT NULL,                -- +1 per update; a2asrv's optimistic concurrency
		created_at INTEGER NOT NULL,                -- unix nanoseconds
		updated_at INTEGER NOT NULL)                -- unix nanoseconds; List orders by it`,
	`CREATE INDEX incoming_tasks_peer ON incoming_tasks (peer_id, updated_at)`,
	`CREATE INDEX incoming_tasks_agent ON incoming_tasks (agent, state)`,

	// Harness sessions: the coding-agent (harness) session each agent is running
	// now, registered by its session-start hook, so the daemon can wake it.
	`CREATE TABLE harness_sessions (
		agent         TEXT PRIMARY KEY REFERENCES agents(name) ON DELETE CASCADE,
		harness       TEXT NOT NULL,               -- e.g. claude-code, codex
		session_id    TEXT NOT NULL,               -- the harness's id for the session
		address       TEXT NOT NULL,               -- JSON: how to reach it (socket, thread, URL, ...)
		registered_at INTEGER NOT NULL)            -- unix seconds`,

	// Where to push an agent's notifications when it has no harness session
	// (set by its human). JSON Notify; NULL = none.
	`ALTER TABLE agents ADD COLUMN notify TEXT`,

	// Invites (toli peer invite). A used one is kept until it expires, so an
	// accept can be retried.
	`CREATE TABLE invites (
		token_hash  TEXT PRIMARY KEY,            -- hash of the token the invite gives
		agent       TEXT NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
		name        TEXT NOT NULL,               -- the peer's name, once accepted
		expires_at  INTEGER NOT NULL,            -- unix seconds
		used        INTEGER NOT NULL DEFAULT 0,  -- 1 once accepted
		peer_id     INTEGER REFERENCES peers(id) ON DELETE SET NULL)  -- the peer it made`,
}

// scanner is what *sql.Row and *sql.Rows have in common, so one scan
// function per type serves both single lookups and lists.
type scanner interface {
	Scan(dest ...any) error
}

func isConstraint(err error, code int) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code() == code
}

type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the database at path and applies pending
// migrations.
func Open(ctx context.Context, path string) (*Store, error) {
	if err := ownerOnly(path); err != nil {
		return nil, err
	}
	var err error
	var s Store
	s.db, err = sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	s.db.SetMaxOpenConns(1)

	err = s.migrate(ctx)
	if err != nil {
		s.db.Close()
		return nil, err
	}
	return &s, nil
}

// ownerOnly makes the database (created if missing) and any -wal and -shm
// files readable by their owner only: they hold tokens and secrets. SQLite
// gives the -wal and -shm files it creates the database's permissions.
func ownerOnly(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	f.Close()
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(p, 0o600); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("make %s owner-only: %w", p, err)
		}
	}
	return nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate(ctx context.Context) error {

	var v int
	err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if v > len(migrations) {
		return fmt.Errorf("database is from a newer toli (schema %d, this build knows %d)", v, len(migrations))
	}
	if v == len(migrations) {
		return nil // up to date
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // this is a no-op if the commit is succesful

	for i, m := range migrations[v:] {
		if _, err := tx.ExecContext(ctx, m); err != nil {
			return fmt.Errorf("migration %d: %w", v+i+1, err)
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", len(migrations))); err != nil {
		return err
	}

	return tx.Commit()
}

// oneRow checks the result of a DELETE or UPDATE that targets one row by
// key: no row affected means it didn't exist (ErrNotFound).
func oneRow(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

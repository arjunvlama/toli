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

// Agent is a local agent identity, created by the human. Coding-agent
// sessions act as it by presenting its token.
type Agent struct {
	Name       string
	Card       a2a.AgentCard // what peers see about this agent
	DisabledAt *time.Time    // set while the human has paused the agent; nil = enabled
	Notify     *Notify       // where to push notifications without a harness session; nil = none
	CreatedAt  time.Time
}

// Notify is where an agent's notifications are pushed: a program run with
// the notification on stdin, or a webhook it's POSTed to (with Secret as a
// Bearer token). One of Command or Webhook is set.
type Notify struct {
	Command string `json:"command,omitempty"`
	Webhook string `json:"webhook,omitempty"`
	Secret  string `json:"secret,omitempty"`
}

// AddAgent stores a new agent. CreatedAt is set here; any value passed in is
// ignored. Returns ErrExists if the name is taken.
func (s *Store) AddAgent(ctx context.Context, a Agent, tokenHash string) error {
	card, err := json.Marshal(a.Card)
	if err != nil {
		return fmt.Errorf("encode card: %w", err)
	}

	_, err = s.db.ExecContext(ctx,
		"INSERT INTO agents (name, card, token_hash, created_at) VALUES (?, ?, ?, ?)",
		a.Name,
		string(card), // string, not []byte, so SQLite stores TEXT rather than BLOB
		tokenHash,
		time.Now().Unix(),
	)
	if isConstraint(err, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY) {
		return ErrExists
	}
	return err
}

// UpdateAgentCard replaces an agent's card. Returns ErrNotFound if there's no
// such agent.
func (s *Store) UpdateAgentCard(ctx context.Context, name string, card a2a.AgentCard) error {
	b, err := json.Marshal(card)
	if err != nil {
		return fmt.Errorf("encode card: %w", err)
	}
	return oneRow(s.db.ExecContext(ctx, "UPDATE agents SET card = ? WHERE name = ?", string(b), name))
}

func (s *Store) ListAgents(ctx context.Context) ([]Agent, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+agentColumns+" FROM agents ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var agents []Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		agents = append(agents, a)
	}
	return agents, rows.Err() // rows.Err reports an error that stopped the loop early
}

func (s *Store) GetAgent(ctx context.Context, name string) (Agent, error) {
	row := s.db.QueryRowContext(ctx, "SELECT "+agentColumns+" FROM agents WHERE name = ?", name)
	return oneAgent(row)
}

// AgentByTokenHash resolves a session's token to the agent it acts as.
func (s *Store) AgentByTokenHash(ctx context.Context, hash string) (Agent, error) {
	row := s.db.QueryRowContext(ctx, "SELECT "+agentColumns+" FROM agents WHERE token_hash = ?", hash)
	return oneAgent(row)
}

// SetAgentDisabled pauses (disabled) or resumes an agent. Disabling an
// already-disabled agent keeps its original time. Returns ErrNotFound if
// there's no such agent.
func (s *Store) SetAgentDisabled(ctx context.Context, name string, disabled bool) error {
	if !disabled {
		return oneRow(s.db.ExecContext(ctx, "UPDATE agents SET disabled_at = NULL WHERE name = ?", name))
	}
	return oneRow(s.db.ExecContext(ctx,
		"UPDATE agents SET disabled_at = COALESCE(disabled_at, ?) WHERE name = ?", time.Now().Unix(), name))
}

// SetAgentTokenHash replaces an agent's token (by its hash); the old token
// stops working. Returns ErrNotFound if there's no such agent.
func (s *Store) SetAgentTokenHash(ctx context.Context, name, tokenHash string) error {
	return oneRow(s.db.ExecContext(ctx, "UPDATE agents SET token_hash = ? WHERE name = ?", tokenHash, name))
}

// SetAgentNotify sets where an agent's notifications are pushed; nil clears
// it. Returns ErrNotFound if there's no such agent.
func (s *Store) SetAgentNotify(ctx context.Context, name string, n *Notify) error {
	var v any // NULL for nil
	if n != nil {
		b, err := json.Marshal(n)
		if err != nil {
			return fmt.Errorf("encode notify: %w", err)
		}
		v = string(b)
	}
	return oneRow(s.db.ExecContext(ctx, "UPDATE agents SET notify = ? WHERE name = ?", v, name))
}

// agentColumns is the column list scanAgent expects, in order.
const agentColumns = "name, card, disabled_at, notify, created_at"

func scanAgent(sc scanner) (Agent, error) {
	var (
		a        Agent
		card     string
		disabled sql.NullInt64
		notify   sql.NullString
		created  int64
	)
	if err := sc.Scan(&a.Name, &card, &disabled, &notify, &created); err != nil {
		return Agent{}, err
	}
	if err := json.Unmarshal([]byte(card), &a.Card); err != nil {
		return Agent{}, fmt.Errorf("decode card for agent '%s': %w", a.Name, err)
	}
	if notify.Valid {
		a.Notify = &Notify{}
		if err := json.Unmarshal([]byte(notify.String), a.Notify); err != nil {
			return Agent{}, fmt.Errorf("decode notify for agent '%s': %w", a.Name, err)
		}
	}
	if disabled.Valid {
		t := time.Unix(disabled.Int64, 0)
		a.DisabledAt = &t
	}
	a.CreatedAt = time.Unix(created, 0)
	return a, nil
}

// oneAgent scans a single-row lookup, mapping "no row" to ErrNotFound.
func oneAgent(row *sql.Row) (Agent, error) {
	a, err := scanAgent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Agent{}, ErrNotFound
	}
	return a, err
}

// RemoveAgent deletes a local agent. Its peers, sessions, and outgoing tasks
// are deleted with it (cascade). It returns the tasks that were still active,
// read in the same transaction, so the caller can cancel them on their peers.
// Returns ErrNotFound if there's no such agent.
func (s *Store) RemoveAgent(ctx context.Context, name string) ([]ActiveTask, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	active, err := activeTasks(ctx, tx, "owner = ?", name)
	if err != nil {
		return nil, fmt.Errorf("list tasks of agent %q: %w", name, err)
	}
	if err := oneRow(tx.ExecContext(ctx, "DELETE FROM agents WHERE name = ?", name)); err != nil {
		return nil, err
	}
	return active, tx.Commit()
}

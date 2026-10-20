package backend

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/arjunvlama/toli/internal/store"
	"github.com/arjunvlama/toli/internal/token"
)

// ErrInvalidName: agent and peer names go in URLs and in agents' tool calls,
// so they're kept to letters, digits, - and _.
var ErrInvalidName = errors.New("use letters, digits, - and _ (up to 64)")

// checkName returns ErrInvalidName (wrapped) unless name follows the rule.
func checkName(name string) error {
	ok := name != "" && len(name) <= 64
	for _, r := range name {
		ok = ok && ('a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' || r == '-' || r == '_')
	}
	if !ok {
		return fmt.Errorf("name %q: %w", name, ErrInvalidName)
	}
	return nil
}

// AddAgent creates a local agent and returns its token, shown once (only its
// hash is stored). Returns store.ErrExists if the name is taken, and
// ErrInvalidName for a name that doesn't follow the rule.
func (b *Backend) AddAgent(ctx context.Context, name, description string, skills []a2a.AgentSkill) (string, error) {
	if err := checkName(name); err != nil {
		return "", err
	}
	skills, err := normalizeSkills(skills)
	if err != nil {
		return "", err
	}
	tok, hash := token.New()
	err = b.store.AddAgent(ctx, store.Agent{
		Name: name,
		Card: a2a.AgentCard{Name: name, Description: description, Skills: skills},
	}, hash)
	if err != nil {
		return "", fmt.Errorf("add agent '%s': %w", name, err)
	}
	return tok, nil
}

// UpdateAgent replaces the parts of an agent's card its human writes: the
// description and skills. Returns store.ErrNotFound if there's no such agent.
func (b *Backend) UpdateAgent(ctx context.Context, name, description string, skills []a2a.AgentSkill) error {
	skills, err := normalizeSkills(skills)
	if err != nil {
		return err
	}
	a, err := b.store.GetAgent(ctx, name)
	if err != nil {
		return fmt.Errorf("load agent '%s': %w", name, err)
	}
	a.Card.Description = description
	a.Card.Skills = skills
	if err := b.store.UpdateAgentCard(ctx, name, a.Card); err != nil {
		return fmt.Errorf("update agent '%s': %w", name, err)
	}
	return nil
}

// ErrInvalidSkill is returned for a skill without a name or with a duplicate id.
var ErrInvalidSkill = errors.New("invalid skill")

// normalizeSkills checks skills and fills in what A2A requires: an id (from
// the name if unset, e.g. "Write SQL" -> "write-sql") and non-nil tags.
func normalizeSkills(skills []a2a.AgentSkill) ([]a2a.AgentSkill, error) {
	seen := map[string]bool{}
	out := make([]a2a.AgentSkill, 0, len(skills))
	for i, s := range skills {
		s.Name = strings.TrimSpace(s.Name)
		if s.Name == "" {
			return nil, fmt.Errorf("skill %d: name is required: %w", i+1, ErrInvalidSkill)
		}
		if s.ID == "" {
			s.ID = slug(s.Name)
		}
		if seen[s.ID] {
			return nil, fmt.Errorf("skill %q: duplicate id %q: %w", s.Name, s.ID, ErrInvalidSkill)
		}
		seen[s.ID] = true
		if s.Tags == nil {
			s.Tags = []string{}
		}
		out = append(out, s)
	}
	return out, nil
}

// slug lowercases s and joins its letters and digits with dashes.
func slug(s string) string {
	words := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	return strings.Join(words, "-")
}

func (b *Backend) ListAgents(ctx context.Context) ([]store.Agent, error) {
	return b.store.ListAgents(ctx)
}

func (b *Backend) GetAgent(ctx context.Context, name string) (store.Agent, error) {
	return b.store.GetAgent(ctx, name)
}

// ErrAgentDisabled is returned for a token whose agent the human has paused.
var ErrAgentDisabled = errors.New("agent is disabled")

// AgentByToken resolves an MCP session's token to the agent it acts as.
// Returns store.ErrNotFound for an unknown token, and ErrAgentDisabled (with
// the agent) if its agent is disabled.
func (b *Backend) AgentByToken(ctx context.Context, tok string) (store.Agent, error) {
	a, err := b.store.AgentByTokenHash(ctx, token.Hash(tok))
	if err != nil {
		return store.Agent{}, err
	}
	if a.DisabledAt != nil {
		return a, fmt.Errorf("agent '%s': %w", a.Name, ErrAgentDisabled)
	}
	return a, nil
}

// DisableAgent pauses an agent: its token is refused until EnableAgent. Its
// card, peers, sessions, and tasks are kept, and tasks already running keep
// being followed. Returns store.ErrNotFound if there's no such agent.
func (b *Backend) DisableAgent(ctx context.Context, name string) error {
	if err := b.store.SetAgentDisabled(ctx, name, true); err != nil {
		return fmt.Errorf("disable agent '%s': %w", name, err)
	}
	return nil
}

// EnableAgent resumes a disabled agent. Returns store.ErrNotFound if there's
// no such agent.
func (b *Backend) EnableAgent(ctx context.Context, name string) error {
	if err := b.store.SetAgentDisabled(ctx, name, false); err != nil {
		return fmt.Errorf("enable agent '%s': %w", name, err)
	}
	return nil
}

// RotateAgentToken issues an agent a new token, shown once (only its hash is
// stored); the old one stops working at once. Returns store.ErrNotFound if
// there's no such agent.
func (b *Backend) RotateAgentToken(ctx context.Context, name string) (string, error) {
	tok, hash := token.New()
	if err := b.store.SetAgentTokenHash(ctx, name, hash); err != nil {
		return "", fmt.Errorf("rotate token for agent '%s': %w", name, err)
	}
	return tok, nil
}

// ErrInvalidNotify is returned for a notify target toli won't use.
var ErrInvalidNotify = errors.New("invalid notify target")

// SetAgentNotify sets where an agent's notifications are pushed when it has
// no harness session: a program (its absolute path), run with each
// notification on stdin, or a webhook it's POSTed to. A webhook gets a new
// secret, returned (shown once), which toli sends as a Bearer token. Neither
// clears it. Returns store.ErrNotFound if there's no such agent.
func (b *Backend) SetAgentNotify(ctx context.Context, name, command, webhook string) (secret string, err error) {
	var n *store.Notify
	switch {
	case command != "" && webhook != "":
		return "", fmt.Errorf("%w: a command or a webhook, not both", ErrInvalidNotify)
	case command != "":
		if err := checkCommand(command); err != nil {
			return "", err
		}
		n = &store.Notify{Command: command}
	case webhook != "":
		if err := checkWebhook(webhook); err != nil {
			return "", err
		}
		secret, _ = token.New()
		n = &store.Notify{Webhook: webhook, Secret: secret}
	}
	if err := b.store.SetAgentNotify(ctx, name, n); err != nil {
		return "", fmt.Errorf("set notify for agent '%s': %w", name, err)
	}
	return secret, nil
}

// checkCommand wants an absolute path to a file: the daemon runs it
// directly, from wherever it was started.
func checkCommand(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%w: command %q: use an absolute path", ErrInvalidNotify, path)
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: command %q: no such file", ErrInvalidNotify, path)
	}
	if err != nil {
		return fmt.Errorf("%w: command %q: %w", ErrInvalidNotify, path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%w: command %q is a directory", ErrInvalidNotify, path)
	}
	return nil
}

// checkWebhook wants https, or http to this machine only: each call carries
// the notification and the secret.
func checkWebhook(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%w: webhook %q isn't a URL", ErrInvalidNotify, raw)
	}
	if u.Scheme == "https" || u.Scheme == "http" && isLocal(u.Hostname()) {
		return nil
	}
	return fmt.Errorf("%w: webhook %q: use https (http only to this machine)", ErrInvalidNotify, raw)
}

func isLocal(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || ip != nil && ip.IsLoopback()
}

// RemoveAgent deletes a local agent; the store deletes its peers, sessions,
// and outgoing tasks with it, and its peers are asked to cancel the tasks
// that were still active (in the background, best effort).
func (b *Backend) RemoveAgent(ctx context.Context, name string) error {
	err := b.peers.RemoveForAgent(ctx, name, func(ctx context.Context) ([]store.ActiveTask, error) {
		return b.store.RemoveAgent(ctx, name)
	})
	if err != nil {
		return fmt.Errorf("remove agent '%s': %w", name, err)
	}
	return nil
}

package wake

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/arjunvlama/toli/internal/store"
)

// errGone means the session can't be reached any more (it ended): no point
// retrying, and its registration should go.
var errGone = errors.New("session is gone")

// retryAfter is how long Notify waits before its one retry.
var retryAfter = 2 * time.Second

// Notify delivers text to agent's registered session, or with none, pushes
// it where the agent's human set (push). Best effort: with neither it does
// nothing (the work waits for the agent's next check); if the session is
// gone its registration is dropped and the text pushed; other failures are
// retried once.
func (s *Service) Notify(ctx context.Context, agent, text string) error {
	sess, err := s.store.HarnessSession(ctx, agent)
	if errors.Is(err, store.ErrNotFound) {
		return s.push(ctx, agent, text)
	}
	if err != nil {
		return fmt.Errorf("load session of %s: %w", agent, err)
	}

	err = s.deliver(ctx, sess, text)
	if err != nil && !errors.Is(err, errGone) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retryAfter):
		}
		err = s.deliver(ctx, sess, text)
	}
	if errors.Is(err, errGone) {
		slog.Info("wake: session gone; unregistering", "agent", agent, "session", sess.SessionID, "err", err)
		if err := s.Unregister(ctx, agent, sess.SessionID); err != nil {
			return err
		}
		return s.push(ctx, agent, text)
	}
	if err != nil {
		return fmt.Errorf("notify %s (%s session %s): %w", agent, sess.Harness, sess.SessionID, err)
	}
	slog.Info("notification sent", "agent", agent, "harness", sess.Harness)
	return nil
}

// deliver hands text to the session, the way its harness takes it.
func (s *Service) deliver(ctx context.Context, sess store.HarnessSession, text string) error {
	switch sess.Harness {
	case "claude-code":
		return deliverSocket(ctx, sess.Address["socket"], sess.Address["socket_token"], text)
	case "codex":
		err := deliverCommand(ctx, "codex", "queue", "--thread", sess.SessionID, "--message", text)
		// What codex queue (0.162) says for a thread it doesn't have. A
		// closed session's thread still takes notes (read when resumed).
		if err != nil && (strings.Contains(err.Error(), "no rollout found") || strings.Contains(err.Error(), "No active session found")) {
			return fmt.Errorf("%w: %w", errGone, err)
		}
		return err
	case "openclaw":
		// A user message to the session through the Gateway, as its web chat
		// sends (no channel needed; deliver: false keeps the reply off
		// channels). It starts a turn and returns at once.
		key := make([]byte, 16)
		rand.Read(key)
		params, err := json.Marshal(map[string]any{
			"sessionKey": sess.SessionID, "message": text, "deliver": false, "idempotencyKey": hex.EncodeToString(key),
		})
		if err != nil {
			return err
		}
		return deliverCommand(ctx, "openclaw", "gateway", "call", "chat.send", "--params", string(params))
	default:
		// Collected from GET /hook/notifications: by toli's plugin (OpenCode) or
		// extension (Pi), or by anyone's own integration.
		s.notes.put(sess.Agent, text)
		return nil
	}
}

// pushTimeout bounds one push, so a stuck program or webhook can't hold up
// the next.
var pushTimeout = 30 * time.Second

// push hands text to where the agent's human set its notifications to go
// when it has no session (store.Notify), if anywhere: a program, run with
// {"agent", "text"} on stdin, or a webhook it's POSTed to. Retried once.
func (s *Service) push(ctx context.Context, agent, text string) error {
	a, err := s.store.GetAgent(ctx, agent)
	if err != nil {
		return fmt.Errorf("load agent '%s': %w", agent, err)
	}
	n := a.Notify
	if n == nil {
		return nil
	}
	body, err := json.Marshal(map[string]string{"agent": agent, "text": text})
	if err != nil {
		return err
	}
	send, to := func() error { return pushCommand(ctx, n.Command, body) }, "command"
	if n.Webhook != "" {
		send, to = func() error { return pushWebhook(ctx, n.Webhook, n.Secret, body) }, "webhook"
	}
	err = send()
	if err != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retryAfter):
		}
		err = send()
	}
	if err != nil {
		return fmt.Errorf("push to %s of agent '%s': %w", to, agent, err)
	}
	slog.Info("notification pushed", "agent", agent, "to", to)
	return nil
}

// pushCommand runs the program (no shell) with body on stdin; exit 0 means
// delivered.
func pushCommand(ctx context.Context, path string, body []byte) error {
	ctx, cancel := context.WithTimeout(ctx, pushTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path)
	cmd.Stdin = bytes.NewReader(body)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w: %s", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// webhookClient doesn't follow redirects: a redirect could send the
// notification and its secret somewhere the human didn't choose.
var webhookClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// pushWebhook POSTs body to the webhook with its secret as a Bearer token; a
// 2xx answer means delivered. Errors name the host only: the URL can be a
// credential itself (e.g. Slack's).
func pushWebhook(ctx context.Context, webhook, secret string, body []byte) error {
	ctx, cancel := context.WithTimeout(ctx, pushTimeout)
	defer cancel()
	u, err := url.Parse(webhook)
	if err != nil {
		return errors.New("bad webhook URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhook, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("webhook at %s: bad request", u.Host)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := webhookClient.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err // without the URL
		}
		return fmt.Errorf("webhook at %s: %w", u.Host, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("webhook at %s answered %s", u.Host, resp.Status)
	}
	return nil
}

// deliverCommand runs a command that hands the note to the session (e.g.
// codex queue); exit 0 means delivered.
func deliverCommand(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// deliverSocket writes a message to a Claude Code session's inbox socket:
// newline-delimited JSON, an auth line with the session's token, then the
// message.
func deliverSocket(ctx context.Context, path, token, text string) error {
	if path == "" {
		return fmt.Errorf("%w: no socket registered", errGone)
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	// Windows refuses a dead socket with WSAECONNREFUSED (10061), not ECONNREFUSED.
	if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.Errno(10061)) {
		return fmt.Errorf("%w: %w", errGone, err)
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}

	enc := json.NewEncoder(conn) // one JSON value per line
	if token != "" {
		if err := enc.Encode(map[string]string{"type": "auth", "token": token}); err != nil {
			return err
		}
	}
	return enc.Encode(map[string]any{"type": "user", "message": map[string]string{"role": "user", "content": text}})
}

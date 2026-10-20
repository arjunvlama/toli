package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"syscall"
	"time"
)

// Client is what the CLI uses to talk to a running daemon.
type Client struct {
	base       string // e.g. "http://127.0.0.1:7421"
	adminToken string
	http       *http.Client
}

func NewClient(base, adminToken string) *Client {
	return &Client{
		base:       base,
		adminToken: adminToken,
		http:       &http.Client{Timeout: 10 * time.Second}, // a hung daemon shouldn't hang the CLI
	}
}

func (c *Client) AddAgent(ctx context.Context, req AddAgentRequest) (AddAgentResponse, error) {
	var resp AddAgentResponse
	if err := c.do(ctx, "POST", "/v1/agents", req, &resp); err != nil {
		return AddAgentResponse{}, err
	}
	return resp, nil
}

func (c *Client) ListAgents(ctx context.Context) ([]AgentInfo, error) {
	var resp ListAgentsResponse
	if err := c.do(ctx, "GET", "/v1/agents", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Agents, nil
}

func (c *Client) UpdateAgent(ctx context.Context, name string, req UpdateAgentRequest) error {
	return c.do(ctx, "PUT", "/v1/agents/"+url.PathEscape(name), req, nil)
}

func (c *Client) RemoveAgent(ctx context.Context, name string) error {
	return c.do(ctx, "DELETE", "/v1/agents/"+url.PathEscape(name), nil, nil)
}

func (c *Client) DisableAgent(ctx context.Context, name string) error {
	return c.do(ctx, "POST", "/v1/agents/"+url.PathEscape(name)+"/disable", nil, nil)
}

func (c *Client) EnableAgent(ctx context.Context, name string) error {
	return c.do(ctx, "POST", "/v1/agents/"+url.PathEscape(name)+"/enable", nil, nil)
}

func (c *Client) SetAgentNotify(ctx context.Context, name string, req SetNotifyRequest) (SetNotifyResponse, error) {
	var resp SetNotifyResponse
	if err := c.do(ctx, "PUT", "/v1/agents/"+url.PathEscape(name)+"/notify", req, &resp); err != nil {
		return SetNotifyResponse{}, err
	}
	return resp, nil
}

func (c *Client) RotateAgentToken(ctx context.Context, name string) (RotateTokenResponse, error) {
	var resp RotateTokenResponse
	if err := c.do(ctx, "POST", "/v1/agents/"+url.PathEscape(name)+"/token", nil, &resp); err != nil {
		return RotateTokenResponse{}, err
	}
	return resp, nil
}

func (c *Client) AddPeer(ctx context.Context, req AddPeerRequest) (AddPeerResponse, error) {
	var resp AddPeerResponse
	if err := c.do(ctx, "POST", "/v1/peers", req, &resp); err != nil {
		return resp, err
	}
	return resp, nil
}

func (c *Client) CreateInvite(ctx context.Context, req CreateInviteRequest) (CreateInviteResponse, error) {
	var resp CreateInviteResponse
	err := c.do(ctx, "POST", "/v1/invites", req, &resp)
	return resp, err
}

// AcceptInvite waits longer than other calls: the daemon calls the inviting
// daemon, which fetches our card.
func (c *Client) AcceptInvite(ctx context.Context, req AcceptInviteRequest) (AcceptInviteResponse, error) {
	slow, hc := *c, *c.http
	hc.Timeout = time.Minute
	slow.http = &hc
	var resp AcceptInviteResponse
	err := slow.do(ctx, "POST", "/v1/invites/accept", req, &resp)
	return resp, err
}

func (c *Client) ListPeers(ctx context.Context, agent string) ([]PeerInfo, error) {
	var resp ListPeersResponse
	if err := c.do(ctx, "GET", "/v1/peers?agent="+url.QueryEscape(agent), nil, &resp); err != nil {
		return nil, err
	}
	return resp.Peers, nil
}

func (c *Client) RemovePeer(ctx context.Context, id int64) error {
	return c.do(ctx, "DELETE", fmt.Sprintf("/v1/peers/%d", id), nil, nil)
}

func (c *Client) SetPeerToken(ctx context.Context, id int64, token string) error {
	return c.do(ctx, "PUT", fmt.Sprintf("/v1/peers/%d/token", id), SetPeerTokenRequest{Token: token}, nil)
}

func (c *Client) RotatePeerToken(ctx context.Context, id int64) (RotateTokenResponse, error) {
	var resp RotateTokenResponse
	if err := c.do(ctx, "POST", fmt.Sprintf("/v1/peers/%d/token", id), nil, &resp); err != nil {
		return RotateTokenResponse{}, err
	}
	return resp, nil
}

func (c *Client) ListTasks(ctx context.Context, agent string) ([]TaskInfo, error) {
	var resp ListTasksResponse
	if err := c.do(ctx, "GET", "/v1/tasks?agent="+url.QueryEscape(agent), nil, &resp); err != nil {
		return nil, err
	}
	return resp.Tasks, nil
}

func (c *Client) GetTask(ctx context.Context, agent, id string) (TaskDetail, error) {
	var resp TaskDetail
	err := c.do(ctx, "GET", "/v1/tasks/"+url.PathEscape(id)+"?agent="+url.QueryEscape(agent), nil, &resp)
	return resp, err
}

func (c *Client) ListAssignedTasks(ctx context.Context, agent string) ([]TaskInfo, error) {
	var resp ListTasksResponse
	if err := c.do(ctx, "GET", "/v1/assigned-tasks?agent="+url.QueryEscape(agent), nil, &resp); err != nil {
		return nil, err
	}
	return resp.Tasks, nil
}

func (c *Client) GetAssignedTask(ctx context.Context, agent, id string) (TaskDetail, error) {
	var resp TaskDetail
	err := c.do(ctx, "GET", "/v1/assigned-tasks/"+url.PathEscape(id)+"?agent="+url.QueryEscape(agent), nil, &resp)
	return resp, err
}

func (c *Client) CancelTask(ctx context.Context, agent, id string) (TaskInfo, error) {
	var resp TaskInfo
	err := c.do(ctx, "POST", "/v1/tasks/"+url.PathEscape(id)+"/cancel?agent="+url.QueryEscape(agent), nil, &resp)
	return resp, err
}

// do sends body as JSON (if non-nil), decodes the response into out (if
// non-nil), and turns non-2xx responses into errors using the {"error": ...}
// body.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reqBody)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.adminToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("can't reach toli daemon at %s (is `toli daemon` running?)", c.base)
	}
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.NewDecoder(resp.Body).Decode(&e) != nil || e.Error == "" {
			// Not one of our error bodies (or something else owns the port).
			return fmt.Errorf("daemon: %s", resp.Status)
		}
		return errors.New(e.Error)
	}

	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

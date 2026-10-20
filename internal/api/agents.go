package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/arjunvlama/toli/internal/a2aserver"
	"github.com/arjunvlama/toli/internal/backend"
	"github.com/arjunvlama/toli/internal/store"
)

// Skill is an A2A AgentSkill, minus the fields toli doesn't use yet
// (per-skill input/output modes and security requirements). ID is optional
// on input: it's derived from the name.
type Skill struct {
	ID          string   `json:"id,omitempty" yaml:"id,omitempty"`
	Name        string   `json:"name" yaml:"name"`
	Description string   `json:"description,omitempty" yaml:"description,omitempty"`
	Tags        []string `json:"tags,omitempty" yaml:"tags,omitempty"`
	Examples    []string `json:"examples,omitempty" yaml:"examples,omitempty"`
}

type AddAgentRequest struct {
	Name        string  `json:"name"`
	Description string  `json:"description,omitempty"`
	Skills      []Skill `json:"skills,omitempty"`
}

// UpdateAgentRequest replaces the agent's description and skills.
type UpdateAgentRequest struct {
	Description string  `json:"description"`
	Skills      []Skill `json:"skills"`
}

type AddAgentResponse struct {
	Name  string `json:"name"`
	Token string `json:"token"` // shown once; only its hash is stored
}

type AgentInfo struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Skills      []Skill     `json:"skills,omitempty"`
	Disabled    bool        `json:"disabled,omitempty"`
	ShareURL    string      `json:"share_url,omitempty"` // the agent's Agent Card URL, which a peer's human enters to add it; "" if the daemon is client-only
	Harness     string      `json:"harness,omitempty"`   // the coding agent its registered session runs in (e.g. claude-code); "" if none
	Notify      *NotifyInfo `json:"notify,omitempty"`    // where notifications are pushed without a harness session; nil if nowhere
}

// NotifyInfo is where an agent's notifications are pushed: never the
// webhook's secret, and only the webhook's scheme and host (a webhook URL
// can be a credential itself, e.g. Slack's).
type NotifyInfo struct {
	Command string `json:"command,omitempty"`
	Webhook string `json:"webhook,omitempty"`
}

// SetNotifyRequest sets a command (an absolute path) or a webhook URL;
// neither clears it.
type SetNotifyRequest struct {
	Command string `json:"command,omitempty"`
	Webhook string `json:"webhook,omitempty"`
}

// SetNotifyResponse carries a new webhook's secret, shown once.
type SetNotifyResponse struct {
	Secret string `json:"secret,omitempty"`
}

// RotateTokenResponse carries an agent's new token, shown once.
type RotateTokenResponse struct {
	Name  string `json:"name"`
	Token string `json:"token"`
}

type ListAgentsResponse struct {
	Agents []AgentInfo `json:"agents"`
}

func toAgentInfo(a store.Agent) AgentInfo {
	info := AgentInfo{Name: a.Name, Description: a.Card.Description, Skills: toSkills(a.Card.Skills), Disabled: a.DisabledAt != nil}
	if a.Notify != nil {
		info.Notify = &NotifyInfo{Command: a.Notify.Command, Webhook: HostOnly(a.Notify.Webhook)}
	}
	return info
}

// HostOnly is a URL's scheme and host, then "/…" if there was more: how
// webhook URLs are shown.
func HostOnly(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	short := u.Scheme + "://" + u.Host
	if strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" {
		short += "/…"
	}
	return short
}

func toSkills(skills []a2a.AgentSkill) []Skill {
	var out []Skill
	for _, s := range skills {
		out = append(out, Skill{ID: s.ID, Name: s.Name, Description: s.Description, Tags: s.Tags, Examples: s.Examples})
	}
	return out
}

func toA2ASkills(skills []Skill) []a2a.AgentSkill {
	out := make([]a2a.AgentSkill, 0, len(skills))
	for _, s := range skills {
		out = append(out, a2a.AgentSkill{
			ID: s.ID, Name: s.Name, Description: s.Description, Tags: s.Tags, Examples: s.Examples,
		})
	}
	return out
}

// POST /v1/agents
func (s *Server) addAgent(w http.ResponseWriter, r *http.Request) {

	var req AddAgentRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	tok, err := s.backend.AddAgent(r.Context(), req.Name, req.Description, toA2ASkills(req.Skills))
	if errors.Is(err, backend.ErrInvalidSkill) || errors.Is(err, backend.ErrInvalidName) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if errors.Is(err, store.ErrExists) {
		writeError(w, http.StatusConflict, "agent '"+req.Name+"' already exists")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "add agent: "+err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, AddAgentResponse{Name: req.Name, Token: tok})
}

// PUT /v1/agents/{name}
func (s *Server) updateAgent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	var req UpdateAgentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	err := s.backend.UpdateAgent(r.Context(), name, req.Description, toA2ASkills(req.Skills))
	if errors.Is(err, backend.ErrInvalidSkill) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "agent '"+name+"' not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "update agent: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DELETE /v1/agents/{name}
func (s *Server) removeAgent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	err := s.backend.RemoveAgent(r.Context(), name)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "agent '"+name+"' not found")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /v1/agents/{name}/disable
func (s *Server) disableAgent(w http.ResponseWriter, r *http.Request) {
	s.setAgentDisabled(w, r, s.backend.DisableAgent)
}

// POST /v1/agents/{name}/enable
func (s *Server) enableAgent(w http.ResponseWriter, r *http.Request) {
	s.setAgentDisabled(w, r, s.backend.EnableAgent)
}

func (s *Server) setAgentDisabled(w http.ResponseWriter, r *http.Request, set func(context.Context, string) error) {
	name := r.PathValue("name")
	err := set(r.Context(), name)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "agent '"+name+"' not found")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PUT /v1/agents/{name}/notify
func (s *Server) setAgentNotify(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req SetNotifyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	secret, err := s.backend.SetAgentNotify(r.Context(), name, req.Command, req.Webhook)
	switch {
	case errors.Is(err, backend.ErrInvalidNotify):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "agent '"+name+"' not found")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, SetNotifyResponse{Secret: secret})
}

// POST /v1/agents/{name}/token
func (s *Server) rotateAgentToken(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	tok, err := s.backend.RotateAgentToken(r.Context(), name)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "agent '"+name+"' not found")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, RotateTokenResponse{Name: name, Token: tok})
}

// GET /v1/agents
func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {

	agents, err := s.backend.ListAgents(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list agents: "+err.Error())
		return
	}

	resp := ListAgentsResponse{Agents: make([]AgentInfo, 0, len(agents))} // non-nil so no agents is [] not null
	for _, agent := range agents {
		info := toAgentInfo(agent)
		if s.publicURL != "" {
			info.ShareURL = a2aserver.CardURL(s.publicURL, agent.Name)
		}
		switch hs, err := s.backend.HarnessSession(r.Context(), agent.Name); {
		case err == nil:
			info.Harness = hs.Harness
		case !errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusInternalServerError, "load harness session: "+err.Error())
			return
		}
		resp.Agents = append(resp.Agents, info)
	}

	writeJSON(w, http.StatusOK, resp)
}

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/arjunvlama/toli/internal/a2aserver"
	"github.com/arjunvlama/toli/internal/backend"
	"github.com/arjunvlama/toli/internal/store"
)

type CreateInviteRequest struct {
	Agent string `json:"agent"` // local agent to pair with
	Name  string `json:"name"`  // what it will call whoever accepts
}

type CreateInviteResponse struct {
	Invite    string    `json:"invite"` // shown once
	ExpiresAt time.Time `json:"expires_at"`
}

type AcceptInviteRequest struct {
	Agent  string `json:"agent"` // local agent that pairs with the inviting one
	Invite string `json:"invite"`
	Name   string `json:"name,omitempty"` // what to call the inviting agent; default: its own name
}

type AcceptInviteResponse struct {
	Peer PeerInfo `json:"peer"`
}

// POST /v1/invites
func (s *Server) createInvite(w http.ResponseWriter, r *http.Request) {
	var req CreateInviteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Agent == "" || req.Name == "" {
		writeError(w, http.StatusBadRequest, "agent and name are required")
		return
	}
	if s.publicURL == "" {
		writeError(w, http.StatusBadRequest, "this daemon has no A2A address (`--a2a-listen`), so no one could reach the agent")
		return
	}

	inv, expires, err := s.backend.CreateInvite(r.Context(), req.Agent, req.Name, a2aserver.CardURL(s.publicURL, req.Agent))
	switch {
	case errors.Is(err, backend.ErrInvalidName):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, store.ErrExists):
		writeError(w, http.StatusConflict, "agent '"+req.Agent+"' already has a peer '"+req.Name+"'")
		return
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "no agent '"+req.Agent+"'")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, CreateInviteResponse{Invite: inv, ExpiresAt: expires})
}

// POST /v1/invites/accept
func (s *Server) acceptInvite(w http.ResponseWriter, r *http.Request) {
	var req AcceptInviteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Agent == "" || req.Invite == "" {
		writeError(w, http.StatusBadRequest, "agent and invite are required")
		return
	}
	if s.publicURL == "" {
		writeError(w, http.StatusBadRequest, "this daemon has no A2A address (`--a2a-listen`), so the other side couldn't reach your agent")
		return
	}
	if _, err := s.backend.GetAgent(r.Context(), req.Agent); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no agent '"+req.Agent+"'")
		return
	}

	peer, err := s.backend.AcceptInvite(r.Context(), req.Invite, req.Agent, req.Name, a2aserver.CardURL(s.publicURL, req.Agent))
	switch {
	case errors.Is(err, backend.ErrInvalidInvite), errors.Is(err, backend.ErrInvalidName):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, store.ErrExists):
		writeError(w, http.StatusConflict, "agent '"+req.Agent+"' already has a peer by that name; choose another name")
		return
	case err != nil:
		writeError(w, http.StatusBadGateway, err.Error()) // the inviting daemon refused, or couldn't be reached
		return
	}
	status, _ := s.backend.PeerStatus(peer.ID)
	writeJSON(w, http.StatusCreated, AcceptInviteResponse{Peer: toPeerInfo(peer, status)})
}

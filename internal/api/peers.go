package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/arjunvlama/toli/internal/backend"
	"github.com/arjunvlama/toli/internal/peerclient"
	"github.com/arjunvlama/toli/internal/store"
)

type AddPeerRequest struct {
	Agent string `json:"agent"`           // local agent whose allowlist this goes on
	Name  string `json:"name"`            // local alias for the peer
	URL   string `json:"url"`             // where its Agent Card is served
	Token string `json:"token,omitempty"` // the token the peer gave us; may be set later (PUT /v1/peers/{id}/token)
}

// SetPeerTokenRequest carries the token a peer gave us.
type SetPeerTokenRequest struct {
	Token string `json:"token"`
}

type AddPeerResponse struct {
	Peer  PeerInfo `json:"peer"`
	Token string   `json:"token"` // the token we issued to the peer; shown once
}

// PeerInfo is a peer as the TUI sees it. Never includes the token we send
// to the peer.
type PeerInfo struct {
	ID          int64      `json:"id"`
	Agent       string     `json:"agent"`
	Name        string     `json:"name"`
	URL         string     `json:"url"`
	CardName    string     `json:"card_name,omitempty"`   // the peer agent's own name, from its card
	Description string     `json:"description,omitempty"` // from its card
	Skills      []Skill    `json:"skills,omitempty"`      // from its extended card, once connected
	LastSeen    *time.Time `json:"last_seen,omitempty"`   // last successful card check; nil if not checked yet
	LastError   string     `json:"last_error,omitempty"`
	Connected   *bool      `json:"connected"` // the last check succeeded: the peer is reachable and accepts our token; null if not checked yet
	HasToken    bool       `json:"has_token"` // we have the token the peer gave us, so we can call it
}

type ListPeersResponse struct {
	Peers []PeerInfo `json:"peers"`
}

// POST /v1/peers
func (s *Server) addPeer(w http.ResponseWriter, r *http.Request) {
	var req AddPeerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Agent == "" || req.Name == "" || req.URL == "" {
		writeError(w, http.StatusBadRequest, "agent, name, and url are required")
		return
	}

	peer, tok, err := s.backend.AddPeer(r.Context(), req.Agent, req.Name, req.URL, req.Token)
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
		writeError(w, http.StatusBadGateway, err.Error()) // most likely the peer's card couldn't be fetched
		return
	}

	status, _ := s.backend.PeerStatus(peer.ID)
	writeJSON(w, http.StatusCreated, AddPeerResponse{Peer: toPeerInfo(peer, status), Token: tok})
}

// GET /v1/peers?agent=<name>
func (s *Server) listPeers(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		writeError(w, http.StatusBadRequest, "agent is required")
		return
	}

	peers, err := s.backend.ListPeers(r.Context(), agent)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list peers: "+err.Error())
		return
	}

	resp := ListPeersResponse{Peers: make([]PeerInfo, 0, len(peers))}
	for _, p := range peers {
		status, _ := s.backend.PeerStatus(p.ID) // zero if not checked yet
		resp.Peers = append(resp.Peers, toPeerInfo(p, status))
	}
	writeJSON(w, http.StatusOK, resp)
}

// DELETE /v1/peers/{id}
func (s *Server) removePeer(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid peer id: "+r.PathValue("id"))
		return
	}

	err = s.backend.RemovePeer(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, fmt.Sprintf("no peer with id %d", id))
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// toPeerInfo picks what the TUI may see. Leaves OutboundToken out on purpose.
func toPeerInfo(p store.Peer, st peerclient.Status) PeerInfo {
	info := PeerInfo{ID: p.ID, Agent: p.Agent, Name: p.Name, URL: p.URL, HasToken: p.OutboundToken != ""}
	if p.Card != nil {
		info.CardName = p.Card.Name
		info.Description = p.Card.Description
		info.Skills = toSkills(p.Card.Skills)
	}
	if !st.LastSeen.IsZero() {
		info.LastSeen = &st.LastSeen
	}
	if st.LastError != nil {
		info.LastError = st.LastError.Error()
	}
	if !st.LastSeen.IsZero() || st.LastError != nil {
		connected := st.LastError == nil
		info.Connected = &connected
	}
	return info
}

// PUT /v1/peers/{id}/token
// Stores the token the peer gave us. The token we issued it is unchanged.
func (s *Server) setPeerToken(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid peer id: "+r.PathValue("id"))
		return
	}
	var req SetPeerTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Token == "" {
		writeError(w, http.StatusBadRequest, "token is required")
		return
	}

	err = s.backend.SetPeerToken(r.Context(), id, req.Token)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, fmt.Sprintf("no peer with id %d", id))
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "set peer token: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /v1/peers/{id}/token
func (s *Server) rotatePeerToken(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid peer id: "+r.PathValue("id"))
		return
	}
	name, tok, err := s.backend.RotatePeerToken(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, fmt.Sprintf("no peer with id %d", id))
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, RotateTokenResponse{Name: name, Token: tok})
}

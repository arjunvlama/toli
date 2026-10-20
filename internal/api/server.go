// Package api is the human-facing control plane: JSON over HTTP under /v1/,
// authorized by the admin token. Agent tokens are never accepted here, so an
// agent can't edit its own identity or allowlist.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/arjunvlama/toli/internal/backend"
	"github.com/arjunvlama/toli/internal/token"
)

type Server struct {
	backend   *backend.Backend
	adminHash string
	publicURL string // the A2A server's public URL; "" if this daemon is client-only
}

// New returns the /v1/ handler, wrapped in the admin-token check. publicURL
// is where peers reach this daemon's A2A server ("" if it has none).
func New(b *backend.Backend, adminHash, publicURL string) http.Handler {
	s := &Server{backend: b, adminHash: adminHash, publicURL: publicURL}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/agents", s.addAgent)
	mux.HandleFunc("GET /v1/agents", s.listAgents)
	mux.HandleFunc("PUT /v1/agents/{name}", s.updateAgent)
	mux.HandleFunc("DELETE /v1/agents/{name}", s.removeAgent)
	mux.HandleFunc("POST /v1/agents/{name}/disable", s.disableAgent)
	mux.HandleFunc("POST /v1/agents/{name}/enable", s.enableAgent)
	mux.HandleFunc("POST /v1/agents/{name}/token", s.rotateAgentToken)
	mux.HandleFunc("PUT /v1/agents/{name}/notify", s.setAgentNotify)
	mux.HandleFunc("POST /v1/peers", s.addPeer)
	mux.HandleFunc("GET /v1/peers", s.listPeers)
	mux.HandleFunc("DELETE /v1/peers/{id}", s.removePeer)
	mux.HandleFunc("PUT /v1/peers/{id}/token", s.setPeerToken)     // the token the peer gave us
	mux.HandleFunc("POST /v1/peers/{id}/token", s.rotatePeerToken) // a new token we issue the peer
	mux.HandleFunc("POST /v1/invites", s.createInvite)
	mux.HandleFunc("POST /v1/invites/accept", s.acceptInvite)
	mux.HandleFunc("GET /v1/tasks", s.listTasks)
	mux.HandleFunc("GET /v1/tasks/{id}", s.getTask)
	mux.HandleFunc("POST /v1/tasks/{id}/cancel", s.cancelTask)
	mux.HandleFunc("GET /v1/assigned-tasks", s.listAssignedTasks)
	mux.HandleFunc("GET /v1/assigned-tasks/{id}", s.getAssignedTask)
	return s.requireAdmin(mux)
}

// requireAdmin rejects any request whose bearer token isn't the admin token.
func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(token.Hash(tok)), []byte(s.adminHash)) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid admin token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// writeJSON sends v as the response body with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError) // status not yet sent: can still send 500
		slog.Error("encode json response", "err", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(b)
}

// writeError sends {"error": msg} with the given status.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

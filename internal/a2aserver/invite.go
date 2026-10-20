package a2aserver

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/arjunvlama/toli/internal/backend"
	"github.com/arjunvlama/toli/internal/backend/peers"
	"github.com/arjunvlama/toli/internal/store"
)

// serveInvite is the call back from a daemon accepting one of our agents'
// invites (backend.AcceptInvite), authenticated by the invite's token.
func (s *Server) serveInvite(w http.ResponseWriter, r *http.Request) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="toli"`)
		inviteError(w, http.StatusUnauthorized, "missing bearer token")
		return
	}
	var req backend.InviteRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil || req.Card == "" || req.Token == "" {
		inviteError(w, http.StatusBadRequest, "want a JSON body with card and token")
		return
	}

	peer, err := s.backend.RedeemInvite(r.Context(), r.PathValue("agent"), tok, req.Card, req.Token)
	switch {
	case err == nil:
	case errors.Is(err, store.ErrNotFound):
		inviteError(w, http.StatusNotFound, "no such invite, or it expired or was used")
		return
	case errors.Is(err, store.ErrExists):
		inviteError(w, http.StatusConflict, "the inviting agent already has a peer by the invite's name")
		return
	case errors.Is(err, peers.ErrUnreachable):
		inviteError(w, http.StatusUnprocessableEntity, "couldn't fetch your agent's card: "+err.Error())
		return
	default:
		slog.Error("a2a: redeem invite", "agent", r.PathValue("agent"), "err", err)
		inviteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	slog.Info("a2a: invite accepted", "agent", peer.Agent, "peer", peer.Name)
	w.WriteHeader(http.StatusNoContent)
}

func inviteError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

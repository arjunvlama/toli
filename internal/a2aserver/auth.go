package a2aserver

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/arjunvlama/toli/internal/store"
)

type callerKey struct{}

// callerFrom returns the peer an authenticated request came from.
func callerFrom(ctx context.Context) (store.Peer, bool) {
	p, ok := ctx.Value(callerKey{}).(store.Peer)
	return p, ok
}

// authenticate lets a request through to next only if it carries a token we
// issued to one of the URL's agent's peers ("Authorization: Bearer <token>"),
// and that agent isn't disabled. The calling peer goes in the context.
//
//   - no token or an unknown one: 401
//   - a token issued for another agent's peer, or a disabled agent: 403
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agent := r.PathValue("agent")

		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || tok == "" {
			unauthorized(w, "missing bearer token")
			return
		}
		peer, err := s.backend.PeerByToken(r.Context(), tok)
		if errors.Is(err, store.ErrNotFound) {
			unauthorized(w, "unknown token")
			return
		}
		if err != nil {
			http.Error(w, "look up token: "+err.Error(), http.StatusInternalServerError)
			return
		}

		if peer.Agent != agent {
			http.Error(w, "this token isn't for agent "+agent, http.StatusForbidden)
			return
		}
		a, err := s.backend.GetAgent(r.Context(), agent)
		if err != nil {
			http.Error(w, "load agent: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if a.DisabledAt != nil {
			http.Error(w, "agent '"+agent+"' isn't accepting work right now", http.StatusForbidden)
			return
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, peer)))
	})
}

// storeCaller identifies the peer behind a request for the incoming task
// store. a2asrv runs executions with a context derived from the request's
// (context.WithoutCancel), so this works there too.
func storeCaller(ctx context.Context) (store.Caller, error) {
	p, ok := callerFrom(ctx)
	if !ok {
		return store.Caller{}, errors.New("no authenticated peer")
	}
	return store.Caller{Agent: p.Agent, PeerID: p.ID}, nil
}

// identify gives a2asrv the caller's identity: the peer's id, matching the
// incoming task store's StoredTask.User.
type identify struct{}

func (identify) Before(ctx context.Context, callCtx *a2asrv.CallContext, req *a2asrv.Request) (context.Context, any, error) {
	if p, ok := callerFrom(ctx); ok {
		callCtx.User = a2asrv.NewAuthenticatedUser(strconv.FormatInt(p.ID, 10), nil)
	}
	return ctx, nil, nil
}

func (identify) After(ctx context.Context, callCtx *a2asrv.CallContext, resp *a2asrv.Response) error {
	return nil
}

func unauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="toli"`)
	http.Error(w, msg, http.StatusUnauthorized)
}

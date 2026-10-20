// Package a2aserver is the daemon's A2A server: what peers call to send our
// agents work. Each local agent has its A2A endpoint at /a2a/<agent>/ and its
// Agent Card at /a2a/<agent>/.well-known/agent-card.json.
package a2aserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2acompat/a2av0"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/arjunvlama/toli/internal/backend"
	"github.com/arjunvlama/toli/internal/store"
)

// cardVersion is the version our Agent Cards advertise (A2A requires one).
const cardVersion = "0.0.1"

// bearerScheme names the one security scheme our cards declare: the token
// we issued the peer, sent as "Authorization: Bearer <token>".
const bearerScheme a2a.SecuritySchemeName = "bearer"

// Server serves the A2A side of every local agent.
type Server struct {
	backend   *backend.Backend
	publicURL string // what peers use to reach this listener, e.g. https://arjun.tailnet.ts.net
}

// New returns the handler for the A2A listener. publicURL is the address
// peers use to reach it; it goes into each agent's card.
func New(b *backend.Backend, publicURL string) http.Handler {
	s := &Server{backend: b, publicURL: strings.TrimSuffix(publicURL, "/")}
	tasks := b.IncomingTasks(storeCaller)
	sdk := a2asrv.NewHandler(executor{},
		a2asrv.WithTaskStore(tasks),
		a2asrv.WithCallInterceptors(identify{}),
	)
	h := &handler{RequestHandler: sdk, tasks: tasks, extendedCard: s.extendedCard, notify: b.NotifyAgent}

	mux := http.NewServeMux()
	mux.HandleFunc("/a2a/{agent}"+a2asrv.WellKnownAgentCardPath, s.serveCard)
	mux.Handle("POST /a2a/{agent}/{$}", s.authenticate(a2asrv.NewJSONRPCHandler(h)))
	mux.Handle("POST /a2a/{agent}/v0.3/{$}", s.authenticate(a2av0.NewJSONRPCHandler(h)))
	mux.HandleFunc("POST /a2a/{agent}/invite", s.serveInvite)
	return mux
}

// AgentURL is an agent's A2A endpoint.
func AgentURL(publicURL, agent string) string {
	return strings.TrimSuffix(publicURL, "/") + "/a2a/" + url.PathEscape(agent) + "/"
}

// agentURLV03 is an agent's endpoint for peers on A2A 0.3.
func agentURLV03(publicURL, agent string) string {
	return AgentURL(publicURL, agent) + "v0.3/"
}

// CardURL is where an agent's card is served: the URL its human shares, and
// that a peer's human enters to add it. A2A clients fetch a card URL with a
// path as is, so it's the full URL, not the agent's base.
func CardURL(publicURL, agent string) string {
	return strings.TrimSuffix(publicURL, "/") + "/a2a/" + url.PathEscape(agent) + a2asrv.WellKnownAgentCardPath
}

// serveCard serves an agent's public Agent Card, built from what's stored
// about it plus how to reach it. Unknown agents are 404.
func (s *Server) serveCard(w http.ResponseWriter, r *http.Request) {
	agent, err := s.backend.GetAgent(r.Context(), r.PathValue("agent"))
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "load agent: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Written as both a 1.0 card and a 0.3 one, for peers on either.
	a2asrv.NewAgentCardHandler(a2av0.NewStaticAgentCardProducer(s.card(agent, false))).ServeHTTP(w, r)
}

// extendedCard is the calling peer's view of the agent it calls: the full
// card, skills included (A2A GetExtendedAgentCard, authenticated).
func (s *Server) extendedCard(ctx context.Context) (*a2a.AgentCard, error) {
	p, ok := callerFrom(ctx)
	if !ok {
		return nil, a2a.ErrUnauthenticated
	}
	agent, err := s.backend.GetAgent(ctx, p.Agent)
	if err != nil {
		return nil, fmt.Errorf("load agent '%s': %w", p.Agent, err)
	}
	return s.card(agent, true), nil
}

// card is agent's Agent Card. The public one, which anyone who can reach us
// can read, has no skills; the extended one, for authenticated peers, does.
func (s *Server) card(agent store.Agent, extended bool) *a2a.AgentCard {
	skills := []a2a.AgentSkill{} // required by A2A, even if empty
	if extended && agent.Card.Skills != nil {
		skills = agent.Card.Skills
	}
	return &a2a.AgentCard{
		Name:        agent.Card.Name,
		Description: agent.Card.Description,
		Version:     cardVersion,
		Skills:      skills,
		SupportedInterfaces: []*a2a.AgentInterface{
			a2a.NewAgentInterface(AgentURL(s.publicURL, agent.Name), a2a.TransportProtocolJSONRPC),
			{URL: agentURLV03(s.publicURL, agent.Name), ProtocolBinding: a2a.TransportProtocolJSONRPC, ProtocolVersion: a2av0.Version},
		},
		Capabilities:       a2a.AgentCapabilities{Streaming: false, PushNotifications: false, ExtendedAgentCard: true},
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		SecuritySchemes: a2a.NamedSecuritySchemes{
			bearerScheme: a2a.HTTPAuthSecurityScheme{
				Scheme:      "Bearer",
				Description: "the token this agent's owner issued you when adding you as a peer",
			},
		},
		SecurityRequirements: a2a.SecurityRequirementsOptions{{bearerScheme: {}}},
	}
}

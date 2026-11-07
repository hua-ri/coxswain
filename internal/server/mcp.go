package server

import (
	"context"
	"net/http"
	"sync"

	mcpsurf "github.com/hua-ri/coxswain/internal/surface/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpHandler serves the tools over streamable HTTP. Each client name has
// one MCP server; each session is authorized once (the answer is cached
// for the session) and a revoked client's sessions are closed.
type mcpHandler struct {
	cfg Config

	mu       sync.Mutex
	servers  map[string]*sdk.Server
	sessions map[*sdk.ServerSession]*sessionAuth
}

// sessionAuth is one session's authorization, shared by its tool calls.
type sessionAuth struct {
	done chan struct{}
	ok   bool
}

func newMCPHandler(cfg Config) http.Handler {
	h := &mcpHandler{cfg: cfg, servers: map[string]*sdk.Server{}, sessions: map[*sdk.ServerSession]*sessionAuth{}}
	cfg.Clients.OnRevoke(h.revoked)
	return sdk.NewStreamableHTTPHandler(h.server, nil)
}

// server returns the MCP server for the request's client.
func (h *mcpHandler) server(r *http.Request) *sdk.Server {
	name := clientName(r)
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.servers[name]
	if !ok {
		s = mcpsurf.NewServer(h.cfg.Engine, Actor(name), h.cfg.Version, func(ctx context.Context, ss *sdk.ServerSession) bool {
			return h.authorized(ctx, name, ss)
		})
		h.servers[name] = s
	}
	return s
}

// authorized asks the Authorizer once per session and waits for the answer.
func (h *mcpHandler) authorized(ctx context.Context, name string, ss *sdk.ServerSession) bool {
	h.mu.Lock()
	a, ok := h.sessions[ss]
	if !ok {
		a = &sessionAuth{done: make(chan struct{})}
		h.sessions[ss] = a
		go func() {
			a.ok, _ = h.cfg.Clients.Authorize(context.Background(), name)
			close(a.done)
		}()
		go func() {
			ss.Wait()
			h.mu.Lock()
			delete(h.sessions, ss)
			h.mu.Unlock()
		}()
	}
	h.mu.Unlock()
	select {
	case <-a.done:
		return a.ok
	case <-ctx.Done():
		return false
	}
}

// revoked closes every session of a revoked client.
func (h *mcpHandler) revoked(name string) {
	h.mu.Lock()
	s := h.servers[name]
	h.mu.Unlock()
	if s == nil {
		return
	}
	for ss := range s.Sessions() {
		ss.Close()
	}
}

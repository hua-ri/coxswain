package server

import (
	"context"
	"net/http"
	"sync"
	"time"

	mcpsurf "github.com/hua-ri/coxswain/internal/surface/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpHandler serves the tools over streamable HTTP. Each client name has
// one MCP server. Every tool call is authorized (the Authorizer checks the
// store each time and limits dialogs), and a revoked client's sessions
// are closed.
type mcpHandler struct {
	cfg Config

	mu      sync.Mutex
	servers map[string]*sdk.Server
}

// sessionTimeout closes MCP sessions idle this long.
const sessionTimeout = 30 * time.Minute

func newMCPHandler(cfg Config) http.Handler {
	h := &mcpHandler{cfg: cfg, servers: map[string]*sdk.Server{}}
	cfg.Clients.OnRevoke(h.revoked)
	return sdk.NewStreamableHTTPHandler(h.server, &sdk.StreamableHTTPOptions{SessionTimeout: sessionTimeout})
}

// server returns the MCP server for the request's client.
func (h *mcpHandler) server(r *http.Request) *sdk.Server {
	name := clientName(r)
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.servers[name]
	if !ok {
		s = mcpsurf.NewServer(h.cfg.Engine, Actor(name), h.cfg.Version, func(ctx context.Context, _ *sdk.ServerSession) (bool, string) {
			return h.cfg.Clients.Authorize(ctx, name)
		})
		h.servers[name] = s
	}
	return s
}

// revoked closes every session of a revoked client, in the background:
// closing waits for the session's running calls, which the engine
// cancels (see New).
func (h *mcpHandler) revoked(name string) {
	h.mu.Lock()
	s := h.servers[name]
	h.mu.Unlock()
	if s == nil {
		return
	}
	for ss := range s.Sessions() {
		go ss.Close()
	}
}

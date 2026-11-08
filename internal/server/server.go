// Package server is `cox server`'s local HTTP API: /healthz, /api/v1/*
// for the CLI and the desktop app, and /mcp for agents. It listens on
// 127.0.0.1 only and every request but /healthz needs the bearer token.
package server

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/clients"
	"github.com/hua-ri/coxswain/internal/engine"
	"github.com/hua-ri/coxswain/internal/eventlog"
	"github.com/hua-ri/coxswain/internal/instance"
	"github.com/hua-ri/coxswain/internal/term"
)

// ClientHeader names the calling client (cli, desktop, claude-code, …).
const ClientHeader = "X-Cox-Client"

// SessionHeader optionally identifies the caller's session (e.g. one cox
// process) in the audit log.
const SessionHeader = "X-Cox-Session"

var sessionPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Config is what the server runs on.
type Config struct {
	Engine  *engine.Engine
	Clients *clients.Authorizer
	Token   string // bearer token; Run generates one if empty
	Version string
	Events  *eventlog.Log // activity stream source; defaults to Engine.Events
	// Terminals are the shared terminal sessions; nil disables
	// /api/v1/terminals. Run closes them all on shutdown.
	Terminals *term.Manager
	PID       int // reported by /healthz; defaults to this process
	// Port to listen on (Run only); 0 reuses the previous port if free.
	Port int
	// KeepAlive is the SSE comment interval; default 15s.
	KeepAlive time.Duration
	// Ready, if set, is called by Run once the server is listening and
	// server.json is written.
	Ready func(instance.Info)
}

func (c *Config) defaults() {
	if c.Events == nil && c.Engine != nil {
		c.Events = c.Engine.Events
	}
	if c.PID == 0 {
		c.PID = os.Getpid()
	}
	if c.KeepAlive == 0 {
		c.KeepAlive = 15 * time.Second
	}
}

// New returns the HTTP handler.
func New(cfg Config) http.Handler {
	cfg.defaults()
	s := &server{cfg: cfg}
	api := http.NewServeMux()
	api.HandleFunc("GET /api/v1/actions", s.authorized(s.listActions))
	api.HandleFunc("POST /api/v1/actions/{id}", s.authorized(s.invoke))
	api.HandleFunc("GET /api/v1/events", s.builtInOnly(s.events))
	api.HandleFunc("GET /api/v1/clients", s.builtInOnly(s.listClients))
	api.HandleFunc("DELETE /api/v1/clients/{name}", s.builtInOnly(s.revokeClient))
	api.HandleFunc("PUT /api/v1/clients/{name}", s.builtInOnly(s.allowClient))
	s.terminalRoutes(api)
	api.Handle("/mcp", newMCPHandler(cfg))

	root := http.NewServeMux()
	root.HandleFunc("GET /healthz", s.healthz)
	root.Handle("/", s.authenticated(api))
	return rejectBrowsers(root)
}

type server struct{ cfg Config }

// rejectBrowsers refuses any request with an Origin header: only local
// programs, never web pages, may talk to the server.
func rejectBrowsers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" {
			fail(w, http.StatusForbidden, "browser requests are not allowed", "")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authenticated checks the bearer token (in constant time) and that the
// request names a valid client.
func (s *server) authenticated(next http.Handler) http.Handler {
	want := []byte("Bearer " + s.cfg.Token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if s.cfg.Token == "" || subtle.ConstantTimeCompare(got, want) != 1 {
			fail(w, http.StatusUnauthorized, "missing or wrong token (see ~/.coxswain/server.json)", "")
			return
		}
		if !clients.ValidName(r.Header.Get(ClientHeader)) {
			fail(w, http.StatusBadRequest, "missing or invalid "+ClientHeader+" header", "")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authorized lets through built-in clients and agents the user allowed.
// Refused action calls are audited.
func (s *server) authorized(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		client := clientName(r)
		if ok, reason := s.cfg.Clients.Authorize(r.Context(), client); !ok {
			if id := r.PathValue("id"); id != "" {
				s.cfg.Engine.Refuse(actorOf(r), id, reason)
			}
			fail(w, http.StatusForbidden, clients.NotAuthorized, clients.NotAuthorizedHint(client, reason))
			return
		}
		next(w, r)
	}
}

// builtInOnly limits a route to the user's own entry points.
func (s *server) builtInOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !clients.BuiltIn(clientName(r)) {
			fail(w, http.StatusForbidden, "only cox (run by you) and the desktop app may do this", "")
			return
		}
		next(w, r)
	}
}

func clientName(r *http.Request) string { return r.Header.Get(ClientHeader) }

// actorOf is the request's client as recorded in the audit log.
func actorOf(r *http.Request) action.Actor {
	a := Actor(clientName(r))
	if s := r.Header.Get(SessionHeader); sessionPattern.MatchString(s) {
		a.Session = s
	}
	return a
}

// Actor is who a client is in the audit log: the user's own entry points
// are the human, anything else is an agent.
func Actor(client string) action.Actor {
	if clients.BuiltIn(client) {
		return action.Actor{Kind: action.Human, Name: client}
	}
	return action.Actor{Kind: action.Agent, Name: client}
}

func (s *server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, instance.Health{Version: s.cfg.Version, PID: s.cfg.PID})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// fail writes an error in the same shape as an action outcome.
func fail(w http.ResponseWriter, code int, msg, hint string) {
	writeJSON(w, code, engine.Outcome{Status: engine.StatusError, Error: msg, Hint: hint})
}

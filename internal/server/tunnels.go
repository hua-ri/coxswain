package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/hua-ri/coxswain/internal/inventory"
	"github.com/hua-ri/coxswain/internal/tunnel"
)

func (s *server) tunnelRoutes(api *http.ServeMux) {
	api.HandleFunc("GET /api/v1/tunnels", s.authorized(s.listTunnels))
	api.HandleFunc("GET /api/v1/tunnels/{id}", s.authorized(s.getTunnel))
	api.HandleFunc("POST /api/v1/tunnels", s.builtInOnly(s.openTunnel))
	api.HandleFunc("DELETE /api/v1/tunnels/{id}", s.builtInOnly(s.closeTunnel))
}

// tunnels returns the tunnel manager, or answers 503 if there is none.
func (s *server) tunnels(w http.ResponseWriter) *tunnel.Manager {
	if s.cfg.Tunnels == nil {
		fail(w, http.StatusServiceUnavailable, "tunnels are not available", "")
	}
	return s.cfg.Tunnels
}

func (s *server) listTunnels(w http.ResponseWriter, _ *http.Request) {
	if m := s.tunnels(w); m != nil {
		writeJSON(w, http.StatusOK, m.List())
	}
}

func (s *server) getTunnel(w http.ResponseWriter, r *http.Request) {
	m := s.tunnels(w)
	if m == nil {
		return
	}
	tn, ok := m.Get(r.PathValue("id"))
	if !ok {
		fail(w, http.StatusNotFound, tunnel.ErrNotFound.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, tn)
}

// openTunnel opens a tunnel for the user; the body is a tunnel.Spec
// ({host, type, local_port, remote_host, remote_port, idle_minutes}).
// Agents open tunnels with the tunnel.open action instead.
func (s *server) openTunnel(w http.ResponseWriter, r *http.Request) {
	m := s.tunnels(w)
	if m == nil {
		return
	}
	var in tunnel.Spec
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&in); err != nil {
		fail(w, http.StatusBadRequest, "body must be {host, type, local_port, remote_host, remote_port, idle_minutes}: "+err.Error(), "")
		return
	}
	owner := actorOf(r)
	owner.Session = ""
	tn, err := m.Open(r.Context(), in, owner)
	if err != nil {
		tunnelError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, tn)
}

func (s *server) closeTunnel(w http.ResponseWriter, r *http.Request) {
	m := s.tunnels(w)
	if m == nil {
		return
	}
	if err := m.Close(r.PathValue("id"), actorOf(r)); err != nil {
		tunnelError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// tunnelError maps a tunnel error to an HTTP status.
func tunnelError(w http.ResponseWriter, err error) {
	code := http.StatusBadGateway // could not reach the host
	switch {
	case errors.Is(err, tunnel.ErrNotFound), errors.Is(err, inventory.ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, tunnel.ErrInvalid):
		code = http.StatusBadRequest
	case errors.Is(err, tunnel.ErrListen), errors.Is(err, tunnel.ErrTooMany):
		code = http.StatusConflict
	}
	fail(w, code, err.Error(), "")
}

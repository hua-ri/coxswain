package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/clients"
)

// maxBody bounds an action's JSON arguments.
const maxBody = 1 << 20

// ActionInfo is one entry of GET /api/v1/actions.
type ActionInfo struct {
	ID          string             `json:"id"`
	Summary     string             `json:"summary"`
	Description string             `json:"description"`
	Input       map[string]any     `json:"input"`
	Positional  []string           `json:"positional,omitempty"`
	RestArgs    bool               `json:"rest_args,omitempty"`
	Annotations action.Annotations `json:"annotations"`
}

func (s *server) listActions(w http.ResponseWriter, _ *http.Request) {
	out := []ActionInfo{}
	for _, a := range s.cfg.Engine.Registry.List() {
		out = append(out, ActionInfo{
			ID: a.ID, Summary: a.Summary, Description: a.Description, Input: a.Input,
			Positional: a.Positional, RestArgs: a.RestArgs, Annotations: a.Annotations,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// invoke runs one action for the calling client. Agent clients must be
// authorized by the user first.
func (s *server) invoke(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.cfg.Engine.Registry.Get(id); !ok {
		fail(w, http.StatusNotFound, "unknown action "+id, "")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		fail(w, http.StatusBadRequest, "read arguments: "+err.Error(), "")
		return
	}
	if len(body) == 0 {
		body = []byte("{}")
	}
	if !json.Valid(body) {
		fail(w, http.StatusBadRequest, "arguments must be JSON", "")
		return
	}
	writeJSON(w, http.StatusOK, s.cfg.Engine.Invoke(r.Context(), Actor(clientName(r)), id, body))
}

func (s *server) listClients(w http.ResponseWriter, _ *http.Request) {
	list, err := s.cfg.Clients.Store.List()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// revokeClient records the name as revoked and disconnects it.
func (s *server) revokeClient(w http.ResponseWriter, r *http.Request) {
	s.decideClient(w, s.cfg.Clients.Revoke(r.PathValue("name")))
}

// allowClient lets the name use Coxswain.
func (s *server) allowClient(w http.ResponseWriter, r *http.Request) {
	s.decideClient(w, s.cfg.Clients.Allow(r.PathValue("name")))
}

func (s *server) decideClient(w http.ResponseWriter, err error) {
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, clients.ErrInvalidName), errors.Is(err, clients.ErrBuiltIn):
		fail(w, http.StatusBadRequest, err.Error(), "")
	default:
		fail(w, http.StatusInternalServerError, err.Error(), "")
	}
}

package server

import (
	"errors"
	"net/http"

	"github.com/hua-ri/coxswain/internal/transfer"
)

func (s *server) transferRoutes(api *http.ServeMux) {
	api.HandleFunc("GET /api/v1/transfers", s.authorized(s.listTransfers))
	api.HandleFunc("GET /api/v1/transfers/{id}", s.authorized(s.getTransfer))
	api.HandleFunc("DELETE /api/v1/transfers/{id}", s.builtInOnly(s.cancelTransfer))
}

// transfers returns the transfer manager, or answers 503 if there is none.
func (s *server) transfers(w http.ResponseWriter) *transfer.Manager {
	if s.cfg.Transfers == nil {
		fail(w, http.StatusServiceUnavailable, "transfers are not available", "")
	}
	return s.cfg.Transfers
}

func (s *server) listTransfers(w http.ResponseWriter, _ *http.Request) {
	if m := s.transfers(w); m != nil {
		writeJSON(w, http.StatusOK, m.List())
	}
}

func (s *server) getTransfer(w http.ResponseWriter, r *http.Request) {
	m := s.transfers(w)
	if m == nil {
		return
	}
	t, ok := m.Get(r.PathValue("id"))
	if !ok {
		fail(w, http.StatusNotFound, transfer.ErrNotFound.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// cancelTransfer stops a running transfer (the user may cancel any).
func (s *server) cancelTransfer(w http.ResponseWriter, r *http.Request) {
	m := s.transfers(w)
	if m == nil {
		return
	}
	switch err := m.Cancel(r.PathValue("id"), actorOf(r)); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, transfer.ErrNotFound):
		fail(w, http.StatusNotFound, err.Error(), "")
	case errors.Is(err, transfer.ErrFinished):
		fail(w, http.StatusConflict, err.Error(), "")
	default:
		fail(w, http.StatusInternalServerError, err.Error(), "")
	}
}

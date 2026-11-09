package server

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/hua-ri/coxswain/internal/inventory"
	"github.com/hua-ri/coxswain/internal/term"
)

// maxInput bounds one terminal input request.
const maxInput = 64 << 10

// OpenTerminal is the body of POST /api/v1/terminals: a host, or a pod.
type OpenTerminal struct {
	Host string        `json:"host,omitempty"`
	Pod  *term.PodSpec `json:"pod,omitempty"`
	Cols int           `json:"cols"`
	Rows int           `json:"rows"`
}

// TerminalSize is the body of POST /api/v1/terminals/{id}/resize.
type TerminalSize struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

// TerminalExit is the data of a terminal stream's exit event; Code is
// null if the shell reported none (e.g. the terminal was closed).
type TerminalExit struct {
	Code *int `json:"code"`
}

func (s *server) terminalRoutes(api *http.ServeMux) {
	api.HandleFunc("GET /api/v1/terminals", s.authorized(s.listTerminals))
	api.HandleFunc("POST /api/v1/terminals", s.builtInOnly(s.openTerminal))
	api.HandleFunc("GET /api/v1/terminals/{id}/stream", s.builtInOnly(s.streamTerminal))
	api.HandleFunc("POST /api/v1/terminals/{id}/input", s.builtInOnly(s.terminalInput))
	api.HandleFunc("POST /api/v1/terminals/{id}/resize", s.builtInOnly(s.resizeTerminal))
	api.HandleFunc("DELETE /api/v1/terminals/{id}", s.builtInOnly(s.closeTerminal))
}

// terms returns the terminal manager, or answers 503 if there is none.
func (s *server) terms(w http.ResponseWriter) *term.Manager {
	if s.cfg.Terminals == nil {
		fail(w, http.StatusServiceUnavailable, "terminals are not available", "")
	}
	return s.cfg.Terminals
}

func (s *server) listTerminals(w http.ResponseWriter, _ *http.Request) {
	if m := s.terms(w); m != nil {
		writeJSON(w, http.StatusOK, m.List())
	}
}

func (s *server) openTerminal(w http.ResponseWriter, r *http.Request) {
	m := s.terms(w)
	if m == nil {
		return
	}
	var in OpenTerminal
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&in); err != nil {
		fail(w, http.StatusBadRequest, "body must be {host, cols, rows} or {pod: {context, namespace, pod, container, shell}, cols, rows}: "+err.Error(), "")
		return
	}
	if (in.Host == "") == (in.Pod == nil) {
		fail(w, http.StatusBadRequest, "give either host or pod", "")
		return
	}
	var sess term.Session
	var err error
	if in.Pod != nil {
		sess, err = m.OpenPod(r.Context(), *in.Pod, actorOf(r), in.Cols, in.Rows)
	} else {
		sess, err = m.OpenHost(r.Context(), in.Host, actorOf(r), in.Cols, in.Rows)
	}
	if err != nil {
		terminalError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sess)
}

// streamTerminal sends a terminal's output as Server-Sent Events: first
// `data` with the backlog, then `data` (base64) for each chunk, `gap` when
// output was dropped for this client, and finally `exit`.
func (s *server) streamTerminal(w http.ResponseWriter, r *http.Request) {
	m := s.terms(w)
	if m == nil {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		fail(w, http.StatusInternalServerError, "streaming unsupported", "")
		return
	}
	backlog, ch, cancel, err := m.Subscribe(r.PathValue("id"))
	if err != nil {
		terminalError(w, err)
		return
	}
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if writeSSE(w, "data", base64.StdEncoding.EncodeToString(backlog)) != nil {
		return
	}
	flusher.Flush()

	tick := time.NewTicker(s.cfg.KeepAlive)
	defer tick.Stop()
	for {
		var err error
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			_, err = fmt.Fprint(w, ": keep-alive\n\n")
		case c, ok := <-ch:
			if !ok {
				return
			}
			if c.Gap {
				err = writeSSE(w, "gap", "{}")
			}
			if err == nil && len(c.Data) > 0 {
				err = writeSSE(w, "data", base64.StdEncoding.EncodeToString(c.Data))
			}
			if err == nil && c.Exit {
				b, _ := json.Marshal(TerminalExit{Code: c.Code})
				err = writeSSE(w, "exit", string(b))
			}
		}
		if err != nil {
			return
		}
		flusher.Flush()
	}
}

func writeSSE(w io.Writer, event, data string) error {
	_, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	return err
}

// terminalInput sends the raw body (at most 64 KiB) as keystrokes.
func (s *server) terminalInput(w http.ResponseWriter, r *http.Request) {
	m := s.terms(w)
	if m == nil {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxInput))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			fail(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("input is limited to %d bytes per request", maxInput), "")
			return
		}
		fail(w, http.StatusBadRequest, "read input: "+err.Error(), "")
		return
	}
	if err := m.Write(r.PathValue("id"), body); err != nil {
		terminalError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) resizeTerminal(w http.ResponseWriter, r *http.Request) {
	m := s.terms(w)
	if m == nil {
		return
	}
	var in TerminalSize
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&in); err != nil {
		fail(w, http.StatusBadRequest, "body must be {cols, rows}: "+err.Error(), "")
		return
	}
	if err := m.Resize(r.PathValue("id"), in.Cols, in.Rows); err != nil {
		terminalError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) closeTerminal(w http.ResponseWriter, r *http.Request) {
	m := s.terms(w)
	if m == nil {
		return
	}
	if err := m.Close(r.PathValue("id"), actorOf(r)); err != nil {
		terminalError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// terminalError maps a term error to an HTTP status.
func terminalError(w http.ResponseWriter, err error) {
	code := http.StatusBadGateway // could not reach the host
	switch {
	case errors.Is(err, term.ErrNotFound), errors.Is(err, inventory.ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, term.ErrClosed), errors.Is(err, term.ErrTooMany):
		code = http.StatusConflict
	case errors.Is(err, term.ErrSize), errors.Is(err, term.ErrPodSpec):
		code = http.StatusBadRequest
	case errors.Is(err, term.ErrNoPods):
		code = http.StatusServiceUnavailable
	}
	fail(w, code, err.Error(), "")
}

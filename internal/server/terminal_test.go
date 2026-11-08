package server

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hua-ri/coxswain/internal/eventlog"
	"github.com/hua-ri/coxswain/internal/inventory"
	"github.com/hua-ri/coxswain/internal/sshx"
	"github.com/hua-ri/coxswain/internal/sshx/sshtest"
	"github.com/hua-ri/coxswain/internal/term"
)

// withTerminals gives the fixture a terminal manager with one host, dev,
// served by a test SSH server.
func withTerminals(t *testing.T, f *fixture) *sshtest.Server {
	t.Helper()
	srv := sshtest.Start(t)
	hosts := inventory.NewStore(filepath.Join(t.TempDir(), "hosts.json"))
	hosts.Merge([]inventory.Host{{Name: "dev", HostName: srv.Host, Port: srv.Port, IdentityFiles: []string{srv.KeyFile}}})
	f.cfg.Terminals = &term.Manager{
		Hosts:  hosts,
		Dialer: &sshx.Dialer{Hosts: hosts, KnownHosts: []string{srv.KnownHosts}},
		Events: f.cfg.Events,
	}
	t.Cleanup(f.cfg.Terminals.CloseAll)
	return srv
}

func openTerminal(t *testing.T, h http.Handler) term.Session {
	t.Helper()
	w := request(t, h, "POST", "/api/v1/terminals", `{"host":"dev","cols":90,"rows":20}`, map[string]string{"X-Cox-Client": "desktop"})
	var s term.Session
	if err := json.Unmarshal(w.Body.Bytes(), &s); w.Code != 201 || err != nil {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	return s
}

// sseEvent is one Server-Sent Event.
type sseEvent struct{ name, data string }

// stream opens a terminal's SSE stream and returns its events.
func stream(t *testing.T, url, id, client string) (<-chan sseEvent, *http.Response) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", url+"/api/v1/terminals/"+id+"/stream", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(ClientHeader, client)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	ch := make(chan sseEvent, 100)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(resp.Body)
		var ev sseEvent
		for sc.Scan() {
			l := sc.Text()
			switch {
			case l == "":
				if ev.name != "" {
					ch <- ev
				}
				ev = sseEvent{}
			case strings.HasPrefix(l, "event: "):
				ev.name = strings.TrimPrefix(l, "event: ")
			case strings.HasPrefix(l, "data: "):
				ev.data = strings.TrimPrefix(l, "data: ")
			}
		}
	}()
	return ch, resp
}

// readUntil collects decoded data events until the output contains want.
func readUntil(t *testing.T, ch <-chan sseEvent, want string) string {
	t.Helper()
	var got strings.Builder
	deadline := time.After(5 * time.Second)
	for !strings.Contains(got.String(), want) {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("stream ended: %q", got.String())
			}
			if ev.name == "data" {
				b, err := base64.StdEncoding.DecodeString(ev.data)
				if err != nil {
					t.Fatal(err)
				}
				got.Write(b)
			}
		case <-deadline:
			t.Fatalf("waiting for %q: %q", want, got.String())
		}
	}
	return got.String()
}

func TestTerminalAPI(t *testing.T) {
	f := newConfig(t)
	sshSrv := withTerminals(t, &f)
	h := New(f.cfg)
	srv := httptest.NewServer(h)
	defer srv.Close()

	s := openTerminal(t, h)
	if s.Host != "dev" || s.Cols != 90 || s.Rows != 20 || s.Owner.Name != "desktop" || s.Owner.Kind != "human" {
		t.Fatalf("%+v", s)
	}
	<-sshSrv.Sizes
	if w := request(t, h, "POST", "/api/v1/terminals/"+s.ID+"/input", "echo one\r", nil); w.Code != 204 {
		t.Fatalf("input: %d %s", w.Code, w.Body)
	}
	ch, resp := stream(t, srv.URL, s.ID, "desktop")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("%d", resp.StatusCode)
	}
	// The first event is the backlog (possibly empty if "one" is not out yet).
	if first := <-ch; first.name != "data" {
		t.Fatalf("first event %+v", first)
	}
	request(t, h, "POST", "/api/v1/terminals/"+s.ID+"/input", "echo two\r", nil)
	readUntil(t, ch, "two\n")
	if w := request(t, h, "POST", "/api/v1/terminals/"+s.ID+"/resize", `{"cols":100,"rows":30}`, nil); w.Code != 204 {
		t.Fatalf("resize: %d %s", w.Code, w.Body)
	}
	if got := <-sshSrv.Sizes; got.Cols != 100 || got.Rows != 30 {
		t.Fatalf("%+v", got)
	}
	// A new stream starts with everything so far.
	ch2, _ := stream(t, srv.URL, s.ID, "cli")
	first := <-ch2
	b, _ := base64.StdEncoding.DecodeString(first.data)
	if first.name != "data" || !strings.Contains(string(b), "one\ntwo\n") {
		t.Fatalf("backlog %+v %q", first, b)
	}

	w := request(t, h, "GET", "/api/v1/terminals", "", nil)
	var list []term.Session
	json.Unmarshal(w.Body.Bytes(), &list)
	if w.Code != 200 || len(list) != 1 || list[0].ID != s.ID || list[0].Cols != 100 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}

	request(t, h, "POST", "/api/v1/terminals/"+s.ID+"/input", "exit 4\r", nil)
	deadline := time.After(5 * time.Second)
	for done := false; !done; {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("stream ended without exit")
			}
			if ev.name == "exit" {
				if ev.data != `{"code":4}` {
					t.Fatalf("exit %q", ev.data)
				}
				done = true
			}
		case <-deadline:
			t.Fatal("no exit event")
		}
	}
	if _, ok := <-ch; ok {
		// Drain: the stream must end after exit.
		for range ch {
		}
	}
	if w := request(t, h, "POST", "/api/v1/terminals/"+s.ID+"/input", "x", nil); w.Code != 409 {
		t.Fatalf("input after exit: %d %s", w.Code, w.Body)
	}
	if w := request(t, h, "DELETE", "/api/v1/terminals/"+s.ID, "", nil); w.Code != 204 {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if w := request(t, h, "DELETE", "/api/v1/terminals/"+s.ID, "", nil); w.Code != 404 {
		t.Fatalf("delete again: %d %s", w.Code, w.Body)
	}
}

func TestTerminalAPIErrors(t *testing.T) {
	f := newConfig(t)
	withTerminals(t, &f)
	h := New(f.cfg)
	s := openTerminal(t, h)
	cases := []struct {
		name, method, path, body string
		want                     int
	}{
		{"unknown host", "POST", "/api/v1/terminals", `{"host":"nope"}`, 404},
		{"bad json", "POST", "/api/v1/terminals", `{`, 400},
		{"bad size", "POST", "/api/v1/terminals", `{"host":"dev","cols":0,"rows":9999}`, 400},
		{"unknown input", "POST", "/api/v1/terminals/t_000000000000/input", "x", 404},
		{"input too big", "POST", "/api/v1/terminals/" + s.ID + "/input", strings.Repeat("x", 64<<10+1), 413},
		{"bad resize", "POST", "/api/v1/terminals/" + s.ID + "/resize", `{"cols":0,"rows":1}`, 400},
		{"unknown stream", "GET", "/api/v1/terminals/nope/stream", "", 404},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if w := request(t, h, c.method, c.path, c.body, nil); w.Code != c.want {
				t.Fatalf("got %d want %d: %s", w.Code, c.want, w.Body)
			}
		})
	}
}

// TestTerminalAPIAccess: agents may list terminals (once allowed) but
// use them only through actions.
func TestTerminalAPIAccess(t *testing.T) {
	f := newConfig(t)
	withTerminals(t, &f)
	h := New(f.cfg)
	s := openTerminal(t, h)
	codex := map[string]string{"X-Cox-Client": "codex"}
	if w := request(t, h, "GET", "/api/v1/terminals", "", codex); w.Code != 403 {
		t.Fatalf("unauthorized agent listed terminals: %d", w.Code)
	}
	f.cfg.Clients.Allow("codex")
	if w := request(t, h, "GET", "/api/v1/terminals", "", codex); w.Code != 200 {
		t.Fatalf("authorized agent: %d %s", w.Code, w.Body)
	}
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/api/v1/terminals", `{"host":"dev"}`},
		{"GET", "/api/v1/terminals/" + s.ID + "/stream", ""},
		{"POST", "/api/v1/terminals/" + s.ID + "/input", "x"},
		{"POST", "/api/v1/terminals/" + s.ID + "/resize", `{"cols":80,"rows":24}`},
		{"DELETE", "/api/v1/terminals/" + s.ID, ""},
	} {
		if w := request(t, h, c.method, c.path, c.body, codex); w.Code != 403 {
			t.Fatalf("agent %s %s: %d %s", c.method, c.path, w.Code, w.Body)
		}
	}
	if _, ok := f.cfg.Terminals.Get(s.ID); !ok {
		t.Fatal("terminal gone")
	}
}

func TestTerminalEvents(t *testing.T) {
	f := newConfig(t)
	withTerminals(t, &f)
	h := New(f.cfg)
	s := openTerminal(t, h)
	request(t, h, "DELETE", "/api/v1/terminals/"+s.ID, "", map[string]string{"X-Cox-Client": "cli"})
	var types []string
	for _, ev := range lastEvents(t, f) {
		if ev.Terminal == s.ID {
			types = append(types, ev.Type+" "+ev.Actor.String()+" "+ev.Reason)
		}
	}
	want := []string{eventlog.TerminalOpened + " human:desktop ", eventlog.TerminalClosed + " human:desktop closed by human:cli"}
	if strings.Join(types, "|") != strings.Join(want, "|") {
		t.Fatalf("%q", types)
	}
}

// TestRunClosesTerminals: stopping the server ends every terminal.
func TestRunClosesTerminals(t *testing.T) {
	f := newConfig(t)
	withTerminals(t, &f)
	f.cfg.Token = ""
	_, stop := runReady(t, f.cfg, t.TempDir())
	if _, err := f.cfg.Terminals.OpenHost(context.Background(), "dev", Actor("cli"), 80, 24); err != nil {
		t.Fatal(err)
	}
	stop()
	if list := f.cfg.Terminals.List(); len(list) != 0 {
		t.Fatalf("%+v", list)
	}
}

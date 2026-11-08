package desktop

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/clients"
	"github.com/hua-ri/coxswain/internal/engine"
	"github.com/hua-ri/coxswain/internal/eventlog"
	"github.com/hua-ri/coxswain/internal/instance"
	"github.com/hua-ri/coxswain/internal/inventory"
	"github.com/hua-ri/coxswain/internal/server"
	"github.com/hua-ri/coxswain/internal/sshx"
	"github.com/hua-ri/coxswain/internal/sshx/sshtest"
	"github.com/hua-ri/coxswain/internal/term"
)

// message is one window.__cox.emit call.
type message struct {
	sub     string
	payload map[string]any
}

// fakeWindow runs dispatched functions at once and decodes emit calls.
type fakeWindow struct {
	t  *testing.T
	ch chan message
}

func newWindow(t *testing.T) *fakeWindow { return &fakeWindow{t: t, ch: make(chan message, 1000)} }

func (w *fakeWindow) Dispatch(f func()) { f() }

func (w *fakeWindow) Eval(js string) {
	rest, ok := strings.CutPrefix(js, "window.__cox&&window.__cox.emit(")
	if !ok || !strings.HasSuffix(rest, ")") {
		w.t.Errorf("unexpected js %q", js)
		return
	}
	dec := json.NewDecoder(strings.NewReader("[" + strings.TrimSuffix(rest, ")") + "]"))
	var args []json.RawMessage
	if err := dec.Decode(&args); err != nil || len(args) != 2 {
		w.t.Errorf("bad emit %q: %v", js, err)
		return
	}
	var m message
	json.Unmarshal(args[0], &m.sub)
	json.Unmarshal(args[1], &m.payload)
	w.ch <- m
}

// next waits for a message on sub matching ok, skipping others.
func (w *fakeWindow) next(sub string, ok func(map[string]any) bool) map[string]any {
	w.t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case m := <-w.ch:
			if m.sub == sub && ok(m.payload) {
				return m.payload
			}
		case <-deadline:
			w.t.Fatalf("no matching message on %s", sub)
		}
	}
}

func ofType(typ string) func(map[string]any) bool {
	return func(p map[string]any) bool { return p["type"] == typ }
}

// testServer runs real `cox server`s (one at a time) in home.
type testServer struct {
	t    *testing.T
	home string
	cfg  server.Config
	ssh  *sshtest.Server
	stop func()
}

func newServer(t *testing.T) *testServer {
	t.Helper()
	dir := t.TempDir()
	reg := action.NewRegistry()
	reg.Register(&action.Action{
		ID: "demo.echo", Summary: "echo", Input: map[string]any{"type": "object"},
		Label: func(context.Context, json.RawMessage) (action.Assessment, error) { return action.Assessment{}, nil },
		Run:   func(context.Context, json.RawMessage) (any, error) { return map[string]string{"hello": "world"}, nil },
	})
	events := eventlog.Open(filepath.Join(dir, "events"))
	srv := sshtest.Start(t)
	hosts := inventory.NewStore(filepath.Join(dir, "hosts.json"))
	hosts.Merge([]inventory.Host{{Name: "dev", HostName: srv.Host, Port: srv.Port, IdentityFiles: []string{srv.KeyFile}}})
	s := &testServer{t: t, home: filepath.Join(dir, "home"), ssh: srv, cfg: server.Config{
		Engine:  &engine.Engine{Registry: reg, Events: events},
		Clients: &clients.Authorizer{Store: clients.NewStore(filepath.Join(dir, "clients.json"))},
		Version: "v-test",
		Terminals: &term.Manager{
			Hosts: hosts, Dialer: &sshx.Dialer{Hosts: hosts, KnownHosts: []string{srv.KnownHosts}}, Events: events,
		},
		KeepAlive: 50 * time.Millisecond,
	}}
	t.Cleanup(func() {
		if s.stop != nil {
			s.stop()
		}
	})
	return s
}

func (s *testServer) start() {
	s.t.Helper()
	ready := make(chan struct{})
	cfg := s.cfg
	cfg.Ready = func(instance.Info) { close(ready) }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx, cfg, s.home) }()
	select {
	case <-ready:
	case err := <-done:
		s.t.Fatal(err)
	case <-time.After(5 * time.Second):
		s.t.Fatal("server not ready")
	}
	s.stop = func() { cancel(); <-done; s.stop = nil }
}

func startBridge(t *testing.T, s *testServer) (*Bridge, *fakeWindow) {
	t.Helper()
	w := newWindow(t)
	b := New(w, s.home)
	b.Retry = 50 * time.Millisecond
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	return b, w
}

func TestStartWithoutServer(t *testing.T) {
	b := New(newWindow(t), t.TempDir())
	if err := b.Start(context.Background()); !errors.Is(err, instance.ErrNotRunning) {
		t.Fatalf("%v", err)
	}
}

func TestAPI(t *testing.T) {
	s := newServer(t)
	s.start()
	b, w := startBridge(t, s)
	if st := b.Status(); !st.Connected || st.Version != "v-test" || !strings.HasPrefix(st.Addr, "127.0.0.1:") {
		t.Fatalf("%+v", st)
	}
	out, err := b.API("POST", "/api/v1/actions/demo.echo", map[string]any{})
	if m, _ := out.(map[string]any); err != nil || m["status"] != "ok" {
		t.Fatalf("%v %v", out, err)
	}
	if out, err := b.API("GET", "/api/v1/clients", nil); err != nil {
		t.Fatalf("%v %v", out, err)
	}
	if out, err := b.API("PUT", "/api/v1/clients/codex", nil); err != nil || out != nil {
		t.Fatalf("%v %v", out, err)
	}
	if _, err := b.API("DELETE", "/api/v1/clients/cli", nil); err == nil || !strings.Contains(err.Error(), "built-in client") {
		t.Fatalf("server error not passed on: %v", err)
	}
	for _, p := range []string{"/healthz", "/mcp", "/api/v1/events", "/api/v1/terminals/t_1/stream", "http://x/api/v1/actions"} {
		if _, err := b.API("GET", p, nil); err == nil {
			t.Errorf("%s allowed", p)
		}
	}
	if _, err := b.Subscribe("nope", ""); err == nil {
		t.Error("unknown kind")
	}
	if _, err := b.Subscribe("terminal", ""); err == nil {
		t.Error("terminal without id")
	}
	gone, _ := b.Subscribe("terminal", "t_000000000000")
	if exit := w.next(gone, ofType("exit")); exit["code"] != nil {
		t.Fatalf("%v", exit)
	}
	// The page's coxApi answers at once with a call id; the result follows.
	call := b.APIAsync("POST", "/api/v1/actions/demo.echo", map[string]any{})
	reply := w.next(ReplyChannel, func(p map[string]any) bool { return p["call"] == call })
	if v, _ := reply["value"].(map[string]any); reply["ok"] != true || v["status"] != "ok" {
		t.Fatalf("%v", reply)
	}
	call = b.APIAsync("GET", "/healthz", nil)
	reply = w.next(ReplyChannel, func(p map[string]any) bool { return p["call"] == call })
	if e, _ := reply["error"].(string); reply["ok"] != false || !strings.Contains(e, "not an API path") {
		t.Fatalf("%v", reply)
	}
	if names := b.Bindings(); len(names) != 6 || names["coxApi"] == nil || names["coxSubscribe"] == nil {
		t.Fatalf("%v", names)
	}
}

func TestEventsSubscription(t *testing.T) {
	s := newServer(t)
	s.start()
	b, w := startBridge(t, s)
	sub, err := b.Subscribe("events", "")
	if err != nil {
		t.Fatal(err)
	}
	// The stream may not be open yet: invoke until an event arrives.
	got := make(chan map[string]any, 1)
	go func() {
		got <- w.next(sub, func(p map[string]any) bool {
			ev, _ := p["event"].(map[string]any)
			return p["type"] == "event" && ev["action"] == "demo.echo" && ev["type"] == eventlog.ActionRequested
		})
	}()
	for i := 0; ; i++ {
		b.API("POST", "/api/v1/actions/demo.echo", nil)
		select {
		case p := <-got:
			if ev := p["event"].(map[string]any); ev["actor"].(map[string]any)["name"] != "desktop" {
				t.Fatalf("%v", p)
			}
			b.Unsubscribe(sub)
			b.Unsubscribe(sub)
			return
		case <-time.After(100 * time.Millisecond):
		}
		if i > 50 {
			t.Fatal("no event")
		}
	}
}

func decode(t *testing.T, p map[string]any) string {
	t.Helper()
	s, _ := p["data"].(string)
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestTerminalSubscription(t *testing.T) {
	s := newServer(t)
	s.start()
	b, w := startBridge(t, s)
	out, err := b.API("POST", "/api/v1/terminals", map[string]any{"host": "dev", "cols": 100, "rows": 30})
	if err != nil {
		t.Fatal(err)
	}
	id := out.(map[string]any)["id"].(string)
	<-s.ssh.Sizes
	if err := b.Input(id, base64.StdEncoding.EncodeToString([]byte("echo first\r"))); err != nil {
		t.Fatal(err)
	}
	sub, err := b.Subscribe("terminal", id)
	if err != nil {
		t.Fatal(err)
	}
	first := w.next(sub, ofType("data"))
	if first["backlog"] != true {
		t.Fatalf("first message is not the backlog: %v", first)
	}
	var seen bytes.Buffer
	seen.WriteString(decode(t, first))
	b.Input(id, base64.StdEncoding.EncodeToString([]byte("echo second\r")))
	for !strings.Contains(seen.String(), "first\nsecond\n") {
		seen.WriteString(decode(t, w.next(sub, ofType("data"))))
	}
	if err := b.Resize(id, 120, 40); err != nil {
		t.Fatal(err)
	}
	if got := <-s.ssh.Sizes; got.Cols != 120 || got.Rows != 40 {
		t.Fatalf("%+v", got)
	}
	if err := b.Input(id, "not base64!"); err == nil {
		t.Fatal("bad base64 accepted")
	}
	if _, err := b.API("DELETE", "/api/v1/terminals/"+id, nil); err != nil {
		t.Fatal(err)
	}
	if exit := w.next(sub, ofType("exit")); exit["code"] != nil {
		t.Fatalf("%v", exit)
	}
}

// TestReconnect: when the server stops the page is told; when it is back,
// subscriptions resume (terminals ended with the old server).
func TestReconnect(t *testing.T) {
	s := newServer(t)
	s.start()
	b, w := startBridge(t, s)
	events, _ := b.Subscribe("events", "")
	out, _ := b.API("POST", "/api/v1/terminals", map[string]any{"host": "dev"})
	id := out.(map[string]any)["id"].(string)
	termSub, _ := b.Subscribe("terminal", id)
	w.next(termSub, ofType("data"))

	s.stop()
	// Stopping the server ends its terminals.
	if exit := w.next(termSub, ofType("exit")); exit["code"] != nil {
		t.Fatalf("%v", exit)
	}
	w.next(StatusChannel, ofType("disconnected"))
	if b.Status().Connected {
		t.Fatal("still connected")
	}
	if _, err := b.API("GET", "/api/v1/clients", nil); !errors.Is(err, instance.ErrNotRunning) {
		t.Fatalf("%v", err)
	}

	s.start()
	conn := w.next(StatusChannel, ofType("connected"))
	if conn["version"] != "v-test" {
		t.Fatalf("%v", conn)
	}
	b.mu.Lock()
	_, stale := b.subs[termSub]
	b.mu.Unlock()
	if stale {
		t.Fatal("ended terminal still subscribed")
	}
	got := make(chan struct{})
	go func() {
		w.next(events, func(p map[string]any) bool { return p["type"] == "event" })
		close(got)
	}()
	for i := 0; ; i++ {
		b.API("POST", "/api/v1/actions/demo.echo", nil)
		select {
		case <-got:
			return
		case <-time.After(100 * time.Millisecond):
		}
		if i > 50 {
			t.Fatal("events not resubscribed")
		}
	}
}

package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/clients"
	"github.com/hua-ri/coxswain/internal/engine"
	"github.com/hua-ri/coxswain/internal/eventlog"
	"github.com/hua-ri/coxswain/internal/filelock"
	"github.com/hua-ri/coxswain/internal/instance"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const token = "test-token"

type fixture struct {
	cfg   Config
	asked *atomic.Int32
	allow *atomic.Bool
}

// newConfig registers demo.echo (returns its args; {"fail":true} fails)
// and a fake dialog that answers with allow.
func newConfig(t *testing.T) fixture {
	t.Helper()
	reg := action.NewRegistry()
	reg.Register(&action.Action{
		ID: "demo.echo", Summary: "echo", Description: "echo args",
		Input:       map[string]any{"type": "object", "properties": map[string]any{"msg": map[string]any{"type": "string"}}},
		Positional:  []string{"msg"},
		RestArgs:    true,
		Annotations: action.Annotations{ReadOnly: true, Idempotent: true},
		Label: func(context.Context, json.RawMessage) (action.Assessment, error) {
			return action.Assessment{Risk: action.Read}, nil
		},
		Run: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in struct {
				Msg  string
				Fail bool
			}
			action.Decode(raw, &in)
			if in.Fail {
				return nil, errors.New("boom")
			}
			who, _ := action.ActorFrom(ctx)
			return map[string]string{"msg": in.Msg, "actor": who.String()}, nil
		},
	})
	dir := t.TempDir()
	events := eventlog.Open(filepath.Join(dir, "events"))
	f := fixture{asked: &atomic.Int32{}, allow: &atomic.Bool{}}
	f.cfg = Config{
		Engine: &engine.Engine{Registry: reg, Events: events},
		Clients: &clients.Authorizer{
			Store: clients.NewStore(filepath.Join(dir, "clients.json")),
			Ask: func(context.Context, string) (bool, error) {
				f.asked.Add(1)
				return f.allow.Load(), nil
			},
		},
		Token:     token,
		Version:   "v-test",
		Events:    events,
		KeepAlive: 50 * time.Millisecond,
	}
	return f
}

func request(t *testing.T, h http.Handler, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Cox-Client", "cli")
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestAuth(t *testing.T) {
	h := New(newConfig(t).cfg)
	cases := []struct {
		name string
		path string
		hdr  map[string]string
		want int
	}{
		{"ok", "/api/v1/actions", nil, 200},
		{"no token", "/api/v1/actions", map[string]string{"Authorization": ""}, 401},
		{"wrong token", "/api/v1/actions", map[string]string{"Authorization": "Bearer nope"}, 401},
		{"not bearer", "/api/v1/actions", map[string]string{"Authorization": token}, 401},
		{"origin", "/api/v1/actions", map[string]string{"Origin": "http://evil.example"}, 403},
		{"origin on healthz", "/healthz", map[string]string{"Origin": "http://evil.example"}, 403},
		{"no client", "/api/v1/actions", map[string]string{"X-Cox-Client": ""}, 400},
		{"bad client", "/api/v1/actions", map[string]string{"X-Cox-Client": "Evil Client"}, 400},
		{"healthz needs nothing", "/healthz", map[string]string{"Authorization": "", "X-Cox-Client": ""}, 200},
		{"mcp needs a token", "/mcp", map[string]string{"Authorization": ""}, 401},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if w := request(t, h, "GET", c.path, "", c.hdr); w.Code != c.want {
				t.Fatalf("got %d want %d: %s", w.Code, c.want, w.Body)
			}
		})
	}
}

func TestHealthz(t *testing.T) {
	f := newConfig(t)
	f.cfg.PID = 4242
	w := request(t, New(f.cfg), "GET", "/healthz", "", nil)
	var h instance.Health
	json.Unmarshal(w.Body.Bytes(), &h)
	if h.PID != 4242 || h.Version != "v-test" {
		t.Fatalf("%s", w.Body)
	}
}

func TestActionsList(t *testing.T) {
	w := request(t, New(newConfig(t).cfg), "GET", "/api/v1/actions", "", nil)
	var list []ActionInfo
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("%s", w.Body)
	}
	a := list[0]
	if a.ID != "demo.echo" || a.Summary != "echo" || a.Description != "echo args" || a.Input["type"] != "object" ||
		len(a.Positional) != 1 || !a.RestArgs || !a.Annotations.ReadOnly || !a.Annotations.Idempotent {
		t.Fatalf("%+v", a)
	}
	for _, k := range []string{`"rest_args"`, `"annotations"`, `"read_only"`, `"input"`} {
		if !strings.Contains(w.Body.String(), k) {
			t.Fatalf("missing %s: %s", k, w.Body)
		}
	}
}

func outcome(t *testing.T, w *httptest.ResponseRecorder) engine.Outcome {
	t.Helper()
	var out engine.Outcome
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("%v: %s", err, w.Body)
	}
	return out
}

func TestInvoke(t *testing.T) {
	h := New(newConfig(t).cfg)
	w := request(t, h, "POST", "/api/v1/actions/demo.echo", `{"msg":"hi"}`, nil)
	out := outcome(t, w)
	res, _ := out.Result.(map[string]any)
	if w.Code != 200 || out.Status != "ok" || res["msg"] != "hi" || !strings.HasPrefix(res["actor"].(string), "human:cli") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if out := outcome(t, request(t, h, "POST", "/api/v1/actions/demo.echo", "", map[string]string{"X-Cox-Client": "desktop"})); out.Status != "ok" {
		t.Fatalf("empty body: %+v", out)
	}

	w = request(t, h, "POST", "/api/v1/actions/demo.echo", `{"fail":true}`, nil)
	if out := outcome(t, w); w.Code != 200 || out.Status != "error" || out.Error != "boom" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	w = request(t, h, "POST", "/api/v1/actions/nope.x", `{}`, nil)
	if out := outcome(t, w); w.Code != 404 || out.Status != "error" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	w = request(t, h, "POST", "/api/v1/actions/demo.echo", `{not json`, nil)
	if out := outcome(t, w); w.Code != 400 || out.Status != "error" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestInvokeByAgentNeedsAuthorization(t *testing.T) {
	f := newConfig(t)
	h := New(f.cfg)
	codex := map[string]string{"X-Cox-Client": "codex"}
	w := request(t, h, "POST", "/api/v1/actions/demo.echo", `{}`, codex)
	if out := outcome(t, w); w.Code != 403 || out.Error != "client_not_authorized" || out.Hint == "" || f.asked.Load() != 1 {
		t.Fatalf("%d %s asked=%d", w.Code, w.Body, f.asked.Load())
	}
	f.allow.Store(true)
	w = request(t, h, "POST", "/api/v1/actions/demo.echo", `{}`, codex)
	out := outcome(t, w)
	if res, _ := out.Result.(map[string]any); out.Status != "ok" || res["actor"] != "agent:codex" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	request(t, h, "POST", "/api/v1/actions/demo.echo", `{}`, codex)
	if f.asked.Load() != 2 {
		t.Fatalf("trusted client asked again: %d", f.asked.Load())
	}
}

func TestClientsListAndRevoke(t *testing.T) {
	f := newConfig(t)
	f.cfg.Clients.Store.Trust("codex")
	h := New(f.cfg)
	w := request(t, h, "GET", "/api/v1/clients", "", nil)
	var list []clients.Client
	json.Unmarshal(w.Body.Bytes(), &list)
	if w.Code != 200 || len(list) != 1 || list[0].Name != "codex" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w := request(t, h, "DELETE", "/api/v1/clients/codex", "", map[string]string{"X-Cox-Client": "codex"}); w.Code != 403 {
		t.Fatalf("an agent managed clients: %d %s", w.Code, w.Body)
	}
	if w := request(t, h, "DELETE", "/api/v1/clients/codex", "", nil); w.Code != 204 {
		t.Fatalf("revoke: %d %s", w.Code, w.Body)
	}
	if ok, _ := f.cfg.Clients.Store.IsTrusted("codex"); ok {
		t.Fatal("still trusted")
	}
	if w := request(t, h, "DELETE", "/api/v1/clients/codex", "", nil); w.Code != 404 {
		t.Fatalf("revoke twice: %d %s", w.Code, w.Body)
	}
	if w := request(t, h, "DELETE", "/api/v1/clients/cli", "", nil); w.Code != 400 {
		t.Fatalf("revoke built-in: %d %s", w.Code, w.Body)
	}
}

func TestEventsStream(t *testing.T) {
	f := newConfig(t)
	srv := httptest.NewServer(New(f.cfg))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Cox-Client", "desktop")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("%d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	// The subscription exists once headers arrive.
	f.cfg.Engine.Invoke(context.Background(), action.Actor{Kind: action.Agent, Name: "codex"}, "demo.echo", json.RawMessage(`{"msg":"x"}`))

	lines := make(chan string)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	var types []string
	keepAlive := false
	deadline := time.After(5 * time.Second)
	for len(types) < 2 || !keepAlive {
		select {
		case l := <-lines:
			switch {
			case strings.HasPrefix(l, "data: "):
				var ev eventlog.Event
				if err := json.Unmarshal([]byte(strings.TrimPrefix(l, "data: ")), &ev); err != nil {
					t.Fatal(err)
				}
				types = append(types, ev.Type)
			case strings.HasPrefix(l, ":"):
				keepAlive = true
			}
		case <-deadline:
			t.Fatalf("got %v keepAlive=%v", types, keepAlive)
		}
	}
	if types[0] != eventlog.ActionRequested || types[1] != eventlog.ActionFinished {
		t.Fatalf("types %v", types)
	}
}

func TestRun(t *testing.T) {
	home := t.TempDir()
	f := newConfig(t)
	f.cfg.Token = ""
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan instance.Info, 1)
	f.cfg.Ready = func(i instance.Info) { ready <- i }
	done := make(chan error, 1)
	go func() { done <- Run(ctx, f.cfg, home) }()

	var info instance.Info
	select {
	case info = <-ready:
	case err := <-done:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("not ready")
	}
	onDisk, err := instance.Read(home)
	if err != nil || onDisk != info || len(info.Token) < 43 || !strings.HasPrefix(info.Addr, "127.0.0.1:") || info.PID != os.Getpid() {
		t.Fatalf("%+v %+v %v", onDisk, info, err)
	}
	if err := instance.Probe(context.Background(), info); err != nil {
		t.Fatal(err)
	}

	// A second server refuses to start.
	if err := Run(context.Background(), f.cfg, home); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second instance: %v", err)
	}

	// An open event stream does not hold up shutdown.
	req, _ := http.NewRequest("GET", "http://"+info.Addr+"/api/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+info.Token)
	req.Header.Set("X-Cox-Client", "cli")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("%v %v", resp, err)
	}
	defer resp.Body.Close()
	go io.Copy(io.Discard, resp.Body)
	// Nor does an open MCP session (it holds a hanging GET).
	f.cfg.Clients.Store.Trust("codex")
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "t", Version: "0"}, nil).Connect(context.Background(),
		&sdk.StreamableClientTransport{Endpoint: "http://" + info.Addr + "/mcp",
			HTTPClient: &http.Client{Transport: headers{info.Token, "codex"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown hung")
	}
	if _, err := os.Stat(instance.Path(home)); !os.IsNotExist(err) {
		t.Fatalf("server.json left behind: %v", err)
	}

	// The next start reuses the port and gets a fresh token.
	ctx2, cancel2 := context.WithCancel(context.Background())
	instance.Write(home, info) // as if the previous server had crashed
	go func() { done <- Run(ctx2, f.cfg, home) }()
	var next instance.Info
	select {
	case next = <-ready:
	case err := <-done:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("not ready")
	}
	cancel2()
	<-done
	if next.Addr != info.Addr || next.Token == info.Token {
		t.Fatalf("first %+v next %+v", info, next)
	}
}

func TestRunFixedPortBusy(t *testing.T) {
	busy := httptest.NewServer(http.NotFoundHandler())
	defer busy.Close()
	f := newConfig(t)
	f.cfg.Port = busy.Listener.Addr().(*net.TCPAddr).Port
	if err := Run(context.Background(), f.cfg, t.TempDir()); err == nil {
		t.Fatal("started on a busy port")
	}
}

func runReady(t *testing.T, cfg Config, home string) (instance.Info, func()) {
	t.Helper()
	ready := make(chan instance.Info, 1)
	cfg.Ready = func(i instance.Info) { ready <- i }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, home) }()
	select {
	case info := <-ready:
		return info, func() { cancel(); <-done }
	case err := <-done:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("not ready")
	}
	return instance.Info{}, nil
}

func TestRunLockHeld(t *testing.T) {
	home := t.TempDir()
	unlock, err := filelock.TryLock(filepath.Join(home, "server.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := Run(ctx, newConfig(t).cfg, home); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("%v", err)
	}
}

func TestRunCorruptInstanceFile(t *testing.T) {
	home := t.TempDir()
	os.WriteFile(instance.Path(home), []byte("{garbage"), 0o600)
	info, stop := runReady(t, newConfig(t).cfg, home)
	defer stop()
	if got, err := instance.Read(home); err != nil || got != info {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestAgentAccess(t *testing.T) {
	f := newConfig(t)
	h := New(f.cfg)
	codex := map[string]string{"X-Cox-Client": "codex"}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, "GET", "/api/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Cox-Client", "codex")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("an agent read the activity stream: %d", w.Code)
	}
	if w := request(t, h, "GET", "/api/v1/actions", "", codex); w.Code != 403 || outcome(t, w).Error != clients.NotAuthorized {
		t.Fatalf("unauthorized agent listed actions: %d %s", w.Code, w.Body)
	}
	f.allow.Store(true)
	if w := request(t, h, "GET", "/api/v1/actions", "", codex); w.Code != 200 {
		t.Fatalf("authorized agent: %d %s", w.Code, w.Body)
	}
}

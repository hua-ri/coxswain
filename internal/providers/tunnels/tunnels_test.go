package tunnels

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/engine"
	"github.com/hua-ri/coxswain/internal/eventlog"
	"github.com/hua-ri/coxswain/internal/inventory"
	"github.com/hua-ri/coxswain/internal/sshx"
	"github.com/hua-ri/coxswain/internal/sshx/sshtest"
	"github.com/hua-ri/coxswain/internal/tunnel"
)

var (
	codex   = action.Actor{Kind: action.Agent, Name: "codex", Session: "s1"}
	kimi    = action.Actor{Kind: action.Agent, Name: "kimi"}
	desktop = action.Actor{Kind: action.Human, Name: "desktop"}
)

func setup(t *testing.T) (*engine.Engine, *tunnel.Manager) {
	t.Helper()
	srv := sshtest.Start(t)
	dir := t.TempDir()
	hosts := inventory.NewStore(filepath.Join(dir, "hosts.json"))
	hosts.Merge([]inventory.Host{{Name: "prod", HostName: srv.Host, Port: srv.Port, IdentityFiles: []string{srv.KeyFile}}})
	hosts.Update(func(m map[string]inventory.Host) error {
		h := m["prod"]
		h.Tags, h.Note = map[string]string{"env": "prod"}, "no restarts"
		m["prod"] = h
		return nil
	})
	events := eventlog.Open(filepath.Join(dir, "events"))
	m := &tunnel.Manager{Hosts: hosts, Dialer: &sshx.Dialer{Hosts: hosts, KnownHosts: []string{srv.KnownHosts}}, Events: events}
	t.Cleanup(m.CloseAll)
	reg := action.NewRegistry()
	(&Provider{Hosts: hosts, Tunnels: m}).Register(reg)
	return &engine.Engine{Registry: reg, Events: events}, m
}

func invoke(t *testing.T, e *engine.Engine, who action.Actor, id string, args any) engine.Outcome {
	t.Helper()
	b, _ := json.Marshal(args)
	return e.Invoke(context.Background(), who, id, b)
}

func mustOK(t *testing.T, out engine.Outcome) any {
	t.Helper()
	if out.Status != engine.StatusOK {
		t.Fatalf("%+v", out)
	}
	return out.Result
}

func requested(t *testing.T, e *engine.Engine) eventlog.Event {
	t.Helper()
	evs, _ := e.Events.Read(time.Now())
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Type == eventlog.ActionRequested {
			return evs[i]
		}
	}
	t.Fatal("no request")
	return eventlog.Event{}
}

func echo(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestAnnotations(t *testing.T) {
	e, _ := setup(t)
	for id, want := range map[string]action.Annotations{
		"tunnel.list":  {ReadOnly: true, Idempotent: true},
		"tunnel.open":  {Destructive: true, OpenWorld: true},
		"tunnel.close": {Destructive: true, Idempotent: true},
	} {
		a, ok := e.Registry.Get(id)
		if !ok || a.Annotations != want {
			t.Errorf("%s: %+v", id, a)
		}
	}
}

func TestOpenListClose(t *testing.T) {
	e, m := setup(t)
	port := echo(t)
	res := mustOK(t, invoke(t, e, codex, "tunnel.open", map[string]any{"host": "prod", "type": "local", "remote_port": port})).(OpenOutput)
	if !strings.HasPrefix(res.Listen, "127.0.0.1:") || res.Owner.Name != "codex" || res.Owner.Session != "" ||
		res.HostNote != "no restarts" || res.HostTags["env"] != "prod" || res.IdleMinutes != 30 || !strings.Contains(res.Hint, res.Listen) {
		t.Fatalf("%+v", res)
	}
	// The host may refuse forwards: say how to check the first connection.
	if !strings.Contains(res.Hint, "first connection") || !strings.Contains(res.Hint, "last_error") {
		t.Fatalf("hint %q", res.Hint)
	}
	if a, _ := e.Registry.Get("tunnel.list"); !strings.Contains(a.Description, "failed_connections") ||
		!strings.Contains(a.Description, "last_error_at") {
		t.Fatalf("tunnel.list description %q", a.Description)
	}
	if ev := requested(t, e); ev.Risk != "write" || ev.Targets[0].Tags["env"] != "prod" {
		t.Fatalf("%+v", ev)
	}
	c, err := net.Dial("tcp", res.Listen)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()

	list := mustOK(t, invoke(t, e, kimi, "tunnel.list", nil)).([]tunnel.Tunnel)
	if len(list) != 1 || list[0].ID != res.ID {
		t.Fatalf("%+v", list)
	}
	mustOK(t, invoke(t, e, codex, "tunnel.close", map[string]any{"id": res.ID}))
	if len(m.List()) != 0 {
		t.Fatal("not closed")
	}
	if out := invoke(t, e, codex, "tunnel.close", map[string]any{"id": res.ID}); out.Status != engine.StatusError {
		t.Fatalf("%+v", out)
	}
}

func TestRemoteIsDestructive(t *testing.T) {
	e, _ := setup(t)
	port := echo(t)
	mustOK(t, invoke(t, e, desktop, "tunnel.open", map[string]any{"host": "prod", "type": "remote", "local_port": port}))
	if ev := requested(t, e); ev.Risk != "destructive" {
		t.Fatalf("%+v", ev)
	}
	// Agents may not open remote tunnels.
	if out := invoke(t, e, codex, "tunnel.open", map[string]any{"host": "prod", "type": "remote", "local_port": port}); out.Status != engine.StatusError ||
		!strings.Contains(out.Error, "ask the user") {
		t.Fatalf("%+v", out)
	}
	mustOK(t, invoke(t, e, codex, "tunnel.open", map[string]any{"host": "prod", "type": "socks", "idle_minutes": 5}))
	if ev := requested(t, e); ev.Risk != "write" {
		t.Fatalf("%+v", ev)
	}
	if out := invoke(t, e, codex, "tunnel.open", map[string]any{"host": "prod", "type": "ftp"}); out.Status != engine.StatusError ||
		!strings.Contains(out.Error, "type") {
		t.Fatalf("%+v", out)
	}
}

// TestCloseOwnership: agents may close only their own tunnels; the user
// may close any.
func TestCloseOwnership(t *testing.T) {
	e, m := setup(t)
	port := echo(t)
	mine, err := m.Open(context.Background(), tunnel.Spec{Host: "prod", Type: tunnel.Local, RemotePort: port}, desktop)
	if err != nil {
		t.Fatal(err)
	}
	codexs := mustOK(t, invoke(t, e, codex, "tunnel.open", map[string]any{"host": "prod", "type": "socks"})).(OpenOutput)
	if out := invoke(t, e, codex, "tunnel.close", map[string]any{"id": mine.ID}); out.Status != engine.StatusError ||
		!strings.Contains(out.Error, "opened by human:desktop") {
		t.Fatalf("agent closed the user's tunnel: %+v", out)
	}
	if out := invoke(t, e, kimi, "tunnel.close", map[string]any{"id": codexs.ID}); out.Status != engine.StatusError {
		t.Fatalf("kimi closed codex's tunnel: %+v", out)
	}
	other := codex
	other.Session = "s2" // another session of the same client
	mustOK(t, invoke(t, e, other, "tunnel.close", map[string]any{"id": codexs.ID}))
	mustOK(t, invoke(t, e, desktop, "tunnel.close", map[string]any{"id": mine.ID}))
}

// podDialer connects k8s tunnels to an echo "pod".
type podDialer struct{}

func (podDialer) Connect(_ context.Context, spec tunnel.Spec, _ func(error)) (tunnel.PodConn, error) {
	return &podConn{pod: spec.Pod, done: make(chan struct{})}, nil
}

func (podDialer) Target(ctx, ns string) action.Target {
	return action.Target{Kind: "k8s", ID: "k8s:" + ctx + "/" + ns, Tags: map[string]string{"env": "prod"}}
}

type podConn struct {
	pod  string
	done chan struct{}
}

func (c *podConn) Pod() string { return c.pod }
func (c *podConn) Port() int   { return 80 }
func (c *podConn) Dial() (net.Conn, error) {
	a, b := net.Pipe()
	go func() { io.Copy(b, b); b.Close() }()
	return a, nil
}
func (c *podConn) Wait() error  { <-c.done; return nil }
func (c *podConn) Close() error { close(c.done); return nil }

// TestCloseK8sTunnel: a k8s tunnel is listed and closed like the others;
// its audit target is the namespace with the context's tags.
func TestCloseK8sTunnel(t *testing.T) {
	e, m := setup(t)
	m.Pods = podDialer{}
	tn, err := m.Open(context.Background(), tunnel.Spec{Type: tunnel.K8s, Context: "prod", Namespace: "shop", Pod: "web-1", RemotePort: 80},
		action.Actor{Kind: action.Agent, Name: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	list := mustOK(t, invoke(t, e, kimi, "tunnel.list", map[string]any{})).([]tunnel.Tunnel)
	if len(list) != 1 || list[0].Pod != "web-1" || list[0].Context != "prod" {
		t.Fatalf("%+v", list)
	}
	if out := invoke(t, e, kimi, "tunnel.close", map[string]any{"id": tn.ID}); out.Status != engine.StatusError {
		t.Fatalf("kimi closed codex's tunnel: %+v", out)
	}
	mustOK(t, invoke(t, e, codex, "tunnel.close", map[string]any{"id": tn.ID}))
	evs, _ := e.Events.Read(time.Now())
	var found bool
	for _, ev := range evs {
		if ev.Type == eventlog.ActionRequested && ev.Action == "tunnel.close" && ev.Actor.Name == "codex" {
			found = len(ev.Targets) == 1 && ev.Targets[0].ID == "k8s:prod/shop" && ev.Targets[0].Tags["env"] == "prod" &&
				strings.Contains(string(ev.Args), tn.ID)
		}
	}
	if !found {
		t.Fatalf("audit: %+v", evs)
	}
}

package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/eventlog"
)

// fakePods is a PodDialer whose pods echo what they get. A service
// resolves to <service>-<n>, n counting connections, so a reconnect lands
// on another pod.
type fakePods struct {
	mu       sync.Mutex
	specs    []Spec
	conns    []*fakePodConn
	refuse   bool  // the pod refuses connections (reported via failed)
	dialErr  error // Dial fails
	connErr  error // Connect fails
	resolved int
}

func (f *fakePods) Connect(_ context.Context, spec Spec, failed func(error)) (PodConn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.specs = append(f.specs, spec)
	if f.connErr != nil {
		return nil, f.connErr
	}
	pod := spec.Pod
	if spec.Service != "" {
		f.resolved++
		pod = fmt.Sprintf("%s-%d", spec.Service, f.resolved)
	}
	c := &fakePodConn{f: f, pod: pod, port: spec.RemotePort + 1, failed: failed, done: make(chan struct{})}
	f.conns = append(f.conns, c)
	return c, nil
}

func (f *fakePods) Target(ctx, ns string) action.Target {
	return action.Target{Kind: "k8s", ID: "k8s:" + ctx + "/" + ns, Tags: map[string]string{"env": "prod"}}
}

// drop ends every open session, like a broken connection.
func (f *fakePods) drop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.conns {
		c.close()
	}
}

type fakePodConn struct {
	f      *fakePods
	pod    string
	port   int
	failed func(error)
	once   sync.Once
	done   chan struct{}
}

func (c *fakePodConn) Pod() string { return c.pod }
func (c *fakePodConn) Port() int   { return c.port }

func (c *fakePodConn) Dial() (net.Conn, error) {
	c.f.mu.Lock()
	refuse, dialErr := c.f.refuse, c.f.dialErr
	c.f.mu.Unlock()
	if dialErr != nil {
		return nil, dialErr
	}
	a, b := net.Pipe()
	if refuse {
		// The pod reports the failure on its error stream, after the
		// stream was opened.
		go func() {
			c.failed(fmt.Errorf("error forwarding port %d to pod %s: connection refused", c.port, c.pod))
			b.Close()
		}()
		return a, nil
	}
	go func() { io.Copy(b, b); b.Close() }()
	return a, nil
}

func (c *fakePodConn) Wait() error {
	<-c.done
	return errors.New("lost connection to pod")
}

func (c *fakePodConn) close() { c.once.Do(func() { close(c.done) }) }

func (c *fakePodConn) Close() error { c.close(); return nil }

func setupK8s(t *testing.T) (fixture, *fakePods) {
	t.Helper()
	dir := t.TempDir()
	events := eventlog.Open(filepath.Join(dir, "events"))
	ch, cancel := events.Subscribe()
	t.Cleanup(cancel)
	ft := &fakeTimers{}
	pods := &fakePods{}
	m := &Manager{Pods: pods, Events: events, Backoff: func(int) time.Duration { return time.Millisecond }, after: ft.after}
	t.Cleanup(m.CloseAll)
	return fixture{m: m, events: ch, timers: ft}, pods
}

func TestK8sPod(t *testing.T) {
	f, pods := setupK8s(t)
	tn, err := f.m.Open(context.Background(), Spec{Type: K8s, Context: "dev", Namespace: "web", Pod: "api-1", RemotePort: 8080}, codex)
	if err != nil {
		t.Fatal(err)
	}
	if tn.Type != K8s || tn.Context != "dev" || tn.Namespace != "web" || tn.Pod != "api-1" || tn.RemotePort != 8080 ||
		tn.Target != "api-1:8081" || !strings.HasPrefix(tn.Listen, "127.0.0.1:") || tn.IdleMinutes != DefaultAgentIdle || tn.Host != "" {
		t.Fatalf("%+v", tn)
	}
	ev := next(t, f.events, eventlog.TunnelOpened)
	if len(ev.Targets) != 1 || ev.Targets[0].ID != "k8s:dev/web" || ev.Targets[0].Tags["env"] != "prod" {
		t.Fatalf("targets %+v", ev.Targets)
	}
	roundTrip(t, tn.Listen)
	roundTrip(t, tn.Listen)
	if got := waitFor(t, f.m, tn.ID, func(tn Tunnel) bool { return tn.Connections == 0 }); got.TotalConnections != 2 || got.BytesIn != 10 {
		t.Fatalf("%+v", got)
	}
	if len(pods.specs) != 1 {
		t.Fatalf("one session for all connections: %d", len(pods.specs))
	}
	// Revoking the agent closes it, like an SSH tunnel.
	f.m.CloseAgent("codex", "client revoked")
	if ev := next(t, f.events, eventlog.TunnelClosed); ev.Reason != "client revoked" {
		t.Fatalf("%+v", ev)
	}
}

func TestK8sServiceReresolvedOnReconnect(t *testing.T) {
	f, pods := setupK8s(t)
	tn, err := f.m.Open(context.Background(), Spec{Type: K8s, Context: "dev", Namespace: "web", Service: "api", RemotePort: 80}, desktop)
	if err != nil {
		t.Fatal(err)
	}
	if tn.Service != "api" || tn.Pod != "api-1" || tn.IdleMinutes != 0 {
		t.Fatalf("%+v", tn)
	}
	roundTrip(t, tn.Listen)
	pods.drop()
	next(t, f.events, eventlog.TunnelReconnecting)
	got := detail(t, next(t, f.events, eventlog.TunnelReconnected))
	if got.Pod != "api-2" || got.Target != "api-2:81" || got.Listen != tn.Listen || got.Status != Open {
		t.Fatalf("after reconnect: %+v", got)
	}
	roundTrip(t, tn.Listen)
}

func TestK8sConnectionFailed(t *testing.T) {
	f, pods := setupK8s(t)
	clock := &fakeClock{now: time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)}
	f.m.now = clock.Now
	tn, err := f.m.Open(context.Background(), Spec{Type: K8s, Context: "dev", Namespace: "web", Pod: "api-1", RemotePort: 8080}, codex)
	if err != nil {
		t.Fatal(err)
	}
	pods.mu.Lock()
	pods.refuse = true
	pods.mu.Unlock()
	refused(t, tn.Listen)
	ev := next(t, f.events, eventlog.TunnelConnectionFailed)
	if ev.Tunnel != tn.ID || !strings.Contains(ev.Error, "connection refused") {
		t.Fatalf("%+v", ev)
	}
	if d := detail(t, ev); d.FailedConnections != 1 || d.LastError != ev.Error {
		t.Fatalf("%+v", d)
	}
	pods.mu.Lock()
	pods.refuse, pods.dialErr = false, errors.New("stream refused")
	pods.mu.Unlock()
	refused(t, tn.Listen)
	waitFor(t, f.m, tn.ID, func(tn Tunnel) bool {
		return tn.FailedConnections == 2 && strings.Contains(tn.LastError, "stream refused")
	})
}

func TestK8sInvalid(t *testing.T) {
	f, pods := setupK8s(t)
	for _, s := range []Spec{
		{Type: K8s, Context: "dev", Namespace: "web", Pod: "a"},                                  // no port
		{Type: K8s, Context: "dev", Namespace: "web", Pod: "a", Service: "b", RemotePort: 80},    // both
		{Type: K8s, Context: "dev", Namespace: "web", RemotePort: 80},                            // neither
		{Type: K8s, Namespace: "web", Pod: "a", RemotePort: 80},                                  // no context
		{Type: K8s, Context: "dev", Pod: "a", RemotePort: 80},                                    // no namespace
		{Type: K8s, Host: "dev", Context: "dev", Namespace: "web", Pod: "a", RemotePort: 80},     // host
		{Type: Local, Host: "dev", Context: "dev", Namespace: "web", Pod: "a", RemotePort: 80},   // k8s fields on ssh
		{Type: K8s, Context: "dev", Namespace: "web", Pod: "a", RemotePort: 80, RemoteHost: "x"}, // remote_host
	} {
		if _, err := f.m.Open(context.Background(), s, desktop); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v", s, err)
		}
	}
	pods.connErr = errors.New(`pods "a" not found`)
	if _, err := f.m.Open(context.Background(), Spec{Type: K8s, Context: "dev", Namespace: "web", Pod: "a", RemotePort: 80}, desktop); err == nil ||
		!strings.Contains(err.Error(), "not found") {
		t.Fatalf("%v", err)
	}
	if len(f.m.List()) != 0 {
		t.Fatal("a failed tunnel is listed")
	}
	none := &Manager{}
	if _, err := none.Open(context.Background(), Spec{Type: K8s, Context: "dev", Namespace: "web", Pod: "a", RemotePort: 80}, desktop); err == nil {
		t.Fatal("k8s tunnel without a PodDialer")
	}
}

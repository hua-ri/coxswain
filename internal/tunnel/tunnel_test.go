package tunnel

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/eventlog"
	"github.com/hua-ri/coxswain/internal/inventory"
	"github.com/hua-ri/coxswain/internal/sshx"
	"github.com/hua-ri/coxswain/internal/sshx/sshtest"
)

var (
	codex   = action.Actor{Kind: action.Agent, Name: "codex"}
	desktop = action.Actor{Kind: action.Human, Name: "desktop"}
)

type fixture struct {
	m      *Manager
	srv    *sshtest.Server
	events <-chan eventlog.Event
	timers *fakeTimers
}

func setup(t *testing.T) fixture {
	t.Helper()
	srv := sshtest.Start(t)
	dir := t.TempDir()
	hosts := inventory.NewStore(filepath.Join(dir, "hosts.json"))
	hosts.Merge([]inventory.Host{{Name: "dev", HostName: srv.Host, Port: srv.Port, IdentityFiles: []string{srv.KeyFile}}})
	hosts.Update(func(m map[string]inventory.Host) error {
		h := m["dev"]
		h.Tags = map[string]string{"env": "prod"}
		m["dev"] = h
		return nil
	})
	events := eventlog.Open(filepath.Join(dir, "events"))
	ch, cancel := events.Subscribe()
	t.Cleanup(cancel)
	ft := &fakeTimers{}
	m := &Manager{
		Hosts:   hosts,
		Dialer:  &sshx.Dialer{Hosts: hosts, KnownHosts: []string{srv.KnownHosts}},
		Events:  events,
		Backoff: func(int) time.Duration { return time.Millisecond },
		after:   ft.after,
	}
	t.Cleanup(m.CloseAll)
	return fixture{m: m, srv: srv, events: ch, timers: ft}
}

// fakeTimers records idle timers so tests fire them by hand.
type fakeTimers struct {
	mu     sync.Mutex
	timers []*fakeTimer
	added  chan struct{}
}

type fakeTimer struct {
	d       time.Duration
	f       func()
	stopped bool
}

func (ft *fakeTimers) after(d time.Duration, f func()) func() bool {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	tm := &fakeTimer{d: d, f: f}
	ft.timers = append(ft.timers, tm)
	return func() bool {
		ft.mu.Lock()
		defer ft.mu.Unlock()
		was := !tm.stopped
		tm.stopped = true
		return was
	}
}

// armed returns the timers that are not stopped.
func (ft *fakeTimers) armed() []*fakeTimer {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	var out []*fakeTimer
	for _, tm := range ft.timers {
		if !tm.stopped {
			out = append(out, tm)
		}
	}
	return out
}

// fire runs an armed timer, as if its time had come.
func (ft *fakeTimers) fire(tm *fakeTimer) {
	ft.mu.Lock()
	tm.stopped = true
	ft.mu.Unlock()
	tm.f()
}

// echo runs a TCP echo server on 127.0.0.1 and returns its port.
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

// roundTrip sends a line through addr and expects it back.
func roundTrip(t *testing.T, addr string) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ping(t, c)
}

func ping(t *testing.T, c net.Conn) {
	t.Helper()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(c, "ping\n"); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil || line != "ping\n" {
		t.Fatalf("%q %v", line, err)
	}
}

func next(t *testing.T, ch <-chan eventlog.Event, typ string) eventlog.Event {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Type == typ {
				return ev
			}
		case <-timeout:
			t.Fatalf("no %s event", typ)
		}
	}
}

func detail(t *testing.T, ev eventlog.Event) Tunnel {
	t.Helper()
	var tn Tunnel
	if err := json.Unmarshal(ev.Detail, &tn); err != nil {
		t.Fatal(err)
	}
	return tn
}

// waitFor polls the tunnel until cond holds.
func waitFor(t *testing.T, m *Manager, id string, cond func(Tunnel) bool) Tunnel {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		tn, ok := m.Get(id)
		if ok && cond(tn) {
			return tn
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not met: %+v", tn)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestLocal(t *testing.T) {
	f := setup(t)
	port := echo(t)
	tn, err := f.m.Open(context.Background(), Spec{Host: "dev", Type: Local, RemotePort: port}, codex)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tn.ID, "f_") || !strings.HasPrefix(tn.Listen, "127.0.0.1:") || tn.Target != "127.0.0.1:"+strconv.Itoa(port) ||
		tn.Status != Open || tn.Owner != codex || tn.IdleMinutes != 30 {
		t.Fatalf("%+v", tn)
	}
	ev := next(t, f.events, eventlog.TunnelOpened)
	if ev.Tunnel != tn.ID || ev.Actor.Name != "codex" || ev.Targets[0].Tags["env"] != "prod" || detail(t, ev).Listen != tn.Listen {
		t.Fatalf("%+v", ev)
	}
	roundTrip(t, tn.Listen)
	got := waitFor(t, f.m, tn.ID, func(tn Tunnel) bool { return tn.TotalConnections == 1 && tn.Connections == 0 && tn.BytesIn == 5 })
	if got.BytesOut != 5 {
		t.Fatalf("%+v", got)
	}
	if list := f.m.List(); len(list) != 1 || list[0].ID != tn.ID {
		t.Fatalf("%+v", list)
	}

	if err := f.m.Close(tn.ID, desktop); err != nil {
		t.Fatal(err)
	}
	if ev := next(t, f.events, eventlog.TunnelClosed); ev.Reason != "closed by human:desktop" || ev.Tunnel != tn.ID {
		t.Fatalf("%+v", ev)
	}
	if _, err := net.Dial("tcp", tn.Listen); err == nil {
		t.Fatal("tunnel still listening")
	}
	if err := f.m.Close(tn.ID, desktop); !errors.Is(err, ErrNotFound) {
		t.Fatalf("close twice: %v", err)
	}
	if len(f.m.List()) != 0 {
		t.Fatal("closed tunnel listed")
	}
}

func TestLocalFixedPortAndRemoteHost(t *testing.T) {
	f := setup(t)
	port := echo(t)
	// Ask for a port that was free a moment ago; if something else took
	// it meanwhile, try another one.
	var tn Tunnel
	var want int
	for range 20 {
		free, _ := net.Listen("tcp", "127.0.0.1:0")
		want = free.Addr().(*net.TCPAddr).Port
		free.Close()
		var err error
		tn, err = f.m.Open(context.Background(), Spec{Host: "dev", Type: Local, LocalPort: want, RemoteHost: "localhost", RemotePort: port}, desktop)
		if err == nil {
			break
		}
		if !errors.Is(err, ErrListen) {
			t.Fatal(err)
		}
	}
	if tn.Listen != "127.0.0.1:"+strconv.Itoa(want) || tn.Target != "localhost:"+strconv.Itoa(port) || tn.IdleMinutes != 0 {
		t.Fatalf("%+v", tn)
	}
	roundTrip(t, tn.Listen)
	// The port is taken now.
	if _, err := f.m.Open(context.Background(), Spec{Host: "dev", Type: Local, LocalPort: want, RemotePort: port}, desktop); !errors.Is(err, ErrListen) {
		t.Fatalf("port in use: %v", err)
	}
}

// TestServerPortRefused: no tunnel may lead to Coxswain's own port, and
// agents may not open remote tunnels at all.
func TestServerPortRefused(t *testing.T) {
	f := setup(t)
	server := echo(t) // stands in for cox server
	f.m.SetServerPort(server)
	for _, s := range []Spec{
		{Host: "dev", Type: Remote, LocalPort: server},
		{Host: "dev", Type: Local, RemotePort: server},
		{Host: "dev", Type: Local, RemoteHost: "localhost", RemotePort: server},
	} {
		if _, err := f.m.Open(context.Background(), s, desktop); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("%+v: %v", s, err)
		}
	}
	other := echo(t)
	_, err := f.m.Open(context.Background(), Spec{Host: "dev", Type: Remote, LocalPort: other}, codex)
	if !errors.Is(err, ErrNotAllowed) || !strings.Contains(err.Error(), "ask the user") {
		t.Fatalf("agent remote tunnel: %v", err)
	}
	if len(f.m.List()) != 0 {
		t.Fatal("refused tunnels listed")
	}
}

// TestAgentLimits: agents get at most 16 tunnels each and must let them
// close when idle (1..240 minutes).
func TestAgentLimits(t *testing.T) {
	f := setup(t)
	port := echo(t)
	spec := Spec{Host: "dev", Type: Local, RemotePort: port}
	for _, idle := range []int{0, 241} {
		s := spec
		s.IdleMinutes = &idle
		if _, err := f.m.Open(context.Background(), s, codex); !errors.Is(err, ErrInvalid) {
			t.Errorf("idle %d: %v", idle, err)
		}
	}
	for i := range maxPerAgent {
		if _, err := f.m.Open(context.Background(), spec, codex); err != nil {
			t.Fatalf("tunnel %d: %v", i, err)
		}
	}
	if _, err := f.m.Open(context.Background(), spec, codex); !errors.Is(err, ErrTooMany) {
		t.Fatalf("17th: %v", err)
	}
	if _, err := f.m.Open(context.Background(), spec, action.Actor{Kind: action.Agent, Name: "kimi"}); err != nil {
		t.Fatalf("another agent: %v", err)
	}
	if _, err := f.m.Open(context.Background(), spec, desktop); err != nil {
		t.Fatalf("the user: %v", err)
	}
}

// TestOpenCancelled: a tunnel whose caller is cancelled while it opens
// (the agent was revoked) does not stay open.
func TestOpenCancelled(t *testing.T) {
	f := setup(t)
	port := echo(t)
	ctx, cancel := context.WithCancelCause(context.Background())
	f.m.afterInsert = func() { cancel(errors.New("client revoked")) }
	if _, err := f.m.Open(ctx, Spec{Host: "dev", Type: Local, RemotePort: port}, codex); err == nil || !strings.Contains(err.Error(), "client revoked") {
		t.Fatalf("%v", err)
	}
	if len(f.m.List()) != 0 {
		t.Fatal("tunnel left open")
	}
}

func TestRemote(t *testing.T) {
	f := setup(t)
	port := echo(t)
	tn, err := f.m.Open(context.Background(), Spec{Host: "dev", Type: Remote, LocalPort: port}, desktop)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tn.Listen, "127.0.0.1:") || tn.Listen == "127.0.0.1:0" || tn.Target != "127.0.0.1:"+strconv.Itoa(port) {
		t.Fatalf("%+v", tn)
	}
	// The test SSH server runs here, so its 127.0.0.1 is ours.
	roundTrip(t, tn.Listen)
	waitFor(t, f.m, tn.ID, func(tn Tunnel) bool { return tn.TotalConnections == 1 && tn.BytesIn == 5 && tn.BytesOut == 5 })
	if err := f.m.Close(tn.ID, desktop); err != nil {
		t.Fatal(err)
	}
	if _, err := net.Dial("tcp", tn.Listen); err == nil {
		t.Fatal("remote forward still listening")
	}
}

// socks connects through a SOCKS5 proxy with the given address type
// and address, and returns the connection and the reply code.
func socks(t *testing.T, proxy string, cmd, atyp byte, addr []byte, port int) (net.Conn, byte) {
	t.Helper()
	c, err := net.Dial("tcp", proxy)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(5 * time.Second))
	c.Write([]byte{5, 1, 0})
	var hello [2]byte
	if _, err := io.ReadFull(c, hello[:]); err != nil || hello != [2]byte{5, 0} {
		t.Fatalf("greeting: %v %v", hello, err)
	}
	req := append([]byte{5, cmd, 0, atyp}, addr...)
	req = binary.BigEndian.AppendUint16(req, uint16(port))
	c.Write(req)
	var reply [10]byte
	if _, err := io.ReadFull(c, reply[:]); err != nil {
		t.Fatalf("reply: %v", err)
	}
	return c, reply[1]
}

func TestSocks(t *testing.T) {
	f := setup(t)
	port := echo(t)
	tn, err := f.m.Open(context.Background(), Spec{Host: "dev", Type: Socks}, codex)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tn.Listen, "127.0.0.1:") || tn.Target != "" {
		t.Fatalf("%+v", tn)
	}
	c, code := socks(t, tn.Listen, 1, 1, []byte{127, 0, 0, 1}, port)
	if code != 0 {
		t.Fatalf("ipv4 reply %d", code)
	}
	ping(t, c)
	c, code = socks(t, tn.Listen, 1, 3, append([]byte{9}, "localhost"...), port)
	if code != 0 {
		t.Fatalf("domain reply %d", code)
	}
	ping(t, c)
	if ln6, err := net.Listen("tcp", "[::1]:0"); err == nil {
		go func() {
			c, err := ln6.Accept()
			if err == nil {
				io.Copy(c, c)
				c.Close()
			}
		}()
		defer ln6.Close()
		c, code = socks(t, tn.Listen, 1, 4, net.ParseIP("::1").To16(), ln6.Addr().(*net.TCPAddr).Port)
		if code != 0 {
			t.Fatalf("ipv6 reply %d", code)
		}
		ping(t, c)
	}
	if _, code := socks(t, tn.Listen, 2, 1, []byte{127, 0, 0, 1}, port); code != 7 {
		t.Fatalf("BIND: reply %d, want 7 (command not supported)", code)
	}
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	closedPort := closed.Addr().(*net.TCPAddr).Port
	closed.Close()
	if _, code := socks(t, tn.Listen, 1, 1, []byte{127, 0, 0, 1}, closedPort); code == 0 {
		t.Fatal("connect to a closed port succeeded")
	}

	// Only "no authentication" is offered.
	c2, _ := net.Dial("tcp", tn.Listen)
	defer c2.Close()
	c2.SetDeadline(time.Now().Add(5 * time.Second))
	c2.Write([]byte{5, 1, 2})
	var hello [2]byte
	if _, err := io.ReadFull(c2, hello[:]); err != nil || hello != [2]byte{5, 0xff} {
		t.Fatalf("auth: %v %v", hello, err)
	}
}

func TestReconnect(t *testing.T) {
	for _, typ := range []string{Local, Remote} {
		t.Run(typ, func(t *testing.T) {
			f := setup(t)
			port := echo(t)
			spec := Spec{Host: "dev", Type: Local, RemotePort: port}
			if typ == Remote {
				spec = Spec{Host: "dev", Type: Remote, LocalPort: port}
			}
			tn, err := f.m.Open(context.Background(), spec, desktop)
			if err != nil {
				t.Fatal(err)
			}
			roundTrip(t, tn.Listen)
			f.srv.DropAll()
			ev := next(t, f.events, eventlog.TunnelReconnecting)
			if ev.Tunnel != tn.ID {
				t.Fatalf("%+v", ev)
			}
			ev = next(t, f.events, eventlog.TunnelReconnected)
			if got := detail(t, ev); got.Listen != tn.Listen || got.Status != Open {
				t.Fatalf("after reconnect: %+v (was %s)", got, tn.Listen)
			}
			roundTrip(t, tn.Listen)
		})
	}
}

func TestBackoff(t *testing.T) {
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, time.Minute, time.Minute}
	for i, w := range want {
		if got := defaultBackoff(i); got != w {
			t.Errorf("attempt %d: %v, want %v", i, got, w)
		}
	}
}

func TestIdleClose(t *testing.T) {
	f := setup(t)
	port := echo(t)
	idle := 5
	tn, err := f.m.Open(context.Background(), Spec{Host: "dev", Type: Local, RemotePort: port, IdleMinutes: &idle}, codex)
	if err != nil {
		t.Fatal(err)
	}
	armed := f.timers.armed()
	if len(armed) != 1 || armed[0].d != 5*time.Minute {
		t.Fatalf("idle timer: %+v", armed)
	}
	// A connection stops the timer; when it ends, the timer starts again.
	c, err := net.Dial("tcp", tn.Listen)
	if err != nil {
		t.Fatal(err)
	}
	ping(t, c)
	waitFor(t, f.m, tn.ID, func(tn Tunnel) bool { return tn.Connections == 1 })
	if len(f.timers.armed()) != 0 {
		t.Fatal("idle timer runs during a connection")
	}
	f.timers.fire(armed[0]) // fired just before it was stopped: no effect
	if _, ok := f.m.Get(tn.ID); !ok {
		t.Fatal("closed while in use")
	}
	c.Close()
	waitFor(t, f.m, tn.ID, func(tn Tunnel) bool { return tn.Connections == 0 })
	armed = f.timers.armed()
	if len(armed) != 1 {
		t.Fatalf("idle timer not restarted: %+v", armed)
	}
	f.timers.fire(armed[0])
	if ev := next(t, f.events, eventlog.TunnelClosed); ev.Reason != "idle for 5 minutes" {
		t.Fatalf("%+v", ev)
	}
	if _, ok := f.m.Get(tn.ID); ok {
		t.Fatal("still open")
	}
}

// TestNoIdleClose: the user's tunnels never close by themselves, by
// default or with idle_minutes 0.
func TestNoIdleClose(t *testing.T) {
	f := setup(t)
	port := echo(t)
	zero := 0
	if _, err := f.m.Open(context.Background(), Spec{Host: "dev", Type: Local, RemotePort: port}, desktop); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Open(context.Background(), Spec{Host: "dev", Type: Local, RemotePort: port, IdleMinutes: &zero}, desktop); err != nil {
		t.Fatal(err)
	}
	if n := len(f.timers.armed()); n != 0 {
		t.Fatalf("%d idle timers", n)
	}
}

func TestInvalid(t *testing.T) {
	f := setup(t)
	neg := -1
	for _, s := range []Spec{
		{Host: "dev", Type: "dynamic"},
		{Host: "dev", Type: Local},
		{Host: "dev", Type: Local, RemotePort: 70000},
		{Host: "dev", Type: Local, RemotePort: 80, LocalPort: -1},
		{Host: "dev", Type: Remote},
		{Host: "dev", Type: Remote, LocalPort: 80, RemoteHost: "10.0.0.1"},
		{Host: "dev", Type: Socks, RemotePort: 80},
		{Host: "dev", Type: Local, RemotePort: 80, IdleMinutes: &neg},
	} {
		if _, err := f.m.Open(context.Background(), s, desktop); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v", s, err)
		}
	}
	if _, err := f.m.Open(context.Background(), Spec{Host: "nope", Type: Socks}, codex); !errors.Is(err, inventory.ErrNotFound) {
		t.Errorf("unknown host: %v", err)
	}
}

func TestCloseAll(t *testing.T) {
	f := setup(t)
	port := echo(t)
	tn, err := f.m.Open(context.Background(), Spec{Host: "dev", Type: Local, RemotePort: port}, codex)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := net.Dial("tcp", tn.Listen)
	ping(t, c)
	f.m.CloseAll()
	if ev := next(t, f.events, eventlog.TunnelClosed); ev.Reason != "server stopped" {
		t.Fatalf("%+v", ev)
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection survived")
	}
	if len(f.m.List()) != 0 {
		t.Fatal("tunnels left")
	}
}

func TestCloseAgent(t *testing.T) {
	f := setup(t)
	port := echo(t)
	spec := Spec{Host: "dev", Type: Local, RemotePort: port}
	mine, _ := f.m.Open(context.Background(), spec, desktop)
	codexs, _ := f.m.Open(context.Background(), spec, codex)
	kimis, _ := f.m.Open(context.Background(), spec, action.Actor{Kind: action.Agent, Name: "kimi"})
	f.m.CloseAgent("codex", "client revoked")
	if ev := next(t, f.events, eventlog.TunnelClosed); ev.Tunnel != codexs.ID || ev.Reason != "client revoked" {
		t.Fatalf("%+v", ev)
	}
	list := f.m.List()
	if len(list) != 2 || list[0].ID != mine.ID || list[1].ID != kimis.ID {
		t.Fatalf("%+v", list)
	}
}

// rawSocks sends b to a SOCKS proxy and returns what it answers before
// closing the connection.
func rawSocks(t *testing.T, proxy string, b []byte, closeWrite bool) []byte {
	t.Helper()
	c, err := net.Dial("tcp", proxy)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	c.Write(b)
	if closeWrite {
		c.(*net.TCPConn).CloseWrite()
	}
	// The proxy closes the connection; unread input may make that a
	// reset rather than an end of file.
	out, err := io.ReadAll(c)
	if err != nil && !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("proxy did not close the connection: %v", err)
	}
	return out
}

func TestSocksMalformed(t *testing.T) {
	f := setup(t)
	port := echo(t)
	tn, err := f.m.Open(context.Background(), Spec{Host: "dev", Type: Socks}, desktop)
	if err != nil {
		t.Fatal(err)
	}
	p := binary.BigEndian.AppendUint16(nil, uint16(port))
	for _, c := range []struct {
		name string
		in   []byte
		want []byte
	}{
		{"no methods", []byte{5, 0}, []byte{5, 0xff}},
		{"socks4", []byte{4, 1, 0, 80, 127, 0, 0, 1, 0}, nil},
		{"empty domain", append([]byte{5, 1, 0, 5, 1, 0, 3, 0}, p...), []byte{5, 0, 5, socksBadAddress, 0, 1, 0, 0, 0, 0, 0, 0}},
		{"bad address type", append([]byte{5, 1, 0, 5, 1, 0, 9}, p...), []byte{5, 0, 5, socksBadAddress, 0, 1, 0, 0, 0, 0, 0, 0}},
		{"eof in greeting", []byte{5, 3, 0}, nil},
		{"eof in request", []byte{5, 1, 0, 5, 1, 0, 1, 127}, []byte{5, 0}},
	} {
		if got := rawSocks(t, tn.Listen, c.in, true); string(got) != string(c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
	// The proxy still works.
	conn, code := socks(t, tn.Listen, 1, 1, []byte{127, 0, 0, 1}, port)
	if code != 0 {
		t.Fatalf("reply %d", code)
	}
	ping(t, conn)
	waitFor(t, f.m, tn.ID, func(tn Tunnel) bool { return tn.Connections <= 1 })
}

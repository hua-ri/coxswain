// Package tunnel keeps the port forwards of `cox server`: local (ssh -L),
// remote (ssh -R) and SOCKS5 (ssh -D) tunnels with an owner, traffic
// counters and automatic reconnection. Every tunnel binds 127.0.0.1 only.
package tunnel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/eventlog"
	"github.com/hua-ri/coxswain/internal/inventory"
	"github.com/hua-ri/coxswain/internal/sshx"
)

var (
	ErrNotFound = errors.New("tunnel not found")
	ErrInvalid  = errors.New("invalid tunnel")
	ErrListen   = errors.New("cannot listen")
	ErrTooMany  = errors.New("too many open tunnels; close some first")
)

// Types.
const (
	Local  = "local"  // 127.0.0.1:port here → remote_host:remote_port as seen from the host
	Remote = "remote" // 127.0.0.1:port on the host → 127.0.0.1:local_port here
	Socks  = "socks"  // SOCKS5 proxy on 127.0.0.1:port here; connections leave from the host
)

// Statuses.
const (
	Open         = "open"
	Reconnecting = "reconnecting"
)

// DefaultAgentIdle is how long an agent's tunnel may go unused before it
// closes, in minutes.
const DefaultAgentIdle = 30

// Spec is what to open.
type Spec struct {
	Host       string `json:"host"`
	Type       string `json:"type"`
	LocalPort  int    `json:"local_port,omitempty"`
	RemoteHost string `json:"remote_host,omitempty"`
	RemotePort int    `json:"remote_port,omitempty"`
	// IdleMinutes closes the tunnel after that many minutes without
	// connections; 0 never. Default: 30 for agents, 0 for people.
	IdleMinutes *int `json:"idle_minutes,omitempty"`
}

// Tunnel describes one tunnel.
type Tunnel struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Host string `json:"host"`
	// Listen is where connections enter: 127.0.0.1:port on this machine
	// (local, socks) or on the host (remote).
	Listen string `json:"listen"`
	// Target is where they go: host:port as seen from the host (local),
	// or on this machine (remote). Empty for socks.
	Target           string       `json:"target,omitempty"`
	Owner            action.Actor `json:"owner"`
	CreatedAt        time.Time    `json:"created_at"`
	Status           string       `json:"status"`
	Connections      int          `json:"connections"` // open now
	TotalConnections int64        `json:"total_connections"`
	BytesOut         int64        `json:"bytes_out"` // toward the target
	BytesIn          int64        `json:"bytes_in"`  // back from the target
	IdleMinutes      int          `json:"idle_minutes"`
	Error            string       `json:"error,omitempty"` // why it is reconnecting
}

// Manager holds the tunnels. Each has its own SSH connection (not the
// pool's) and reconnects when it breaks, keeping its ports.
type Manager struct {
	Hosts  *inventory.Store
	Dialer *sshx.Dialer
	Events *eventlog.Log // tunnel.* events; may be nil
	// Backoff is the wait before reconnect attempt n (from 0); default
	// 1s doubling up to 1 minute.
	Backoff func(attempt int) time.Duration
	// MaxOpen bounds the open tunnels; default 64.
	MaxOpen int

	// after starts a timer (time.AfterFunc); tests replace it.
	after func(time.Duration, func()) (stop func() bool)

	mu      sync.Mutex
	tunnels map[string]*tunnel
}

func defaultBackoff(attempt int) time.Duration {
	if attempt >= 6 {
		return time.Minute
	}
	return time.Second << attempt
}

// Open validates spec, connects to the host and starts the tunnel.
func (m *Manager) Open(ctx context.Context, spec Spec, owner action.Actor) (Tunnel, error) {
	idle, target, err := check(&spec, owner)
	if err != nil {
		return Tunnel{}, err
	}
	h, err := m.Hosts.Get(spec.Host)
	if err != nil {
		return Tunnel{}, err
	}
	m.mu.Lock()
	n := len(m.tunnels)
	m.mu.Unlock()
	if n >= m.maxOpen() {
		return Tunnel{}, ErrTooMany
	}
	id, err := newID()
	if err != nil {
		return Tunnel{}, err
	}
	conn, err := m.Dialer.Dial(ctx, h)
	if err != nil {
		return Tunnel{}, err
	}
	t := &tunnel{
		m: m, host: h, spec: spec, conn: conn,
		closing: make(chan struct{}), done: make(chan struct{}), active: map[net.Conn]struct{}{},
		info: Tunnel{ID: id, Type: spec.Type, Host: h.Name, Target: target, Owner: owner,
			CreatedAt: time.Now().UTC(), Status: Open, IdleMinutes: idle},
	}
	switch spec.Type {
	case Remote:
		ln, err := conn.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(spec.RemotePort)))
		if err != nil {
			conn.Close()
			return Tunnel{}, fmt.Errorf("%w on %s at 127.0.0.1:%d: %v", ErrListen, h.Name, spec.RemotePort, err)
		}
		t.rln = ln
		t.remotePort = ln.Addr().(*net.TCPAddr).Port
		t.info.Listen = net.JoinHostPort("127.0.0.1", strconv.Itoa(t.remotePort))
	default:
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(spec.LocalPort)))
		if err != nil {
			conn.Close()
			return Tunnel{}, fmt.Errorf("%w on 127.0.0.1:%d: %v", ErrListen, spec.LocalPort, err)
		}
		t.ln = ln
		t.info.Listen = ln.Addr().String()
	}
	m.mu.Lock()
	if m.tunnels == nil {
		m.tunnels = map[string]*tunnel{}
	}
	m.tunnels[id] = t
	m.mu.Unlock()

	t.mu.Lock()
	t.armIdle()
	t.mu.Unlock()
	if t.ln != nil {
		t.wg.Add(1)
		go t.acceptLocal()
	} else {
		t.wg.Add(1)
		go t.acceptRemote(t.rln)
	}
	go t.supervise()
	info := t.snapshot()
	m.log(eventlog.TunnelOpened, t, nil)
	return info, nil
}

// check validates spec (filling defaults) and returns the idle minutes
// and the target description.
func check(s *Spec, owner action.Actor) (int, string, error) {
	bad := func(format string, args ...any) (int, string, error) {
		return 0, "", fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, args...)...)
	}
	port := func(p int) bool { return p >= 0 && p <= 65535 }
	if !port(s.LocalPort) || !port(s.RemotePort) {
		return bad("ports must be 0..65535")
	}
	idle := 0
	if owner.Kind == action.Agent {
		idle = DefaultAgentIdle
	}
	if s.IdleMinutes != nil {
		if *s.IdleMinutes < 0 {
			return bad("idle_minutes must not be negative")
		}
		idle = *s.IdleMinutes
	}
	var target string
	switch s.Type {
	case Local:
		if s.RemotePort == 0 {
			return bad("a local tunnel needs remote_port")
		}
		if s.RemoteHost == "" {
			s.RemoteHost = "127.0.0.1"
		}
		target = net.JoinHostPort(s.RemoteHost, strconv.Itoa(s.RemotePort))
	case Remote:
		if s.LocalPort == 0 {
			return bad("a remote tunnel needs local_port (the port on this machine it leads to)")
		}
		if s.RemoteHost != "" && s.RemoteHost != "127.0.0.1" && s.RemoteHost != "localhost" {
			return bad("a remote tunnel leads to 127.0.0.1:local_port on this machine; remote_host is not used")
		}
		s.RemoteHost = ""
		target = net.JoinHostPort("127.0.0.1", strconv.Itoa(s.LocalPort))
	case Socks:
		if s.RemotePort != 0 || s.RemoteHost != "" {
			return bad("a socks tunnel takes no remote_host or remote_port")
		}
	default:
		return bad("type must be local, remote or socks, not %q", s.Type)
	}
	return idle, target, nil
}

// Get returns an open tunnel.
func (m *Manager) Get(id string) (Tunnel, bool) {
	t := m.get(id)
	if t == nil {
		return Tunnel{}, false
	}
	return t.snapshot(), true
}

// List returns the open tunnels, oldest first.
func (m *Manager) List() []Tunnel {
	m.mu.Lock()
	out := make([]Tunnel, 0, len(m.tunnels))
	for _, t := range m.tunnels {
		out = append(out, t.snapshot())
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Close closes a tunnel for by (owner rules are the caller's).
func (m *Manager) Close(id string, by action.Actor) error {
	return m.close(id, "closed by "+by.String())
}

// CloseAgent closes the tunnels of the agent named name, e.g. when it is
// revoked.
func (m *Manager) CloseAgent(name, reason string) {
	m.mu.Lock()
	var ids []string
	for id, t := range m.tunnels {
		if t.info.Owner.Kind == action.Agent && t.info.Owner.Name == name {
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.close(id, reason)
	}
}

// CloseAll closes every tunnel; the server calls it on shutdown.
func (m *Manager) CloseAll() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.tunnels))
	for id := range m.tunnels {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.close(id, "server stopped")
	}
}

func (m *Manager) close(id, reason string) error {
	m.mu.Lock()
	t := m.tunnels[id]
	delete(m.tunnels, id)
	m.mu.Unlock()
	if t == nil {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	t.shutdown()
	m.log(eventlog.TunnelClosed, t, func(ev *eventlog.Event) { ev.Reason = reason })
	return nil
}

func (m *Manager) get(id string) *tunnel {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tunnels[id]
}

func (m *Manager) log(typ string, t *tunnel, edit func(*eventlog.Event)) {
	if m.Events == nil {
		return
	}
	info := t.snapshot()
	owner := info.Owner
	detail, _ := json.Marshal(info)
	ev := eventlog.Event{Type: typ, Actor: &owner, Tunnel: info.ID, Detail: detail,
		Targets: []action.Target{{Kind: "host", ID: t.host.Name, Tags: t.host.Tags}}}
	if edit != nil {
		edit(&ev)
	}
	m.Events.Append(ev)
}

func (m *Manager) backoff(attempt int) time.Duration {
	if m.Backoff != nil {
		return m.Backoff(attempt)
	}
	return defaultBackoff(attempt)
}

func (m *Manager) afterFunc(d time.Duration, f func()) func() bool {
	if m.after != nil {
		return m.after(d, f)
	}
	return time.AfterFunc(d, f).Stop
}

func (m *Manager) maxOpen() int {
	if m.MaxOpen > 0 {
		return m.MaxOpen
	}
	return 64
}

func newID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "f_" + hex.EncodeToString(b[:]), nil
}

// tunnel is one open tunnel.
type tunnel struct {
	m          *Manager
	host       inventory.Host
	spec       Spec
	ln         net.Listener // local and socks: on this machine, kept across reconnects
	remotePort int          // remote: the port on the host, kept across reconnects
	closing    chan struct{}
	done       chan struct{} // the supervisor has stopped
	wg         sync.WaitGroup
	total      atomic.Int64
	out, in    atomic.Int64

	mu       sync.Mutex
	info     Tunnel
	conn     *sshx.Conn            // nil while reconnecting
	rln      net.Listener          // remote: the forward on the current connection
	active   map[net.Conn]struct{} // incoming connections
	targets  map[net.Conn]struct{} // their outgoing connections
	closed   bool
	idleStop func() bool
	idleGen  int
}

func (t *tunnel) snapshot() Tunnel {
	t.mu.Lock()
	info := t.info
	info.Connections = len(t.active)
	t.mu.Unlock()
	info.TotalConnections, info.BytesOut, info.BytesIn = t.total.Load(), t.out.Load(), t.in.Load()
	return info
}

// armIdle starts the idle timer if the tunnel has one and is unused.
// t.mu is held.
func (t *tunnel) armIdle() {
	if t.info.IdleMinutes == 0 || t.closed || len(t.active) > 0 {
		return
	}
	t.idleGen++
	gen := t.idleGen
	d := time.Duration(t.info.IdleMinutes) * time.Minute
	t.idleStop = t.m.afterFunc(d, func() {
		t.mu.Lock()
		stale := gen != t.idleGen || len(t.active) > 0 || t.closed
		t.mu.Unlock()
		if !stale {
			t.m.close(t.info.ID, fmt.Sprintf("idle for %d minutes", t.info.IdleMinutes))
		}
	})
}

// track registers a connection; false if the tunnel is closing.
func (t *tunnel) track(c net.Conn) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return false
	}
	t.active[c] = struct{}{}
	t.total.Add(1)
	t.idleGen++ // a timer that fires now is stale
	if t.idleStop != nil {
		t.idleStop()
		t.idleStop = nil
	}
	return true
}

func (t *tunnel) untrack(c net.Conn) {
	t.mu.Lock()
	delete(t.active, c)
	t.armIdle()
	t.mu.Unlock()
}

func (t *tunnel) current() *sshx.Conn {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.conn
}

// acceptLocal serves the listener on this machine.
func (t *tunnel) acceptLocal() {
	defer t.wg.Done()
	for {
		c, err := t.ln.Accept()
		if err != nil {
			return
		}
		if !t.track(c) {
			c.Close()
			return
		}
		t.wg.Add(1)
		go func() {
			defer t.wg.Done()
			defer t.untrack(c)
			defer c.Close()
			t.serveLocal(c)
		}()
	}
}

func (t *tunnel) serveLocal(c net.Conn) {
	target := t.info.Target
	if t.spec.Type == Socks {
		var err error
		if target, err = socksHandshake(c); err != nil {
			return
		}
	}
	conn := t.current()
	if conn == nil { // reconnecting
		if t.spec.Type == Socks {
			socksReply(c, socksGeneralFailure)
		}
		return
	}
	up, err := conn.Dial("tcp", target)
	if err != nil {
		if t.spec.Type == Socks {
			socksReply(c, socksRefused)
		}
		return
	}
	defer up.Close()
	if t.spec.Type == Socks {
		if socksReply(c, socksOK) != nil {
			return
		}
	}
	t.pipe(c, up)
}

// acceptRemote serves a remote forward until the connection or the
// tunnel ends.
func (t *tunnel) acceptRemote(ln net.Listener) {
	defer t.wg.Done()
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		if !t.track(c) {
			c.Close()
			return
		}
		t.wg.Add(1)
		go func() {
			defer t.wg.Done()
			defer t.untrack(c)
			defer c.Close()
			up, err := net.Dial("tcp", t.info.Target)
			if err != nil {
				return
			}
			defer up.Close()
			t.pipe(c, up)
		}()
	}
}

// pipe copies both ways between the connecting side c and the target,
// counting bytes, until both directions end.
func (t *tunnel) pipe(c, target net.Conn) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	if t.targets == nil {
		t.targets = map[net.Conn]struct{}{}
	}
	t.targets[target] = struct{}{}
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		delete(t.targets, target)
		t.mu.Unlock()
	}()
	done := make(chan struct{})
	go func() {
		io.Copy(counter{target, &t.out}, c)
		closeWrite(target)
		close(done)
	}()
	io.Copy(counter{c, &t.in}, target)
	closeWrite(c)
	<-done
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
		return
	}
	c.Close()
}

type counter struct {
	w io.Writer
	n *atomic.Int64
}

func (c counter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))
	return n, err
}

// supervise reconnects when the SSH connection breaks, until the tunnel
// closes.
func (t *tunnel) supervise() {
	defer close(t.done)
	for {
		conn := t.current()
		wait := make(chan error, 1)
		go func() { wait <- conn.Wait() }()
		var err error
		select {
		case <-t.closing:
			return
		case err = <-wait:
		}
		t.mu.Lock()
		if t.closed {
			t.mu.Unlock()
			return
		}
		t.conn, t.rln = nil, nil
		t.info.Status = Reconnecting
		t.info.Error = "connection lost"
		if err != nil {
			t.info.Error = "connection lost: " + err.Error()
		}
		t.mu.Unlock()
		conn.Close()
		t.m.log(eventlog.TunnelReconnecting, t, func(ev *eventlog.Event) { ev.Error = t.snapshot().Error })
		if !t.reconnect() {
			return
		}
		t.m.log(eventlog.TunnelReconnected, t, nil)
	}
}

// reconnect dials until it succeeds (and restores the remote forward on
// the same port), or the tunnel closes.
func (t *tunnel) reconnect() bool {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-t.closing:
			cancel()
		case <-ctx.Done():
		}
	}()
	for attempt := 0; ; attempt++ {
		select {
		case <-t.closing:
			return false
		case <-time.After(t.m.backoff(attempt)):
		}
		conn, err := t.m.Dialer.Dial(ctx, t.host)
		var rln net.Listener
		if err == nil && t.spec.Type == Remote {
			rln, err = conn.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(t.remotePort)))
			if err != nil {
				conn.Close()
			}
		}
		if err != nil {
			t.mu.Lock()
			t.info.Error = "reconnecting: " + err.Error()
			t.mu.Unlock()
			continue
		}
		t.mu.Lock()
		if t.closed {
			t.mu.Unlock()
			conn.Close()
			return false
		}
		t.conn, t.rln = conn, rln
		t.info.Status, t.info.Error = Open, ""
		if rln != nil {
			t.wg.Add(1)
			go t.acceptRemote(rln)
		}
		t.mu.Unlock()
		return true
	}
}

// shutdown stops listening, ends every connection and waits for the
// tunnel's goroutines.
func (t *tunnel) shutdown() {
	t.mu.Lock()
	t.closed = true
	if t.idleStop != nil {
		t.idleStop()
	}
	conn, rln := t.conn, t.rln
	active := make([]net.Conn, 0, len(t.active)+len(t.targets))
	for c := range t.active {
		active = append(active, c)
	}
	for c := range t.targets {
		active = append(active, c)
	}
	t.mu.Unlock()
	close(t.closing)
	if t.ln != nil {
		t.ln.Close()
	}
	if rln != nil {
		rln.Close() // cancel-tcpip-forward
	}
	if conn != nil {
		conn.Close()
	}
	for _, c := range active {
		c.Close()
	}
	<-t.done
	t.wg.Wait()
}

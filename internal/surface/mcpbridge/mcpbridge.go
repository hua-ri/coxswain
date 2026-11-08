// Package mcpbridge is `cox mcp`: it relays JSON-RPC messages between an
// agent on stdio and the running server's /mcp endpoint.
//
// While the server is down the bridge answers by itself, from a local MCP
// server with the same tools whose calls report that Coxswain is not
// running, so the agent's handshake never fails. Once the server is
// reachable, the bridge connects, re-initializes the session the agent
// already set up and relays from then on.
//
// If the server drops the session (e.g. the client was revoked), the
// next call reconnects; a call the server refused for that reason is sent
// once more. A call whose connection dropped after the server took it is
// never resent: the agent is told the outcome is unknown, or that the
// server is not running if it no longer answers.
package mcpbridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/instance"
	mcpsurf "github.com/hua-ri/coxswain/internal/surface/mcp"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// CodeNotRunning is the JSON-RPC error code for "server not reachable".
const CodeNotRunning = -32000

// CodeConnectionLost is the JSON-RPC error code for a call whose
// connection dropped after the server accepted it, while the server is
// still running.
const CodeConnectionLost = -32001

// connectionLost is its message.
const connectionLost = "lost the connection to the Coxswain server; the outcome is unknown (the call may have run). " +
	"Check before retrying"

// replayTimeout bounds re-initializing a session after a reconnect.
const replayTimeout = 10 * time.Second

// Options configure Run.
type Options struct {
	Stdin    io.Reader
	Stdout   io.Writer
	Home     string // where server.json is
	Client   string // X-Cox-Client
	Version  string
	Registry *action.Registry // tools offered while the server is down
}

// Run relays until stdin ends or ctx is done.
func Run(ctx context.Context, o Options) error {
	down, err := (&sdk.IOTransport{Reader: io.NopCloser(o.Stdin), Writer: nopWriteCloser{o.Stdout}}).Connect(ctx)
	if err != nil {
		return err
	}
	defer down.Close()
	off, err := startOffline(ctx, mcpsurf.NewOffline(o.Registry, o.Version, instance.ErrNotRunning.Error()))
	if err != nil {
		return err
	}
	defer off.conn.Close()
	b := &bridge{ctx: ctx, down: down, off: off, home: o.Home, client: o.Client}
	defer b.closeUp()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		msg, err := down.Read(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return nil
			}
			return err
		}
		switch m := msg.(type) {
		case *jsonrpc.Request:
			switch {
			case m.Method == "initialize":
				b.initialize(m)
			case m.IsCall():
				b.call(m, &wg)
			default:
				if m.Method == "notifications/initialized" {
					b.mu.Lock()
					b.initialized = m
					b.mu.Unlock()
					off.notify(m)
				}
				b.notify(m)
			}
		case *jsonrpc.Response:
			b.notify(m) // the agent answering a server request
		}
	}
}

type bridge struct {
	ctx          context.Context
	down         sdk.Connection
	off          *offline
	home, client string
	ids          atomic.Int64

	mu          sync.Mutex // guards the fields below; never held during I/O
	up          *upstream
	init        *jsonrpc.Request // the agent's initialize, replayed on reconnect
	initialized *jsonrpc.Request
}

// initialize sets up the local session (always) and a server session if
// the server is reachable; the agent gets the server's answer, or the
// local one while the server is down.
func (b *bridge) initialize(req *jsonrpc.Request) {
	b.mu.Lock()
	b.init, b.initialized = req, nil
	b.mu.Unlock()
	b.drop(nil) // a new handshake starts a new server session
	local, err := b.off.call(b.ctx, req)
	if u := b.connect(false); u != nil {
		u.track(req.ID)
		if b.send(u, req) {
			return
		}
	}
	if err != nil {
		b.fail(req.ID)
		return
	}
	local.ID = req.ID
	b.down.Write(b.ctx, local)
}

// call relays one agent request, connecting first (and re-initializing
// the session). Requests go out in order; their responses arrive
// concurrently. If the server is unreachable, the local server answers.
func (b *bridge) call(req *jsonrpc.Request, wg *sync.WaitGroup) {
	if u := b.connect(true); u != nil {
		b.relay(u, req, true)
		return
	}
	wg.Add(1)
	go func() { defer wg.Done(); b.local(req) }()
}

// relay sends an agent call on u and returns once it is on its way. If
// the server did not take it (its session was gone, or the request never
// got out), the bridge reconnects and, with retry, sends it once more.
// A call the server may have taken is never sent twice.
func (b *bridge) relay(u *upstream, req *jsonrpc.Request, retry bool) {
	u.track(req.ID)
	done := u.write(b.ctx, req)
	go func() {
		res := <-done
		if res.err == nil {
			if !u.accept(req.ID) { // the connection failed meanwhile
				b.lost(req.ID)
			}
			return
		}
		hook, _ := writeFailed.Load().(func())
		if hook != nil {
			hook()
		}
		u.untrack(req.ID)
		b.drop(u)
		notTaken := errors.Is(res.err, sdk.ErrSessionMissing) || !res.sent
		switch {
		case !notTaken:
			b.lost(req.ID)
		case !retry:
			b.unsent(req)
		default:
			if nu := b.connect(true); nu != nil {
				b.relay(nu, req, false)
				return
			}
			b.unsent(req)
		}
	}()
}

// lost answers a call the server may have run but whose answer can no
// longer arrive: the outcome is unknown, unless the server is gone.
func (b *bridge) lost(id jsonrpc.ID) {
	if !b.reachable() {
		b.fail(id)
		return
	}
	b.down.Write(b.ctx, &jsonrpc.Response{ID: id, Error: &jsonrpc.Error{Code: CodeConnectionLost, Message: connectionLost}})
}

// unsent answers a call that did not reach the server: from the local
// server if the server is gone, else with an error saying it did not run.
func (b *bridge) unsent(req *jsonrpc.Request) {
	if !b.reachable() {
		b.local(req)
		return
	}
	b.down.Write(b.ctx, &jsonrpc.Response{ID: req.ID, Error: &jsonrpc.Error{Code: CodeConnectionLost,
		Message: "could not reach the Coxswain server session; the call did not run. Try again"}})
}

// reachable reports whether the server answers.
func (b *bridge) reachable() bool {
	_, err := instance.Discover(b.ctx, b.home)
	return err == nil
}

// local answers req from the offline server.
func (b *bridge) local(req *jsonrpc.Request) {
	resp, err := b.off.call(b.ctx, req)
	if err != nil {
		b.fail(req.ID)
		return
	}
	resp.ID = req.ID
	b.down.Write(b.ctx, resp)
}

// notify relays a notification or response if connected; else drops it.
func (b *bridge) notify(msg jsonrpc.Message) {
	b.mu.Lock()
	u := b.up
	b.mu.Unlock()
	if u != nil {
		b.sendAsync(u, msg, nil)
	}
}

// fail answers id with the not-running error.
func (b *bridge) fail(id jsonrpc.ID) {
	b.down.Write(b.ctx, &jsonrpc.Response{ID: id, Error: &jsonrpc.Error{Code: CodeNotRunning, Message: instance.ErrNotRunning.Error()}})
}

// send writes msg on u and waits for the result; on failure u is dropped.
func (b *bridge) send(u *upstream, msg jsonrpc.Message) bool {
	if res := <-u.write(b.ctx, msg); res.err != nil {
		b.forget(u, msg)
		return false
	}
	return true
}

// sendAsync starts writing msg on u and returns once the request has
// been handed to the network, so the next message goes out after it. If
// the write fails later, u is dropped and onFail (if any) runs.
func (b *bridge) sendAsync(u *upstream, msg jsonrpc.Message, onFail func()) {
	done := u.write(b.ctx, msg)
	go func() {
		if res := <-done; res.err != nil {
			b.forget(u, msg)
			if onFail != nil {
				onFail()
			}
		}
	}()
}

func (b *bridge) forget(u *upstream, msg jsonrpc.Message) {
	if req, ok := msg.(*jsonrpc.Request); ok && req.IsCall() {
		u.untrack(req.ID)
	}
	b.drop(u)
}

// connect returns the live upstream or opens a new one: it re-reads
// server.json (the token changes on every server start) and, with
// replay, re-initializes the session the agent already set up. It
// returns nil if the server is not reachable.
func (b *bridge) connect(replay bool) *upstream {
	b.mu.Lock()
	u, init, initialized := b.up, b.init, b.initialized
	b.mu.Unlock()
	if u != nil {
		return u
	}
	info, err := instance.Discover(b.ctx, b.home)
	if err != nil {
		return nil
	}
	conn, err := (&sdk.StreamableClientTransport{
		Endpoint:   "http://" + info.Addr + "/mcp",
		HTTPClient: &http.Client{Transport: &headers{token: info.Token, client: b.client}},
		MaxRetries: -1,
	}).Connect(b.ctx)
	if err != nil {
		return nil
	}
	u = &upstream{conn: conn, pending: map[jsonrpc.ID]bool{}, internal: map[jsonrpc.ID]chan *jsonrpc.Response{}}
	go b.read(u)
	if replay && init != nil {
		if err := b.replay(u, init, initialized); err != nil {
			conn.Close()
			return nil
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.up != nil { // connected concurrently: keep the first
		go conn.Close()
		return b.up
	}
	b.up = u
	return u
}

// replay sends the agent's initialize (under a bridge-owned id) and its
// initialized notification on a fresh connection.
func (b *bridge) replay(u *upstream, init, initialized *jsonrpc.Request) error {
	req := *init
	req.ID, _ = jsonrpc.MakeID(fmt.Sprintf("cox-bridge-%d", b.ids.Add(1)))
	ch := make(chan *jsonrpc.Response, 1)
	u.mu.Lock()
	u.internal[req.ID] = ch
	u.mu.Unlock()
	if res := <-u.write(b.ctx, &req); res.err != nil {
		return res.err
	}
	select {
	case resp, ok := <-ch:
		if !ok {
			return errors.New("connection lost while re-initializing")
		}
		if resp.Error != nil {
			return resp.Error
		}
	case <-time.After(replayTimeout):
		return errors.New("timed out re-initializing the session")
	case <-b.ctx.Done():
		return b.ctx.Err()
	}
	if initialized != nil {
		return (<-u.write(b.ctx, initialized)).err
	}
	return nil
}

// read relays server messages to the agent until the connection fails.
// Then the calls the server had taken are answered (see lost); calls
// still being sent are left to relay, which knows whether they got out.
func (b *bridge) read(u *upstream) {
	for {
		msg, err := u.conn.Read(b.ctx)
		if err != nil {
			b.drop(u)
			u.mu.Lock()
			u.dead = true
			var taken []jsonrpc.ID
			for id, accepted := range u.pending {
				if accepted {
					taken = append(taken, id)
					delete(u.pending, id)
				}
			}
			for _, ch := range u.internal {
				close(ch)
			}
			u.internal = map[jsonrpc.ID]chan *jsonrpc.Response{}
			u.mu.Unlock()
			for _, id := range taken {
				b.lost(id)
			}
			return
		}
		if resp, ok := msg.(*jsonrpc.Response); ok {
			u.mu.Lock()
			ch, internal := u.internal[resp.ID]
			delete(u.internal, resp.ID)
			delete(u.pending, resp.ID)
			u.mu.Unlock()
			if internal {
				ch <- resp
				continue
			}
			if terminated(resp.Error) {
				// The server closed the call's stream without answering:
				// it is shutting down or closed the session.
				b.drop(u)
				b.lost(resp.ID)
				continue
			}
		}
		b.down.Write(b.ctx, msg)
	}
}

// terminated reports the SDK's synthetic error for a stream that ended
// without a response.
func terminated(err error) bool {
	var wire *jsonrpc.Error
	return err != nil && !errors.As(err, &wire) && strings.Contains(err.Error(), "terminated without response")
}

// closeUp ends the server session, waiting for it.
func (b *bridge) closeUp() {
	b.mu.Lock()
	u := b.up
	b.up = nil
	b.mu.Unlock()
	if u != nil {
		u.conn.Close()
	}
}

// drop forgets u (nil: the current upstream) and closes it.
func (b *bridge) drop(u *upstream) {
	b.mu.Lock()
	if u == nil {
		u = b.up
	}
	if u != nil && b.up == u {
		b.up = nil
	}
	b.mu.Unlock()
	if u != nil {
		go u.conn.Close()
	}
}

// upstream is one connection to the server.
type upstream struct {
	conn     sdk.Connection
	mu       sync.Mutex
	pending  map[jsonrpc.ID]bool                   // agent calls awaiting a response: true once the server took them
	internal map[jsonrpc.ID]chan *jsonrpc.Response // the bridge's own calls
	dead     bool                                  // the connection failed
}

// track records a call about to be sent.
func (u *upstream) track(id jsonrpc.ID) {
	u.mu.Lock()
	u.pending[id] = false
	u.mu.Unlock()
}

// accept records that the server took a call (its write succeeded). It
// reports false, forgetting the call, if the connection already failed:
// its answer will not arrive.
func (u *upstream) accept(id jsonrpc.ID) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if _, ok := u.pending[id]; !ok {
		return true // already answered
	}
	if u.dead {
		delete(u.pending, id)
		return false
	}
	u.pending[id] = true
	return true
}

func (u *upstream) untrack(id jsonrpc.ID) {
	u.mu.Lock()
	delete(u.pending, id)
	u.mu.Unlock()
}

// writeResult is how a write ended; sent reports that the whole request
// body went out (so the server may have received it).
type writeResult struct {
	err  error
	sent bool
}

// write starts writing msg and returns once its HTTP request body has
// been sent (or the write ended). The channel yields the write's result.
func (u *upstream) write(ctx context.Context, msg jsonrpc.Message) <-chan writeResult {
	started := make(chan struct{})
	var once sync.Once
	t := &sendTracker{signal: func() { once.Do(func() { close(started) }) }}
	done := make(chan writeResult, 1)
	go func() {
		err := u.conn.Write(context.WithValue(ctx, sentKey{}, t), msg)
		t.signal()
		done <- writeResult{err: err, sent: t.sent.Load()}
	}()
	<-started
	return done
}

type sentKey struct{}

// sendTracker follows one request body: signal when it is done (sent or
// abandoned), sent if it was read to the end.
type sendTracker struct {
	signal func()
	sent   atomic.Bool
}

// writeFailed (a func()) runs when a call's write fails, before the
// bridge reacts. Tests use it to let the connection's failure be seen
// first.
var writeFailed atomic.Value

// headers adds the bearer token and client name to every request, and
// reports when a request body has been sent (see upstream.write).
type headers struct {
	token, client string
}

func (h *headers) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+h.token)
	r.Header.Set("X-Cox-Client", h.client)
	if t, ok := r.Context().Value(sentKey{}).(*sendTracker); ok && r.Body != nil {
		r.Body = &signalBody{ReadCloser: r.Body, t: t}
	}
	return http.DefaultTransport.RoundTrip(r)
}

// signalBody reports to its tracker once the body has been read to the
// end (sent) or closed.
type signalBody struct {
	io.ReadCloser
	t *sendTracker
}

func (s *signalBody) Read(p []byte) (int, error) {
	n, err := s.ReadCloser.Read(p)
	if err == io.EOF {
		s.t.sent.Store(true)
	}
	if err != nil {
		s.t.signal()
	}
	return n, err
}

func (s *signalBody) Close() error {
	s.t.signal()
	return s.ReadCloser.Close()
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

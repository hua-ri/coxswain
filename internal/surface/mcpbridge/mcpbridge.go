// Package mcpbridge is `cox mcp`: it relays JSON-RPC messages between an
// agent on stdio and the running server's /mcp endpoint.
//
// While the server is down the bridge answers by itself, from a local MCP
// server with the same tools whose calls report that Coxswain is not
// running, so the agent's handshake never fails. Once the server is
// reachable, the bridge connects, re-initializes the session the agent
// already set up and relays from then on.
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
		u.track(req.ID)
		b.sendAsync(u, req, func() { // the write failed: reconnect once
			if u := b.connect(true); u != nil {
				u.track(req.ID)
				if b.send(u, req) {
					return
				}
			}
			b.local(req)
		})
		return
	}
	wg.Add(1)
	go func() { defer wg.Done(); b.local(req) }()
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
	if err := <-u.write(b.ctx, msg); err != nil {
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
		if err := <-done; err != nil {
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
	if err := <-u.write(b.ctx, &req); err != nil {
		return err
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
		return <-u.write(b.ctx, initialized)
	}
	return nil
}

// read relays server messages to the agent until the connection fails,
// then answers the agent's pending calls with the not-running error.
func (b *bridge) read(u *upstream) {
	for {
		msg, err := u.conn.Read(b.ctx)
		if err != nil {
			b.drop(u)
			u.mu.Lock()
			pending := u.pending
			u.pending = map[jsonrpc.ID]bool{}
			for _, ch := range u.internal {
				close(ch)
			}
			u.internal = map[jsonrpc.ID]chan *jsonrpc.Response{}
			u.mu.Unlock()
			for id := range pending {
				b.fail(id)
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
			if lost(resp.Error) {
				// The server closed the stream without answering: it is
				// most likely shutting down.
				b.drop(u)
				b.fail(resp.ID)
				continue
			}
		}
		b.down.Write(b.ctx, msg)
	}
}

// lost reports the SDK's synthetic error for a stream that ended without
// a response.
func lost(err error) bool {
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
	pending  map[jsonrpc.ID]bool                   // agent calls awaiting a response
	internal map[jsonrpc.ID]chan *jsonrpc.Response // the bridge's own calls
}

func (u *upstream) track(id jsonrpc.ID) {
	u.mu.Lock()
	u.pending[id] = true
	u.mu.Unlock()
}

func (u *upstream) untrack(id jsonrpc.ID) {
	u.mu.Lock()
	delete(u.pending, id)
	u.mu.Unlock()
}

// write starts writing msg and returns once its HTTP request body has
// been sent (or the write ended). The channel yields the write's result.
func (u *upstream) write(ctx context.Context, msg jsonrpc.Message) <-chan error {
	sent := make(chan struct{})
	var once sync.Once
	signal := func() { once.Do(func() { close(sent) }) }
	done := make(chan error, 1)
	go func() {
		err := u.conn.Write(context.WithValue(ctx, sentKey{}, signal), msg)
		signal()
		done <- err
	}()
	<-sent
	return done
}

type sentKey struct{}

// headers adds the bearer token and client name to every request, and
// reports when a request body has been sent (see upstream.write).
type headers struct {
	token, client string
}

func (h *headers) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+h.token)
	r.Header.Set("X-Cox-Client", h.client)
	if signal, ok := r.Context().Value(sentKey{}).(func()); ok && r.Body != nil {
		r.Body = &signalBody{ReadCloser: r.Body, signal: signal}
	}
	return http.DefaultTransport.RoundTrip(r)
}

// signalBody calls signal once the body has been read to the end.
type signalBody struct {
	io.ReadCloser
	signal func()
}

func (s *signalBody) Read(p []byte) (int, error) {
	n, err := s.ReadCloser.Read(p)
	if err != nil {
		s.signal()
	}
	return n, err
}

func (s *signalBody) Close() error {
	s.signal()
	return s.ReadCloser.Close()
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

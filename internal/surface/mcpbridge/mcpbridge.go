// Package mcpbridge is `cox mcp`: it relays JSON-RPC messages between an
// agent on stdio and the running server's /mcp endpoint. It keeps serving
// while the server is down (answering requests with an error) and
// reconnects, re-initializing the session, once the server is back.
package mcpbridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hua-ri/coxswain/internal/instance"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// CodeNotRunning is the JSON-RPC error code for "server not reachable".
const CodeNotRunning = -32000

// replayTimeout bounds re-initializing a session after a reconnect.
const replayTimeout = 30 * time.Second

// Run relays between stdin/stdout and the server found in home, as
// client, until stdin ends or ctx is done.
func Run(ctx context.Context, stdin io.Reader, stdout io.Writer, home, client string) error {
	down, err := (&sdk.IOTransport{Reader: io.NopCloser(stdin), Writer: nopWriteCloser{stdout}}).Connect(ctx)
	if err != nil {
		return err
	}
	defer down.Close()
	b := &bridge{ctx: ctx, down: down, home: home, client: client}
	defer b.drop(nil)
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
				b.mu.Lock()
				b.init, b.initialized = m, nil
				b.mu.Unlock()
				b.call(m, false) // in order: later messages need its session
			case m.IsCall():
				wg.Add(1)
				go func() { defer wg.Done(); b.call(m, true) }()
			default:
				if m.Method == "notifications/initialized" {
					b.mu.Lock()
					b.initialized = m
					b.mu.Unlock()
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
	home, client string
	ids          atomic.Int64

	mu          sync.Mutex // guards the fields below; held while connecting
	up          *upstream
	init        *jsonrpc.Request // the agent's initialize, replayed on reconnect
	initialized *jsonrpc.Request
}

// upstream is one connection to the server.
type upstream struct {
	conn     sdk.Connection
	mu       sync.Mutex
	pending  map[jsonrpc.ID]bool                   // agent calls awaiting a response
	internal map[jsonrpc.ID]chan *jsonrpc.Response // the bridge's own calls
}

// call forwards one agent request, connecting (and, with replay,
// re-initializing) first. If the connection turns out to be stale, it
// reconnects once. Failures are answered with a JSON-RPC error.
func (b *bridge) call(req *jsonrpc.Request, replay bool) {
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		var u *upstream
		if u, err = b.connect(replay); err != nil {
			break
		}
		u.track(req.ID)
		if err = u.conn.Write(b.ctx, req); err == nil {
			return
		}
		u.untrack(req.ID)
		b.drop(u)
	}
	b.reply(req.ID, err)
}

// notify forwards a notification or response if connected; else drops it.
func (b *bridge) notify(msg jsonrpc.Message) {
	b.mu.Lock()
	u := b.up
	b.mu.Unlock()
	if u != nil && u.conn.Write(b.ctx, msg) != nil {
		b.drop(u)
	}
}

func (b *bridge) reply(id jsonrpc.ID, err error) {
	msg := err.Error()
	if errors.Is(err, instance.ErrNotRunning) {
		msg = instance.ErrNotRunning.Error()
	}
	b.down.Write(b.ctx, &jsonrpc.Response{ID: id, Error: &jsonrpc.Error{Code: CodeNotRunning, Message: msg}})
}

// connect returns the live upstream or opens a new one: it re-reads
// server.json (the token changes on every server start) and, with
// replay, re-initializes the session the agent already set up.
func (b *bridge) connect(replay bool) (*upstream, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.up != nil {
		return b.up, nil
	}
	info, err := instance.Discover(b.ctx, b.home)
	if err != nil {
		return nil, err
	}
	conn, err := (&sdk.StreamableClientTransport{
		Endpoint:   "http://" + info.Addr + "/mcp",
		HTTPClient: &http.Client{Transport: &headers{token: info.Token, client: b.client}},
		MaxRetries: -1,
	}).Connect(b.ctx)
	if err != nil {
		return nil, err
	}
	u := &upstream{conn: conn, pending: map[jsonrpc.ID]bool{}, internal: map[jsonrpc.ID]chan *jsonrpc.Response{}}
	go b.read(u)
	if replay && b.init != nil {
		if err := b.replay(u); err != nil {
			conn.Close()
			return nil, err
		}
	}
	b.up = u
	return u, nil
}

// replay sends the agent's initialize (under a bridge-owned id) and its
// initialized notification on a fresh connection. b.mu is held.
func (b *bridge) replay(u *upstream) error {
	init := *b.init
	init.ID, _ = jsonrpc.MakeID(fmt.Sprintf("cox-bridge-%d", b.ids.Add(1)))
	ch := make(chan *jsonrpc.Response, 1)
	u.mu.Lock()
	u.internal[init.ID] = ch
	u.mu.Unlock()
	if err := u.conn.Write(b.ctx, &init); err != nil {
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
	if b.initialized != nil {
		return u.conn.Write(b.ctx, b.initialized)
	}
	return nil
}

// read relays server messages to the agent until the connection fails,
// then answers the agent's pending calls with an error.
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
				b.reply(id, fmt.Errorf("lost the connection to the Coxswain server: %w", err))
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
		}
		b.down.Write(b.ctx, msg)
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
		u.conn.Close()
	}
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

// headers adds the bearer token and client name to every request.
type headers struct {
	token, client string
}

func (h *headers) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+h.token)
	r.Header.Set("X-Cox-Client", h.client)
	return http.DefaultTransport.RoundTrip(r)
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

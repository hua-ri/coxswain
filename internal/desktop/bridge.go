// Package desktop is the Go side of `cox desktop`. The page in the
// webview never talks to the server itself (the server refuses browser
// requests and the page never sees the token): it calls Bridge functions
// bound into the page, and Bridge pushes streams to the page with
// window.__cox.emit(subID, payload).
package desktop

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hua-ri/coxswain/internal/client"
	"github.com/hua-ri/coxswain/internal/instance"
)

// ClientName is who the desktop app is to the server.
const ClientName = "desktop"

// StatusChannel is the subscription id under which connection changes
// are pushed: {type:"connected", addr, version, pid} and
// {type:"disconnected"}.
const StatusChannel = "bridge"

// ReplyChannel is the subscription id under which the results of the
// page's coxApi calls are pushed: {type:"reply", call, ok, value|error}.
const ReplyChannel = "reply"

// inputQueue bounds the keystroke and resize requests waiting per terminal.
const inputQueue = 1024

// Window is what the bridge needs from the webview. Dispatch runs f on
// the UI thread; Eval runs JavaScript in the page.
type Window interface {
	Eval(js string)
	Dispatch(f func())
}

// Status is the connection state, for the status bar.
type Status struct {
	Connected bool   `json:"connected"`
	Addr      string `json:"addr,omitempty"`
	Version   string `json:"version,omitempty"`
	PID       int    `json:"pid,omitempty"`
}

// Bridge connects the page to the running server. While the server is
// away it retries discovery every Retry and, once back, resubscribes.
//
// Payloads pushed with window.__cox.emit(subID, payload):
//
//	events:   {type:"event", event:<audit event>}
//	terminal: {type:"data", data:<base64>, backlog:true|absent}  (backlog: reset the screen, then write)
//	          {type:"gap"}                                       (some output was dropped)
//	          {type:"exit", code:<int>|null}                     (last message of the subscription)
//	"bridge": {type:"connected", addr, version, pid} / {type:"disconnected"}
//	"reply":  {type:"reply", call:<id from coxApi>, ok:true, value:<JSON>} / {…, ok:false, error:"…"}
//
// Bound functions run on the UI thread, so none of them waits for the
// server: coxApi returns a call id and its result comes on "reply";
// coxInput and coxResize queue their request (in order, per terminal).
type Bridge struct {
	// Retry is how often the server is checked; default 2s.
	Retry time.Duration
	// Pick shows a native "open file" dialog and returns the chosen
	// file's absolute path, or "" if cancelled. It runs on the UI
	// thread. nil: this platform has no file picker.
	Pick func() string

	home string
	win  Window

	ctx    context.Context
	cancel context.CancelFunc
	check  chan struct{}
	done   chan struct{}

	mu     sync.Mutex
	client *client.Client // nil while disconnected
	subs   map[string]*sub
	next   int
	calls  int
	inputs map[string]chan func(*client.Client) error // per terminal, in order
}

// sub is one page subscription; cancel is set while its stream runs.
type sub struct {
	kind, id string
	cancel   context.CancelFunc
}

// New returns a bridge for the server found in home (~/.coxswain).
func New(win Window, home string) *Bridge {
	return &Bridge{home: home, win: win, subs: map[string]*sub{}, inputs: map[string]chan func(*client.Client) error{},
		check: make(chan struct{}, 1)}
}

// Start connects to the running server (instance.ErrNotRunning if there
// is none) and keeps watching it until Close.
func (b *Bridge) Start(ctx context.Context) error {
	c, err := client.Discover(ctx, b.home, ClientName)
	if err != nil {
		return err
	}
	b.ctx, b.cancel = context.WithCancel(context.Background())
	b.done = make(chan struct{})
	b.mu.Lock()
	b.client = c
	b.mu.Unlock()
	go b.watch()
	return nil
}

// Close stops every stream and the watcher.
func (b *Bridge) Close() {
	if b.cancel == nil {
		return
	}
	b.cancel()
	<-b.done
}

// Bindings are the functions to bind into the page (webview.Bind).
func (b *Bridge) Bindings() map[string]any {
	return map[string]any{
		"coxApi":         b.APIAsync,
		"coxSubscribe":   b.Subscribe,
		"coxUnsubscribe": b.Unsubscribe,
		"coxInput":       b.Input,
		"coxResize":      b.Resize,
		"coxStatus":      b.Status,
		"coxPickFile":    b.PickFileAsync,
	}
}

// Status reports the connection.
func (b *Bridge) Status() Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.client == nil {
		return Status{}
	}
	i := b.client.Info
	return Status{Connected: true, Addr: i.Addr, Version: i.Version, PID: i.PID}
}

// API calls the server: path must be under /api/v1/ (streams go through
// Subscribe). body, if not nil, is sent as JSON. It returns the decoded
// JSON answer (nil for 204); a non-2xx answer is an error with the
// server's message. Actions answer 200 with {status, result, error}.
func (b *Bridge) API(method, path string, body any) (any, error) {
	if !strings.HasPrefix(path, "/api/v1/") || path == "/api/v1/events" || strings.HasSuffix(path, "/stream") {
		return nil, fmt.Errorf("not an API path: %q", path)
	}
	c, err := b.current()
	if err != nil {
		return nil, err
	}
	var raw []byte
	if body != nil {
		if raw, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	resp, err := c.Do(b.ctx, method, path, raw)
	if err != nil {
		b.lost()
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var out any
	if len(data) > 0 {
		if err := json.Unmarshal(data, &out); err != nil {
			return nil, fmt.Errorf("server: %s: %w", resp.Status, err)
		}
	}
	if resp.StatusCode/100 != 2 {
		msg := resp.Status
		if m, ok := out.(map[string]any); ok && m["error"] != nil {
			msg = fmt.Sprint(m["error"])
		}
		return nil, fmt.Errorf("server: %s (%d)", msg, resp.StatusCode)
	}
	return out, nil
}

// APIAsync starts an API call and returns its call id at once; the
// result is pushed on ReplyChannel. It is the page's coxApi.
func (b *Bridge) APIAsync(method, path string, body any) string {
	b.mu.Lock()
	b.calls++
	call := "c" + strconv.Itoa(b.calls)
	b.mu.Unlock()
	go func() {
		v, err := b.API(method, path, body)
		reply := map[string]any{"type": "reply", "call": call, "ok": err == nil}
		if err != nil {
			reply["error"] = err.Error()
		} else {
			reply["value"] = v
		}
		b.emit(ReplyChannel, reply)
	}()
	return call
}

// PickFileAsync asks the user for a local file and returns a call id at
// once; the chosen absolute path ("" if cancelled) is pushed on
// ReplyChannel. The dialog runs from the UI thread's queue, not inside
// the binding call. It is the page's coxPickFile.
func (b *Bridge) PickFileAsync() string {
	b.mu.Lock()
	b.calls++
	call := "c" + strconv.Itoa(b.calls)
	pick := b.Pick
	b.mu.Unlock()
	b.win.Dispatch(func() {
		reply := map[string]any{"type": "reply", "call": call, "ok": pick != nil}
		if pick == nil {
			reply["error"] = "this desktop build has no file picker"
		} else {
			reply["value"] = pick()
		}
		b.emit(ReplyChannel, reply)
	})
	return call
}

// Input queues base64-encoded keystrokes for a terminal.
func (b *Bridge) Input(termID, data string) error {
	p, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return fmt.Errorf("input must be base64: %w", err)
	}
	return b.queue(termID, func(c *client.Client) error { return c.TerminalInput(b.ctx, termID, p) })
}

// Resize queues a size change for a terminal.
func (b *Bridge) Resize(termID string, cols, rows int) error {
	return b.queue(termID, func(c *client.Client) error { return c.ResizeTerminal(b.ctx, termID, cols, rows) })
}

// queue runs f for the terminal after the requests queued before it.
// A worker per terminal sends them and exits once the queue is empty.
func (b *Bridge) queue(termID string, f func(*client.Client) error) error {
	if _, err := b.current(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	q, ok := b.inputs[termID]
	if !ok {
		q = make(chan func(*client.Client) error, inputQueue)
		b.inputs[termID] = q
		go b.send(termID, q)
	}
	select {
	case q <- f:
		return nil
	default:
		return errors.New("terminal input is backed up; try again")
	}
}

func (b *Bridge) send(termID string, q chan func(*client.Client) error) {
	for {
		b.mu.Lock()
		if len(q) == 0 {
			delete(b.inputs, termID)
			b.mu.Unlock()
			return
		}
		b.mu.Unlock()
		f := <-q
		if c, err := b.current(); err == nil {
			b.checked(f(c))
		}
	}
}

// Subscribe starts pushing a stream to the page: kind "events" (the
// activity stream) or "terminal" (id is the terminal). It returns the
// subscription id that payloads are emitted under.
func (b *Bridge) Subscribe(kind, id string) (string, error) {
	switch {
	case kind == "events":
	case kind == "terminal" && id != "":
	default:
		return "", fmt.Errorf("cannot subscribe to %q %q", kind, id)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.next++
	subID := "s" + strconv.Itoa(b.next)
	s := &sub{kind: kind, id: id}
	b.subs[subID] = s
	if b.client != nil {
		b.startLocked(subID, s, b.client)
	}
	return subID, nil
}

// Unsubscribe stops a subscription.
func (b *Bridge) Unsubscribe(subID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s := b.subs[subID]; s != nil {
		if s.cancel != nil {
			s.cancel()
		}
		delete(b.subs, subID)
	}
}

func (b *Bridge) current() (*client.Client, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.client == nil {
		return nil, instance.ErrNotRunning
	}
	return b.client, nil
}

// checked passes err on, first asking for a check if the server is gone.
func (b *Bridge) checked(err error) error {
	if errors.Is(err, instance.ErrNotRunning) {
		b.lost()
	}
	return err
}

// lost asks the watcher to check the server now.
func (b *Bridge) lost() {
	select {
	case b.check <- struct{}{}:
	default:
	}
}

// watch checks the server every Retry (or when a stream breaks):
// it notices the server going away or being replaced, reconnects, and
// restarts streams that are not running.
func (b *Bridge) watch() {
	defer close(b.done)
	retry := b.Retry
	if retry <= 0 {
		retry = 2 * time.Second
	}
	tick := time.NewTicker(retry)
	defer tick.Stop()
	for {
		select {
		case <-b.ctx.Done():
			b.disconnect(false)
			return
		case <-tick.C:
		case <-b.check:
		}
		info, err := instance.Discover(b.ctx, b.home)
		b.mu.Lock()
		cur := b.client
		b.mu.Unlock()
		switch {
		case err != nil:
			if cur != nil {
				b.disconnect(true)
			}
		case cur == nil || cur.Info != info:
			if cur != nil {
				b.disconnect(true)
			}
			b.connect(client.New(info, ClientName))
		default:
			b.mu.Lock()
			for id, s := range b.subs {
				if s.cancel == nil {
					b.startLocked(id, s, cur)
				}
			}
			b.mu.Unlock()
		}
	}
}

func (b *Bridge) connect(c *client.Client) {
	b.mu.Lock()
	b.client = c
	for id, s := range b.subs {
		b.startLocked(id, s, c)
	}
	b.mu.Unlock()
	b.emit(StatusChannel, map[string]any{"type": "connected", "addr": c.Info.Addr, "version": c.Info.Version, "pid": c.Info.PID})
}

// disconnect stops every stream; tell reports it to the page.
func (b *Bridge) disconnect(tell bool) {
	b.mu.Lock()
	b.client = nil
	for _, s := range b.subs {
		if s.cancel != nil {
			s.cancel()
			s.cancel = nil
		}
	}
	b.mu.Unlock()
	if tell {
		b.emit(StatusChannel, map[string]any{"type": "disconnected"})
	}
}

// startLocked runs a subscription's stream on c. b.mu is held.
func (b *Bridge) startLocked(subID string, s *sub, c *client.Client) {
	ctx, cancel := context.WithCancel(b.ctx)
	s.cancel = cancel
	go func() {
		var ended bool // the subscription is over (not cut off)
		if s.kind == "events" {
			b.streamEvents(ctx, subID, c)
		} else {
			ended = b.streamTerminal(ctx, subID, s.id, c)
		}
		b.mu.Lock()
		if b.subs[subID] == s && ctx.Err() == nil {
			if ended {
				delete(b.subs, subID)
			} else {
				s.cancel = nil
			}
		}
		b.mu.Unlock()
		cancel()
		if !ended && ctx.Err() == nil {
			b.lost()
		}
	}()
}

func (b *Bridge) streamEvents(ctx context.Context, subID string, c *client.Client) {
	ch, err := c.Events(ctx)
	if err != nil {
		return
	}
	for ev := range ch {
		b.emit(subID, map[string]any{"type": "event", "event": ev})
	}
}

// streamTerminal pushes a terminal's output; it reports whether the
// terminal ended (or no longer exists) rather than being cut off.
func (b *Bridge) streamTerminal(ctx context.Context, subID, id string, c *client.Client) bool {
	ch, err := c.TerminalStream(ctx, id)
	if errors.Is(err, client.ErrNoTerminal) {
		b.emit(subID, map[string]any{"type": "exit", "code": nil})
		return true
	}
	if err != nil {
		return false
	}
	first := true
	for ev := range ch {
		switch ev.Type {
		case "data":
			p := map[string]any{"type": "data", "data": base64.StdEncoding.EncodeToString(ev.Data)}
			if first {
				p["backlog"] = true
				first = false
			}
			b.emit(subID, p)
		case "gap":
			b.emit(subID, map[string]any{"type": "gap"})
		case "exit":
			b.emit(subID, map[string]any{"type": "exit", "code": ev.Code})
			return true
		}
	}
	return false
}

// emit pushes payload to the page on the UI thread.
func (b *Bridge) emit(subID string, payload any) {
	id, _ := json.Marshal(subID)
	p, err := json.Marshal(payload)
	if err != nil {
		return
	}
	js := "window.__cox&&window.__cox.emit(" + string(id) + "," + string(p) + ")"
	b.win.Dispatch(func() { b.win.Eval(js) })
}

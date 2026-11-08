package mcpbridge

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hua-ri/coxswain/internal/instance"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// proxy sits between the bridge and the server (server.json points at
// it) and can break the next tools/call in a chosen way.
type proxy struct {
	target *url.URL
	mode   atomic.Value // string: "", "missing", "cut", "eof" or "reject"; used once
	calls  atomic.Int32 // tools/call requests that reached the server

	mu      sync.Mutex
	streams map[*context.CancelFunc]bool // open GET streams
}

func startProxy(t *testing.T, home string) *proxy {
	t.Helper()
	info, err := instance.Read(home)
	if err != nil {
		t.Fatal(err)
	}
	p := &proxy{target: &url.URL{Scheme: "http", Host: info.Addr}, streams: map[*context.CancelFunc]bool{}}
	p.mode.Store("")
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	info.Addr = strings.TrimPrefix(srv.URL, "http://")
	if err := instance.Write(home, info); err != nil {
		t.Fatal(err)
	}
	return p
}

// closeStreams ends every open GET stream, as the server does when it
// closes a session.
func (p *proxy) closeStreams() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for cancel := range p.streams {
		(*cancel)()
	}
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	if r.Method == http.MethodPost && bytes.Contains(body, []byte(`"tools/call"`)) {
		switch p.mode.Swap("") {
		case "missing":
			// The session is gone: its stream ends, then the call gets 404.
			p.closeStreams()
			time.Sleep(100 * time.Millisecond)
			http.Error(w, "session not found", http.StatusNotFound)
			return
		case "eof":
			// A dead keep-alive connection: the request went out, no answer.
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		case "reject":
			// The server refuses the request outright.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"jsonrpc":"2.0","id":null,"error":{"code":-32602,"message":"bad request from proxy"}}`)
			return
		case "cut":
			// The server accepts the call, then the connection drops.
			p.calls.Add(1)
			out := r.Clone(r.Context())
			out.URL.Scheme, out.URL.Host, out.RequestURI, out.Host = p.target.Scheme, p.target.Host, "", ""
			resp, err := http.DefaultTransport.RoundTrip(out)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			resp.Body.Close()
			for k, v := range resp.Header {
				w.Header()[k] = v
			}
			w.WriteHeader(resp.StatusCode)
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		}
		p.calls.Add(1)
	}
	if r.Method == http.MethodGet {
		ctx, cancel := context.WithCancel(r.Context())
		p.mu.Lock()
		p.streams[&cancel] = true
		p.mu.Unlock()
		defer func() {
			p.mu.Lock()
			delete(p.streams, &cancel)
			p.mu.Unlock()
			cancel()
		}()
		r = r.WithContext(ctx)
	}
	rp := &httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) { pr.SetURL(p.target) }, FlushInterval: -1}
	rp.ServeHTTP(w, r)
}

// TestSessionGoneBeforeCall: the server dropped the session and the call
// is refused with 404 before running. The bridge reconnects and sends it
// once more, instead of saying the server is not running.
func TestSessionGoneBeforeCall(t *testing.T) {
	home := t.TempDir()
	startServer(t, home, newBlocker())
	p := startProxy(t, home)
	cs := agentSession(t, home)
	if got, isErr := echo(t, cs, "one"); isErr || got != "one" {
		t.Fatalf("%q %v", got, isErr)
	}
	p.mode.Store("missing")
	// Let the bridge see the dropped stream before the 404.
	writeFailed.Store(func() { time.Sleep(200 * time.Millisecond) })
	t.Cleanup(func() { writeFailed.Store(func() {}) })
	if got, isErr := echo(t, cs, "two"); isErr || got != "two" {
		t.Fatalf("after the session was dropped: %q %v", got, isErr)
	}
	if n := p.calls.Load(); n != 2 {
		t.Fatalf("calls that reached the server: %d", n)
	}
}

// TestConnectionLostAfterAccept: the server accepted the call, then the
// connection dropped. The call may have run, so it is not sent again; the
// agent is told the outcome is unknown (the server is still running).
func TestConnectionLostAfterAccept(t *testing.T) {
	home := t.TempDir()
	startServer(t, home, newBlocker())
	p := startProxy(t, home)
	cs := agentSession(t, home)
	if got, isErr := echo(t, cs, "one"); isErr || got != "one" {
		t.Fatalf("%q %v", got, isErr)
	}
	p.mode.Store("cut")
	_, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "demo_echo", Arguments: map[string]any{"msg": "two"}})
	if err == nil || !strings.Contains(err.Error(), "outcome is unknown") {
		t.Fatalf("got %v", err)
	}
	if n := p.calls.Load(); n != 2 {
		t.Fatalf("calls that reached the server: %d (resent?)", n)
	}
	if got, isErr := echo(t, cs, "three"); isErr || got != "three" {
		t.Fatalf("next call: %q %v", got, isErr)
	}
}

// TestServerGoneMidWrite: the call went out on a connection that died
// (the server stopped) and no server answers any more. The agent gets the
// usual offline answer, not a protocol error.
func TestServerGoneMidWrite(t *testing.T) {
	home := t.TempDir()
	startServer(t, home, newBlocker())
	p := startProxy(t, home)
	cs := agentSession(t, home)
	if got, isErr := echo(t, cs, "one"); isErr || got != "one" {
		t.Fatalf("%q %v", got, isErr)
	}
	p.mode.Store("eof")
	os.Remove(instance.Path(home)) // as if the server had stopped
	if got, isErr := echo(t, cs, "two"); !isErr || got != instance.ErrNotRunning.Error() {
		t.Fatalf("%q %v", got, isErr)
	}
}

// TestCallRejected: a call the server refuses (HTTP 400) did not run; the
// agent gets the server's error, and it is not sent again.
func TestCallRejected(t *testing.T) {
	home := t.TempDir()
	startServer(t, home, newBlocker())
	p := startProxy(t, home)
	cs := agentSession(t, home)
	echo(t, cs, "one")
	p.mode.Store("reject")
	_, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "demo_echo", Arguments: map[string]any{"msg": "two"}})
	if err == nil || !strings.Contains(err.Error(), "Bad Request") || strings.Contains(err.Error(), "outcome") {
		t.Fatalf("got %v", err)
	}
	if n := p.calls.Load(); n != 1 {
		t.Fatalf("calls that reached the server: %d", n)
	}
	if got, isErr := echo(t, cs, "three"); isErr || got != "three" {
		t.Fatalf("next call: %q %v", got, isErr)
	}
}

package k8s_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hua-ri/coxswain/internal/k8s"
	"github.com/hua-ri/coxswain/internal/k8s/k8stest"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
)

// fakeStream is one side of a stream; the pod side reads what the client
// writes and vice versa.
type fakeStream struct {
	h       http.Header
	r       *io.PipeReader // what the pod sent
	w       *io.PipeWriter // to the pod
	podR    *io.PipeReader
	podW    *io.PipeWriter
	mu      sync.Mutex
	reset   bool
	writeCl bool
}

func newFakeStream(h http.Header) *fakeStream {
	s := &fakeStream{h: h.Clone()}
	s.r, s.podW = io.Pipe()
	s.podR, s.w = io.Pipe()
	return s
}

func (s *fakeStream) Read(p []byte) (int, error)  { return s.r.Read(p) }
func (s *fakeStream) Write(p []byte) (int, error) { return s.w.Write(p) }
func (s *fakeStream) Close() error {
	s.mu.Lock()
	s.writeCl = true
	s.mu.Unlock()
	return s.w.Close()
}
func (s *fakeStream) Reset() error {
	s.mu.Lock()
	s.reset = true
	s.mu.Unlock()
	s.w.Close()
	return s.r.Close()
}
func (s *fakeStream) Headers() http.Header { return s.h }
func (s *fakeStream) Identifier() uint32   { return 0 }

type fakeConn struct {
	mu      sync.Mutex
	streams []*fakeStream
	removed int
	closed  chan bool
	once    sync.Once
}

func (c *fakeConn) CreateStream(h http.Header) (httpstream.Stream, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := newFakeStream(h)
	c.streams = append(c.streams, s)
	return s, nil
}
func (c *fakeConn) Close() error                 { c.once.Do(func() { close(c.closed) }); return nil }
func (c *fakeConn) CloseChan() <-chan bool       { return c.closed }
func (c *fakeConn) SetIdleTimeout(time.Duration) {}
func (c *fakeConn) RemoveStreams(s ...httpstream.Stream) {
	c.mu.Lock()
	c.removed += len(s)
	c.mu.Unlock()
}

func (c *fakeConn) stream(i int) *fakeStream {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.streams[i]
}

// TestForwardSession: each connection gets an error and a data stream for
// the port with one request id; data flows both ways; an error the pod
// reports goes to onError and ends the connection.
func TestForwardSession(t *testing.T) {
	conn := &fakeConn{closed: make(chan bool)}
	s := k8s.NewForwardSession(conn)
	errs := make(chan error, 1)
	c, err := s.Dial(8080, func(err error) { errs <- err })
	if err != nil {
		t.Fatal(err)
	}
	errStream, data := conn.stream(0), conn.stream(1)
	if errStream.h.Get(corev1.StreamType) != corev1.StreamTypeError || data.h.Get(corev1.StreamType) != corev1.StreamTypeData ||
		errStream.h.Get(corev1.PortHeader) != "8080" || data.h.Get(corev1.PortHeader) != "8080" ||
		data.h.Get(corev1.PortForwardRequestIDHeader) == "" ||
		data.h.Get(corev1.PortForwardRequestIDHeader) != errStream.h.Get(corev1.PortForwardRequestIDHeader) {
		t.Fatalf("headers %v %v", errStream.h, data.h)
	}
	go func() { io.Copy(data.podW, data.podR) }() // the pod echoes
	if _, err := io.WriteString(c, "ping"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("%q %v", buf, err)
	}
	if cw, ok := c.(interface{ CloseWrite() error }); !ok || cw.CloseWrite() != nil {
		t.Fatal("no half close")
	}

	c2, err := s.Dial(9090, func(err error) { errs <- err })
	if err != nil {
		t.Fatal(err)
	}
	if conn.stream(2).h.Get(corev1.PortForwardRequestIDHeader) == conn.stream(0).h.Get(corev1.PortForwardRequestIDHeader) {
		t.Fatal("request ids repeat")
	}
	io.WriteString(conn.stream(2).podW, "error forwarding port 9090: connection refused")
	conn.stream(2).podW.Close()
	select {
	case err := <-errs:
		if !strings.Contains(err.Error(), "connection refused") {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no error reported")
	}
	if _, err := io.ReadAll(c2); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("connection not ended: %v", err)
	}
	c.Close()
	c2.Close()

	select {
	case <-s.Done():
		t.Fatal("done before the connection closed")
	default:
	}
	conn.Close()
	<-s.Done()
	s.Close()
}

// TestForwarderRequest: port-forward goes to the pod's portforward
// subresource; a server that does not upgrade is an error.
func TestForwarderRequest(t *testing.T) {
	api := k8stest.NewAPIServer(t)
	c, err := apiKubeconfig(t, api).Client("fake")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Forward.Forward(context.Background(), "web", "p-1"); err == nil {
		t.Fatal("forward against a server without port-forward succeeded")
	}
	var found bool
	for _, r := range api.Requests() {
		if r.Path == "/api/v1/namespaces/web/pods/p-1/portforward" && r.Auth == "Bearer tok" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no portforward request: %+v", api.Requests())
	}
}

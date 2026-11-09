package k8s

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// Forwarder opens port-forward sessions to pods. Tests use a fake.
type Forwarder interface {
	Forward(ctx context.Context, namespace, pod string) (ForwardSession, error)
}

// ForwardSession is one port-forward connection to a pod; it carries any
// number of connections to the pod's ports.
type ForwardSession interface {
	// Dial opens a connection to port in the pod. An error the pod reports
	// afterwards (nothing listens on the port) ends the connection and is
	// passed to onError.
	Dial(port int, onError func(error)) (net.Conn, error)
	Done() <-chan struct{} // closed when the session ends
	Close() error
}

// SPDYForwarder port-forwards over the API server: WebSocket tunneling,
// falling back to SPDY for servers that do not support it.
type SPDYForwarder struct {
	Config *rest.Config // without a request timeout
}

func (f *SPDYForwarder) Forward(ctx context.Context, namespace, pod string) (ForwardSession, error) {
	cs, err := kubernetes.NewForConfig(f.Config)
	if err != nil {
		return nil, err
	}
	u := cs.CoreV1().RESTClient().Post().Resource("pods").Namespace(namespace).Name(pod).SubResource("portforward").URL()
	transport, upgrader, err := spdy.RoundTripperFor(f.Config)
	if err != nil {
		return nil, err
	}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, u)
	if ws, err := portforward.NewSPDYOverWebsocketDialer(u, f.Config); err == nil {
		dialer = portforward.NewFallbackDialer(ws, dialer, func(err error) bool {
			return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
		})
	}
	type result struct {
		conn httpstream.Connection
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		conn, _, err := dialer.Dial(portforward.PortForwardProtocolV1Name)
		ch <- result{conn, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, Explain(fmt.Errorf("port-forward to pod %s/%s: %w", namespace, pod, r.err))
		}
		return NewForwardSession(r.conn), nil
	case <-ctx.Done():
		go func() {
			if r := <-ch; r.conn != nil {
				r.conn.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

// NewForwardSession runs the port-forward protocol on conn.
func NewForwardSession(conn httpstream.Connection) ForwardSession {
	s := &forwardSession{conn: conn, done: make(chan struct{})}
	go func() {
		<-conn.CloseChan()
		close(s.done)
	}()
	return s
}

type forwardSession struct {
	conn  httpstream.Connection
	next  atomic.Int64
	done  chan struct{}
	close sync.Once
}

func (s *forwardSession) Done() <-chan struct{} { return s.done }

func (s *forwardSession) Close() error {
	var err error
	s.close.Do(func() { err = s.conn.Close() })
	return err
}

func (s *forwardSession) Dial(port int, onError func(error)) (net.Conn, error) {
	h := http.Header{}
	h.Set(corev1.StreamType, corev1.StreamTypeError)
	h.Set(corev1.PortHeader, strconv.Itoa(port))
	h.Set(corev1.PortForwardRequestIDHeader, strconv.FormatInt(s.next.Add(1), 10))
	errStream, err := s.conn.CreateStream(h)
	if err != nil {
		return nil, fmt.Errorf("port %d: %w", port, err)
	}
	errStream.Close() // only read
	h.Set(corev1.StreamType, corev1.StreamTypeData)
	data, err := s.conn.CreateStream(h)
	if err != nil {
		s.conn.RemoveStreams(errStream)
		return nil, fmt.Errorf("port %d: %w", port, err)
	}
	c := &streamConn{Stream: data, errStream: errStream, conn: s.conn}
	go func() {
		msg, err := io.ReadAll(errStream)
		if err == nil && len(msg) > 0 && onError != nil {
			onError(fmt.Errorf("%s", msg))
			data.Reset()
		}
	}()
	return c, nil
}

// streamConn is a port-forward data stream as a net.Conn.
type streamConn struct {
	httpstream.Stream
	errStream httpstream.Stream
	conn      httpstream.Connection
	once      sync.Once
}

// CloseWrite ends the direction toward the pod.
func (c *streamConn) CloseWrite() error { return c.Stream.Close() }

func (c *streamConn) Close() error {
	c.once.Do(func() {
		c.Stream.Reset()
		c.errStream.Reset()
		c.conn.RemoveStreams(c.Stream, c.errStream)
	})
	return nil
}

type pfAddr struct{}

func (pfAddr) Network() string { return "port-forward" }
func (pfAddr) String() string  { return "port-forward" }

func (c *streamConn) LocalAddr() net.Addr              { return pfAddr{} }
func (c *streamConn) RemoteAddr() net.Addr             { return pfAddr{} }
func (c *streamConn) SetDeadline(time.Time) error      { return nil }
func (c *streamConn) SetReadDeadline(time.Time) error  { return nil }
func (c *streamConn) SetWriteDeadline(time.Time) error { return nil }

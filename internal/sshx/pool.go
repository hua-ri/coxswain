package sshx

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/hua-ri/coxswain/internal/inventory"
)

// ErrPoolClosed is returned by Get after Close.
var ErrPoolClosed = errors.New("ssh connection pool closed")

// healthTimeout bounds the keepalive check before an idle connection is
// reused.
const healthTimeout = 3 * time.Second

// Pool keeps SSH connections open between commands. A connection is
// used by one caller at a time; idle ones are checked with a keepalive
// before reuse and closed after IdleTimeout.
type Pool struct {
	Dialer      *Dialer
	IdleTimeout time.Duration // default 5 minutes

	mu     sync.Mutex
	idle   map[string][]*idleConn
	closed bool
}

type idleConn struct {
	conn  *Conn
	timer *time.Timer
}

// key identifies where a connection goes.
func key(h inventory.Host) string {
	return h.Name + "\x00" + h.HostName + "\x00" + strconv.Itoa(h.Port) + "\x00" + h.User + "\x00" + h.ProxyJump
}

// Get returns a connection to h and a release func the caller must call
// once done, with broken=true if the connection failed.
func (p *Pool) Get(ctx context.Context, h inventory.Host) (*Conn, func(broken bool), error) {
	k := key(h)
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, nil, ErrPoolClosed
		}
		list := p.idle[k]
		if len(list) == 0 {
			p.mu.Unlock()
			break
		}
		ic := list[len(list)-1]
		p.idle[k] = list[:len(list)-1]
		p.mu.Unlock()
		if !ic.timer.Stop() {
			continue // the idle timer is closing it
		}
		if healthy(ic.conn) {
			return ic.conn, p.release(k, ic.conn), nil
		}
		ic.conn.Close()
	}
	c, err := p.Dialer.Dial(ctx, h)
	if err != nil {
		return nil, nil, err
	}
	return c, p.release(k, c), nil
}

func (p *Pool) release(k string, c *Conn) func(bool) {
	var once sync.Once
	return func(broken bool) {
		once.Do(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			if broken || p.closed {
				c.Close()
				return
			}
			if p.idle == nil {
				p.idle = map[string][]*idleConn{}
			}
			ic := &idleConn{conn: c}
			ic.timer = time.AfterFunc(p.idleTimeout(), func() { p.expire(k, ic) })
			p.idle[k] = append(p.idle[k], ic)
		})
	}
}

// expire closes an idle connection whose timer fired.
func (p *Pool) expire(k string, ic *idleConn) {
	p.mu.Lock()
	list := p.idle[k]
	for i, x := range list {
		if x == ic {
			p.idle[k] = append(list[:i:i], list[i+1:]...)
			break
		}
	}
	p.mu.Unlock()
	ic.conn.Close()
}

// Close closes every idle connection; connections in use are closed when
// released.
func (p *Pool) Close() error {
	p.mu.Lock()
	p.closed = true
	idle := p.idle
	p.idle = nil
	p.mu.Unlock()
	for _, list := range idle {
		for _, ic := range list {
			ic.timer.Stop()
			ic.conn.Close()
		}
	}
	return nil
}

func (p *Pool) idleTimeout() time.Duration {
	if p.IdleTimeout > 0 {
		return p.IdleTimeout
	}
	return 5 * time.Minute
}

// healthy sends an OpenSSH keepalive and waits for any answer.
func healthy(c *Conn) bool {
	done := make(chan error, 1)
	go func() {
		_, _, err := c.SendRequest("keepalive@openssh.com", true, nil)
		done <- err
	}()
	select {
	case err := <-done:
		return err == nil
	case <-time.After(healthTimeout):
		return false
	}
}

package sshx

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hua-ri/coxswain/internal/inventory"
	"github.com/hua-ri/coxswain/internal/sshx/sshtest"
)

func poolFor(t *testing.T, idle time.Duration) (*Pool, inventory.Host, *sshtest.Server) {
	t.Helper()
	srv := sshtest.Start(t)
	p := &Pool{Dialer: &Dialer{KnownHosts: []string{srv.KnownHosts}}, IdleTimeout: idle}
	t.Cleanup(func() { p.Close() })
	h := inventory.Host{Name: "box", HostName: srv.Host, Port: srv.Port, User: srv.User, IdentityFiles: []string{srv.KeyFile}}
	return p, h, srv
}

func execOn(t *testing.T, p *Pool, h inventory.Host) {
	t.Helper()
	c, release, err := p.Get(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Exec(context.Background(), c.Client, "echo ok", 1<<10)
	release(err != nil)
	if err != nil || res.Stdout != "ok\n" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestPoolReuses(t *testing.T) {
	p, h, srv := poolFor(t, time.Minute)
	execOn(t, p, h)
	execOn(t, p, h)
	if n := srv.Accepts.Load(); n != 1 {
		t.Fatalf("%d connections, want 1", n)
	}
	other := h
	other.Name = "alias" // a different host entry gets its own connection
	execOn(t, p, other)
	if n := srv.Accepts.Load(); n != 2 {
		t.Fatalf("%d connections, want 2", n)
	}
}

func TestPoolReplacesBroken(t *testing.T) {
	p, h, srv := poolFor(t, time.Minute)
	execOn(t, p, h)
	srv.DropAll() // the idle connection dies
	execOn(t, p, h)
	if n := srv.Accepts.Load(); n != 2 {
		t.Fatalf("%d connections, want 2", n)
	}

	c, release, err := p.Get(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	release(true) // the caller saw it fail
	waitClosed(t, c)
	execOn(t, p, h)
	if n := srv.Accepts.Load(); n != 3 {
		t.Fatalf("%d connections after a broken release, want 3", n)
	}
}

func TestPoolIdleTimeout(t *testing.T) {
	p, h, srv := poolFor(t, 50*time.Millisecond)
	c, release, err := p.Get(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	release(false)
	waitClosed(t, c)
	execOn(t, p, h)
	if n := srv.Accepts.Load(); n != 2 {
		t.Fatalf("%d connections, want 2", n)
	}
}

func TestPoolClose(t *testing.T) {
	p, h, _ := poolFor(t, time.Minute)
	idle, release, err := p.Get(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	busy, releaseBusy, err := p.Get(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	release(false)
	p.Close()
	waitClosed(t, idle)
	releaseBusy(false) // released after Close: closed, not kept
	waitClosed(t, busy)
	if _, _, err := p.Get(context.Background(), h); err == nil {
		t.Fatal("Get after Close")
	}
}

func waitClosed(t *testing.T, c *Conn) {
	t.Helper()
	done := make(chan struct{})
	go func() { c.Client.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("connection not closed")
	}
}

func TestPoolKeyIncludesIdentity(t *testing.T) {
	p, h, srv := poolFor(t, time.Minute)
	execOn(t, p, h)
	key, _ := os.ReadFile(h.IdentityFiles[0])
	other := filepath.Join(t.TempDir(), "id_copy")
	os.WriteFile(other, key, 0o600)
	h2 := h
	h2.IdentityFiles = []string{other}
	execOn(t, p, h2)
	if n := srv.Accepts.Load(); n != 2 {
		t.Fatalf("%d connections: a different identity must not share one", n)
	}
}

func TestPoolIdleCap(t *testing.T) {
	p, h, _ := poolFor(t, time.Minute)
	var conns []*Conn
	var releases []func(bool)
	for range 3 {
		c, release, err := p.Get(context.Background(), h)
		if err != nil {
			t.Fatal(err)
		}
		conns, releases = append(conns, c), append(releases, release)
	}
	for _, r := range releases {
		r(false)
	}
	waitClosed(t, conns[2]) // only two stay idle
	p.mu.Lock()
	n := len(p.idle[key(h)])
	p.mu.Unlock()
	if n != maxIdlePerHost {
		t.Fatalf("%d idle", n)
	}
}

func TestPoolConcurrentGet(t *testing.T) {
	p, h, srv := poolFor(t, time.Minute)
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, release, err := p.Get(context.Background(), h)
			if err != nil {
				t.Error(err)
				return
			}
			_, err = Exec(context.Background(), c.Client, "true", 1<<10)
			release(err != nil)
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := srv.Accepts.Load(); n < 1 || n > 10 {
		t.Fatalf("%d connections", n)
	}
	execOn(t, p, h) // still usable
}

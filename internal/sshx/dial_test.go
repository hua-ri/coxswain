package sshx

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hua-ri/coxswain/internal/inventory"
	"github.com/hua-ri/coxswain/internal/sshx/sshtest"
)

func TestDialAndExec(t *testing.T) {
	srv := sshtest.Start(t)
	d := &Dialer{KnownHosts: []string{srv.KnownHosts}}
	h := inventory.Host{Name: "box", HostName: srv.Host, Port: srv.Port, User: srv.User, IdentityFiles: []string{srv.KeyFile}}
	c, err := d.Dial(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	res, err := Exec(context.Background(), c.Client, "echo out; echo err >&2; exit 3", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "out\n" || res.Stderr != "err\n" || res.ExitCode != 3 {
		t.Fatalf("res = %+v", res)
	}
}

func TestExecCancel(t *testing.T) {
	srv := sshtest.Start(t)
	d := &Dialer{KnownHosts: []string{srv.KnownHosts}}
	c, err := d.Dial(context.Background(), inventory.Host{Name: "box", HostName: srv.Host, Port: srv.Port, IdentityFiles: []string{srv.KeyFile}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := Exec(ctx, c.Client, "sleep 10", 1024); err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("err = %v after %v", err, time.Since(start))
	}
}

func TestDialThroughJump(t *testing.T) {
	jump, target := sshtest.Start(t), sshtest.Start(t)
	store := inventory.NewStore(filepath.Join(t.TempDir(), "hosts.json"))
	store.Merge([]inventory.Host{{Name: "jump", HostName: jump.Host, Port: jump.Port, IdentityFiles: []string{jump.KeyFile}}})
	d := &Dialer{Hosts: store, KnownHosts: []string{jump.KnownHosts, target.KnownHosts}}
	c, err := d.Dial(context.Background(), inventory.Host{
		Name: "target", HostName: target.Host, Port: target.Port, IdentityFiles: []string{target.KeyFile}, ProxyJump: "jump",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	res, err := Exec(context.Background(), c.Client, "echo via-jump", 1024)
	if err != nil || res.Stdout != "via-jump\n" {
		t.Fatalf("res = %+v %v", res, err)
	}
	if len(c.jumps) != 1 {
		t.Fatalf("jumps = %d", len(c.jumps))
	}
}

func TestUnknownHostKeyIsExplained(t *testing.T) {
	srv := sshtest.Start(t)
	other := sshtest.Start(t) // its known_hosts does not contain srv's key
	d := &Dialer{KnownHosts: []string{other.KnownHosts}}
	_, err := d.Dial(context.Background(), inventory.Host{Name: "box", HostName: srv.Host, Port: srv.Port, IdentityFiles: []string{srv.KeyFile}})
	if err == nil || !strings.Contains(err.Error(), "not in known_hosts") {
		t.Fatalf("err = %v", err)
	}
}

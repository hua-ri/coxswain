package sshx

import (
	"path/filepath"
	"testing"

	"github.com/hua-ri/coxswain/internal/inventory"
)

func TestChainResolvesAliasesAndSpecs(t *testing.T) {
	store := inventory.NewStore(filepath.Join(t.TempDir(), "hosts.json"))
	store.Merge([]inventory.Host{
		{Name: "outer", HostName: "1.1.1.1", Port: 22},
		{Name: "bastion", HostName: "10.0.0.1", Port: 22, ProxyJump: "outer"},
	})
	d := &Dialer{Hosts: store}
	hops, err := d.chain(inventory.Host{Name: "web", HostName: "10.0.1.1", ProxyJump: "bastion"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, h := range hops {
		got = append(got, h.Name)
	}
	if len(got) != 3 || got[0] != "outer" || got[1] != "bastion" || got[2] != "web" {
		t.Fatalf("chain = %v", got)
	}

	hop, err := d.resolveJump("ops@jump.example.com:2200")
	if err != nil || hop.User != "ops" || hop.HostName != "jump.example.com" || hop.Port != 2200 {
		t.Fatalf("spec = %+v %v", hop, err)
	}
}

func TestCapped(t *testing.T) {
	c := &capped{max: 5}
	c.Write([]byte("abc"))
	c.Write([]byte("defgh"))
	if c.String() != "abcde" || !c.over {
		t.Fatalf("got %q over=%v", c.String(), c.over)
	}
}

func TestNoKnownHosts(t *testing.T) {
	d := &Dialer{KnownHosts: []string{filepath.Join(t.TempDir(), "missing")}}
	if _, err := d.clientConfig(inventory.Host{Name: "x", HostName: "x"}, "x:22"); err == nil {
		t.Fatal("expected error without known_hosts")
	}
}

package client

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hua-ri/coxswain/internal/inventory"
	"github.com/hua-ri/coxswain/internal/server"
	"github.com/hua-ri/coxswain/internal/sshx"
	"github.com/hua-ri/coxswain/internal/sshx/sshtest"
	"github.com/hua-ri/coxswain/internal/tunnel"
)

func TestTunnels(t *testing.T) {
	srv := sshtest.Start(t)
	hosts := inventory.NewStore(filepath.Join(t.TempDir(), "hosts.json"))
	hosts.Merge([]inventory.Host{{Name: "dev", HostName: srv.Host, Port: srv.Port, IdentityFiles: []string{srv.KeyFile}}})
	var m *tunnel.Manager
	c, _ := setupWith(t, func(cfg *server.Config) {
		m = &tunnel.Manager{Hosts: hosts, Dialer: &sshx.Dialer{Hosts: hosts, KnownHosts: []string{srv.KnownHosts}}, Events: cfg.Engine.Events}
		cfg.Tunnels = m
	})
	t.Cleanup(m.CloseAll)
	ctx := context.Background()
	tn, err := c.OpenTunnel(ctx, tunnel.Spec{Host: "dev", Type: tunnel.Socks})
	if err != nil || tn.Owner.Name != "cli" || !strings.HasPrefix(tn.Listen, "127.0.0.1:") {
		t.Fatalf("%+v %v", tn, err)
	}
	list, err := c.Tunnels(ctx)
	if err != nil || len(list) != 1 || list[0].ID != tn.ID {
		t.Fatalf("%+v %v", list, err)
	}
	if got, err := c.Tunnel(ctx, tn.ID); err != nil || got.ID != tn.ID {
		t.Fatalf("%+v %v", got, err)
	}
	if err := c.CloseTunnel(ctx, tn.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.CloseTunnel(ctx, tn.ID); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("close twice: %v", err)
	}
	if _, err := c.OpenTunnel(ctx, tunnel.Spec{Host: "dev", Type: "x"}); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("invalid: %v", err)
	}
}

package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hua-ri/coxswain/internal/inventory"
	"github.com/hua-ri/coxswain/internal/sshx"
	"github.com/hua-ri/coxswain/internal/sshx/sshtest"
	"github.com/hua-ri/coxswain/internal/tunnel"
)

// withTunnels gives the fixture a tunnel manager with one host, dev, and
// returns the port of an echo server.
func withTunnels(t *testing.T, f *fixture) int {
	t.Helper()
	srv := sshtest.Start(t)
	hosts := inventory.NewStore(filepath.Join(t.TempDir(), "hosts.json"))
	hosts.Merge([]inventory.Host{{Name: "dev", HostName: srv.Host, Port: srv.Port, IdentityFiles: []string{srv.KeyFile}}})
	f.cfg.Tunnels = &tunnel.Manager{
		Hosts:  hosts,
		Dialer: &sshx.Dialer{Hosts: hosts, KnownHosts: []string{srv.KnownHosts}},
		Events: f.cfg.Events,
	}
	t.Cleanup(f.cfg.Tunnels.CloseAll)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestTunnelsAPI(t *testing.T) {
	f := newConfig(t)
	port := withTunnels(t, &f)
	h := New(f.cfg)
	desktop := map[string]string{"X-Cox-Client": "desktop"}

	w := request(t, h, "POST", "/api/v1/tunnels", fmt.Sprintf(`{"host":"dev","type":"local","remote_port":%d}`, port), desktop)
	var tn tunnel.Tunnel
	if err := json.Unmarshal(w.Body.Bytes(), &tn); w.Code != 201 || err != nil {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if tn.Owner.Name != "desktop" || tn.Owner.Kind != "human" || tn.IdleMinutes != 0 || !strings.HasPrefix(tn.Listen, "127.0.0.1:") {
		t.Fatalf("%+v", tn)
	}
	c, err := net.Dial("tcp", tn.Listen)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()

	w = request(t, h, "GET", "/api/v1/tunnels", "", nil)
	var list []tunnel.Tunnel
	if err := json.Unmarshal(w.Body.Bytes(), &list); w.Code != 200 || err != nil || len(list) != 1 || list[0].ID != tn.ID {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w := request(t, h, "GET", "/api/v1/tunnels/"+tn.ID, "", nil); w.Code != 200 || !strings.Contains(w.Body.String(), tn.ID) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}

	for _, c := range []struct {
		body string
		code int
	}{
		{`{"host":"dev","type":"bogus"}`, 400},
		{`{"host":"nope","type":"socks"}`, 404},
		{`not json`, 400},
		{fmt.Sprintf(`{"host":"dev","type":"local","local_port":%d,"remote_port":%d}`, port, port), 409}, // port in use
	} {
		if w := request(t, h, "POST", "/api/v1/tunnels", c.body, desktop); w.Code != c.code {
			t.Errorf("%s: %d %s", c.body, w.Code, w.Body)
		}
	}

	// Agents may look (once allowed) but not open or close.
	codex := map[string]string{"X-Cox-Client": "codex"}
	if w := request(t, h, "GET", "/api/v1/tunnels", "", codex); w.Code != 403 {
		t.Fatalf("unauthorized agent: %d", w.Code)
	}
	f.cfg.Clients.Allow("codex")
	if w := request(t, h, "GET", "/api/v1/tunnels", "", codex); w.Code != 200 {
		t.Fatalf("authorized agent: %d", w.Code)
	}
	if w := request(t, h, "POST", "/api/v1/tunnels", `{"host":"dev","type":"socks"}`, codex); w.Code != 403 {
		t.Fatalf("agent opened: %d", w.Code)
	}
	if w := request(t, h, "DELETE", "/api/v1/tunnels/"+tn.ID, "", codex); w.Code != 403 {
		t.Fatalf("agent closed: %d", w.Code)
	}

	if w := request(t, h, "DELETE", "/api/v1/tunnels/"+tn.ID, "", desktop); w.Code != 204 {
		t.Fatalf("close: %d %s", w.Code, w.Body)
	}
	if w := request(t, h, "DELETE", "/api/v1/tunnels/"+tn.ID, "", desktop); w.Code != 404 {
		t.Fatalf("close twice: %d", w.Code)
	}
	if w := request(t, h, "GET", "/api/v1/tunnels/"+tn.ID, "", nil); w.Code != 404 {
		t.Fatalf("get closed: %d", w.Code)
	}
}

func TestRunClosesTunnels(t *testing.T) {
	f := newConfig(t)
	port := withTunnels(t, &f)
	f.cfg.Token = ""
	_, stop := runReady(t, f.cfg, t.TempDir())
	if _, err := f.cfg.Tunnels.Open(t.Context(), tunnel.Spec{Host: "dev", Type: tunnel.Local, RemotePort: port}, Actor("cli")); err != nil {
		t.Fatal(err)
	}
	stop()
	if list := f.cfg.Tunnels.List(); len(list) != 0 {
		t.Fatalf("%+v", list)
	}
}

// TestRevokeClosesTunnels: a revoked agent's tunnels close (their ports
// would otherwise still be usable); others' stay.
func TestRevokeClosesTunnels(t *testing.T) {
	f := newConfig(t)
	port := withTunnels(t, &f)
	New(f.cfg)
	spec := tunnel.Spec{Host: "dev", Type: tunnel.Local, RemotePort: port}
	mine, _ := f.cfg.Tunnels.Open(t.Context(), spec, Actor("desktop"))
	if _, err := f.cfg.Tunnels.Open(t.Context(), spec, Actor("codex")); err != nil {
		t.Fatal(err)
	}
	f.cfg.Clients.Revoke("codex")
	if list := f.cfg.Tunnels.List(); len(list) != 1 || list[0].ID != mine.ID {
		t.Fatalf("%+v", list)
	}
}

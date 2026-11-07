package instance

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestWriteReadRemove(t *testing.T) {
	home := t.TempDir() + "/cox"
	in := Info{Addr: "127.0.0.1:4242", PID: 77, Version: "v1", StartedAt: time.Now().UTC().Truncate(time.Second), Token: "tok"}
	if err := Write(home, in); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(Path(home))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", st.Mode(), err)
	}
	got, err := Read(home)
	if err != nil || got != in {
		t.Fatalf("got %+v %v", got, err)
	}
	b, _ := os.ReadFile(Path(home))
	for _, k := range []string{`"addr"`, `"pid"`, `"version"`, `"started_at"`, `"token"`} {
		if !strings.Contains(string(b), k) {
			t.Fatalf("missing %s in %s", k, b)
		}
	}

	if err := Remove(home, 78); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(Path(home)); err != nil {
		t.Fatal("Remove deleted another server's file")
	}
	if err := Remove(home, 77); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(home); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("after remove: %v", err)
	}
	if err := Remove(home, 77); err != nil {
		t.Fatalf("removing a missing file: %v", err)
	}
}

func TestReadMissing(t *testing.T) {
	_, err := Read(t.TempDir())
	if !errors.Is(err, ErrNotRunning) || err.Error() != "Coxswain server is not running. Start it with: cox server" {
		t.Fatalf("%v", err)
	}
}

func healthz(h Health) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(h)
	}))
}

func TestProbe(t *testing.T) {
	srv := healthz(Health{Version: "v1", PID: 77})
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	ctx := context.Background()
	if err := Probe(ctx, Info{Addr: addr, PID: 77, Version: "v1"}); err != nil {
		t.Fatal(err)
	}
	cases := map[string]Info{
		"other pid":     {Addr: addr, PID: 78, Version: "v1"},
		"other version": {Addr: addr, PID: 77, Version: "v2"},
		"nothing there": {Addr: "127.0.0.1:1", PID: 77, Version: "v1"},
	}
	for name, in := range cases {
		if err := Probe(ctx, in); !errors.Is(err, ErrNotRunning) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestProbeTimesOut(t *testing.T) {
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-stop }))
	defer srv.Close()
	defer close(stop)
	start := time.Now()
	err := Probe(context.Background(), Info{Addr: strings.TrimPrefix(srv.URL, "http://"), PID: 1})
	if !errors.Is(err, ErrNotRunning) || time.Since(start) > 3*time.Second {
		t.Fatalf("%v after %v", err, time.Since(start))
	}
}

func TestDiscover(t *testing.T) {
	srv := healthz(Health{Version: "v1", PID: 77})
	defer srv.Close()
	home := t.TempDir()
	if _, err := Discover(context.Background(), home); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("no file: %v", err)
	}
	in := Info{Addr: strings.TrimPrefix(srv.URL, "http://"), PID: 77, Version: "v1", Token: "t"}
	Write(home, in)
	if got, err := Discover(context.Background(), home); err != nil || got.Token != "t" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestProbeErrorMessage(t *testing.T) {
	err := Probe(context.Background(), Info{Addr: "127.0.0.1:1"})
	var nr *NotRunningError
	if err.Error() != ErrNotRunning.Error() || !errors.As(err, &nr) || nr.Cause == nil {
		t.Fatalf("%v", err)
	}
}

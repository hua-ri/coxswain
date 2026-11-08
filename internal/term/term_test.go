package term

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/eventlog"
	"github.com/hua-ri/coxswain/internal/inventory"
	"github.com/hua-ri/coxswain/internal/sshx"
	"github.com/hua-ri/coxswain/internal/sshx/sshtest"
)

var (
	codex = action.Actor{Kind: action.Agent, Name: "codex"}
	human = action.Actor{Kind: action.Human, Name: "desktop"}
)

func setup(t *testing.T) (*Manager, *sshtest.Server) {
	t.Helper()
	srv := sshtest.Start(t)
	dir := t.TempDir()
	hosts := inventory.NewStore(filepath.Join(dir, "hosts.json"))
	hosts.Merge([]inventory.Host{{Name: "dev", HostName: srv.Host, Port: srv.Port, IdentityFiles: []string{srv.KeyFile}}})
	hosts.Update(func(m map[string]inventory.Host) error {
		h := m["dev"]
		h.Tags = map[string]string{"env": "dev"}
		m["dev"] = h
		return nil
	})
	m := &Manager{
		Hosts:  hosts,
		Dialer: &sshx.Dialer{Hosts: hosts, KnownHosts: []string{srv.KnownHosts}},
		Events: eventlog.Open(filepath.Join(dir, "events")),
	}
	t.Cleanup(m.CloseAll)
	return m, srv
}

// waitFor reads chunks until the collected output contains want.
func waitFor(t *testing.T, ch <-chan Chunk, want string) string {
	t.Helper()
	var got bytes.Buffer
	deadline := time.After(5 * time.Second)
	for !strings.Contains(got.String(), want) {
		select {
		case c, ok := <-ch:
			if !ok {
				t.Fatalf("stream ended; got %q", got.String())
			}
			got.Write(c.Data)
		case <-deadline:
			t.Fatalf("timed out waiting for %q; got %q", want, got.String())
		}
	}
	return got.String()
}

// waitExit reads chunks until the exit chunk and the end of the stream.
func waitExit(t *testing.T, ch <-chan Chunk) Chunk {
	t.Helper()
	var exit Chunk
	deadline := time.After(5 * time.Second)
	for {
		select {
		case c, ok := <-ch:
			if !ok {
				if !exit.Exit {
					t.Fatal("stream ended without an exit chunk")
				}
				return exit
			}
			if c.Exit {
				exit = c
			}
		case <-deadline:
			t.Fatal("no exit")
		}
	}
}

func TestOpenWriteSubscribe(t *testing.T) {
	m, srv := setup(t)
	s, err := m.OpenHost(context.Background(), "dev", codex, 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s.ID, "t_") || len(s.ID) != 14 || s.Host != "dev" || s.Owner != codex ||
		s.Cols != 100 || s.Rows != 30 || s.Closed || s.CreatedAt.IsZero() {
		t.Fatalf("%+v", s)
	}
	if got := <-srv.Sizes; got != (sshtest.Size{Term: "xterm-256color", Cols: 100, Rows: 30}) {
		t.Fatalf("pty-req %+v", got)
	}
	_, ch, cancel, err := m.Subscribe(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if err := m.Write(s.ID, []byte("echo hello\r")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, "hello\n")

	// A later subscriber gets what was said so far.
	backlog, _, cancel2, err := m.Subscribe(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	cancel2()
	cancel2() // twice is fine
	if !strings.Contains(string(backlog), "hello\n") {
		t.Fatalf("backlog %q", backlog)
	}
	if txt, err := m.Text(s.ID, 10); err != nil || txt != "hello" {
		t.Fatalf("text %q %v", txt, err)
	}

	if err := m.Resize(s.ID, 120, 40); err != nil {
		t.Fatal(err)
	}
	if got := <-srv.Sizes; got != (sshtest.Size{Cols: 120, Rows: 40}) {
		t.Fatalf("window-change %+v", got)
	}
	if list := m.List(); len(list) != 1 || list[0].Cols != 120 || list[0].Rows != 40 {
		t.Fatalf("%+v", list)
	}
	if err := m.Resize(s.ID, 0, 40); !errors.Is(err, ErrSize) {
		t.Fatalf("bad size: %v", err)
	}

	evs, _ := m.Events.Read(time.Now())
	if len(evs) != 1 || evs[0].Type != eventlog.TerminalOpened || evs[0].Terminal != s.ID || *evs[0].Actor != codex ||
		len(evs[0].Targets) != 1 || evs[0].Targets[0].ID != "dev" || evs[0].Targets[0].Tags["env"] != "dev" {
		t.Fatalf("%+v", evs)
	}
}

func TestRemoteExit(t *testing.T) {
	m, _ := setup(t)
	m.Retain = 100 * time.Millisecond
	s, err := m.OpenHost(context.Background(), "dev", human, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.Cols != 80 || s.Rows != 24 {
		t.Fatalf("default size %+v", s)
	}
	_, ch, cancel, _ := m.Subscribe(s.ID)
	defer cancel()
	m.Write(s.ID, []byte("echo bye; exit 3\r"))
	if c := waitExit(t, ch); c.Code == nil || *c.Code != 3 {
		t.Fatalf("exit %+v", c)
	}
	got, ok := m.Get(s.ID)
	if !ok || !got.Closed || got.ExitCode == nil || *got.ExitCode != 3 {
		t.Fatalf("%+v %v", got, ok)
	}
	if err := m.Write(s.ID, []byte("x")); !errors.Is(err, ErrClosed) {
		t.Fatalf("write after exit: %v", err)
	}
	// The output stays readable for a while.
	if txt, _ := m.Text(s.ID, 5); txt != "bye" {
		t.Fatalf("text %q", txt)
	}
	backlog, ch2, _, err := m.Subscribe(s.ID)
	if err != nil || !strings.Contains(string(backlog), "bye") {
		t.Fatalf("%q %v", backlog, err)
	}
	if c := waitExit(t, ch2); *c.Code != 3 {
		t.Fatalf("%+v", c)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := m.Get(s.ID); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("closed session not reclaimed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	evs, _ := m.Events.Read(time.Now())
	last := evs[len(evs)-1]
	if last.Type != eventlog.TerminalClosed || last.Terminal != s.ID || last.ExitCode == nil || *last.ExitCode != 3 || *last.Actor != human {
		t.Fatalf("%+v", last)
	}
}

func TestClose(t *testing.T) {
	m, _ := setup(t)
	s, err := m.OpenHost(context.Background(), "dev", codex, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	_, ch, cancel, _ := m.Subscribe(s.ID)
	defer cancel()
	if err := m.Close(s.ID, human); err != nil {
		t.Fatal(err)
	}
	if c := waitExit(t, ch); c.Code != nil {
		t.Fatalf("%+v", c)
	}
	if _, ok := m.Get(s.ID); ok {
		t.Fatal("closed session still listed")
	}
	if err := m.Close(s.ID, human); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second close: %v", err)
	}
	for _, err := range []error{m.Write("t_000000000000", nil), m.Resize("nope", 80, 24)} {
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("%v", err)
		}
	}
	if _, _, _, err := m.Subscribe("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	evs, _ := m.Events.Read(time.Now())
	last := evs[len(evs)-1]
	if last.Type != eventlog.TerminalClosed || *last.Actor != codex || last.Reason != "closed by human:desktop" {
		t.Fatalf("%+v", last)
	}
}

func TestOpenErrors(t *testing.T) {
	m, _ := setup(t)
	if _, err := m.OpenHost(context.Background(), "nope", codex, 80, 24); !errors.Is(err, inventory.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if _, err := m.OpenHost(context.Background(), "dev", codex, 80, 5000); !errors.Is(err, ErrSize) {
		t.Fatalf("%v", err)
	}
	m.MaxOpen = 1
	if _, err := m.OpenHost(context.Background(), "dev", codex, 80, 24); err != nil {
		t.Fatal(err)
	}
	if _, err := m.OpenHost(context.Background(), "dev", codex, 80, 24); !errors.Is(err, ErrTooMany) {
		t.Fatalf("%v", err)
	}
}

func TestCloseAll(t *testing.T) {
	m, _ := setup(t)
	a, _ := m.OpenHost(context.Background(), "dev", codex, 80, 24)
	b, _ := m.OpenHost(context.Background(), "dev", human, 80, 24)
	_, ch, _, _ := m.Subscribe(a.ID)
	m.CloseAll()
	waitExit(t, ch)
	if list := m.List(); len(list) != 0 {
		t.Fatalf("%+v", list)
	}
	evs, _ := m.Events.Read(time.Now())
	closed := 0
	for _, ev := range evs {
		if ev.Type == eventlog.TerminalClosed && (ev.Terminal == a.ID || ev.Terminal == b.ID) && ev.Reason == "server stopped" {
			closed++
		}
	}
	if closed != 2 {
		t.Fatalf("%+v", evs)
	}
}

// TestSlowSubscriberGap: a subscriber that falls behind loses the oldest
// chunks, and the next chunk it gets says so.
func TestSlowSubscriberGap(t *testing.T) {
	s := &session{}
	_, ch, cancel := s.subscribe()
	defer cancel()
	for i := range subscriberBuffer + 10 {
		s.output([]byte{byte(i)})
	}
	first := <-ch
	if !first.Gap && first.Data[0] == 0 {
		t.Fatal("oldest chunk kept")
	}
	gaps := 0
	n := 1
	for len(ch) > 0 {
		if c := <-ch; c.Gap {
			gaps++
		}
		n++
	}
	if n != subscriberBuffer || gaps == 0 {
		t.Fatalf("n=%d gaps=%d", n, gaps)
	}
}

func TestBacklogBounded(t *testing.T) {
	s := &session{}
	big := bytes.Repeat([]byte("x"), backlogSize)
	s.output(big)
	s.output([]byte("tail"))
	backlog, _, cancel := s.subscribe()
	cancel()
	if len(backlog) != backlogSize || !bytes.HasSuffix(backlog, []byte("tail")) || !s.wrapped {
		t.Fatalf("len %d", len(backlog))
	}
}

package term

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/eventlog"
)

// fakePods opens fake pod shells that echo input and record resizes.
type fakePods struct {
	mu     sync.Mutex
	shells []*fakeShell
	err    error
}

func (f *fakePods) OpenPod(_ context.Context, spec PodSpec, cols, rows int, out io.Writer) (Backend, PodSpec, error) {
	if f.err != nil {
		return nil, spec, f.err
	}
	if spec.Namespace == "" {
		spec.Namespace = "default"
	}
	if spec.Container == "" {
		spec.Container = "app"
	}
	sh := &fakeShell{out: out, done: make(chan struct{}), sizes: [][2]int{{cols, rows}}}
	f.mu.Lock()
	f.shells = append(f.shells, sh)
	f.mu.Unlock()
	io.WriteString(out, "$ ")
	return sh, spec, nil
}

func (f *fakePods) Target(ctx, ns string) action.Target {
	return action.Target{Kind: "k8s", ID: "k8s:" + ctx + "/" + ns, Tags: map[string]string{"env": "prod"}}
}

type fakeShell struct {
	out   io.Writer
	mu    sync.Mutex
	sizes [][2]int
	once  sync.Once
	done  chan struct{}
	code  *int
}

func (s *fakeShell) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "exit 4") {
		code := 4
		s.end(&code)
		return len(p), nil
	}
	s.out.Write(p)
	return len(p), nil
}

func (s *fakeShell) Resize(cols, rows int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sizes = append(s.sizes, [2]int{cols, rows})
	return nil
}

func (s *fakeShell) end(code *int) {
	s.once.Do(func() {
		s.mu.Lock()
		s.code = code
		s.mu.Unlock()
		close(s.done)
	})
}

func (s *fakeShell) Wait() *int {
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.code
}

func (s *fakeShell) Close() error { s.end(nil); return nil }

func setupPods(t *testing.T) (*Manager, *fakePods, <-chan eventlog.Event) {
	t.Helper()
	events := eventlog.Open(filepath.Join(t.TempDir(), "events"))
	ch, cancel := events.Subscribe()
	t.Cleanup(cancel)
	pods := &fakePods{}
	m := &Manager{Pods: pods, Events: events}
	t.Cleanup(m.CloseAll)
	return m, pods, ch
}

func nextEvent(t *testing.T, ch <-chan eventlog.Event, typ string) eventlog.Event {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Type == typ {
				return ev
			}
		case <-timeout:
			t.Fatalf("no %s event", typ)
		}
	}
}

func TestPodTerminal(t *testing.T) {
	m, pods, events := setupPods(t)
	s, err := m.OpenPod(context.Background(), PodSpec{Context: "kind", Pod: "web-1", Shell: "bash"}, codex, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.Kind != KindPod || s.Context != "kind" || s.Namespace != "default" || s.Pod != "web-1" || s.Container != "app" ||
		s.Host != "" || s.Cols != 80 || s.Rows != 24 {
		t.Fatalf("%+v", s)
	}
	ev := nextEvent(t, events, eventlog.TerminalOpened)
	if len(ev.Targets) != 1 || ev.Targets[0].ID != "k8s:kind/default" || ev.Targets[0].Tags["env"] != "prod" {
		t.Fatalf("targets %+v", ev.Targets)
	}
	var detail Session
	if err := json.Unmarshal(ev.Detail, &detail); err != nil || detail.Pod != "web-1" || detail.Kind != KindPod {
		t.Fatalf("detail %s %v", ev.Detail, err)
	}

	_, ch, cancel, err := m.Subscribe(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if err := m.Write(s.ID, []byte("ls\r")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, "ls")
	if text, _ := m.Text(s.ID, 10); !strings.Contains(text, "$ ls") {
		t.Fatalf("text %q", text)
	}
	if err := m.Resize(s.ID, 120, 40); err != nil {
		t.Fatal(err)
	}
	pods.shells[0].mu.Lock()
	sizes := fmt.Sprint(pods.shells[0].sizes)
	pods.shells[0].mu.Unlock()
	if sizes != "[[80 24] [120 40]]" {
		t.Fatalf("sizes %s", sizes)
	}
	if got, _ := m.Get(s.ID); got.Cols != 120 || got.Rows != 40 {
		t.Fatalf("%+v", got)
	}
	// The shell exits by itself, with its code.
	m.Write(s.ID, []byte("exit 4\r"))
	if c := waitExit(t, ch); c.Code == nil || *c.Code != 4 {
		t.Fatalf("exit %+v", c)
	}
	if ev := nextEvent(t, events, eventlog.TerminalClosed); ev.Reason != "exited" || *ev.ExitCode != 4 {
		t.Fatalf("%+v", ev)
	}
	if got, ok := m.Get(s.ID); !ok || !got.Closed {
		t.Fatal("ended terminal not kept for reading")
	}
}

func TestPodTerminalClose(t *testing.T) {
	m, _, events := setupPods(t)
	s, err := m.OpenPod(context.Background(), PodSpec{Context: "kind", Namespace: "shop", Pod: "web-1"}, human, 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Close(s.ID, human); err != nil {
		t.Fatal(err)
	}
	if ev := nextEvent(t, events, eventlog.TerminalClosed); ev.Reason != "closed by human:desktop" || ev.ExitCode != nil {
		t.Fatalf("%+v", ev)
	}
	if _, ok := m.Get(s.ID); ok {
		t.Fatal("closed terminal kept")
	}
}

func TestPodTerminalErrors(t *testing.T) {
	m, pods, _ := setupPods(t)
	for _, spec := range []PodSpec{{Pod: "web-1"}, {Context: "kind"}} {
		if _, err := m.OpenPod(context.Background(), spec, human, 0, 0); err == nil {
			t.Errorf("%+v opened", spec)
		}
	}
	if _, err := m.OpenPod(context.Background(), PodSpec{Context: "kind", Pod: "p"}, human, 0, 5000); !errors.Is(err, ErrSize) {
		t.Fatalf("%v", err)
	}
	pods.err = errors.New(`pods "p" not found`)
	if _, err := m.OpenPod(context.Background(), PodSpec{Context: "kind", Pod: "p"}, human, 0, 0); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("%v", err)
	}
	if len(m.List()) != 0 {
		t.Fatal("failed terminal listed")
	}
	if _, err := (&Manager{}).OpenPod(context.Background(), PodSpec{Context: "kind", Pod: "p"}, human, 0, 0); err == nil {
		t.Fatal("pod terminal without a PodOpener")
	}
	pods.err = nil
	m.MaxOpen = 1
	if _, err := m.OpenPod(context.Background(), PodSpec{Context: "kind", Pod: "p"}, human, 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := m.OpenPod(context.Background(), PodSpec{Context: "kind", Pod: "p"}, human, 0, 0); !errors.Is(err, ErrTooMany) {
		t.Fatalf("limit: %v", err)
	}
}

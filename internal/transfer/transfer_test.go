package transfer

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/eventlog"
)

var (
	codex   = action.Actor{Kind: action.Agent, Name: "codex"}
	desktop = action.Actor{Kind: action.Human, Name: "desktop"}
	target  = []action.Target{{Kind: "host", ID: "prod", Tags: map[string]string{"env": "prod"}}}
)

// clock is a settable time source.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newManager(t *testing.T) (*Manager, <-chan eventlog.Event, *clock) {
	t.Helper()
	events := eventlog.Open(filepath.Join(t.TempDir(), "events"))
	ch, cancel := events.Subscribe()
	t.Cleanup(cancel)
	c := &clock{now: time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)}
	m := &Manager{Events: events, Now: c.Now}
	t.Cleanup(m.CancelAll)
	return m, ch, c
}

func next(t *testing.T, ch <-chan eventlog.Event) eventlog.Event {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
	}
	return eventlog.Event{}
}

func detail(t *testing.T, ev eventlog.Event) Transfer {
	t.Helper()
	var tr Transfer
	if err := json.Unmarshal(ev.Detail, &tr); err != nil {
		t.Fatalf("%s: %v", ev.Detail, err)
	}
	return tr
}

func upload() Transfer {
	return Transfer{Direction: Upload, Host: "prod", LocalPath: "/tmp/a", RemotePath: "/srv/a", Owner: codex}
}

func TestRunRecordsProgressAndOutcome(t *testing.T) {
	m, ch, clk := newManager(t)
	got, err := m.Run(context.Background(), upload(), target, func(_ context.Context, p *Progress) error {
		p.Start(10, 100)
		p.Add(20) // t=0: the first update is published at once
		clk.advance(500 * time.Millisecond)
		p.Add(20) // 0.5s: throttled
		clk.advance(500 * time.Millisecond)
		p.Add(20) // 1.0s: published
		clk.advance(200 * time.Millisecond)
		p.Add(30) // 1.2s: throttled
		p.Update(func(t *Transfer) { t.SHA256 = "abc" })
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.ID, "x_") || got.Status != Done || got.Bytes != 100 || got.Total != 100 ||
		got.ResumedFrom != 10 || got.SHA256 != "abc" || got.FinishedAt == nil || got.Owner != codex {
		t.Fatalf("%+v", got)
	}
	ev := next(t, ch)
	if ev.Type != eventlog.TransferStarted || ev.Transfer != got.ID || ev.Actor.Name != "codex" || ev.Targets[0].Tags["env"] != "prod" {
		t.Fatalf("started: %+v", ev)
	}
	var progress []int64
	for {
		ev = next(t, ch)
		if ev.Type != eventlog.TransferProgress {
			break
		}
		progress = append(progress, detail(t, ev).Bytes)
	}
	if len(progress) != 2 || progress[0] != 30 || progress[1] != 70 {
		t.Fatalf("progress events: %v", progress)
	}
	if ev.Type != eventlog.TransferFinished || detail(t, ev).Status != Done || detail(t, ev).Bytes != 100 {
		t.Fatalf("finished: %+v", ev)
	}
	if list := m.List(); len(list) != 1 || list[0].ID != got.ID || list[0].Status != Done {
		t.Fatalf("list: %+v", list)
	}
	// Only started and finished are in the audit log.
	evs, _ := m.Events.Read(clk.Now())
	if len(evs) != 2 {
		t.Fatalf("audit log: %+v", evs)
	}
}

func TestRunFailure(t *testing.T) {
	m, ch, _ := newManager(t)
	got, err := m.Run(context.Background(), upload(), target, func(context.Context, *Progress) error {
		return errors.New("disk full")
	})
	if err == nil || got.Status != Failed || got.Error != "disk full" {
		t.Fatalf("%+v %v", got, err)
	}
	next(t, ch)
	if ev := next(t, ch); ev.Type != eventlog.TransferFailed || ev.Error != "disk full" || detail(t, ev).Status != Failed {
		t.Fatalf("%+v", ev)
	}
}

// running starts a transfer whose work waits for its context and returns
// the transfer id; done receives Run's result.
func running(t *testing.T, m *Manager, ctx context.Context, ch <-chan eventlog.Event) (string, <-chan error) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := m.Run(ctx, upload(), target, func(ctx context.Context, _ *Progress) error {
			<-ctx.Done()
			return ctx.Err()
		})
		done <- err
	}()
	ev := next(t, ch)
	if ev.Type != eventlog.TransferStarted {
		t.Fatalf("%+v", ev)
	}
	return ev.Transfer, done
}

func TestCancel(t *testing.T) {
	m, ch, _ := newManager(t)
	id, done := running(t, m, context.Background(), ch)
	if err := m.Cancel(id, desktop); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "cancelled by human:desktop") {
		t.Fatalf("run: %v", err)
	}
	ev := next(t, ch)
	if ev.Type != eventlog.TransferFailed || ev.Reason != "cancelled by human:desktop" || detail(t, ev).Status != Cancelled {
		t.Fatalf("%+v", ev)
	}
	if err := m.Cancel(id, desktop); !errors.Is(err, ErrFinished) {
		t.Fatalf("cancel twice: %v", err)
	}
	if err := m.Cancel("x_nope", desktop); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cancel unknown: %v", err)
	}
}

// TestCallerCancel: when the caller's context ends (the client was
// revoked, the server stops), the transfer is cancelled with its cause.
func TestCallerCancel(t *testing.T) {
	m, ch, _ := newManager(t)
	ctx, cancel := context.WithCancelCause(context.Background())
	id, done := running(t, m, ctx, ch)
	cancel(errors.New("client revoked"))
	if err := <-done; err == nil || !strings.Contains(err.Error(), "client revoked") {
		t.Fatalf("run: %v", err)
	}
	if tr, _ := m.Get(id); tr.Status != Cancelled || tr.Error != "client revoked" {
		t.Fatalf("%+v", tr)
	}
}

func TestCancelAllWaits(t *testing.T) {
	m, ch, _ := newManager(t)
	id, done := running(t, m, context.Background(), ch)
	m.CancelAll()
	if tr, _ := m.Get(id); tr.Status != Cancelled || tr.Error != "server stopped" {
		t.Fatalf("%+v", tr)
	}
	<-done
}

func TestKeepsRecentFinished(t *testing.T) {
	m, _, _ := newManager(t)
	m.Keep = 3
	var ids []string
	for range 5 {
		tr, _ := m.Run(context.Background(), upload(), target, func(context.Context, *Progress) error { return nil })
		ids = append(ids, tr.ID)
	}
	list := m.List()
	if len(list) != 3 || list[0].ID != ids[2] || list[2].ID != ids[4] {
		t.Fatalf("%+v", list)
	}
	if _, ok := m.Get(ids[0]); ok {
		t.Fatal("oldest kept")
	}
}

// TestSuccessWins: a transfer that completed is done even if it was
// cancelled just as it finished.
func TestSuccessWins(t *testing.T) {
	m, _, _ := newManager(t)
	got, err := m.Run(context.Background(), upload(), target, func(_ context.Context, p *Progress) error {
		var id string
		p.Update(func(t *Transfer) { id = t.ID })
		m.Cancel(id, desktop)
		return nil
	})
	if err != nil || got.Status != Done || got.Error != "" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestClaim(t *testing.T) {
	m, _, _ := newManager(t)
	release, err := m.Claim("dev\x00/srv/a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Claim("dev\x00/srv/a"); !errors.Is(err, ErrBusy) {
		t.Fatalf("%v", err)
	}
	other, err := m.Claim("dev\x00/srv/b")
	if err != nil {
		t.Fatal(err)
	}
	other()
	release()
	release()
	if r, err := m.Claim("dev\x00/srv/a"); err != nil {
		t.Fatal(err)
	} else {
		r()
	}
}

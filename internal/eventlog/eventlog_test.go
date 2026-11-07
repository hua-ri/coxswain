package eventlog

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
)

func TestAppendAndRead(t *testing.T) {
	dir := t.TempDir()
	day := time.Date(2026, 11, 7, 10, 0, 0, 0, time.Local)
	l := Open(dir)
	l.Now = func() time.Time { return day }

	actor := &action.Actor{Kind: action.Agent, Name: "codex"}
	if err := l.Append(Event{Type: ActionRequested, Actor: actor, Action: "ssh.exec"}); err != nil {
		t.Fatal(err)
	}
	if err := l.Append(Event{Type: ActionFinished, Action: "ssh.exec", DurationMs: 12}); err != nil {
		t.Fatal(err)
	}

	got, err := l.Read(day)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Actor.Name != "codex" || got[1].DurationMs != 12 {
		t.Fatalf("unexpected events: %+v", got)
	}
	info, err := os.Stat(filepath.Join(dir, "2026-11-07.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestReadMissingDay(t *testing.T) {
	got, err := Open(t.TempDir()).Read(time.Now())
	if err != nil || got != nil {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestSubscribeInOrder(t *testing.T) {
	l := Open(t.TempDir())
	ch, cancel := l.Subscribe()
	defer cancel()
	for i := range 3 {
		if err := l.Append(Event{Type: ActionFinished, Action: "a", DurationMs: int64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 3 {
		select {
		case ev := <-ch:
			if ev.DurationMs != int64(i) || ev.Time.IsZero() {
				t.Fatalf("event %d: %+v", i, ev)
			}
		case <-time.After(time.Second):
			t.Fatalf("event %d not delivered", i)
		}
	}
}

func TestSlowSubscriberDoesNotBlock(t *testing.T) {
	l := Open(t.TempDir())
	_, cancel := l.Subscribe() // never read
	defer cancel()
	fast, cancelFast := l.Subscribe()
	defer cancelFast()
	done := make(chan struct{})
	go func() {
		for range subscriberBuffer + 10 {
			l.Append(Event{Type: ActionFinished})
			<-fast
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Append blocked on a slow subscriber")
	}
	if l.Dropped() != 10 {
		t.Fatalf("dropped %d, want 10", l.Dropped())
	}
}

func TestCancelStopsDelivery(t *testing.T) {
	l := Open(t.TempDir())
	ch, cancel := l.Subscribe()
	cancel()
	cancel() // idempotent
	l.Append(Event{Type: ActionFinished})
	if _, ok := <-ch; ok {
		t.Fatal("delivered after cancel")
	}
}

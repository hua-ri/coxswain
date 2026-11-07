package approval

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
)

func newStore(t *testing.T) (*Store, *time.Time) {
	now := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	s := NewStore(t.TempDir(), 10*time.Minute)
	s.Now = func() time.Time { return now }
	return s, &now
}

func create(t *testing.T, s *Store) Approval {
	ap, err := s.Create(Approval{
		ActionID: "ssh.exec",
		Args:     json.RawMessage(`{"host":"web","command":"reboot"}`),
		Actor:    action.Actor{Kind: action.Agent, Name: "codex"},
		Risk:     "destructive",
		Preview:  "web$ reboot",
	})
	if err != nil {
		t.Fatal(err)
	}
	return ap
}

func TestHashIgnoresKeyOrder(t *testing.T) {
	a, _ := Hash("ssh.exec", json.RawMessage(`{"a":1,"b":2}`))
	b, _ := Hash("ssh.exec", json.RawMessage(`{"b":2,"a":1}`))
	c, _ := Hash("ssh.exec", json.RawMessage(`{"a":1,"b":3}`))
	if a != b || a == c {
		t.Fatalf("hash: a=%s b=%s c=%s", a, b, c)
	}
}

func TestApproveThenConsumeOnce(t *testing.T) {
	s, _ := newStore(t)
	ap := create(t, s)
	if ap.Status != Pending || !idPattern.MatchString(ap.ID) {
		t.Fatalf("created: %+v", ap)
	}
	if _, err := s.Decide(ap.ID, true); err != nil {
		t.Fatal(err)
	}
	got, err := s.Consume(ap.ID)
	if err != nil || got.Status != Consumed {
		t.Fatalf("consume: %+v %v", got, err)
	}
	if _, err := s.Consume(ap.ID); err == nil {
		t.Fatal("second consume must fail")
	}
}

func TestDenyCannotBeConsumed(t *testing.T) {
	s, _ := newStore(t)
	ap := create(t, s)
	if _, err := s.Decide(ap.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Consume(ap.ID); err == nil {
		t.Fatal("consume after deny must fail")
	}
	if _, err := s.Decide(ap.ID, true); !errors.Is(err, ErrNotPending) {
		t.Fatalf("re-decide: %v", err)
	}
}

func TestExpiry(t *testing.T) {
	s, now := newStore(t)
	ap := create(t, s)
	*now = now.Add(11 * time.Minute)
	got, err := s.Get(ap.ID)
	if err != nil || got.Status != Expired {
		t.Fatalf("got %+v %v", got.Status, err)
	}
	if _, err := s.Decide(ap.ID, true); !errors.Is(err, ErrNotPending) {
		t.Fatalf("decide expired: %v", err)
	}
}

func TestListAndBadID(t *testing.T) {
	s, now := newStore(t)
	first := create(t, s)
	*now = now.Add(time.Second)
	create(t, s)
	all, err := s.List()
	if err != nil || len(all) != 2 || all[0].ID != first.ID {
		t.Fatalf("list: %v %v", all, err)
	}
	if _, err := s.Get("../../etc/passwd"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad id: %v", err)
	}
}

func TestApprovedExpires(t *testing.T) {
	s, now := newStore(t)
	ap := create(t, s)
	if _, err := s.Decide(ap.ID, true); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(11 * time.Minute)
	if got, _ := s.Get(ap.ID); got.Status != Expired {
		t.Fatalf("approved past ExpiresAt must read as expired: %s", got.Status)
	}
	if _, err := s.Consume(ap.ID); err == nil {
		t.Fatal("expired approval consumed")
	}
}

func TestConsumeChecksHashFirst(t *testing.T) {
	s, _ := newStore(t)
	ap := create(t, s)
	if _, err := s.Decide(ap.ID, true); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(s.path(ap.ID))
	os.WriteFile(s.path(ap.ID), []byte(strings.Replace(string(b), "reboot", "halt", 1)), 0o600)
	if _, err := s.Consume(ap.ID); err == nil {
		t.Fatal("tampered args consumed")
	}
	if got, _ := s.Get(ap.ID); got.Status != Approved {
		t.Fatalf("a failed hash check must not consume: %s", got.Status)
	}
}

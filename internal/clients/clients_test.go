package clients

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hua-ri/coxswain/internal/eventlog"
)

func newStore(t *testing.T) *Store {
	return NewStore(filepath.Join(t.TempDir(), "clients.json"))
}

func TestStore(t *testing.T) {
	s := newStore(t)
	if ok, err := s.IsTrusted("codex"); ok || err != nil {
		t.Fatalf("empty store: %v %v", ok, err)
	}
	if err := s.Trust("codex"); err != nil {
		t.Fatal(err)
	}
	if err := s.Trust("kimi"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := NewStore(s.path).IsTrusted("codex"); !ok {
		t.Fatal("trust not persisted")
	}
	list, err := s.List()
	if err != nil || len(list) != 2 || list[0].Name != "codex" || list[1].Name != "kimi" || list[0].AuthorizedAt.IsZero() {
		t.Fatalf("list %+v %v", list, err)
	}
	if st, _ := os.Stat(s.path); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
}

// TestStoreRevoke: a revoked name is remembered as revoked (so the user is
// not asked about it again) until it is trusted again.
func TestStoreRevoke(t *testing.T) {
	s := newStore(t)
	s.Trust("codex")
	for range 2 { // idempotent
		if err := s.Revoke("codex"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Revoke("never-seen"); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Status("codex"); st != Revoked {
		t.Fatalf("status %v", st)
	}
	if ok, _ := s.IsTrusted("codex"); ok {
		t.Fatal("still trusted after revoke")
	}
	list, _ := s.List()
	if len(list) != 2 || !list[0].Revoked || list[0].RevokedAt.IsZero() {
		t.Fatalf("list %+v", list)
	}
	s.Trust("codex")
	if st, _ := s.Status("codex"); st != Trusted {
		t.Fatalf("after trust: %v", st)
	}
	if st, _ := s.Status("other"); st != Unknown {
		t.Fatalf("unknown: %v", st)
	}
}

func TestValidName(t *testing.T) {
	cases := map[string]bool{
		"claude-code": true, "codex": true, "kimi": true, "a.b_c-1": true, "0x": true,
		"": false, "Codex": false, "-x": false, "a b": false, "a/b": false,
		strings.Repeat("a", 64): true, strings.Repeat("a", 65): false,
	}
	for name, want := range cases {
		if got := ValidName(name); got != want {
			t.Errorf("ValidName(%q) = %v", name, got)
		}
	}
}

func never(t *testing.T) func(context.Context, string) (bool, error) {
	return func(context.Context, string) (bool, error) {
		t.Error("asked the user")
		return false, nil
	}
}

func TestBuiltInsNeedNoPrompt(t *testing.T) {
	a := &Authorizer{Store: newStore(t), Ask: never(t)}
	for _, name := range []string{"cli", "desktop"} {
		if ok, reason := a.Authorize(context.Background(), name); !ok || reason != "" {
			t.Fatalf("%s: %v %q", name, ok, reason)
		}
	}
	if err := a.Revoke("cli"); err == nil {
		t.Fatal("revoked a built-in client")
	}
}

func TestInvalidNameRefused(t *testing.T) {
	a := &Authorizer{Store: newStore(t), Ask: never(t)}
	if ok, reason := a.Authorize(context.Background(), "Bad Name"); ok || reason != ReasonInvalidName {
		t.Fatalf("%v %q", ok, reason)
	}
	if err := a.Revoke("Bad Name"); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("%v", err)
	}
}

func TestConcurrentAuthorizePromptsOnce(t *testing.T) {
	var asked atomic.Int32
	release := make(chan struct{})
	a := &Authorizer{Store: newStore(t), Ask: func(_ context.Context, name string) (bool, error) {
		asked.Add(1)
		<-release
		return name == "codex", nil
	}}
	var wg sync.WaitGroup
	results := make(chan bool, 10)
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, _ := a.Authorize(context.Background(), "codex")
			results <- ok
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	close(results)
	for ok := range results {
		if !ok {
			t.Fatal("a waiter was denied")
		}
	}
	if asked.Load() != 1 {
		t.Fatalf("asked %d times", asked.Load())
	}
	if ok, _ := a.Store.IsTrusted("codex"); !ok {
		t.Fatal("allow not persisted")
	}
	if ok, _ := a.Authorize(context.Background(), "codex"); !ok || asked.Load() != 1 {
		t.Fatal("trusted client asked again")
	}
}

// TestDenialCached: a denial is not stored, but for a while the user is
// not asked again, so a busy agent cannot flood them with dialogs.
func TestDenialCached(t *testing.T) {
	var asked atomic.Int32
	now := time.Now()
	a := &Authorizer{Store: newStore(t), DenyFor: time.Minute, Now: func() time.Time { return now },
		Ask: func(context.Context, string) (bool, error) { asked.Add(1); return false, nil }}
	for range 3 {
		if ok, reason := a.Authorize(context.Background(), "kimi"); ok || reason != ReasonDenied {
			t.Fatalf("%v %q", ok, reason)
		}
	}
	if asked.Load() != 1 {
		t.Fatalf("asked %d times within a minute", asked.Load())
	}
	if ok, _ := a.Store.IsTrusted("kimi"); ok {
		t.Fatal("denied client trusted")
	}
	now = now.Add(2 * time.Minute)
	a.Authorize(context.Background(), "kimi")
	if asked.Load() != 2 {
		t.Fatalf("not asked again after the cache expired: %d", asked.Load())
	}
}

func TestAskErrorDenies(t *testing.T) {
	a := &Authorizer{Store: newStore(t), Ask: func(context.Context, string) (bool, error) { return true, errors.New("no osascript") }}
	if ok, reason := a.Authorize(context.Background(), "kimi"); ok || reason != ReasonDenied {
		t.Fatalf("%v %q", ok, reason)
	}
	if ok, _ := (&Authorizer{Store: a.Store}).Authorize(context.Background(), "kimi"); ok {
		t.Fatal("nil Ask must deny")
	}
}

func TestWaiterContextCancelled(t *testing.T) {
	release := make(chan struct{})
	a := &Authorizer{Store: newStore(t), Ask: func(context.Context, string) (bool, error) { <-release; return true, nil }}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if ok, _ := a.Authorize(ctx, "codex"); ok {
		t.Fatal("allowed without an answer")
	}
	// The prompt is still open; a later caller gets its answer.
	close(release)
	if ok, reason := a.Authorize(context.Background(), "codex"); !ok {
		t.Fatalf("after the answer: %q", reason)
	}
}

// TestRevokeThenAllow: a revoked client is refused without a prompt until
// the user allows it again, which takes effect at once.
func TestRevokeThenAllow(t *testing.T) {
	a := &Authorizer{Store: newStore(t), Ask: never(t)}
	a.Store.Trust("codex")
	var got []string
	a.OnRevoke(func(name string) { got = append(got, name) })
	if err := a.Revoke("codex"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "codex" {
		t.Fatalf("notified %v", got)
	}
	if ok, reason := a.Authorize(context.Background(), "codex"); ok || reason != ReasonRevoked {
		t.Fatalf("%v %q", ok, reason)
	}
	if err := a.Allow("codex"); err != nil {
		t.Fatal(err)
	}
	if ok, reason := a.Authorize(context.Background(), "codex"); !ok {
		t.Fatalf("after allow: %q", reason)
	}
}

func TestAllowClearsDenial(t *testing.T) {
	a := &Authorizer{Store: newStore(t), Ask: func(context.Context, string) (bool, error) { return false, nil }}
	a.Authorize(context.Background(), "kimi")
	a.Allow("kimi")
	if ok, reason := a.Authorize(context.Background(), "kimi"); !ok {
		t.Fatalf("%q", reason)
	}
	if err := a.Allow("desktop"); err == nil {
		t.Fatal("allowed a built-in name")
	}
}

func TestHints(t *testing.T) {
	if h := NotAuthorizedHint("codex", ReasonRevoked); !strings.Contains(h, "allow it again with: cox clients allow codex") {
		t.Fatal(h)
	}
	if h := NotAuthorizedHint("kimi", ReasonDenied); !strings.Contains(h, "cox clients allow kimi") {
		t.Fatal(h)
	}
}

func TestDialogDisabled(t *testing.T) {
	t.Setenv("COX_NO_DIALOG", "1")
	if ok, err := Dialog(time.Second)(context.Background(), "codex"); ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	if m := Message("codex\r\x1b[2K"); m != `codex\x0d\x1b[2K wants to use Coxswain to operate your servers.` {
		t.Fatalf("message %q", m)
	}
}

func TestDecisionsAudited(t *testing.T) {
	dir := t.TempDir()
	log := eventlog.Open(filepath.Join(dir, "events"))
	answer := true
	a := &Authorizer{Store: NewStore(filepath.Join(dir, "clients.json")), Events: log,
		Ask: func(context.Context, string) (bool, error) { return answer, nil }}
	a.Authorize(context.Background(), "codex")
	answer = false
	a.Authorize(context.Background(), "kimi")
	a.Revoke("codex")
	a.Allow("codex")
	evs, _ := log.Read(time.Now())
	var got []string
	for _, ev := range evs {
		got = append(got, ev.Type+" "+ev.Client)
	}
	want := "client.authorized codex,client.denied kimi,client.revoked codex,client.allowed codex"
	if strings.Join(got, ",") != want {
		t.Fatalf("got %v", got)
	}
}

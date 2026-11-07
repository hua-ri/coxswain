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
	if err := s.Revoke("codex"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.IsTrusted("codex"); ok {
		t.Fatal("still trusted after revoke")
	}
	if err := s.Revoke("codex"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke twice: %v", err)
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

func TestBuiltInsNeedNoPrompt(t *testing.T) {
	a := &Authorizer{Store: newStore(t), Ask: func(context.Context, string) (bool, error) {
		t.Fatal("asked for a built-in client")
		return false, nil
	}}
	for _, name := range []string{"cli", "desktop"} {
		if ok, err := a.Authorize(context.Background(), name); !ok || err != nil {
			t.Fatalf("%s: %v %v", name, ok, err)
		}
	}
	if err := a.Revoke("cli"); err == nil {
		t.Fatal("revoked a built-in client")
	}
}

func TestInvalidNameRefused(t *testing.T) {
	a := &Authorizer{Store: newStore(t), Ask: func(context.Context, string) (bool, error) { return true, nil }}
	if ok, err := a.Authorize(context.Background(), "Bad Name"); ok || !errors.Is(err, ErrInvalidName) {
		t.Fatalf("%v %v", ok, err)
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
			ok, err := a.Authorize(context.Background(), "codex")
			if err != nil {
				t.Error(err)
			}
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
		t.Fatal("approval not persisted")
	}
	if ok, _ := a.Authorize(context.Background(), "codex"); !ok || asked.Load() != 1 {
		t.Fatal("trusted client asked again")
	}
}

func TestDenyNotPersisted(t *testing.T) {
	var asked atomic.Int32
	a := &Authorizer{Store: newStore(t), Ask: func(context.Context, string) (bool, error) { asked.Add(1); return false, nil }}
	for range 2 {
		if ok, err := a.Authorize(context.Background(), "kimi"); ok || err != nil {
			t.Fatalf("%v %v", ok, err)
		}
	}
	if asked.Load() != 2 {
		t.Fatalf("a denial must not be remembered; asked %d", asked.Load())
	}
	if ok, _ := a.Store.IsTrusted("kimi"); ok {
		t.Fatal("denied client trusted")
	}
}

func TestAskErrorDenies(t *testing.T) {
	a := &Authorizer{Store: newStore(t), Ask: func(context.Context, string) (bool, error) { return true, errors.New("no osascript") }}
	if ok, err := a.Authorize(context.Background(), "kimi"); ok || err == nil {
		t.Fatalf("%v %v", ok, err)
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
	if ok, err := a.Authorize(ctx, "codex"); ok || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v %v", ok, err)
	}
	// The prompt is still open; a later caller gets its answer.
	close(release)
	if ok, err := a.Authorize(context.Background(), "codex"); !ok || err != nil {
		t.Fatalf("after the answer: %v %v", ok, err)
	}
}

func TestRevokeNotifies(t *testing.T) {
	a := &Authorizer{Store: newStore(t), Ask: func(context.Context, string) (bool, error) { return true, nil }}
	a.Authorize(context.Background(), "codex")
	var got []string
	a.OnRevoke(func(name string) { got = append(got, name) })
	if err := a.Revoke("codex"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "codex" {
		t.Fatalf("notified %v", got)
	}
	if ok, _ := a.Store.IsTrusted("codex"); ok {
		t.Fatal("still trusted")
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

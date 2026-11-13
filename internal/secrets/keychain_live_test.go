//go:build keychain

package secrets

import "testing"

// go test -tags keychain ./internal/secrets: round-trips through the real
// login keychain under a throwaway service.
func TestKeychainLive(t *testing.T) {
	k := &Keychain{Service: "coxswain-selftest"}
	for _, v := range []string{"s3cret", "\x00\x01binary\xff"} {
		if err := k.Set("probe", []byte(v)); err != nil {
			t.Fatal(err)
		}
		b, err := k.Get("probe")
		if err != nil || string(b) != v {
			t.Fatalf("Get = %q, %v", b, err)
		}
	}
	if err := k.Delete("probe"); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Get("probe"); err != ErrNotFound {
		t.Fatalf("after Delete: %v", err)
	}
}

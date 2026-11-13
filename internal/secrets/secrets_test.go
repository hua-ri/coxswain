package secrets

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMemory(t *testing.T) {
	var s Memory
	if _, err := s.Get("a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing = %v", err)
	}
	if err := s.Set("a", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if b, _ := s.Get("a"); string(b) != "x" {
		t.Fatalf("Get = %q", b)
	}
	if err := s.Delete("a"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("a"); err != nil {
		t.Fatal(err)
	}
}

// fakeTool records its arguments and stdin, and answers like security.
func fakeTool(t *testing.T, script string) (tool, log string) {
	dir := t.TempDir()
	log = filepath.Join(dir, "log")
	tool = filepath.Join(dir, "security")
	body := "#!/bin/sh\necho \"ARGS $*\" >> " + log + "\ncat >> " + log + "\n" + script + "\n"
	if err := os.WriteFile(tool, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return tool, log
}

func TestKeychainSetKeepsSecretOutOfArguments(t *testing.T) {
	tool, log := fakeTool(t, "exit 0")
	k := &Keychain{Service: "coxswain", Tool: tool}
	if err := k.Set("argocd/prod", []byte("s3cret")); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	got := string(b)
	if !strings.Contains(got, "ARGS -i\n") {
		t.Fatalf("Set should use security -i, got %q", got)
	}
	if strings.Contains(got, "s3cret") || strings.Contains(got, "czNjcmV0") {
		t.Fatalf("secret in plain text: %q", got)
	}
	if !strings.Contains(got, "-X 637a4e6a636d5630") {
		t.Fatalf("secret should be written as hex on stdin: %q", got)
	}
}

func TestKeychainGet(t *testing.T) {
	tool, _ := fakeTool(t, "echo czNjcmV0")
	k := &Keychain{Service: "coxswain", Tool: tool}
	b, err := k.Get("argocd/prod")
	if err != nil || string(b) != "s3cret" {
		t.Fatalf("Get = %q, %v", b, err)
	}
}

func TestKeychainNotFound(t *testing.T) {
	tool, _ := fakeTool(t, "echo 'security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain.' >&2; exit 44")
	k := &Keychain{Service: "coxswain", Tool: tool}
	if _, err := k.Get("argocd/prod"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get = %v", err)
	}
	if err := k.Delete("argocd/prod"); err != nil {
		t.Fatalf("Delete missing = %v", err)
	}
}

func TestKeychainRejectsOddAccounts(t *testing.T) {
	k := &Keychain{Service: "coxswain", Tool: "/nonexistent"}
	for _, a := range []string{"", "a b", "a\n-X ff", "a;b"} {
		if err := k.Set(a, []byte("x")); err == nil {
			t.Errorf("Set(%q) accepted", a)
		}
	}
}

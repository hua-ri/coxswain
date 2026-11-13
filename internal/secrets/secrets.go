// Package secrets keeps credentials (Argo tokens) out of Coxswain's JSON
// files: in the macOS login keychain, or in memory for tests.
package secrets

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ErrNotFound means there is no secret for the account.
var ErrNotFound = errors.New("no secret stored")

// Store holds secrets by account name.
type Store interface {
	Get(account string) ([]byte, error)
	Set(account string, secret []byte) error
	Delete(account string) error // no error if there is none
}

// Memory is a Store in memory, for tests.
type Memory struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (s *Memory) Get(account string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[account]
	if !ok {
		return nil, ErrNotFound
	}
	return bytes.Clone(v), nil
}

func (s *Memory) Set(account string, secret []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string][]byte{}
	}
	s.m[account] = bytes.Clone(secret)
	return nil
}

func (s *Memory) Delete(account string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, account)
	return nil
}

// Keychain is a Store in the login keychain, as generic passwords of
// service Service. It goes through /usr/bin/security, as gh does: the
// items then trust that Apple-signed tool rather than cox, whose
// signature changes with every build, so upgrading cox does not make
// macOS ask for access again. Secrets are written through its stdin
// (security -i), never as arguments that ps would show.
type Keychain struct {
	Service string
	Tool    string // default /usr/bin/security
}

var accountPattern = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,128}$`)

func (k *Keychain) tool() string {
	if k.Tool != "" {
		return k.Tool
	}
	return "/usr/bin/security"
}

func (k *Keychain) check(account string) error {
	if !accountPattern.MatchString(account) || !accountPattern.MatchString(k.Service) {
		return fmt.Errorf("keychain: invalid account %q", account)
	}
	return nil
}

func (k *Keychain) run(stdin string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, k.tool(), args...)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		var ee *exec.ExitError
		// 44: errSecItemNotFound.
		if errors.As(err, &ee) && ee.ExitCode() == 44 {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("keychain: %v: %s", err, strings.TrimSpace(errb.String()))
	}
	// security -i reports a failed command on stderr but exits 0.
	if msg := strings.TrimSpace(errb.String()); msg != "" && len(args) > 0 && args[0] == "-i" {
		if strings.Contains(msg, "could not be found") {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("keychain: %s", msg)
	}
	return out.Bytes(), nil
}

func (k *Keychain) Get(account string) ([]byte, error) {
	if err := k.check(account); err != nil {
		return nil, err
	}
	out, err := k.run("", "find-generic-password", "-s", k.Service, "-a", account, "-w")
	if err != nil {
		return nil, err
	}
	// Stored as base64 text, which -w prints as is (it prints
	// non-printable data as hex, which would be ambiguous).
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(out)))
	if err != nil {
		return nil, errors.New("keychain: the stored secret is not Coxswain's")
	}
	return b, nil
}

func (k *Keychain) Set(account string, secret []byte) error {
	if err := k.check(account); err != nil {
		return err
	}
	line := fmt.Sprintf("add-generic-password -U -s %s -a %s -l %s -X %s\n",
		k.Service, account, "coxswain:"+account,
		hex.EncodeToString([]byte(base64.StdEncoding.EncodeToString(secret))))
	_, err := k.run(line, "-i")
	return err
}

func (k *Keychain) Delete(account string) error {
	if err := k.check(account); err != nil {
		return err
	}
	_, err := k.run("", "delete-generic-password", "-s", k.Service, "-a", account)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

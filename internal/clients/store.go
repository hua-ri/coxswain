// Package clients remembers which agent clients the user has allowed to
// use Coxswain (clients.json) and asks the user, once, about new ones.
package clients

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"time"

	"github.com/hua-ri/coxswain/internal/filelock"
)

// ErrBuiltIn is returned when allowing or revoking cli or desktop.
var ErrBuiltIn = errors.New("built-in client")

// ErrInvalidName is returned for client names outside namePattern.
var ErrInvalidName = errors.New("invalid client name")

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ValidName reports whether name may identify a client (X-Cox-Client).
func ValidName(name string) bool { return namePattern.MatchString(name) }

// Client is one client the user decided about: allowed, or revoked.
type Client struct {
	Name         string    `json:"name"`
	AuthorizedAt time.Time `json:"authorized_at,omitzero"`
	Revoked      bool      `json:"revoked,omitempty"`
	RevokedAt    time.Time `json:"revoked_at,omitzero"`
}

// Status is what the user decided about a client.
type Status int

const (
	Unknown Status = iota // never decided: ask
	Trusted
	Revoked // refused without asking until allowed again
)

// Store keeps trusted clients in one JSON file (mode 0600). Access is
// serialised across processes with a flock on <file>.lock.
type Store struct {
	path string
	Now  func() time.Time
}

func NewStore(path string) *Store { return &Store{path: path, Now: time.Now} }

// update loads the client map under the file lock, lets fn change it and
// saves it if fn reports a change.
func (s *Store) update(fn func(m map[string]Client) (bool, error)) error {
	return filelock.With(s.path+".lock", func() error {
		m, err := s.load()
		if err != nil {
			return err
		}
		changed, err := fn(m)
		if err != nil || !changed {
			return err
		}
		return s.save(m)
	})
}

// Status reports what the user decided about name.
func (s *Store) Status(name string) (Status, error) {
	st := Unknown
	err := s.update(func(m map[string]Client) (bool, error) {
		if c, ok := m[name]; ok {
			st = Trusted
			if c.Revoked {
				st = Revoked
			}
		}
		return false, nil
	})
	return st, err
}

func (s *Store) IsTrusted(name string) (bool, error) {
	st, err := s.Status(name)
	return st == Trusted, err
}

// Trust records name as allowed now (keeping an earlier time if it was
// already allowed).
func (s *Store) Trust(name string) error {
	return s.update(func(m map[string]Client) (bool, error) {
		if c, ok := m[name]; ok && !c.Revoked {
			return false, nil
		}
		m[name] = Client{Name: name, AuthorizedAt: s.Now().UTC()}
		return true, nil
	})
}

// Revoke records name as revoked (also a name never seen before).
func (s *Store) Revoke(name string) error {
	return s.update(func(m map[string]Client) (bool, error) {
		c := m[name]
		if c.Revoked {
			return false, nil
		}
		c.Name, c.Revoked, c.RevokedAt = name, true, s.Now().UTC()
		m[name] = c
		return true, nil
	})
}

// List returns every client decided about, sorted by name.
func (s *Store) List() ([]Client, error) {
	out := []Client{}
	err := s.update(func(m map[string]Client) (bool, error) {
		for _, c := range m {
			out = append(out, c)
		}
		return false, nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, err
}

func (s *Store) load() (map[string]Client, error) {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]Client{}, nil
	}
	if err != nil {
		return nil, err
	}
	var list []Client
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("%s: %w", s.path, err)
	}
	m := make(map[string]Client, len(list))
	for _, c := range list {
		m[c.Name] = c
	}
	return m, nil
}

func (s *Store) save(m map[string]Client) error {
	list := make([]Client, 0, len(m))
	for _, c := range m {
		list = append(list, c)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return filelock.WriteAtomic(s.path, b)
}

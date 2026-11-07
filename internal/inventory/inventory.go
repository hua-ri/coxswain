// Package inventory stores the SSH hosts Coxswain knows about, with tags
// (env=prod marks production) and a free-text note shown to agents.
package inventory

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/hua-ri/coxswain/internal/filelock"
)

type Host struct {
	Name          string            `json:"name"`
	HostName      string            `json:"hostname"`
	Port          int               `json:"port,omitempty"`
	User          string            `json:"user,omitempty"`
	IdentityFiles []string          `json:"identity_files,omitempty"`
	ProxyJump     string            `json:"proxy_jump,omitempty"`
	Tags          map[string]string `json:"tags,omitempty"`
	Note          string            `json:"note,omitempty"`
	Source        string            `json:"source,omitempty"` // "ssh_config" or "manual"
}

var ErrNotFound = errors.New("host not found")

// Store keeps hosts in one JSON file (mode 0600). Access is serialised
// across processes with a flock on <file>.lock.
type Store struct {
	path string
}

func NewStore(path string) *Store { return &Store{path: path} }

// locked runs fn with the host map loaded under the file lock.
func (s *Store) locked(fn func(m map[string]Host) error) error {
	return filelock.With(s.path+".lock", func() error {
		m, err := s.load()
		if err != nil {
			return err
		}
		return fn(m)
	})
}

func (s *Store) List() ([]Host, error) {
	var m map[string]Host
	if err := s.locked(func(hosts map[string]Host) error { m = hosts; return nil }); err != nil {
		return nil, err
	}
	out := make([]Host, 0, len(m))
	for _, h := range m {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Store) Get(name string) (Host, error) {
	var m map[string]Host
	if err := s.locked(func(hosts map[string]Host) error { m = hosts; return nil }); err != nil {
		return Host{}, err
	}
	h, ok := m[name]
	if !ok {
		return Host{}, fmt.Errorf("%w: %s (list hosts with `cox ssh hosts list`)", ErrNotFound, name)
	}
	return h, nil
}

// Update loads the host map, lets fn change it, and saves it.
func (s *Store) Update(fn func(hosts map[string]Host) error) error {
	return s.locked(func(m map[string]Host) error {
		if err := fn(m); err != nil {
			return err
		}
		return s.save(m)
	})
}

// Merge adds imported hosts. Hosts that already exist get the imported
// connection fields but keep their tags and note. A new alias without
// tags inherits tags and note from the strictest existing host at the
// same address, so an alias cannot shed env=prod.
func (s *Store) Merge(imported []Host) (added, updated int, err error) {
	err = s.Update(func(m map[string]Host) error {
		for _, h := range imported {
			if old, ok := m[h.Name]; ok {
				h.Tags, h.Note = old.Tags, old.Note
				updated++
			} else {
				if len(h.Tags) == 0 {
					if same, ok := SameAddress(m, h); ok {
						h.Tags, h.Note = maps.Clone(same.Tags), same.Note
					}
				}
				added++
			}
			m[h.Name] = h
		}
		return nil
	})
	return
}

// SameAddress returns the host in m at h's HostName and port (other than
// h itself), preferring a production one (env=prod or production) so an
// alias keeps that label; ties go to the first by name.
func SameAddress(m map[string]Host, h Host) (Host, bool) {
	var same []Host
	for _, n := range slices.Sorted(maps.Keys(m)) {
		if o := m[n]; n != h.Name && o.Addr() == h.Addr() {
			if o.IsProd() {
				return o, true
			}
			same = append(same, o)
		}
	}
	if len(same) == 0 {
		return Host{}, false
	}
	return same[0], true
}

// IsProd reports whether the host is tagged as production (env=prod or
// env=production, any case).
func (h Host) IsProd() bool {
	env := strings.ToLower(h.Tags["env"])
	return env == "prod" || env == "production"
}

// Addr is the host's network address, host:port (port 22 if unset).
func (h Host) Addr() string {
	port := h.Port
	if port == 0 {
		port = 22
	}
	return net.JoinHostPort(strings.ToLower(h.HostName), strconv.Itoa(port))
}

func (s *Store) load() (map[string]Host, error) {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]Host{}, nil
	}
	if err != nil {
		return nil, err
	}
	var list []Host
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("%s: %w", s.path, err)
	}
	m := make(map[string]Host, len(list))
	for _, h := range list {
		m[h.Name] = h
	}
	return m, nil
}

func (s *Store) save(m map[string]Host) error {
	list := make([]Host, 0, len(m))
	for _, h := range m {
		list = append(list, h)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return filelock.WriteAtomic(s.path, b)
}

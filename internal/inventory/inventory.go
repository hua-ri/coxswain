// Package inventory stores the SSH hosts Coxswain knows about, with tags
// (env=prod drives policy) and a free-text note shown to agents.
package inventory

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/policy"
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

// Store keeps hosts in one JSON file (mode 0600).
type Store struct {
	path string
	mu   sync.Mutex
}

func NewStore(path string) *Store { return &Store{path: path} }

func (s *Store) List() ([]Host, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
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
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
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
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return err
	}
	if err := fn(m); err != nil {
		return err
	}
	return s.save(m)
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
// h itself) whose policy mode is strictest; ties go to the first by name.
func SameAddress(m map[string]Host, h Host) (Host, bool) {
	var same []Host
	var targets []action.Target
	for _, n := range slices.Sorted(maps.Keys(m)) {
		if o := m[n]; n != h.Name && o.Addr() == h.Addr() {
			same = append(same, o)
			targets = append(targets, action.Target{Tags: o.Tags})
		}
	}
	strictest := policy.ModeFor(targets)
	for _, o := range same {
		if policy.ModeFor([]action.Target{{Tags: o.Tags}}) == strictest {
			return o, true
		}
	}
	return Host{}, false
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
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Package argo talks to Argo CD (argocd-server) and Argo Workflows
// (argo-server) instances through their official REST APIs.
package argo

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/hua-ri/coxswain/internal/filelock"
)

// Kind is which Argo an instance runs.
type Kind string

const (
	CD        Kind = "argocd"
	Workflows Kind = "argowf"
)

// Instance is one Argo CD or Argo Workflows server. It never holds
// credentials; those are in the keychain (see Accounts).
type Instance struct {
	Kind               Kind              `json:"kind"`
	Name               string            `json:"name"`
	URL                string            `json:"url"`
	Namespace          string            `json:"namespace,omitempty"` // Workflows: the default namespace
	CAFile             string            `json:"ca_file,omitempty"`
	InsecureSkipVerify bool              `json:"insecure_skip_verify,omitempty"`
	SSOPort            int               `json:"sso_port,omitempty"` // Argo CD browser login callback port
	Tags               map[string]string `json:"tags,omitempty"`
	Note               string            `json:"note,omitempty"`
}

// ID is how actions and targets name it: argocd:prod.
func (i Instance) ID() string { return string(i.Kind) + ":" + i.Name }

// Account is the keychain account of the instance's credential.
func (i Instance) Account() string { return string(i.Kind) + "/" + i.Name }

var namePattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,63}$`)

// Normalize checks an instance and cleans its URL (no trailing slash).
func (i *Instance) Normalize() error {
	if i.Kind != CD && i.Kind != Workflows {
		return fmt.Errorf("kind must be %q or %q", CD, Workflows)
	}
	if !namePattern.MatchString(i.Name) {
		return fmt.Errorf("name %q: use letters, digits, '.', '_' and '-' (up to 64)", i.Name)
	}
	raw := strings.TrimSpace(i.URL)
	if raw != "" && !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("url %q: give http(s)://host[:port][/path]", i.URL)
	}
	i.URL = strings.TrimRight(u.String(), "/")
	i.Namespace = strings.TrimSpace(i.Namespace)
	if i.Kind == CD && i.Namespace != "" {
		return errors.New("namespace is for Argo Workflows instances")
	}
	if i.SSOPort != 0 && (i.Kind != CD || i.SSOPort < 1024 || i.SSOPort > 65535) {
		return errors.New("sso_port is an Argo CD instance's local port for browser login, 1024–65535")
	}
	i.Note = strings.TrimSpace(i.Note)
	if len(i.Tags) == 0 {
		i.Tags = nil
	}
	return nil
}

// Store keeps instances in one JSON file (~/.coxswain/argo.json, mode
// 0600), serialised across processes with a flock on <file>.lock.
type Store struct{ path string }

func NewStore(path string) *Store { return &Store{path: path} }

type file struct {
	Instances []Instance `json:"instances"`
}

// List returns the instances of kind ("" for all), sorted by kind and name.
func (s *Store) List(kind Kind) ([]Instance, error) {
	var out []Instance
	err := filelock.With(s.path+".lock", func() error {
		all, err := s.load()
		for _, i := range all {
			if kind == "" || i.Kind == kind {
				out = append(out, i)
			}
		}
		return err
	})
	return out, err
}

// ErrNoInstance is returned for a name that is not configured.
var ErrNoInstance = errors.New("no such instance")

// Get returns one instance.
func (s *Store) Get(kind Kind, name string) (Instance, error) {
	list, err := s.List(kind)
	if err != nil {
		return Instance{}, err
	}
	for _, i := range list {
		if i.Name == name {
			return i, nil
		}
	}
	return Instance{}, fmt.Errorf("%w: %s %q (see %s.instances)", ErrNoInstance, kind, name, kind)
}

// Update loads the instances, lets fn change them, checks and saves them.
func (s *Store) Update(fn func(list []Instance) ([]Instance, error)) error {
	return filelock.With(s.path+".lock", func() error {
		list, err := s.load()
		if err != nil {
			return err
		}
		list, err = fn(list)
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		for n := range list {
			if err := list[n].Normalize(); err != nil {
				return err
			}
			if seen[list[n].ID()] {
				return fmt.Errorf("%s %q already exists", list[n].Kind, list[n].Name)
			}
			seen[list[n].ID()] = true
		}
		sortInstances(list)
		b, err := json.MarshalIndent(file{Instances: list}, "", "  ")
		if err != nil {
			return err
		}
		return filelock.WriteAtomic(s.path, append(b, '\n'))
	})
}

func sortInstances(list []Instance) {
	sort.Slice(list, func(a, b int) bool {
		if list[a].Kind != list[b].Kind {
			return list[a].Kind < list[b].Kind
		}
		return list[a].Name < list[b].Name
	})
}

func (s *Store) load() ([]Instance, error) {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f file
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", s.path, err)
	}
	return f.Instances, nil
}

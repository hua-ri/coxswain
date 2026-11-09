package k8s

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/hua-ri/coxswain/internal/filelock"
)

// Meta is what the user told Coxswain about a context: tags (env=prod
// marks production) and a note shown to agents.
type Meta struct {
	Tags map[string]string `json:"tags,omitempty"`
	Note string            `json:"note,omitempty"`
}

// MetaStore keeps Meta per context name in one JSON file
// (~/.coxswain/clusters.json, mode 0600), serialised across processes
// with a flock on <file>.lock.
type MetaStore struct{ path string }

func NewMetaStore(path string) *MetaStore { return &MetaStore{path: path} }

// All returns every context's Meta.
func (s *MetaStore) All() (map[string]Meta, error) {
	var out map[string]Meta
	err := filelock.With(s.path+".lock", func() error {
		var err error
		out, err = s.load()
		return err
	})
	return out, err
}

// Get returns a context's Meta (empty if there is none).
func (s *MetaStore) Get(name string) (Meta, error) {
	all, err := s.All()
	return all[name], err
}

// Update loads the map, lets fn change it, and saves it.
func (s *MetaStore) Update(fn func(m map[string]Meta) error) error {
	return filelock.With(s.path+".lock", func() error {
		m, err := s.load()
		if err != nil {
			return err
		}
		if err := fn(m); err != nil {
			return err
		}
		for k, v := range m {
			if len(v.Tags) == 0 && v.Note == "" {
				delete(m, k)
			}
		}
		b, err := json.MarshalIndent(m, "", "  ")
		if err != nil {
			return err
		}
		return filelock.WriteAtomic(s.path, b)
	})
}

func (s *MetaStore) load() (map[string]Meta, error) {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]Meta{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]Meta{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", s.path, err)
	}
	return m, nil
}

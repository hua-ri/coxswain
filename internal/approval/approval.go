// Package approval stores pending approvals on disk so that the process
// that asked (e.g. `cox mcp` under an agent) and the process that decides
// (`cox approve`, a dialog, later the desktop) can be different processes.
package approval

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/filelock"
)

type Status string

const (
	Pending  Status = "pending"
	Approved Status = "approved"
	Denied   Status = "denied"
	Consumed Status = "consumed"
	Expired  Status = "expired"
)

type Approval struct {
	ID        string          `json:"id"`
	ActionID  string          `json:"action"`
	Args      json.RawMessage `json:"args"`
	ArgsHash  string          `json:"args_hash"`
	Actor     action.Actor    `json:"actor"`
	Risk      string          `json:"risk"`
	Targets   []action.Target `json:"targets,omitempty"`
	Preview   string          `json:"preview"`
	Status    Status          `json:"status"`
	CreatedAt time.Time       `json:"created_at"`
	ExpiresAt time.Time       `json:"expires_at"`
	DecidedAt *time.Time      `json:"decided_at,omitempty"`
}

var (
	ErrNotFound   = errors.New("approval not found")
	ErrNotPending = errors.New("approval is not pending")
	idPattern     = regexp.MustCompile(`^ap_[0-9a-f]{12}$`)
)

type Store struct {
	dir string
	ttl time.Duration
	Now func() time.Time
}

func NewStore(dir string, ttl time.Duration) *Store {
	return &Store{dir: dir, ttl: ttl, Now: time.Now}
}

// Hash binds an approval to one exact action and argument set.
func Hash(actionID string, args json.RawMessage) (string, error) {
	var v any
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	if err := json.Unmarshal(args, &v); err != nil {
		return "", err
	}
	canon, err := json.Marshal(v) // map keys are sorted by encoding/json
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte(actionID+"\x00"), canon...))
	return hex.EncodeToString(sum[:]), nil
}

// Create stores a new pending approval and fills in ID, hash and times.
func (s *Store) Create(ap Approval) (Approval, error) {
	h, err := Hash(ap.ActionID, ap.Args)
	if err != nil {
		return Approval{}, err
	}
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return Approval{}, err
	}
	now := s.Now()
	ap.ID = "ap_" + hex.EncodeToString(b[:])
	ap.ArgsHash = h
	ap.Status = Pending
	ap.CreatedAt = now
	ap.ExpiresAt = now.Add(s.ttl)
	return ap, s.withLock(func() error { return s.write(ap) })
}

// Get returns an approval; a pending or approved (not yet used) one past
// its deadline reads as Expired.
func (s *Store) Get(id string) (Approval, error) {
	var ap Approval
	err := s.withLock(func() (err error) { ap, err = s.read(id); return })
	return ap, err
}

// List returns all approvals, oldest first.
func (s *Store) List() ([]Approval, error) {
	var out []Approval
	err := s.withLock(func() error {
		entries, err := os.ReadDir(s.dir)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, e := range entries {
			id, ok := strings.CutSuffix(e.Name(), ".json")
			if !ok || !idPattern.MatchString(id) {
				continue
			}
			ap, err := s.read(id)
			if err != nil {
				return err
			}
			out = append(out, ap)
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, err
}

// Decide approves or denies a pending approval.
func (s *Store) Decide(id string, approve bool) (Approval, error) {
	return s.transition(id, Pending, map[bool]Status{true: Approved, false: Denied}[approve])
}

// Consume marks an approved approval as used. It succeeds at most once,
// so one approval can never run twice. It first checks the stored
// arguments against the hash taken at creation, which detects tampering
// with the args between approval and use; it is no defence against a
// same-user process that rewrites the whole file.
func (s *Store) Consume(id string) (Approval, error) {
	return s.transition(id, Approved, Consumed, func(ap Approval) error {
		h, err := Hash(ap.ActionID, ap.Args)
		if err != nil {
			return err
		}
		if h != ap.ArgsHash {
			return fmt.Errorf("approval %s: arguments changed after approval", id)
		}
		return nil
	})
}

// transition moves an approval from one status to another under the lock;
// check (optional) may veto the change.
func (s *Store) transition(id string, from, to Status, check ...func(Approval) error) (Approval, error) {
	var ap Approval
	err := s.withLock(func() error {
		var err error
		ap, err = s.read(id)
		if err != nil {
			return err
		}
		if ap.Status != from {
			if from == Pending {
				return fmt.Errorf("%w (status: %s)", ErrNotPending, ap.Status)
			}
			return fmt.Errorf("approval %s is %s, not %s", id, ap.Status, from)
		}
		for _, c := range check {
			if err := c(ap); err != nil {
				return err
			}
		}
		now := s.Now()
		ap.Status = to
		if from == Pending {
			ap.DecidedAt = &now
		}
		return s.write(ap)
	})
	return ap, err
}

func (s *Store) path(id string) string { return filepath.Join(s.dir, id+".json") }

func (s *Store) read(id string) (Approval, error) {
	if !idPattern.MatchString(id) {
		return Approval{}, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	b, err := os.ReadFile(s.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return Approval{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return Approval{}, err
	}
	var ap Approval
	if err := json.Unmarshal(b, &ap); err != nil {
		return Approval{}, err
	}
	if (ap.Status == Pending || ap.Status == Approved) && s.Now().After(ap.ExpiresAt) {
		ap.Status = Expired
	}
	return ap, nil
}

func (s *Store) write(ap Approval) error {
	b, err := json.MarshalIndent(ap, "", "  ")
	if err != nil {
		return err
	}
	return filelock.WriteAtomic(s.path(ap.ID), b)
}

// withLock serialises store access across processes with flock(2).
func (s *Store) withLock(fn func() error) error {
	return filelock.With(filepath.Join(s.dir, ".lock"), fn)
}

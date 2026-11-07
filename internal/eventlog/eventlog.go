// Package eventlog is the append-only audit trail: one JSON object per
// line, one file per day, under <home>/events/.
package eventlog

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
)

// Event types.
const (
	ActionRequested = "action.requested"
	ActionFinished  = "action.finished"
	ActionFailed    = "action.failed"
)

type Event struct {
	Time       time.Time       `json:"time"`
	Type       string          `json:"type"`
	Actor      *action.Actor   `json:"actor,omitempty"`
	Action     string          `json:"action,omitempty"`
	Args       json.RawMessage `json:"args,omitempty"`
	Risk       string          `json:"risk,omitempty"`
	Targets    []action.Target `json:"targets,omitempty"`
	DurationMs int64           `json:"duration_ms,omitempty"`
	Error      string          `json:"error,omitempty"`
}

type Log struct {
	dir string
	mu  sync.Mutex
	Now func() time.Time
}

func Open(dir string) *Log { return &Log{dir: dir, Now: time.Now} }

func (l *Log) file(day time.Time) string {
	return filepath.Join(l.dir, day.Format("2006-01-02")+".jsonl")
}

// Append writes one event. Several processes may append to the same
// file; each event is a single write on an O_APPEND descriptor.
func (l *Log) Append(ev Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ev.Time.IsZero() {
		ev.Time = l.Now()
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(l.file(ev.Time), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

// Read returns all events recorded on the given day.
func (l *Log) Read(day time.Time) ([]Event, error) {
	f, err := os.Open(l.file(day))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		var ev Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, sc.Err()
}

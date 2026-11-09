// Package eventlog is the append-only audit trail: one JSON object per
// line, one file per day, under <home>/events/. Subscribers receive every
// appended event live (the activity stream).
package eventlog

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
)

// Event types.
const (
	ActionRequested = "action.requested"
	ActionFinished  = "action.finished"
	ActionFailed    = "action.failed"
	ActionRefused   = "action.refused" // the client is not allowed

	ClientAuthorized = "client.authorized" // the user allowed it in the dialog
	ClientDenied     = "client.denied"     // denied, or the dialog went unanswered
	ClientRevoked    = "client.revoked"
	ClientAllowed    = "client.allowed" // allowed with `cox clients allow`

	TerminalOpened = "terminal.opened" // Actor is the terminal's owner
	TerminalClosed = "terminal.closed"

	// Transfers (file.upload / file.download). Actor is the owner, Detail
	// the transfer. Progress is only published live (see Publish), at
	// most once a second per transfer; a cancelled transfer fails with
	// Reason saying who cancelled it.
	TransferStarted  = "transfer.started"
	TransferProgress = "transfer.progress"
	TransferFinished = "transfer.finished"
	TransferFailed   = "transfer.failed"

	// Tunnels. Actor is the owner, Detail the tunnel.
	TunnelOpened       = "tunnel.opened"
	TunnelReconnecting = "tunnel.reconnecting" // the SSH connection broke; Error says why
	TunnelReconnected  = "tunnel.reconnected"
	TunnelClosed       = "tunnel.closed" // Reason says why
	// A connection through the tunnel failed (the host refused the
	// forward, or the target refused the connection); Error says why. At
	// most one per tunnel every 10 seconds; Detail counts them all.
	TunnelConnectionFailed = "tunnel.connection_failed"

	// Batch runs (ssh.batch). Actor is the caller.
	BatchStarted  = "batch.started"
	BatchHostDone = "batch.host_done"
	BatchFinished = "batch.finished"
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
	Client     string          `json:"client,omitempty"` // client.* events
	Reason     string          `json:"reason,omitempty"` // why it was refused, or why a terminal closed
	Error      string          `json:"error,omitempty"`
	Terminal   string          `json:"terminal,omitempty"`  // terminal.* events: the session id
	ExitCode   *int            `json:"exit_code,omitempty"` // terminal.closed: the shell's exit code, if known
	Transfer   string          `json:"transfer,omitempty"`  // transfer.* events: the transfer id
	Tunnel     string          `json:"tunnel,omitempty"`    // tunnel.* events: the tunnel id
	Batch      string          `json:"batch,omitempty"`     // batch.* events: the batch run id
	// Detail is the object the event is about (a transfer, a tunnel, a
	// batch host result), as JSON.
	Detail json.RawMessage `json:"detail,omitempty"`
}

// subscriberBuffer is how many events a subscriber may fall behind before
// further events are dropped for it.
const subscriberBuffer = 256

type Log struct {
	dir     string
	mu      sync.Mutex
	subs    map[chan Event]struct{}
	dropped atomic.Int64
	Now     func() time.Time
}

func Open(dir string) *Log { return &Log{dir: dir, Now: time.Now} }

func (l *Log) file(day time.Time) string {
	return filepath.Join(l.dir, day.Format("2006-01-02")+".jsonl")
}

// Subscribe returns a channel that receives every event appended from now
// on, in order. A subscriber that falls behind loses events rather than
// slowing Append down. cancel stops delivery and closes the channel.
func (l *Log) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, subscriberBuffer)
	l.mu.Lock()
	if l.subs == nil {
		l.subs = map[chan Event]struct{}{}
	}
	l.subs[ch] = struct{}{}
	l.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			l.mu.Lock()
			delete(l.subs, ch)
			l.mu.Unlock()
			close(ch)
		})
	}
}

// Dropped reports how many events slow subscribers have missed.
func (l *Log) Dropped() int64 { return l.dropped.Load() }

// Append writes one event, then delivers it to subscribers (even if the
// write failed, so the live view still shows it). Several processes may
// append to the same file; each event is a single write on an O_APPEND
// descriptor.
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
	defer l.deliver(ev)
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

// Publish delivers ev to subscribers without writing it to the audit log,
// for live updates such as transfer progress.
func (l *Log) Publish(ev Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ev.Time.IsZero() {
		ev.Time = l.Now()
	}
	l.deliver(ev)
}

// deliver sends ev to every subscriber without blocking. l.mu is held.
func (l *Log) deliver(ev Event) {
	for ch := range l.subs {
		select {
		case ch <- ev:
		default:
			l.dropped.Add(1)
		}
	}
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

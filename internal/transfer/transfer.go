// Package transfer keeps the file transfers (uploads and downloads) of
// `cox server`: who started them, how far they are, and a way for the
// user to cancel them. The transfer itself runs in the caller (an Action);
// the Manager records it and publishes its progress.
package transfer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/eventlog"
)

var (
	ErrNotFound = errors.New("transfer not found")
	ErrFinished = errors.New("transfer has already finished")
)

// Directions.
const (
	Upload   = "upload"
	Download = "download"
)

// Statuses.
const (
	Running   = "running"
	Done      = "done"
	Failed    = "failed"
	Cancelled = "cancelled"
)

// Transfer describes one upload or download.
type Transfer struct {
	ID         string       `json:"id"`
	Direction  string       `json:"direction"` // upload or download
	Host       string       `json:"host"`
	LocalPath  string       `json:"local_path"`
	RemotePath string       `json:"remote_path"`
	Owner      action.Actor `json:"owner"` // who started it
	Status     string       `json:"status"`
	// Bytes is how much of the file is at the destination, including
	// ResumedFrom bytes that were already there.
	Bytes       int64      `json:"bytes"`
	Total       int64      `json:"total"`
	ResumedFrom int64      `json:"resumed_from,omitempty"`
	SHA256      string     `json:"sha256,omitempty"`
	Verified    *bool      `json:"verified,omitempty"` // upload: whether the remote checksum was compared
	StartedAt   time.Time  `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	Error       string     `json:"error,omitempty"`
}

// Manager records transfers.
type Manager struct {
	Events *eventlog.Log // may be nil
	Now    func() time.Time
	// ProgressEvery is the least time between two progress events of one
	// transfer; default 1s.
	ProgressEvery time.Duration
	// Keep is how many finished transfers List keeps showing; default 100.
	Keep int

	mu    sync.Mutex
	items map[string]*item
	seq   uint64
	wg    sync.WaitGroup
}

type item struct {
	seq      uint64 // start order
	info     Transfer
	targets  []action.Target
	cancel   context.CancelCauseFunc
	lastSent time.Time
	sent     bool
}

// Progress is how a running transfer reports what it did.
type Progress struct {
	m  *Manager
	it *item
}

// Start sets where the transfer begins (bytes already at the
// destination) and the total size.
func (p *Progress) Start(offset, total int64) {
	p.Update(func(t *Transfer) { t.ResumedFrom, t.Bytes, t.Total = offset, offset, total })
}

// Add records n more bytes transferred and publishes a progress event
// unless one was published less than ProgressEvery ago.
func (p *Progress) Add(n int64) {
	m := p.m
	m.mu.Lock()
	p.it.info.Bytes += n
	now := m.now()
	if p.it.sent && now.Sub(p.it.lastSent) < m.progressEvery() {
		m.mu.Unlock()
		return
	}
	p.it.sent, p.it.lastSent = true, now
	ev := m.event(eventlog.TransferProgress, p.it)
	m.mu.Unlock()
	if m.Events != nil {
		m.Events.Publish(ev)
	}
}

// Update changes the transfer's record.
func (p *Progress) Update(f func(*Transfer)) {
	p.m.mu.Lock()
	f(&p.it.info)
	p.m.mu.Unlock()
}

// Run records t (its ID, Status and times are set here), runs work and
// records how it ended. work's context ends when the caller's does, or
// when the transfer is cancelled; the cause says why.
func (m *Manager) Run(ctx context.Context, t Transfer, targets []action.Target, work func(context.Context, *Progress) error) (Transfer, error) {
	id, err := newID()
	if err != nil {
		return t, err
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	t.ID, t.Status, t.StartedAt, t.FinishedAt, t.Error = id, Running, m.now().UTC(), nil, ""
	it := &item{info: t, targets: targets, cancel: cancel}
	m.mu.Lock()
	m.seq++
	it.seq = m.seq
	if m.items == nil {
		m.items = map[string]*item{}
	}
	m.items[id] = it
	m.wg.Add(1)
	started := m.event(eventlog.TransferStarted, it)
	m.mu.Unlock()
	defer m.wg.Done()
	m.append(started)

	// A transfer that completed is done, even if it was cancelled as it
	// finished.
	err = work(ctx, &Progress{m: m, it: it})
	m.mu.Lock()
	finished := m.now().UTC()
	it.info.FinishedAt = &finished
	it.cancel = nil
	typ := eventlog.TransferFinished
	switch {
	case err == nil:
		it.info.Status = Done
	case ctx.Err() != nil:
		err = context.Cause(ctx)
		it.info.Status, it.info.Error, typ = Cancelled, err.Error(), eventlog.TransferFailed
	default:
		it.info.Status, it.info.Error, typ = Failed, err.Error(), eventlog.TransferFailed
	}
	ev := m.event(typ, it)
	if it.info.Status == Cancelled {
		ev.Reason = it.info.Error
	} else {
		ev.Error = it.info.Error
	}
	out := it.info
	m.prune()
	m.mu.Unlock()
	m.append(ev)
	return out, err
}

// Get returns a transfer, running or recently finished.
func (m *Manager) Get(id string) (Transfer, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	it, ok := m.items[id]
	if !ok {
		return Transfer{}, false
	}
	return it.info, true
}

// List returns the running and recently finished transfers, oldest first.
func (m *Manager) List() []Transfer {
	m.mu.Lock()
	items := m.sorted(func(*item) bool { return true })
	out := make([]Transfer, len(items))
	for i, it := range items {
		out[i] = it.info
	}
	m.mu.Unlock()
	return out
}

// sorted returns the items that match, oldest first. m.mu is held.
func (m *Manager) sorted(match func(*item) bool) []*item {
	var out []*item
	for _, it := range m.items {
		if match(it) {
			out = append(out, it)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].seq < out[j].seq })
	return out
}

// Cancel stops a running transfer on behalf of by. It does not wait.
func (m *Manager) Cancel(id string, by action.Actor) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	it, ok := m.items[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if it.cancel == nil {
		return fmt.Errorf("%w: %s", ErrFinished, id)
	}
	it.cancel(errors.New("cancelled by " + by.String()))
	return nil
}

// CancelAll stops every running transfer and waits until they are
// recorded; the server calls it on shutdown.
func (m *Manager) CancelAll() {
	m.mu.Lock()
	for _, it := range m.items {
		if it.cancel != nil {
			it.cancel(errors.New("server stopped"))
		}
	}
	m.mu.Unlock()
	m.wg.Wait()
}

// prune forgets the oldest finished transfers beyond Keep. m.mu is held.
func (m *Manager) prune() {
	done := m.sorted(func(it *item) bool { return it.info.Status != Running })
	keep := m.Keep
	if keep <= 0 {
		keep = 100
	}
	if len(done) <= keep {
		return
	}
	for _, it := range done[:len(done)-keep] {
		delete(m.items, it.info.ID)
	}
}

// event describes it for the activity stream. m.mu is held.
func (m *Manager) event(typ string, it *item) eventlog.Event {
	owner := it.info.Owner
	detail, _ := json.Marshal(it.info)
	return eventlog.Event{Type: typ, Actor: &owner, Transfer: it.info.ID, Targets: it.targets, Detail: detail}
}

func (m *Manager) append(ev eventlog.Event) {
	if m.Events != nil {
		m.Events.Append(ev)
	}
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) progressEvery() time.Duration {
	if m.ProgressEvery > 0 {
		return m.ProgressEvery
	}
	return time.Second
}

func newID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "x_" + hex.EncodeToString(b[:]), nil
}

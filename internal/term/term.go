// Package term keeps shared terminal sessions in `cox server`: a person
// (desktop, CLI) and agents can open, watch, type into and read the same
// remote shells, on SSH hosts or in Kubernetes pods.
package term

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/eventlog"
	"github.com/hua-ri/coxswain/internal/inventory"
	"github.com/hua-ri/coxswain/internal/sshx"
	"golang.org/x/crypto/ssh"
)

var (
	ErrNotFound = errors.New("terminal not found")
	ErrClosed   = errors.New("terminal is closed")
	ErrSize     = errors.New("terminal size must be 1..1000 columns and rows")
	ErrTooMany  = errors.New("too many open terminals; close some first")
	// ErrNoPods: pod terminals are not available (no PodOpener).
	ErrNoPods  = errors.New("pod terminals are not available")
	ErrPodSpec = errors.New("a pod terminal needs context and pod")
)

const (
	// backlogSize is how much recent output each session keeps.
	backlogSize = 256 << 10
	// subscriberBuffer is how many chunks a subscriber may fall behind
	// before the oldest are dropped.
	subscriberBuffer = 256
	defaultCols      = 80
	defaultRows      = 24
	maxSize          = 1000
)

// Kinds of terminals.
const (
	KindSSH = "ssh" // a login shell on a saved SSH host
	KindPod = "pod" // a shell in a pod's container
)

// Session describes one terminal.
type Session struct {
	ID   string `json:"id"`
	Kind string `json:"kind"` // ssh or pod
	Host string `json:"host"` // ssh
	// pod terminals: where the shell runs.
	Context   string       `json:"context,omitempty"`
	Namespace string       `json:"namespace,omitempty"`
	Pod       string       `json:"pod,omitempty"`
	Container string       `json:"container,omitempty"`
	Owner     action.Actor `json:"owner"` // who opened it
	CreatedAt time.Time    `json:"created_at"`
	Cols      int          `json:"cols"`
	Rows      int          `json:"rows"`
	Closed    bool         `json:"closed"`
	ExitCode  *int         `json:"exit_code,omitempty"`
}

// Chunk is a piece of a session's output for one subscriber.
type Chunk struct {
	Data []byte
	// Gap reports that output before this chunk was dropped because the
	// subscriber fell behind.
	Gap bool
	// Exit marks the last chunk: the session ended, with exit code Code
	// if the shell reported one.
	Exit bool
	Code *int
}

// Backend is the process behind a terminal.
type Backend interface {
	Write(p []byte) (int, error) // input (keystrokes)
	Resize(cols, rows int) error
	// Wait blocks until the process ends and returns its exit code, or
	// nil if there is none (e.g. it was closed).
	Wait() *int
	Close() error // ends the process
}

// PodSpec is a shell in a pod.
type PodSpec struct {
	Context   string `json:"context"`
	Namespace string `json:"namespace,omitempty"` // default: the context's
	Pod       string `json:"pod"`
	Container string `json:"container,omitempty"` // required if the pod has several
	Shell     string `json:"shell,omitempty"`     // default sh; falls back to sh if missing
}

// PodOpener starts shells in pods.
type PodOpener interface {
	// OpenPod starts an interactive shell with a TTY of cols×rows in the
	// pod, writing its output to out, and returns it with spec completed
	// (namespace, container).
	OpenPod(ctx context.Context, spec PodSpec, cols, rows int, out io.Writer) (Backend, PodSpec, error)
	// Target is the audit target of a namespace of a context ("" is the
	// context's default namespace).
	Target(context, namespace string) action.Target
}

// Manager holds the sessions. Each SSH session has its own SSH
// connection (not the pool's), a pty and a login shell; each pod session
// its own exec stream with a TTY.
type Manager struct {
	Hosts  *inventory.Store
	Dialer *sshx.Dialer
	Pods   PodOpener     // pod terminals; nil disables them
	Events *eventlog.Log // terminal.opened / terminal.closed; may be nil
	// Retain is how long an ended session's output stays readable;
	// default 10 minutes. Sessions closed with Close go at once.
	Retain time.Duration
	// MaxOpen bounds the open sessions; default 32.
	MaxOpen int

	mu       sync.Mutex
	sessions map[string]*session
}

// OpenHost opens a terminal on the saved host name.
func (m *Manager) OpenHost(ctx context.Context, name string, owner action.Actor, cols, rows int) (Session, error) {
	h, err := m.Hosts.Get(name)
	if err != nil {
		return Session{}, err
	}
	return m.Open(ctx, h, owner, cols, rows)
}

// Open connects to h, requests an xterm-256color pty of cols×rows (0
// means 80×24) and starts a shell.
func (m *Manager) Open(ctx context.Context, h inventory.Host, owner action.Actor, cols, rows int) (Session, error) {
	s, err := m.newSession(owner, &cols, &rows)
	if err != nil {
		return Session{}, err
	}
	s.info.Kind, s.info.Host = KindSSH, h.Name
	s.target = action.Target{Kind: "host", ID: h.Name, Tags: h.Tags}
	conn, err := m.Dialer.Dial(ctx, h)
	if err != nil {
		return Session{}, err
	}
	b, err := startSSH(conn, cols, rows, writerFunc(s.output))
	if err != nil {
		conn.Close()
		return Session{}, fmt.Errorf("start shell on %s: %w", h.Name, err)
	}
	return m.add(s, b), nil
}

// OpenPod starts a shell (TTY of cols×rows, 0 means 80×24) in a pod.
func (m *Manager) OpenPod(ctx context.Context, spec PodSpec, owner action.Actor, cols, rows int) (Session, error) {
	if m.Pods == nil {
		return Session{}, ErrNoPods
	}
	if spec.Context == "" || spec.Pod == "" {
		return Session{}, ErrPodSpec
	}
	s, err := m.newSession(owner, &cols, &rows)
	if err != nil {
		return Session{}, err
	}
	b, spec, err := m.Pods.OpenPod(ctx, spec, cols, rows, writerFunc(s.output))
	if err != nil {
		return Session{}, err
	}
	s.info.Kind, s.info.Context, s.info.Namespace, s.info.Pod, s.info.Container = KindPod, spec.Context, spec.Namespace, spec.Pod, spec.Container
	s.target = m.Pods.Target(spec.Context, spec.Namespace)
	return m.add(s, b), nil
}

// newSession checks the size (0×0 means 80×24) and the limit and returns
// a session yet to be started.
func (m *Manager) newSession(owner action.Actor, cols, rows *int) (*session, error) {
	if *cols == 0 && *rows == 0 {
		*cols, *rows = defaultCols, defaultRows
	}
	if err := checkSize(*cols, *rows); err != nil {
		return nil, err
	}
	if m.openCount() >= m.maxOpen() {
		return nil, ErrTooMany
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	return &session{
		info: Session{ID: id, Owner: owner, CreatedAt: time.Now().UTC(), Cols: *cols, Rows: *rows},
		done: make(chan struct{}),
	}, nil
}

// add registers a started session.
func (m *Manager) add(s *session, b Backend) Session {
	s.backend = b
	m.mu.Lock()
	if m.sessions == nil {
		m.sessions = map[string]*session{}
	}
	m.sessions[s.info.ID] = s
	m.mu.Unlock()
	owner := s.info.Owner
	info := s.snapshot()
	detail, _ := json.Marshal(info)
	m.log(eventlog.Event{Type: eventlog.TerminalOpened, Actor: &owner, Terminal: info.ID, Targets: s.targets(), Detail: detail})
	go m.wait(s)
	return info
}

// wait records the end of a session.
func (m *Manager) wait(s *session) {
	code := s.backend.Wait()
	reason := s.finish(code)
	owner := s.info.Owner
	m.log(eventlog.Event{Type: eventlog.TerminalClosed, Actor: &owner, Terminal: s.info.ID,
		Targets: s.targets(), ExitCode: code, Reason: reason})
	close(s.done)
	if reason != "exited" {
		m.remove(s.info.ID)
		return
	}
	time.AfterFunc(m.retain(), func() { m.remove(s.info.ID) })
}

func exitCode(err error) *int {
	var exitErr *ssh.ExitError
	if err == nil {
		code := 0
		return &code
	}
	if errors.As(err, &exitErr) {
		code := exitErr.ExitStatus()
		return &code
	}
	return nil
}

// Get returns a session, open or recently ended.
func (m *Manager) Get(id string) (Session, bool) {
	s := m.get(id)
	if s == nil {
		return Session{}, false
	}
	return s.snapshot(), true
}

// List returns all sessions, oldest first.
func (m *Manager) List() []Session {
	m.mu.Lock()
	out := make([]Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s.snapshot())
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Write sends input (keystrokes) to a session.
func (m *Manager) Write(id string, p []byte) error {
	s, err := m.open(id)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.backend.Write(p); err != nil {
		return fmt.Errorf("%w: %v", ErrClosed, err)
	}
	return nil
}

// Resize changes a session's pty size.
func (m *Manager) Resize(id string, cols, rows int) error {
	if err := checkSize(cols, rows); err != nil {
		return err
	}
	s, err := m.open(id)
	if err != nil {
		return err
	}
	if err := s.backend.Resize(cols, rows); err != nil {
		return err
	}
	s.mu.Lock()
	s.info.Cols, s.info.Rows = cols, rows
	s.mu.Unlock()
	return nil
}

// Subscribe returns the output so far and a channel of what follows. The
// channel ends with an Exit chunk and is then closed; cancel stops
// delivery early (and may be called more than once).
func (m *Manager) Subscribe(id string) (backlog []byte, ch <-chan Chunk, cancel func(), err error) {
	s := m.get(id)
	if s == nil {
		return nil, nil, nil, ErrNotFound
	}
	backlog, ch, cancel = s.subscribe()
	return backlog, ch, cancel, nil
}

// Text returns the last n lines of a session's output as plain text (see
// Plain).
func (m *Manager) Text(id string, n int) (string, error) {
	s := m.get(id)
	if s == nil {
		return "", ErrNotFound
	}
	s.mu.Lock()
	out, wrapped := s.backlog(), s.wrapped
	s.mu.Unlock()
	return lastLines(Plain(out), n, wrapped), nil
}

// Close ends a session for by and forgets it.
func (m *Manager) Close(id string, by action.Actor) error {
	return m.close(id, "closed by "+by.String())
}

// CloseAll ends every session; the server calls it on shutdown.
func (m *Manager) CloseAll() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.close(id, "server stopped")
	}
}

func (m *Manager) close(id, reason string) error {
	s := m.get(id)
	if s == nil {
		return ErrNotFound
	}
	s.mu.Lock()
	if !s.info.Closed && s.reason == "" {
		s.reason = reason
	}
	s.mu.Unlock()
	s.backend.Close()
	<-s.done
	m.remove(id)
	return nil
}

func (m *Manager) get(id string) *session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}

// open returns a session that has not ended.
func (m *Manager) open(id string) (*session, error) {
	s := m.get(id)
	if s == nil {
		return nil, ErrNotFound
	}
	s.mu.Lock()
	closed := s.info.Closed
	s.mu.Unlock()
	if closed {
		return nil, ErrClosed
	}
	return s, nil
}

func (m *Manager) remove(id string) {
	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()
}

func (m *Manager) openCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, s := range m.sessions {
		s.mu.Lock()
		if !s.info.Closed {
			n++
		}
		s.mu.Unlock()
	}
	return n
}

func (m *Manager) log(ev eventlog.Event) {
	if m.Events != nil {
		m.Events.Append(ev)
	}
}

func (m *Manager) retain() time.Duration {
	if m.Retain > 0 {
		return m.Retain
	}
	return 10 * time.Minute
}

func (m *Manager) maxOpen() int {
	if m.MaxOpen > 0 {
		return m.MaxOpen
	}
	return 32
}

func checkSize(cols, rows int) error {
	if cols < 1 || rows < 1 || cols > maxSize || rows > maxSize {
		return ErrSize
	}
	return nil
}

func newID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "t_" + hex.EncodeToString(b[:]), nil
}

// session is one terminal and its subscribers.
type session struct {
	target  action.Target // the host, or the pod's namespace
	backend Backend
	done    chan struct{} // closed once the end is recorded

	writeMu sync.Mutex

	mu      sync.Mutex
	info    Session
	reason  string // why it is being closed, if by Close
	out     []byte // recent output; the last backlogSize bytes count
	wrapped bool   // older output was dropped
	subs    map[*subscriber]struct{}
}

type subscriber struct{ ch chan Chunk }

// sshShell is a login shell on an SSH host.
type sshShell struct {
	conn  *sshx.Conn
	ss    *ssh.Session
	stdin io.WriteCloser
}

// startSSH requests an xterm-256color pty and a shell on conn.
func startSSH(conn *sshx.Conn, cols, rows int, out io.Writer) (*sshShell, error) {
	ss, err := conn.NewSession()
	if err != nil {
		return nil, err
	}
	b := &sshShell{conn: conn, ss: ss}
	if b.stdin, err = ss.StdinPipe(); err != nil {
		return nil, err
	}
	ss.Stdout, ss.Stderr = out, out
	if err := ss.RequestPty("xterm-256color", rows, cols, ssh.TerminalModes{}); err != nil {
		return nil, err
	}
	return b, ss.Shell()
}

func (b *sshShell) Write(p []byte) (int, error) { return b.stdin.Write(p) }
func (b *sshShell) Resize(cols, rows int) error { return b.ss.WindowChange(rows, cols) }

func (b *sshShell) Wait() *int {
	code := exitCode(b.ss.Wait())
	b.conn.Close()
	return code
}

func (b *sshShell) Close() error {
	b.ss.Close()
	return b.conn.Close()
}

func (s *session) targets() []action.Target { return []action.Target{s.target} }

func (s *session) snapshot() Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.info
}

// backlog returns a copy of the recent output. s.mu is held.
func (s *session) backlog() []byte {
	b := s.out[max(len(s.out)-backlogSize, 0):]
	return append([]byte(nil), b...)
}

// output records p and sends it to every subscriber.
func (s *session) output(p []byte) {
	if len(p) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.out = append(s.out, p...)
	if len(s.out) > 2*backlogSize {
		s.out = append(s.out[:0], s.out[len(s.out)-backlogSize:]...)
	}
	if len(s.out) > backlogSize {
		s.wrapped = true
	}
	data := append([]byte(nil), p...)
	for sub := range s.subs {
		sub.send(Chunk{Data: data})
	}
}

// send delivers c without blocking: if the subscriber is full, its oldest
// chunk is dropped. Only the session (holding s.mu) sends.
func (sub *subscriber) send(c Chunk) {
	select {
	case sub.ch <- c:
		return
	default:
	}
	select {
	case <-sub.ch:
	default:
	}
	c.Gap = true
	select {
	case sub.ch <- c:
	default:
	}
}

// finish marks the session ended, tells subscribers and returns why it
// ended ("exited" if the shell ended by itself).
func (s *session) finish(code *int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.info.Closed, s.info.ExitCode = true, code
	for sub := range s.subs {
		sub.send(Chunk{Exit: true, Code: code})
		close(sub.ch)
	}
	s.subs = nil
	if s.reason == "" {
		return "exited"
	}
	return s.reason
}

func (s *session) subscribe() ([]byte, <-chan Chunk, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	backlog := s.backlog()
	sub := &subscriber{ch: make(chan Chunk, subscriberBuffer)}
	if s.info.Closed {
		sub.ch <- Chunk{Exit: true, Code: s.info.ExitCode}
		close(sub.ch)
		return backlog, sub.ch, func() {}
	}
	if s.subs == nil {
		s.subs = map[*subscriber]struct{}{}
	}
	s.subs[sub] = struct{}{}
	return backlog, sub.ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, ok := s.subs[sub]; ok {
			delete(s.subs, sub)
			close(sub.ch)
		}
	}
}

type writerFunc func([]byte)

func (f writerFunc) Write(p []byte) (int, error) {
	f(p)
	return len(p), nil
}

package clients

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/hua-ri/coxswain/internal/confirm"
)

// NotAuthorized is the error returned to a client the user has not allowed.
const NotAuthorized = "client_not_authorized"

// NotAuthorizedHint tells an agent what to do when the user has not
// allowed it.
func NotAuthorizedHint(client string) string {
	return "The user has not allowed " + client + " to use Coxswain. Tell the user; they can allow it " +
		"in the dialog that appears when you reconnect. Do not try to reach the servers another way."
}

// BuiltIn reports whether name is one of the user's own entry points
// (`cox` commands and the desktop app), which never need authorization.
func BuiltIn(name string) bool { return name == "cli" || name == "desktop" }

// Authorizer decides whether a client may use Coxswain, asking the user
// the first time a client connects.
type Authorizer struct {
	Store *Store
	// Ask asks the user whether name may use Coxswain; nil denies.
	Ask func(ctx context.Context, name string) (bool, error)

	mu       sync.Mutex
	inflight map[string]*prompt
	onRevoke []func(name string)
}

// prompt is one question to the user that concurrent callers share.
type prompt struct {
	done chan struct{}
	ok   bool
	err  error
}

// Authorize reports whether the client may use Coxswain. Built-in and
// trusted clients pass; otherwise the user is asked once (concurrent
// callers for the same name share the answer) and an approval is
// remembered. A denial is not.
func (a *Authorizer) Authorize(ctx context.Context, name string) (bool, error) {
	if !ValidName(name) {
		return false, fmt.Errorf("%w: %q", ErrInvalidName, name)
	}
	if BuiltIn(name) {
		return true, nil
	}
	if ok, err := a.Store.IsTrusted(name); ok || err != nil {
		return ok, err
	}

	a.mu.Lock()
	p, waiting := a.inflight[name]
	if !waiting {
		p = &prompt{done: make(chan struct{})}
		if a.inflight == nil {
			a.inflight = map[string]*prompt{}
		}
		a.inflight[name] = p
		// The prompt outlives a caller that gives up; others may wait on it.
		go a.ask(context.WithoutCancel(ctx), name, p)
	}
	a.mu.Unlock()

	select {
	case <-p.done:
		return p.ok, p.err
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func (a *Authorizer) ask(ctx context.Context, name string, p *prompt) {
	if a.Ask != nil {
		p.ok, p.err = a.Ask(ctx, name)
		if p.err != nil {
			p.ok = false
		}
	}
	if p.ok {
		p.err = a.Store.Trust(name)
		p.ok = p.err == nil
	}
	a.mu.Lock()
	delete(a.inflight, name)
	a.mu.Unlock()
	close(p.done)
}

// OnRevoke registers fn to run after a client is revoked, e.g. to close
// its sessions.
func (a *Authorizer) OnRevoke(fn func(name string)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.onRevoke = append(a.onRevoke, fn)
}

// Revoke forgets a trusted client and notifies OnRevoke callbacks.
func (a *Authorizer) Revoke(name string) error {
	if BuiltIn(name) {
		return fmt.Errorf("%s is built in and cannot be revoked", name)
	}
	if err := a.Store.Revoke(name); err != nil {
		return err
	}
	a.mu.Lock()
	fns := append([]func(string){}, a.onRevoke...)
	a.mu.Unlock()
	for _, fn := range fns {
		fn(name)
	}
	return nil
}

// Message is the question shown to the user for a new client.
func Message(name string) string {
	return confirm.Sanitize(name) + " wants to use Coxswain to operate your servers."
}

// Dialog returns an Ask that shows a macOS dialog; nobody answering
// within timeout denies.
func Dialog(timeout time.Duration) func(ctx context.Context, name string) (bool, error) {
	return func(ctx context.Context, name string) (bool, error) {
		return confirm.Ask(ctx, "Coxswain", Message(name), timeout)
	}
}

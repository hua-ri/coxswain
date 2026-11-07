package clients

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/hua-ri/coxswain/internal/confirm"
	"github.com/hua-ri/coxswain/internal/eventlog"
)

// NotAuthorized is the error returned to a client the user has not allowed.
const NotAuthorized = "client_not_authorized"

// Reasons a client is refused.
const (
	ReasonRevoked     = "revoked"      // the user revoked it
	ReasonDenied      = "denied"       // denied or unanswered dialog (or no dialog possible)
	ReasonInvalidName = "invalid name" // not a valid client name
)

// NotAuthorizedHint tells an agent what to do when it was refused.
func NotAuthorizedHint(client, reason string) string {
	if reason == ReasonRevoked {
		return "The user revoked " + client + "'s access to Coxswain. Tell the user; they can allow it again with: " +
			"cox clients allow " + client + ". Do not try to reach the servers another way."
	}
	return "The user has not allowed " + client + " to use Coxswain. Tell the user; they can allow it with: " +
		"cox clients allow " + client + ". Do not try to reach the servers another way."
}

// BuiltIn reports whether name is one of the user's own entry points
// (`cox` commands and the desktop app), which never need authorization.
func BuiltIn(name string) bool { return name == "cli" || name == "desktop" }

// Authorizer decides whether a client may use Coxswain, asking the user
// the first time a client connects. The store is checked on every call,
// so `cox clients allow|revoke` take effect at once.
type Authorizer struct {
	Store *Store
	// Ask asks the user whether name may use Coxswain; nil denies.
	Ask func(ctx context.Context, name string) (bool, error)
	// DenyFor is how long a denial stops further dialogs for that name
	// (default 60s).
	DenyFor time.Duration
	Now     func() time.Time
	// Events, if set, records the user's decisions about clients.
	Events *eventlog.Log

	mu       sync.Mutex
	inflight map[string]*prompt
	deniedAt map[string]time.Time
	onRevoke []func(name string)
}

// prompt is one question to the user that concurrent callers share.
type prompt struct {
	done chan struct{}
	ok   bool
}

// Authorize reports whether the client may use Coxswain, and if not, why.
// Built-in and allowed clients pass; revoked ones are refused without a
// dialog; otherwise the user is asked (concurrent callers for one name
// share the dialog). An approval is stored; a denial only suppresses
// dialogs for DenyFor.
func (a *Authorizer) Authorize(ctx context.Context, name string) (bool, string) {
	if !ValidName(name) {
		return false, ReasonInvalidName
	}
	if BuiltIn(name) {
		return true, ""
	}
	switch st, err := a.Store.Status(name); {
	case err != nil:
		return false, ReasonDenied
	case st == Trusted:
		return true, ""
	case st == Revoked:
		return false, ReasonRevoked
	}

	a.mu.Lock()
	if t, ok := a.deniedAt[name]; ok && a.now().Sub(t) < a.denyFor() {
		a.mu.Unlock()
		return false, ReasonDenied
	}
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
		if p.ok {
			return true, ""
		}
		return false, ReasonDenied
	case <-ctx.Done():
		return false, ReasonDenied
	}
}

func (a *Authorizer) ask(ctx context.Context, name string, p *prompt) {
	if a.Ask != nil {
		ok, err := a.Ask(ctx, name)
		p.ok = ok && err == nil
	}
	if p.ok {
		p.ok = a.Store.Trust(name) == nil
	}
	if p.ok {
		a.audit(eventlog.ClientAuthorized, name)
	} else {
		a.audit(eventlog.ClientDenied, name)
	}
	a.mu.Lock()
	delete(a.inflight, name)
	if !p.ok {
		if a.deniedAt == nil {
			a.deniedAt = map[string]time.Time{}
		}
		a.deniedAt[name] = a.now()
	}
	a.mu.Unlock()
	close(p.done)
}

// Allow records the user's decision to let name use Coxswain.
func (a *Authorizer) Allow(name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	if err := a.Store.Trust(name); err != nil {
		return err
	}
	a.mu.Lock()
	delete(a.deniedAt, name)
	a.mu.Unlock()
	a.audit(eventlog.ClientAllowed, name)
	return nil
}

func (a *Authorizer) audit(typ, name string) {
	if a.Events != nil {
		a.Events.Append(eventlog.Event{Type: typ, Client: name})
	}
}

// OnRevoke registers fn to run after a client is revoked, e.g. to close
// its sessions.
func (a *Authorizer) OnRevoke(fn func(name string)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.onRevoke = append(a.onRevoke, fn)
}

// Revoke records name as revoked and notifies OnRevoke callbacks.
func (a *Authorizer) Revoke(name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	if err := a.Store.Revoke(name); err != nil {
		return err
	}
	a.audit(eventlog.ClientRevoked, name)
	a.mu.Lock()
	fns := append([]func(string){}, a.onRevoke...)
	a.mu.Unlock()
	for _, fn := range fns {
		fn(name)
	}
	return nil
}

// checkName accepts agent names, not built-in ones.
func checkName(name string) error {
	if !ValidName(name) {
		return fmt.Errorf("%w: %q", ErrInvalidName, name)
	}
	if BuiltIn(name) {
		return fmt.Errorf("%w: %s needs no permission", ErrBuiltIn, name)
	}
	return nil
}

func (a *Authorizer) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Authorizer) denyFor() time.Duration {
	if a.DenyFor > 0 {
		return a.DenyFor
	}
	return time.Minute
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

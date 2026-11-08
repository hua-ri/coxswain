// Package action defines the unit every Coxswain capability is built from.
// An Action is registered once and exposed through every surface (CLI, MCP).
package action

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
)

// Risk is how much damage an invocation can do. Higher is riskier.
type Risk int

const (
	Read Risk = iota
	Write
	Destructive
)

func (r Risk) String() string {
	switch r {
	case Read:
		return "read"
	case Write:
		return "write"
	default:
		return "destructive"
	}
}

func (r Risk) MarshalText() ([]byte, error) { return []byte(r.String()), nil }

// ActorKind tells humans and agents apart; agents get shaped output.
type ActorKind string

const (
	Human ActorKind = "human"
	Agent ActorKind = "agent"
)

// Actor is who asked for an invocation.
type Actor struct {
	Kind    ActorKind `json:"kind"`
	Name    string    `json:"name"`
	Session string    `json:"session,omitempty"`
}

func (a Actor) String() string { return string(a.Kind) + ":" + a.Name }

// Target is an object an invocation affects, with its tags (env=prod).
type Target struct {
	Kind string            `json:"kind"`
	ID   string            `json:"id"`
	Tags map[string]string `json:"tags,omitempty"`
}

// Assessment describes one concrete invocation before it runs: how risky
// it is, what it touches, and a one-line preview. It is purely a label for
// the audit log and the activity stream; nothing is blocked on it.
type Assessment struct {
	Risk    Risk
	Targets []Target
	Preview string
}

// Annotations are static hints about a tool, passed to agents (MCP tool
// annotations) so their own permission settings can decide whether to ask.
type Annotations struct {
	ReadOnly    bool `json:"read_only"`   // does not modify anything
	Destructive bool `json:"destructive"` // may change or delete state
	Idempotent  bool `json:"idempotent"`  // repeating a call has no further effect
	OpenWorld   bool `json:"open_world"`  // reaches outside Coxswain (remote hosts)
}

// Action is one capability.
type Action struct {
	// ID is dotted, e.g. "ssh.exec". It becomes the CLI path (cox ssh exec)
	// and the MCP tool name (ssh_exec).
	ID          string
	Summary     string // one line, for humans
	Description string // for the LLM: when to use it, what not to do
	// Input is a JSON Schema object describing the arguments.
	Input map[string]any
	// Positional lists properties that the CLI accepts as positional
	// arguments, in order.
	Positional []string
	// RestArgs makes the last Positional (a string) collect all remaining
	// arguments joined by spaces, e.g. `cox ssh exec web df -h`. Flags must
	// then come before the positionals.
	RestArgs    bool
	Annotations Annotations
	// Label describes one invocation for the audit log (risk, targets).
	Label func(ctx context.Context, args json.RawMessage) (Assessment, error)
	Run   func(ctx context.Context, args json.RawMessage) (any, error)
	// Redact, if set, returns the arguments as they are recorded in the
	// audit log and the activity stream (e.g. without file contents).
	Redact func(args json.RawMessage) json.RawMessage
}

var idPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(\.[a-z][a-z0-9_]*)+$`)

// Registry holds all registered Actions.
type Registry struct {
	actions map[string]*Action
}

func NewRegistry() *Registry { return &Registry{actions: map[string]*Action{}} }

// Register adds an Action. It panics on programmer errors (bad ID,
// duplicate, missing schema or functions) so they surface at startup.
func (r *Registry) Register(a *Action) {
	if !idPattern.MatchString(a.ID) {
		panic(fmt.Sprintf("action: invalid id %q", a.ID))
	}
	if _, dup := r.actions[a.ID]; dup {
		panic(fmt.Sprintf("action: duplicate id %q", a.ID))
	}
	if a.Input == nil || a.Input["type"] != "object" {
		panic(fmt.Sprintf("action %q: Input must be a JSON Schema with type object", a.ID))
	}
	if a.Label == nil || a.Run == nil {
		panic(fmt.Sprintf("action %q: Label and Run are required", a.ID))
	}
	r.actions[a.ID] = a
}

func (r *Registry) Get(id string) (*Action, bool) {
	a, ok := r.actions[id]
	return a, ok
}

// List returns all Actions sorted by ID.
func (r *Registry) List() []*Action {
	out := make([]*Action, 0, len(r.actions))
	for _, a := range r.actions {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Decode unmarshals args into v, treating empty args as {}.
func Decode(args json.RawMessage, v any) error {
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	if err := json.Unmarshal(args, v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

type actorKey struct{}

// WithActor records the invoking actor in ctx; the engine does this
// before calling Run so Actions can tailor output (e.g. shaping for agents).
func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, a)
}

// ActorFrom returns the actor recorded by WithActor.
func ActorFrom(ctx context.Context) (Actor, bool) {
	a, ok := ctx.Value(actorKey{}).(Actor)
	return a, ok
}

type assessmentKey struct{}

// WithAssessment records the invocation's assessment in ctx; the engine
// does this before calling Run, so Run acts on what was audited (e.g.
// the same hosts).
func WithAssessment(ctx context.Context, as Assessment) context.Context {
	return context.WithValue(ctx, assessmentKey{}, as)
}

// AssessmentFrom returns the assessment recorded by WithAssessment.
func AssessmentFrom(ctx context.Context) (Assessment, bool) {
	as, ok := ctx.Value(assessmentKey{}).(Assessment)
	return as, ok
}

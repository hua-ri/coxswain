// Package terminal registers the shared-terminal Actions: agents open,
// type into, read and close the same terminals the user sees in the
// desktop app.
package terminal

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/inventory"
	"github.com/hua-ri/coxswain/internal/policy"
	"github.com/hua-ri/coxswain/internal/shaper"
	"github.com/hua-ri/coxswain/internal/term"
)

const (
	defaultLines = 100
	maxLines     = 2000
)

// humanLimits effectively disables shaping for people.
var humanLimits = shaper.Limits{MaxBytes: 4 << 20, MaxLines: 1 << 30}

type Provider struct {
	Hosts     *inventory.Store
	Terms     *term.Manager
	Artifacts *shaper.Artifacts
}

func (p *Provider) Register(r *action.Registry) {
	r.Register(p.list())
	r.Register(p.open())
	r.Register(p.send())
	r.Register(p.read())
	r.Register(p.close())
}

var idProperty = map[string]any{"type": "string", "description": "terminal id (t_…), from terminal_open or terminal_list"}

// target is the host a terminal runs on, with its current tags.
func (p *Provider) target(id string) (term.Session, action.Target, error) {
	s, ok := p.Terms.Get(id)
	if !ok {
		return s, action.Target{}, fmt.Errorf("%w: %s", term.ErrNotFound, id)
	}
	if s.Kind == term.KindPod && p.Terms.Pods != nil {
		return s, p.Terms.Pods.Target(s.Context, s.Namespace), nil
	}
	t := action.Target{Kind: "host", ID: s.Host}
	if h, err := p.Hosts.Get(s.Host); err == nil {
		t.Tags = h.Tags
	}
	return s, t, nil
}

// where is where a terminal runs: its host, or context/namespace/pod[/container].
func where(s term.Session) string {
	if s.Kind != term.KindPod {
		return s.Host
	}
	w := s.Context + "/" + s.Namespace + "/" + s.Pod
	if s.Container != "" {
		w += "/" + s.Container
	}
	return w
}

type idInput struct {
	ID string `json:"id"`
}

// labelID labels an invocation on one terminal with the given risk.
func (p *Provider) labelID(risk action.Risk, verb string) func(context.Context, json.RawMessage) (action.Assessment, error) {
	return func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
		var in idInput
		if err := action.Decode(raw, &in); err != nil {
			return action.Assessment{}, err
		}
		s, t, err := p.target(in.ID)
		if err != nil {
			return action.Assessment{}, err
		}
		return action.Assessment{Risk: risk, Targets: []action.Target{t},
			Preview: fmt.Sprintf("%s terminal %s on %s (opened by %s)", verb, s.ID, where(s), s.Owner)}, nil
	}
}

func (p *Provider) list() *action.Action {
	return &action.Action{
		ID:      "terminal.list",
		Summary: "List shared terminals",
		Description: "List the shared terminals in Coxswain: id, kind (ssh or pod), host or context/namespace/pod/container, who opened it (the user or an agent), size, " +
			"and whether it has ended. Read one with terminal_read.",
		Input:       map[string]any{"type": "object", "properties": map[string]any{}},
		Annotations: action.Annotations{ReadOnly: true, Idempotent: true},
		Label: func(context.Context, json.RawMessage) (action.Assessment, error) {
			return action.Assessment{Risk: action.Read}, nil
		},
		Run: func(context.Context, json.RawMessage) (any, error) { return p.Terms.List(), nil },
	}
}

type openInput struct {
	Host string        `json:"host"`
	Pod  *term.PodSpec `json:"pod"`
	Cols int           `json:"cols"`
	Rows int           `json:"rows"`
}

func (in openInput) check() error {
	switch {
	case (in.Host == "") == (in.Pod == nil):
		return fmt.Errorf("give either host or pod")
	case in.Pod != nil && (in.Pod.Context == "" || in.Pod.Pod == ""):
		return fmt.Errorf("pod needs context and pod")
	}
	return nil
}

// OpenOutput is the result of terminal.open.
type OpenOutput struct {
	term.Session
	HostNote    string            `json:"host_note,omitempty"`
	HostTags    map[string]string `json:"host_tags,omitempty"`
	ContextTags map[string]string `json:"context_tags,omitempty"` // pod terminals
}

func (p *Provider) open() *action.Action {
	return &action.Action{
		ID:      "terminal.open",
		Summary: "Open a shared terminal on a saved SSH host or in a pod",
		Description: "Open an interactive shell on a saved SSH host (host), or in a Kubernetes pod's container (pod: " +
			"{context, namespace, pod, container, shell}; shell defaults to sh, and sh is used if the given one is missing), " +
			"as a shared terminal. The user sees it at once in the " +
			"Coxswain desktop app and can type in it too. Use terminals for long-running work or work the user should " +
			"watch (builds, deploys, following logs); use ssh_exec for one-off commands. " +
			"Then type with terminal_send and read with terminal_read; close it with terminal_close when done. " +
			"Hosts tagged env=prod are production: confirm with the user before changing them. Respect the host note.",
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"host": map[string]any{"type": "string", "description": "saved host name (see ssh_hosts_list)"},
				"pod": map[string]any{"type": "object", "description": "instead of host: a shell in a pod", "properties": map[string]any{
					"context":   map[string]any{"type": "string", "description": "kubeconfig context (see k8s_contexts)"},
					"namespace": map[string]any{"type": "string", "description": "default: the context's namespace"},
					"pod":       map[string]any{"type": "string"},
					"container": map[string]any{"type": "string", "description": "required if the pod has several containers"},
					"shell":     map[string]any{"type": "string", "description": "e.g. bash; default sh"},
				}, "required": []string{"context", "pod"}},
				"cols": map[string]any{"type": "integer", "minimum": 1, "maximum": 1000, "description": "default 80"},
				"rows": map[string]any{"type": "integer", "minimum": 1, "maximum": 1000, "description": "default 24"},
			},
		},
		Positional:  []string{"host"},
		Annotations: action.Annotations{Destructive: true, OpenWorld: true},
		Label: func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
			var in openInput
			if err := action.Decode(raw, &in); err != nil {
				return action.Assessment{}, err
			}
			if err := in.check(); err != nil {
				return action.Assessment{}, err
			}
			if in.Pod != nil {
				if p.Terms.Pods == nil {
					return action.Assessment{}, fmt.Errorf("pod terminals are not available")
				}
				t := p.Terms.Pods.Target(in.Pod.Context, in.Pod.Namespace)
				return action.Assessment{Risk: action.Write, Targets: []action.Target{t},
					Preview: fmt.Sprintf("open a terminal in pod %s (%s)", in.Pod.Pod, t.ID)}, nil
			}
			h, err := p.Hosts.Get(in.Host)
			if err != nil {
				return action.Assessment{}, err
			}
			return action.Assessment{Risk: action.Write, Targets: []action.Target{{Kind: "host", ID: h.Name, Tags: h.Tags}},
				Preview: "open a terminal on " + h.Name}, nil
		},
		Run: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in openInput
			if err := action.Decode(raw, &in); err != nil {
				return nil, err
			}
			if err := in.check(); err != nil {
				return nil, err
			}
			who, _ := action.ActorFrom(ctx)
			who.Session = "" // the owner is the client, not one of its sessions
			if in.Pod != nil {
				s, err := p.Terms.OpenPod(ctx, *in.Pod, who, in.Cols, in.Rows)
				if err != nil {
					return nil, err
				}
				return OpenOutput{Session: s, ContextTags: p.Terms.Pods.Target(s.Context, s.Namespace).Tags}, nil
			}
			h, err := p.Hosts.Get(in.Host)
			if err != nil {
				return nil, err
			}
			s, err := p.Terms.Open(ctx, h, who, in.Cols, in.Rows)
			if err != nil {
				return nil, err
			}
			return OpenOutput{Session: s, HostNote: h.Note, HostTags: h.Tags}, nil
		},
	}
}

type sendInput struct {
	ID    string `json:"id"`
	Text  string `json:"text"`
	Enter *bool  `json:"enter"`
}

func (in sendInput) bytes() []byte {
	if in.Enter == nil || *in.Enter {
		return []byte(in.Text + "\r")
	}
	return []byte(in.Text)
}

func (p *Provider) send() *action.Action {
	return &action.Action{
		ID:      "terminal.send",
		Summary: "Type into a shared terminal",
		Description: "Type text into a shared terminal, followed by Enter unless enter is false. This is a terminal the " +
			"user can see and may be using: do not type into a terminal the user is working in without asking. " +
			"Use it for long-running commands or ones the user should watch; for one-off commands use ssh_exec, which " +
			"returns the output and exit code directly. Read the result with terminal_read. " +
			"Use sudo -n, never type passwords. " +
			"Hosts tagged env=prod are production: confirm with the user before changing them.",
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id":    idProperty,
				"text":  map[string]any{"type": "string", "description": "text to type, e.g. a command line"},
				"enter": map[string]any{"type": "boolean", "description": "press Enter after the text; default true"},
			},
			"required": []string{"id", "text"},
		},
		Positional:  []string{"id", "text"},
		RestArgs:    true,
		Annotations: action.Annotations{Destructive: true, OpenWorld: true},
		Label: func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
			var in sendInput
			if err := action.Decode(raw, &in); err != nil {
				return action.Assessment{}, err
			}
			s, t, err := p.target(in.ID)
			if err != nil {
				return action.Assessment{}, err
			}
			return action.Assessment{Risk: policy.ClassifyCommand(in.Text), Targets: []action.Target{t},
				Preview: fmt.Sprintf("%s [%s]$ %s", where(s), s.ID, in.Text)}, nil
		},
		Run: func(_ context.Context, raw json.RawMessage) (any, error) {
			var in sendInput
			if err := action.Decode(raw, &in); err != nil {
				return nil, err
			}
			b := in.bytes()
			if err := p.Terms.Write(in.ID, b); err != nil {
				return nil, err
			}
			return map[string]any{"id": in.ID, "sent_bytes": len(b)}, nil
		},
	}
}

type readInput struct {
	ID    string `json:"id"`
	Lines int    `json:"lines"`
}

// ReadOutput is the result of terminal.read.
type ReadOutput struct {
	ID       string        `json:"id"`
	Host     string        `json:"host"`
	Closed   bool          `json:"closed"`
	ExitCode *int          `json:"exit_code,omitempty"`
	Output   shaper.Shaped `json:"output"`
}

// PrintCLI writes the terminal text.
func (o ReadOutput) PrintCLI(stdout, _ io.Writer) int {
	io.WriteString(stdout, o.Output.Text)
	if !strings.HasSuffix(o.Output.Text, "\n") && o.Output.Text != "" {
		io.WriteString(stdout, "\n")
	}
	return 0
}

func (p *Provider) read() *action.Action {
	return &action.Action{
		ID:      "terminal.read",
		Summary: "Read the last lines of a shared terminal",
		Description: "Read the last lines of a shared terminal as plain text (colors and control sequences removed). " +
			"Works for terminals the user opened too. It is not a screen capture: output of full-screen programs " +
			"(top, vim, less) comes out garbled; prefer batch modes such as top -b -n 1. " +
			"Large outputs are truncated; read more with artifact_read.",
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id":    idProperty,
				"lines": map[string]any{"type": "integer", "minimum": 1, "maximum": maxLines, "description": "default 100"},
			},
			"required": []string{"id"},
		},
		Positional:  []string{"id"},
		Annotations: action.Annotations{ReadOnly: true},
		Label:       p.labelID(action.Read, "read"),
		Run: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in readInput
			if err := action.Decode(raw, &in); err != nil {
				return nil, err
			}
			switch {
			case in.Lines == 0:
				in.Lines = defaultLines
			case in.Lines < 0 || in.Lines > maxLines:
				return nil, fmt.Errorf("lines must be 1..%d", maxLines)
			}
			s, ok := p.Terms.Get(in.ID)
			if !ok {
				return nil, fmt.Errorf("%w: %s", term.ErrNotFound, in.ID)
			}
			text, err := p.Terms.Text(in.ID, in.Lines)
			if err != nil {
				return nil, err
			}
			lim := shaper.Default
			if a, ok := action.ActorFrom(ctx); ok && a.Kind == action.Human {
				lim = humanLimits
			}
			out := ReadOutput{ID: s.ID, Host: s.Host, Closed: s.Closed, ExitCode: s.ExitCode}
			out.Output, err = p.Artifacts.Shape(text, lim)
			return out, err
		},
	}
}

func (p *Provider) close() *action.Action {
	return &action.Action{
		ID:      "terminal.close",
		Summary: "Close a shared terminal",
		Description: "Close a shared terminal (ends its shell). Agents may close only terminals they opened; " +
			"ask the user to close theirs.",
		Input: map[string]any{
			"type":       "object",
			"properties": map[string]any{"id": idProperty},
			"required":   []string{"id"},
		},
		Positional:  []string{"id"},
		Annotations: action.Annotations{Destructive: true},
		Label:       p.labelID(action.Write, "close"),
		Run: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in idInput
			if err := action.Decode(raw, &in); err != nil {
				return nil, err
			}
			s, ok := p.Terms.Get(in.ID)
			if !ok {
				return nil, fmt.Errorf("%w: %s", term.ErrNotFound, in.ID)
			}
			who, _ := action.ActorFrom(ctx)
			if who.Kind != action.Human && (who.Kind != s.Owner.Kind || who.Name != s.Owner.Name) {
				return nil, fmt.Errorf("terminal %s was opened by %s; agents may close only terminals they opened", s.ID, s.Owner)
			}
			if err := p.Terms.Close(in.ID, who); err != nil {
				return nil, err
			}
			return map[string]any{"id": in.ID, "closed": true}, nil
		},
	}
}

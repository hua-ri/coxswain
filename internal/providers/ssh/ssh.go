// Package ssh registers the SSH Actions: host inventory and remote exec.
package ssh

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/eventlog"
	"github.com/hua-ri/coxswain/internal/inventory"
	"github.com/hua-ri/coxswain/internal/policy"
	"github.com/hua-ri/coxswain/internal/shaper"
	"github.com/hua-ri/coxswain/internal/sshx"
)

// captureLimit bounds how much remote output is held in memory.
const captureLimit = 4 << 20

// humanLimits effectively disables shaping for people at a terminal.
var humanLimits = shaper.Limits{MaxBytes: captureLimit, MaxLines: 1 << 30}

type Provider struct {
	Hosts     *inventory.Store
	Dialer    *sshx.Dialer
	Pool      *sshx.Pool // reuses connections if set (in `cox server`)
	Artifacts *shaper.Artifacts
	UserHome  string        // for ~ expansion and the default ~/.ssh/config
	Events    *eventlog.Log // batch.* events; may be nil
}

func (p *Provider) Register(r *action.Registry) {
	r.Register(p.hostsList())
	r.Register(p.hostsImport())
	r.Register(p.hostsTag())
	r.Register(p.hostsAdd())
	r.Register(p.hostsUpdate())
	r.Register(p.hostsRemove())
	r.Register(p.hostsScanKey())
	r.Register(p.hostsTrustKey())
	r.Register(p.hostsForgetKey())
	r.Register(p.hostsTest())
	r.Register(p.exec())
	r.Register(p.batch())
}

func hostTarget(h inventory.Host) action.Target {
	return action.Target{Kind: "host", ID: h.Name, Tags: h.Tags}
}

func readOnly(context.Context, json.RawMessage) (action.Assessment, error) {
	return action.Assessment{Risk: action.Read}, nil
}

func (p *Provider) hostsList() *action.Action {
	return &action.Action{
		ID:      "ssh.hosts.list",
		Summary: "List saved SSH hosts",
		Description: "List the SSH hosts the user has saved, with tags (env=prod marks production) and notes. " +
			"A host's note may restrict what you may do on it; always respect it. " +
			"Use the host name with ssh_exec.",
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"tag": map[string]any{"type": "string", "description": "only hosts with this tag, as key=value"},
			},
		},
		Annotations: action.Annotations{ReadOnly: true, Idempotent: true},
		Label:       readOnly,
		Run: func(_ context.Context, raw json.RawMessage) (any, error) {
			var in struct{ Tag string }
			if err := action.Decode(raw, &in); err != nil {
				return nil, err
			}
			hosts, err := p.Hosts.List()
			if err != nil {
				return nil, err
			}
			if in.Tag == "" {
				return hosts, nil
			}
			k, v, _ := strings.Cut(in.Tag, "=")
			out := []inventory.Host{}
			for _, h := range hosts {
				if h.Tags[k] == v {
					out = append(out, h)
				}
			}
			return out, nil
		},
	}
}

// ImportOutput is the result of ssh.hosts.import: what it adds and
// refreshes, what was already up to date, and the hosts edited in
// Coxswain that it leaves alone (unless listed in overwrite).
type ImportOutput struct {
	Path   string `json:"path"`
	DryRun bool   `json:"dry_run,omitempty"`
	inventory.ImportPlan
}

type importInput struct {
	Path      string   `json:"path"`
	DryRun    bool     `json:"dry_run"`
	Overwrite []string `json:"overwrite"`
}

func (p *Provider) hostsImport() *action.Action {
	defaultPath := filepath.Join(p.UserHome, ".ssh", "config")
	read := func(raw json.RawMessage) (importInput, string, []inventory.Host, error) {
		var in importInput
		if err := action.Decode(raw, &in); err != nil {
			return in, "", nil, err
		}
		f := in.Path
		if f == "" {
			f = defaultPath
		}
		r, err := os.Open(f)
		if err != nil {
			return in, "", nil, err
		}
		defer r.Close()
		hosts, err := inventory.ImportSSHConfig(r, p.UserHome)
		return in, f, hosts, err
	}
	return &action.Action{
		ID:      "ssh.hosts.import",
		Summary: "Sync hosts from an OpenSSH config file (adds hosts, refreshes addresses)",
		Description: "Add the Host entries of an OpenSSH client config (default ~/.ssh/config) and refresh the addresses, " +
			"ports, users, keys and jump hosts of hosts already imported from it. The config file is only read. Coxswain's " +
			"tags and notes are kept. Hosts whose connection fields were edited in Coxswain (or added by hand) are left " +
			"as they are unless listed in overwrite. dry_run shows what would change without changing anything.",
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":      map[string]any{"type": "string", "description": "config file, default ~/.ssh/config"},
				"dry_run":   map[string]any{"type": "boolean", "description": "only show what would change"},
				"overwrite": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "hosts edited in Coxswain to overwrite with the config's fields"},
			},
		},
		Positional:  []string{"path"},
		Annotations: action.Annotations{Idempotent: true},
		Label: func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
			in, f, imported, err := read(raw)
			if err != nil {
				return action.Assessment{}, err
			}
			existing, err := p.Hosts.List()
			if err != nil {
				return action.Assessment{}, err
			}
			if in.DryRun {
				return action.Assessment{Risk: action.Read, Preview: "preview syncing SSH hosts from " + f}, nil
			}
			as := assessImport(existing, imported)
			as.Preview = "sync SSH hosts from " + f + ": " + as.Preview
			if len(in.Overwrite) > 0 {
				as.Preview += fmt.Sprintf(" (overwrite edited: %s)", strings.Join(in.Overwrite, ", "))
			}
			if filepath.Clean(f) != defaultPath {
				as.Risk = action.Destructive // a file the agent may have written
			}
			return as, nil
		},
		Run: func(_ context.Context, raw json.RawMessage) (any, error) {
			in, f, hosts, err := read(raw)
			if err != nil {
				return nil, err
			}
			var plan inventory.ImportPlan
			if in.DryRun {
				plan, err = p.Hosts.PlanImport(hosts)
			} else {
				plan, err = p.Hosts.Import(hosts, in.Overwrite)
			}
			return ImportOutput{Path: f, DryRun: in.DryRun, ImportPlan: plan}, err
		},
	}
}

// assessImport labels an import: it targets every existing host that an
// imported entry replaces (same name) or aliases (same address).
func assessImport(existing, imported []inventory.Host) action.Assessment {
	byName := map[string]inventory.Host{}
	addrs := map[string]bool{}
	for _, h := range imported {
		addrs[h.Addr()] = true
	}
	var targets []action.Target
	var changes []string
	for _, h := range existing {
		byName[h.Name] = h
	}
	importedNames := map[string]bool{}
	for _, h := range imported {
		importedNames[h.Name] = true
		verb := "add "
		if _, ok := byName[h.Name]; ok {
			verb = "change "
		}
		changes = append(changes, verb+h.Name+" -> "+address(h))
	}
	for _, h := range existing {
		if importedNames[h.Name] || addrs[h.Addr()] {
			targets = append(targets, hostTarget(h))
		}
	}
	if len(changes) > 20 {
		changes = append(changes[:20], "…")
	}
	return action.Assessment{Risk: action.Write, Targets: targets, Preview: strings.Join(changes, ", ")}
}

type tagInput struct {
	Host  string   `json:"host"`
	Set   []string `json:"set"`
	Unset []string `json:"unset"`
	Note  *string  `json:"note"`
}

func (p *Provider) hostsTag() *action.Action {
	return &action.Action{
		ID:      "ssh.hosts.tag",
		Summary: "Set or remove tags and the note of a host",
		Description: "Change a host's tags (key=value) or note. Tags (env=prod marks production) and the note " +
			"tell agents how to treat the host; change them only when the user asks.",
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"host":  map[string]any{"type": "string"},
				"set":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "tags to set, key=value"},
				"unset": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "tag keys to remove"},
				"note":  map[string]any{"type": "string", "description": "replace the note"},
			},
			"required": []string{"host"},
		},
		Positional:  []string{"host"},
		Annotations: action.Annotations{Destructive: true, Idempotent: true},
		Label: func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
			var in tagInput
			if err := action.Decode(raw, &in); err != nil {
				return action.Assessment{}, err
			}
			h, err := p.Hosts.Get(in.Host)
			if err != nil {
				return action.Assessment{}, err
			}
			risk := action.Write
			keys := append([]string{}, in.Unset...)
			for _, kv := range in.Set {
				k, _, ok := strings.Cut(kv, "=")
				if !ok {
					return action.Assessment{}, fmt.Errorf("tag %q must be key=value", kv)
				}
				keys = append(keys, k)
			}
			for _, k := range keys {
				if agentFacing(k) {
					risk = action.Destructive
				}
			}
			if in.Note != nil && *in.Note != h.Note {
				risk = action.Destructive // the note instructs agents, like the env tag
			}
			preview := fmt.Sprintf("host %s: set %v, unset %v", in.Host, in.Set, in.Unset)
			if in.Note != nil {
				preview += fmt.Sprintf(", note %q", *in.Note)
			}
			return action.Assessment{Risk: risk, Targets: []action.Target{hostTarget(h)}, Preview: preview}, nil
		},
		Run: func(_ context.Context, raw json.RawMessage) (any, error) {
			var in tagInput
			if err := action.Decode(raw, &in); err != nil {
				return nil, err
			}
			var out inventory.Host
			err := p.Hosts.Update(func(m map[string]inventory.Host) error {
				h, ok := m[in.Host]
				if !ok {
					return fmt.Errorf("%w: %s", inventory.ErrNotFound, in.Host)
				}
				if h.Tags == nil {
					h.Tags = map[string]string{}
				}
				for _, kv := range in.Set {
					k, v, _ := strings.Cut(kv, "=")
					h.Tags[k] = v
				}
				for _, k := range in.Unset {
					delete(h.Tags, k)
				}
				if in.Note != nil {
					h.Note = *in.Note
				}
				m[in.Host], out = h, h
				return nil
			})
			return out, err
		},
	}
}

type execInput struct {
	Host       string `json:"host"`
	Command    string `json:"command"`
	TimeoutSec int    `json:"timeout_sec"`
}

// ExecOutput is the result of ssh.exec.
type ExecOutput struct {
	Host       string            `json:"host"`
	HostNote   string            `json:"host_note,omitempty"`
	HostTags   map[string]string `json:"host_tags,omitempty"`
	ExitCode   int               `json:"exit_code"`
	Stdout     shaper.Shaped     `json:"stdout"`
	Stderr     shaper.Shaped     `json:"stderr"`
	DurationMs int64             `json:"duration_ms"`
	Capped     bool              `json:"capture_truncated,omitempty"`
}

// PrintCLI writes the remote output as-is and returns the remote exit code.
func (o ExecOutput) PrintCLI(stdout, stderr io.Writer) int {
	io.WriteString(stdout, o.Stdout.Text)
	io.WriteString(stderr, o.Stderr.Text)
	return o.ExitCode
}

func (p *Provider) exec() *action.Action {
	return &action.Action{
		ID:      "ssh.exec",
		Summary: "Run a command on a saved SSH host",
		Description: "Run one non-interactive shell command on a saved SSH host and return the exit code, stdout and stderr. " +
			"Good for inspection: df -h, free -m, journalctl -u X -n 200, systemctl status X, ss -lntp. " +
			"Do not start interactive programs or follow logs (tail -f); " +
			"raise timeout_sec for slow commands; a timed-out command may keep running on the host. " +
			"Use sudo -n, never a password prompt. " +
			"Hosts tagged env=prod are production: confirm with the user before changing them. " +
			"Respect the host note.",
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"host":        map[string]any{"type": "string", "description": "saved host name (see ssh_hosts_list)"},
				"command":     map[string]any{"type": "string", "description": "shell command line"},
				"timeout_sec": map[string]any{"type": "integer", "minimum": 1, "maximum": 600, "description": "default 60"},
			},
			"required": []string{"host", "command"},
		},
		Positional:  []string{"host", "command"},
		RestArgs:    true,
		Annotations: action.Annotations{Destructive: true, OpenWorld: true},
		Label: func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
			var in execInput
			if err := action.Decode(raw, &in); err != nil {
				return action.Assessment{}, err
			}
			if strings.TrimSpace(in.Command) == "" {
				return action.Assessment{}, fmt.Errorf("command is required")
			}
			h, err := p.Hosts.Get(in.Host)
			if err != nil {
				return action.Assessment{}, err
			}
			return action.Assessment{
				Risk:    policy.ClassifyCommand(in.Command),
				Targets: []action.Target{hostTarget(h)},
				Preview: fmt.Sprintf("%s (%s)$ %s", h.Name, describe(h), in.Command),
			}, nil
		},
		Run: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in execInput
			if err := action.Decode(raw, &in); err != nil {
				return nil, err
			}
			h, err := p.Hosts.Get(in.Host)
			if err != nil {
				return nil, err
			}
			timeout := time.Duration(in.TimeoutSec) * time.Second
			if in.TimeoutSec <= 0 {
				timeout = 60 * time.Second
			}
			ctx, cancel := context.WithTimeout(ctx, min(timeout, 600*time.Second))
			defer cancel()

			start := time.Now()
			conn, release, err := p.connect(ctx, h)
			if err != nil {
				return nil, err
			}
			res, err := sshx.Exec(ctx, conn.Client, in.Command, captureLimit)
			release(err != nil)
			if err != nil {
				return nil, err
			}
			lim := shaper.Default
			if a, ok := action.ActorFrom(ctx); ok && a.Kind == action.Human {
				lim = humanLimits
			}
			out := ExecOutput{Host: h.Name, HostNote: h.Note, HostTags: h.Tags, ExitCode: res.ExitCode, Capped: res.Truncated,
				DurationMs: time.Since(start).Milliseconds()}
			if out.Stdout, err = p.Artifacts.Shape(res.Stdout, lim); err != nil {
				return nil, err
			}
			if out.Stderr, err = p.Artifacts.Shape(res.Stderr, lim); err != nil {
				return nil, err
			}
			return out, nil
		},
	}
}

// connect returns a connection to h from the pool, or a fresh one that
// release closes.
func (p *Provider) connect(ctx context.Context, h inventory.Host) (*sshx.Conn, func(broken bool), error) {
	return sshx.Connect(ctx, p.Pool, p.Dialer, h)
}

// address is user@host:port as an import would connect to it.
func address(h inventory.Host) string {
	port := h.Port
	if port == 0 {
		port = 22
	}
	s := net.JoinHostPort(h.HostName, strconv.Itoa(port))
	if h.User != "" {
		s = h.User + "@" + s
	}
	return s
}

func describe(h inventory.Host) string {
	s := h.HostName
	if h.User != "" {
		s = h.User + "@" + s
	}
	if h.Port != 0 && h.Port != 22 {
		s = fmt.Sprintf("%s:%d", s, h.Port)
	}
	if len(h.Tags) > 0 {
		keys := make([]string, 0, len(h.Tags))
		for k := range h.Tags {
			keys = append(keys, k+"="+h.Tags[k])
		}
		sort.Strings(keys)
		s += " " + strings.Join(keys, ",")
	}
	return s
}

package ssh

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/eventlog"
	"github.com/hua-ri/coxswain/internal/inventory"
	"github.com/hua-ri/coxswain/internal/policy"
	"github.com/hua-ri/coxswain/internal/shaper"
	"github.com/hua-ri/coxswain/internal/sshx"
)

const (
	// confirmAbove is how many hosts a batch may target without
	// confirm_count.
	confirmAbove = 50
	maxBatchConc = 50
)

// batchLimits is how much of each host's output an agent sees.
var batchLimits = shaper.Limits{MaxBytes: 2 << 10, MaxLines: 20}

// Batch host statuses.
const (
	HostOK      = "ok"
	HostFailed  = "failed"
	HostSkipped = "skipped"
)

type batchInput struct {
	Hosts         []string `json:"hosts"`
	Tag           string   `json:"tag"`
	Command       string   `json:"command"`
	Concurrency   int      `json:"concurrency"`
	Canary        *bool    `json:"canary"`
	StopOnFailure *bool    `json:"stop_on_failure"`
	TimeoutSec    int      `json:"timeout_sec"`
	ConfirmCount  int      `json:"confirm_count"`
}

// HostResult is one host's part of a batch.
type HostResult struct {
	Host       string `json:"host"`
	Status     string `json:"status"` // ok, failed or skipped
	ExitCode   *int   `json:"exit_code,omitempty"`
	Stdout     string `json:"stdout,omitempty"`
	Stderr     string `json:"stderr,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"` // output was shortened; all of it is in the artifact
	DurationMs int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"` // could not run (connection, timeout)
}

// BatchSummary counts the hosts of a batch by status.
type BatchSummary struct {
	Total   int `json:"total"`
	OK      int `json:"ok"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
}

// BatchOutput is the result of ssh.batch.
type BatchOutput struct {
	ID         string            `json:"id"`
	Command    string            `json:"command"`
	Summary    BatchSummary      `json:"summary"`
	Stopped    string            `json:"stopped,omitempty"` // why hosts were skipped
	Results    []HostResult      `json:"results"`
	ArtifactID string            `json:"artifact_id,omitempty"` // every host's full output
	HostNotes  map[string]string `json:"host_notes,omitempty"`
}

// BatchStart is the detail of a batch.started event.
type BatchStart struct {
	ID            string   `json:"id"`
	Command       string   `json:"command"`
	Hosts         []string `json:"hosts"`
	Concurrency   int      `json:"concurrency"`
	Canary        bool     `json:"canary"`
	StopOnFailure bool     `json:"stop_on_failure"`
}

// BatchEnd is the detail of a batch.finished event.
type BatchEnd struct {
	ID         string       `json:"id"`
	Summary    BatchSummary `json:"summary"`
	Stopped    string       `json:"stopped,omitempty"`
	ArtifactID string       `json:"artifact_id,omitempty"`
}

// PrintCLI writes each host's output under a header and returns 1 if a
// host failed.
func (o BatchOutput) PrintCLI(stdout, _ io.Writer) int {
	code := 0
	for _, r := range o.Results {
		fmt.Fprintf(stdout, "=== %s: %s\n", r.Host, describeResult(r))
		io.WriteString(stdout, r.Stdout)
		if r.Stderr != "" {
			fmt.Fprintf(stdout, "--- stderr ---\n%s", r.Stderr)
		}
		if r.Status == HostFailed {
			code = 1
		}
	}
	fmt.Fprintf(stdout, "=== %d hosts: %d ok, %d failed, %d skipped\n", o.Summary.Total, o.Summary.OK, o.Summary.Failed, o.Summary.Skipped)
	if o.Stopped != "" {
		fmt.Fprintf(stdout, "stopped: %s\n", o.Stopped)
	}
	return code
}

func describeResult(r HostResult) string {
	switch {
	case r.Status == HostSkipped:
		return "skipped"
	case r.Error != "":
		return fmt.Sprintf("error after %dms: %s", r.DurationMs, r.Error)
	case r.ExitCode != nil:
		return fmt.Sprintf("exit %d, %dms", *r.ExitCode, r.DurationMs)
	}
	return r.Status
}

// resolve returns the hosts a batch targets: the named ones in order,
// then those with the tag by name, each once.
func (p *Provider) resolve(in batchInput) ([]inventory.Host, error) {
	var out []inventory.Host
	seen := map[string]bool{}
	for _, name := range in.Hosts {
		h, err := p.Hosts.Get(name)
		if err != nil {
			return nil, err
		}
		if !seen[h.Name] {
			seen[h.Name] = true
			out = append(out, h)
		}
	}
	if in.Tag != "" {
		k, v, ok := strings.Cut(in.Tag, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("tag must be key=value, not %q", in.Tag)
		}
		all, err := p.Hosts.List()
		if err != nil {
			return nil, err
		}
		for _, h := range all {
			if tv, has := h.Tags[k]; has && tv == v && !seen[h.Name] {
				seen[h.Name] = true
				out = append(out, h)
			}
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no hosts to run on: give hosts and/or a tag that matches saved hosts")
	}
	if len(out) > confirmAbove && in.ConfirmCount != len(out) {
		return nil, fmt.Errorf("this would run on %d hosts; check the list (ssh_hosts_list) and, if it is right, repeat with confirm_count=%d",
			len(out), len(out))
	}
	return out, nil
}

func (in *batchInput) check() error {
	if strings.TrimSpace(in.Command) == "" {
		return errors.New("command is required")
	}
	if in.Concurrency == 0 {
		in.Concurrency = 5
	}
	if in.Concurrency < 1 || in.Concurrency > maxBatchConc {
		return fmt.Errorf("concurrency must be 1..%d", maxBatchConc)
	}
	return nil
}

func (p *Provider) batch() *action.Action {
	return &action.Action{
		ID:      "ssh.batch",
		Summary: "Run a command on several saved SSH hosts",
		Description: "Run one non-interactive command on several saved hosts: the hosts named and/or every host with tag " +
			"(key=value). By default the first host runs alone first as a canary; if it fails, nothing else runs. Then up " +
			"to concurrency (default 5) hosts run at once, and after a failure no further hosts start (stop_on_failure, " +
			"default true). More than 50 hosts need confirm_count set to their number. Returns each host's exit code and " +
			"output (shortened to 20 lines per host; all of it is in artifact_id, read with artifact_read) and a summary. " +
			"Inspect with ssh_exec on one host first; hosts tagged env=prod are production: confirm with the user before " +
			"changing them. Respect host notes.",
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"hosts":           map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "saved host names"},
				"tag":             map[string]any{"type": "string", "description": "also every host with this tag, key=value"},
				"command":         map[string]any{"type": "string", "description": "shell command line"},
				"concurrency":     map[string]any{"type": "integer", "minimum": 1, "maximum": maxBatchConc, "description": "hosts at once, default 5"},
				"canary":          map[string]any{"type": "boolean", "description": "run the first host alone first, default true"},
				"stop_on_failure": map[string]any{"type": "boolean", "description": "start no more hosts after a failure, default true"},
				"timeout_sec":     map[string]any{"type": "integer", "minimum": 1, "maximum": 600, "description": "per host, default 60"},
				"confirm_count":   map[string]any{"type": "integer", "description": "required above 50 hosts: the number of hosts"},
			},
			"required": []string{"command"},
		},
		Positional:  []string{"command"},
		RestArgs:    true,
		Annotations: action.Annotations{Destructive: true, OpenWorld: true},
		Label: func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
			var in batchInput
			if err := action.Decode(raw, &in); err != nil {
				return action.Assessment{}, err
			}
			if err := in.check(); err != nil {
				return action.Assessment{}, err
			}
			hosts, err := p.resolve(in)
			if err != nil {
				return action.Assessment{}, err
			}
			targets := make([]action.Target, len(hosts))
			names := make([]string, len(hosts))
			for i, h := range hosts {
				targets[i], names[i] = hostTarget(h), h.Name
			}
			if len(names) > 10 {
				names = append(names[:10], fmt.Sprintf("… %d more", len(hosts)-10))
			}
			return action.Assessment{Risk: policy.ClassifyCommand(in.Command), Targets: targets,
				Preview: fmt.Sprintf("on %d hosts (%s)$ %s", len(hosts), strings.Join(names, ", "), in.Command)}, nil
		},
		Run: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in batchInput
			if err := action.Decode(raw, &in); err != nil {
				return nil, err
			}
			if err := in.check(); err != nil {
				return nil, err
			}
			hosts, err := p.resolve(in)
			if err != nil {
				return nil, err
			}
			return p.runBatch(ctx, in, hosts)
		},
	}
}

// batchRun is one ssh.batch in progress.
type batchRun struct {
	p       *Provider
	id      string
	command string
	timeout time.Duration
	actor   action.Actor
	hosts   []inventory.Host

	mu      sync.Mutex
	results []HostResult
	raw     []sshx.Result // full output, for the artifact
	failed  string        // the first host that failed
}

func (p *Provider) runBatch(ctx context.Context, in batchInput, hosts []inventory.Host) (any, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	who, _ := action.ActorFrom(ctx)
	r := &batchRun{p: p, id: "b_" + hex.EncodeToString(b[:]), command: in.Command, actor: who, hosts: hosts,
		results: make([]HostResult, len(hosts)), raw: make([]sshx.Result, len(hosts)),
		timeout: time.Duration(min(max(in.TimeoutSec, 0), 600)) * time.Second}
	if r.timeout == 0 {
		r.timeout = 60 * time.Second
	}
	canary := in.Canary == nil || *in.Canary
	stopOnFailure := in.StopOnFailure == nil || *in.StopOnFailure
	targets := make([]action.Target, len(hosts))
	names := make([]string, len(hosts))
	for i, h := range hosts {
		r.results[i] = HostResult{Host: h.Name, Status: HostSkipped}
		targets[i], names[i] = hostTarget(h), h.Name
	}
	r.log(eventlog.BatchStarted, targets, BatchStart{ID: r.id, Command: in.Command, Hosts: names,
		Concurrency: in.Concurrency, Canary: canary, StopOnFailure: stopOnFailure}, "")

	stopped := ""
	start := 0
	if canary && len(hosts) > 1 {
		r.run(ctx, 0)
		start = 1
		if r.failed != "" {
			stopped = fmt.Sprintf("the canary %s failed; no other host ran", hosts[0].Name)
		}
	}
	if stopped == "" {
		sem := make(chan struct{}, in.Concurrency)
		var wg sync.WaitGroup
		for i := start; i < len(hosts); i++ {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
			}
			r.mu.Lock()
			failed := r.failed
			r.mu.Unlock()
			if ctx.Err() != nil {
				break
			}
			if stopOnFailure && failed != "" {
				stopped = fmt.Sprintf("%s failed; no further hosts were started (stop_on_failure)", failed)
				break
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				r.run(ctx, i)
			}()
		}
		wg.Wait()
		if stopOnFailure && stopped == "" && ctx.Err() == nil && r.failed != "" && r.skipped() > 0 {
			stopped = fmt.Sprintf("%s failed; no further hosts were started (stop_on_failure)", r.failed)
		}
	}

	out := BatchOutput{ID: r.id, Command: in.Command, Stopped: stopped, Results: r.results}
	for _, res := range r.results {
		switch res.Status {
		case HostOK:
			out.Summary.OK++
		case HostFailed:
			out.Summary.Failed++
		default:
			out.Summary.Skipped++
		}
	}
	out.Summary.Total = len(hosts)
	for _, h := range hosts {
		if h.Note != "" {
			if out.HostNotes == nil {
				out.HostNotes = map[string]string{}
			}
			out.HostNotes[h.Name] = h.Note
		}
	}
	var err error
	out.ArtifactID, err = r.saveArtifact()
	if err == nil {
		r.shape(ctx)
	}
	errText := ""
	if ctx.Err() != nil {
		errText = context.Cause(ctx).Error()
	}
	r.log(eventlog.BatchFinished, targets, BatchEnd{ID: r.id, Summary: out.Summary, Stopped: stopped, ArtifactID: out.ArtifactID}, errText)
	if ctx.Err() != nil {
		return nil, context.Cause(ctx)
	}
	return out, err
}

func (r *batchRun) skipped() int {
	n := 0
	for _, res := range r.results {
		if res.Status == HostSkipped {
			n++
		}
	}
	return n
}

// run runs the command on host i and records the result.
func (r *batchRun) run(ctx context.Context, i int) {
	h := r.hosts[i]
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	res := HostResult{Host: h.Name}
	var raw sshx.Result
	conn, release, err := sshx.Connect(ctx, r.p.Pool, r.p.Dialer, h)
	if err == nil {
		raw, err = sshx.Exec(ctx, conn.Client, r.command, captureLimit)
		release(err != nil)
	}
	res.DurationMs = time.Since(start).Milliseconds()
	switch {
	case err != nil:
		res.Status, res.Error = HostFailed, err.Error()
	case raw.ExitCode != 0:
		res.Status = HostFailed
	default:
		res.Status = HostOK
	}
	if err == nil {
		code := raw.ExitCode
		res.ExitCode = &code
	}
	r.mu.Lock()
	r.results[i], r.raw[i] = res, raw
	if res.Status == HostFailed && r.failed == "" {
		r.failed = h.Name
	}
	r.mu.Unlock()
	r.log(eventlog.BatchHostDone, []action.Target{hostTarget(h)}, res, "")
}

// saveArtifact stores every host's full output, if there is any.
func (r *batchRun) saveArtifact() (string, error) {
	var sb strings.Builder
	has := false
	for i, res := range r.results {
		raw := r.raw[i]
		fmt.Fprintf(&sb, "=== %s: %s ===\n", res.Host, describeResult(res))
		sb.WriteString(raw.Stdout)
		if raw.Stdout != "" && !strings.HasSuffix(raw.Stdout, "\n") {
			sb.WriteString("\n")
		}
		if raw.Stderr != "" {
			fmt.Fprintf(&sb, "--- stderr ---\n%s", raw.Stderr)
			if !strings.HasSuffix(raw.Stderr, "\n") {
				sb.WriteString("\n")
			}
		}
		has = has || raw.Stdout != "" || raw.Stderr != ""
	}
	if !has {
		return "", nil
	}
	return r.p.Artifacts.Save(sb.String())
}

// shape fills in each host's output, shortened for agents.
func (r *batchRun) shape(ctx context.Context) {
	lim := batchLimits
	if a, ok := action.ActorFrom(ctx); ok && a.Kind == action.Human {
		lim = humanLimits
	}
	for i := range r.results {
		var cut1, cut2 bool
		r.results[i].Stdout, cut1 = shaper.Clip(r.raw[i].Stdout, lim)
		r.results[i].Stderr, cut2 = shaper.Clip(r.raw[i].Stderr, lim)
		r.results[i].Truncated = cut1 || cut2 || r.raw[i].Truncated
	}
}

func (r *batchRun) log(typ string, targets []action.Target, detail any, errText string) {
	if r.p.Events == nil {
		return
	}
	b, _ := json.Marshal(detail)
	actor := r.actor
	actor.Session = ""
	r.p.Events.Append(eventlog.Event{Type: typ, Actor: &actor, Batch: r.id, Targets: targets, Detail: b, Error: errText})
}

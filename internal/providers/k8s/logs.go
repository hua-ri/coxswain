package k8s

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	kube "github.com/hua-ri/coxswain/internal/k8s"
	"github.com/hua-ri/coxswain/internal/policy"
	"github.com/hua-ri/coxswain/internal/shaper"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// podInput addresses one pod (and maybe a container).
type podInput struct {
	Context   string `json:"context"`
	Namespace string `json:"namespace"`
	Pod       string `json:"pod"`
	Container string `json:"container"`
}

func (in podInput) scope() scope {
	return scope{Context: in.Context, Namespace: in.Namespace, Kind: "", Name: in.Pod}
}

// pod looks up the pod and the container to use: the given one, or the
// only one; with several and none given it is an error naming them.
func (p *Provider) pod(ctx context.Context, in podInput) (resolved, *corev1.Pod, string, error) {
	if in.Pod == "" {
		return resolved{}, nil, "", fmt.Errorf("pod is required")
	}
	r, err := p.resolve(in.scope())
	if err != nil {
		return r, nil, "", err
	}
	pod, err := r.client.Typed.CoreV1().Pods(r.ns).Get(ctx, in.Pod, metav1.GetOptions{})
	if err != nil {
		return r, nil, "", kube.Explain(err)
	}
	c, err := pickContainer(pod, in.Container)
	return r, pod, c, err
}

func pickContainer(pod *corev1.Pod, name string) (string, error) {
	var names []string
	for _, c := range pod.Spec.Containers {
		names = append(names, c.Name)
	}
	if name == "" {
		if len(names) == 1 {
			return names[0], nil
		}
		return "", fmt.Errorf("pod %s has several containers; pass container: %s", pod.Name, strings.Join(names, ", "))
	}
	all := append([]string{}, names...)
	for _, c := range pod.Spec.InitContainers {
		all = append(all, c.Name)
	}
	for _, c := range pod.Spec.EphemeralContainers {
		all = append(all, c.Name)
	}
	for _, n := range all {
		if n == name {
			return name, nil
		}
	}
	return "", fmt.Errorf("pod %s has no container %q (containers: %s)", pod.Name, name, strings.Join(all, ", "))
}

// podLabel assesses an invocation on a pod.
func (p *Provider) podLabel(raw json.RawMessage, risk func(json.RawMessage) (action.Risk, string, error)) (action.Assessment, error) {
	var in podInput
	if err := action.Decode(raw, &in); err != nil {
		return action.Assessment{}, err
	}
	if in.Pod == "" {
		return action.Assessment{}, fmt.Errorf("pod is required")
	}
	r, err := p.resolve(in.scope())
	if err != nil {
		return action.Assessment{}, err
	}
	rk, what, err := risk(raw)
	if err != nil {
		return action.Assessment{}, err
	}
	where := in.Context + "/" + r.ns + "/" + in.Pod
	if in.Container != "" {
		where += "/" + in.Container
	}
	if len(r.info.Tags) > 0 {
		where += " (" + tagString(r.info.Tags) + ")"
	}
	return action.Assessment{Risk: rk, Targets: []action.Target{p.Clusters.Target(in.Context, r.ns)}, Preview: where + what}, nil
}

// LogsOutput is the result of k8s.logs.
type LogsOutput struct {
	Context   string        `json:"context"`
	Namespace string        `json:"namespace"`
	Pod       string        `json:"pod"`
	Container string        `json:"container"`
	Previous  bool          `json:"previous,omitempty"`
	Text      shaper.Shaped `json:"text"`
}

// PrintCLI prints the log lines as they are.
func (o LogsOutput) PrintCLI(stdout, _ io.Writer) int {
	io.WriteString(stdout, o.Text.Text)
	return 0
}

type logsInput struct {
	podInput
	Previous bool   `json:"previous"`
	Tail     int    `json:"tail"`
	Since    string `json:"since"`
}

func (p *Provider) logs() *action.Action {
	return &action.Action{
		ID:      "k8s.logs",
		Summary: "Show the last lines of a pod's logs",
		Description: "Return the last lines (tail, default 200) of a container's log; previous=true for the " +
			"previous (crashed) instance. Pods with several containers need container. Long output is shortened; " +
			"read the rest with artifact_read. Does not follow logs.",
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"context":   contextProperty,
				"namespace": namespaceProperty,
				"pod":       map[string]any{"type": "string"},
				"container": map[string]any{"type": "string", "description": "required if the pod has several containers"},
				"previous":  map[string]any{"type": "boolean", "description": "logs of the previous, terminated instance"},
				"tail":      map[string]any{"type": "integer", "minimum": 1, "maximum": 10000, "description": "lines, default 200"},
				"since":     map[string]any{"type": "string", "description": "only newer lines, e.g. 10m, 2h"},
			},
			"required": []string{"context", "pod"},
		},
		Positional:  []string{"context", "pod"},
		Annotations: readOnlyRemote,
		Label: func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
			return p.podLabel(raw, func(json.RawMessage) (action.Risk, string, error) { return action.Read, " logs", nil })
		},
		Run: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in logsInput
			if err := action.Decode(raw, &in); err != nil {
				return nil, err
			}
			tail := int64(in.Tail)
			if tail <= 0 {
				tail = 200
			}
			opts := &corev1.PodLogOptions{Previous: in.Previous, TailLines: &tail, LimitBytes: new(int64(captureLimit))}
			if in.Since != "" {
				d, err := parseSince(in.Since, 0)
				if err != nil {
					return nil, err
				}
				opts.SinceSeconds = new(int64(max(d/time.Second, 1)))
			}
			r, pod, container, err := p.pod(ctx, in.podInput)
			if err != nil {
				return nil, err
			}
			opts.Container = container
			text, err := p.readLogs(ctx, r.client, pod, opts)
			if err != nil {
				return nil, err
			}
			shaped, err := p.shape(ctx, text)
			if err != nil {
				return nil, err
			}
			return LogsOutput{Context: in.Context, Namespace: pod.Namespace, Pod: pod.Name, Container: container,
				Previous: in.Previous, Text: shaped}, nil
		},
	}
}

// readLogs returns a container's log (at most captureLimit bytes).
func (p *Provider) readLogs(ctx context.Context, c *kube.Client, pod *corev1.Pod, opts *corev1.PodLogOptions) (string, error) {
	rc, err := c.Typed.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, opts).Stream(ctx)
	if err != nil {
		return "", kube.Explain(err)
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, captureLimit))
	return string(b), err
}

// TopOutput is the result of k8s.top.
type TopOutput struct {
	Context   string     `json:"context"`
	Kind      string     `json:"kind"` // pods or nodes
	Namespace string     `json:"namespace,omitempty"`
	Columns   []string   `json:"columns"`
	Rows      [][]string `json:"rows"`
}

func (o TopOutput) PrintCLI(stdout, _ io.Writer) int {
	writeTable(stdout, o.Columns, o.Rows)
	return 0
}

type topInput struct {
	scope
	LabelSelector string `json:"label_selector"`
	SortBy        string `json:"sort_by"`
}

var metricsGV = schema.GroupVersion{Group: "metrics.k8s.io", Version: "v1beta1"}

func (p *Provider) top() *action.Action {
	return &action.Action{
		ID:      "k8s.top",
		Summary: "Show CPU and memory use of pods or nodes",
		Description: "Current CPU and memory use of pods (kind pods) or nodes (kind nodes), from metrics-server; " +
			"nodes also as a share of what is allocatable. sort_by: cpu or memory (default: name).",
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"context":        contextProperty,
				"kind":           map[string]any{"type": "string", "enum": []string{"pods", "nodes"}},
				"namespace":      namespaceProperty,
				"all_namespaces": map[string]any{"type": "boolean"},
				"label_selector": map[string]any{"type": "string"},
				"sort_by":        map[string]any{"type": "string", "enum": []string{"cpu", "memory"}},
			},
			"required": []string{"context", "kind"},
		},
		Positional:  []string{"context", "kind"},
		Annotations: readOnlyRemote,
		Label: func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
			var in topInput
			if err := action.Decode(raw, &in); err != nil {
				return action.Assessment{}, err
			}
			nodes, err := topKind(in.Kind)
			if err != nil {
				return action.Assessment{}, err
			}
			in.Kind = ""
			r, err := p.resolve(in.scope)
			if err != nil {
				return action.Assessment{}, err
			}
			ns := r.ns
			switch {
			case nodes:
				ns = ""
			case in.AllNamespaces:
				ns = "*"
			}
			return action.Assessment{Risk: action.Read, Targets: []action.Target{p.Clusters.Target(in.Context, ns)},
				Preview: "top " + strings.ToLower(raw2kind(nodes)) + " in " + in.Context}, nil
		},
		Run: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in topInput
			if err := action.Decode(raw, &in); err != nil {
				return nil, err
			}
			nodes, err := topKind(in.Kind)
			if err != nil {
				return nil, err
			}
			in.Kind = ""
			r, err := p.resolve(in.scope)
			if err != nil {
				return nil, err
			}
			if !r.client.Resources.HasGroup(metricsGV.Group) {
				return nil, fmt.Errorf("context %s has no metrics API (metrics.k8s.io): metrics-server is not installed "+
					"or not ready; use k8s_describe or the node's SSH host instead", in.Context)
			}
			if nodes {
				return p.topNodes(ctx, r, in)
			}
			return p.topPods(ctx, r, in)
		},
	}
}

func topKind(kind string) (nodes bool, err error) {
	switch strings.ToLower(kind) {
	case "pod", "pods", "po":
		return false, nil
	case "node", "nodes", "no":
		return true, nil
	}
	return false, fmt.Errorf("kind must be pods or nodes")
}

func raw2kind(nodes bool) string {
	if nodes {
		return "nodes"
	}
	return "pods"
}

type usage struct {
	cpu, mem *resource.Quantity
}

func usageOf(m map[string]any) usage {
	u := usage{cpu: resource.NewMilliQuantity(0, resource.DecimalSI), mem: resource.NewQuantity(0, resource.BinarySI)}
	if s, ok := m["cpu"].(string); ok {
		if q, err := resource.ParseQuantity(s); err == nil {
			u.cpu.Add(q)
		}
	}
	if s, ok := m["memory"].(string); ok {
		if q, err := resource.ParseQuantity(s); err == nil {
			u.mem.Add(q)
		}
	}
	return u
}

func (u usage) add(o usage) { u.cpu.Add(*o.cpu); u.mem.Add(*o.mem) }

func cpuString(q *resource.Quantity) string { return fmt.Sprintf("%dm", q.MilliValue()) }
func memString(q *resource.Quantity) string { return fmt.Sprintf("%dMi", q.Value()/(1<<20)) }

type usageRow struct {
	ns, name string
	u        usage
	extra    func(usage) []string
}

func sortUsage(rows []usageRow, by string) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		switch by {
		case "cpu":
			if c := a.u.cpu.Cmp(*b.u.cpu); c != 0 {
				return c > 0
			}
		case "memory":
			if c := a.u.mem.Cmp(*b.u.mem); c != 0 {
				return c > 0
			}
		}
		if a.ns != b.ns {
			return a.ns < b.ns
		}
		return a.name < b.name
	})
}

func (p *Provider) topPods(ctx context.Context, r resolved, in topInput) (TopOutput, error) {
	ri := r.client.Dynamic.Resource(metricsGV.WithResource("pods"))
	var list *unstructured.UnstructuredList
	var err error
	opts := metav1.ListOptions{LabelSelector: in.LabelSelector}
	if in.AllNamespaces {
		list, err = ri.List(ctx, opts)
	} else {
		list, err = ri.Namespace(r.ns).List(ctx, opts)
	}
	if err != nil {
		return TopOutput{}, kube.Explain(err)
	}
	var rows []usageRow
	for _, item := range list.Items {
		total := usageOf(nil)
		cs, _, _ := unstructured.NestedSlice(item.Object, "containers")
		for _, c := range cs {
			m, _ := c.(map[string]any)
			u, _ := m["usage"].(map[string]any)
			total.add(usageOf(u))
		}
		rows = append(rows, usageRow{ns: item.GetNamespace(), name: item.GetName(), u: total})
	}
	sortUsage(rows, in.SortBy)
	out := TopOutput{Context: in.Context, Kind: "pods", Columns: []string{"name", "cpu", "memory"}}
	if in.AllNamespaces {
		out.Columns = append([]string{"namespace"}, out.Columns...)
	} else {
		out.Namespace = r.ns
	}
	for _, row := range rows {
		cells := []string{row.name, cpuString(row.u.cpu), memString(row.u.mem)}
		if in.AllNamespaces {
			cells = append([]string{row.ns}, cells...)
		}
		out.Rows = append(out.Rows, cells)
	}
	return out, nil
}

func (p *Provider) topNodes(ctx context.Context, r resolved, in topInput) (TopOutput, error) {
	list, err := r.client.Dynamic.Resource(metricsGV.WithResource("nodes")).List(ctx, metav1.ListOptions{LabelSelector: in.LabelSelector})
	if err != nil {
		return TopOutput{}, kube.Explain(err)
	}
	nodes, err := r.client.Typed.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: in.LabelSelector})
	if err != nil {
		return TopOutput{}, kube.Explain(err)
	}
	alloc := map[string]corev1.ResourceList{}
	for _, n := range nodes.Items {
		alloc[n.Name] = n.Status.Allocatable
	}
	var rows []usageRow
	for _, item := range list.Items {
		u, _, _ := unstructured.NestedMap(item.Object, "usage")
		rows = append(rows, usageRow{name: item.GetName(), u: usageOf(u)})
	}
	sortUsage(rows, in.SortBy)
	out := TopOutput{Context: in.Context, Kind: "nodes", Columns: []string{"name", "cpu", "cpu%", "memory", "memory%"}}
	for _, row := range rows {
		a := alloc[row.name]
		out.Rows = append(out.Rows, []string{row.name, cpuString(row.u.cpu), percent(row.u.cpu.MilliValue(), a.Cpu().MilliValue()),
			memString(row.u.mem), percent(row.u.mem.Value(), a.Memory().Value())})
	}
	return out, nil
}

func percent(used, total int64) string {
	if total <= 0 {
		return "-"
	}
	return fmt.Sprintf("%d%%", used*100/total)
}

// ExecOutput is the result of k8s.exec.
type ExecOutput struct {
	Context     string            `json:"context"`
	ContextNote string            `json:"context_note,omitempty"`
	ContextTags map[string]string `json:"context_tags,omitempty"`
	Namespace   string            `json:"namespace"`
	Pod         string            `json:"pod"`
	Container   string            `json:"container"`
	ExitCode    int               `json:"exit_code"`
	Stdout      shaper.Shaped     `json:"stdout"`
	Stderr      shaper.Shaped     `json:"stderr"`
	DurationMs  int64             `json:"duration_ms"`
	Capped      bool              `json:"capture_truncated,omitempty"`
}

// PrintCLI writes the output as-is and returns the command's exit code.
func (o ExecOutput) PrintCLI(stdout, stderr io.Writer) int {
	io.WriteString(stdout, o.Stdout.Text)
	io.WriteString(stderr, o.Stderr.Text)
	return o.ExitCode
}

type execInput struct {
	podInput
	Command    json.RawMessage `json:"command"`
	TimeoutSec int             `json:"timeout_sec"`
}

// argv is the command to run and the command line it is rated as: a
// string runs with sh -c, an array as is.
func (in execInput) argv() ([]string, string, error) {
	var s string
	if err := json.Unmarshal(in.Command, &s); err == nil {
		if strings.TrimSpace(s) == "" {
			return nil, "", fmt.Errorf("command is required")
		}
		return []string{"sh", "-c", s}, s, nil
	}
	var list []string
	if err := json.Unmarshal(in.Command, &list); err != nil || len(list) == 0 || list[0] == "" {
		return nil, "", fmt.Errorf("command is required: a string (run with sh -c) or an array of arguments")
	}
	quoted := make([]string, len(list))
	for i, a := range list {
		quoted[i] = shellQuote(a)
	}
	return list, strings.Join(quoted, " "), nil
}

func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r == '-' || r == '_' || r == '.' || r == '/' || r == '=' || r == ':' || r == ',' || r == '@' || r == '+' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func (p *Provider) exec() *action.Action {
	return &action.Action{
		ID:      "k8s.exec",
		Summary: "Run a command in a pod's container",
		Description: "Run one non-interactive command in a container and return the exit code, stdout and stderr. " +
			"command is a string (run with sh -c, needs a shell in the image) or an array of arguments. Good for " +
			"inspection (env, cat /etc/resolv.conf, ls); do not start interactive programs; raise timeout_sec for slow " +
			"commands. Pods with several containers need container. A command in a container can read the secrets " +
			"mounted in it (environment variables from Secrets, the service account token): never print them. " + prodHint,
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"context":     contextProperty,
				"namespace":   namespaceProperty,
				"pod":         map[string]any{"type": "string"},
				"container":   map[string]any{"type": "string", "description": "required if the pod has several containers"},
				"command":     map[string]any{"description": "a command line (string, run with sh -c) or an array of arguments", "anyOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}},
				"timeout_sec": map[string]any{"type": "integer", "minimum": 1, "maximum": 600, "description": "default 60"},
			},
			"required": []string{"context", "pod", "command"},
		},
		Positional:  []string{"context", "pod", "command"},
		RestArgs:    true,
		Annotations: action.Annotations{Destructive: true, OpenWorld: true},
		Label: func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
			return p.podLabel(raw, func(raw json.RawMessage) (action.Risk, string, error) {
				var in execInput
				if err := action.Decode(raw, &in); err != nil {
					return 0, "", err
				}
				_, line, err := in.argv()
				if err != nil {
					return 0, "", err
				}
				// At least write: any command can read the pod's secrets
				// (secretKeyRef env vars, the service account token).
				return max(policy.ClassifyCommand(line), action.Write), "$ " + line, nil
			})
		},
		Run: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in execInput
			if err := action.Decode(raw, &in); err != nil {
				return nil, err
			}
			argv, _, err := in.argv()
			if err != nil {
				return nil, err
			}
			r, pod, container, err := p.pod(ctx, in.podInput)
			if err != nil {
				return nil, err
			}
			timeout := time.Duration(in.TimeoutSec) * time.Second
			if in.TimeoutSec <= 0 {
				timeout = 60 * time.Second
			}
			timeout = min(timeout, 600*time.Second)
			runCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			start := time.Now()
			stdout, stderr := &cappedBuffer{limit: captureLimit}, &cappedBuffer{limit: captureLimit}
			err = r.client.Exec.Exec(runCtx, kube.ExecRequest{Namespace: pod.Namespace, Pod: pod.Name, Container: container,
				Command: argv, Stdout: stdout, Stderr: stderr})
			out := ExecOutput{Context: in.Context, ContextNote: r.info.Note, ContextTags: r.info.Tags, Namespace: pod.Namespace,
				Pod: pod.Name, Container: container, DurationMs: time.Since(start).Milliseconds(),
				Capped: stdout.capped() || stderr.capped()}
			if code, ok := kube.ExitCode(err); ok {
				out.ExitCode, err = code, nil
			}
			if err != nil {
				if errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
					return nil, fmt.Errorf("timed out after %s (the command may still be running in the container)", timeout)
				}
				return nil, err
			}
			lim := limits(ctx)
			if out.Stdout, err = p.Artifacts.Shape(stdout.String(), lim); err != nil {
				return nil, err
			}
			if out.Stderr, err = p.Artifacts.Shape(stderr.String(), lim); err != nil {
				return nil, err
			}
			return out, nil
		},
	}
}

// cappedBuffer keeps the first limit bytes written to it; it is safe for
// the concurrent writes of a stream.
type cappedBuffer struct {
	mu    sync.Mutex
	b     bytes.Buffer
	limit int
	over  bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := c.limit - c.b.Len(); room < len(p) {
		c.b.Write(p[:max(room, 0)])
		c.over = true
		return len(p), nil
	}
	return c.b.Write(p)
}

func (c *cappedBuffer) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.b.String()
}

func (c *cappedBuffer) capped() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.over
}

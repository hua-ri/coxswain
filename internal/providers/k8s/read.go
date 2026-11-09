package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	kube "github.com/hua-ri/coxswain/internal/k8s"
	"github.com/hua-ri/coxswain/internal/shaper"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
)

// ContextsOutput is the result of k8s.contexts.
type ContextsOutput struct {
	Kubeconfig []string           `json:"kubeconfig"` // the files read (never written)
	Current    string             `json:"current,omitempty"`
	Contexts   []kube.ContextInfo `json:"contexts"`
}

func (p *Provider) contexts() *action.Action {
	return &action.Action{
		ID:      "k8s.contexts",
		Summary: "List Kubernetes contexts",
		Description: "List the contexts of the user's kubeconfig with their cluster, default namespace, tags " +
			"(env=prod marks production) and notes. A context's note may restrict what you may do; always respect it. " +
			"Contexts on the same cluster server share their tags and notes (inherited_from names where they come from). " +
			"Pass the context name to every other k8s tool; do not assume the current context.",
		Input:       map[string]any{"type": "object", "properties": map[string]any{}},
		Annotations: action.Annotations{ReadOnly: true, Idempotent: true},
		Label: func(context.Context, json.RawMessage) (action.Assessment, error) {
			return action.Assessment{Risk: action.Read}, nil
		},
		Run: func(context.Context, json.RawMessage) (any, error) {
			current, list, err := p.Clusters.Contexts()
			if err != nil {
				return nil, err
			}
			return ContextsOutput{Kubeconfig: p.Clusters.Kubeconfig.Paths(), Current: current, Contexts: list}, nil
		},
	}
}

type tagInput struct {
	Context string   `json:"context"`
	Set     []string `json:"set"`
	Unset   []string `json:"unset"`
	Note    *string  `json:"note"`
}

func (p *Provider) contextsTag() *action.Action {
	return &action.Action{
		ID:      "k8s.contexts.tag",
		Summary: "Set or remove tags and the note of a Kubernetes context",
		Description: "Change a context's tags (key=value) or note, kept by Coxswain (never in the kubeconfig). Tags " +
			"(env=prod marks production) and the note tell agents how to treat the cluster; change them only when the user asks.",
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"context": contextProperty,
				"set":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "tags to set, key=value"},
				"unset":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "tag keys to remove"},
				"note":    map[string]any{"type": "string", "description": "replace the note"},
			},
			"required": []string{"context"},
		},
		Positional:  []string{"context"},
		Annotations: action.Annotations{Destructive: true, Idempotent: true},
		Label: func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
			var in tagInput
			if err := action.Decode(raw, &in); err != nil {
				return action.Assessment{}, err
			}
			if _, err := p.Clusters.Context(in.Context); err != nil {
				return action.Assessment{}, err
			}
			own, err := p.Clusters.Meta.Get(in.Context)
			if err != nil {
				return action.Assessment{}, err
			}
			risk := action.Write
			keys := append([]string{}, in.Unset...)
			for _, kv := range in.Set {
				k, _, ok := strings.Cut(kv, "=")
				if !ok || k == "" {
					return action.Assessment{}, fmt.Errorf("tag %q must be key=value", kv)
				}
				keys = append(keys, k)
			}
			for _, k := range keys {
				if k == "env" {
					risk = action.Destructive // they tell agents how careful to be
				}
			}
			if in.Note != nil && *in.Note != own.Note {
				risk = action.Destructive // the note instructs agents, like the env tag
			}
			preview := fmt.Sprintf("context %s: set %v, unset %v", in.Context, in.Set, in.Unset)
			if in.Note != nil {
				preview += fmt.Sprintf(", note %q", *in.Note)
			}
			return action.Assessment{Risk: risk, Targets: []action.Target{p.Clusters.Target(in.Context, "")}, Preview: preview}, nil
		},
		Run: func(_ context.Context, raw json.RawMessage) (any, error) {
			var in tagInput
			if err := action.Decode(raw, &in); err != nil {
				return nil, err
			}
			if _, err := p.Clusters.Context(in.Context); err != nil {
				return nil, err
			}
			err := p.Clusters.Meta.Update(func(m map[string]kube.Meta) error {
				meta := m[in.Context]
				tags := maps.Clone(meta.Tags)
				if tags == nil {
					tags = map[string]string{}
				}
				for _, kv := range in.Set {
					k, v, _ := strings.Cut(kv, "=")
					tags[k] = v
				}
				for _, k := range in.Unset {
					delete(tags, k)
				}
				meta.Tags = tags
				if in.Note != nil {
					meta.Note = *in.Note
				}
				m[in.Context] = meta
				return nil
			})
			if err != nil {
				return nil, err
			}
			return p.Clusters.Context(in.Context)
		},
	}
}

// GetOutput is the result of k8s.get: a compact table, or one object's
// YAML when a name was given.
type GetOutput struct {
	Context       string     `json:"context"`
	Kind          string     `json:"kind"`
	Namespace     string     `json:"namespace,omitempty"`
	AllNamespaces bool       `json:"all_namespaces,omitempty"`
	Columns       []string   `json:"columns,omitempty"`
	Rows          [][]string `json:"rows,omitempty"`
	Total         int        `json:"total"`          // rows before shortening
	More          bool       `json:"more,omitempty"` // the limit cut the list
	Truncated     bool       `json:"truncated,omitempty"`
	ArtifactID    string     `json:"artifact_id,omitempty"` // the full table, if truncated

	Name string         `json:"name,omitempty"`
	YAML *shaper.Shaped `json:"yaml,omitempty"`
}

// PrintCLI prints the table (or the YAML) for a terminal.
func (o GetOutput) PrintCLI(stdout, _ io.Writer) int {
	if o.YAML != nil {
		io.WriteString(stdout, o.YAML.Text)
		return 0
	}
	if len(o.Rows) == 0 {
		fmt.Fprintf(stdout, "No %s found.\n", o.Kind)
		return 0
	}
	writeTable(stdout, o.Columns, o.Rows)
	if o.More {
		fmt.Fprintln(stdout, "(more objects exist; raise --limit or narrow with --label-selector)")
	}
	return 0
}

func writeTable(w io.Writer, cols []string, rows [][]string) {
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, strings.ToUpper(strings.Join(cols, "\t")))
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	tw.Flush()
}

type getInput struct {
	scope
	LabelSelector string `json:"label_selector"`
	FieldSelector string `json:"field_selector"`
	Limit         int    `json:"limit"`
}

func (p *Provider) get() *action.Action {
	return &action.Action{
		ID:      "k8s.get",
		Summary: "List Kubernetes objects, or show one as YAML",
		Description: "List objects of a kind as a compact table (pods: ready/status/restarts/node; deployments: " +
			"ready/up-to-date/available; nodes: status/roles/version/IP; services: type/IP/ports; others: status " +
			"conditions). With name, return that object's YAML. Secret values are never returned, only keys and lengths. " +
			"Use k8s_describe for an object's events and container states, k8s_diagnose to find why a workload is unhealthy.",
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"context":        contextProperty,
				"kind":           kindProperty,
				"name":           map[string]any{"type": "string", "description": "one object: return its YAML"},
				"namespace":      namespaceProperty,
				"all_namespaces": map[string]any{"type": "boolean"},
				"label_selector": map[string]any{"type": "string", "description": "e.g. app=web,tier!=db"},
				"field_selector": map[string]any{"type": "string", "description": "e.g. status.phase=Running"},
				"limit":          map[string]any{"type": "integer", "minimum": 1, "maximum": 1000, "description": "default 200"},
			},
			"required": []string{"context", "kind"},
		},
		Positional:  []string{"context", "kind", "name"},
		Annotations: readOnlyRemote,
		Label:       readLabel("get", p),
		Run: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in getInput
			if err := action.Decode(raw, &in); err != nil {
				return nil, err
			}
			r, err := p.resolve(in.scope)
			if err != nil {
				return nil, err
			}
			out := GetOutput{Context: in.Context, Kind: r.res.Kind, Namespace: r.ns, AllNamespaces: in.AllNamespaces && r.res.Namespaced}
			if in.Name != "" {
				if in.AllNamespaces {
					return nil, fmt.Errorf("name and all_namespaces cannot be combined")
				}
				obj, err := r.resource().Get(ctx, in.Name, metav1.GetOptions{})
				if err != nil {
					return nil, kube.Explain(err)
				}
				y, err := toYAML(clean(obj))
				if err != nil {
					return nil, err
				}
				shaped, err := p.shape(ctx, y)
				if err != nil {
					return nil, err
				}
				out.Name, out.YAML, out.Total = in.Name, &shaped, 1
				return out, nil
			}
			limit := in.Limit
			if limit <= 0 {
				limit = 200
			}
			limit = min(limit, 1000)
			list, err := r.resource().List(ctx, metav1.ListOptions{LabelSelector: in.LabelSelector, FieldSelector: in.FieldSelector, Limit: int64(limit)})
			if err != nil {
				return nil, kube.Explain(err)
			}
			items := list.Items
			sortObjects(items)
			if len(items) > limit || list.GetContinue() != "" {
				out.More = true
				items = items[:min(limit, len(items))]
			}
			out.Columns, out.Rows = table(r.res.Kind, items, out.AllNamespaces, p.now())
			out.Total = len(out.Rows)
			return out, p.fitRows(ctx, &out)
		},
	}
}

func sortObjects(items []unstructured.Unstructured) {
	sort.SliceStable(items, func(i, j int) bool {
		if a, b := items[i].GetNamespace(), items[j].GetNamespace(); a != b {
			return a < b
		}
		return items[i].GetName() < items[j].GetName()
	})
}

// fitRows shortens a table for agents: if it is too large, the full table
// is saved as an artifact and only the first rows are kept.
func (p *Provider) fitRows(ctx context.Context, out *GetOutput) error {
	lim := limits(ctx)
	var b strings.Builder
	writeTable(&b, out.Columns, out.Rows)
	text := b.String()
	if len(text) <= lim.MaxBytes && len(out.Rows) < lim.MaxLines {
		return nil
	}
	id, err := p.Artifacts.Save(text)
	if err != nil {
		return err
	}
	size, n := 0, 0
	for n < len(out.Rows) && n < lim.MaxLines/2 {
		size += len(strings.Join(out.Rows[n], "\t")) + 8
		if size > lim.MaxBytes/2 {
			break
		}
		n++
	}
	out.Rows, out.Truncated, out.ArtifactID = out.Rows[:n], true, id
	return nil
}

// EventRow is one event, compact.
type EventRow struct {
	Namespace string `json:"namespace,omitempty"`
	Type      string `json:"type"`
	Reason    string `json:"reason"`
	Object    string `json:"object"` // Kind/name
	Message   string `json:"message"`
	Count     int32  `json:"count,omitempty"`
	Age       string `json:"age"`
	LastSeen  string `json:"last_seen,omitempty"` // RFC 3339
}

func (p *Provider) eventRows(evs []corev1.Event, namespaces bool) []EventRow {
	sort.SliceStable(evs, func(i, j int) bool { return eventTime(&evs[i]).After(eventTime(&evs[j])) })
	now := p.now()
	out := make([]EventRow, 0, len(evs))
	for i := range evs {
		e := &evs[i]
		row := EventRow{Type: e.Type, Reason: e.Reason, Object: e.InvolvedObject.Kind + "/" + e.InvolvedObject.Name,
			Message: strings.TrimSpace(e.Message), Count: e.Count, Age: age(now, eventTime(e))}
		if t := eventTime(e); !t.IsZero() {
			row.LastSeen = t.UTC().Format(time.RFC3339)
		}
		if namespaces {
			row.Namespace = e.Namespace
		}
		out = append(out, row)
	}
	return out
}

// objectEvents returns up to max events about one object, newest first.
func (p *Provider) objectEvents(ctx context.Context, c *kube.Client, ns, kind, name string, max int) ([]EventRow, error) {
	sel := fields.Set{"involvedObject.name": name, "involvedObject.kind": kind}.AsSelector().String()
	list, err := c.Typed.CoreV1().Events(ns).List(ctx, metav1.ListOptions{FieldSelector: sel})
	if err != nil {
		return nil, kube.Explain(err)
	}
	var evs []corev1.Event
	for _, e := range list.Items { // field selectors are not applied by every server (or fake)
		if e.InvolvedObject.Name == name && strings.EqualFold(e.InvolvedObject.Kind, kind) {
			evs = append(evs, e)
		}
	}
	rows := p.eventRows(evs, false)
	return rows[:min(len(rows), max)], nil
}

// ContainerInfo is one container of a pod.
type ContainerInfo struct {
	Name      string `json:"name"`
	Image     string `json:"image"`
	Init      bool   `json:"init,omitempty"`
	Ready     bool   `json:"ready"`
	Restarts  int32  `json:"restarts"`
	State     string `json:"state,omitempty"`
	LastState string `json:"last_state,omitempty"`
}

// DescribeOutput is the result of k8s.describe.
type DescribeOutput struct {
	Context    string            `json:"context"`
	Kind       string            `json:"kind"`
	Namespace  string            `json:"namespace,omitempty"`
	Name       string            `json:"name"`
	Summary    map[string]string `json:"summary"`
	Labels     map[string]string `json:"labels,omitempty"`
	Owners     []string          `json:"owners,omitempty"` // Kind/name
	Conditions []Condition       `json:"conditions,omitempty"`
	Containers []ContainerInfo   `json:"containers,omitempty"`
	Keys       []string          `json:"keys,omitempty"` // Secrets and ConfigMaps: key(length)
	Events     []EventRow        `json:"events"`
}

func (p *Provider) describe() *action.Action {
	return &action.Action{
		ID:      "k8s.describe",
		Summary: "Describe a Kubernetes object: summary, conditions, containers and events",
		Description: "Show one object's summary, conditions, its recent events and, for pods, each container's state " +
			"and last termination (reason, exit code). Secret values are never returned. For an unhealthy workload " +
			"prefer k8s_diagnose, which also follows controllers to their pods and nodes.",
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"context":   contextProperty,
				"kind":      kindProperty,
				"name":      map[string]any{"type": "string"},
				"namespace": namespaceProperty,
			},
			"required": []string{"context", "kind", "name"},
		},
		Positional:  []string{"context", "kind", "name"},
		Annotations: readOnlyRemote,
		Label:       readLabel("describe", p),
		Run: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in scope
			if err := action.Decode(raw, &in); err != nil {
				return nil, err
			}
			if in.Name == "" {
				return nil, fmt.Errorf("name is required")
			}
			in.AllNamespaces = false
			r, err := p.resolve(in)
			if err != nil {
				return nil, err
			}
			obj, err := r.resource().Get(ctx, in.Name, metav1.GetOptions{})
			if err != nil {
				return nil, kube.Explain(err)
			}
			return p.describeObject(ctx, r, obj)
		},
	}
}

func (p *Provider) describeObject(ctx context.Context, r resolved, obj *unstructured.Unstructured) (DescribeOutput, error) {
	now := p.now()
	out := DescribeOutput{Context: r.client.Context, Kind: r.res.Kind, Namespace: obj.GetNamespace(), Name: obj.GetName(),
		Summary: summaryMap(r.res.Kind, obj, now), Labels: obj.GetLabels(), Conditions: conditions(obj)}
	for _, o := range obj.GetOwnerReferences() {
		out.Owners = append(out.Owners, o.Kind+"/"+o.Name)
	}
	switch r.res.Kind {
	case "Pod":
		out.Containers = containers(convert[corev1.Pod](obj), now)
	case "Secret":
		out.Keys = secretKeys(obj)
	case "ConfigMap":
		for _, field := range []string{"data", "binaryData"} {
			m, _, _ := unstructured.NestedMap(obj.Object, field)
			for k, v := range m {
				s, _ := v.(string)
				out.Keys = append(out.Keys, k+"("+strconv.Itoa(len(s))+")")
			}
		}
		sort.Strings(out.Keys)
	}
	evs, err := p.objectEvents(ctx, r.client, obj.GetNamespace(), r.res.Kind, obj.GetName(), 20)
	if err != nil {
		return out, err
	}
	out.Events = evs
	return out, nil
}

func containers(pod *corev1.Pod, now time.Time) []ContainerInfo {
	statuses := map[string]corev1.ContainerStatus{}
	for _, s := range append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...) {
		statuses[s.Name] = s
	}
	var out []ContainerInfo
	add := func(c corev1.Container, init bool) {
		s := statuses[c.Name]
		out = append(out, ContainerInfo{Name: c.Name, Image: c.Image, Init: init, Ready: s.Ready, Restarts: s.RestartCount,
			State: stateString(s.State, now), LastState: stateString(s.LastTerminationState, now)})
	}
	for _, c := range pod.Spec.InitContainers {
		add(c, true)
	}
	for _, c := range pod.Spec.Containers {
		add(c, false)
	}
	return out
}

// stateString describes a container state, e.g. "waiting: CrashLoopBackOff
// (back-off 5m0s)" or "terminated: OOMKilled, exit code 137, 3m ago".
func stateString(s corev1.ContainerState, now time.Time) string {
	switch {
	case s.Waiting != nil:
		out := "waiting: " + dash(s.Waiting.Reason)
		if s.Waiting.Message != "" {
			out += " (" + clipLine(s.Waiting.Message, 300) + ")"
		}
		return out
	case s.Running != nil:
		if s.Running.StartedAt.IsZero() {
			return "running"
		}
		return "running since " + age(now, s.Running.StartedAt.Time)
	case s.Terminated != nil:
		t := s.Terminated
		out := fmt.Sprintf("terminated: %s, exit code %d", dash(t.Reason), t.ExitCode)
		if t.Signal != 0 {
			out += fmt.Sprintf(", signal %d", t.Signal)
		}
		if !t.FinishedAt.IsZero() {
			out += ", " + age(now, t.FinishedAt.Time) + " ago"
		}
		if t.Message != "" {
			out += " (" + clipLine(t.Message, 300) + ")"
		}
		return out
	}
	return ""
}

func clipLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// EventsOutput is the result of k8s.events.
type EventsOutput struct {
	Context   string     `json:"context"`
	Namespace string     `json:"namespace,omitempty"`
	Events    []EventRow `json:"events"`
	More      bool       `json:"more,omitempty"`
}

type eventsInput struct {
	scope
	Type  string `json:"type"`
	Since string `json:"since"`
	Limit int    `json:"limit"`
}

// parseSince parses a duration such as 90s, 15m, 2h or 3d.
func parseSince(s string, def time.Duration) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	if d, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(d)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("invalid since %q (e.g. 30m, 2h, 1d)", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("invalid since %q (e.g. 30m, 2h, 1d)", s)
	}
	return d, nil
}

func (p *Provider) events() *action.Action {
	return &action.Action{
		ID:      "k8s.events",
		Summary: "List recent Kubernetes events",
		Description: "List recent events in a namespace (or all), newest first: by default only Warning events of the " +
			"last hour. type: Warning, Normal or all.",
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"context":        contextProperty,
				"namespace":      namespaceProperty,
				"all_namespaces": map[string]any{"type": "boolean"},
				"type":           map[string]any{"type": "string", "enum": []string{"Warning", "Normal", "all"}, "description": "default Warning"},
				"since":          map[string]any{"type": "string", "description": "e.g. 30m, 2h, 1d; default 1h"},
				"limit":          map[string]any{"type": "integer", "minimum": 1, "maximum": 1000, "description": "default 200"},
			},
			"required": []string{"context"},
		},
		Positional:  []string{"context"},
		Annotations: readOnlyRemote,
		Label:       readLabel("events", p),
		Run: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in eventsInput
			if err := action.Decode(raw, &in); err != nil {
				return nil, err
			}
			since, err := parseSince(in.Since, time.Hour)
			if err != nil {
				return nil, err
			}
			typ := in.Type
			switch strings.ToLower(typ) {
			case "":
				typ = corev1.EventTypeWarning
			case "all":
				typ = ""
			case "warning":
				typ = corev1.EventTypeWarning
			case "normal":
				typ = corev1.EventTypeNormal
			default:
				return nil, fmt.Errorf("type must be Warning, Normal or all")
			}
			limit := in.Limit
			if limit <= 0 {
				limit = 200
			}
			limit = min(limit, 1000)
			in.Kind = ""
			r, err := p.resolve(in.scope)
			if err != nil {
				return nil, err
			}
			opts := metav1.ListOptions{}
			if typ != "" {
				opts.FieldSelector = fields.OneTermEqualSelector("type", typ).String()
			}
			list, err := r.client.Typed.CoreV1().Events(r.ns).List(ctx, opts)
			if err != nil {
				return nil, kube.Explain(err)
			}
			cutoff := p.now().Add(-since)
			var evs []corev1.Event
			for _, e := range list.Items {
				if (typ == "" || e.Type == typ) && !eventTime(&e).Before(cutoff) {
					evs = append(evs, e)
				}
			}
			out := EventsOutput{Context: in.Context, Namespace: r.ns, Events: p.eventRows(evs, in.AllNamespaces)}
			if len(out.Events) > limit {
				out.Events, out.More = out.Events[:limit], true
			}
			return out, nil
		},
	}
}

// tagString renders tags as k=v,… sorted.
func tagString(tags map[string]string) string {
	keys := make([]string, 0, len(tags))
	for k, v := range tags {
		keys = append(keys, k+"="+v)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

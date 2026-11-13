package argo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/argo"
	"github.com/hua-ri/coxswain/internal/shaper"
	"github.com/hua-ri/coxswain/internal/textdiff"
	"sigs.k8s.io/yaml"
)

func (p *Provider) registerCD(r *action.Registry) {
	r.Register(p.cdApps())
	r.Register(p.cdApp())
	r.Register(p.cdTree())
	r.Register(p.cdDiff())
	r.Register(p.cdEvents())
	r.Register(p.cdLogs())
	r.Register(p.cdRefresh())
	r.Register(p.cdSync())
	r.Register(p.cdRollback())
	r.Register(p.cdTerminate())
	r.Register(p.cdDiagnose())
}

// cdInput is what the argocd.* app actions take.
type cdInput struct {
	Instance   string   `json:"instance"`
	App        string   `json:"app"`
	AppNS      string   `json:"app_namespace"`
	Project    string   `json:"project"`
	Selector   string   `json:"selector"`
	Hard       bool     `json:"hard"`
	Revision   string   `json:"revision"`
	Prune      bool     `json:"prune"`
	DryRun     bool     `json:"dry_run"`
	Resources  []string `json:"resources"`
	ID         int64    `json:"id"`
	Pod        string   `json:"pod"`
	Container  string   `json:"container"`
	Namespace  string   `json:"namespace"`
	Kind       string   `json:"kind"`
	Name       string   `json:"name"`
	Tail       int      `json:"tail"`
	Previous   bool     `json:"previous"`
	OnlyIssues bool     `json:"only_issues"`
}

var (
	appProperty   = str("the Application's name")
	appNSProperty = str("the Application's namespace, if apps live outside the Argo CD namespace (apps in any namespace)")
)

func cdSchema(extra map[string]any, required ...string) map[string]any {
	props := map[string]any{"instance": instanceProperty}
	for k, v := range extra {
		props[k] = v
	}
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func appProps(extra map[string]any) map[string]any {
	out := map[string]any{"app": appProperty, "app_namespace": appNSProperty}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// cdAction builds an argocd.* action: arguments checked exactly, the
// instance resolved, and the label's targets set.
func (p *Provider) cdAction(a *action.Action, label func(argo.Instance, cdInput) (action.Assessment, error),
	run func(context.Context, argo.Instance, cdInput) (any, error)) *action.Action {
	parse := func(raw json.RawMessage) (argo.Instance, cdInput, error) {
		var in cdInput
		if err := decode(raw, a.Input, &in); err != nil {
			return argo.Instance{}, in, err
		}
		i, err := p.instance(argo.CD, in.Instance)
		return i, in, err
	}
	a.Label = func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
		i, in, err := parse(raw)
		if err != nil {
			return action.Assessment{}, err
		}
		as, err := label(i, in)
		if len(as.Targets) == 0 {
			as.Targets = []action.Target{target(i, in.App)}
		}
		return as, err
	}
	a.Run = func(ctx context.Context, raw json.RawMessage) (any, error) {
		i, in, err := parse(raw)
		if err != nil {
			return nil, err
		}
		return run(ctx, i, in)
	}
	return a
}

func read(preview string) func(argo.Instance, cdInput) (action.Assessment, error) {
	return func(i argo.Instance, in cdInput) (action.Assessment, error) {
		p := preview
		if in.App != "" {
			p += " " + in.App
		}
		return action.Assessment{Risk: action.Read, Preview: p + " on " + i.Name}, nil
	}
}

func appPath(in cdInput, sub string) string {
	p := "/api/v1/applications/" + url.PathEscape(in.App) + sub
	if in.AppNS != "" {
		sep := "?"
		if strings.Contains(p, "?") {
			sep = "&"
		}
		p += sep + "appNamespace=" + url.QueryEscape(in.AppNS)
	}
	return p
}

func needApp(in cdInput) error {
	if in.App == "" {
		return errors.New("app is required (see argocd_apps)")
	}
	return nil
}

// AppSummary is one Application in a list.
type AppSummary struct {
	Name        string     `json:"name"`
	Namespace   string     `json:"namespace,omitempty"`
	Project     string     `json:"project"`
	Source      string     `json:"source"`
	Destination string     `json:"destination"`
	Sync        string     `json:"sync"`
	Health      string     `json:"health"`
	AutoSync    bool       `json:"auto_sync,omitempty"`
	Revision    string     `json:"revision,omitempty"`
	LastSync    *time.Time `json:"last_sync,omitempty"`
	LastResult  string     `json:"last_result,omitempty"`
	Problem     string     `json:"problem,omitempty"` // first condition
}

func summarize(a argo.Application) AppSummary {
	s := AppSummary{Name: a.Metadata.Name, Namespace: a.Metadata.Namespace, Project: a.Spec.Project,
		Sync: a.Status.Sync.Status, Health: a.Status.Health.Status, Revision: short(a.Status.Sync.Revision)}
	srcs := a.Spec.Sources
	if a.Spec.Source != nil {
		srcs = append([]argo.AppSource{*a.Spec.Source}, srcs...)
	}
	var parts []string
	for _, src := range srcs {
		x := src.RepoURL
		switch {
		case src.Chart != "":
			x += " " + src.Chart
		case src.Path != "":
			x += " " + src.Path
		}
		if src.TargetRevision != "" {
			x += "@" + src.TargetRevision
		}
		parts = append(parts, x)
	}
	s.Source = strings.Join(parts, ", ")
	d := a.Spec.Destination
	s.Destination = d.Server
	if d.Name != "" {
		s.Destination = d.Name
	}
	if d.Namespace != "" {
		s.Destination += "/" + d.Namespace
	}
	s.AutoSync = a.Spec.SyncPolicy != nil && a.Spec.SyncPolicy.Automated != nil
	if op := a.Status.OperationState; op != nil {
		s.LastSync = op.FinishedAt
		if s.LastSync == nil {
			s.LastSync = op.StartedAt
		}
		s.LastResult = op.Phase
	}
	if len(a.Status.Conditions) > 0 {
		c := a.Status.Conditions[0]
		s.Problem = c.Type + ": " + firstLine(c.Message)
	}
	return s
}

func short(rev string) string {
	if len(rev) == 40 {
		return rev[:8]
	}
	return rev
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// AppsOutput is the result of argocd.apps.
type AppsOutput struct {
	Instance     string            `json:"instance"`
	InstanceNote string            `json:"instance_note,omitempty"`
	InstanceTags map[string]string `json:"instance_tags,omitempty"`
	Apps         []AppSummary      `json:"apps"`
	Counts       map[string]int    `json:"counts"`
}

func (p *Provider) cdApps() *action.Action {
	return p.cdAction(&action.Action{
		ID:      "argocd.apps",
		Summary: "List Argo CD applications with sync and health",
		Description: "Lists Applications (name, project, source, destination, sync, health, last sync). " +
			"only_issues keeps those not Synced or not Healthy. Respect the instance's note.",
		Input: cdSchema(map[string]any{
			"project":     str("only this project"),
			"selector":    str("label selector, e.g. team=payments"),
			"only_issues": map[string]any{"type": "boolean", "description": "only apps that are not Synced+Healthy"},
		}),
		Positional:  []string{"instance"},
		Annotations: action.Annotations{ReadOnly: true, Idempotent: true, OpenWorld: true},
	}, read("list applications"), func(ctx context.Context, i argo.Instance, in cdInput) (any, error) {
		q := url.Values{}
		if in.Project != "" {
			q.Set("projects", in.Project)
		}
		if in.Selector != "" {
			q.Set("selector", in.Selector)
		}
		var list struct {
			Items []argo.Application `json:"items"`
		}
		if err := p.Sessions.CD(ctx, i, http.MethodGet, "/api/v1/applications?"+q.Encode(), nil, &list); err != nil {
			return nil, err
		}
		out := AppsOutput{Instance: i.Name, InstanceNote: i.Note, InstanceTags: i.Tags, Apps: []AppSummary{}, Counts: map[string]int{}}
		for _, a := range list.Items {
			s := summarize(a)
			out.Counts[s.Sync+"/"+s.Health]++
			if in.OnlyIssues && s.Sync == "Synced" && s.Health == "Healthy" && s.Problem == "" {
				continue
			}
			out.Apps = append(out.Apps, s)
		}
		sort.Slice(out.Apps, func(a, b int) bool { return out.Apps[a].Name < out.Apps[b].Name })
		return out, nil
	})
}

// AppDetail is the result of argocd.app.
type AppDetail struct {
	AppSummary
	InstanceNote string              `json:"instance_note,omitempty"`
	HealthMsg    string              `json:"health_message,omitempty"`
	Conditions   []argo.AppCondition `json:"conditions,omitempty"`
	SelfHeal     bool                `json:"self_heal,omitempty"`
	AutoPrune    bool                `json:"auto_prune,omitempty"`
	Operation    *OperationView      `json:"last_operation,omitempty"`
	Running      bool                `json:"operation_running,omitempty"`
	History      []HistoryView       `json:"history,omitempty"`
	Resources    map[string]int      `json:"resources"` // "Synced/Healthy": n
	Unhealthy    []ResourceView      `json:"unhealthy_or_out_of_sync,omitempty"`
}

type OperationView struct {
	Phase      string     `json:"phase"`
	Message    string     `json:"message,omitempty"`
	Revision   string     `json:"revision,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Failed     []string   `json:"failed_resources,omitempty"`
}

type HistoryView struct {
	ID         int64      `json:"id"`
	Revision   string     `json:"revision"`
	DeployedAt *time.Time `json:"deployed_at,omitempty"`
	By         string     `json:"by,omitempty"`
}

type ResourceView struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Sync      string `json:"sync,omitempty"`
	Health    string `json:"health,omitempty"`
	Message   string `json:"message,omitempty"`
	Prune     bool   `json:"requires_pruning,omitempty"`
}

func detail(i argo.Instance, a argo.Application) AppDetail {
	d := AppDetail{AppSummary: summarize(a), InstanceNote: i.Note, HealthMsg: firstLine(a.Status.Health.Message),
		Conditions: a.Status.Conditions, Running: a.Operation != nil, Resources: map[string]int{}}
	if sp := a.Spec.SyncPolicy; sp != nil && sp.Automated != nil {
		d.SelfHeal, d.AutoPrune = sp.Automated.SelfHeal, sp.Automated.Prune
	}
	if op := a.Status.OperationState; op != nil {
		v := &OperationView{Phase: op.Phase, Message: firstLine(op.Message), StartedAt: op.StartedAt, FinishedAt: op.FinishedAt}
		if op.SyncResult != nil {
			v.Revision = short(op.SyncResult.Revision)
			for _, r := range op.SyncResult.Resources {
				if r.Status != "" && r.Status != "Synced" && r.Status != "PruneSkipped" {
					v.Failed = append(v.Failed, fmt.Sprintf("%s/%s %s: %s", r.Kind, r.Name, r.Status, firstLine(r.Message)))
				}
			}
		}
		d.Operation = v
	}
	h := a.Status.History
	for k := len(h) - 1; k >= 0 && len(d.History) < 10; k-- {
		e := h[k]
		v := HistoryView{ID: e.ID, Revision: short(e.Revision), DeployedAt: e.DeployedAt}
		if v.Revision == "" && len(e.Revisions) > 0 {
			v.Revision = short(strings.Join(e.Revisions, ","))
		}
		if e.InitiatedBy != nil {
			v.By = e.InitiatedBy.Username
			if e.InitiatedBy.Automated {
				v.By = "auto-sync"
			}
		}
		d.History = append(d.History, v)
	}
	for _, r := range a.Status.Resources {
		health := ""
		msg := ""
		if r.Health != nil {
			health, msg = r.Health.Status, r.Health.Message
		}
		d.Resources[r.Status+"/"+health]++
		if (r.Status != "" && r.Status != "Synced") || (health != "" && health != "Healthy") || r.RequiresPruning {
			d.Unhealthy = append(d.Unhealthy, ResourceView{Kind: r.Kind, Namespace: r.Namespace, Name: r.Name,
				Sync: r.Status, Health: health, Message: firstLine(msg), Prune: r.RequiresPruning})
		}
	}
	return d
}

func (p *Provider) getApp(ctx context.Context, i argo.Instance, in cdInput, refresh string) (argo.Application, error) {
	var a argo.Application
	path := appPath(in, "")
	if refresh != "" {
		path = appPath(in, "?refresh="+refresh)
	}
	err := p.Sessions.CD(ctx, i, http.MethodGet, path, nil, &a)
	return a, err
}

func (p *Provider) cdApp() *action.Action {
	return p.cdAction(&action.Action{
		ID:      "argocd.app",
		Summary: "Show an Argo CD application: status, last sync, history",
		Description: "Sources, destination, sync policy, conditions, the last operation (with failed resources), " +
			"the last 10 deployments (ids for argocd_rollback), and the resources that are not Synced or not Healthy.",
		Input:       cdSchema(appProps(nil), "app"),
		Positional:  []string{"app"},
		Annotations: action.Annotations{ReadOnly: true, Idempotent: true, OpenWorld: true},
	}, read("show application"), func(ctx context.Context, i argo.Instance, in cdInput) (any, error) {
		a, err := p.getApp(ctx, i, in, "")
		if err != nil {
			return nil, err
		}
		return detail(i, a), nil
	})
}

// TreeOutput is the result of argocd.tree.
type TreeOutput struct {
	App   string     `json:"app"`
	Nodes []TreeNode `json:"nodes"`
	Total int        `json:"total"`
}

type TreeNode struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Parent    string `json:"parent,omitempty"` // Kind/name
	Health    string `json:"health,omitempty"`
	Message   string `json:"message,omitempty"`
	Info      string `json:"info,omitempty"` // e.g. Pod status, restarts
}

func (p *Provider) tree(ctx context.Context, i argo.Instance, in cdInput) ([]TreeNode, error) {
	var t argo.ResourceTree
	if err := p.Sessions.CD(ctx, i, http.MethodGet, appPath(in, "/resource-tree"), nil, &t); err != nil {
		return nil, err
	}
	var out []TreeNode
	for _, n := range t.Nodes {
		v := TreeNode{Kind: n.Kind, Namespace: n.Namespace, Name: n.Name}
		if len(n.ParentRefs) > 0 {
			v.Parent = n.ParentRefs[0].Kind + "/" + n.ParentRefs[0].Name
		}
		if n.Health != nil {
			v.Health, v.Message = n.Health.Status, firstLine(n.Health.Message)
		}
		var info []string
		for _, x := range n.Info {
			info = append(info, x.Name+"="+x.Value)
		}
		v.Info = strings.Join(info, " ")
		out = append(out, v)
	}
	// Unhealthy first, then by kind and name.
	rank := func(h string) int {
		switch h {
		case "Degraded", "Missing":
			return 0
		case "Progressing", "Suspended", "Unknown":
			return 1
		case "":
			return 3
		}
		return 2
	}
	sort.SliceStable(out, func(a, b int) bool {
		if ra, rb := rank(out[a].Health), rank(out[b].Health); ra != rb {
			return ra < rb
		}
		if out[a].Kind != out[b].Kind {
			return out[a].Kind < out[b].Kind
		}
		return out[a].Name < out[b].Name
	})
	return out, nil
}

func (p *Provider) cdTree() *action.Action {
	return p.cdAction(&action.Action{
		ID:      "argocd.tree",
		Summary: "Show an application's live resources (Deployments, Pods…)",
		Description: "The resource tree Argo CD sees, including Pods and ReplicaSets, with health; unhealthy first. " +
			"only_issues keeps the unhealthy ones.",
		Input: cdSchema(appProps(map[string]any{
			"only_issues": map[string]any{"type": "boolean", "description": "only resources that are not Healthy"},
		}), "app"),
		Positional:  []string{"app"},
		Annotations: action.Annotations{ReadOnly: true, Idempotent: true, OpenWorld: true},
	}, read("resource tree of"), func(ctx context.Context, i argo.Instance, in cdInput) (any, error) {
		nodes, err := p.tree(ctx, i, in)
		if err != nil {
			return nil, err
		}
		out := TreeOutput{App: in.App, Total: len(nodes), Nodes: []TreeNode{}}
		for _, n := range nodes {
			if in.OnlyIssues && (n.Health == "" || n.Health == "Healthy") {
				continue
			}
			out.Nodes = append(out.Nodes, n)
			if len(out.Nodes) == 300 {
				break
			}
		}
		return out, nil
	})
}

// DiffOutput is the result of argocd.diff.
type DiffOutput struct {
	App   string       `json:"app"`
	Diffs []ObjectDiff `json:"diffs"`
	Same  int          `json:"in_sync"`
}

type ObjectDiff struct {
	Object string        `json:"object"` // Kind ns/name
	Change string        `json:"change"` // modified, added (not in the cluster), removed (would be pruned)
	Diff   shaper.Shaped `json:"diff"`
}

// redactSecretYAML replaces a Secret's values with their lengths (Argo CD
// masks them already; this does not rely on it).
func redactSecretYAML(kind, state string) string {
	if state == "" || !strings.EqualFold(kind, "Secret") {
		return state
	}
	var obj map[string]any
	if json.Unmarshal([]byte(state), &obj) != nil {
		return "<secret, redacted>"
	}
	for _, f := range []string{"data", "stringData"} {
		if m, ok := obj[f].(map[string]any); ok {
			for k, v := range m {
				s, _ := v.(string)
				m[k] = fmt.Sprintf("<redacted %d bytes>", len(s))
			}
		}
	}
	if md, ok := obj["metadata"].(map[string]any); ok {
		if ann, ok := md["annotations"].(map[string]any); ok {
			delete(ann, "kubectl.kubernetes.io/last-applied-configuration")
		}
	}
	b, _ := json.Marshal(obj)
	return string(b)
}

// toYAML renders a JSON object (Argo CD returns states as JSON strings)
// as YAML without the fields that always differ.
func toYAML(state string) string {
	if state == "" || state == "null" {
		return ""
	}
	var obj map[string]any
	if json.Unmarshal([]byte(state), &obj) != nil {
		return state
	}
	delete(obj, "status")
	if md, ok := obj["metadata"].(map[string]any); ok {
		for _, f := range []string{"managedFields", "resourceVersion", "generation", "uid", "creationTimestamp"} {
			delete(md, f)
		}
	}
	b, err := yaml.Marshal(obj)
	if err != nil {
		return state
	}
	return string(b)
}

func (p *Provider) cdDiff() *action.Action {
	return p.cdAction(&action.Action{
		ID:      "argocd.diff",
		Summary: "Show what differs between Git and the cluster",
		Description: "Unified diff (live → desired) of each resource that is out of sync, as Argo CD computes it " +
			"(after its normalisation and ignoreDifferences). Secret values are redacted. Large diffs become artifacts.",
		Input:       cdSchema(appProps(nil), "app"),
		Positional:  []string{"app"},
		Annotations: action.Annotations{ReadOnly: true, Idempotent: true, OpenWorld: true},
	}, read("diff of"), func(ctx context.Context, i argo.Instance, in cdInput) (any, error) {
		var res struct {
			Items []argo.ResourceDiff `json:"items"`
		}
		if err := p.Sessions.CD(ctx, i, http.MethodGet, "/api/v1/applications/"+url.PathEscape(in.App)+"/managed-resources"+
			qs("appNamespace", in.AppNS), nil, &res); err != nil {
			return nil, err
		}
		out := DiffOutput{App: in.App, Diffs: []ObjectDiff{}}
		lim := p.limits(ctx)
		for _, d := range res.Items {
			if d.Hook {
				continue
			}
			live := d.NormalizedLiveState
			if live == "" {
				live = d.LiveState
			}
			want := d.PredictedLiveState
			if want == "" {
				want = d.TargetState
			}
			a := toYAML(redactSecretYAML(d.Kind, live))
			b := toYAML(redactSecretYAML(d.Kind, want))
			// Redacted Secrets can look the same while their values differ.
			// Argo CD already reports the Secret as out of sync, so saying
			// so reveals nothing more.
			hidden := a == b && toYAML(live) != toYAML(want)
			if a == b && !hidden {
				out.Same++
				continue
			}
			obj := d.Kind + " " + d.Name
			if d.Namespace != "" {
				obj = d.Kind + " " + d.Namespace + "/" + d.Name
			}
			change := "modified"
			switch {
			case a == "":
				change = "added"
			case b == "":
				change = "removed"
			}
			text := textdiff.Lines(obj+" (live)", obj+" (desired)", a, b)
			if hidden {
				text = "secret values differ (redacted)\n"
			}
			shaped, err := p.Artifacts.Shape(text, lim)
			if err != nil {
				return nil, err
			}
			out.Diffs = append(out.Diffs, ObjectDiff{Object: obj, Change: change, Diff: shaped})
		}
		return out, nil
	})
}

func qs(k, v string) string {
	if v == "" {
		return ""
	}
	return "?" + url.Values{k: {v}}.Encode()
}

// limits are the shaping limits for the caller: agents get small output.
func (p *Provider) limits(ctx context.Context) shaper.Limits {
	if a, ok := action.ActorFrom(ctx); ok && a.Kind == action.Human {
		return shaper.Limits{MaxBytes: 4 << 20, MaxLines: 1 << 30}
	}
	return shaper.Default
}

// EventView is one Kubernetes event of an app's resources.
type EventView struct {
	Time    *time.Time `json:"time,omitempty"`
	Type    string     `json:"type"`
	Reason  string     `json:"reason"`
	Object  string     `json:"object"`
	Message string     `json:"message"`
	Count   int        `json:"count,omitempty"`
}

func (p *Provider) events(ctx context.Context, i argo.Instance, in cdInput) ([]EventView, error) {
	q := url.Values{}
	if in.AppNS != "" {
		q.Set("appNamespace", in.AppNS)
	}
	if in.Name != "" {
		q.Set("resourceName", in.Name)
		q.Set("resourceNamespace", in.Namespace)
	}
	var list struct {
		Items []argo.Event `json:"items"`
	}
	if err := p.Sessions.CD(ctx, i, http.MethodGet, "/api/v1/applications/"+url.PathEscape(in.App)+"/events?"+q.Encode(), nil, &list); err != nil {
		return nil, err
	}
	sort.SliceStable(list.Items, func(a, b int) bool { return list.Items[a].When().After(list.Items[b].When()) })
	out := []EventView{}
	for _, e := range list.Items {
		t := e.When()
		v := EventView{Type: e.Type, Reason: e.Reason, Message: firstLine(e.Message), Count: e.Count,
			Object: e.InvolvedObject.Kind + "/" + e.InvolvedObject.Name}
		if !t.IsZero() {
			v.Time = &t
		}
		out = append(out, v)
	}
	return out, nil
}

func (p *Provider) cdEvents() *action.Action {
	return p.cdAction(&action.Action{
		ID:          "argocd.events",
		Summary:     "Show Kubernetes events of an application's resources",
		Description: "Events of the Application, or of one of its resources (kind is not needed: give name and namespace), newest first, up to 100.",
		Input: cdSchema(appProps(map[string]any{
			"name":      str("only this resource's events"),
			"namespace": str("the resource's namespace"),
		}), "app"),
		Positional:  []string{"app"},
		Annotations: action.Annotations{ReadOnly: true, Idempotent: true, OpenWorld: true},
	}, read("events of"), func(ctx context.Context, i argo.Instance, in cdInput) (any, error) {
		ev, err := p.events(ctx, i, in)
		if len(ev) > 100 {
			ev = ev[:100]
		}
		return ev, err
	})
}

// LogsOutput is the result of argocd.logs.
type LogsOutput struct {
	App  string        `json:"app"`
	Pod  string        `json:"pod,omitempty"`
	Logs shaper.Shaped `json:"logs"`
}

func (p *Provider) podLogs(ctx context.Context, i argo.Instance, in cdInput, tail int) (string, error) {
	q := url.Values{}
	q.Set("tailLines", fmt.Sprint(tail))
	q.Set("follow", "false")
	if in.Container != "" {
		q.Set("container", in.Container)
	}
	if in.Namespace != "" {
		q.Set("namespace", in.Namespace)
	}
	if in.Previous {
		q.Set("previous", "true")
	}
	if in.AppNS != "" {
		q.Set("appNamespace", in.AppNS)
	}
	path := "/api/v1/applications/" + url.PathEscape(in.App) + "/logs?"
	switch {
	case in.Pod != "":
		q.Set("podName", in.Pod)
	case in.Kind != "" && in.Name != "":
		q.Set("kind", in.Kind)
		q.Set("resourceName", in.Name)
	default:
		return "", errors.New("give pod, or kind and name (e.g. Deployment web): see argocd_tree")
	}
	var b strings.Builder
	err := p.Sessions.Stream(ctx, i, path+q.Encode(), 8<<20, func(raw json.RawMessage) bool {
		var e struct {
			Content string `json:"content"`
			PodName string `json:"podName"`
			Last    bool   `json:"last"`
		}
		json.Unmarshal(raw, &e)
		if e.Last {
			return false
		}
		if in.Pod == "" && e.PodName != "" {
			b.WriteString(e.PodName + " ")
		}
		b.WriteString(e.Content)
		b.WriteByte('\n')
		return true
	})
	return b.String(), err
}

func (p *Provider) cdLogs() *action.Action {
	return p.cdAction(&action.Action{
		ID:      "argocd.logs",
		Summary: "Show container logs of an application's Pod or workload",
		Description: "Logs through Argo CD (no cluster access needed): a pod, or a workload's pods (kind+name, lines prefixed " +
			"with the pod). previous gives the last crashed container's logs. Long output becomes an artifact.",
		Input: cdSchema(appProps(map[string]any{
			"pod":       str("a Pod's name (see argocd_tree)"),
			"kind":      str("or a workload's kind, e.g. Deployment"),
			"name":      str("the workload's name"),
			"namespace": str("the Pod's or workload's namespace"),
			"container": str("the container, if several"),
			"tail":      map[string]any{"type": "integer", "minimum": 1, "maximum": 5000, "description": "lines per container, default 200"},
			"previous":  map[string]any{"type": "boolean", "description": "the previous (crashed) container's logs"},
		}), "app"),
		Positional:  []string{"app"},
		Annotations: action.Annotations{ReadOnly: true, Idempotent: true, OpenWorld: true},
	}, read("logs of"), func(ctx context.Context, i argo.Instance, in cdInput) (any, error) {
		tail := in.Tail
		if tail <= 0 {
			tail = 200
		}
		text, err := p.podLogs(ctx, i, in, min(tail, 5000))
		if err != nil {
			return nil, err
		}
		shaped, err := p.Artifacts.Shape(text, p.limits(ctx))
		return LogsOutput{App: in.App, Pod: in.Pod, Logs: shaped}, err
	})
}

func (p *Provider) cdRefresh() *action.Action {
	return p.cdAction(&action.Action{
		ID:      "argocd.refresh",
		Summary: "Make Argo CD compare an application with Git again",
		Description: "Normal refresh re-compares with the cached manifests; hard also regenerates them from Git (e.g. after a " +
			"Helm chart or plugin change). Nothing is deployed.",
		Input: cdSchema(appProps(map[string]any{
			"hard": map[string]any{"type": "boolean", "description": "regenerate manifests from Git (slower)"},
		}), "app"),
		Positional:  []string{"app"},
		Annotations: action.Annotations{Idempotent: true, OpenWorld: true},
	}, func(i argo.Instance, in cdInput) (action.Assessment, error) {
		kind := "refresh"
		if in.Hard {
			kind = "hard refresh"
		}
		return action.Assessment{Risk: action.Write, Preview: fmt.Sprintf("app/%s %s on %s", in.App, kind, i.Name)}, needApp(in)
	}, func(ctx context.Context, i argo.Instance, in cdInput) (any, error) {
		mode := "normal"
		if in.Hard {
			mode = "hard"
		}
		a, err := p.getApp(ctx, i, in, mode)
		if err != nil {
			return nil, err
		}
		return summarize(a), nil
	})
}

// SyncOutput is the result of argocd.sync and argocd.rollback.
type SyncOutput struct {
	App     string `json:"app"`
	DryRun  bool   `json:"dry_run,omitempty"`
	Started bool   `json:"started"`
	Note    string `json:"note"`
}

func resourceList(list []string) ([]map[string]string, error) {
	var out []map[string]string
	for _, r := range list {
		// group:Kind:namespace/name, Kind:name or Kind:namespace/name
		parts := strings.Split(r, ":")
		var group, kind, rest string
		switch len(parts) {
		case 2:
			kind, rest = parts[0], parts[1]
		case 3:
			group, kind, rest = parts[0], parts[1], parts[2]
		default:
			return nil, fmt.Errorf("resource %q: use Kind:name, Kind:namespace/name or group:Kind:namespace/name", r)
		}
		ns, name, ok := strings.Cut(rest, "/")
		if !ok {
			ns, name = "", rest
		}
		if kind == "" || name == "" {
			return nil, fmt.Errorf("resource %q: kind and name are required", r)
		}
		out = append(out, map[string]string{"group": group, "kind": kind, "namespace": ns, "name": name})
	}
	return out, nil
}

func syncRisk(in cdInput) action.Risk {
	switch {
	case in.DryRun:
		return action.Read
	case in.Prune:
		return action.Destructive
	}
	return action.Write
}

func (p *Provider) cdSync() *action.Action {
	return p.cdAction(&action.Action{
		ID:      "argocd.sync",
		Summary: "Sync an Argo CD application (deploy what is in Git)",
		Description: "Starts a sync and returns; follow it with argocd_app. dry_run first to see what would change. " +
			"prune deletes resources no longer in Git (destructive). resources limits the sync (Kind:namespace/name). " +
			"Refused while another operation runs (argocd_terminate stops it).",
		Input: cdSchema(appProps(map[string]any{
			"revision":  str("Git revision to sync to; default the app's target revision"),
			"prune":     map[string]any{"type": "boolean", "description": "delete resources that are no longer in Git"},
			"dry_run":   map[string]any{"type": "boolean", "description": "only check what would change"},
			"resources": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "only these, as Kind:namespace/name or group:Kind:namespace/name"},
		}), "app"),
		Positional:  []string{"app"},
		Annotations: action.Annotations{Destructive: true, OpenWorld: true},
	}, func(i argo.Instance, in cdInput) (action.Assessment, error) {
		if err := needApp(in); err != nil {
			return action.Assessment{}, err
		}
		if _, err := resourceList(in.Resources); err != nil {
			return action.Assessment{}, err
		}
		pv := fmt.Sprintf("app/%s sync", in.App)
		if in.Prune {
			pv += " prune=true"
		}
		if in.DryRun {
			pv += " dry_run=true"
		}
		if in.Revision != "" {
			pv += " revision=" + in.Revision
		}
		if len(in.Resources) > 0 {
			pv += " resources=" + strings.Join(in.Resources, ",")
		}
		return action.Assessment{Risk: syncRisk(in), Preview: pv + " on " + i.Name}, nil
	}, func(ctx context.Context, i argo.Instance, in cdInput) (any, error) {
		a, err := p.getApp(ctx, i, in, "")
		if err != nil {
			return nil, err
		}
		if a.Operation != nil {
			return nil, fmt.Errorf("app %s already has an operation running; wait for it (argocd_app) or stop it (argocd_terminate)", in.App)
		}
		res, _ := resourceList(in.Resources)
		body := map[string]any{"name": in.App, "prune": in.Prune, "dryRun": in.DryRun}
		if in.Revision != "" {
			body["revision"] = in.Revision
		}
		if len(res) > 0 {
			body["resources"] = res
		}
		if in.AppNS != "" {
			body["appNamespace"] = in.AppNS
		}
		if err := p.Sessions.CD(ctx, i, http.MethodPost, appPath(in, "/sync"), body, nil); err != nil {
			return nil, err
		}
		note := "sync started; argocd_app shows its progress and result"
		if in.DryRun {
			note = "dry-run sync started; argocd_app's last_operation shows what would change"
		}
		return SyncOutput{App: in.App, DryRun: in.DryRun, Started: true, Note: note}, nil
	})
}

func (p *Provider) cdRollback() *action.Action {
	return p.cdAction(&action.Action{
		ID:      "argocd.rollback",
		Summary: "Roll an application back to an earlier deployment",
		Description: "Redeploys deployment id (from argocd_app's history). Argo CD refuses while auto-sync is on, which would " +
			"undo it: tell the user to disable auto-sync first. The app stays OutOfSync with Git afterwards.",
		Input: cdSchema(appProps(map[string]any{
			"id":      map[string]any{"type": "integer", "description": "the history id to roll back to"},
			"prune":   map[string]any{"type": "boolean", "description": "delete resources not in that deployment"},
			"dry_run": map[string]any{"type": "boolean", "description": "only check"},
		}), "app", "id"),
		Positional:  []string{"app", "id"},
		Annotations: action.Annotations{Destructive: true, OpenWorld: true},
	}, func(i argo.Instance, in cdInput) (action.Assessment, error) {
		if err := needApp(in); err != nil {
			return action.Assessment{}, err
		}
		risk := action.Destructive
		if in.DryRun {
			risk = action.Read
		}
		return action.Assessment{Risk: risk, Preview: fmt.Sprintf("app/%s rollback to %d on %s", in.App, in.ID, i.Name)}, nil
	}, func(ctx context.Context, i argo.Instance, in cdInput) (any, error) {
		a, err := p.getApp(ctx, i, in, "")
		if err != nil {
			return nil, err
		}
		if a.Spec.SyncPolicy != nil && a.Spec.SyncPolicy.Automated != nil {
			return nil, fmt.Errorf("app %s has auto-sync on, which would undo a rollback; ask the user to disable it first", in.App)
		}
		found := false
		for _, h := range a.Status.History {
			found = found || h.ID == in.ID
		}
		if !found {
			return nil, fmt.Errorf("app %s has no deployment with id %d (see argocd_app history)", in.App, in.ID)
		}
		body := map[string]any{"name": in.App, "id": in.ID, "prune": in.Prune, "dryRun": in.DryRun}
		if in.AppNS != "" {
			body["appNamespace"] = in.AppNS
		}
		if err := p.Sessions.CD(ctx, i, http.MethodPost, appPath(in, "/rollback"), body, nil); err != nil {
			return nil, err
		}
		return SyncOutput{App: in.App, DryRun: in.DryRun, Started: true, Note: "rollback started; argocd_app shows its progress"}, nil
	})
}

func (p *Provider) cdTerminate() *action.Action {
	return p.cdAction(&action.Action{
		ID:          "argocd.terminate",
		Summary:     "Stop an application's running sync",
		Description: "Terminates the operation in progress; resources already applied stay as they are.",
		Input:       cdSchema(appProps(nil), "app"),
		Positional:  []string{"app"},
		Annotations: action.Annotations{Destructive: true, Idempotent: true, OpenWorld: true},
	}, func(i argo.Instance, in cdInput) (action.Assessment, error) {
		return action.Assessment{Risk: action.Destructive, Preview: fmt.Sprintf("app/%s terminate operation on %s", in.App, i.Name)}, needApp(in)
	}, func(ctx context.Context, i argo.Instance, in cdInput) (any, error) {
		err := p.Sessions.CD(ctx, i, http.MethodDelete, appPath(in, "/operation"), nil, nil)
		var ae *argo.APIError
		if errors.As(err, &ae) && strings.Contains(ae.Message, "Unable to terminate operation. No operation is in progress") {
			return map[string]any{"app": in.App, "terminated": false, "note": "no operation was running"}, nil
		}
		if err != nil {
			return nil, err
		}
		return map[string]any{"app": in.App, "terminated": true}, nil
	})
}

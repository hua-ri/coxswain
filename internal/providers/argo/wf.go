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
)

func (p *Provider) registerWF(r *action.Registry) {
	r.Register(p.wfList())
	r.Register(p.wfGet())
	r.Register(p.wfLogs())
	r.Register(p.wfTemplates())
	r.Register(p.wfSubmit())
	for _, op := range wfOps {
		r.Register(p.wfOp(op))
	}
	r.Register(p.wfDelete())
	r.Register(p.wfCron())
	r.Register(p.wfDiagnose())
}

// wfInput is what the argowf.* actions take.
type wfInput struct {
	Instance          string            `json:"instance"`
	Namespace         string            `json:"namespace"`
	Name              string            `json:"name"`
	Phase             string            `json:"phase"`
	Selector          string            `json:"label_selector"`
	Limit             int               `json:"limit"`
	Node              string            `json:"node"`
	Container         string            `json:"container"`
	Tail              int               `json:"tail"`
	Grep              string            `json:"grep"`
	Template          string            `json:"template"`
	ClusterScope      bool              `json:"cluster_scope"`
	Parameters        map[string]string `json:"parameters"`
	GenerateName      string            `json:"generate_name"`
	DryRun            bool              `json:"dry_run"`
	RestartSuccessful bool              `json:"restart_successful"`
	NodeSelector      string            `json:"node_field_selector"`
	Memoized          bool              `json:"memoized"`
	Suspend           bool              `json:"suspend"`
}

var (
	nsProperty   = str("namespace; default the instance's")
	wfNameProp   = str("the Workflow's name")
	cronNameProp = str("the CronWorkflow's name")
)

func wfSchema(extra map[string]any, required ...string) map[string]any {
	props := map[string]any{"instance": instanceProperty, "namespace": nsProperty}
	for k, v := range extra {
		props[k] = v
	}
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

// wfAction builds an argowf.* action: arguments checked exactly, the
// instance and namespace resolved, the targets set.
func (p *Provider) wfAction(a *action.Action, label func(argo.Instance, wfInput) (action.Assessment, error),
	run func(context.Context, argo.Instance, wfInput) (any, error)) *action.Action {
	parse := func(raw json.RawMessage) (argo.Instance, wfInput, error) {
		var in wfInput
		if err := decode(raw, a.Input, &in); err != nil {
			return argo.Instance{}, in, err
		}
		i, err := p.instance(argo.Workflows, in.Instance)
		if in.Namespace == "" {
			in.Namespace = i.Namespace
		}
		return i, in, err
	}
	a.Label = func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
		i, in, err := parse(raw)
		if err != nil {
			return action.Assessment{}, err
		}
		as, err := label(i, in)
		if len(as.Targets) == 0 {
			sub := in.Namespace
			if in.Name != "" {
				sub += "/" + in.Name
			}
			as.Targets = []action.Target{target(i, sub)}
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

func wfRead(preview string) func(argo.Instance, wfInput) (action.Assessment, error) {
	return func(i argo.Instance, in wfInput) (action.Assessment, error) {
		p := preview
		if in.Name != "" {
			p += " " + in.Name
		}
		return action.Assessment{Risk: action.Read, Preview: p + " in " + in.Namespace + " on " + i.Name}, nil
	}
}

func wfPath(in wfInput, sub string) string {
	return "/api/v1/workflows/" + url.PathEscape(in.Namespace) + "/" + url.PathEscape(in.Name) + sub
}

func needName(in wfInput, what string) error {
	if in.Name == "" {
		return fmt.Errorf("name is required (the %s)", what)
	}
	return nil
}

// WFSummary is one Workflow in a list.
type WFSummary struct {
	Name       string     `json:"name"`
	Phase      string     `json:"phase"`
	Progress   string     `json:"progress,omitempty"`
	Template   string     `json:"template,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Duration   string     `json:"duration,omitempty"`
	Message    string     `json:"message,omitempty"`
	Suspended  bool       `json:"suspended,omitempty"`
}

func (p *Provider) wfSummary(w argo.Workflow) WFSummary {
	s := WFSummary{Name: w.Metadata.Name, Phase: w.Status.Phase, Progress: w.Status.Progress,
		StartedAt: w.Status.StartedAt, FinishedAt: w.Status.FinishedAt, Message: firstLine(w.Status.Message)}
	if s.Phase == "" {
		s.Phase = "Pending"
	}
	if ref := w.Spec.WorkflowTemplateRef; ref != nil {
		s.Template = ref.Name
	}
	s.Suspended = w.Spec.Suspend != nil && *w.Spec.Suspend
	if w.Status.StartedAt != nil {
		end := p.now()
		if w.Status.FinishedAt != nil {
			end = *w.Status.FinishedAt
		}
		s.Duration = end.Sub(*w.Status.StartedAt).Round(time.Second).String()
	}
	return s
}

// WFListOutput is the result of argowf.list.
type WFListOutput struct {
	Instance     string         `json:"instance"`
	InstanceNote string         `json:"instance_note,omitempty"`
	Namespace    string         `json:"namespace"`
	Workflows    []WFSummary    `json:"workflows"`
	Counts       map[string]int `json:"counts"`
	More         bool           `json:"more,omitempty"`
}

// wfListFields keeps list answers small: no node trees.
const wfListFields = "metadata,items.metadata.name,items.metadata.namespace,items.metadata.creationTimestamp," +
	"items.spec.suspend,items.spec.workflowTemplateRef,items.status.phase,items.status.progress," +
	"items.status.startedAt,items.status.finishedAt,items.status.message"

func (p *Provider) wfList() *action.Action {
	return p.wfAction(&action.Action{
		ID:      "argowf.list",
		Summary: "List Argo workflows with phase, progress and duration",
		Description: "Newest first. phase filters (Running, Succeeded, Failed, Error, Pending). Respect the instance's note. " +
			"argowf_diagnose explains a failed one.",
		Input: wfSchema(map[string]any{
			"phase":          map[string]any{"type": "string", "enum": []string{"Pending", "Running", "Succeeded", "Failed", "Error"}},
			"label_selector": str("e.g. workflows.argoproj.io/workflow-template=build"),
			"limit":          map[string]any{"type": "integer", "minimum": 1, "maximum": 500, "description": "default 50"},
		}),
		Positional:  []string{"instance"},
		Annotations: action.Annotations{ReadOnly: true, Idempotent: true, OpenWorld: true},
	}, wfRead("list workflows"), func(ctx context.Context, i argo.Instance, in wfInput) (any, error) {
		limit := in.Limit
		if limit <= 0 {
			limit = 50
		}
		q := url.Values{}
		sel := in.Selector
		if in.Phase != "" {
			if sel != "" {
				sel += ","
			}
			sel += "workflows.argoproj.io/phase=" + in.Phase
		}
		if sel != "" {
			q.Set("listOptions.labelSelector", sel)
		}
		q.Set("fields", wfListFields)
		var list struct {
			Items []argo.Workflow `json:"items"`
		}
		if err := p.Sessions.WF(ctx, i, http.MethodGet, "/api/v1/workflows/"+url.PathEscape(in.Namespace)+"?"+q.Encode(), nil, &list); err != nil {
			return nil, err
		}
		sort.SliceStable(list.Items, func(a, b int) bool {
			return created(list.Items[a]).After(created(list.Items[b]))
		})
		out := WFListOutput{Instance: i.Name, InstanceNote: i.Note, Namespace: in.Namespace, Workflows: []WFSummary{}, Counts: map[string]int{}}
		for _, w := range list.Items {
			s := p.wfSummary(w)
			out.Counts[s.Phase]++
			if len(out.Workflows) < limit {
				out.Workflows = append(out.Workflows, s)
			} else {
				out.More = true
			}
		}
		return out, nil
	})
}

func created(w argo.Workflow) time.Time {
	if w.Status.StartedAt != nil {
		return *w.Status.StartedAt
	}
	if w.Metadata.CreationTimestamp != nil {
		return *w.Metadata.CreationTimestamp
	}
	return time.Time{}
}

// NodeView is one step of a workflow.
type NodeView struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Depth    int    `json:"depth"`
	Type     string `json:"type"`
	Template string `json:"template,omitempty"`
	Phase    string `json:"phase,omitempty"`
	Message  string `json:"message,omitempty"`
	ExitCode string `json:"exit_code,omitempty"`
	Pod      string `json:"pod,omitempty"`
	Duration string `json:"duration,omitempty"`
}

// WFDetail is the result of argowf.get.
type WFDetail struct {
	WFSummary
	Namespace    string            `json:"namespace"`
	InstanceNote string            `json:"instance_note,omitempty"`
	Parameters   map[string]string `json:"parameters,omitempty"`
	Failed       []NodeView        `json:"failed_nodes,omitempty"`
	Nodes        []NodeView        `json:"nodes"`
	NodesOmitted int               `json:"nodes_omitted,omitempty"`
}

func (p *Provider) getWF(ctx context.Context, i argo.Instance, in wfInput) (argo.Workflow, error) {
	var w argo.Workflow
	err := p.Sessions.WF(ctx, i, http.MethodGet, wfPath(in, ""), nil, &w)
	return w, err
}

// nodes walks the node tree from the root in order, with depths.
func (p *Provider) nodes(w argo.Workflow) []NodeView {
	byID := w.Status.Nodes
	view := func(n argo.WFNode, depth int) NodeView {
		v := NodeView{ID: n.ID, Name: n.DisplayName, Depth: depth, Type: n.Type, Template: n.TemplateName,
			Phase: n.Phase, Message: firstLine(n.Message), Pod: w.PodName(n)}
		if v.Template == "" && n.TemplateRef != nil {
			v.Template = n.TemplateRef.Template
		}
		if n.Outputs != nil && n.Outputs.ExitCode != nil {
			v.ExitCode = *n.Outputs.ExitCode
		}
		if n.StartedAt != nil && n.FinishedAt != nil {
			v.Duration = n.FinishedAt.Sub(*n.StartedAt).Round(time.Second).String()
		}
		return v
	}
	var out []NodeView
	seen := map[string]bool{}
	var walk func(id string, depth int)
	walk = func(id string, depth int) {
		n, ok := byID[id]
		if !ok || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, view(n, depth))
		for _, c := range n.Children {
			walk(c, depth+1)
		}
	}
	root := w.Metadata.Name
	if _, ok := byID[root]; !ok {
		for id, n := range byID {
			if n.Name == w.Metadata.Name {
				root = id
			}
		}
	}
	walk(root, 0)
	// Nodes not reached from the root (onExit handlers, …), in name order.
	var rest []string
	for id := range byID {
		if !seen[id] {
			rest = append(rest, id)
		}
	}
	sort.Strings(rest)
	for _, id := range rest {
		walk(id, 0)
	}
	return out
}

func failed(phase string) bool { return phase == "Failed" || phase == "Error" }

func (p *Provider) wfGet() *action.Action {
	return p.wfAction(&action.Action{
		ID:      "argowf.get",
		Summary: "Show an Argo workflow's steps",
		Description: "Phase, parameters, and the node tree (depth-indented: steps, DAG tasks, pods with exit codes); failed " +
			"nodes are also listed first. argowf_logs reads a node's logs.",
		Input:       wfSchema(map[string]any{"name": wfNameProp}, "name"),
		Positional:  []string{"name"},
		Annotations: action.Annotations{ReadOnly: true, Idempotent: true, OpenWorld: true},
	}, wfRead("show workflow"), func(ctx context.Context, i argo.Instance, in wfInput) (any, error) {
		w, err := p.getWF(ctx, i, in)
		if err != nil {
			return nil, err
		}
		d := WFDetail{WFSummary: p.wfSummary(w), Namespace: w.Metadata.Namespace, InstanceNote: i.Note, Nodes: []NodeView{}}
		for _, prm := range w.Spec.Arguments.Parameters {
			if d.Parameters == nil {
				d.Parameters = map[string]string{}
			}
			if prm.Value != nil {
				d.Parameters[prm.Name] = *prm.Value
			}
		}
		all := p.nodes(w)
		for _, n := range all {
			if failed(n.Phase) && n.Type == "Pod" {
				d.Failed = append(d.Failed, n)
			}
		}
		max := 200
		if a, ok := action.ActorFrom(ctx); ok && a.Kind == action.Human {
			max = 5000
		}
		if len(all) > max {
			d.NodesOmitted = len(all) - max
			all = all[:max]
		}
		d.Nodes = append(d.Nodes, all...)
		return d, nil
	})
}

// node finds a node by id, display name or pod name.
func (p *Provider) node(w argo.Workflow, key string) (argo.WFNode, error) {
	var byName []argo.WFNode
	for id, n := range w.Status.Nodes {
		if id == key || w.PodName(n) == key {
			return n, nil
		}
		if n.DisplayName == key || n.Name == key {
			byName = append(byName, n)
		}
	}
	var pods []argo.WFNode
	for _, n := range byName {
		if n.Type == "Pod" {
			pods = append(pods, n)
		}
	}
	switch {
	case len(pods) == 1:
		return pods[0], nil
	case len(pods) > 1:
		return argo.WFNode{}, fmt.Errorf("several steps are named %q; give the node id (see argowf_get)", key)
	case len(byName) > 0:
		return argo.WFNode{}, fmt.Errorf("%q is a %s node, not a pod; give one of its pod steps (see argowf_get)", key, byName[0].Type)
	}
	return argo.WFNode{}, fmt.Errorf("workflow %s has no node %q (see argowf_get)", w.Metadata.Name, key)
}

func (p *Provider) nodeLogs(ctx context.Context, i argo.Instance, w argo.Workflow, n argo.WFNode, container string, tail int, grep string) (string, error) {
	in := wfInput{Namespace: w.Metadata.Namespace, Name: w.Metadata.Name}
	q := url.Values{}
	if container == "" {
		container = "main"
	}
	q.Set("logOptions.container", container)
	q.Set("logOptions.tailLines", fmt.Sprint(tail))
	if grep != "" {
		q.Set("grep", grep)
	}
	pod := w.PodName(n)
	path := wfPath(in, "/"+url.PathEscape(pod)+"/log?"+q.Encode())
	var b strings.Builder
	err := p.Sessions.Stream(ctx, i, path, 8<<20, func(raw json.RawMessage) bool {
		var e struct {
			Content string `json:"content"`
		}
		json.Unmarshal(raw, &e)
		b.WriteString(e.Content)
		b.WriteByte('\n')
		return true
	})
	return b.String(), err
}

// WFLogsOutput is the result of argowf.logs.
type WFLogsOutput struct {
	Workflow string        `json:"workflow"`
	Node     string        `json:"node"`
	Pod      string        `json:"pod"`
	Logs     shaper.Shaped `json:"logs"`
}

func (p *Provider) wfLogs() *action.Action {
	return p.wfAction(&action.Action{
		ID:      "argowf.logs",
		Summary: "Show the logs of a workflow step",
		Description: "Logs of one pod step (node id, step name or pod name from argowf_get), container main by default. " +
			"Logs of finished pods remain only while the pods exist (or in the artifact repository, which this does not read).",
		Input: wfSchema(map[string]any{
			"name":      wfNameProp,
			"node":      str("the step: node id, display name or pod name"),
			"container": str("default main (init and wait are Argo's own)"),
			"tail":      map[string]any{"type": "integer", "minimum": 1, "maximum": 5000, "description": "default 200"},
			"grep":      str("only lines containing this"),
		}, "name", "node"),
		Positional:  []string{"name", "node"},
		Annotations: action.Annotations{ReadOnly: true, Idempotent: true, OpenWorld: true},
	}, wfRead("logs of workflow"), func(ctx context.Context, i argo.Instance, in wfInput) (any, error) {
		w, err := p.getWF(ctx, i, in)
		if err != nil {
			return nil, err
		}
		n, err := p.node(w, in.Node)
		if err != nil {
			return nil, err
		}
		tail := in.Tail
		if tail <= 0 {
			tail = 200
		}
		text, err := p.nodeLogs(ctx, i, w, n, in.Container, min(tail, 5000), in.Grep)
		if err != nil {
			return nil, err
		}
		shaped, err := p.Artifacts.Shape(text, p.limits(ctx))
		return WFLogsOutput{Workflow: w.Metadata.Name, Node: n.DisplayName, Pod: w.PodName(n), Logs: shaped}, err
	})
}

// TemplateView is a template and the parameters it takes.
type TemplateView struct {
	Kind        string           `json:"kind"` // WorkflowTemplate, ClusterWorkflowTemplate, CronWorkflow
	Name        string           `json:"name"`
	Entrypoint  string           `json:"entrypoint,omitempty"`
	Description string           `json:"description,omitempty"`
	Parameters  []argo.Parameter `json:"parameters,omitempty"`
	Schedule    string           `json:"schedule,omitempty"`
	Suspended   bool             `json:"suspended,omitempty"`
	LastRun     *time.Time       `json:"last_run,omitempty"`
	Active      int              `json:"active,omitempty"`
}

func templateView(kind string, t argo.WorkflowTemplate) TemplateView {
	return TemplateView{Kind: kind, Name: t.Metadata.Name, Entrypoint: t.Spec.Entrypoint,
		Description: t.Metadata.Annotations["workflows.argoproj.io/description"], Parameters: t.Spec.Arguments.Parameters}
}

func (p *Provider) wfTemplates() *action.Action {
	return p.wfAction(&action.Action{
		ID:      "argowf.templates",
		Summary: "List workflow templates, cluster templates and cron workflows",
		Description: "WorkflowTemplates of the namespace, ClusterWorkflowTemplates, and CronWorkflows (schedule, suspended, last run), " +
			"with the parameters each takes (default, enum). argowf_submit runs a template.",
		Input:       wfSchema(nil),
		Positional:  []string{"instance"},
		Annotations: action.Annotations{ReadOnly: true, Idempotent: true, OpenWorld: true},
	}, wfRead("list templates"), func(ctx context.Context, i argo.Instance, in wfInput) (any, error) {
		out := []TemplateView{}
		var wt struct {
			Items []argo.WorkflowTemplate `json:"items"`
		}
		if err := p.Sessions.WF(ctx, i, http.MethodGet, "/api/v1/workflow-templates/"+url.PathEscape(in.Namespace), nil, &wt); err != nil {
			return nil, err
		}
		for _, t := range wt.Items {
			out = append(out, templateView("WorkflowTemplate", t))
		}
		var problems []string
		var ct struct {
			Items []argo.WorkflowTemplate `json:"items"`
		}
		if err := p.Sessions.WF(ctx, i, http.MethodGet, "/api/v1/cluster-workflow-templates", nil, &ct); err != nil {
			problems = append(problems, "cluster workflow templates: "+err.Error()) // often not permitted
		}
		for _, t := range ct.Items {
			out = append(out, templateView("ClusterWorkflowTemplate", t))
		}
		var cw struct {
			Items []argo.CronWorkflow `json:"items"`
		}
		if err := p.Sessions.WF(ctx, i, http.MethodGet, "/api/v1/cron-workflows/"+url.PathEscape(in.Namespace), nil, &cw); err != nil {
			problems = append(problems, "cron workflows: "+err.Error())
		}
		for _, c := range cw.Items {
			sched := c.Spec.Schedule
			if len(c.Spec.Schedules) > 0 {
				sched = strings.Join(c.Spec.Schedules, "; ")
			}
			if c.Spec.Timezone != "" {
				sched += " (" + c.Spec.Timezone + ")"
			}
			out = append(out, TemplateView{Kind: "CronWorkflow", Name: c.Metadata.Name, Schedule: sched, Suspended: c.Spec.Suspend,
				LastRun: c.Status.LastScheduledTime, Active: len(c.Status.Active)})
		}
		return map[string]any{"namespace": in.Namespace, "templates": out, "problems": problems}, nil
	})
}

// checkParameters checks submitted parameters against a template's:
// known names, enum values, and required ones (no default) given.
func checkParameters(tmpl []argo.Parameter, given map[string]string) ([]string, error) {
	known := map[string]argo.Parameter{}
	for _, p := range tmpl {
		known[p.Name] = p
	}
	var out, missing []string
	for k, v := range given {
		p, ok := known[k]
		if !ok {
			names := make([]string, 0, len(known))
			for n := range known {
				names = append(names, n)
			}
			sort.Strings(names)
			return nil, fmt.Errorf("the template has no parameter %q (it takes: %s)", k, strings.Join(names, ", "))
		}
		if len(p.Enum) > 0 && !contains(p.Enum, v) {
			return nil, fmt.Errorf("parameter %s must be one of %s", k, strings.Join(p.Enum, ", "))
		}
		out = append(out, k+"="+v)
	}
	for _, p := range tmpl {
		if _, ok := given[p.Name]; !ok && p.Value == nil && p.Default == nil {
			missing = append(missing, p.Name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("parameters without a default must be given: %s", strings.Join(missing, ", "))
	}
	sort.Strings(out)
	return out, nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func (p *Provider) template(ctx context.Context, i argo.Instance, in wfInput) (argo.WorkflowTemplate, string, error) {
	var t argo.WorkflowTemplate
	kind := "WorkflowTemplate"
	path := "/api/v1/workflow-templates/" + url.PathEscape(in.Namespace) + "/" + url.PathEscape(in.Template)
	if in.ClusterScope {
		kind = "ClusterWorkflowTemplate"
		path = "/api/v1/cluster-workflow-templates/" + url.PathEscape(in.Template)
	}
	err := p.Sessions.WF(ctx, i, http.MethodGet, path, nil, &t)
	return t, kind, err
}

func (p *Provider) wfSubmit() *action.Action {
	return p.wfAction(&action.Action{
		ID:      "argowf.submit",
		Summary: "Run a workflow from a template",
		Description: "Submits a WorkflowTemplate (or ClusterWorkflowTemplate with cluster_scope) with parameters, after checking " +
			"them against the template (names, enums, required). dry_run asks the server to validate without creating it. " +
			"Returns the new workflow's name; follow it with argowf_get.",
		Input: wfSchema(map[string]any{
			"template":      str("the WorkflowTemplate's name (see argowf_templates)"),
			"cluster_scope": map[string]any{"type": "boolean", "description": "template is a ClusterWorkflowTemplate"},
			"parameters":    map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}, "description": "name → value"},
			"generate_name": str("name prefix for the new workflow; default <template>-"),
			"dry_run":       map[string]any{"type": "boolean", "description": "validate on the server, create nothing"},
		}, "template"),
		Positional:  []string{"template"},
		Annotations: action.Annotations{OpenWorld: true},
	}, func(i argo.Instance, in wfInput) (action.Assessment, error) {
		if in.Template == "" {
			return action.Assessment{}, errors.New("template is required (see argowf_templates)")
		}
		risk := action.Write
		if in.DryRun {
			risk = action.Read
		}
		var params []string
		for k, v := range in.Parameters {
			params = append(params, k+"="+v)
		}
		sort.Strings(params)
		pv := "submit " + in.Template
		if len(params) > 0 {
			pv += " " + strings.Join(params, " ")
		}
		if in.DryRun {
			pv += " dry_run=true"
		}
		return action.Assessment{Risk: risk, Targets: []action.Target{target(i, in.Namespace)}, Preview: pv + " in " + in.Namespace + " on " + i.Name}, nil
	}, func(ctx context.Context, i argo.Instance, in wfInput) (any, error) {
		t, kind, err := p.template(ctx, i, in)
		if err != nil {
			return nil, err
		}
		params, err := checkParameters(t.Spec.Arguments.Parameters, in.Parameters)
		if err != nil {
			return nil, err
		}
		gen := in.GenerateName
		if gen == "" {
			gen = in.Template + "-"
		}
		opts := map[string]any{"generateName": gen, "parameters": params}
		if in.DryRun {
			opts["serverDryRun"] = true
		}
		body := map[string]any{"namespace": in.Namespace, "resourceKind": kind, "resourceName": in.Template, "submitOptions": opts}
		var w argo.Workflow
		if err := p.Sessions.WF(ctx, i, http.MethodPost, "/api/v1/workflows/"+url.PathEscape(in.Namespace)+"/submit", body, &w); err != nil {
			return nil, err
		}
		return map[string]any{"workflow": w.Metadata.Name, "namespace": in.Namespace, "dry_run": in.DryRun, "parameters": params}, nil
	})
}

type wfOpSpec struct {
	op, summary, desc string
	risk              action.Risk
	destructive       bool
}

var wfOps = []wfOpSpec{
	{"suspend", "Pause a running workflow", "No new steps start until argowf_resume; running pods continue.", action.Write, false},
	{"resume", "Resume a suspended workflow", "Also resumes a workflow waiting at a suspend step.", action.Write, false},
	{"stop", "Stop a workflow, running its exit handlers", "Running steps are stopped, then onExit handlers run. Destructive: the run cannot be continued.", action.Destructive, true},
	{"terminate", "Terminate a workflow immediately, without exit handlers", "Kills running steps; onExit handlers do not run. Prefer argowf_stop.", action.Destructive, true},
	{"retry", "Retry a failed workflow from its failed steps", "Reruns failed and errored steps (restart_successful also reruns successful ones matching node_field_selector). Diagnose first: retrying a deterministic failure fails again.", action.Write, false},
	{"resubmit", "Run a finished workflow again as a new one", "Creates a new workflow with the same spec and parameters (memoized reuses memoized step results).", action.Write, false},
}

func (p *Provider) wfOp(o wfOpSpec) *action.Action {
	extra := map[string]any{"name": wfNameProp}
	switch o.op {
	case "retry":
		extra["restart_successful"] = map[string]any{"type": "boolean", "description": "also rerun successful steps matching node_field_selector"}
		extra["node_field_selector"] = str("e.g. templateName=build")
	case "resubmit":
		extra["memoized"] = map[string]any{"type": "boolean", "description": "reuse memoized results"}
	}
	return p.wfAction(&action.Action{
		ID:          "argowf." + o.op,
		Summary:     o.summary,
		Description: o.desc,
		Input:       wfSchema(extra, "name"),
		Positional:  []string{"name"},
		Annotations: action.Annotations{Destructive: o.destructive, OpenWorld: true},
	}, func(i argo.Instance, in wfInput) (action.Assessment, error) {
		return action.Assessment{Risk: o.risk, Preview: fmt.Sprintf("wf/%s %s in %s on %s", in.Name, o.op, in.Namespace, i.Name)}, needName(in, "Workflow")
	}, func(ctx context.Context, i argo.Instance, in wfInput) (any, error) {
		body := map[string]any{"name": in.Name, "namespace": in.Namespace}
		switch o.op {
		case "retry":
			body["restartSuccessful"] = in.RestartSuccessful
			if in.NodeSelector != "" {
				body["nodeFieldSelector"] = in.NodeSelector
			}
		case "resubmit":
			body["memoized"] = in.Memoized
		}
		var w argo.Workflow
		if err := p.Sessions.WF(ctx, i, http.MethodPut, wfPath(in, "/"+o.op), body, &w); err != nil {
			return nil, err
		}
		return p.wfSummary(w), nil
	})
}

func (p *Provider) wfDelete() *action.Action {
	return p.wfAction(&action.Action{
		ID:          "argowf.delete",
		Summary:     "Delete a workflow and its pods",
		Description: "Deletes the Workflow object (and so its pods and their logs). Archived copies, if any, stay.",
		Input:       wfSchema(map[string]any{"name": wfNameProp}, "name"),
		Positional:  []string{"name"},
		Annotations: action.Annotations{Destructive: true, Idempotent: true, OpenWorld: true},
	}, func(i argo.Instance, in wfInput) (action.Assessment, error) {
		return action.Assessment{Risk: action.Destructive, Preview: fmt.Sprintf("wf/%s delete in %s on %s", in.Name, in.Namespace, i.Name)}, needName(in, "Workflow")
	}, func(ctx context.Context, i argo.Instance, in wfInput) (any, error) {
		return map[string]any{"deleted": in.Name}, p.Sessions.WF(ctx, i, http.MethodDelete, wfPath(in, ""), nil, nil)
	})
}

func (p *Provider) wfCron() *action.Action {
	return p.wfAction(&action.Action{
		ID:          "argowf.cron",
		Summary:     "Suspend or resume a cron workflow",
		Description: "suspend=true stops scheduling new runs (running ones continue); false resumes it.",
		Input: wfSchema(map[string]any{
			"name":    cronNameProp,
			"suspend": map[string]any{"type": "boolean", "description": "true to suspend, false to resume"},
		}, "name", "suspend"),
		Positional:  []string{"name"},
		Annotations: action.Annotations{Idempotent: true, OpenWorld: true},
	}, func(i argo.Instance, in wfInput) (action.Assessment, error) {
		op := "resume"
		if in.Suspend {
			op = "suspend"
		}
		return action.Assessment{Risk: action.Write, Preview: fmt.Sprintf("cron/%s %s in %s on %s", in.Name, op, in.Namespace, i.Name)},
			needName(in, "CronWorkflow")
	}, func(ctx context.Context, i argo.Instance, in wfInput) (any, error) {
		op := "resume"
		if in.Suspend {
			op = "suspend"
		}
		path := "/api/v1/cron-workflows/" + url.PathEscape(in.Namespace) + "/" + url.PathEscape(in.Name) + "/" + op
		var c argo.CronWorkflow
		if err := p.Sessions.WF(ctx, i, http.MethodPut, path, map[string]any{"name": in.Name, "namespace": in.Namespace}, &c); err != nil {
			return nil, err
		}
		return map[string]any{"cron_workflow": c.Metadata.Name, "suspended": c.Spec.Suspend}, nil
	})
}

package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	kube "github.com/hua-ri/coxswain/internal/k8s"
	"github.com/hua-ri/coxswain/internal/shaper"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/yaml"
)

var writeRemote = action.Annotations{Destructive: true, Idempotent: true, OpenWorld: true}

// ApplyObject is one object of an apply.
type ApplyObject struct {
	Kind      string         `json:"kind"`
	Namespace string         `json:"namespace,omitempty"`
	Name      string         `json:"name"`
	Operation string         `json:"operation"` // create, configure or unchanged
	Diff      *shaper.Shaped `json:"diff,omitempty"`
	Applied   bool           `json:"applied,omitempty"`
}

// ApplyOutput is the result of k8s.apply.
type ApplyOutput struct {
	Context     string            `json:"context"`
	ContextNote string            `json:"context_note,omitempty"`
	ContextTags map[string]string `json:"context_tags,omitempty"`
	DryRun      bool              `json:"dry_run"`
	Objects     []ApplyObject     `json:"objects"`
}

// PrintCLI prints each object's operation and diff.
func (o ApplyOutput) PrintCLI(stdout, _ io.Writer) int {
	for _, obj := range o.Objects {
		state := obj.Operation
		if o.DryRun {
			state += " (dry run)"
		} else if obj.Applied {
			state += "d"
			if obj.Operation == "unchanged" {
				state = "unchanged"
			}
		}
		fmt.Fprintf(stdout, "%s/%s %s\n", obj.Kind, obj.Name, state)
		if obj.Diff != nil {
			io.WriteString(stdout, obj.Diff.Text)
		}
	}
	return 0
}

type applyInput struct {
	Context   string `json:"context"`
	Namespace string `json:"namespace"`
	Manifest  string `json:"manifest"`
	DryRun    bool   `json:"dry_run"`
}

// planned is one manifest object, resolved.
type planned struct {
	obj *unstructured.Unstructured
	res kube.Resource
	ns  string
}

func (pl planned) id() string { return pl.res.Kind + "/" + pl.obj.GetName() }

func (pl planned) resource(c *kube.Client) dynamic.ResourceInterface {
	if pl.res.Namespaced {
		return c.Dynamic.Resource(pl.res.GVR).Namespace(pl.ns)
	}
	return c.Dynamic.Resource(pl.res.GVR)
}

// parseManifest splits a YAML or JSON manifest into objects; List kinds
// are expanded.
func parseManifest(manifest string) ([]map[string]any, error) {
	dec := yamlutil.NewYAMLOrJSONDecoder(strings.NewReader(manifest), 4096)
	var out []map[string]any
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("manifest: %w", err)
		}
		if len(doc) == 0 {
			continue
		}
		if kind, _ := doc["kind"].(string); strings.HasSuffix(kind, "List") {
			if items, ok := doc["items"].([]any); ok {
				if v, _ := doc["apiVersion"].(string); v != "v1" {
					return nil, fmt.Errorf("manifest: %s %s is not supported; give its items as separate documents", v, kind)
				}
				for _, it := range items {
					if m, ok := it.(map[string]any); ok {
						out = append(out, m)
					}
				}
				continue
			}
		}
		out = append(out, doc)
	}
}

// plan resolves the manifest's objects and their namespaces.
func (p *Provider) plan(in applyInput) (resolved, []planned, error) {
	r, err := p.resolve(scope{Context: in.Context, Namespace: in.Namespace})
	if err != nil {
		return r, nil, err
	}
	docs, err := parseManifest(in.Manifest)
	if err != nil {
		return r, nil, err
	}
	if len(docs) == 0 {
		return r, nil, fmt.Errorf("the manifest has no objects")
	}
	var out []planned
	for i, doc := range docs {
		obj := &unstructured.Unstructured{Object: doc}
		if obj.GetAPIVersion() == "" || obj.GetKind() == "" {
			return r, nil, fmt.Errorf("manifest object %d: apiVersion and kind are required", i+1)
		}
		if obj.GetName() == "" {
			return r, nil, fmt.Errorf("manifest object %d (%s): metadata.name is required (generateName is not supported)", i+1, obj.GetKind())
		}
		res, err := r.client.Resources.ForKind(obj.GetAPIVersion(), obj.GetKind())
		if err != nil {
			return r, nil, kube.Explain(err)
		}
		pl := planned{obj: obj, res: res}
		if res.Namespaced {
			pl.ns = obj.GetNamespace()
			switch {
			case pl.ns == "":
				pl.ns = r.ns
			case in.Namespace != "" && pl.ns != in.Namespace:
				return r, nil, fmt.Errorf("%s is in namespace %s, but namespace %s was given", pl.id(), pl.ns, in.Namespace)
			}
			obj.SetNamespace(pl.ns)
		} else {
			obj.SetNamespace("")
		}
		out = append(out, pl)
	}
	return r, out, nil
}

// normalized is an object as shown in a diff: without fields the server
// manages (managedFields, resourceVersion, generation, uid,
// creationTimestamp, status) and the last-applied annotation.
func normalized(u *unstructured.Unstructured) *unstructured.Unstructured {
	if u == nil {
		return nil
	}
	c := u.DeepCopy()
	for _, f := range []string{"managedFields", "resourceVersion", "generation", "uid", "creationTimestamp"} {
		unstructured.RemoveNestedField(c.Object, "metadata", f)
	}
	unstructured.RemoveNestedField(c.Object, "status")
	if ann := c.GetAnnotations(); ann[lastApplied] != "" {
		delete(ann, lastApplied)
		if len(ann) == 0 {
			ann = nil
		}
		c.SetAnnotations(ann)
	}
	return c
}

// objectDiff is the diff between the live object (nil if none) and the
// result of applying, with Secret values redacted.
func objectDiff(id string, res kube.Resource, live, after *unstructured.Unstructured) (string, error) {
	cur, next := normalized(live), normalized(after)
	if secretResource(res) {
		redactSecret(next)
		if cur != nil {
			redactSecret(cur)
		}
	}
	a := ""
	if cur != nil {
		var err error
		if a, err = toYAML(cur); err != nil {
			return "", err
		}
	}
	b, err := toYAML(next)
	if err != nil {
		return "", err
	}
	return diffLines(id+" (live)", id+" (after apply)", a, b), nil
}

func (p *Provider) apply() *action.Action {
	return &action.Action{
		ID:      "k8s.apply",
		Summary: "Apply a manifest (server-side apply), showing a dry-run diff first",
		Description: "Apply one or more objects (YAML, several documents allowed) with server-side apply (field manager cox, " +
			"conflicts are not forced). Every object is first checked with a server-side dry run; if any fails, nothing is " +
			"applied. Returns each object's operation (create, configure, unchanged) and diff against the live object. " +
			"Use dry_run=true first and show the user the diff. Secret values are redacted in diffs and never compared. " +
			"Objects that depend on others in the same manifest (a Namespace and objects in it, a CRD and its custom " +
			"resources) fail the dry run: apply them in two steps. List kinds other than v1 List are not accepted. " + prodHint,
		Input:       applySchema,
		Positional:  []string{"context"},
		Annotations: writeRemote,
		Redact:      redactApply,
		Label: func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
			if err := action.ExactKeys(raw, applySchema); err != nil {
				return action.Assessment{}, err
			}
			var in applyInput
			if err := action.Decode(raw, &in); err != nil {
				return action.Assessment{}, err
			}
			r, objs, err := p.plan(in)
			if err != nil {
				return action.Assessment{}, err
			}
			risk := action.Destructive
			if in.DryRun {
				risk = action.Read
				for _, pl := range objs {
					if secretResource(pl.res) {
						risk = action.Write // compares against a live Secret
					}
				}
			}
			var targets []action.Target
			seen := map[string]bool{}
			var ids []string
			for _, pl := range objs {
				ids = append(ids, pl.id())
				if !seen[pl.ns] {
					seen[pl.ns] = true
					targets = append(targets, p.Clusters.Target(in.Context, pl.ns))
				}
			}
			if len(ids) > 10 {
				ids = append(ids[:10], "…")
			}
			preview := fmt.Sprintf("apply %d object(s) to %s: %s", len(objs), in.Context, strings.Join(ids, ", "))
			if in.DryRun {
				preview += " (dry run)"
			}
			if len(r.info.Tags) > 0 {
				preview += " (" + tagString(r.info.Tags) + ")"
			}
			return action.Assessment{Risk: risk, Targets: targets, Preview: preview}, nil
		},
		Run: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in applyInput
			if err := action.Decode(raw, &in); err != nil {
				return nil, err
			}
			r, objs, err := p.plan(in)
			if err != nil {
				return nil, err
			}
			c := r.client
			out := ApplyOutput{Context: in.Context, ContextNote: r.info.Note, ContextTags: r.info.Tags, DryRun: in.DryRun}
			for _, pl := range objs {
				live, err := pl.resource(c).Get(ctx, pl.obj.GetName(), metav1.GetOptions{})
				if apierrors.IsNotFound(err) {
					live, err = nil, nil
				}
				if err != nil {
					return nil, fmt.Errorf("%s: %w; nothing was applied", pl.id(), kube.Explain(err))
				}
				after, err := c.Apply.Apply(ctx, pl.res, pl.ns, pl.obj, true)
				if err != nil {
					return nil, fmt.Errorf("dry run of %s failed: %w; nothing was applied", pl.id(), err)
				}
				diff, err := objectDiff(pl.id(), pl.res, live, after)
				if err != nil {
					return nil, err
				}
				o := ApplyObject{Kind: pl.res.Kind, Namespace: pl.ns, Name: pl.obj.GetName(), Operation: "configure"}
				switch {
				case live == nil:
					o.Operation = "create"
				case secretResource(pl.res):
					// Always: "unchanged" would say the given values
					// equal the live ones.
				case diff == "":
					o.Operation = "unchanged"
				}
				if diff != "" {
					shaped, err := p.shape(ctx, diff)
					if err != nil {
						return nil, err
					}
					o.Diff = &shaped
				}
				out.Objects = append(out.Objects, o)
			}
			if in.DryRun {
				return out, nil
			}
			for i, pl := range objs {
				if _, err := c.Apply.Apply(ctx, pl.res, pl.ns, pl.obj, false); err != nil {
					done := []string{}
					for _, prev := range objs[:i] {
						done = append(done, prev.id())
					}
					return nil, fmt.Errorf("applying %s failed: %w (applied before it: %s)", pl.id(), err, dash(strings.Join(done, ", ")))
				}
				out.Objects[i].Applied = true
			}
			return out, nil
		},
	}
}

// applySchema is k8s.apply's input.
var applySchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"context":   contextProperty,
		"namespace": map[string]any{"type": "string", "description": "for objects without one; default: the context's namespace"},
		"manifest":  map[string]any{"type": "string", "description": "YAML (or JSON) objects, separated by ---"},
		"dry_run":   map[string]any{"type": "boolean", "description": "only show what would change"},
	},
	"required": []string{"context", "manifest"},
}

// redactApply is k8s.apply's arguments for the audit log: Secret values
// in the manifest are replaced by their lengths.
func redactApply(raw json.RawMessage) json.RawMessage {
	var in applyInput
	if action.ExactKeys(raw, applySchema) != nil || action.Decode(raw, &in) != nil {
		return json.RawMessage(`{"redacted":"invalid arguments"}`)
	}
	in.Manifest = redactManifest(in.Manifest)
	b, err := json.Marshal(in)
	if err != nil {
		return json.RawMessage(`{"redacted":"arguments could not be encoded"}`)
	}
	return b
}

// redactManifest returns the manifest with every Secret's values replaced
// by their lengths; a manifest that cannot be parsed is not kept at all.
func redactManifest(manifest string) string {
	docs, err := parseManifest(manifest)
	if err != nil {
		return fmt.Sprintf("<manifest redacted: not valid YAML, %d bytes>", len(manifest))
	}
	parts := make([]string, 0, len(docs))
	for _, doc := range docs {
		u := &unstructured.Unstructured{Object: doc}
		if looksLikeSecret(u) {
			redactSecret(u)
		}
		b, err := yaml.Marshal(u.Object)
		if err != nil {
			return fmt.Sprintf("<manifest redacted: %d bytes>", len(manifest))
		}
		parts = append(parts, string(b))
	}
	return strings.Join(parts, "---\n")
}

// DeleteOutput is the result of k8s.delete.
type DeleteOutput struct {
	Context   string `json:"context"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Warning   string `json:"warning,omitempty"`
}

// cascades warns about deleting kinds that take much more with them.
var cascades = map[string]string{
	"Namespace":                "deleting a Namespace deletes every object in it",
	"Node":                     "the node's pods are evicted and it leaves the cluster; drain it first if it is coming back",
	"PersistentVolume":         "the volume's data may be deleted, depending on its reclaim policy",
	"CustomResourceDefinition": "deleting a CRD deletes every object of its kind in all namespaces",
}

func (p *Provider) delete() *action.Action {
	return &action.Action{
		ID:      "k8s.delete",
		Summary: "Delete one Kubernetes object",
		Description: "Delete one object by kind and name (no selectors, no bulk deletion). Deleting a Namespace, Node, " +
			"PersistentVolume or CustomResourceDefinition takes much more with it: confirm with the user first. " + prodHint,
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
		Annotations: writeRemote,
		Label: func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
			as, in, _, err := p.label(raw, action.Destructive, "delete")
			switch {
			case err != nil:
			case in.Name == "":
				err = fmt.Errorf("name is required")
			case in.AllNamespaces:
				err = fmt.Errorf("delete takes one object: all_namespaces is not allowed")
			}
			return as, err
		},
		Run: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in scope
			if err := action.Decode(raw, &in); err != nil {
				return nil, err
			}
			if in.AllNamespaces {
				return nil, fmt.Errorf("delete takes one object: all_namespaces is not allowed")
			}
			r, err := p.resolve(in)
			if err != nil {
				return nil, err
			}
			if in.Name == "" {
				return nil, fmt.Errorf("name is required")
			}
			if _, err := r.resource().Get(ctx, in.Name, metav1.GetOptions{}); err != nil {
				return nil, kube.Explain(err)
			}
			bg := metav1.DeletePropagationBackground
			if err := r.resource().Delete(ctx, in.Name, metav1.DeleteOptions{PropagationPolicy: &bg}); err != nil {
				return nil, kube.Explain(err)
			}
			return DeleteOutput{Context: in.Context, Kind: r.res.Kind, Namespace: r.ns, Name: in.Name, Warning: cascades[r.res.Kind]}, nil
		},
	}
}

// ScaleOutput is the result of k8s.scale.
type ScaleOutput struct {
	Context   string `json:"context"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	From      int64  `json:"from"`
	To        int64  `json:"to"`
}

type scaleInput struct {
	scope
	Replicas *int64 `json:"replicas"`
}

var scalable = map[string]bool{"Deployment": true, "StatefulSet": true, "ReplicaSet": true}

func (p *Provider) scale() *action.Action {
	return &action.Action{
		ID:      "k8s.scale",
		Summary: "Set the replica count of a Deployment, StatefulSet or ReplicaSet",
		Description: "Set the number of replicas of a Deployment, StatefulSet or ReplicaSet. Scaling to 0 stops the " +
			"workload. " + prodHint,
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"context":   contextProperty,
				"kind":      map[string]any{"type": "string", "description": "deployment, statefulset or replicaset"},
				"name":      map[string]any{"type": "string"},
				"namespace": namespaceProperty,
				"replicas":  map[string]any{"type": "integer", "minimum": 0},
			},
			"required": []string{"context", "kind", "name", "replicas"},
		},
		Positional:  []string{"context", "kind", "name"},
		Annotations: writeRemote,
		Label: func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
			var in scaleInput
			if err := action.Decode(raw, &in); err != nil {
				return action.Assessment{}, err
			}
			if err := in.check(); err != nil {
				return action.Assessment{}, err
			}
			risk := action.Write
			if *in.Replicas == 0 {
				risk = action.Destructive // stops the workload
			}
			as, _, r, err := p.label(raw, risk, "scale")
			if err != nil {
				return as, err
			}
			if !scalable[r.res.Kind] {
				return as, fmt.Errorf("cannot scale a %s (only Deployment, StatefulSet and ReplicaSet)", r.res.Kind)
			}
			as.Preview += fmt.Sprintf(" to %d replicas", *in.Replicas)
			return as, nil
		},
		Run: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in scaleInput
			if err := action.Decode(raw, &in); err != nil {
				return nil, err
			}
			if err := in.check(); err != nil {
				return nil, err
			}
			in.AllNamespaces = false
			r, err := p.resolve(in.scope)
			if err != nil {
				return nil, err
			}
			if !scalable[r.res.Kind] {
				return nil, fmt.Errorf("cannot scale a %s (only Deployment, StatefulSet and ReplicaSet)", r.res.Kind)
			}
			cur, err := r.resource().Get(ctx, in.Name, metav1.GetOptions{})
			if err != nil {
				return nil, kube.Explain(err)
			}
			from, ok, _ := unstructured.NestedInt64(cur.Object, "spec", "replicas")
			if !ok {
				from = 1
			}
			patch := fmt.Sprintf(`{"spec":{"replicas":%d}}`, *in.Replicas)
			if _, err := r.resource().Patch(ctx, in.Name, types.MergePatchType, []byte(patch), metav1.PatchOptions{FieldManager: kube.FieldManager}); err != nil {
				return nil, kube.Explain(err)
			}
			return ScaleOutput{Context: in.Context, Kind: r.res.Kind, Namespace: r.ns, Name: in.Name, From: from, To: *in.Replicas}, nil
		},
	}
}

func (in scaleInput) check() error {
	switch {
	case in.Name == "":
		return fmt.Errorf("name is required")
	case in.Replicas == nil:
		return fmt.Errorf("replicas is required")
	case *in.Replicas < 0:
		return fmt.Errorf("replicas must not be negative")
	}
	return nil
}

// Revision is one revision of a Deployment (one of its ReplicaSets).
type Revision struct {
	Revision   int64    `json:"revision"`
	ReplicaSet string   `json:"replicaset"`
	Images     []string `json:"images"`
	Age        string   `json:"age"`
	Current    bool     `json:"current,omitempty"`
}

// RolloutOutput is the result of k8s.rollout.
type RolloutOutput struct {
	Context    string     `json:"context"`
	Kind       string     `json:"kind"`
	Namespace  string     `json:"namespace"`
	Name       string     `json:"name"`
	Action     string     `json:"action"`
	Done       bool       `json:"done,omitempty"` // status: the rollout has finished
	Message    string     `json:"message"`
	Revisions  []Revision `json:"revisions,omitempty"` // status of a Deployment, newest first
	ToRevision int64      `json:"to_revision,omitempty"`
}

type rolloutInput struct {
	scope
	Action     string `json:"action"`
	ToRevision int64  `json:"to_revision"`
}

var rollable = map[string]bool{"Deployment": true, "StatefulSet": true, "DaemonSet": true}

const (
	revisionAnnotation = "deployment.kubernetes.io/revision"
	restartedAt        = "kubectl.kubernetes.io/restartedAt"
)

func (p *Provider) rollout() *action.Action {
	return &action.Action{
		ID:      "k8s.rollout",
		Summary: "Show or manage the rollout of a Deployment, StatefulSet or DaemonSet",
		Description: "action status: whether the latest rollout finished (and a Deployment's revisions); restart: " +
			"restart all pods (rolling); pause / resume: a Deployment's rollouts; undo: roll a Deployment back to " +
			"to_revision (default: the previous one). status only reads. " + prodHint,
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"context":     contextProperty,
				"action":      map[string]any{"type": "string", "enum": []string{"status", "restart", "pause", "resume", "undo"}},
				"kind":        map[string]any{"type": "string", "description": "deployment, statefulset or daemonset"},
				"name":        map[string]any{"type": "string"},
				"namespace":   namespaceProperty,
				"to_revision": map[string]any{"type": "integer", "minimum": 1, "description": "undo: the revision to go back to"},
			},
			"required": []string{"context", "action", "kind", "name"},
		},
		Positional:  []string{"context", "action", "kind", "name"},
		Annotations: action.Annotations{Destructive: true, OpenWorld: true},
		Label: func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
			var in rolloutInput
			if err := action.Decode(raw, &in); err != nil {
				return action.Assessment{}, err
			}
			risk := action.Write
			switch in.Action {
			case "status":
				risk = action.Read
			case "restart", "pause", "resume", "undo":
			default:
				return action.Assessment{}, fmt.Errorf("action must be status, restart, pause, resume or undo")
			}
			as, _, r, err := p.label(raw, risk, "rollout "+in.Action)
			if err != nil {
				return as, err
			}
			return as, checkRollout(r.res.Kind, in)
		},
		Run: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in rolloutInput
			if err := action.Decode(raw, &in); err != nil {
				return nil, err
			}
			in.AllNamespaces = false
			r, err := p.resolve(in.scope)
			if err != nil {
				return nil, err
			}
			if err := checkRollout(r.res.Kind, in); err != nil {
				return nil, err
			}
			obj, err := r.resource().Get(ctx, in.Name, metav1.GetOptions{})
			if err != nil {
				return nil, kube.Explain(err)
			}
			out := RolloutOutput{Context: in.Context, Kind: r.res.Kind, Namespace: r.ns, Name: in.Name, Action: in.Action}
			switch in.Action {
			case "status":
				out.Done, out.Message = rolloutStatus(obj)
				if r.res.Kind == "Deployment" {
					if out.Revisions, err = p.revisions(ctx, r, obj); err != nil {
						return nil, err
					}
				}
				return out, nil
			case "restart":
				if paused, _, _ := unstructured.NestedBool(obj.Object, "spec", "paused"); paused {
					return nil, fmt.Errorf("%s/%s is paused; resume it before restarting", r.res.Kind, in.Name)
				}
				patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{"metadata": map[string]any{
					"annotations": map[string]string{restartedAt: p.now().Format(time.RFC3339)}}}}})
				err = p.patch(ctx, r, in.Name, types.MergePatchType, patch)
				out.Message = "restarted: pods are replaced one by one"
			case "pause", "resume":
				err = p.patch(ctx, r, in.Name, types.MergePatchType, []byte(fmt.Sprintf(`{"spec":{"paused":%v}}`, in.Action == "pause")))
				out.Message = in.Action + "d"
			case "undo":
				if paused, _, _ := unstructured.NestedBool(obj.Object, "spec", "paused"); paused {
					return nil, fmt.Errorf("%s/%s is paused; resume it before rolling back", r.res.Kind, in.Name)
				}
				out.ToRevision, err = p.undo(ctx, r, obj, in.ToRevision)
				out.Message = fmt.Sprintf("rolled back to revision %d", out.ToRevision)
			}
			if err != nil {
				return nil, err
			}
			return out, nil
		},
	}
}

func checkRollout(kind string, in rolloutInput) error {
	switch {
	case in.Name == "":
		return fmt.Errorf("name is required")
	case !rollable[kind]:
		return fmt.Errorf("%s has no rollouts (only Deployment, StatefulSet and DaemonSet)", kind)
	case (in.Action == "pause" || in.Action == "resume" || in.Action == "undo") && kind != "Deployment":
		return fmt.Errorf("%s is supported for Deployments only; for a %s, apply the previous spec instead", in.Action, kind)
	}
	return nil
}

func (p *Provider) patch(ctx context.Context, r resolved, name string, pt types.PatchType, data []byte) error {
	_, err := r.resource().Patch(ctx, name, pt, data, metav1.PatchOptions{FieldManager: kube.FieldManager})
	return kube.Explain(err)
}

// rolloutStatus reports whether a workload's rollout is done, like
// kubectl rollout status (without waiting).
func rolloutStatus(u *unstructured.Unstructured) (bool, string) {
	gen := u.GetGeneration()
	observed, _, _ := unstructured.NestedInt64(u.Object, "status", "observedGeneration")
	name := u.GetName()
	if gen > observed {
		return false, fmt.Sprintf("waiting for the %s spec update to be observed", strings.ToLower(u.GetKind()))
	}
	switch u.GetKind() {
	case "Deployment":
		d := convert[appsv1.Deployment](u)
		for _, c := range d.Status.Conditions {
			if c.Type == appsv1.DeploymentProgressing && c.Reason == "ProgressDeadlineExceeded" {
				return false, fmt.Sprintf("deployment %q exceeded its progress deadline", name)
			}
		}
		want := replicas(d.Spec.Replicas)
		switch s := d.Status; {
		case s.UpdatedReplicas < want:
			return false, fmt.Sprintf("waiting for deployment %q rollout to finish: %d out of %d new replicas have been updated", name, s.UpdatedReplicas, want)
		case s.Replicas > s.UpdatedReplicas:
			return false, fmt.Sprintf("waiting for deployment %q rollout to finish: %d old replicas are pending termination", name, s.Replicas-s.UpdatedReplicas)
		case s.AvailableReplicas < s.UpdatedReplicas:
			return false, fmt.Sprintf("waiting for deployment %q rollout to finish: %d of %d updated replicas are available", name, s.AvailableReplicas, s.UpdatedReplicas)
		}
		return true, fmt.Sprintf("deployment %q successfully rolled out", name)
	case "StatefulSet":
		s := convert[appsv1.StatefulSet](u)
		if s.Spec.UpdateStrategy.Type == appsv1.OnDeleteStatefulSetStrategyType {
			return true, "rollout status is not available for the OnDelete update strategy"
		}
		want := replicas(s.Spec.Replicas)
		switch {
		case s.Status.ReadyReplicas < want:
			return false, fmt.Sprintf("waiting for %d pods to be ready (%d of %d)", want-s.Status.ReadyReplicas, s.Status.ReadyReplicas, want)
		case s.Status.UpdateRevision != s.Status.CurrentRevision:
			return false, fmt.Sprintf("waiting for statefulset rolling update to complete: %d pods at revision %s", s.Status.UpdatedReplicas, s.Status.UpdateRevision)
		}
		return true, fmt.Sprintf("statefulset rolling update complete: %d pods at revision %s", s.Status.CurrentReplicas, s.Status.CurrentRevision)
	case "DaemonSet":
		d := convert[appsv1.DaemonSet](u)
		if d.Spec.UpdateStrategy.Type == appsv1.OnDeleteDaemonSetStrategyType {
			return true, "rollout status is not available for the OnDelete update strategy"
		}
		s := d.Status
		switch {
		case s.UpdatedNumberScheduled < s.DesiredNumberScheduled:
			return false, fmt.Sprintf("waiting for daemon set %q rollout to finish: %d out of %d new pods have been updated", name, s.UpdatedNumberScheduled, s.DesiredNumberScheduled)
		case s.NumberAvailable < s.DesiredNumberScheduled:
			return false, fmt.Sprintf("waiting for daemon set %q rollout to finish: %d of %d updated pods are available", name, s.NumberAvailable, s.DesiredNumberScheduled)
		}
		return true, fmt.Sprintf("daemon set %q successfully rolled out", name)
	}
	return false, "unknown kind"
}

type revisionRS struct {
	Revision
	template map[string]any
}

// revisionSets returns a Deployment's ReplicaSets by revision, newest first.
func (p *Provider) revisionSets(ctx context.Context, r resolved, d *unstructured.Unstructured) ([]revisionRS, error) {
	rsRes, err := r.client.Resources.ForKind("apps/v1", "ReplicaSet")
	if err != nil {
		return nil, kube.Explain(err)
	}
	list, err := r.client.Dynamic.Resource(rsRes.GVR).Namespace(d.GetNamespace()).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, kube.Explain(err)
	}
	current := d.GetAnnotations()[revisionAnnotation]
	now := p.now()
	var out []revisionRS
	for i := range list.Items {
		rs := &list.Items[i]
		if !ownedBy(rs, d) {
			continue
		}
		rev, err := strconv.ParseInt(rs.GetAnnotations()[revisionAnnotation], 10, 64)
		if err != nil {
			continue
		}
		typed := convert[appsv1.ReplicaSet](rs)
		var images []string
		for _, c := range typed.Spec.Template.Spec.Containers {
			images = append(images, c.Image)
		}
		tmpl, _, _ := unstructured.NestedMap(rs.Object, "spec", "template")
		out = append(out, revisionRS{Revision: Revision{Revision: rev, ReplicaSet: rs.GetName(), Images: images,
			Age: age(now, rs.GetCreationTimestamp().Time), Current: strconv.FormatInt(rev, 10) == current}, template: tmpl})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Revision.Revision > out[j].Revision.Revision })
	return out, nil
}

func ownedBy(obj, owner *unstructured.Unstructured) bool {
	for _, o := range obj.GetOwnerReferences() {
		if o.Kind == owner.GetKind() && o.Name == owner.GetName() && (o.UID == "" || owner.GetUID() == "" || o.UID == owner.GetUID()) {
			return true
		}
	}
	return false
}

func (p *Provider) revisions(ctx context.Context, r resolved, d *unstructured.Unstructured) ([]Revision, error) {
	sets, err := p.revisionSets(ctx, r, d)
	out := make([]Revision, len(sets))
	for i, s := range sets {
		out[i] = s.Revision
	}
	return out, err
}

// undo rolls a Deployment back to a revision (default: the one before the
// current one) by restoring that ReplicaSet's pod template.
func (p *Provider) undo(ctx context.Context, r resolved, d *unstructured.Unstructured, to int64) (int64, error) {
	sets, err := p.revisionSets(ctx, r, d)
	if err != nil {
		return 0, err
	}
	if len(sets) == 0 {
		return 0, fmt.Errorf("deployment %s has no revisions to roll back to", d.GetName())
	}
	current, err := strconv.ParseInt(d.GetAnnotations()[revisionAnnotation], 10, 64)
	if err != nil {
		current = sets[0].Revision.Revision
	}
	var target *revisionRS
	var have []string
	for i := range sets {
		rev := sets[i].Revision.Revision
		have = append(have, strconv.FormatInt(rev, 10))
		if target == nil && ((to != 0 && rev == to) || (to == 0 && rev < current)) {
			target = &sets[i]
		}
	}
	if target == nil {
		if to == 0 {
			return 0, fmt.Errorf("deployment %s has no revision before %d (revisions: %s)", d.GetName(), current, strings.Join(have, ", "))
		}
		return 0, fmt.Errorf("deployment %s has no revision %d (revisions: %s)", d.GetName(), to, strings.Join(have, ", "))
	}
	tmpl := runtime.DeepCopyJSON(target.template)
	unstructured.RemoveNestedField(tmpl, "metadata", "labels", "pod-template-hash")
	patch, err := json.Marshal([]map[string]any{{"op": "replace", "path": "/spec/template", "value": tmpl}})
	if err != nil {
		return 0, err
	}
	return target.Revision.Revision, p.patch(ctx, r, d.GetName(), types.JSONPatchType, patch)
}

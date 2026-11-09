// Package k8s registers the Kubernetes Actions: contexts, reading objects,
// events, logs, exec, changes (apply, delete, scale, rollout) and
// diagnosis, on the contexts of the user's kubeconfig.
package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/inventory"
	kube "github.com/hua-ri/coxswain/internal/k8s"
	"github.com/hua-ri/coxswain/internal/shaper"
	"k8s.io/client-go/dynamic"
)

// captureLimit bounds how much output (logs, exec) is held in memory.
const captureLimit = 4 << 20

// humanLimits effectively disables shaping for people.
var humanLimits = shaper.Limits{MaxBytes: captureLimit, MaxLines: 1 << 30}

type Provider struct {
	Clusters  *kube.Clusters
	Hosts     *inventory.Store // for k8s.node_host
	Artifacts *shaper.Artifacts
	Now       func() time.Time // tests
}

func (p *Provider) Register(r *action.Registry) {
	r.Register(p.contexts())
	r.Register(p.contextsTag())
	r.Register(p.get())
	r.Register(p.describe())
	r.Register(p.events())
}

func (p *Provider) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// prodHint is part of every description of a tool that changes things.
const prodHint = "Contexts tagged env=prod are production: confirm with the user before changing them. Respect the context note."

var (
	contextProperty   = map[string]any{"type": "string", "description": "kubeconfig context (see k8s_contexts)"}
	namespaceProperty = map[string]any{"type": "string", "description": "namespace; default: the context's namespace"}
	kindProperty      = map[string]any{"type": "string", "description": "kind: pods, po, Deployment, deployments.apps, apps/v1/Deployment, …"}
)

// scope is what most actions take to address objects.
type scope struct {
	Context       string `json:"context"`
	Namespace     string `json:"namespace"`
	AllNamespaces bool   `json:"all_namespaces"`
	Kind          string `json:"kind"`
	Name          string `json:"name"`
}

// resolved is a scope with its context, client and resource looked up.
type resolved struct {
	info   kube.ContextInfo
	client *kube.Client
	res    kube.Resource
	ns     string // "" for cluster-scoped resources and all namespaces
}

// resolve looks up the context, its client and (if kind is set) the
// resource; ns defaults to the context's namespace.
func (p *Provider) resolve(in scope) (resolved, error) {
	var r resolved
	if in.Context == "" {
		return r, fmt.Errorf("context is required (see k8s_contexts)")
	}
	var err error
	if r.info, err = p.Clusters.Context(in.Context); err != nil {
		return r, err
	}
	if r.client, err = p.Clusters.Client(in.Context); err != nil {
		return r, err
	}
	r.ns = in.Namespace
	if r.ns == "" && !in.AllNamespaces {
		r.ns = r.info.Namespace
	}
	if in.AllNamespaces {
		r.ns = ""
	}
	if in.Kind != "" {
		if r.res, err = r.client.Resources.Resolve(in.Kind); err != nil {
			return r, kube.Explain(err)
		}
		if !r.res.Namespaced {
			r.ns = ""
		}
	}
	return r, nil
}

// resource is the dynamic client for r's resource in its namespace.
func (r resolved) resource() dynamic.ResourceInterface {
	ri := r.client.Dynamic.Resource(r.res.GVR)
	if r.res.Namespaced && r.ns != "" {
		return ri.Namespace(r.ns)
	}
	return ri
}

// targetNS is how a resolved scope appears in the audit target.
func (r resolved) targetNS(all bool) string {
	switch {
	case all && (r.res.Kind == "" || r.res.Namespaced):
		return "*"
	case r.res.Kind != "" && !r.res.Namespaced:
		return ""
	}
	return r.ns
}

// label assesses an invocation on a scope with the given risk.
func (p *Provider) label(raw json.RawMessage, risk action.Risk, verb string) (action.Assessment, scope, resolved, error) {
	var in scope
	if err := action.Decode(raw, &in); err != nil {
		return action.Assessment{}, in, resolved{}, err
	}
	r, err := p.resolve(in)
	if err != nil {
		return action.Assessment{}, in, r, err
	}
	ns := r.targetNS(in.AllNamespaces)
	return action.Assessment{
		Risk:    risk,
		Targets: []action.Target{p.Clusters.Target(in.Context, ns)},
		Preview: preview(verb, in, r, ns),
	}, in, r, nil
}

func preview(verb string, in scope, r resolved, ns string) string {
	parts := []string{in.Context}
	if ns != "" {
		parts = append(parts, ns)
	}
	s := verb + " "
	if r.res.Kind != "" {
		s += r.res.Kind
		if in.Name != "" {
			s += "/" + in.Name
		}
		s += " "
	}
	s += "in " + strings.Join(parts, "/")
	if len(r.info.Tags) > 0 {
		s += " (" + tagString(r.info.Tags) + ")"
	}
	return s
}

func readLabel(verb string, p *Provider) func(context.Context, json.RawMessage) (action.Assessment, error) {
	return func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
		as, _, _, err := p.label(raw, action.Read, verb)
		return as, err
	}
}

// limits are the shaping limits for the invoking actor.
func limits(ctx context.Context) shaper.Limits {
	if a, ok := action.ActorFrom(ctx); ok && a.Kind == action.Human {
		return humanLimits
	}
	return shaper.Default
}

func (p *Provider) shape(ctx context.Context, s string) (shaper.Shaped, error) {
	return p.Artifacts.Shape(s, limits(ctx))
}

var readOnlyRemote = action.Annotations{ReadOnly: true, Idempotent: true, OpenWorld: true}

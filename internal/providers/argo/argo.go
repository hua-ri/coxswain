// Package argo registers the Argo CD and Argo Workflows actions:
// instances, logging in, and working with applications and workflows
// through the servers' REST APIs.
package argo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/argo"
	k8sprov "github.com/hua-ri/coxswain/internal/providers/k8s"
	"github.com/hua-ri/coxswain/internal/shaper"
)

type Provider struct {
	Sessions  *argo.Sessions
	Artifacts *shaper.Artifacts
	K8s       K8s              // to follow an Argo problem into its cluster and to the node's SSH host; may be nil
	Now       func() time.Time // tests
}

// K8s is what Argo diagnoses use of the Kubernetes provider.
type K8s interface {
	Diagnose(ctx context.Context, kctx, namespace, kind, name string) (k8sprov.DiagnoseOutput, error)
	NodeHost(ctx context.Context, kctx, node string) (k8sprov.NodeHostOutput, error)
	ContextsForServer(server string) []string
	HasContext(name string) bool
}

func (p *Provider) Register(r *action.Registry) {
	r.Register(p.add())
	r.Register(p.update())
	r.Register(p.remove())
	r.Register(p.export())
	r.Register(p.importInstances())
	for _, k := range []argo.Kind{argo.CD, argo.Workflows} {
		r.Register(p.instances(k))
		r.Register(p.loginMethods(k))
		r.Register(p.login(k))
		r.Register(p.logout(k))
		r.Register(p.cancelLogin(k))
	}
	p.registerCD(r)
	p.registerWF(r)
}

func (p *Provider) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func str(d string) map[string]any { return map[string]any{"type": "string", "description": d} }

var (
	kindProperty     = map[string]any{"type": "string", "enum": []string{"argocd", "argowf"}, "description": "argocd (Argo CD) or argowf (Argo Workflows)"}
	instanceProperty = str("the instance's name, as listed by argocd_instances / argowf_instances")
	tagsProperty     = map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "key=value; env=prod marks production"}
)

func kindName(k argo.Kind) string {
	if k == argo.CD {
		return "Argo CD"
	}
	return "Argo Workflows"
}

// target is the audit target of an instance (or something in it).
func target(i argo.Instance, sub string) action.Target {
	id := i.ID()
	if sub != "" {
		id += "/" + sub
	}
	return action.Target{Kind: string(i.Kind), ID: id, Tags: i.Tags}
}

func parseTags(list []string) (map[string]string, error) {
	tags := map[string]string{}
	for _, kv := range list {
		k, v, ok := strings.Cut(kv, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" || strings.ContainsAny(k, " \t") {
			return nil, fmt.Errorf("tag %q must be key=value", kv)
		}
		tags[k] = strings.TrimSpace(v)
	}
	if len(tags) == 0 {
		return nil, nil
	}
	return tags, nil
}

func agentFacing(key string) bool {
	return key == "env" || key == "ai" || strings.HasPrefix(key, "ai.")
}

// riskOfChange: the note and agent-facing tags instruct agents, so
// changing them is destructive, as for hosts and clusters.
func riskOfChange(old, n argo.Instance) action.Risk {
	if old.Note != n.Note {
		return action.Destructive
	}
	for k := range n.Tags {
		if agentFacing(k) && n.Tags[k] != old.Tags[k] {
			return action.Destructive
		}
	}
	for k := range old.Tags {
		if agentFacing(k) && n.Tags[k] != old.Tags[k] {
			return action.Destructive
		}
	}
	return action.Write
}

// InstanceView is an instance as actions show it: its settings and the
// state of its credential, never the credential.
type InstanceView struct {
	argo.Instance
	Login        argo.Status `json:"login"`
	LoginPending bool        `json:"login_pending,omitempty"` // a browser login waits for the user
}

func (p *Provider) view(i argo.Instance) InstanceView {
	return InstanceView{Instance: i, Login: p.Sessions.Status(i), LoginPending: p.Sessions.LoginPending(i)}
}

// decode checks keys exactly (several arguments are secrets, see
// action.RedactWriteOnly) and decodes.
func decode(raw json.RawMessage, schema map[string]any, v any) error {
	if err := action.ExactKeys(raw, schema); err != nil {
		return err
	}
	return action.Decode(raw, v)
}

func (p *Provider) instance(kind argo.Kind, name string) (argo.Instance, error) {
	if name == "" {
		list, err := p.Sessions.Store.List(kind)
		if err != nil {
			return argo.Instance{}, err
		}
		if len(list) == 1 {
			return list[0], nil
		}
		if len(list) == 0 {
			return argo.Instance{}, fmt.Errorf("no %s instance is configured; add one with argo_add", kindName(kind))
		}
		return argo.Instance{}, fmt.Errorf("instance is required: there are %d %s instances (see %s_instances)", len(list), kindName(kind), kind)
	}
	return p.Sessions.Store.Get(kind, name)
}

// instanceFields are argo.add's and argo.update's arguments; update
// leaves unset ones as they are.
type instanceFields struct {
	Kind               argo.Kind `json:"kind"`
	Name               string    `json:"name"`
	NewName            *string   `json:"new_name"`
	URL                *string   `json:"url"`
	Namespace          *string   `json:"namespace"`
	CAFile             *string   `json:"ca_file"`
	InsecureSkipVerify *bool     `json:"insecure_skip_verify"`
	SSOPort            *int      `json:"sso_port"`
	K8sContext         *string   `json:"k8s_context"`
	Tags               *[]string `json:"tags"`
	Note               *string   `json:"note"`
}

func instanceProperties(update bool) map[string]any {
	props := map[string]any{
		"kind":                 kindProperty,
		"name":                 str("a short name for the instance: letters, digits, '.', '_' and '-'"),
		"url":                  str("the server's address, e.g. https://argocd.example.com (as in the browser)"),
		"namespace":            str("Argo Workflows: the namespace used when none is given"),
		"ca_file":              str("a PEM file of CA certificates to trust for this server"),
		"insecure_skip_verify": map[string]any{"type": "boolean", "description": "do not check the server's TLS certificate (unsafe)"},
		"sso_port":             map[string]any{"type": "integer", "minimum": 1024, "maximum": 65535, "description": "Argo CD: local port for the browser login callback, default 8085 (as argocd --sso-port)"},
		"k8s_context":          str("the Coxswain cluster (k8s_contexts) this instance runs in; lets its diagnoses look at pods, nodes and their SSH hosts"),
		"tags":                 tagsProperty,
		"note":                 str("shown to agents with every result on this instance"),
	}
	if update {
		props["new_name"] = str("rename the instance (its login is kept)")
		props["tags"] = map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "replace all tags, key=value"}
	}
	return props
}

func (p *Provider) apply(f instanceFields, i argo.Instance) (argo.Instance, error) {
	i, err := f.apply(i)
	if err == nil && i.K8sContext != "" && p.K8s != nil && !p.K8s.HasContext(i.K8sContext) {
		err = fmt.Errorf("k8s_context %q is not a cluster in Coxswain (see k8s_contexts)", i.K8sContext)
	}
	return i, err
}

func (f instanceFields) apply(i argo.Instance) (argo.Instance, error) {
	if f.URL != nil {
		i.URL = *f.URL
	}
	if f.Namespace != nil {
		i.Namespace = *f.Namespace
	}
	if f.CAFile != nil {
		i.CAFile = strings.TrimSpace(*f.CAFile)
	}
	if f.InsecureSkipVerify != nil {
		i.InsecureSkipVerify = *f.InsecureSkipVerify
	}
	if f.SSOPort != nil {
		i.SSOPort = *f.SSOPort
	}
	if f.K8sContext != nil {
		i.K8sContext = *f.K8sContext
	}
	if f.Tags != nil {
		tags, err := parseTags(*f.Tags)
		if err != nil {
			return i, err
		}
		i.Tags = tags
	}
	if f.Note != nil {
		i.Note = *f.Note
	}
	if i.Kind == argo.Workflows && strings.TrimSpace(i.Namespace) == "" {
		return i, errors.New("an Argo Workflows instance needs a namespace (the one used when none is given)")
	}
	err := i.Normalize()
	return i, err
}

func (p *Provider) add() *action.Action {
	schema := map[string]any{"type": "object", "properties": instanceProperties(false), "required": []string{"kind", "name", "url"}}
	plan := func(raw json.RawMessage) (argo.Instance, error) {
		var in instanceFields
		if err := decode(raw, schema, &in); err != nil {
			return argo.Instance{}, err
		}
		i, err := p.apply(in, argo.Instance{Kind: in.Kind, Name: in.Name})
		if err != nil {
			return i, err
		}
		if _, err := p.Sessions.Store.Get(i.Kind, i.Name); err == nil {
			return i, fmt.Errorf("%s instance %q already exists (use argo_update)", kindName(i.Kind), i.Name)
		}
		return i, nil
	}
	return &action.Action{
		ID:      "argo.add",
		Summary: "Add an Argo CD or Argo Workflows instance",
		Description: "Adds a server by URL; then log in to it (argocd_login_methods / argowf_login_methods list the ways). " +
			"Settings are stored in ~/.coxswain/argo.json; credentials only in the macOS keychain.",
		Input:       schema,
		Positional:  []string{"kind", "name", "url"},
		Annotations: action.Annotations{},
		Label: func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
			i, err := plan(raw)
			if err != nil {
				return action.Assessment{}, err
			}
			return action.Assessment{Risk: riskOfChange(argo.Instance{}, i), Targets: []action.Target{target(i, "")},
				Preview: fmt.Sprintf("add %s %s (%s)", kindName(i.Kind), i.Name, i.URL)}, nil
		},
		Run: func(_ context.Context, raw json.RawMessage) (any, error) {
			i, err := plan(raw)
			if err != nil {
				return nil, err
			}
			err = p.Sessions.Store.Update(func(list []argo.Instance) ([]argo.Instance, error) { return append(list, i), nil })
			if err != nil {
				return nil, err
			}
			return p.view(i), nil
		},
	}
}

func (p *Provider) update() *action.Action {
	schema := map[string]any{"type": "object", "properties": instanceProperties(true), "required": []string{"kind", "name"}}
	plan := func(raw json.RawMessage) (old, n argo.Instance, err error) {
		var in instanceFields
		if err := decode(raw, schema, &in); err != nil {
			return old, n, err
		}
		if old, err = p.Sessions.Store.Get(in.Kind, in.Name); err != nil {
			return old, n, err
		}
		n, err = p.apply(in, old)
		if in.NewName != nil {
			n.Name = *in.NewName
		}
		if err == nil {
			err = n.Normalize()
		}
		return old, n, err
	}
	return &action.Action{
		ID:          "argo.update",
		Summary:     "Change an Argo instance's settings, tags or note",
		Description: "Unset fields stay as they are; tags replaces all tags. Renaming keeps the login.",
		Input:       schema,
		Positional:  []string{"kind", "name"},
		Annotations: action.Annotations{Idempotent: true},
		Label: func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
			old, n, err := plan(raw)
			if err != nil {
				return action.Assessment{}, err
			}
			return action.Assessment{Risk: riskOfChange(old, n), Targets: []action.Target{target(old, ""), target(n, "")},
				Preview: fmt.Sprintf("update %s %s", kindName(n.Kind), old.Name)}, nil
		},
		Run: func(_ context.Context, raw json.RawMessage) (any, error) {
			old, n, err := plan(raw)
			if err != nil {
				return nil, err
			}
			if n.Name != old.Name {
				if _, err := p.Sessions.Store.Get(n.Kind, n.Name); err == nil {
					return nil, fmt.Errorf("%s instance %q already exists", kindName(n.Kind), n.Name)
				}
			}
			err = p.Sessions.Store.Update(func(list []argo.Instance) ([]argo.Instance, error) {
				for k := range list {
					if list[k].ID() == old.ID() {
						list[k] = n
						return list, nil
					}
				}
				return nil, fmt.Errorf("%s instance %q was removed meanwhile", kindName(old.Kind), old.Name)
			})
			if err != nil {
				return nil, err
			}
			if n.Name != old.Name {
				if err := p.Sessions.Rename(old, n); err != nil {
					return nil, fmt.Errorf("renamed, but moving its login failed (log in again): %w", err)
				}
			}
			return p.view(n), nil
		},
	}
}

func (p *Provider) remove() *action.Action {
	schema := map[string]any{"type": "object", "properties": map[string]any{"kind": kindProperty, "name": instanceProperty},
		"required": []string{"kind", "name"}}
	return &action.Action{
		ID:          "argo.remove",
		Summary:     "Remove an Argo instance and its login",
		Description: "Removes the instance from Coxswain and deletes its credential from the keychain. Nothing changes on the server.",
		Input:       schema,
		Positional:  []string{"kind", "name"},
		Annotations: action.Annotations{Destructive: true, Idempotent: true},
		Label: func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
			var in instanceFields
			if err := decode(raw, schema, &in); err != nil {
				return action.Assessment{}, err
			}
			i, err := p.Sessions.Store.Get(in.Kind, in.Name)
			if err != nil {
				return action.Assessment{}, err
			}
			return action.Assessment{Risk: action.Destructive, Targets: []action.Target{target(i, "")},
				Preview: fmt.Sprintf("remove %s %s (%s)", kindName(i.Kind), i.Name, i.URL)}, nil
		},
		Run: func(_ context.Context, raw json.RawMessage) (any, error) {
			var in instanceFields
			if err := action.Decode(raw, &in); err != nil {
				return nil, err
			}
			i, err := p.Sessions.Store.Get(in.Kind, in.Name)
			if err != nil {
				return nil, err
			}
			p.Sessions.CancelLogin(i)
			err = p.Sessions.Store.Update(func(list []argo.Instance) ([]argo.Instance, error) {
				out := list[:0]
				for _, x := range list {
					if x.ID() != i.ID() {
						out = append(out, x)
					}
				}
				return out, nil
			})
			if err != nil {
				return nil, err
			}
			return map[string]any{"removed": i.ID()}, p.Sessions.Logout(i)
		},
	}
}

// Export is the shareable form of the instances: settings, no credentials.
type Export struct {
	Coxswain  string          `json:"coxswain_argo_instances"`
	Instances []argo.Instance `json:"instances"`
}

func (p *Provider) export() *action.Action {
	schema := map[string]any{"type": "object", "properties": map[string]any{"kind": kindProperty}}
	return &action.Action{
		ID:      "argo.export",
		Summary: "Export Argo instances (no credentials) to share",
		Description: "Returns the instances' settings, tags and notes as JSON for argo_import on another Mac. " +
			"Credentials are never exported; whoever imports logs in themselves.",
		Input:       schema,
		Annotations: action.Annotations{ReadOnly: true, Idempotent: true},
		Label: func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
			return action.Assessment{Risk: action.Read, Preview: "export Argo instances"}, decode(raw, schema, &instanceFields{})
		},
		Run: func(_ context.Context, raw json.RawMessage) (any, error) {
			var in instanceFields
			if err := action.Decode(raw, &in); err != nil {
				return nil, err
			}
			list, err := p.Sessions.Store.List(in.Kind)
			if list == nil {
				list = []argo.Instance{}
			}
			return Export{Coxswain: "1", Instances: list}, err
		},
	}
}

// ImportResult says what argo.import did.
type ImportResult struct {
	Added   []string `json:"added"`
	Skipped []string `json:"skipped,omitempty"` // already there by that name
}

func (p *Provider) importInstances() *action.Action {
	schema := map[string]any{"type": "object", "properties": map[string]any{
		"json": str("the output of argo_export"),
	}, "required": []string{"json"}}
	plan := func(raw json.RawMessage) ([]argo.Instance, error) {
		var in struct {
			JSON string `json:"json"`
		}
		if err := decode(raw, schema, &in); err != nil {
			return nil, err
		}
		var ex Export
		if err := json.Unmarshal([]byte(in.JSON), &ex); err != nil || ex.Coxswain == "" {
			return nil, errors.New("json is not the output of argo export")
		}
		for k := range ex.Instances {
			if err := ex.Instances[k].Normalize(); err != nil {
				return nil, fmt.Errorf("instance %q: %w", ex.Instances[k].Name, err)
			}
		}
		return ex.Instances, nil
	}
	return &action.Action{
		ID:          "argo.import",
		Summary:     "Import Argo instances exported with argo export",
		Description: "Adds the instances that are not configured yet (by kind and name); existing ones are left as they are. Log in to each afterwards.",
		Input:       schema,
		Annotations: action.Annotations{},
		Label: func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
			list, err := plan(raw)
			if err != nil {
				return action.Assessment{}, err
			}
			as := action.Assessment{Risk: action.Write, Preview: fmt.Sprintf("import %d Argo instances", len(list))}
			for _, i := range list {
				as.Targets = append(as.Targets, target(i, ""))
				if r := riskOfChange(argo.Instance{}, i); r > as.Risk {
					as.Risk = r
				}
			}
			return as, nil
		},
		Run: func(_ context.Context, raw json.RawMessage) (any, error) {
			list, err := plan(raw)
			if err != nil {
				return nil, err
			}
			res := ImportResult{Added: []string{}}
			err = p.Sessions.Store.Update(func(cur []argo.Instance) ([]argo.Instance, error) {
				have := map[string]bool{}
				for _, i := range cur {
					have[i.ID()] = true
				}
				for _, i := range list {
					if have[i.ID()] {
						res.Skipped = append(res.Skipped, i.ID())
						continue
					}
					have[i.ID()] = true
					cur = append(cur, i)
					res.Added = append(res.Added, i.ID())
				}
				return cur, nil
			})
			return res, err
		},
	}
}

func (p *Provider) instances(k argo.Kind) *action.Action {
	schema := map[string]any{"type": "object", "properties": map[string]any{}}
	return &action.Action{
		ID:      string(k) + ".instances",
		Summary: "List " + kindName(k) + " instances and whether you are logged in",
		Description: "Lists the configured " + kindName(k) + " servers with tags, notes (respect them) and login state " +
			"(method, identity, expiry). Credentials are never shown.",
		Input:       schema,
		Annotations: action.Annotations{ReadOnly: true, Idempotent: true},
		Label: func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
			return action.Assessment{Risk: action.Read, Preview: "list " + kindName(k) + " instances"}, action.ExactKeys(raw, schema)
		},
		Run: func(context.Context, json.RawMessage) (any, error) {
			list, err := p.Sessions.Store.List(k)
			out := []InstanceView{}
			for _, i := range list {
				out = append(out, p.view(i))
			}
			return out, err
		},
	}
}

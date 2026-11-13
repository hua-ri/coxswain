package k8s

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	kube "github.com/hua-ri/coxswain/internal/k8s"
	"k8s.io/client-go/tools/clientcmd"
)

// clusterInput is what the k8s.clusters.* actions take. The kubeconfig
// carries credentials: it is never logged (see redactKubeconfig).
type clusterInput struct {
	Kubeconfig string   `json:"kubeconfig"`
	Path       string   `json:"path"`
	Context    string   `json:"context"`
	Name       string   `json:"name"`
	Tags       []string `json:"tags"`
	Note       string   `json:"note"`
}

func (in clusterInput) source() kube.ClusterSource {
	return kube.ClusterSource{YAML: []byte(in.Kubeconfig), Path: in.Path}
}

var (
	sourceProperties = map[string]any{
		"kubeconfig": map[string]any{"type": "string", "description": "kubeconfig YAML (or give path)"},
		"path":       map[string]any{"type": "string", "description": "a kubeconfig file to read (it is only read)"},
		"context":    map[string]any{"type": "string", "description": "the context to use; needed if there are several"},
	}
	inspectSchema = map[string]any{"type": "object", "properties": sourceProperties}
	probeSchema   = inspectSchema
	addSchema     = map[string]any{"type": "object", "properties": withProps(sourceProperties, map[string]any{
		"name": map[string]any{"type": "string", "description": "name of the context in Coxswain; default its own (rename it if your kubeconfig has one by that name)"},
		"tags": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "key=value; env=prod marks production"},
		"note": map[string]any{"type": "string", "description": "shown to agents with every result on this cluster"},
	})}
)

func withProps(a, b map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// redactKubeconfig records a kubeconfig as its size, hash and context
// names, never its contents.
func redactKubeconfig(schema map[string]any) func(json.RawMessage) json.RawMessage {
	return func(raw json.RawMessage) json.RawMessage {
		if action.ExactKeys(raw, schema) != nil {
			return json.RawMessage(`{"redacted":"arguments with unknown keys"}`)
		}
		var m map[string]json.RawMessage
		if json.Unmarshal(raw, &m) != nil {
			return json.RawMessage(`{"redacted":true}`)
		}
		if c, ok := m["kubeconfig"]; ok {
			var text string
			if json.Unmarshal(c, &text) != nil {
				text = string(c)
			}
			sum := sha256.Sum256([]byte(text))
			red := map[string]any{"bytes": len(text), "sha256": hex.EncodeToString(sum[:])}
			if cfg, err := clientcmd.Load([]byte(text)); err == nil {
				names := []string{}
				for n := range cfg.Contexts {
					names = append(names, n)
				}
				sort.Strings(names)
				red["contexts"] = names
			}
			m["kubeconfig"], _ = json.Marshal(red)
		}
		out, err := json.Marshal(m)
		if err != nil {
			return json.RawMessage(`{"redacted":true}`)
		}
		return out
	}
}

const clusterHint = "add the cluster in cox desktop (Clusters → +) or with `cox k8s clusters add`"

// clusterLabel checks the caller and the arguments of a k8s.clusters.*
// action.
func clusterLabel(ctx context.Context, raw json.RawMessage, schema map[string]any, what string) (clusterInput, error) {
	var in clusterInput
	if err := action.HumanOnly(ctx, what, clusterHint); err != nil {
		return in, err
	}
	if err := action.ExactKeys(raw, schema); err != nil {
		return in, err
	}
	err := action.Decode(raw, &in)
	return in, err
}

// InspectOutput is the result of k8s.clusters.inspect.
type InspectOutput struct {
	Contexts []kube.Candidate `json:"contexts"`
}

func (p *Provider) clustersInspect() *action.Action {
	return &action.Action{
		ID:      "k8s.clusters.inspect",
		Summary: "List the contexts of a kubeconfig before adding it (you only)",
		Description: "For the user only; agents are refused. Reads a kubeconfig (pasted or a file) and lists its contexts, " +
			"how they log in, and whether the name is taken. Nothing is stored or contacted.",
		Input:       inspectSchema,
		Annotations: action.Annotations{ReadOnly: true, Idempotent: true},
		Redact:      redactKubeconfig(inspectSchema),
		Label: func(ctx context.Context, raw json.RawMessage) (action.Assessment, error) {
			if _, err := clusterLabel(ctx, raw, inspectSchema, "reading credentials for a new cluster"); err != nil {
				return action.Assessment{}, err
			}
			return action.Assessment{Risk: action.Read, Preview: "inspect a kubeconfig"}, nil
		},
		Run: func(_ context.Context, raw json.RawMessage) (any, error) {
			var in clusterInput
			if err := action.Decode(raw, &in); err != nil {
				return nil, err
			}
			list, err := p.Clusters.Inspect(in.source())
			return InspectOutput{Contexts: list}, err
		},
	}
}

// ProbeOutput is the result of k8s.clusters.test.
type ProbeOutput struct {
	Context    string `json:"context"`
	Version    string `json:"version"`
	Platform   string `json:"platform,omitempty"`
	DurationMs int64  `json:"duration_ms"`
}

func (p *Provider) clustersTest() *action.Action {
	return &action.Action{
		ID:      "k8s.clusters.test",
		Summary: "Check a kubeconfig context before adding it (you only)",
		Description: "For the user only; agents are refused. Connects with one context of a kubeconfig (pasted or a file), " +
			"running its credential plugin if it has one, and returns the server version. Nothing is stored.",
		Input:       probeSchema,
		Annotations: action.Annotations{ReadOnly: true, Idempotent: true, OpenWorld: true},
		Redact:      redactKubeconfig(probeSchema),
		Label: func(ctx context.Context, raw json.RawMessage) (action.Assessment, error) {
			in, err := clusterLabel(ctx, raw, probeSchema, "testing credentials for a new cluster")
			if err != nil {
				return action.Assessment{}, err
			}
			return action.Assessment{Risk: action.Read, Preview: "test kubeconfig context " + in.Context}, nil
		},
		Run: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in clusterInput
			if err := action.Decode(raw, &in); err != nil {
				return nil, err
			}
			start := time.Now()
			ctx, cancel := context.WithTimeout(ctx, kube.RequestTimeout+kube.DefaultCredentialTimeout)
			defer cancel()
			v, err := p.Clusters.Probe(ctx, in.source(), in.Context)
			if err != nil {
				return nil, err
			}
			name := in.Context
			if name == "" {
				if list, err := p.Clusters.Inspect(in.source()); err == nil && len(list) == 1 {
					name = list[0].Name
				}
			}
			return ProbeOutput{Context: name, Version: v.GitVersion, Platform: v.Platform, DurationMs: time.Since(start).Milliseconds()}, nil
		},
	}
}

func (p *Provider) clustersAdd() *action.Action {
	return &action.Action{
		ID:      "k8s.clusters.add",
		Summary: "Add a Kubernetes cluster from a kubeconfig (you only)",
		Description: "For the user only; agents are refused. Stores one context of a kubeconfig (pasted or a file) in " +
			"~/.coxswain/kubeconfigs, with its cluster and credentials. Your own kubeconfig (~/.kube/config, $KUBECONFIG) " +
			"is never written; a context name it already uses is refused (give another name).",
		Input:       addSchema,
		Annotations: action.Annotations{Destructive: true},
		Redact:      redactKubeconfig(addSchema),
		Label: func(ctx context.Context, raw json.RawMessage) (action.Assessment, error) {
			in, err := clusterLabel(ctx, raw, addSchema, "adding a cluster's credentials")
			if err != nil {
				return action.Assessment{}, err
			}
			tags, err := parseTags(in.Tags)
			if err != nil {
				return action.Assessment{}, err
			}
			name := in.Name
			if name == "" {
				name = in.Context
			}
			risk := action.Write
			for k := range tags {
				if k == "env" || k == "ai" || strings.HasPrefix(k, "ai.") {
					risk = action.Destructive
				}
			}
			if in.Note != "" {
				risk = action.Destructive
			}
			return action.Assessment{Risk: risk, Targets: []action.Target{{Kind: "k8s", ID: "k8s:" + name, Tags: tags}},
				Preview: "add cluster " + name}, nil
		},
		Run: func(_ context.Context, raw json.RawMessage) (any, error) {
			var in clusterInput
			if err := action.Decode(raw, &in); err != nil {
				return nil, err
			}
			tags, err := parseTags(in.Tags)
			if err != nil {
				return nil, err
			}
			ci, err := p.Clusters.AddCluster(in.source(), in.Context, in.Name)
			if err != nil {
				return nil, err
			}
			if len(tags) > 0 || in.Note != "" {
				err := p.Clusters.Meta.Update(func(m map[string]kube.Meta) error {
					m[ci.Name] = kube.Meta{Tags: tags, Note: strings.TrimSpace(in.Note)}
					return nil
				})
				if err != nil {
					return nil, err
				}
			}
			return p.Clusters.Context(ci.Name)
		},
	}
}

func (p *Provider) clustersRemove() *action.Action {
	schema := map[string]any{"type": "object", "properties": map[string]any{"context": contextProperty}, "required": []string{"context"}}
	return &action.Action{
		ID:      "k8s.clusters.remove",
		Summary: "Remove a cluster added in Coxswain (you only)",
		Description: "For the user only; agents are refused. Deletes a cluster added with k8s.clusters.add (its stored " +
			"kubeconfig, tags and note). Contexts of your own kubeconfig are read-only and cannot be removed here.",
		Input:       schema,
		Positional:  []string{"context"},
		Annotations: action.Annotations{Destructive: true, Idempotent: true},
		Label: func(ctx context.Context, raw json.RawMessage) (action.Assessment, error) {
			if err := action.HumanOnly(ctx, "removing a cluster", "remove it in cox desktop or with `cox k8s clusters remove`"); err != nil {
				return action.Assessment{}, err
			}
			var in clusterInput
			if err := action.Decode(raw, &in); err != nil {
				return action.Assessment{}, err
			}
			ci, err := p.Clusters.Context(in.Context)
			if err != nil {
				return action.Assessment{}, err
			}
			if ci.Source != kube.SourceCoxswain {
				return action.Assessment{}, fmt.Errorf("context %q comes from your kubeconfig, which is read-only for Coxswain; "+
					"only clusters added in Coxswain can be removed here", in.Context)
			}
			return action.Assessment{Risk: action.Destructive, Targets: []action.Target{p.Clusters.Target(in.Context, "")},
				Preview: "remove cluster " + in.Context + " (" + ci.Server + ")"}, nil
		},
		Run: func(_ context.Context, raw json.RawMessage) (any, error) {
			var in clusterInput
			if err := action.Decode(raw, &in); err != nil {
				return nil, err
			}
			return map[string]any{"removed": in.Context}, p.Clusters.RemoveCluster(in.Context)
		},
	}
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

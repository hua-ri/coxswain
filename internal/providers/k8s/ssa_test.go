package k8s

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/hua-ri/coxswain/internal/engine"
	"github.com/hua-ri/coxswain/internal/k8s/k8stest"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const webPath = "/apis/apps/v1/namespaces/shop/deployments/web"

// fieldManagers returns the fieldManager of each PATCH to path, in order.
func fieldManagers(api *k8stest.APIServer, path string) []string {
	var out []string
	for _, r := range api.Requests() {
		if r.Method == "PATCH" && r.Path == path {
			q, _ := url.ParseQuery(r.Query)
			out = append(out, q.Get("fieldManager"))
		}
	}
	return out
}

// TestScaleThenApply reproduces the acceptance bug: scale (an update)
// and apply (server-side apply) used the same manager name, so applying
// replicas after scaling conflicted with cox itself. Imperative changes
// are now cox-ops; a conflict names the field, its manager and what to
// do instead, and is never forced.
func TestScaleThenApply(t *testing.T) {
	api := k8stest.NewAPIServer(t)
	api.Seed(webPath, map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "web", "namespace": "shop", "uid": "u1", "generation": int64(1)},
		"spec": map[string]any{"replicas": int64(2), "selector": map[string]any{"matchLabels": map[string]any{"app": "web"}},
			"template": map[string]any{"metadata": map[string]any{"labels": map[string]any{"app": "web"}},
				"spec": map[string]any{"containers": []any{map[string]any{"name": "app", "image": "web:1"}}}}},
	})
	f := setupAPI(t, api)
	f.invoke(t, codex, "k8s.scale", args("context", "fake", "namespace", "shop", "kind", "deployment", "name", "web", "replicas", 3))
	f.invoke(t, codex, "k8s.rollout", args("context", "fake", "namespace", "shop", "kind", "deployment", "name", "web", "action", "restart"))
	if got := strings.Join(fieldManagers(api, webPath), ","); got != "cox-ops,cox-ops" {
		t.Fatalf("imperative changes' field managers: %s", got)
	}

	manifest := "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: web, namespace: shop}\nspec: {replicas: 2}\n"
	out := f.e.Invoke(context.Background(), codex, "k8s.apply", args("context", "fake", "manifest", manifest, "dry_run", true))
	if out.Status != engine.StatusError {
		t.Fatalf("no conflict: %+v", out)
	}
	for _, want := range []string{"Deployment/web", ".spec.replicas", `"cox-ops"`, "k8s_scale", "not forced", "ask the user"} {
		if !strings.Contains(out.Error, want) {
			t.Errorf("error lacks %q:\n%s", want, out.Error)
		}
	}
	ms := fieldManagers(api, webPath)
	if ms[len(ms)-1] != "cox" {
		t.Fatalf("apply's field manager: %v", ms)
	}

	// Applying what is already there, or fields nobody else owns, works.
	same := "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: web, namespace: shop}\nspec: {replicas: 3, minReadySeconds: 5}\n"
	res := f.invoke(t, codex, "k8s.apply", args("context", "fake", "manifest", same)).(ApplyOutput)
	if !res.Objects[0].Applied {
		t.Fatalf("%+v", res)
	}
}

// TestForceConflicts: an earlier build left replicas owned by "cox"
// (an update). An agent gets the conflicts (structured, with hints) and
// cannot force; the user can take the fields over.
func TestForceConflicts(t *testing.T) {
	api := k8stest.NewAPIServer(t)
	api.Seed(webPath, map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "web", "namespace": "shop", "uid": "u1"},
		"spec":     map[string]any{"replicas": int64(2)},
	})
	f := setupAPI(t, api)
	c, err := f.p.Clusters.Client("fake")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Dynamic.Resource(deployGVR).Namespace("shop").Patch(context.Background(), "web", types.MergePatchType,
		[]byte(`{"spec":{"replicas":3}}`), metav1.PatchOptions{FieldManager: "cox"}); err != nil {
		t.Fatal(err)
	}
	manifest := "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: web, namespace: shop}\nspec: {replicas: 2}\n"

	out := f.e.Invoke(context.Background(), codex, "k8s.apply", args("context", "fake", "manifest", manifest, "dry_run", true))
	b, _ := json.Marshal(out.Details)
	if out.Status != engine.StatusError || !strings.Contains(out.Error, `"cox"`) || !strings.Contains(out.Error, "k8s_scale") ||
		string(b) != `{"conflicts":[{"object":"Deployment/web","field":".spec.replicas","manager":"cox","operation":"Update","hint":"k8s_scale"}]}` {
		t.Fatalf("%+v\n%s", out, b)
	}
	out = f.e.Invoke(context.Background(), codex, "k8s.apply", args("context", "fake", "manifest", manifest, "force_conflicts", true))
	if out.Status != engine.StatusError || !strings.Contains(out.Error, "ask the user") {
		t.Fatalf("agent forced: %+v", out)
	}

	res := f.invoke(t, desktop, "k8s.apply", args("context", "fake", "manifest", manifest, "dry_run", true, "force_conflicts", true)).(ApplyOutput)
	if res.Objects[0].Diff == nil || !strings.Contains(res.Objects[0].Diff.Text, "+  replicas: 2") {
		t.Fatalf("forced dry run: %+v", res.Objects[0])
	}
	if ev := f.requested(t, "k8s.apply"); ev.Risk != "destructive" {
		t.Fatalf("forced dry run risk %s", ev.Risk)
	}
	res = f.invoke(t, desktop, "k8s.apply", args("context", "fake", "manifest", manifest, "force_conflicts", true)).(ApplyOutput)
	if !res.Objects[0].Applied {
		t.Fatalf("%+v", res)
	}
	var forced []string
	for _, r := range api.Requests() {
		if r.Method == "PATCH" && strings.Contains(r.ContentType, "apply-patch") {
			q, _ := url.ParseQuery(r.Query)
			forced = append(forced, q.Get("force")+"/"+q.Get("dryRun"))
		}
	}
	if strings.Join(forced, ",") != "false/All,true/All,true/All,true/" {
		t.Fatalf("applies (force/dryRun): %v", forced)
	}
	// The agent's next apply of the same value is no conflict any more.
	f.invoke(t, codex, "k8s.apply", args("context", "fake", "manifest", manifest, "dry_run", true))
}

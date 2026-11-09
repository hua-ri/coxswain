package k8s

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/hua-ri/coxswain/internal/engine"
	"github.com/hua-ri/coxswain/internal/k8s/k8stest"
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

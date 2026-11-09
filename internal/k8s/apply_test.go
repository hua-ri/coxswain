package k8s_test

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/hua-ri/coxswain/internal/k8s"
	"github.com/hua-ri/coxswain/internal/k8s/k8stest"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestDynamicApplier: apply is server-side apply as field manager cox,
// never forced; a dry run asks the server for dryRun=All.
func TestDynamicApplier(t *testing.T) {
	api := k8stest.NewAPIServer(t)
	c, err := apiKubeconfig(t, api).Client("fake")
	if err != nil {
		t.Fatal(err)
	}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "web", "namespace": "shop"},
		"spec":     map[string]any{"replicas": int64(2)},
	}}
	res := k8s.Resource{GVR: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, Kind: "Deployment", Namespaced: true}
	for _, dry := range []bool{true, false} {
		out, err := c.Apply.Apply(context.Background(), res, "shop", obj, dry)
		if err != nil {
			t.Fatal(err)
		}
		if out.GetResourceVersion() != "1" || out.GetNamespace() != "shop" {
			t.Fatalf("result %v", out.Object)
		}
		reqs := api.Requests()
		last := reqs[len(reqs)-1]
		q, _ := url.ParseQuery(last.Query)
		if last.Method != "PATCH" || last.Path != "/apis/apps/v1/namespaces/shop/deployments/web" ||
			last.ContentType != "application/apply-patch+yaml" || q.Get("fieldManager") != "cox" || q.Get("force") == "true" ||
			(q.Get("dryRun") == "All") != dry {
			t.Fatalf("dry %v: request %+v", dry, last)
		}
		if !strings.Contains(last.Body, `"replicas":2`) {
			t.Fatalf("body %s", last.Body)
		}
	}
}

package k8s_test

import (
	"strings"
	"testing"
	"time"

	"github.com/hua-ri/coxswain/internal/k8s"
	"github.com/hua-ri/coxswain/internal/k8s/k8stest"
	fakediscovery "k8s.io/client-go/discovery/fake"
	clienttesting "k8s.io/client-go/testing"
)

func resolver(t *testing.T) (*k8s.Resolver, *fakediscovery.FakeDiscovery) {
	disc := &fakediscovery.FakeDiscovery{Fake: &clienttesting.Fake{}}
	disc.Resources = k8stest.Resources()
	return k8s.NewResolver(disc), disc
}

func TestResolveKinds(t *testing.T) {
	r, _ := resolver(t)
	for in, want := range map[string]string{
		"po":                    "/v1/pods Pod ns",
		"pods":                  "/v1/pods Pod ns",
		"Pod":                   "/v1/pods Pod ns",
		"pod":                   "/v1/pods Pod ns",
		"deploy":                "apps/v1/deployments Deployment ns",
		"Deployments":           "apps/v1/deployments Deployment ns",
		"deployments.apps":      "apps/v1/deployments Deployment ns",
		"deployment.apps":       "apps/v1/deployments Deployment ns",
		"apps/v1/Deployment":    "apps/v1/deployments Deployment ns",
		"v1/Pod":                "/v1/pods Pod ns",
		"no":                    "/v1/nodes Node cluster",
		"events":                "/v1/events Event ns",
		"events.events.k8s.io":  "events.k8s.io/v1/events Event ns",
		"crd":                   "apiextensions.k8s.io/v1/customresourcedefinitions CustomResourceDefinition cluster",
		"pv":                    "/v1/persistentvolumes PersistentVolume cluster",
		"cronjobs":              "batch/v1/cronjobs CronJob ns",
		"batch/v1/Job":          "batch/v1/jobs Job ns",
		"widgets.example.com":   "example.com/v1beta1/widgets Widget ns",
		"example.com/v1/Widget": "example.com/v1/widgets Widget ns",
	} {
		res, err := r.Resolve(in)
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		scope := "cluster"
		if res.Namespaced {
			scope = "ns"
		}
		got := res.GVR.Group + "/" + res.GVR.Version + "/" + res.GVR.Resource + " " + res.Kind + " " + scope
		if got != want {
			t.Errorf("%s: got %q, want %q", in, got, want)
		}
	}
	if _, err := r.Resolve("gizmo"); err == nil || !strings.Contains(err.Error(), "gizmo") {
		t.Fatalf("unknown kind: %v", err)
	}
	if _, err := r.Resolve("pods/log"); err == nil {
		t.Fatal("a subresource resolved")
	}
}

func TestResolveCachesDiscovery(t *testing.T) {
	r, disc := resolver(t)
	now := time.Unix(0, 0)
	r.Now = func() time.Time { return now }
	r.Resolve("pods")
	calls := len(disc.Actions())
	r.Resolve("deploy")
	if len(disc.Actions()) != calls {
		t.Fatal("discovery not cached")
	}
	now = now.Add(11 * time.Minute)
	r.Resolve("pods")
	if len(disc.Actions()) == calls {
		t.Fatal("discovery cached past 10 minutes")
	}
	// An unknown kind refreshes once (a CRD may have been added).
	calls = len(disc.Actions())
	r.Resolve("gizmo")
	if len(disc.Actions()) == calls {
		t.Fatal("unknown kind did not refresh discovery")
	}
	if !r.HasGroup("apps") || r.HasGroup("metrics.k8s.io") {
		t.Fatal("HasGroup")
	}
}

func TestResolveGVK(t *testing.T) {
	r, _ := resolver(t)
	res, err := r.ForKind("apps/v1", "Deployment")
	if err != nil || res.GVR.Resource != "deployments" || !res.Namespaced {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := r.ForKind("v1", "Gizmo"); err == nil {
		t.Fatal("unknown GVK resolved")
	}
}

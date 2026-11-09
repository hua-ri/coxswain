package k8s

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
)

// FieldManager is who Coxswain's server-side applies are recorded as.
const FieldManager = "cox"

// Applier applies objects. Tests use a fake.
type Applier interface {
	// Apply server-side applies obj (of resource res, in namespace ns, ""
	// for cluster-scoped) as FieldManager without forcing conflicts, and
	// returns the result. With dryRun the server only reports the result.
	Apply(ctx context.Context, res Resource, ns string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error)
}

// DynamicApplier applies through the dynamic client.
type DynamicApplier struct{ Dynamic dynamic.Interface }

func (a *DynamicApplier) Apply(ctx context.Context, res Resource, ns string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	var ri dynamic.ResourceInterface = a.Dynamic.Resource(res.GVR)
	if res.Namespaced {
		ri = a.Dynamic.Resource(res.GVR).Namespace(ns)
	}
	opts := metav1.ApplyOptions{FieldManager: FieldManager, Force: false}
	if dryRun {
		opts.DryRun = []string{metav1.DryRunAll}
	}
	out, err := ri.Apply(ctx, obj.GetName(), obj, opts)
	return out, Explain(err)
}

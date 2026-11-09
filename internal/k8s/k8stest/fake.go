package k8stest

import (
	"strings"

	"github.com/hua-ri/coxswain/internal/k8s"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
)

func res(name, kind string, namespaced bool, short ...string) metav1.APIResource {
	return metav1.APIResource{Name: name, SingularName: strings.ToLower(kind), Kind: kind, Namespaced: namespaced, ShortNames: short,
		Verbs: metav1.Verbs{"get", "list", "watch", "create", "update", "patch", "delete"}}
}

// Resources is discovery for a typical cluster, plus a CRD example.com
// Widget served as v1beta1 (preferred) and v1.
func Resources() []*metav1.APIResourceList {
	return []*metav1.APIResourceList{
		{GroupVersion: "v1", APIResources: []metav1.APIResource{
			res("pods", "Pod", true, "po"), {Name: "pods/log", Kind: "Pod", Namespaced: true},
			res("services", "Service", true, "svc"), res("nodes", "Node", false, "no"),
			res("namespaces", "Namespace", false, "ns"), res("secrets", "Secret", true),
			res("configmaps", "ConfigMap", true, "cm"), res("events", "Event", true, "ev"),
			res("persistentvolumes", "PersistentVolume", false, "pv"),
			res("persistentvolumeclaims", "PersistentVolumeClaim", true, "pvc"),
		}},
		{GroupVersion: "apps/v1", APIResources: []metav1.APIResource{
			res("deployments", "Deployment", true, "deploy"), res("statefulsets", "StatefulSet", true, "sts"),
			res("replicasets", "ReplicaSet", true, "rs"), res("daemonsets", "DaemonSet", true, "ds"),
		}},
		{GroupVersion: "batch/v1", APIResources: []metav1.APIResource{
			res("jobs", "Job", true), res("cronjobs", "CronJob", true, "cj"),
		}},
		{GroupVersion: "events.k8s.io/v1", APIResources: []metav1.APIResource{res("events", "Event", true, "ev")}},
		{GroupVersion: "apiextensions.k8s.io/v1", APIResources: []metav1.APIResource{
			res("customresourcedefinitions", "CustomResourceDefinition", false, "crd", "crds"),
		}},
		{GroupVersion: "example.com/v1beta1", APIResources: []metav1.APIResource{res("widgets", "Widget", true)}},
		{GroupVersion: "example.com/v1", APIResources: []metav1.APIResource{res("widgets", "Widget", true)}},
	}
}

// MetricsResources is discovery for metrics.k8s.io.
func MetricsResources() *metav1.APIResourceList {
	return &metav1.APIResourceList{GroupVersion: "metrics.k8s.io/v1beta1", APIResources: []metav1.APIResource{
		res("pods", "PodMetrics", true), res("nodes", "NodeMetrics", false),
	}}
}

// Fake is a k8s.Client on client-go fakes, with both the typed and the
// dynamic client seeded with the same objects.
type Fake struct {
	*k8s.Client
	Typed     *fake.Clientset
	Dynamic   *dynamicfake.FakeDynamicClient
	Discovery *fakediscovery.FakeDiscovery
}

// NewFake returns a fake client for context name with objs.
func NewFake(name string, objs ...runtime.Object) *Fake {
	typed := fake.NewClientset(objs...)
	disc := typed.Discovery().(*fakediscovery.FakeDiscovery)
	disc.Resources = Resources()
	dyn := dynamicfake.NewSimpleDynamicClient(scheme.Scheme, objs...)
	return &Fake{
		Client: &k8s.Client{Context: name, Typed: typed, Dynamic: dyn, Resources: k8s.NewResolver(disc)},
		Typed:  typed, Dynamic: dyn, Discovery: disc,
	}
}

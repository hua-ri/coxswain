package k8stest

import (
	"context"
	"io"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/dynamic"
	"net"
	"strings"
	"sync"

	"github.com/hua-ri/coxswain/internal/k8s"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
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
	ExecCalls *FakeExec
	Applies   *FakeApplier
	Forwards  *FakeForwarder
}

// NewFake returns a fake client for context name with objs.
func NewFake(name string, objs ...runtime.Object) *Fake {
	typed := fake.NewClientset(objs...)
	disc := typed.Discovery().(*fakediscovery.FakeDiscovery)
	disc.Resources = Resources()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme.Scheme, map[schema.GroupVersionResource]string{
		{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "pods"}:  "PodMetricsList",
		{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "nodes"}: "NodeMetricsList",
		{Group: "example.com", Version: "v1beta1", Resource: "widgets"}:  "WidgetList",
	}, objs...)
	exec := &FakeExec{}
	apply := &FakeApplier{Dynamic: dyn}
	fwd := &FakeForwarder{}
	return &Fake{
		Client: &k8s.Client{Context: name, Typed: typed, Dynamic: dyn, Resources: k8s.NewResolver(disc), Exec: exec, Apply: apply,
			Forward: fwd},
		Typed: typed, Dynamic: dyn, Discovery: disc, ExecCalls: exec, Applies: apply, Forwards: fwd,
	}
}

// WithMetrics makes the fake serve metrics.k8s.io with the given
// PodMetrics and NodeMetrics.
func (f *Fake) WithMetrics(objs ...*unstructured.Unstructured) *Fake {
	f.Discovery.Resources = append(f.Discovery.Resources, MetricsResources())
	for _, o := range objs {
		res := "pods"
		if o.GetKind() == "NodeMetrics" {
			res = "nodes"
		}
		gvr := schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: res}
		if err := f.Dynamic.Tracker().Create(gvr, o, o.GetNamespace()); err != nil {
			panic(err)
		}
	}
	return f
}

// FakeExec records exec requests and answers them with Run (default:
// no output, exit 0).
type FakeExec struct {
	mu    sync.Mutex
	calls []k8s.ExecRequest
	Run   func(ctx context.Context, req k8s.ExecRequest) error
}

func (f *FakeExec) Exec(ctx context.Context, req k8s.ExecRequest) error {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	run := f.Run
	f.mu.Unlock()
	if run == nil {
		return nil
	}
	return run(ctx, req)
}

// Calls returns the requests so far.
func (f *FakeExec) Calls() []k8s.ExecRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]k8s.ExecRequest(nil), f.calls...)
}

// ApplyCall is one call of FakeApplier.
type ApplyCall struct {
	Kind, Namespace, Name string
	DryRun                bool
}

// FakeApplier imitates server-side apply on a fake dynamic client: the
// object is merged into the live one (maps merged, lists replaced); a dry
// run returns the result without storing it.
type FakeApplier struct {
	Dynamic *dynamicfake.FakeDynamicClient
	// Fail, if set, may fail a call.
	Fail func(obj *unstructured.Unstructured, dryRun bool) error

	mu    sync.Mutex
	calls []ApplyCall
}

func (a *FakeApplier) Apply(ctx context.Context, res k8s.Resource, ns string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	a.mu.Lock()
	a.calls = append(a.calls, ApplyCall{Kind: obj.GetKind(), Namespace: ns, Name: obj.GetName(), DryRun: dryRun})
	fail := a.Fail
	a.mu.Unlock()
	if fail != nil {
		if err := fail(obj, dryRun); err != nil {
			return nil, err
		}
	}
	var ri dynamic.ResourceInterface = a.Dynamic.Resource(res.GVR)
	if ns != "" {
		ri = a.Dynamic.Resource(res.GVR).Namespace(ns)
	}
	cur, err := ri.Get(ctx, obj.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		cur = nil
	} else if err != nil {
		return nil, err
	}
	out := obj.DeepCopy()
	if ns != "" {
		out.SetNamespace(ns)
	}
	if cur != nil {
		merged := cur.DeepCopy()
		merge(merged.Object, out.Object)
		out = merged
	} else {
		out.SetUID(types.UID("uid-" + obj.GetName()))
	}
	if dryRun {
		return out, nil
	}
	if cur == nil {
		return ri.Create(ctx, out, metav1.CreateOptions{})
	}
	return ri.Update(ctx, out, metav1.UpdateOptions{})
}

// Calls returns the calls so far.
func (a *FakeApplier) Calls() []ApplyCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]ApplyCall(nil), a.calls...)
}

func merge(dst, src map[string]any) {
	for k, v := range src {
		if sm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				merge(dm, sm)
				continue
			}
		}
		dst[k] = v
	}
}

// FakeForwarder imitates port-forwarding: each connection is a pipe to
// Serve (default: echo).
type FakeForwarder struct {
	// Serve answers one connection to port of pod.
	Serve func(pod string, port int, c net.Conn)

	mu       sync.Mutex
	calls    []string // namespace/pod
	sessions []*fakeSession
}

func (f *FakeForwarder) Forward(_ context.Context, namespace, pod string) (k8s.ForwardSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, namespace+"/"+pod)
	s := &fakeSession{f: f, pod: pod, done: make(chan struct{})}
	f.sessions = append(f.sessions, s)
	return s, nil
}

// Calls returns namespace/pod of every session opened.
func (f *FakeForwarder) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// Drop ends every session, like a broken connection.
func (f *FakeForwarder) Drop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.sessions {
		s.Close()
	}
}

type fakeSession struct {
	f    *FakeForwarder
	pod  string
	once sync.Once
	done chan struct{}
}

func (s *fakeSession) Dial(port int, onError func(error)) (net.Conn, error) {
	a, b := net.Pipe()
	s.f.mu.Lock()
	serve := s.f.Serve
	s.f.mu.Unlock()
	if serve == nil {
		serve = func(_ string, _ int, c net.Conn) { io.Copy(c, c); c.Close() }
	}
	go serve(s.pod, port, b)
	return a, nil
}

func (s *fakeSession) Done() <-chan struct{} { return s.done }

func (s *fakeSession) Close() error {
	s.once.Do(func() { close(s.done) })
	return nil
}

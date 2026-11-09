package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/engine"
	"github.com/hua-ri/coxswain/internal/eventlog"
	"github.com/hua-ri/coxswain/internal/inventory"
	kube "github.com/hua-ri/coxswain/internal/k8s"
	"github.com/hua-ri/coxswain/internal/k8s/k8stest"
	"github.com/hua-ri/coxswain/internal/shaper"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
)

func TestMain(m *testing.M) { k8stest.Main(m) }

var (
	codex   = action.Actor{Kind: action.Agent, Name: "codex"}
	desktop = action.Actor{Kind: action.Human, Name: "desktop"}
	now     = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
)

const testKubeconfig = `apiVersion: v1
kind: Config
current-context: dev
clusters:
- name: c
  cluster: {server: https://127.0.0.1:1}
users:
- name: u
  user: {token: t}
contexts:
- name: dev
  context: {cluster: c, user: u}
- name: prod
  context: {cluster: c, user: u, namespace: shop}
`

type fixture struct {
	e          *engine.Engine
	p          *Provider
	fakes      map[string]*k8stest.Fake
	coxHome    string
	kubeconfig string
}

// setup registers the provider on fake clusters "dev" and "prod" (tagged
// env=prod, with a note), both holding objs.
func setup(t *testing.T, objs ...runtime.Object) fixture {
	t.Helper()
	path := k8stest.Isolate(t)
	if err := os.WriteFile(path, []byte(testKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	coxHome := t.TempDir()
	kc := kube.NewKubeconfig(kube.DefaultPaths(os.Getenv("KUBECONFIG"), os.Getenv("HOME")))
	k8stest.Guard(t, kc.Paths()...)
	meta := kube.NewMetaStore(filepath.Join(coxHome, "clusters.json"))
	meta.Update(func(m map[string]kube.Meta) error {
		m["prod"] = kube.Meta{Tags: map[string]string{"env": "prod"}, Note: "no restarts during the day"}
		return nil
	})
	fakes := map[string]*k8stest.Fake{"dev": k8stest.NewFake("dev", objs...), "prod": k8stest.NewFake("prod", objs...)}
	clusters := &kube.Clusters{Kubeconfig: kc, Meta: meta, NewClient: func(name string, _ *rest.Config) (*kube.Client, error) {
		return fakes[name].Client, nil
	}}
	hosts := inventory.NewStore(filepath.Join(coxHome, "hosts.json"))
	p := &Provider{Clusters: clusters, Hosts: hosts, Artifacts: shaper.NewArtifacts(filepath.Join(coxHome, "artifacts")),
		Now: func() time.Time { return now }}
	reg := action.NewRegistry()
	p.Register(reg)
	return fixture{e: &engine.Engine{Registry: reg, Events: eventlog.Open(filepath.Join(coxHome, "events"))}, p: p,
		fakes: fakes, coxHome: coxHome, kubeconfig: path}
}

func args(kv ...any) json.RawMessage {
	m := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	b, _ := json.Marshal(m)
	return b
}

// invoke runs an action and fails the test on error.
func (f fixture) invoke(t *testing.T, who action.Actor, id string, raw json.RawMessage) any {
	t.Helper()
	out := f.e.Invoke(context.Background(), who, id, raw)
	if out.Status != engine.StatusOK {
		t.Fatalf("%s: %+v", id, out)
	}
	return out.Result
}

// requested returns the audit event for the last request of id.
func (f fixture) requested(t *testing.T, id string) eventlog.Event {
	t.Helper()
	evs, err := f.e.Events.Read(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Type == eventlog.ActionRequested && evs[i].Action == id {
			return evs[i]
		}
	}
	t.Fatalf("no request for %s", id)
	return eventlog.Event{}
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func meta(ns, name string, age time.Duration) metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID("uid-" + name), CreationTimestamp: metav1.NewTime(now.Add(-age)),
		Labels: map[string]string{"app": strings.Split(name, "-")[0]}}
}

func runningPod(ns, name, node string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: meta(ns, name, 2*time.Hour),
		Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "app", Image: "nginx:1"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{
			{Name: "app", Ready: true, RestartCount: 1, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
		}}}
}

func crashingPod(ns, name string) *corev1.Pod {
	p := runningPod(ns, name, "node-1")
	p.Status.ContainerStatuses[0] = corev1.ContainerStatus{Name: "app", RestartCount: 7,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "back-off 5m0s"}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason: "Error", ExitCode: 1, FinishedAt: metav1.NewTime(now.Add(-time.Minute))}}}
	return p
}

func secret() *corev1.Secret {
	s := &corev1.Secret{ObjectMeta: meta("default", "db-creds", time.Hour), Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"password": []byte("hunter2-supersecret"), "user": []byte("admin")}}
	s.Annotations = map[string]string{"kubectl.kubernetes.io/last-applied-configuration": `{"data":{"password":"aHVudGVyMi1zdXBlcnNlY3JldA=="}}`}
	return s
}

func deployment(ns, name string, want, ready int32) *appsv1.Deployment {
	return &appsv1.Deployment{ObjectMeta: meta(ns, name, 24*time.Hour),
		Spec: appsv1.DeploymentSpec{Replicas: &want, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "web:2"}}}}},
		Status: appsv1.DeploymentStatus{Replicas: want, ReadyReplicas: ready, UpdatedReplicas: want, AvailableReplicas: ready}}
}

func node(name, ip string, ready bool) *corev1.Node {
	st := corev1.ConditionTrue
	if !ready {
		st = corev1.ConditionFalse
	}
	n := &corev1.Node{ObjectMeta: meta("", name, 72*time.Hour),
		Status: corev1.NodeStatus{
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: st}},
			Addresses:   []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: ip}, {Type: corev1.NodeHostName, Address: name}},
			NodeInfo:    corev1.NodeSystemInfo{KubeletVersion: "v1.34.0"},
			Allocatable: corev1.ResourceList{},
		}}
	n.Labels["node-role.kubernetes.io/control-plane"] = ""
	return n
}

func event(ns, obj, kind, typ, reason, msg string, ago time.Duration) *corev1.Event {
	return &corev1.Event{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: obj + "." + reason, CreationTimestamp: metav1.NewTime(now.Add(-ago))},
		InvolvedObject: corev1.ObjectReference{Kind: kind, Name: obj, Namespace: ns},
		Type:           typ, Reason: reason, Message: msg, Count: 3, LastTimestamp: metav1.NewTime(now.Add(-ago))}
}

func TestAnnotations(t *testing.T) {
	f := setup(t)
	for id, want := range map[string]action.Annotations{
		"k8s.contexts":     {ReadOnly: true, Idempotent: true},
		"k8s.contexts.tag": {Destructive: true, Idempotent: true},
		"k8s.get":          {ReadOnly: true, Idempotent: true, OpenWorld: true},
		"k8s.describe":     {ReadOnly: true, Idempotent: true, OpenWorld: true},
		"k8s.events":       {ReadOnly: true, Idempotent: true, OpenWorld: true},
		"k8s.logs":         {ReadOnly: true, Idempotent: true, OpenWorld: true},
		"k8s.top":          {ReadOnly: true, Idempotent: true, OpenWorld: true},
		"k8s.exec":         {Destructive: true, OpenWorld: true},
		"k8s.apply":        {Destructive: true, Idempotent: true, OpenWorld: true},
		"k8s.delete":       {Destructive: true, Idempotent: true, OpenWorld: true},
		"k8s.scale":        {Destructive: true, Idempotent: true, OpenWorld: true},
		"k8s.rollout":      {Destructive: true, OpenWorld: true},
		"k8s.diagnose":     {ReadOnly: true, Idempotent: true, OpenWorld: true},
		"k8s.node_host":    {ReadOnly: true, Idempotent: true, OpenWorld: true},
		"k8s.port_forward": {Destructive: true, OpenWorld: true},
	} {
		a, ok := f.e.Registry.Get(id)
		if !ok {
			t.Fatalf("%s not registered", id)
		}
		if a.Annotations != want {
			t.Errorf("%s: %+v, want %+v", id, a.Annotations, want)
		}
	}
}

func TestContexts(t *testing.T) {
	f := setup(t)
	res := f.invoke(t, codex, "k8s.contexts", nil).(ContextsOutput)
	if res.Current != "dev" || len(res.Contexts) != 2 {
		t.Fatalf("%+v", res)
	}
	prod := res.Contexts[1]
	if prod.Name != "prod" || prod.Namespace != "shop" || prod.Tags["env"] != "prod" || prod.Note == "" {
		t.Fatalf("prod: %+v", prod)
	}
	if ev := f.requested(t, "k8s.contexts"); ev.Risk != "read" {
		t.Fatalf("risk %s", ev.Risk)
	}
}

func TestContextsTag(t *testing.T) {
	f := setup(t)
	before, _ := os.ReadFile(f.kubeconfig)
	for _, c := range []struct {
		args json.RawMessage
		risk string
	}{
		{args("context", "dev", "set", []string{"team=web"}), "write"},
		{args("context", "dev", "set", []string{"env=prod"}), "destructive"},
		{args("context", "prod", "unset", []string{"env"}), "destructive"},
		{args("context", "dev", "note", "careful"), "destructive"},
		{args("context", "dev", "note", "careful", "set", []string{"x=y"}), "write"}, // same note
	} {
		f.invoke(t, desktop, "k8s.contexts.tag", c.args)
		if ev := f.requested(t, "k8s.contexts.tag"); ev.Risk != c.risk {
			t.Fatalf("%s: risk %s, want %s", c.args, ev.Risk, c.risk)
		}
	}
	ci, err := f.p.Clusters.Context("dev")
	if err != nil || ci.Tags["env"] != "prod" || ci.Tags["team"] != "web" || ci.Note != "careful" {
		t.Fatalf("dev: %+v %v", ci, err)
	}
	if ci, _ := f.p.Clusters.Context("prod"); ci.Tags["env"] != "" {
		t.Fatalf("prod: %+v", ci)
	}
	if ev := f.requested(t, "k8s.contexts.tag"); len(ev.Targets) != 1 || ev.Targets[0].ID != "k8s:dev" || ev.Targets[0].Tags["env"] != "prod" {
		t.Fatalf("targets %+v", ev.Targets)
	}
	if out := f.e.Invoke(context.Background(), desktop, "k8s.contexts.tag", args("context", "nope", "set", []string{"a=b"})); out.Status != engine.StatusError {
		t.Fatalf("unknown context tagged: %+v", out)
	}
	if out := f.e.Invoke(context.Background(), desktop, "k8s.contexts.tag", args("context", "dev", "set", []string{"bad"})); out.Status != engine.StatusError {
		t.Fatalf("bad tag accepted: %+v", out)
	}
	after, _ := os.ReadFile(f.kubeconfig)
	if string(before) != string(after) {
		t.Fatal("tagging changed the kubeconfig")
	}
	if b, err := os.ReadFile(filepath.Join(f.coxHome, "clusters.json")); err != nil || !strings.Contains(string(b), "careful") {
		t.Fatalf("clusters.json: %s %v", b, err)
	}
}

func TestGetPods(t *testing.T) {
	f := setup(t, runningPod("default", "web-1", "node-1"), crashingPod("default", "api-1"), runningPod("other", "db-1", "node-2"))
	res := f.invoke(t, codex, "k8s.get", args("context", "prod", "kind", "po", "namespace", "default")).(GetOutput)
	if res.Kind != "Pod" || res.Namespace != "default" || strings.Join(res.Columns, ",") != "name,ready,status,restarts,age,node" {
		t.Fatalf("%+v", res)
	}
	if len(res.Rows) != 2 || strings.Join(res.Rows[0], ",") != "api-1,0/1,CrashLoopBackOff,7,2h,node-1" ||
		strings.Join(res.Rows[1], ",") != "web-1,1/1,Running,1,2h,node-1" {
		t.Fatalf("rows %v", res.Rows)
	}
	ev := f.requested(t, "k8s.get")
	if ev.Risk != "read" || len(ev.Targets) != 1 || ev.Targets[0].ID != "k8s:prod/default" || ev.Targets[0].Tags["env"] != "prod" {
		t.Fatalf("audit %+v", ev)
	}

	// Default namespace is the context's; all_namespaces adds a column.
	res = f.invoke(t, codex, "k8s.get", args("context", "dev", "kind", "pods")).(GetOutput)
	if res.Namespace != "default" || len(res.Rows) != 2 {
		t.Fatalf("default ns: %+v", res)
	}
	res = f.invoke(t, codex, "k8s.get", args("context", "dev", "kind", "pods", "all_namespaces", true)).(GetOutput)
	if res.Columns[0] != "namespace" || len(res.Rows) != 3 || res.Rows[2][0] != "other" {
		t.Fatalf("all namespaces: %+v", res)
	}
	if ev := f.requested(t, "k8s.get"); ev.Targets[0].ID != "k8s:dev/*" {
		t.Fatalf("all-namespaces target %+v", ev.Targets)
	}
	res = f.invoke(t, codex, "k8s.get", args("context", "dev", "kind", "pods", "all_namespaces", true, "label_selector", "app=db")).(GetOutput)
	if len(res.Rows) != 1 {
		t.Fatalf("selector: %+v", res.Rows)
	}
	res = f.invoke(t, codex, "k8s.get", args("context", "dev", "kind", "pods", "all_namespaces", true, "limit", 2)).(GetOutput)
	if len(res.Rows) != 2 || !res.More {
		t.Fatalf("limit: %+v", res)
	}
}

func TestGetSummaries(t *testing.T) {
	svc := &corev1.Service{ObjectMeta: meta("default", "web", time.Hour), Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP,
		ClusterIP: "10.0.0.1", Ports: []corev1.ServicePort{{Port: 80, Protocol: corev1.ProtocolTCP}, {Port: 443, NodePort: 30443, Protocol: corev1.ProtocolTCP}}}}
	cm := &corev1.ConfigMap{ObjectMeta: meta("default", "conf", time.Minute), Data: map[string]string{"a": "b"}}
	f := setup(t, deployment("default", "web", 3, 2), node("node-1", "10.1.0.5", true), node("node-2", "10.1.0.6", false), svc, cm)
	for _, c := range []struct{ kind, cols, row string }{
		{"deploy", "name,ready,up-to-date,available,age", "web,2/3,3,2,1d"},
		{"nodes", "name,status,roles,age,version,internal-ip", "node-1,Ready,control-plane,3d,v1.34.0,10.1.0.5"},
		{"svc", "name,type,cluster-ip,ports,age", "web,ClusterIP,10.0.0.1,80/TCP 443:30443/TCP,1h"},
		{"configmaps", "name,status,age", "conf,-,1m"},
	} {
		res := f.invoke(t, codex, "k8s.get", args("context", "dev", "kind", c.kind)).(GetOutput)
		if strings.Join(res.Columns, ",") != c.cols || len(res.Rows) == 0 || strings.Join(res.Rows[0], ",") != c.row {
			t.Errorf("%s: %v %v", c.kind, res.Columns, res.Rows)
		}
	}
	res := f.invoke(t, codex, "k8s.get", args("context", "dev", "kind", "nodes")).(GetOutput)
	if res.Rows[1][1] != "NotReady" {
		t.Fatalf("node-2: %v", res.Rows[1])
	}
	if ev := f.requested(t, "k8s.get"); ev.Targets[0].ID != "k8s:dev" {
		t.Fatalf("cluster-scoped target %+v", ev.Targets)
	}
}

func TestSecretValuesNeverLeave(t *testing.T) {
	f := setup(t, secret())
	for _, a := range []json.RawMessage{
		args("context", "dev", "kind", "secrets"),
		args("context", "dev", "kind", "secret", "name", "db-creds"),
		args("context", "dev", "kind", "secret", "name", "db-creds", "namespace", "default"),
	} {
		res := f.invoke(t, codex, "k8s.get", a)
		s := jsonOf(t, res)
		if strings.Contains(s, "hunter2") || strings.Contains(s, "aHVudGVy") || strings.Contains(s, "YWRtaW4") || strings.Contains(s, "admin\"") {
			t.Fatalf("secret value leaked: %s", s)
		}
		if !strings.Contains(s, "password") {
			t.Fatalf("keys missing: %s", s)
		}
	}
	res := f.invoke(t, codex, "k8s.get", args("context", "dev", "kind", "secrets")).(GetOutput)
	if strings.Join(res.Rows[0], ",") != "db-creds,Opaque,password(19) user(5),1h" {
		t.Fatalf("row %v", res.Rows)
	}
	one := f.invoke(t, codex, "k8s.get", args("context", "dev", "kind", "secret", "name", "db-creds")).(GetOutput)
	if !strings.Contains(one.YAML.Text, "password: <redacted 19 bytes>") || strings.Contains(one.YAML.Text, "last-applied") {
		t.Fatalf("yaml:\n%s", one.YAML.Text)
	}
	d := f.invoke(t, codex, "k8s.describe", args("context", "dev", "kind", "secret", "name", "db-creds"))
	if s := jsonOf(t, d); strings.Contains(s, "hunter2") || strings.Contains(s, "aHVudGVy") || !strings.Contains(s, "password") {
		t.Fatalf("describe: %s", s)
	}
}

func TestGetOneObject(t *testing.T) {
	big := &corev1.ConfigMap{ObjectMeta: meta("default", "big", time.Minute), Data: map[string]string{}}
	big.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "kubectl", Operation: metav1.ManagedFieldsOperationApply}}
	for i := range 400 {
		big.Data[fmt.Sprintf("key%03d", i)] = strings.Repeat("v", 40)
	}
	f := setup(t, big)
	res := f.invoke(t, codex, "k8s.get", args("context", "dev", "kind", "cm", "name", "big")).(GetOutput)
	if res.Name != "big" || res.YAML == nil || !res.YAML.Truncated || res.YAML.ArtifactID == "" {
		t.Fatalf("agent output not shaped: %+v", res.YAML)
	}
	full := f.invoke(t, desktop, "k8s.get", args("context", "dev", "kind", "cm", "name", "big")).(GetOutput)
	if full.YAML.Truncated || !strings.Contains(full.YAML.Text, "kind: ConfigMap") || strings.Contains(full.YAML.Text, "managedFields") {
		t.Fatalf("human output:\n%.500s", full.YAML.Text)
	}
	if out := f.e.Invoke(context.Background(), codex, "k8s.get", args("context", "dev", "kind", "cm", "name", "nope")); out.Status != engine.StatusError ||
		!strings.Contains(out.Error, "not found") {
		t.Fatalf("missing object: %+v", out)
	}
	if out := f.e.Invoke(context.Background(), codex, "k8s.get", args("context", "dev", "kind", "gizmos")); out.Status != engine.StatusError ||
		!strings.Contains(out.Error, "gizmos") {
		t.Fatalf("unknown kind: %+v", out)
	}
	if out := f.e.Invoke(context.Background(), codex, "k8s.get", args("context", "staging", "kind", "pods")); out.Status != engine.StatusError ||
		!strings.Contains(out.Error, "staging") {
		t.Fatalf("unknown context: %+v", out)
	}
}

func TestGetManyRowsShaped(t *testing.T) {
	var objs []runtime.Object
	for i := range 300 {
		objs = append(objs, runningPod("default", fmt.Sprintf("web-%03d", i), "node-1"))
	}
	f := setup(t, objs...)
	res := f.invoke(t, codex, "k8s.get", args("context", "dev", "kind", "pods", "limit", 1000)).(GetOutput)
	if res.Total != 300 || !res.Truncated || res.ArtifactID == "" || len(res.Rows) >= 300 || len(res.Rows) == 0 {
		t.Fatalf("total %d rows %d truncated %v", res.Total, len(res.Rows), res.Truncated)
	}
	full := f.invoke(t, desktop, "k8s.get", args("context", "dev", "kind", "pods", "limit", 1000)).(GetOutput)
	if len(full.Rows) != 300 || full.Truncated {
		t.Fatalf("human rows %d", len(full.Rows))
	}
}

func TestDescribePod(t *testing.T) {
	f := setup(t, crashingPod("default", "api-1"), runningPod("default", "web-1", "node-1"),
		event("default", "api-1", "Pod", "Warning", "BackOff", "Back-off restarting failed container", time.Minute),
		event("default", "web-1", "Pod", "Normal", "Pulled", "pulled", time.Minute))
	res := f.invoke(t, codex, "k8s.describe", args("context", "dev", "kind", "pod", "name", "api-1")).(DescribeOutput)
	if res.Kind != "Pod" || res.Name != "api-1" || len(res.Summary) == 0 || res.Summary["status"] != "CrashLoopBackOff" {
		t.Fatalf("%+v", res)
	}
	if len(res.Containers) != 1 || res.Containers[0].State != "waiting: CrashLoopBackOff (back-off 5m0s)" ||
		res.Containers[0].LastState != "terminated: Error, exit code 1, 1m ago" || res.Containers[0].Restarts != 7 {
		t.Fatalf("containers %+v", res.Containers)
	}
	if len(res.Events) != 1 || res.Events[0].Reason != "BackOff" {
		t.Fatalf("events %+v", res.Events)
	}
	d := f.invoke(t, codex, "k8s.describe", args("context", "dev", "kind", "pod", "name", "web-1")).(DescribeOutput)
	if len(d.Events) != 1 || d.Events[0].Reason != "Pulled" {
		t.Fatalf("web-1 events %+v", d.Events)
	}
}

func TestEvents(t *testing.T) {
	f := setup(t,
		event("default", "api-1", "Pod", "Warning", "BackOff", "back-off", 2*time.Minute),
		event("default", "api-1", "Pod", "Warning", "Unhealthy", "probe failed", time.Minute),
		event("default", "web-1", "Pod", "Normal", "Pulled", "pulled", time.Minute),
		event("default", "old", "Pod", "Warning", "Failed", "old", 3*time.Hour),
		event("other", "db-1", "Pod", "Warning", "OOMKilling", "oom", 5*time.Minute))
	res := f.invoke(t, codex, "k8s.events", args("context", "dev")).(EventsOutput)
	if len(res.Events) != 2 || res.Events[0].Reason != "Unhealthy" || res.Events[1].Reason != "BackOff" ||
		res.Events[0].Object != "Pod/api-1" || res.Events[0].Age != "1m" {
		t.Fatalf("default: %+v", res.Events)
	}
	res = f.invoke(t, codex, "k8s.events", args("context", "dev", "type", "all", "since", "4h")).(EventsOutput)
	if len(res.Events) != 4 {
		t.Fatalf("all types: %+v", res.Events)
	}
	res = f.invoke(t, codex, "k8s.events", args("context", "dev", "all_namespaces", true, "limit", 2)).(EventsOutput)
	if len(res.Events) != 2 || res.Events[0].Namespace != "default" || !res.More {
		t.Fatalf("limit: %+v", res)
	}
	if out := f.e.Invoke(context.Background(), codex, "k8s.events", args("context", "dev", "since", "soon")); out.Status != engine.StatusError {
		t.Fatalf("bad since: %+v", out)
	}
}

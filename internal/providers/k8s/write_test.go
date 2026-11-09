package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hua-ri/coxswain/internal/engine"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const manifest = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  replicas: 3
  template:
    spec:
      containers:
      - name: app
        image: web:3
---
apiVersion: v1
kind: Service
metadata:
  name: web
spec:
  ports:
  - port: 80
---
# a comment only
---
apiVersion: v1
kind: Secret
metadata:
  name: api-key
stringData:
  token: s3cr3t-value-123
`

var deployGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

func liveImage(t *testing.T, f fixture, ctx, ns, name string) string {
	t.Helper()
	u, err := f.fakes[ctx].Dynamic.Resource(deployGVR).Namespace(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cs, _, _ := unstructured.NestedSlice(u.Object, "spec", "template", "spec", "containers")
	return cs[0].(map[string]any)["image"].(string)
}

func TestApplyDryRun(t *testing.T) {
	f := setup(t, deployment("shop", "web", 3, 3))
	res := f.invoke(t, codex, "k8s.apply", args("context", "prod", "manifest", manifest, "dry_run", true)).(ApplyOutput)
	if !res.DryRun || len(res.Objects) != 3 || res.ContextTags["env"] != "prod" {
		t.Fatalf("%+v", res)
	}
	dep, svc, sec := res.Objects[0], res.Objects[1], res.Objects[2]
	if dep.Kind != "Deployment" || dep.Namespace != "shop" || dep.Operation != "configure" || dep.Applied ||
		!strings.Contains(dep.Diff.Text, "-      - image: web:2") || !strings.Contains(dep.Diff.Text, "+      - image: web:3") {
		t.Fatalf("deployment: %+v\n%s", dep, dep.Diff.Text)
	}
	if strings.Contains(dep.Diff.Text, "resourceVersion") || strings.Contains(dep.Diff.Text, "status:") {
		t.Fatalf("diff shows ignored fields:\n%s", dep.Diff.Text)
	}
	if svc.Operation != "create" || !strings.Contains(svc.Diff.Text, "+  name: web") {
		t.Fatalf("service: %+v", svc)
	}
	if sec.Operation != "create" || strings.Contains(jsonOf(t, res), "s3cr3t") || !strings.Contains(sec.Diff.Text, "token: <redacted 16 bytes") {
		t.Fatalf("secret: %s", jsonOf(t, sec))
	}
	if img := liveImage(t, f, "prod", "shop", "web"); img != "web:2" {
		t.Fatalf("dry run changed the deployment: %s", img)
	}
	for _, c := range f.fakes["prod"].Applies.Calls() {
		if !c.DryRun {
			t.Fatalf("real apply in a dry run: %+v", c)
		}
	}
	ev := f.requested(t, "k8s.apply")
	// A write: the manifest has a Secret, compared with the live one.
	if ev.Risk != "write" || ev.Targets[0].ID != "k8s:prod/shop" || ev.Targets[0].Tags["env"] != "prod" {
		t.Fatalf("audit %+v", ev)
	}
	if strings.Contains(string(ev.Args), "s3cr3t") || !strings.Contains(string(ev.Args), "redacted") {
		t.Fatalf("audit args leak the secret: %s", ev.Args)
	}
}

func TestApply(t *testing.T) {
	f := setup(t, deployment("shop", "web", 3, 3), deployment("shop", "same", 1, 1))
	res := f.invoke(t, codex, "k8s.apply", args("context", "prod", "manifest", manifest)).(ApplyOutput)
	if res.DryRun {
		t.Fatal("dry run")
	}
	for _, o := range res.Objects {
		if !o.Applied {
			t.Fatalf("not applied: %+v", o)
		}
	}
	if img := liveImage(t, f, "prod", "shop", "web"); img != "web:3" {
		t.Fatalf("not applied: %s", img)
	}
	calls := f.fakes["prod"].Applies.Calls()
	if len(calls) != 6 || !calls[0].DryRun || !calls[2].DryRun || calls[3].DryRun || calls[5].DryRun {
		t.Fatalf("dry runs must all come first: %+v", calls)
	}
	ev := f.requested(t, "k8s.apply")
	if ev.Risk != "destructive" || strings.Contains(string(ev.Args), "s3cr3t") {
		t.Fatalf("audit %+v %s", ev, ev.Args)
	}
	// Unchanged objects are reported as such.
	res = f.invoke(t, codex, "k8s.apply", args("context", "prod", "manifest", manifest, "dry_run", true)).(ApplyOutput)
	if res.Objects[0].Operation != "unchanged" || res.Objects[0].Diff != nil {
		t.Fatalf("second apply: %+v", res.Objects[0])
	}
}

func TestApplyFailsBeforeChangingAnything(t *testing.T) {
	f := setup(t, deployment("shop", "web", 3, 3))
	f.fakes["prod"].Applies.Fail = func(obj *unstructured.Unstructured, dryRun bool) error {
		if obj.GetKind() == "Service" {
			return errors.New(`Service "web" is invalid: spec.ports[0].port: Invalid value`)
		}
		return nil
	}
	out := f.e.Invoke(context.Background(), codex, "k8s.apply", args("context", "prod", "manifest", manifest))
	if out.Status != engine.StatusError || !strings.Contains(out.Error, "Service/web") || !strings.Contains(out.Error, "nothing was applied") {
		t.Fatalf("%+v", out)
	}
	for _, c := range f.fakes["prod"].Applies.Calls() {
		if !c.DryRun {
			t.Fatalf("applied after a failed dry run: %+v", c)
		}
	}
}

func TestApplyRejects(t *testing.T) {
	f := setup(t)
	for name, c := range map[string]struct{ manifest, ns, want string }{
		"empty":        {"# nothing\n---\n", "", "no objects"},
		"no name":      {"apiVersion: v1\nkind: ConfigMap\nmetadata: {generateName: x-}\n", "", "name"},
		"unknown kind": {"apiVersion: v1\nkind: Gizmo\nmetadata: {name: x}\n", "", "Gizmo"},
		"ns conflict":  {"apiVersion: v1\nkind: ConfigMap\nmetadata: {name: x, namespace: a}\n", "b", "namespace"},
		"bad yaml":     {"apiVersion: v1\nkind: [\n", "", "manifest"},
		"no kind":      {"metadata: {name: x}\n", "", "kind"},
	} {
		a := args("context", "dev", "manifest", c.manifest, "dry_run", true)
		if c.ns != "" {
			a = args("context", "dev", "manifest", c.manifest, "dry_run", true, "namespace", c.ns)
		}
		out := f.e.Invoke(context.Background(), codex, "k8s.apply", a)
		if out.Status != engine.StatusError || !strings.Contains(out.Error, c.want) {
			t.Errorf("%s: %+v", name, out)
		}
	}
	// A cluster-scoped object ignores the namespace and targets the cluster.
	f.invoke(t, codex, "k8s.apply", args("context", "dev", "manifest", "apiVersion: v1\nkind: Namespace\nmetadata: {name: team}\n", "dry_run", true))
	if ev := f.requested(t, "k8s.apply"); len(ev.Targets) != 1 || ev.Targets[0].ID != "k8s:dev" {
		t.Fatalf("targets %+v", ev.Targets)
	}
}

func TestRedactManifest(t *testing.T) {
	in := "apiVersion: v1\nkind: Secret\nmetadata: {name: s}\ndata: {pw: aHVudGVyMg==}\nstringData: {tok: abc}\n---\n" +
		"apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c}\ndata: {k: visible}\n---\napiVersion: v1\nkind: List\nitems:\n" +
		"- apiVersion: v1\n  kind: Secret\n  metadata: {name: t}\n  data: {x: c2VjcmV0}\n"
	out := redactManifest(in)
	for _, leak := range []string{"aHVudGVyMg", "abc", "c2VjcmV0"} {
		if strings.Contains(out, leak) {
			t.Fatalf("%s leaked:\n%s", leak, out)
		}
	}
	if !strings.Contains(out, "visible") || !strings.Contains(out, "pw: <redacted 7 bytes>") || !strings.Contains(out, "tok: <redacted 3 bytes>") {
		t.Fatalf("redacted:\n%s", out)
	}
	if out := redactManifest("kind: Secret\ndata: {pw: x\n"); strings.Contains(out, "pw") {
		t.Fatalf("unparseable manifest kept: %s", out)
	}
	raw := redactApply(json.RawMessage(`{"context":"dev","manifest":"apiVersion: v1\nkind: Secret\nmetadata: {name: s}\nstringData: {tok: abc}\n"}`))
	if strings.Contains(string(raw), "abc") || !strings.Contains(string(raw), `"context":"dev"`) {
		t.Fatalf("args: %s", raw)
	}
}

func TestDiffLines(t *testing.T) {
	a := "a\nb\nc\nd\ne\nf\ng\nh\ni\n"
	b := "a\nb\nc\nd\nE\nf\ng\nh\ni\nj\n"
	got := diffLines("old", "new", a, b)
	want := "--- old\n+++ new\n@@ -2,8 +2,9 @@\n b\n c\n d\n-e\n+E\n f\n g\n h\n i\n+j\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if diffLines("old", "new", a, a) != "" {
		t.Fatal("no change, but a diff")
	}
}

func TestDelete(t *testing.T) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team"}}
	f := setup(t, runningPod("shop", "web-1", "node-1"), ns)
	res := f.invoke(t, codex, "k8s.delete", args("context", "prod", "kind", "pod", "name", "web-1")).(DeleteOutput)
	if res.Kind != "Pod" || res.Namespace != "shop" || res.Warning != "" {
		t.Fatalf("%+v", res)
	}
	podGVR := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	if _, err := f.fakes["prod"].Dynamic.Resource(podGVR).Namespace("shop").Get(context.Background(), "web-1", metav1.GetOptions{}); err == nil {
		t.Fatal("not deleted")
	}
	if ev := f.requested(t, "k8s.delete"); ev.Risk != "destructive" || ev.Targets[0].ID != "k8s:prod/shop" {
		t.Fatalf("audit %+v", ev)
	}
	res = f.invoke(t, codex, "k8s.delete", args("context", "dev", "kind", "ns", "name", "team")).(DeleteOutput)
	if !strings.Contains(res.Warning, "every object") {
		t.Fatalf("namespace warning: %+v", res)
	}
	if ev := f.requested(t, "k8s.delete"); ev.Risk != "destructive" || ev.Targets[0].ID != "k8s:dev" {
		t.Fatalf("audit %+v", ev)
	}
	if out := f.e.Invoke(context.Background(), codex, "k8s.delete", args("context", "dev", "kind", "pod", "name", "nope")); out.Status != engine.StatusError ||
		!strings.Contains(out.Error, "not found") {
		t.Fatalf("missing: %+v", out)
	}
	if out := f.e.Invoke(context.Background(), codex, "k8s.delete", args("context", "dev", "kind", "pod")); out.Status != engine.StatusError {
		t.Fatalf("no name: %+v", out)
	}
}

func TestScale(t *testing.T) {
	f := setup(t, deployment("shop", "web", 3, 3), &corev1.ConfigMap{ObjectMeta: meta("shop", "conf", time.Hour)})
	res := f.invoke(t, codex, "k8s.scale", args("context", "prod", "kind", "deploy", "name", "web", "replicas", 5)).(ScaleOutput)
	if res.From != 3 || res.To != 5 || res.Kind != "Deployment" {
		t.Fatalf("%+v", res)
	}
	if ev := f.requested(t, "k8s.scale"); ev.Risk != "write" || ev.Targets[0].ID != "k8s:prod/shop" {
		t.Fatalf("audit %+v", ev)
	}
	u, _ := f.fakes["prod"].Dynamic.Resource(deployGVR).Namespace("shop").Get(context.Background(), "web", metav1.GetOptions{})
	if n, _, _ := unstructured.NestedInt64(u.Object, "spec", "replicas"); n != 5 {
		t.Fatalf("replicas %d", n)
	}
	f.invoke(t, codex, "k8s.scale", args("context", "prod", "kind", "deploy", "name", "web", "replicas", 0))
	if ev := f.requested(t, "k8s.scale"); ev.Risk != "destructive" {
		t.Fatalf("scale to 0: %s", ev.Risk)
	}
	for _, a := range []json.RawMessage{
		args("context", "prod", "kind", "cm", "name", "conf", "replicas", 1),
		args("context", "prod", "kind", "deploy", "name", "web"),
		args("context", "prod", "kind", "deploy", "name", "web", "replicas", -1),
	} {
		if out := f.e.Invoke(context.Background(), codex, "k8s.scale", a); out.Status != engine.StatusError {
			t.Errorf("%s: %+v", a, out)
		}
	}
}

func testRS(d *appsv1.Deployment, rev, image string) *appsv1.ReplicaSet {
	rs := &appsv1.ReplicaSet{ObjectMeta: meta(d.Namespace, d.Name+"-"+rev, time.Duration(10-len(rev))*time.Hour),
		Spec: appsv1.ReplicaSetSpec{Template: *d.Spec.Template.DeepCopy()}}
	rs.Annotations = map[string]string{"deployment.kubernetes.io/revision": rev}
	rs.OwnerReferences = []metav1.OwnerReference{{Kind: "Deployment", Name: d.Name, UID: d.UID, APIVersion: "apps/v1"}}
	rs.Spec.Template.Labels["pod-template-hash"] = "h" + rev
	rs.Spec.Template.Spec.Containers[0].Image = image
	return rs
}

func TestRollout(t *testing.T) {
	d := deployment("shop", "web", 3, 1)
	d.Generation, d.Status.ObservedGeneration = 2, 2
	d.Status.UpdatedReplicas, d.Status.AvailableReplicas = 3, 1
	d.Annotations = map[string]string{"deployment.kubernetes.io/revision": "3"}
	sts := &appsv1.StatefulSet{ObjectMeta: meta("shop", "db", time.Hour), Spec: appsv1.StatefulSetSpec{Replicas: new(int32(2))},
		Status: appsv1.StatefulSetStatus{Replicas: 2, ReadyReplicas: 2, UpdatedReplicas: 2, CurrentRevision: "r1", UpdateRevision: "r1"}}
	f := setup(t, d, testRS(d, "1", "web:1"), testRS(d, "2", "web:2"), testRS(d, "3", "web:3"), sts)

	st := f.invoke(t, codex, "k8s.rollout", args("context", "prod", "kind", "deploy", "name", "web", "action", "status")).(RolloutOutput)
	if st.Done || !strings.Contains(st.Message, "1 of 3 updated replicas are available") || len(st.Revisions) != 3 ||
		st.Revisions[0].Revision != 3 || st.Revisions[0].Images[0] != "web:3" {
		t.Fatalf("status: %+v", st)
	}
	if ev := f.requested(t, "k8s.rollout"); ev.Risk != "read" {
		t.Fatalf("status risk %s", ev.Risk)
	}
	st = f.invoke(t, codex, "k8s.rollout", args("context", "prod", "kind", "sts", "name", "db", "action", "status")).(RolloutOutput)
	if !st.Done {
		t.Fatalf("sts status: %+v", st)
	}

	f.invoke(t, codex, "k8s.rollout", args("context", "prod", "kind", "deploy", "name", "web", "action", "restart"))
	if ev := f.requested(t, "k8s.rollout"); ev.Risk != "write" {
		t.Fatalf("restart risk %s", ev.Risk)
	}
	u, _ := f.fakes["prod"].Dynamic.Resource(deployGVR).Namespace("shop").Get(context.Background(), "web", metav1.GetOptions{})
	if at, _, _ := unstructured.NestedString(u.Object, "spec", "template", "metadata", "annotations", "kubectl.kubernetes.io/restartedAt"); at != now.Format(time.RFC3339) {
		t.Fatalf("restartedAt %q", at)
	}

	f.invoke(t, codex, "k8s.rollout", args("context", "prod", "kind", "deploy", "name", "web", "action", "pause"))
	if out := f.e.Invoke(context.Background(), codex, "k8s.rollout", args("context", "prod", "kind", "deploy", "name", "web", "action", "restart")); out.Status != engine.StatusError ||
		!strings.Contains(out.Error, "paused") {
		t.Fatalf("restart while paused: %+v", out)
	}
	f.invoke(t, codex, "k8s.rollout", args("context", "prod", "kind", "deploy", "name", "web", "action", "resume"))

	undo := f.invoke(t, codex, "k8s.rollout", args("context", "prod", "kind", "deploy", "name", "web", "action", "undo")).(RolloutOutput)
	if undo.ToRevision != 2 {
		t.Fatalf("undo: %+v", undo)
	}
	if img := liveImage(t, f, "prod", "shop", "web"); img != "web:2" {
		t.Fatalf("undo image %s", img)
	}
	u, _ = f.fakes["prod"].Dynamic.Resource(deployGVR).Namespace("shop").Get(context.Background(), "web", metav1.GetOptions{})
	if h, _, _ := unstructured.NestedString(u.Object, "spec", "template", "metadata", "labels", "pod-template-hash"); h != "" {
		t.Fatalf("pod-template-hash copied: %s", h)
	}
	undo = f.invoke(t, codex, "k8s.rollout", args("context", "prod", "kind", "deploy", "name", "web", "action", "undo", "to_revision", 1)).(RolloutOutput)
	if undo.ToRevision != 1 || liveImage(t, f, "prod", "shop", "web") != "web:1" {
		t.Fatalf("undo to 1: %+v", undo)
	}
	for _, a := range []json.RawMessage{
		args("context", "prod", "kind", "deploy", "name", "web", "action", "undo", "to_revision", 9),
		args("context", "prod", "kind", "sts", "name", "db", "action", "undo"),
		args("context", "prod", "kind", "sts", "name", "db", "action", "pause"),
		args("context", "prod", "kind", "cm", "name", "x", "action", "status"),
		args("context", "prod", "kind", "deploy", "name", "web", "action", "explode"),
	} {
		if out := f.e.Invoke(context.Background(), codex, "k8s.rollout", a); out.Status != engine.StatusError {
			t.Errorf("%s: %+v", a, out)
		}
	}
}

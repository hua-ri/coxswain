package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hua-ri/coxswain/internal/engine"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// TestApplySecretNoOracle: a dry run must not tell whether given Secret
// values equal the live ones (an oracle to guess them), and a dry run
// with a Secret is at least a write.
func TestApplySecretNoOracle(t *testing.T) {
	f := setup(t, secret())
	same := "apiVersion: v1\nkind: Secret\nmetadata: {name: db-creds, namespace: default}\ndata: {password: aHVudGVyMi1zdXBlcnNlY3JldA==, user: YWRtaW4=}\n"
	other := strings.Replace(same, "aHVudGVyMi1zdXBlcnNlY3JldA==", "Z3Vlc3MtZ3Vlc3MtZ3Vlc3MtMQ==", 1)
	var outs []ApplyOutput
	for _, m := range []string{same, other} {
		outs = append(outs, f.invoke(t, codex, "k8s.apply", args("context", "dev", "manifest", m, "dry_run", true)).(ApplyOutput))
		if ev := f.requested(t, "k8s.apply"); ev.Risk != "write" {
			t.Fatalf("risk %s", ev.Risk)
		}
	}
	a, b := jsonOf(t, outs[0]), jsonOf(t, outs[1])
	if a != b || outs[0].Objects[0].Operation != "configure" || strings.Contains(a, "changed") {
		t.Fatalf("the result depends on the values:\n%s\n%s", a, b)
	}
}

// TestApplyKeysExact: "Manifest" would be applied (encoding/json ignores
// case) but missed by the audit redaction; such arguments are refused.
func TestApplyKeysExact(t *testing.T) {
	f := setup(t)
	raw := json.RawMessage(`{"context":"dev","Manifest":"apiVersion: v1\nkind: Secret\nmetadata: {name: s}\nstringData: {tok: s3cr3t}\n"}`)
	if out := f.e.Invoke(context.Background(), codex, "k8s.apply", raw); out.Status != engine.StatusError || !strings.Contains(out.Error, "Manifest") {
		t.Fatalf("%+v", out)
	}
	if got := string(redactApply(raw)); strings.Contains(got, "s3cr3t") {
		t.Fatalf("redacted: %s", got)
	}
	// A secret whatever the case of its kind.
	if got := redactManifest("apiVersion: v1\nkind: SECRET\nmetadata: {name: s}\nstringData: {tok: s3cr3t}\n"); strings.Contains(got, "s3cr3t") {
		t.Fatalf("redacted: %s", got)
	}
}

func TestApplyListKinds(t *testing.T) {
	f := setup(t)
	list := "apiVersion: apps/v1\nkind: DeploymentList\nitems:\n- apiVersion: v1\n  kind: ConfigMap\n  metadata: {name: x}\n"
	if out := f.e.Invoke(context.Background(), codex, "k8s.apply", args("context", "dev", "manifest", list, "dry_run", true)); out.Status != engine.StatusError ||
		!strings.Contains(out.Error, "DeploymentList") {
		t.Fatalf("%+v", out)
	}
	core := "apiVersion: v1\nkind: List\nitems:\n- apiVersion: v1\n  kind: ConfigMap\n  metadata: {name: x}\n"
	res := f.invoke(t, codex, "k8s.apply", args("context", "dev", "manifest", core, "dry_run", true)).(ApplyOutput)
	if len(res.Objects) != 1 || res.Objects[0].Kind != "ConfigMap" {
		t.Fatalf("%+v", res)
	}
}

// TestSecretByResource: a custom kind named Secret in another group is
// not a Secret; summaries go by group and kind.
func TestSummariesByGroup(t *testing.T) {
	if got := summaryFor("example.com", "Deployment"); got.columns[1] != "status" {
		t.Fatalf("%v", got.columns)
	}
	if got := summaryFor("apps", "Deployment"); got.columns[1] != "ready" {
		t.Fatalf("%v", got.columns)
	}
	if got := summaryFor("", "Pod"); got.columns[1] != "ready" {
		t.Fatalf("%v", got.columns)
	}
}

func TestGetByteBudget(t *testing.T) {
	var objs []runtime.Object
	for i := range 50 {
		objs = append(objs, &corev1.ConfigMap{ObjectMeta: meta("default", fmt.Sprintf("cm-%02d", i), time.Hour),
			Data: map[string]string{"k": strings.Repeat("v", 1000)}})
	}
	f := setup(t, objs...)
	defer func(b int) { listBudget = b }(listBudget)
	listBudget = 10 << 10
	res := f.invoke(t, desktop, "k8s.get", args("context", "dev", "kind", "cm")).(GetOutput)
	if len(res.Rows) == 0 || len(res.Rows) >= 50 || !res.More {
		t.Fatalf("rows %d more %v", len(res.Rows), res.More)
	}
}

func TestEventsBounded(t *testing.T) {
	var objs []runtime.Object
	for i := range 40 {
		objs = append(objs, event("default", fmt.Sprintf("p-%02d", i), "Pod", "Warning", "BackOff", strings.Repeat("x", 1000), time.Minute))
	}
	f := setup(t, objs...)
	res := f.invoke(t, codex, "k8s.events", args("context", "dev")).(EventsOutput)
	if !res.Truncated || res.ArtifactID == "" || len(res.Events) == 0 || len(res.Events) >= 40 || len(res.Events[0].Message) > 310 {
		t.Fatalf("events %d truncated %v msg %d", len(res.Events), res.Truncated, len(res.Events[0].Message))
	}
	defer func(n int) { maxEvents = n }(maxEvents)
	maxEvents = 5
	res = f.invoke(t, desktop, "k8s.events", args("context", "dev")).(EventsOutput)
	if len(res.Events) != 5 || !res.Incomplete {
		t.Fatalf("cap: %d %v", len(res.Events), res.Incomplete)
	}
}

func TestDeleteAllNamespacesRefused(t *testing.T) {
	f := setup(t, runningPod("default", "web-1", "node-1"))
	if out := f.e.Invoke(context.Background(), codex, "k8s.delete", args("context", "dev", "kind", "pod", "name", "web-1", "all_namespaces", true)); out.Status != engine.StatusError ||
		!strings.Contains(out.Error, "all_namespaces") {
		t.Fatalf("%+v", out)
	}
}

func TestUndoPausedRefused(t *testing.T) {
	d := deployment("shop", "web", 1, 1)
	d.Spec.Paused = true
	d.Annotations = map[string]string{"deployment.kubernetes.io/revision": "2"}
	f := setup(t, d, testRS(d, "1", "web:1"), testRS(d, "2", "web:2"))
	if out := f.e.Invoke(context.Background(), codex, "k8s.rollout", args("context", "prod", "kind", "deploy", "name", "web", "action", "undo")); out.Status != engine.StatusError ||
		!strings.Contains(out.Error, "paused") {
		t.Fatalf("%+v", out)
	}
}

func TestDiagnoseSelectors(t *testing.T) {
	empty := deployment("default", "web", 1, 0)
	empty.Spec.Selector = &metav1.LabelSelector{}
	bad := deployment("default", "api", 1, 0)
	bad.Spec.Selector = &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "app", Operator: "Bogus"}}}
	f := setup(t, empty, bad, crashingPod("default", "web-1"))
	d := f.invoke(t, codex, "k8s.diagnose", args("context", "dev", "kind", "deployment", "name", "web")).(DiagnoseOutput)
	if got := findings(d); !strings.Contains(got, "empty selector") || strings.Contains(got, "web-1") || len(d.Evidence.Pods) != 0 {
		t.Fatalf("empty:\n%s", got)
	}
	d = f.invoke(t, codex, "k8s.diagnose", args("context", "dev", "kind", "deployment", "name", "api")).(DiagnoseOutput)
	if got := findings(d); !strings.Contains(got, "invalid selector") {
		t.Fatalf("invalid:\n%s", got)
	}
}

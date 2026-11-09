package k8s

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hua-ri/coxswain/internal/engine"
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

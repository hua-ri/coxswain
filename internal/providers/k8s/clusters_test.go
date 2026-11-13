package k8s

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hua-ri/coxswain/internal/engine"
	"github.com/hua-ri/coxswain/internal/eventlog"
	kube "github.com/hua-ri/coxswain/internal/k8s"
	"github.com/hua-ri/coxswain/internal/k8s/k8stest"
)

const addedKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster: {server: https://10.9.0.1:6443}
users:
- name: u
  user: {token: very-secret-token}
contexts:
- name: staging
  context: {cluster: c, user: u, namespace: shop}
`

func TestClustersAreForTheUser(t *testing.T) {
	f := setup(t)
	for _, id := range []string{"k8s.clusters.add", "k8s.clusters.inspect", "k8s.clusters.test", "k8s.clusters.remove"} {
		out := f.e.Invoke(context.Background(), codex, id, args("kubeconfig", addedKubeconfig, "context", "staging"))
		if out.Status == engine.StatusOK || !strings.Contains(out.Error, "ask the user") {
			t.Fatalf("%s for an agent: %+v", id, out)
		}
	}
	evs, _ := f.e.Events.Read(time.Now())
	refused := 0
	for _, ev := range evs {
		if ev.Type == eventlog.ActionRefused {
			refused++
		}
		if strings.Contains(string(ev.Args), "very-secret-token") {
			t.Fatalf("credentials in the audit log: %s", ev.Args)
		}
	}
	if refused != 4 {
		t.Fatalf("%d refusals audited", refused)
	}
	if files, _ := filepath.Glob(filepath.Join(f.coxHome, "kubeconfigs", "*.yaml")); len(files) != 0 {
		t.Fatalf("stored for an agent: %v", files)
	}
}

func TestClustersAddAndRemove(t *testing.T) {
	f := setup(t)
	before, _ := os.ReadFile(f.kubeconfig)
	st0, _ := os.Stat(f.kubeconfig)
	f.fakes["staging"] = k8stest.NewFake("staging")

	insp := f.invoke(t, desktop, "k8s.clusters.inspect", args("kubeconfig", addedKubeconfig)).(InspectOutput)
	if len(insp.Contexts) != 1 || insp.Contexts[0].Name != "staging" || insp.Contexts[0].Auth != "token" {
		t.Fatalf("%+v", insp)
	}
	if out := f.invoke(t, desktop, "k8s.clusters.test", args("kubeconfig", addedKubeconfig)).(ProbeOutput); out.Context != "staging" {
		t.Fatalf("%+v", out)
	}

	ci := f.invoke(t, desktop, "k8s.clusters.add", args("kubeconfig", addedKubeconfig, "tags", []string{"env=staging"}, "note", "shared")).(kube.ContextInfo)
	if ci.Name != "staging" || ci.Source != "coxswain" || ci.Tags["env"] != "staging" || ci.Note != "shared" {
		t.Fatalf("%+v", ci)
	}
	ev := f.requested(t, "k8s.clusters.add")
	if strings.Contains(string(ev.Args), "very-secret-token") || !strings.Contains(string(ev.Args), "sha256") ||
		!strings.Contains(string(ev.Args), `"contexts":["staging"]`) {
		t.Fatalf("redaction: %s", ev.Args)
	}
	list := f.invoke(t, codex, "k8s.contexts", nil).(ContextsOutput)
	sources := map[string]string{}
	for _, c := range list.Contexts {
		sources[c.Name] = c.Source
	}
	if sources["staging"] != "coxswain" || sources["dev"] != "kubeconfig" || sources["prod"] != "kubeconfig" {
		t.Fatalf("sources %v", sources)
	}
	// Agents may use an added cluster like any other.
	f.invoke(t, codex, "k8s.get", args("context", "staging", "kind", "pods"))

	out := f.e.Invoke(context.Background(), desktop, "k8s.clusters.remove", args("context", "dev"))
	if out.Status == engine.StatusOK || !strings.Contains(out.Error, "read-only") {
		t.Fatalf("removed the user's context: %+v", out)
	}
	f.invoke(t, desktop, "k8s.clusters.remove", args("context", "staging"))
	if ev := f.requested(t, "k8s.clusters.remove"); ev.Risk != "destructive" {
		t.Fatalf("%+v", ev)
	}
	after, _ := os.ReadFile(f.kubeconfig)
	st1, _ := os.Stat(f.kubeconfig)
	if string(before) != string(after) || !st0.ModTime().Equal(st1.ModTime()) {
		t.Fatal("red line: the user's kubeconfig changed")
	}
}

func TestClustersRedactUnknownKeys(t *testing.T) {
	f := setup(t)
	out := f.e.Invoke(context.Background(), desktop, "k8s.clusters.add", args("Kubeconfig", addedKubeconfig))
	if out.Status == engine.StatusOK {
		t.Fatal("misspelt key accepted")
	}
	evs, _ := f.e.Events.Read(time.Now())
	for _, ev := range evs {
		if strings.Contains(string(ev.Args), "very-secret-token") {
			t.Fatalf("credentials in the audit log: %s", ev.Args)
		}
	}
}

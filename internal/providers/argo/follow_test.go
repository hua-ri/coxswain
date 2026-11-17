package argo

import (
	"context"
	"errors"
	"strings"
	"testing"

	k8sprov "github.com/hua-ri/coxswain/internal/providers/k8s"
)

// fakeK8s stands in for the Kubernetes provider: one context, prod-hk,
// whose server is the web app's destination in these tests.
type fakeK8s struct {
	diagnosed []string
	podGone   bool
}

func (f *fakeK8s) Diagnose(_ context.Context, kctx, ns, kind, name string) (k8sprov.DiagnoseOutput, error) {
	f.diagnosed = append(f.diagnosed, kctx+"/"+ns+"/"+name)
	if f.podGone {
		return k8sprov.DiagnoseOutput{}, errors.New(`pods "` + name + `" not found`)
	}
	return k8sprov.DiagnoseOutput{Context: kctx, Kind: "Pod", Namespace: ns, Name: name,
		Findings:  []k8sprov.Finding{{Severity: "error", Title: "CrashLoopBackOff", Detail: "container web exited 1"}},
		Evidence:  k8sprov.Evidence{Nodes: []k8sprov.NodeEvidence{{Name: "node-7", Status: "Ready", SSHHost: "web-7"}}},
		NextSteps: []string{"ssh_exec on web-7"}}, nil
}

func (f *fakeK8s) NodeHost(_ context.Context, kctx, node string) (k8sprov.NodeHostOutput, error) {
	return k8sprov.NodeHostOutput{Context: kctx, Node: node, Host: "gpu-3"}, nil
}

func (f *fakeK8s) ContextsForServer(server string) []string {
	if server == "https://kubernetes.default.svc" {
		return nil
	}
	return []string{"prod-hk"}
}

func (f *fakeK8s) HasContext(name string) bool { return name == "prod-hk" }

func TestCDDiagnoseFollowsPodsIntoTheCluster(t *testing.T) {
	f := setup(t)
	k := &fakeK8s{}
	f.withK8s(k)
	f.cdServer(t)
	// The web app's destination is kubernetes.default.svc: Argo CD's own cluster.
	out := f.ok(t, agent, "argocd.diagnose", map[string]any{"app": "web"})
	if len(k.diagnosed) != 0 || !strings.Contains(out, "k8s_context=") {
		t.Fatalf("without k8s_context: %v %s", k.diagnosed, out)
	}
	if o := f.call(t, human, "argo.update", map[string]any{"kind": "argocd", "name": "dev", "k8s_context": "nope"}); o.Status == "ok" {
		t.Fatal("unknown k8s_context accepted")
	}
	f.ok(t, human, "argo.update", map[string]any{"kind": "argocd", "name": "dev", "k8s_context": "prod-hk"})
	out = f.ok(t, agent, "argocd.diagnose", map[string]any{"app": "web"})
	if len(k.diagnosed) != 1 || k.diagnosed[0] != "prod-hk/shop/web-7d9-abc" {
		t.Fatalf("diagnosed %v", k.diagnosed)
	}
	for _, want := range []string{"Pod web-7d9-abc: CrashLoopBackOff", "Node node-7 is SSH host web-7", "ssh_exec on web-7"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q: %s", want, out)
		}
	}
}

func TestWFDiagnoseFindsTheNodeOfAGonePod(t *testing.T) {
	f := setup(t)
	k := &fakeK8s{podGone: true}
	f.withK8s(k)
	f.wfServer(t)
	f.ok(t, human, "argo.update", map[string]any{"kind": "argowf", "name": "ci", "k8s_context": "prod-hk"})
	out := f.ok(t, agent, "argowf.diagnose", map[string]any{"name": "build-x7"})
	if len(k.diagnosed) != 1 || !strings.Contains(out, "ran on node node-9, SSH host gpu-3") {
		t.Fatalf("diagnosed %v: %s", k.diagnosed, out)
	}
}

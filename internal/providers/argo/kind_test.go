//go:build kind

package argo

// go test -tags kind ./internal/providers/argo: runs the actions against the
// Argo CD and Argo Workflows of scripts/kind-argo.sh (cox-test cluster,
// port-forwarded to localhost:8080 and :2746). COX_ARGOWF_TOKEN must be a
// token from `scripts/kind-argo.sh token`. Credentials stay in memory.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/argo"
	"github.com/hua-ri/coxswain/internal/engine"
	"github.com/hua-ri/coxswain/internal/inventory"
	kube "github.com/hua-ri/coxswain/internal/k8s"
	k8sprov "github.com/hua-ri/coxswain/internal/providers/k8s"
	"github.com/hua-ri/coxswain/internal/eventlog"
	"github.com/hua-ri/coxswain/internal/secrets"
	"github.com/hua-ri/coxswain/internal/shaper"
)

func kindEngine(t *testing.T) (*engine.Engine, string) {
	dir := t.TempDir()
	s := &argo.Sessions{Store: argo.NewStore(filepath.Join(dir, "argo.json")), Secrets: &secrets.Memory{}}
	reg := action.NewRegistry()
	(&Provider{Sessions: s, Artifacts: shaper.NewArtifacts(filepath.Join(dir, "artifacts"))}).Register(reg)
	return &engine.Engine{Registry: reg, Events: eventlog.Open(filepath.Join(dir, "events"))}, dir
}

func run(t *testing.T, e *engine.Engine, who action.Actor, id string, in map[string]any) string {
	t.Helper()
	b, _ := json.Marshal(in)
	out := e.Invoke(context.Background(), who, id, b)
	if out.Status != "ok" {
		t.Fatalf("%s: %s %s", id, out.Error, out.Hint)
	}
	r, _ := json.Marshal(out.Result)
	t.Logf("%s → %.600s", id, r)
	return string(r)
}

func TestKindArgoCD(t *testing.T) {
	e, dir := kindEngine(t)
	run(t, e, agent, "argo.add", map[string]any{"kind": "argocd", "name": "kind", "url": "http://localhost:8080"})
	m := run(t, e, agent, "argocd.login_methods", map[string]any{})
	if !strings.Contains(m, `"browser"`) || !strings.Contains(m, "Mock SSO") {
		t.Fatalf("methods: %s", m)
	}
	run(t, e, agent, "argocd.login", map[string]any{"method": "password", "username": "admin", "password": "cox-admin-pw"})
	apps := run(t, e, agent, "argocd.apps", map[string]any{})
	if !strings.Contains(apps, "guestbook") {
		t.Fatalf("apps: %s", apps)
	}
	run(t, e, agent, "argocd.sync", map[string]any{"app": "guestbook", "dry_run": true})
	run(t, e, agent, "argocd.diff", map[string]any{"app": "guestbook"})
	run(t, e, agent, "argocd.diagnose", map[string]any{"app": "guestbook"})
	b, _ := os.ReadFile(filepath.Join(dir, "events", time.Now().Format("2006-01-02")+".jsonl"))
	if strings.Contains(string(b), "cox-admin-pw") {
		t.Fatal("password in the audit log")
	}
}

func TestKindArgoWorkflows(t *testing.T) {
	tok := os.Getenv("COX_ARGOWF_TOKEN")
	if tok == "" {
		t.Skip("COX_ARGOWF_TOKEN not set")
	}
	e, _ := kindEngine(t)
	run(t, e, agent, "argo.add", map[string]any{"kind": "argowf", "name": "kind", "url": "http://localhost:2746", "namespace": "ci"})
	m := run(t, e, agent, "argowf.login_methods", map[string]any{})
	if !strings.HasPrefix(strings.SplitN(m, `"methods":[`, 2)[1], `{"method":"token"`) {
		t.Fatalf("client mode should suggest a token first: %s", m)
	}
	login := run(t, e, human, "argowf.login", map[string]any{"method": "token", "token": tok})
	if !strings.Contains(login, "system:serviceaccount:ci:cox-ops") {
		t.Fatalf("identity: %s", login)
	}
	list := run(t, e, agent, "argowf.list", map[string]any{"phase": "Failed"})
	var out WFListOutput
	json.Unmarshal([]byte(list), &out)
	var build, oom string
	for _, w := range out.Workflows {
		if strings.HasPrefix(w.Name, "build-") {
			build = w.Name
		}
		if strings.HasPrefix(w.Name, "oom-") {
			oom = w.Name
		}
	}
	if build == "" || oom == "" {
		t.Fatalf("expected a failed build and an OOM: %s", list)
	}
	d := run(t, e, agent, "argowf.diagnose", map[string]any{"name": build})
	if !strings.Contains(d, "exit code 2") || !strings.Contains(d, "undefined symbol") {
		t.Fatalf("build diagnosis: %s", d)
	}
	if d := run(t, e, agent, "argowf.diagnose", map[string]any{"name": oom}); !strings.Contains(d, "OOM") {
		t.Fatalf("oom diagnosis: %s", d)
	}
	run(t, e, agent, "argowf.logs", map[string]any{"name": build, "node": "compile"})
	tpl := run(t, e, agent, "argowf.templates", map[string]any{})
	if !strings.Contains(tpl, `"name":"build"`) {
		t.Fatalf("templates: %s", tpl)
	}
	sub := run(t, e, agent, "argowf.submit", map[string]any{"template": "build", "parameters": map[string]string{"branch": "kind-test"}})
	var s struct{ Workflow string }
	json.Unmarshal([]byte(sub), &s)
	deadline := time.Now().Add(90 * time.Second)
	for {
		g := run(t, e, agent, "argowf.get", map[string]any{"name": s.Workflow})
		if strings.Contains(g, `"phase":"Succeeded"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("submitted workflow did not succeed: %s", g)
		}
		time.Sleep(3 * time.Second)
	}
	run(t, e, agent, "argowf.delete", map[string]any{"name": s.Workflow})
}

// TestKindArgoCDBrowserLogin runs argocd login --sso's flow against the real
// Dex: an HTTP client that follows redirects plays the browser (Dex's mock
// connector needs no user input).
func TestKindArgoCDBrowserLogin(t *testing.T) {
	dir := t.TempDir()
	s := &argo.Sessions{Store: argo.NewStore(filepath.Join(dir, "argo.json")), Secrets: &secrets.Memory{}}
	s.OpenBrowser = func(u string) error {
		go func() {
			resp, err := http.Get(u)
			if err != nil {
				t.Errorf("browser: %v", err)
				return
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if !strings.Contains(string(b), "Logged in") {
				t.Errorf("browser ended on %s: %.300s", resp.Request.URL, b)
			}
		}()
		return nil
	}
	reg := action.NewRegistry()
	(&Provider{Sessions: s, Artifacts: shaper.NewArtifacts(filepath.Join(dir, "artifacts"))}).Register(reg)
	e := &engine.Engine{Registry: reg, Events: eventlog.Open(filepath.Join(dir, "events"))}
	run(t, e, agent, "argo.add", map[string]any{"kind": "argocd", "name": "kind", "url": "http://localhost:8080"})
	out := run(t, e, agent, "argocd.login", map[string]any{"method": "browser"})
	if !strings.Contains(out, `"logged_in":true`) || !strings.Contains(out, `"method":"browser"`) || !strings.Contains(out, `"renewable":true`) {
		t.Fatalf("browser login: %s", out)
	}
	run(t, e, agent, "argocd.apps", map[string]any{})
}

// TestKindWorkflowDiagnoseFollowsIntoTheCluster wires the real Kubernetes
// provider (the cox-test kubeconfig only) and checks a failed step is
// followed to its pod or node.
func TestKindWorkflowDiagnoseFollowsIntoTheCluster(t *testing.T) {
	tok := os.Getenv("COX_ARGOWF_TOKEN")
	if tok == "" {
		t.Skip("COX_ARGOWF_TOKEN not set")
	}
	home, _ := os.UserHomeDir()
	kc := kube.NewKubeconfig([]string{filepath.Join(home, ".coxswain-test", "kind", "kubeconfig")})
	dir := t.TempDir()
	clusters := &kube.Clusters{Kubeconfig: kc, Meta: kube.NewMetaStore(filepath.Join(dir, "clusters.json"))}
	k8sp := &k8sprov.Provider{Clusters: clusters, Hosts: inventory.NewStore(filepath.Join(dir, "hosts.json")),
		Artifacts: shaper.NewArtifacts(filepath.Join(dir, "k8s-artifacts"))}
	s := &argo.Sessions{Store: argo.NewStore(filepath.Join(dir, "argo.json")), Secrets: &secrets.Memory{}}
	reg := action.NewRegistry()
	(&Provider{Sessions: s, Artifacts: shaper.NewArtifacts(filepath.Join(dir, "artifacts")), K8s: k8sp}).Register(reg)
	e := &engine.Engine{Registry: reg, Events: eventlog.Open(filepath.Join(dir, "events"))}
	run(t, e, human, "argo.add", map[string]any{"kind": "argowf", "name": "kind", "url": "http://localhost:2746", "namespace": "ci", "k8s_context": "kind-cox-test"})
	run(t, e, human, "argowf.login", map[string]any{"method": "token", "token": tok})
	list := run(t, e, agent, "argowf.list", map[string]any{"phase": "Failed"})
	var out WFListOutput
	json.Unmarshal([]byte(list), &out)
	for _, w := range out.Workflows {
		if strings.HasPrefix(w.Name, "build-") {
			d := run(t, e, agent, "argowf.diagnose", map[string]any{"name": w.Name})
			if !strings.Contains(d, "kind-cox-test") && !strings.Contains(d, "cox-test-control-plane") {
				t.Fatalf("not followed into the cluster: %s", d)
			}
			return
		}
	}
	t.Fatalf("no failed build: %s", list)
}

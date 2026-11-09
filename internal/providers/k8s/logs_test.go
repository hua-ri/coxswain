package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/engine"
	"github.com/hua-ri/coxswain/internal/eventlog"
	kube "github.com/hua-ri/coxswain/internal/k8s"
	"github.com/hua-ri/coxswain/internal/k8s/k8stest"
	"github.com/hua-ri/coxswain/internal/shaper"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilexec "k8s.io/client-go/util/exec"
)

// setupAPI registers the provider with context "fake" on a fake API
// server, through the real client.
func setupAPI(t *testing.T, api *k8stest.APIServer) fixture {
	t.Helper()
	path := k8stest.Isolate(t)
	content := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: fake
  cluster: {server: %s, certificate-authority-data: %s}
users:
- name: u
  user: {token: tok}
contexts:
- name: fake
  context: {cluster: fake, user: u}
`, api.URL, api.CAData())
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	coxHome := t.TempDir()
	kc := kube.NewKubeconfig(kube.DefaultPaths(os.Getenv("KUBECONFIG"), os.Getenv("HOME")))
	k8stest.Guard(t, kc.Paths()...)
	clusters := &kube.Clusters{Kubeconfig: kc, Meta: kube.NewMetaStore(filepath.Join(coxHome, "clusters.json"))}
	p := &Provider{Clusters: clusters, Artifacts: shaper.NewArtifacts(filepath.Join(coxHome, "artifacts")), Now: func() time.Time { return now }}
	reg := action.NewRegistry()
	p.Register(reg)
	return fixture{e: &engine.Engine{Registry: reg, Events: eventlog.Open(filepath.Join(coxHome, "events"))}, p: p, coxHome: coxHome, kubeconfig: path}
}

func servePod(api *k8stest.APIServer, pod *corev1.Pod) {
	pod.APIVersion, pod.Kind = "v1", "Pod"
	api.Handle("GET", "/api/v1/namespaces/"+pod.Namespace+"/pods/"+pod.Name, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(pod)
	})
}

func TestLogs(t *testing.T) {
	api := k8stest.NewAPIServer(t)
	servePod(api, runningPod("default", "web-1", "node-1"))
	var lines []string
	for i := range 1000 {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	api.Handle("GET", "/api/v1/namespaces/default/pods/web-1/log", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strings.Join(lines, "\n")+"\n")
	})
	f := setupAPI(t, api)

	res := f.invoke(t, codex, "k8s.logs", args("context", "fake", "pod", "web-1")).(LogsOutput)
	if res.Container != "app" || !res.Text.Truncated || res.Text.ArtifactID == "" || !strings.Contains(res.Text.Text, "line 999") {
		t.Fatalf("agent logs: %+v", res)
	}
	q := lastQuery(t, api, "/api/v1/namespaces/default/pods/web-1/log")
	if q.Get("tailLines") != "200" || q.Get("container") != "app" || q.Get("previous") == "true" {
		t.Fatalf("query %v", q)
	}
	if ev := f.requested(t, "k8s.logs"); ev.Risk != "read" || ev.Targets[0].ID != "k8s:fake/default" {
		t.Fatalf("audit %+v", ev)
	}

	full := f.invoke(t, desktop, "k8s.logs", args("context", "fake", "pod", "web-1", "previous", true, "tail", 5000, "since", "10m")).(LogsOutput)
	if full.Text.Truncated || full.Text.TotalLines != 1000 || !full.Previous {
		t.Fatalf("human logs: %+v", full.Text.TotalLines)
	}
	q = lastQuery(t, api, "/api/v1/namespaces/default/pods/web-1/log")
	if q.Get("tailLines") != "5000" || q.Get("previous") != "true" || q.Get("sinceSeconds") != "600" {
		t.Fatalf("query %v", q)
	}
}

func lastQuery(t *testing.T, api *k8stest.APIServer, path string) url.Values {
	t.Helper()
	reqs := api.Requests()
	for i := len(reqs) - 1; i >= 0; i-- {
		if reqs[i].Path == path {
			q, _ := url.ParseQuery(reqs[i].Query)
			return q
		}
	}
	t.Fatalf("no request to %s", path)
	return nil
}

func twoContainers(ns, name string) *corev1.Pod {
	p := runningPod(ns, name, "node-1")
	p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "sidecar", Image: "envoy"})
	return p
}

func TestLogsNeedAContainerChoice(t *testing.T) {
	f := setup(t, twoContainers("default", "web-1"))
	out := f.e.Invoke(context.Background(), codex, "k8s.logs", args("context", "dev", "pod", "web-1"))
	if out.Status != engine.StatusError || !strings.Contains(out.Error, "app, sidecar") {
		t.Fatalf("%+v", out)
	}
	out = f.e.Invoke(context.Background(), codex, "k8s.logs", args("context", "dev", "pod", "web-1", "container", "nope"))
	if out.Status != engine.StatusError || !strings.Contains(out.Error, "nope") {
		t.Fatalf("%+v", out)
	}
	res := f.invoke(t, codex, "k8s.logs", args("context", "dev", "pod", "web-1", "container", "sidecar")).(LogsOutput)
	if res.Container != "sidecar" || res.Text.Text != "fake logs" {
		t.Fatalf("%+v", res)
	}
}

func podMetrics(ns, name string, cpu, mem string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "metrics.k8s.io/v1beta1", "kind": "PodMetrics",
		"metadata":   map[string]any{"name": name, "namespace": ns},
		"containers": []any{map[string]any{"name": "app", "usage": map[string]any{"cpu": cpu, "memory": mem}}},
	}}
}

func nodeMetrics(name string, cpu, mem string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "metrics.k8s.io/v1beta1", "kind": "NodeMetrics",
		"metadata": map[string]any{"name": name},
		"usage":    map[string]any{"cpu": cpu, "memory": mem},
	}}
}

func TestTopWithoutMetrics(t *testing.T) {
	f := setup(t)
	out := f.e.Invoke(context.Background(), codex, "k8s.top", args("context", "dev", "kind", "pods"))
	if out.Status != engine.StatusError || !strings.Contains(out.Error, "metrics-server") {
		t.Fatalf("%+v", out)
	}
}

func TestTop(t *testing.T) {
	n := node("node-1", "10.1.0.5", true)
	n.Status.Allocatable = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("4Gi")}
	f := setup(t, n)
	f.fakes["dev"].WithMetrics(podMetrics("default", "web-1", "250m", "128Mi"), podMetrics("default", "api-1", "1500m", "64Mi"),
		podMetrics("other", "db-1", "10m", "1Gi"), nodeMetrics("node-1", "500m", "1Gi"))
	res := f.invoke(t, codex, "k8s.top", args("context", "dev", "kind", "pods")).(TopOutput)
	if strings.Join(res.Columns, ",") != "name,cpu,memory" || len(res.Rows) != 2 ||
		strings.Join(res.Rows[0], ",") != "api-1,1500m,64Mi" || strings.Join(res.Rows[1], ",") != "web-1,250m,128Mi" {
		t.Fatalf("pods: %+v", res)
	}
	res = f.invoke(t, codex, "k8s.top", args("context", "dev", "kind", "pod", "all_namespaces", true, "sort_by", "memory")).(TopOutput)
	if len(res.Rows) != 3 || strings.Join(res.Rows[0], ",") != "other,db-1,10m,1024Mi" {
		t.Fatalf("all pods by memory: %+v", res.Rows)
	}
	res = f.invoke(t, codex, "k8s.top", args("context", "dev", "kind", "nodes")).(TopOutput)
	if strings.Join(res.Columns, ",") != "name,cpu,cpu%,memory,memory%" || strings.Join(res.Rows[0], ",") != "node-1,500m,25%,1024Mi,25%" {
		t.Fatalf("nodes: %+v", res)
	}
	if out := f.e.Invoke(context.Background(), codex, "k8s.top", args("context", "dev", "kind", "deployments")); out.Status != engine.StatusError {
		t.Fatalf("top deployments: %+v", out)
	}
}

func TestExec(t *testing.T) {
	f := setup(t, runningPod("shop", "web-1", "node-1"), twoContainers("shop", "api-1"))
	fe := f.fakes["prod"].ExecCalls
	fe.Run = func(_ context.Context, req kube.ExecRequest) error {
		fmt.Fprintf(req.Stdout, "ran %q in %s\n", req.Command, req.Container)
		if req.Command[0] == "false" {
			io.WriteString(req.Stderr, "nope\n")
			return fmt.Errorf("command terminated: %w", utilexec.CodeExitError{Err: errors.New("exit 3"), Code: 3})
		}
		return nil
	}
	res := f.invoke(t, codex, "k8s.exec", args("context", "prod", "pod", "web-1", "command", "ls -l /")).(ExecOutput)
	if res.ExitCode != 0 || res.Stdout.Text != "ran [\"sh\" \"-c\" \"ls -l /\"] in app\n" || res.Container != "app" ||
		res.ContextTags["env"] != "prod" || res.ContextNote == "" {
		t.Fatalf("string command: %+v", res)
	}
	ev := f.requested(t, "k8s.exec")
	if ev.Risk != "read" || ev.Targets[0].ID != "k8s:prod/shop" || ev.Targets[0].Tags["env"] != "prod" {
		t.Fatalf("audit %+v", ev)
	}

	res = f.invoke(t, codex, "k8s.exec", args("context", "prod", "pod", "api-1", "container", "sidecar", "command", []string{"false", "x"})).(ExecOutput)
	if res.ExitCode != 3 || res.Stderr.Text != "nope\n" || fe.Calls()[1].Container != "sidecar" {
		t.Fatalf("array command: %+v", res)
	}

	for cmd, risk := range map[string]string{"rm -rf /data": "destructive", "touch /tmp/x": "write", "cat /etc/hosts": "read"} {
		f.invoke(t, codex, "k8s.exec", args("context", "prod", "pod", "web-1", "command", cmd))
		if ev := f.requested(t, "k8s.exec"); ev.Risk != risk {
			t.Errorf("%v: risk %s, want %s", cmd, ev.Risk, risk)
		}
	}
	f.invoke(t, codex, "k8s.exec", args("context", "prod", "pod", "web-1", "command", []string{"rm", "-rf", "/x y"}))
	if ev := f.requested(t, "k8s.exec"); ev.Risk != "destructive" {
		t.Errorf("array rm: risk %s", ev.Risk)
	}

	if out := f.e.Invoke(context.Background(), codex, "k8s.exec", args("context", "prod", "pod", "api-1", "command", "ls")); out.Status != engine.StatusError ||
		!strings.Contains(out.Error, "app, sidecar") {
		t.Fatalf("no container: %+v", out)
	}
	if out := f.e.Invoke(context.Background(), codex, "k8s.exec", args("context", "prod", "pod", "web-1", "command", "  ")); out.Status != engine.StatusError {
		t.Fatalf("empty command: %+v", out)
	}
	fe.Run = func(_ context.Context, req kube.ExecRequest) error { return errors.New("upgrade request failed") }
	if out := f.e.Invoke(context.Background(), codex, "k8s.exec", args("context", "prod", "pod", "web-1", "command", "ls")); out.Status != engine.StatusError ||
		!strings.Contains(out.Error, "upgrade") {
		t.Fatalf("stream error: %+v", out)
	}
}

func TestExecShapedAndTimedOut(t *testing.T) {
	f := setup(t, runningPod("default", "web-1", "node-1"))
	fe := f.fakes["dev"].ExecCalls
	fe.Run = func(ctx context.Context, req kube.ExecRequest) error {
		if req.Command[2] == "sleep" {
			<-ctx.Done()
			return ctx.Err()
		}
		for i := range 5000 {
			fmt.Fprintf(req.Stdout, "%d\n", i)
		}
		return nil
	}
	res := f.invoke(t, codex, "k8s.exec", args("context", "dev", "pod", "web-1", "command", "seq 5000")).(ExecOutput)
	if !res.Stdout.Truncated || res.Stdout.ArtifactID == "" {
		t.Fatalf("not shaped: %d lines", res.Stdout.TotalLines)
	}
	out := f.e.Invoke(context.Background(), codex, "k8s.exec", args("context", "dev", "pod", "web-1", "command", "sleep", "timeout_sec", 1))
	if out.Status != engine.StatusError || !strings.Contains(out.Error, "timed out") {
		t.Fatalf("timeout: %+v", out)
	}
}

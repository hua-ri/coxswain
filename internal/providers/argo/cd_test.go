package argo

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

const webApp = `{
 "metadata":{"name":"web","namespace":"argocd"},
 "spec":{"project":"default","source":{"repoURL":"https://git.example.com/web.git","path":"deploy","targetRevision":"main"},
  "destination":{"server":"https://kubernetes.default.svc","namespace":"shop"}},
 "status":{
  "sync":{"status":"OutOfSync","revision":"0123456789abcdef0123456789abcdef01234567"},
  "health":{"status":"Degraded","message":"Deployment web is not available"},
  "conditions":[{"type":"SyncError","message":"one or more objects failed to apply"}],
  "operationState":{"phase":"Failed","message":"one or more objects failed to apply",
    "finishedAt":"2026-11-13T10:00:00Z",
    "syncResult":{"revision":"0123456789abcdef0123456789abcdef01234567","resources":[
      {"kind":"ConfigMap","name":"web-config","status":"SyncFailed","message":"field is immutable"},
      {"kind":"Service","name":"web","status":"Synced"}]}},
  "history":[{"id":1,"revision":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","deployedAt":"2026-11-12T10:00:00Z"},
             {"id":2,"revision":"0123456789abcdef0123456789abcdef01234567","deployedAt":"2026-11-13T10:00:00Z","initiatedBy":{"username":"admin"}}],
  "resources":[
    {"kind":"Deployment","namespace":"shop","name":"web","status":"Synced","health":{"status":"Degraded","message":"not available"}},
    {"kind":"ConfigMap","namespace":"shop","name":"web-config","status":"OutOfSync"},
    {"kind":"Secret","namespace":"shop","name":"web-db","status":"OutOfSync"},
    {"kind":"Job","namespace":"shop","name":"old","status":"OutOfSync","requiresPruning":true}]}
}`

const webTree = `{"nodes":[
 {"kind":"Deployment","namespace":"shop","name":"web","health":{"status":"Degraded"}},
 {"kind":"ReplicaSet","namespace":"shop","name":"web-7d9","parentRefs":[{"kind":"Deployment","name":"web"}],"health":{"status":"Degraded"}},
 {"kind":"Pod","namespace":"shop","name":"web-7d9-abc","parentRefs":[{"kind":"ReplicaSet","name":"web-7d9"}],
  "info":[{"name":"Status Reason","value":"CrashLoopBackOff"},{"name":"Restart Count","value":"7"}],"health":{"status":"Degraded","message":"back-off restarting"}},
 {"kind":"Service","namespace":"shop","name":"web","health":{"status":"Healthy"}}]}`

// cdServer serves a logged-in argocd-server with the web app.
func (f *fixture) cdServer(t *testing.T) {
	t.Helper()
	f.argocd()
	f.mux.HandleFunc("GET /api/v1/applications", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"items":[%s,{"metadata":{"name":"api"},"spec":{"project":"default","destination":{}},"status":{"sync":{"status":"Synced"},"health":{"status":"Healthy"}}}]}`, webApp)
	})
	f.mux.HandleFunc("GET /api/v1/applications/web", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, webApp) })
	f.mux.HandleFunc("GET /api/v1/applications/web/resource-tree", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, webTree) })
	f.mux.HandleFunc("GET /api/v1/applications/web/events", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"items":[{"type":"Warning","reason":"BackOff","message":"Back-off restarting failed container","involvedObject":{"kind":"Pod","name":"web-7d9-abc"},"lastTimestamp":"2026-11-13T10:05:00Z"}]}`)
	})
	f.mux.HandleFunc("GET /api/v1/applications/web/logs", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("previous") == "true" {
			fmt.Fprint(w, `{"result":{"content":"panic: DB_URL not set","podName":"web-7d9-abc"}}`+"\n")
		} else {
			fmt.Fprint(w, `{"result":{"content":"starting","podName":"web-7d9-abc"}}`+"\n")
		}
		fmt.Fprint(w, `{"result":{"content":"","last":true}}`+"\n")
	})
	f.mux.HandleFunc("GET /api/v1/applications/web/managed-resources", func(w http.ResponseWriter, r *http.Request) {
		cm := func(v string) string {
			b, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "web-config", "resourceVersion": v}, "data": map[string]any{"mode": v}})
			return string(b)
		}
		sec := func(v string) string {
			b, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "web-db"}, "data": map[string]any{"password": v}})
			return string(b)
		}
		items := []map[string]any{
			{"kind": "ConfigMap", "namespace": "shop", "name": "web-config", "normalizedLiveState": cm("old"), "predictedLiveState": cm("new")},
			{"kind": "Secret", "namespace": "shop", "name": "web-db", "normalizedLiveState": sec("bGl2ZS1zZWNyZXQ="), "predictedLiveState": sec("Z2l0LXNlY3JldA==")},
			{"kind": "Service", "namespace": "shop", "name": "web", "normalizedLiveState": `{"kind":"Service"}`, "predictedLiveState": `{"kind":"Service"}`},
		}
		json.NewEncoder(w).Encode(map[string]any{"items": items})
	})
	f.mux.HandleFunc("POST /api/v1/applications/web/sync", func(w http.ResponseWriter, r *http.Request) {
		b := new(strings.Builder)
		fmt.Fprint(b, r.Body)
		fmt.Fprint(w, webApp)
	})
	f.ok(t, human, "argo.add", map[string]any{"kind": "argocd", "name": "dev", "url": f.server.URL, "note": "dev cluster"})
	f.ok(t, human, "argocd.login", map[string]any{"method": "token", "token": "session-token-abc"})
}

func TestCDApps(t *testing.T) {
	f := setup(t)
	f.cdServer(t)
	out := f.ok(t, agent, "argocd.apps", map[string]any{"only_issues": true})
	if !strings.Contains(out, `"name":"web"`) || strings.Contains(out, `"name":"api"`) || !strings.Contains(out, `"revision":"01234567"`) ||
		!strings.Contains(out, `"Synced/Healthy":1`) || !strings.Contains(out, "dev cluster") {
		t.Fatalf("apps = %s", out)
	}
}

func TestCDApp(t *testing.T) {
	f := setup(t)
	f.cdServer(t)
	out := f.ok(t, agent, "argocd.app", map[string]any{"app": "web"})
	for _, want := range []string{`"phase":"Failed"`, "ConfigMap/web-config SyncFailed: field is immutable", `"id":2`, `"by":"admin"`, `"requires_pruning":true`} {
		if !strings.Contains(out, want) {
			t.Errorf("app lacks %s: %s", want, out)
		}
	}
	if strings.Index(out, `"id":2`) > strings.Index(out, `"id":1`) {
		t.Error("history should be newest first")
	}
}

func TestCDDiffRedactsSecrets(t *testing.T) {
	f := setup(t)
	f.cdServer(t)
	out := f.ok(t, agent, "argocd.diff", map[string]any{"app": "web"})
	if strings.Contains(out, "bGl2ZS1zZWNyZXQ") || strings.Contains(out, "Z2l0LXNlY3JldA") {
		t.Fatalf("secret value in diff: %s", out)
	}
	if !strings.Contains(out, "-  mode: old") || !strings.Contains(out, "+  mode: new") || strings.Contains(out, "resourceVersion") || !strings.Contains(out, `"in_sync":1`) ||
		!strings.Contains(out, "secret values differ (redacted)") {
		t.Fatalf("diff = %s", out)
	}
}

func TestCDLogs(t *testing.T) {
	f := setup(t)
	f.cdServer(t)
	out := f.ok(t, agent, "argocd.logs", map[string]any{"app": "web", "pod": "web-7d9-abc", "previous": true})
	if !strings.Contains(out, "DB_URL not set") {
		t.Fatalf("logs = %s", out)
	}
	if o := f.call(t, agent, "argocd.logs", map[string]any{"app": "web"}); o.Status == "ok" {
		t.Fatal("logs without a pod or workload accepted")
	}
}

func TestCDDiagnose(t *testing.T) {
	f := setup(t)
	f.cdServer(t)
	out := f.ok(t, agent, "argocd.diagnose", map[string]any{"app": "web"})
	for _, want := range []string{"field is immutable", "Pod web-7d9-abc is Degraded", "DB_URL not set", "Back-off restarting",
		"no longer in Git", "previous container", "Deployment/web is Degraded"} {
		if !strings.Contains(out, want) {
			t.Errorf("diagnosis lacks %q: %s", want, out)
		}
	}
	if strings.Contains(out, "ReplicaSet/web-7d9 is") {
		t.Error("ReplicaSets should not be listed separately")
	}
}

func TestCDSyncRisk(t *testing.T) {
	f := setup(t)
	f.cdServer(t)
	f.ok(t, agent, "argocd.sync", map[string]any{"app": "web", "dry_run": true})
	f.ok(t, agent, "argocd.sync", map[string]any{"app": "web", "prune": true, "resources": []string{"ConfigMap:shop/web-config"}})
	audit := f.audit(t)
	if !strings.Contains(audit, `"args":{"app":"web","dry_run":true},"risk":"read"`) ||
		!strings.Contains(audit, `"resources":["ConfigMap:shop/web-config"]},"risk":"destructive"`) {
		t.Fatalf("audit = %s", audit)
	}
	var syncs []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "POST /api/v1/applications/web/sync") {
			syncs = append(syncs, c)
		}
	}
	if len(syncs) != 2 {
		t.Fatalf("sync calls = %v", f.calls)
	}
	if o := f.call(t, agent, "argocd.sync", map[string]any{"app": "web", "resources": []string{"nonsense"}}); o.Status == "ok" {
		t.Fatal("bad resource accepted")
	}
}

func TestCDRollbackChecksHistory(t *testing.T) {
	f := setup(t)
	f.cdServer(t)
	if o := f.call(t, agent, "argocd.rollback", map[string]any{"app": "web", "id": 9}); o.Status == "ok" || !strings.Contains(o.Error, "no deployment with id 9") {
		t.Fatalf("rollback to unknown id: %+v", o)
	}
}

func TestCDNeedsLogin(t *testing.T) {
	f := setup(t)
	f.cdServer(t)
	f.ok(t, human, "argocd.logout", map[string]any{})
	o := f.call(t, agent, "argocd.apps", map[string]any{})
	if o.Status == "ok" || !strings.Contains(o.Error, "needs login") || !strings.Contains(o.Error, "login_methods") {
		t.Fatalf("after logout: %+v", o)
	}
}

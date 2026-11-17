package argo

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/argo"
	"github.com/hua-ri/coxswain/internal/engine"
	"github.com/hua-ri/coxswain/internal/eventlog"
	"github.com/hua-ri/coxswain/internal/secrets"
	"github.com/hua-ri/coxswain/internal/shaper"
)

var (
	agent = action.Actor{Kind: action.Agent, Name: "codex"}
	human = action.Actor{Kind: action.Human, Name: "you"}
)

type fixture struct {
	e      *engine.Engine
	s      *argo.Sessions
	dir    string
	server *httptest.Server
	mux    *http.ServeMux
	mu     sync.Mutex
	calls  []string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{dir: dir, mux: http.NewServeMux()}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls = append(f.calls, r.Method+" "+r.URL.RequestURI())
		f.mu.Unlock()
		f.mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.server.Close)
	f.s = &argo.Sessions{Store: argo.NewStore(filepath.Join(dir, "argo.json")), Secrets: &secrets.Memory{}}
	f.withK8s(nil)
	return f
}

// withK8s (re)builds the engine with a Kubernetes provider stand-in.
func (f *fixture) withK8s(k K8s) {
	reg := action.NewRegistry()
	p := &Provider{Sessions: f.s, Artifacts: shaper.NewArtifacts(filepath.Join(f.dir, "artifacts")),
		Now: func() time.Time { return time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC) }}
	if k != nil {
		p.K8s = k
	}
	p.Register(reg)
	f.e = &engine.Engine{Registry: reg, Events: eventlog.Open(filepath.Join(f.dir, "events"))}
}

func args(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func (f *fixture) call(t *testing.T, who action.Actor, id string, in any) engine.Outcome {
	t.Helper()
	return f.e.Invoke(context.Background(), who, id, args(in))
}

func (f *fixture) ok(t *testing.T, who action.Actor, id string, in any) string {
	t.Helper()
	out := f.call(t, who, id, in)
	if out.Status != "ok" {
		t.Fatalf("%s: %s", id, out.Error)
	}
	b, _ := json.Marshal(out.Result)
	return string(b)
}

// audit is everything the event log holds.
func (f *fixture) audit(t *testing.T) string {
	var all strings.Builder
	filepath.Walk(filepath.Join(f.dir, "events"), func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			b, _ := os.ReadFile(p)
			all.Write(b)
		}
		return nil
	})
	return all.String()
}

// argocd fakes the session endpoints of an argocd-server.
func (f *fixture) argocd() {
	f.mux.HandleFunc("GET /api/v1/settings", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"url":""}`) })
	f.mux.HandleFunc("POST /api/v1/session", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		json.NewDecoder(r.Body).Decode(&in)
		if in["password"] != "hunter2-secret" {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"message":"Invalid username or password"}`)
			return
		}
		fmt.Fprint(w, `{"token":"session-token-abc"}`)
	})
	f.mux.HandleFunc("GET /api/v1/session/userinfo", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer session-token-abc" {
			w.WriteHeader(401)
			return
		}
		fmt.Fprint(w, `{"loggedIn":true,"username":"admin"}`)
	})
}

func TestAgentLogsInButNeverSeesSecrets(t *testing.T) {
	f := setup(t)
	f.argocd()
	f.ok(t, agent, "argo.add", map[string]any{"kind": "argocd", "name": "dev", "url": f.server.URL})
	methods := f.ok(t, agent, "argocd.login_methods", map[string]any{"instance": "dev"})
	if !strings.Contains(methods, `"password"`) || strings.Contains(methods, `"browser"`) {
		t.Fatalf("methods = %s", methods)
	}
	// encoding/json would match "Password" too; it is refused, and not logged.
	if out := f.call(t, agent, "argocd.login", map[string]any{"instance": "dev", "method": "password", "username": "admin", "Password": "hunter2-secret"}); out.Status == "ok" {
		t.Fatal("mis-cased key accepted")
	}
	out := f.ok(t, agent, "argocd.login", map[string]any{"instance": "dev", "method": "password", "username": "admin", "password": "hunter2-secret"})
	if !strings.Contains(out, `"logged_in":true`) {
		t.Fatalf("login = %s", out)
	}
	for _, view := range []string{out, f.ok(t, agent, "argocd.instances", map[string]any{}), f.ok(t, human, "argo.export", map[string]any{}), f.audit(t)} {
		if strings.Contains(view, "hunter2") || strings.Contains(view, "session-token-abc") {
			t.Fatalf("secret leaked: %s", view)
		}
	}
	if !strings.Contains(f.audit(t), "redacted 14 chars") {
		t.Fatal("audit lacks the redaction note")
	}
	// A wrong password fails without echoing it.
	bad := f.call(t, agent, "argocd.login", map[string]any{"instance": "dev", "method": "password", "username": "admin", "password": "wrong-pw-123"})
	if bad.Status == "ok" || strings.Contains(bad.Error, "wrong-pw-123") || strings.Contains(f.audit(t), "wrong-pw-123") {
		t.Fatalf("bad login: %+v", bad)
	}
}

func TestAgentTokenLoginWorkflows(t *testing.T) {
	f := setup(t)
	f.mux.HandleFunc("GET /api/v1/workflows/{ns}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k8s-token-xyz" {
			w.WriteHeader(401)
			return
		}
		fmt.Fprint(w, `{"items":null}`)
	})
	f.mux.HandleFunc("GET /api/v1/userinfo", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"serviceAccountName":"ops","serviceAccountNamespace":"argo"}`)
	})
	if out := f.call(t, agent, "argo.add", map[string]any{"kind": "argowf", "name": "wf", "url": f.server.URL}); out.Status == "ok" {
		t.Fatal("argowf without namespace accepted")
	}
	f.ok(t, agent, "argo.add", map[string]any{"kind": "argowf", "name": "wf", "url": f.server.URL, "namespace": "argo"})
	out := f.ok(t, agent, "argowf.login", map[string]any{"method": "token", "token": "Bearer k8s-token-xyz"})
	if !strings.Contains(out, "system:serviceaccount:argo:ops") {
		t.Fatalf("login = %s", out)
	}
	if strings.Contains(f.audit(t)+out+f.ok(t, agent, "argowf.instances", map[string]any{}), "k8s-token-xyz") {
		t.Fatal("token leaked")
	}
}

func TestInstanceLifecycle(t *testing.T) {
	f := setup(t)
	f.argocd()
	f.ok(t, human, "argo.add", map[string]any{"kind": "argocd", "name": "a", "url": f.server.URL, "tags": []string{"env=prod"}, "note": "be careful"})
	if out := f.call(t, human, "argo.add", map[string]any{"kind": "argocd", "name": "a", "url": "https://x"}); out.Status == "ok" {
		t.Fatal("duplicate added")
	}
	f.ok(t, human, "argocd.login", map[string]any{"instance": "a", "method": "token", "token": "session-token-abc"})
	f.ok(t, human, "argo.update", map[string]any{"kind": "argocd", "name": "a", "new_name": "b"})
	list := f.ok(t, human, "argocd.instances", map[string]any{})
	if !strings.Contains(list, `"name":"b"`) || !strings.Contains(list, `"logged_in":true`) || !strings.Contains(list, "be careful") {
		t.Fatalf("after rename: %s", list)
	}
	exported := f.ok(t, human, "argo.export", map[string]any{})
	f.ok(t, human, "argo.remove", map[string]any{"kind": "argocd", "name": "b"})
	if _, err := f.s.Secrets.Get("argocd/b"); err == nil {
		t.Fatal("credential kept after remove")
	}
	res := f.ok(t, human, "argo.import", map[string]any{"json": exported})
	if !strings.Contains(res, `"argocd:b"`) || !strings.Contains(f.ok(t, human, "argocd.instances", map[string]any{}), `"logged_in":false`) {
		t.Fatalf("import = %s", res)
	}
	res = f.ok(t, human, "argo.import", map[string]any{"json": exported})
	if !strings.Contains(res, "skipped") {
		t.Fatalf("re-import = %s", res)
	}
}

func TestNoteChangeIsDestructive(t *testing.T) {
	f := setup(t)
	f.ok(t, human, "argo.add", map[string]any{"kind": "argocd", "name": "a", "url": "https://cd.example.com"})
	f.ok(t, agent, "argo.update", map[string]any{"kind": "argocd", "name": "a", "note": "agents may sync"})
	if !strings.Contains(f.audit(t), `"risk":"destructive"`) {
		t.Fatal("changing the note should be labelled destructive")
	}
}

func TestInstanceRequiredWhenSeveral(t *testing.T) {
	f := setup(t)
	f.ok(t, human, "argo.add", map[string]any{"kind": "argocd", "name": "a", "url": "https://a.example.com"})
	f.ok(t, human, "argo.add", map[string]any{"kind": "argocd", "name": "b", "url": "https://b.example.com"})
	out := f.call(t, agent, "argocd.logout", map[string]any{})
	if out.Status == "ok" || !strings.Contains(out.Error, "instance is required") {
		t.Fatalf("logout without instance: %+v", out)
	}
}

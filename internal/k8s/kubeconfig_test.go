package k8s_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hua-ri/coxswain/internal/k8s"
	"github.com/hua-ri/coxswain/internal/k8s/k8stest"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

func TestMain(m *testing.M) { k8stest.Main(m) }

// kubeconfig writes a kubeconfig file and returns a Kubeconfig on it
// that re-checks the file on every load.
func kubeconfig(t *testing.T, path, content string) *k8s.Kubeconfig {
	t.Helper()
	k8stest.Guard(t, path)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	kc := k8s.NewKubeconfig([]string{path})
	kc.Interval = 0
	k8stest.Guard(t, kc.Paths()...)
	return kc
}

const twoContexts = `apiVersion: v1
kind: Config
current-context: dev
clusters:
- name: c1
  cluster: {server: https://dev.example:6443}
- name: c2
  cluster: {server: https://prod.example:6443}
users:
- name: u1
  user: {token: t1}
contexts:
- name: dev
  context: {cluster: c1, user: u1, namespace: web}
- name: prod
  context: {cluster: c2, user: u1}
`

func TestDefaultPaths(t *testing.T) {
	home := "/home/x"
	if got := k8s.DefaultPaths("", home); len(got) != 1 || got[0] != "/home/x/.kube/config" {
		t.Fatalf("default: %v", got)
	}
	got := k8s.DefaultPaths("/a/one"+string(filepath.ListSeparator)+string(filepath.ListSeparator)+"/b/two"+
		string(filepath.ListSeparator)+"/a/one", home)
	if strings.Join(got, ",") != "/a/one,/b/two" {
		t.Fatalf("KUBECONFIG list: %v", got)
	}
}

func TestIsolateAndGuard(t *testing.T) {
	path := k8stest.Isolate(t)
	if os.Getenv("KUBECONFIG") != path || !strings.HasPrefix(path, os.Getenv("HOME")) {
		t.Fatalf("not isolated: %s %s", os.Getenv("KUBECONFIG"), os.Getenv("HOME"))
	}
	// The guard must catch the real user's kubeconfig (HOME is a temp
	// dir now; the passwd entry still names the real home).
	u, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	rec := &recorder{}
	k8stest.Guard(rec, filepath.Join(u.HomeDir, ".kube", "config"))
	k8stest.Guard(rec, filepath.Join(u.HomeDir, ".kube", "other", "..", "config"))
	if rec.failed != 2 {
		t.Fatal("guard let the real ~/.kube/config through")
	}
	rec = &recorder{}
	k8stest.Guard(rec, path)
	if rec.failed != 0 {
		t.Fatal("guard refused a temp kubeconfig")
	}
}

type recorder struct{ failed int }

func (r *recorder) Helper()               {}
func (r *recorder) Fatalf(string, ...any) { r.failed++ }

func TestBeforeReadRefuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	kc := kubeconfig(t, path, twoContexts)
	prev := k8s.BeforeRead
	t.Cleanup(func() { k8s.BeforeRead = prev })
	k8s.BeforeRead = func(p string) error { return fmt.Errorf("no reading %s", p) }
	if _, _, err := kc.Load(); err == nil || !strings.Contains(err.Error(), "no reading") {
		t.Fatalf("load went ahead: %v", err)
	}
}

func TestLoadMergesLikeKubectl(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	os.WriteFile(a, []byte(twoContexts), 0o600)
	os.WriteFile(b, []byte(strings.ReplaceAll(strings.ReplaceAll(twoContexts, "current-context: dev", "current-context: prod"),
		"namespace: web", "namespace: other")+
		"- name: extra\n  context: {cluster: c1, user: u1}\n"), 0o600)
	kc := k8s.NewKubeconfig([]string{a, b, filepath.Join(dir, "missing")})
	k8stest.Guard(t, kc.Paths()...)
	cfg, _, err := kc.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CurrentContext != "dev" || cfg.Contexts["dev"].Namespace != "web" || cfg.Contexts["extra"] == nil {
		t.Fatalf("merge: current %q dev %+v extra %v", cfg.CurrentContext, cfg.Contexts["dev"], cfg.Contexts["extra"])
	}
}

func TestReloadsWhenTheFileChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	kc := kubeconfig(t, path, twoContexts)
	cl := &k8s.Clusters{Kubeconfig: kc, Meta: k8s.NewMetaStore(filepath.Join(t.TempDir(), "clusters.json"))}
	built := 0
	cl.NewClient = func(name string, rc *rest.Config) (*k8s.Client, error) {
		built++
		return &k8s.Client{Context: name}, nil
	}
	c1, err := cl.Client("dev")
	if err != nil {
		t.Fatal(err)
	}
	if c1.Namespace != "web" {
		t.Fatalf("default namespace %q", c1.Namespace)
	}
	if c2, _ := cl.Client("dev"); c2 != c1 || built != 1 {
		t.Fatalf("client not cached: built %d", built)
	}
	os.WriteFile(path, []byte(twoContexts+"- name: added\n  context: {cluster: c1, user: u1}\n"), 0o600)
	_, list, err := cl.Contexts()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("not reloaded: %+v", list)
	}
	if c3, _ := cl.Client("dev"); c3 == c1 || built != 2 {
		t.Fatalf("client kept after reload: built %d", built)
	}
	if _, err := cl.Client("nope"); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("unknown context: %v", err)
	}
}

func TestReloadIsThrottled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	kc := kubeconfig(t, path, twoContexts)
	now := time.Unix(1000, 0)
	kc.Interval = 5 * time.Second
	kc.Now = func() time.Time { return now }
	if _, gen, _ := kc.Load(); gen != 1 {
		t.Fatalf("gen %d", gen)
	}
	os.WriteFile(path, []byte(twoContexts+"- name: added\n  context: {cluster: c1, user: u1}\n"), 0o600)
	if _, gen, _ := kc.Load(); gen != 1 {
		t.Fatalf("checked again within the interval: gen %d", gen)
	}
	now = now.Add(5 * time.Second)
	if cfg, gen, _ := kc.Load(); gen != 2 || cfg.Contexts["added"] == nil {
		t.Fatalf("not reloaded after the interval: gen %d", gen)
	}
}

func TestContextsCarryTagsAndNotes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	meta := k8s.NewMetaStore(filepath.Join(t.TempDir(), "clusters.json"))
	meta.Update(func(m map[string]k8s.Meta) error {
		m["prod"] = k8s.Meta{Tags: map[string]string{"env": "prod"}, Note: "ask first"}
		return nil
	})
	cl := &k8s.Clusters{Kubeconfig: kubeconfig(t, path, twoContexts), Meta: meta}
	current, list, err := cl.Contexts()
	if err != nil {
		t.Fatal(err)
	}
	if current != "dev" || len(list) != 2 {
		t.Fatalf("%q %+v", current, list)
	}
	dev, prod := list[0], list[1]
	if !dev.Current || dev.Namespace != "web" || dev.Server != "https://dev.example:6443" || dev.Cluster != "c1" {
		t.Fatalf("dev: %+v", dev)
	}
	if prod.Namespace != "default" || prod.Tags["env"] != "prod" || prod.Note != "ask first" {
		t.Fatalf("prod: %+v", prod)
	}
	got, err := cl.Context("prod")
	if err != nil || got.Tags["env"] != "prod" {
		t.Fatalf("Context: %+v %v", got, err)
	}
	if tg := cl.Target("prod", "kube-system"); tg.ID != "k8s:prod/kube-system" || tg.Kind != "k8s" || tg.Tags["env"] != "prod" {
		t.Fatalf("target %+v", tg)
	}
	if tg := cl.Target("prod", ""); tg.ID != "k8s:prod" {
		t.Fatalf("cluster target %+v", tg)
	}
}

func TestMetaStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clusters.json")
	s := k8s.NewMetaStore(path)
	if m, err := s.Get("x"); err != nil || len(m.Tags) != 0 {
		t.Fatalf("empty: %+v %v", m, err)
	}
	if err := s.Update(func(m map[string]k8s.Meta) error {
		m["x"] = k8s.Meta{Tags: map[string]string{"env": "prod"}, Note: "n"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if m, _ := s.Get("x"); m.Tags["env"] != "prod" || m.Note != "n" {
		t.Fatalf("%+v", m)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode: %v %v", st, err)
	}
	b, _ := os.ReadFile(path)
	var raw map[string]map[string]any
	if err := json.Unmarshal(b, &raw); err != nil || raw["x"]["note"] != "n" {
		t.Fatalf("format: %s %v", b, err)
	}
}

// jwt returns an unsigned JWT that expires at exp.
func jwt(exp time.Time) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(fmt.Sprintf(`{"exp":%d}`, exp.Unix()))) + ".sig"
}

// TestKubeconfigNeverWritten is the red line: loading the kubeconfig,
// building clients and making requests (with a static token, an exec
// credential plugin and an OIDC auth provider whose token is refreshed)
// leaves the file byte-identical with the same mtime.
func TestKubeconfigNeverWritten(t *testing.T) {
	api := k8stest.NewAPIServer(t)
	fresh := jwt(time.Now().Add(time.Hour))
	api.Handle("GET", "/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"issuer": api.URL, "token_endpoint": api.URL + "/token"})
	})
	api.Handle("POST", "/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "a", "token_type": "Bearer", "expires_in": 3600,
			"id_token": fresh, "refresh_token": "rotated-refresh"})
	})
	dir := t.TempDir()
	plugin := filepath.Join(dir, "cred-plugin")
	os.WriteFile(plugin, []byte("#!/bin/sh\necho '{\"apiVersion\":\"client.authentication.k8s.io/v1\",\"kind\":\"ExecCredential\",\"status\":{\"token\":\"plugin-token\"}}'\n"), 0o700)
	path := filepath.Join(dir, "config")
	content := fmt.Sprintf(`apiVersion: v1
kind: Config
current-context: token
clusters:
- name: fake
  cluster: {server: %[1]s, certificate-authority-data: %[4]s}
users:
- name: token
  user: {token: static-token}
- name: exec
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: %[2]s
      interactiveMode: IfAvailable
- name: oidc
  user:
    auth-provider:
      name: oidc
      config:
        idp-issuer-url: %[1]s
        idp-certificate-authority-data: %[4]s
        client-id: cox-test
        client-secret: s
        id-token: %[3]s
        refresh-token: old-refresh
contexts:
- name: token
  context: {cluster: fake, user: token}
- name: exec
  context: {cluster: fake, user: exec}
- name: oidc
  context: {cluster: fake, user: oidc}
`, api.URL, plugin, jwt(time.Now().Add(-time.Hour)), api.CAData())
	kc := kubeconfig(t, path, content)
	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)

	cl := &k8s.Clusters{Kubeconfig: kc, Meta: k8s.NewMetaStore(filepath.Join(dir, "clusters.json"))}
	for ctxName, auth := range map[string]string{"token": "Bearer static-token", "exec": "Bearer plugin-token", "oidc": "Bearer " + fresh} {
		c, err := cl.Client(ctxName)
		if err != nil {
			t.Fatalf("%s: %v", ctxName, err)
		}
		if c.REST.AuthConfigPersister != nil {
			t.Fatalf("%s: client could persist auth config", ctxName)
		}
		if _, err := c.Typed.CoreV1().Namespaces().List(context.Background(), metav1.ListOptions{}); err != nil {
			t.Fatalf("%s: list: %v", ctxName, err)
		}
		if _, err := c.Resources.Resolve("deploy"); err != nil {
			t.Fatalf("%s: discovery: %v", ctxName, err)
		}
		reqs := api.Requests()
		if last := reqs[len(reqs)-1]; last.Auth != auth {
			t.Fatalf("%s: auth %q, want %q", ctxName, last.Auth, auth)
		}
	}
	var refreshed bool
	for _, r := range api.Requests() {
		refreshed = refreshed || r.Path == "/token"
	}
	if !refreshed {
		t.Fatal("the OIDC token was not refreshed; the test proves nothing")
	}
	after, _ := os.ReadFile(path)
	st, _ := os.Stat(path)
	if !bytes.Equal(before, after) || !st.ModTime().Equal(old) {
		t.Fatalf("kubeconfig changed (mtime %v):\n%s", st.ModTime(), after)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if n := e.Name(); n != "config" && n != "cred-plugin" && n != "clusters.json" {
			t.Fatalf("unexpected file %s next to the kubeconfig", n)
		}
	}
}

func TestExecPluginThatNeedsALogin(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	content := `apiVersion: v1
kind: Config
clusters:
- name: fake
  cluster: {server: https://127.0.0.1:1}
users:
- name: sso
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: /bin/false
      interactiveMode: Always
contexts:
- name: sso
  context: {cluster: fake, user: sso}
`
	cl := &k8s.Clusters{Kubeconfig: kubeconfig(t, path, content), Meta: k8s.NewMetaStore(filepath.Join(dir, "m.json"))}
	c, err := cl.Client("sso")
	if err == nil {
		_, err = c.Typed.CoreV1().Namespaces().List(context.Background(), metav1.ListOptions{})
	}
	if err = k8s.Explain(err); err == nil || !strings.Contains(err.Error(), "log in") {
		t.Fatalf("want a hint to log in first, got %v", err)
	}
}

func TestMissingKubeconfig(t *testing.T) {
	kc := k8s.NewKubeconfig([]string{filepath.Join(t.TempDir(), "none")})
	k8stest.Guard(t, kc.Paths()...)
	cl := &k8s.Clusters{Kubeconfig: kc, Meta: k8s.NewMetaStore(filepath.Join(t.TempDir(), "m.json"))}
	_, list, err := cl.Contexts()
	if err != nil || len(list) != 0 {
		t.Fatalf("%+v %v", list, err)
	}
	if _, err := cl.Client("x"); !errors.Is(err, k8s.ErrNoContext) {
		t.Fatalf("%v", err)
	}
}

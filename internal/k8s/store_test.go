package k8s_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hua-ri/coxswain/internal/k8s"
	"github.com/hua-ri/coxswain/internal/k8s/k8stest"
	"k8s.io/client-go/tools/clientcmd"
)

// pasted has two contexts whose cluster and user names collide with
// twoContexts' (c1, u1): stored copies must not be mixed up with them.
const pasted = `apiVersion: v1
kind: Config
current-context: staging
clusters:
- name: c1
  cluster: {server: https://staging.example:6443}
- name: c9
  cluster: {server: https://other.example:6443}
users:
- name: u1
  user: {token: secret-staging-token}
contexts:
- name: staging
  context: {cluster: c1, user: u1, namespace: shop}
- name: other
  context: {cluster: c9, user: u1}
`

type storeFixture struct {
	clusters *k8s.Clusters
	store    string // Coxswain's kubeconfigs dir
	user     string // the user's kubeconfig
	before   []byte
	mtime    time.Time
}

func newStoreFixture(t *testing.T) *storeFixture {
	t.Helper()
	path := k8stest.Isolate(t)
	if err := os.WriteFile(path, []byte(twoContexts), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	os.Chtimes(path, old, old)
	cox := t.TempDir()
	kc := k8s.NewKubeconfig(k8s.DefaultPaths(os.Getenv("KUBECONFIG"), os.Getenv("HOME")))
	kc.Store = k8s.NewStore(filepath.Join(cox, "kubeconfigs"))
	k8stest.Guard(t, kc.Paths()...)
	f := &storeFixture{
		clusters: &k8s.Clusters{Kubeconfig: kc, Meta: k8s.NewMetaStore(filepath.Join(cox, "clusters.json"))},
		store:    filepath.Join(cox, "kubeconfigs"), user: path, before: []byte(twoContexts), mtime: old,
	}
	t.Cleanup(func() {
		b, err := os.ReadFile(path)
		st, _ := os.Stat(path)
		if err != nil || string(b) != string(f.before) || !st.ModTime().Equal(f.mtime) {
			t.Errorf("red line: the user's kubeconfig changed")
		}
		entries, _ := os.ReadDir(filepath.Dir(path))
		if len(entries) != 1 {
			t.Errorf("red line: files appeared in ~/.kube: %v", entries)
		}
	})
	return f
}

func (f *storeFixture) source(t *testing.T, name string) string {
	t.Helper()
	_, list, err := f.clusters.Contexts()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range list {
		if c.Name == name {
			return c.Source + " " + c.Server
		}
	}
	return ""
}

func TestAddClusterStoresOneContext(t *testing.T) {
	f := newStoreFixture(t)
	f.clusters.Contexts() // loaded once before: the add must show at once
	ci, err := f.clusters.AddCluster(k8s.ClusterSource{YAML: []byte(pasted)}, "staging", "")
	if err != nil {
		t.Fatal(err)
	}
	if ci.Name != "staging" || ci.Source != "coxswain" || ci.Server != "https://staging.example:6443" || ci.Namespace != "shop" {
		t.Fatalf("%+v", ci)
	}
	if got := f.source(t, "staging"); got != "coxswain https://staging.example:6443" {
		t.Fatalf("staging: %q", got)
	}
	if got := f.source(t, "dev"); got != "kubeconfig https://dev.example:6443" {
		t.Fatalf("the user's dev changed: %q", got)
	}
	if f.source(t, "other") != "" {
		t.Fatal("the other context was stored too")
	}
	st, err := os.Stat(f.store)
	if err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("dir %v %v", st, err)
	}
	files, _ := filepath.Glob(filepath.Join(f.store, "*.yaml"))
	if len(files) != 1 {
		t.Fatalf("files %v", files)
	}
	if st, _ := os.Stat(files[0]); st.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v", st.Mode())
	}
	cfg, err := clientcmd.LoadFromFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CurrentContext != "" || len(cfg.Contexts) != 1 || len(cfg.Clusters) != 1 || len(cfg.AuthInfos) != 1 {
		t.Fatalf("stored: %+v", cfg)
	}
	// The current context stays the user's.
	if cur, _, _ := f.clusters.Contexts(); cur != "dev" {
		t.Fatalf("current %q", cur)
	}
	// Again: already there.
	if _, err := f.clusters.AddCluster(k8s.ClusterSource{YAML: []byte(pasted)}, "staging", ""); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("second add: %v", err)
	}
}

func TestAddClusterCollisionAndRename(t *testing.T) {
	f := newStoreFixture(t)
	collide := strings.ReplaceAll(pasted, "name: staging", "name: dev")
	_, err := f.clusters.AddCluster(k8s.ClusterSource{YAML: []byte(collide)}, "dev", "")
	if err == nil || !strings.Contains(err.Error(), "your kubeconfig") || !strings.Contains(err.Error(), "rename") {
		t.Fatalf("collision: %v", err)
	}
	ci, err := f.clusters.AddCluster(k8s.ClusterSource{YAML: []byte(collide)}, "dev", "dev-copy")
	if err != nil || ci.Name != "dev-copy" || ci.Server != "https://staging.example:6443" {
		t.Fatalf("renamed: %+v %v", ci, err)
	}
	if got := f.source(t, "dev"); got != "kubeconfig https://dev.example:6443" {
		t.Fatalf("dev: %q", got)
	}
	for _, bad := range []string{"has space", "", "tab\tname"} {
		if _, err := f.clusters.AddCluster(k8s.ClusterSource{YAML: []byte(collide)}, "dev", bad+"x"); bad != "" && err == nil {
			t.Errorf("name %q accepted", bad)
		}
	}
}

func TestRemoveCluster(t *testing.T) {
	f := newStoreFixture(t)
	if _, err := f.clusters.AddCluster(k8s.ClusterSource{YAML: []byte(pasted)}, "staging", ""); err != nil {
		t.Fatal(err)
	}
	f.clusters.Meta.Update(func(m map[string]k8s.Meta) error {
		m["staging"] = k8s.Meta{Tags: map[string]string{"env": "staging"}}
		return nil
	})
	if err := f.clusters.RemoveCluster("dev"); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("removed the user's context: %v", err)
	}
	if err := f.clusters.RemoveCluster("nope"); err == nil {
		t.Fatal("removed nothing")
	}
	if err := f.clusters.RemoveCluster("staging"); err != nil {
		t.Fatal(err)
	}
	if f.source(t, "staging") != "" {
		t.Fatal("still listed")
	}
	if files, _ := filepath.Glob(filepath.Join(f.store, "*.yaml")); len(files) != 0 {
		t.Fatalf("left %v", files)
	}
	if m, _ := f.clusters.Meta.Get("staging"); len(m.Tags) != 0 {
		t.Fatalf("tags left: %+v", m)
	}
}

func TestAddClusterChecks(t *testing.T) {
	f := newStoreFixture(t)
	add := func(yaml, ctx string) error {
		_, err := f.clusters.AddCluster(k8s.ClusterSource{YAML: []byte(yaml)}, ctx, "")
		return err
	}
	if err := add(pasted, ""); err == nil || !strings.Contains(err.Error(), "staging, other") && !strings.Contains(err.Error(), "other, staging") {
		t.Fatalf("no context chosen: %v", err)
	}
	if err := add("not: [yaml", ""); err == nil {
		t.Fatal("bad yaml accepted")
	}
	if err := add(pasted, "nope"); err == nil {
		t.Fatal("missing context accepted")
	}
	gcp := strings.Replace(pasted, "user: {token: secret-staging-token}", "user: {auth-provider: {name: gcp}}", 1)
	if err := add(gcp, "staging"); err == nil || !strings.Contains(err.Error(), "gcp") {
		t.Fatalf("gcp auth provider: %v", err)
	}
	rel := strings.Replace(pasted, "cluster: {server: https://staging.example:6443}", "cluster: {server: https://staging.example:6443, certificate-authority: ca.crt}", 1)
	if err := add(rel, "staging"); err == nil || !strings.Contains(err.Error(), "relative") {
		t.Fatalf("relative path in pasted YAML: %v", err)
	}
	// From a file, relative paths are resolved against it.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ca.crt"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(dir, "config"), []byte(rel), 0o600)
	ci, err := f.clusters.AddCluster(k8s.ClusterSource{Path: filepath.Join(dir, "config")}, "staging", "")
	if err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(f.store, "*.yaml"))
	b, _ := os.ReadFile(files[0])
	if !strings.Contains(string(b), filepath.Join(dir, "ca.crt")) || ci.Source != "coxswain" {
		t.Fatalf("stored: %s", b)
	}
	exec := strings.Replace(pasted, "user: {token: secret-staging-token}", "user: {exec: {apiVersion: client.authentication.k8s.io/v1, command: kubelogin}}", 1)
	if _, err := f.clusters.AddCluster(k8s.ClusterSource{YAML: []byte(exec)}, "other", ""); err != nil {
		t.Fatalf("exec plugin: %v", err)
	}
}

func TestInspect(t *testing.T) {
	f := newStoreFixture(t)
	list, err := f.clusters.Inspect(k8s.ClusterSource{YAML: []byte(strings.ReplaceAll(pasted, "name: other", "name: dev"))})
	if err != nil || len(list) != 2 {
		t.Fatalf("%+v %v", list, err)
	}
	byName := map[string]k8s.Candidate{}
	for _, c := range list {
		byName[c.Name] = c
	}
	if c := byName["staging"]; c.Server != "https://staging.example:6443" || c.Auth != "token" || c.Exists != "" || !c.Current {
		t.Fatalf("staging: %+v", c)
	}
	if c := byName["dev"]; c.Exists != "kubeconfig" {
		t.Fatalf("dev: %+v", c)
	}
}

func TestProbe(t *testing.T) {
	f := newStoreFixture(t)
	srv := k8stest.NewAPIServer(t)
	srv.Handle("GET", "/version", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, `{"kind":"Status","code":401,"message":"Unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"major":"1","minor":"31","gitVersion":"v1.31.2"}`))
	})
	cfg := func(token string) string {
		return "apiVersion: v1\nkind: Config\nclusters:\n- name: c\n  cluster: {server: " + srv.URL +
			", certificate-authority-data: " + srv.CAData() + "}\nusers:\n- name: u\n  user: {token: " + token +
			"}\ncontexts:\n- name: t\n  context: {cluster: c, user: u}\n"
	}
	v, err := f.clusters.Probe(context.Background(), k8s.ClusterSource{YAML: []byte(cfg("tok"))}, "")
	if err != nil || v.GitVersion != "v1.31.2" {
		t.Fatalf("%+v %v", v, err)
	}
	if _, err := f.clusters.Probe(context.Background(), k8s.ClusterSource{YAML: []byte(cfg("bad"))}, ""); err == nil ||
		!strings.Contains(err.Error(), "log in") {
		t.Fatalf("bad token: %v", err)
	}
	// Nothing was stored.
	if files, _ := filepath.Glob(filepath.Join(f.store, "*")); len(files) != 0 {
		t.Fatalf("probe stored %v", files)
	}
}

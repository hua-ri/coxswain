package k8s_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hua-ri/coxswain/internal/k8s"
	"github.com/hua-ri/coxswain/internal/k8s/k8stest"
	utilexec "k8s.io/client-go/util/exec"
)

// apiKubeconfig points context "fake" at api.
func apiKubeconfig(t *testing.T, api *k8stest.APIServer) *k8s.Clusters {
	t.Helper()
	dir := t.TempDir()
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
	return &k8s.Clusters{Kubeconfig: kubeconfig(t, filepath.Join(dir, "config"), content), Meta: k8s.NewMetaStore(filepath.Join(dir, "m.json"))}
}

// TestRemoteExecutorRequest: exec goes to the pod's exec subresource with
// the command, container and streams; a failed upgrade is an error.
func TestRemoteExecutorRequest(t *testing.T) {
	api := k8stest.NewAPIServer(t)
	c, err := apiKubeconfig(t, api).Client("fake")
	if err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	err = c.Exec.Exec(context.Background(), k8s.ExecRequest{Namespace: "web", Pod: "p-1", Container: "app",
		Command: []string{"ls", "-l"}, Stdout: &stdout})
	if err == nil {
		t.Fatal("exec against a server without exec succeeded")
	}
	var found bool
	for _, r := range api.Requests() {
		if r.Path != "/api/v1/namespaces/web/pods/p-1/exec" {
			continue
		}
		q, _ := url.ParseQuery(r.Query)
		if strings.Join(q["command"], " ") != "ls -l" || q.Get("container") != "app" || q.Get("stdout") != "true" ||
			q.Get("stdin") == "true" || q.Get("tty") == "true" || r.Auth != "Bearer tok" {
			t.Fatalf("exec request %+v", r)
		}
		found = true
	}
	if !found {
		t.Fatalf("no exec request: %+v", api.Requests())
	}
}

func TestExitCode(t *testing.T) {
	if code, ok := k8s.ExitCode(fmt.Errorf("wrapped: %w", utilexec.CodeExitError{Err: errors.New("x"), Code: 3})); !ok || code != 3 {
		t.Fatalf("%d %v", code, ok)
	}
	if _, ok := k8s.ExitCode(errors.New("connection refused")); ok {
		t.Fatal("not an exit")
	}
}

// Package k8stest keeps Kubernetes tests away from the user's real
// kubeconfig and clusters, and provides fakes: a minimal API server
// (httptest) and kubeconfig files in temp directories.
//
// Every test package that touches internal/k8s calls Main from its
// TestMain: HOME and KUBECONFIG then point into a temp dir for the whole
// test binary, and any attempt by internal/k8s to read a kubeconfig under
// the user's real ~/.kube (or their $KUBECONFIG) fails the run.
package k8stest

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hua-ri/coxswain/internal/k8s"
)

// The user's real home directories and $KUBECONFIG, captured before any
// test changes the environment (package init runs before TestMain).
var (
	realHomes      []string
	realKubeconfig []string
)

func init() {
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		realHomes = append(realHomes, u.HomeDir)
	}
	if h := os.Getenv("HOME"); h != "" {
		realHomes = append(realHomes, h)
	}
	for _, p := range filepath.SplitList(os.Getenv("KUBECONFIG")) {
		if p != "" {
			realKubeconfig = append(realKubeconfig, clean(p))
		}
	}
}

func clean(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	return filepath.Clean(p)
}

// Forbidden reports why reading path would touch the user's real
// kubeconfig, or "" if it is fine.
func Forbidden(path string) string { return ForbiddenIn(path, realHomes, realKubeconfig) }

// resolved is p with symlinks resolved, as far as it exists.
func resolved(p string) string {
	p = clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	parent := filepath.Dir(p)
	if parent == p {
		return p
	}
	return filepath.Join(resolved(parent), filepath.Base(p))
}

// ForbiddenIn reports why path is (or, following symlinks, leads to) a
// file under one of homes' .kube or one of kubeconfigs. Only path and the
// home directories themselves are resolved: the real kubeconfig files and
// .kube directories are never looked at.
func ForbiddenIn(path string, homes, kubeconfigs []string) string {
	paths := []string{clean(path), resolved(path)}
	for _, h := range homes {
		for _, home := range []string{clean(h), resolved(h)} {
			dir := filepath.Join(home, ".kube")
			for _, p := range paths {
				if p == dir || strings.HasPrefix(p, dir+string(filepath.Separator)) {
					return fmt.Sprintf("kubeconfig %s is under the real user's %s", path, dir)
				}
			}
		}
	}
	for _, k := range kubeconfigs {
		for _, p := range paths {
			if p == clean(k) {
				return fmt.Sprintf("kubeconfig %s is the real user's $KUBECONFIG %s", path, k)
			}
		}
	}
	return ""
}

// TB is the part of testing.TB that Guard uses.
type TB interface {
	Helper()
	Fatalf(format string, args ...any)
}

// Guard fails the test if any path is (or is under) the user's real
// ~/.kube or one of their $KUBECONFIG files.
func Guard(t TB, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if why := Forbidden(p); why != "" {
			t.Fatalf("red line: %s", why)
		}
	}
}

// InstallGuard makes internal/k8s refuse to read the user's real
// kubeconfig for the rest of the process, and returns the refused reads.
func InstallGuard() (violations func() []string) {
	var (
		mu  sync.Mutex
		got []string
	)
	k8s.BeforeRead = func(path string) error {
		if why := Forbidden(path); why != "" {
			mu.Lock()
			got = append(got, why)
			mu.Unlock()
			return fmt.Errorf("red line: %s", why)
		}
		return nil
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

// Main runs a test binary with HOME and KUBECONFIG in a temp dir, and
// with internal/k8s refusing to read the user's real kubeconfig. A
// refused read fails the run even if the test that caused it passed.
func Main(m *testing.M) {
	dir, err := os.MkdirTemp("", "cox-k8stest")
	if err != nil {
		panic(err)
	}
	home := filepath.Join(dir, "home")
	os.MkdirAll(filepath.Join(home, ".kube"), 0o700)
	os.Setenv("HOME", home)
	os.Setenv("KUBECONFIG", filepath.Join(home, ".kube", "config"))
	violations := InstallGuard()
	code := m.Run()
	os.RemoveAll(dir)
	if v := violations(); len(v) > 0 {
		fmt.Fprintf(os.Stderr, "red line: tests tried to read the real kubeconfig: %v\n", v)
		code = 1
	}
	os.Exit(code)
}

// Isolate gives one test its own HOME and KUBECONFIG and returns the
// kubeconfig path (the file does not exist yet).
func Isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".kube"), 0o700)
	path := filepath.Join(home, ".kube", "config")
	t.Setenv("HOME", home)
	t.Setenv("KUBECONFIG", path)
	Guard(t, path)
	return path
}

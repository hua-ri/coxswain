// Package k8s gives Coxswain access to the user's Kubernetes clusters.
//
// The user's kubeconfig is only ever read, never written: clients are
// built from a loaded config with no ConfigAccess, so client-go has no way
// to persist refreshed credentials (auth-provider tokens) back into it.
// Coxswain's own notes about contexts (tags, note) live in clusters.json.
package k8s

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// BeforeRead, if set, is called with every kubeconfig path before it is
// looked at; an error stops the load. Tests use it to make sure the
// user's real kubeconfig is never touched.
var BeforeRead func(path string) error

// DefaultPaths are the kubeconfig files kubectl would use: the entries of
// $KUBECONFIG (kubeconfigEnv), or ~/.kube/config.
func DefaultPaths(kubeconfigEnv, home string) []string {
	var out []string
	for _, p := range filepath.SplitList(kubeconfigEnv) {
		if p != "" && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		out = []string{filepath.Join(home, ".kube", "config")}
	}
	return out
}

// Kubeconfig is the merged kubeconfig, reloaded when one of its files
// changes. It is read on first use, not when created.
type Kubeconfig struct {
	paths []string
	// Interval is how often the files are checked for changes (stat of
	// size and mtime); default 5s.
	Interval time.Duration
	Now      func() time.Time

	mu      sync.Mutex
	loaded  bool
	cfg     *clientcmdapi.Config
	err     error
	stamp   string
	checked time.Time
	gen     int
}

func NewKubeconfig(paths []string) *Kubeconfig {
	return &Kubeconfig{paths: slices.Clone(paths), Interval: 5 * time.Second}
}

// Paths are the files the kubeconfig is merged from, in precedence order.
func (k *Kubeconfig) Paths() []string { return slices.Clone(k.paths) }

// Load returns the merged config and its generation, which changes every
// time the files are reloaded. The config must not be modified.
func (k *Kubeconfig) Load() (*clientcmdapi.Config, int, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := time.Now
	if k.Now != nil {
		now = k.Now
	}
	t := now()
	if k.loaded && t.Sub(k.checked) < k.Interval {
		return k.cfg, k.gen, k.err
	}
	k.checked = t
	stamp, err := k.stat()
	if err != nil {
		return nil, k.gen, err
	}
	if k.loaded && stamp == k.stamp {
		return k.cfg, k.gen, k.err
	}
	// Only read: no MigrationRules (those may copy files), no ExplicitPath.
	rules := &clientcmd.ClientConfigLoadingRules{Precedence: k.paths}
	k.cfg, k.err = rules.Load()
	if k.err != nil {
		k.cfg = nil
		k.err = fmt.Errorf("kubeconfig: %w", k.err)
	}
	k.stamp, k.loaded = stamp, true
	k.gen++
	return k.cfg, k.gen, k.err
}

// stat fingerprints the files (size and mtime, or missing).
func (k *Kubeconfig) stat() (string, error) {
	var b strings.Builder
	for _, p := range k.paths {
		if BeforeRead != nil {
			if err := BeforeRead(p); err != nil {
				return "", err
			}
		}
		st, err := os.Stat(p)
		switch {
		case errors.Is(err, os.ErrNotExist):
			b.WriteString("-;")
		case err != nil:
			b.WriteString(err.Error() + ";")
		default:
			b.WriteString(strconv.FormatInt(st.Size(), 10) + "," + strconv.FormatInt(st.ModTime().UnixNano(), 10) + ";")
		}
	}
	return b.String(), nil
}

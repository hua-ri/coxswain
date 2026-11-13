package k8s

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/hua-ri/coxswain/internal/filelock"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// Store holds the clusters added in Coxswain: one kubeconfig file per
// context in Dir (~/.coxswain/kubeconfigs, mode 0700; files 0600). The
// user's own kubeconfig is never written; these files are merged after
// it, read-only like it.
type Store struct{ Dir string }

func NewStore(dir string) *Store { return &Store{Dir: dir} }

// Files are the stored kubeconfigs, sorted.
func (s *Store) Files() []string {
	files, _ := filepath.Glob(filepath.Join(s.Dir, "*.yaml"))
	sort.Strings(files)
	return files
}

// Owns reports whether path is one of the store's files.
func (s *Store) Owns(path string) bool {
	if path == "" || s.Dir == "" {
		return false
	}
	return filepath.Dir(filepath.Clean(path)) == filepath.Clean(s.Dir) && strings.HasSuffix(path, ".yaml")
}

var unsafeFile = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// file is where context name is stored: a readable slug plus a hash, so
// that any context name (ARNs have ':' and '/') maps to a safe file.
func (s *Store) file(name string) string {
	slug := strings.Trim(unsafeFile.ReplaceAllString(name, "-"), "-.")
	if len(slug) > 40 {
		slug = slug[:40]
	}
	sum := sha256.Sum256([]byte(name))
	return filepath.Join(s.Dir, slug+"-"+hex.EncodeToString(sum[:4])+".yaml")
}

// ClusterSource is a kubeconfig the user gives: pasted YAML, or a file.
type ClusterSource struct {
	YAML []byte
	Path string
}

// load parses the source. Paths in a file are made absolute (relative to
// it); pasted YAML may not have relative paths, there is nothing they
// could be relative to.
func (src ClusterSource) load() (*clientcmdapi.Config, error) {
	if (len(src.YAML) == 0) == (src.Path == "") {
		return nil, errors.New("give either the kubeconfig YAML or a path to a kubeconfig file")
	}
	if src.Path != "" {
		if BeforeRead != nil {
			if err := BeforeRead(src.Path); err != nil {
				return nil, err
			}
		}
		cfg, err := clientcmd.LoadFromFile(src.Path)
		if err != nil {
			return nil, fmt.Errorf("kubeconfig %s: %w", src.Path, err)
		}
		if err := clientcmd.ResolveLocalPaths(cfg); err != nil {
			return nil, err
		}
		return cfg, nil
	}
	cfg, err := clientcmd.Load(src.YAML)
	if err != nil {
		return nil, fmt.Errorf("not a kubeconfig: %w", err)
	}
	return cfg, nil
}

// Candidate is a context of a kubeconfig being added.
type Candidate struct {
	Name      string `json:"name"`
	Cluster   string `json:"cluster"`
	Server    string `json:"server,omitempty"`
	User      string `json:"user,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Auth      string `json:"auth"` // token, cert, exec, oidc, basic, none
	Current   bool   `json:"current,omitempty"`
	// Problem says why it cannot be added as it is.
	Problem string `json:"problem,omitempty"`
	// Exists is where a context of the same name already is: "kubeconfig"
	// or "coxswain" (then it needs another name).
	Exists string `json:"exists,omitempty"`
}

// Inspect lists the contexts of a kubeconfig being added.
func (c *Clusters) Inspect(src ClusterSource) ([]Candidate, error) {
	cfg, err := src.load()
	if err != nil {
		return nil, err
	}
	existing, _, err := c.Kubeconfig.Load()
	if err != nil {
		return nil, err
	}
	var out []Candidate
	for name, ctx := range cfg.Contexts {
		cand := Candidate{Name: name, Cluster: ctx.Cluster, User: ctx.AuthInfo, Namespace: ctx.Namespace, Current: name == cfg.CurrentContext}
		if cl := cfg.Clusters[ctx.Cluster]; cl != nil {
			cand.Server = cl.Server
		}
		cand.Auth = authKind(cfg.AuthInfos[ctx.AuthInfo])
		if err := check(cfg, name, len(src.YAML) > 0); err != nil {
			cand.Problem = err.Error()
		}
		if _, ok := existing.Contexts[name]; ok {
			cand.Exists = c.source(existing, name)
		}
		out = append(out, cand)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func authKind(a *clientcmdapi.AuthInfo) string {
	switch {
	case a == nil:
		return "none"
	case a.Exec != nil:
		return "exec"
	case a.AuthProvider != nil:
		return a.AuthProvider.Name
	case a.Token != "" || a.TokenFile != "":
		return "token"
	case len(a.ClientCertificateData) > 0 || a.ClientCertificate != "":
		return "cert"
	case a.Username != "":
		return "basic"
	}
	return "none"
}

// check reports why a context cannot be used as Coxswain loads contexts.
func check(cfg *clientcmdapi.Config, name string, pasted bool) error {
	ctx := cfg.Contexts[name]
	if ctx == nil {
		return noContextIn(cfg, name)
	}
	cl := cfg.Clusters[ctx.Cluster]
	if cl == nil || cl.Server == "" {
		return fmt.Errorf("context %q: its cluster %q has no server", name, ctx.Cluster)
	}
	a := cfg.AuthInfos[ctx.AuthInfo]
	if ctx.AuthInfo != "" && a == nil {
		return fmt.Errorf("context %q: its user %q is missing", name, ctx.AuthInfo)
	}
	if a != nil && a.AuthProvider != nil && a.AuthProvider.Name != "oidc" {
		return fmt.Errorf("context %q: auth provider %q is not supported (use an exec credential plugin, a token, a client certificate or an OIDC id-token)",
			name, a.AuthProvider.Name)
	}
	if pasted {
		files := []string{cl.CertificateAuthority}
		if a != nil {
			files = append(files, a.ClientCertificate, a.ClientKey, a.TokenFile)
		}
		for _, f := range files {
			if f != "" && !filepath.IsAbs(f) && !strings.HasPrefix(f, "~") {
				return fmt.Errorf("context %q refers to the relative path %q; pick the kubeconfig file instead of pasting it, or make the path absolute", name, f)
			}
		}
	}
	return nil
}

func noContextIn(cfg *clientcmdapi.Config, name string) error {
	names := slices.Sorted(func(yield func(string) bool) {
		for n := range cfg.Contexts {
			if !yield(n) {
				return
			}
		}
	})
	if len(names) == 0 {
		return errors.New("the kubeconfig has no contexts")
	}
	if name == "" {
		return fmt.Errorf("the kubeconfig has %d contexts (%s); choose one", len(names), strings.Join(names, ", "))
	}
	return fmt.Errorf("the kubeconfig has no context %q (it has %s)", name, strings.Join(names, ", "))
}

// choose picks the context to use: the given one, or the only one.
func choose(cfg *clientcmdapi.Config, context string) (string, error) {
	if context != "" {
		if _, ok := cfg.Contexts[context]; !ok {
			return "", noContextIn(cfg, context)
		}
		return context, nil
	}
	if len(cfg.Contexts) == 1 {
		for n := range cfg.Contexts {
			return n, nil
		}
	}
	return "", noContextIn(cfg, "")
}

// ValidContextName reports whether a context name can be stored: 1-253
// characters, no spaces or control characters.
func ValidContextName(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	for _, r := range name {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// only is cfg reduced to one context, named name, with its cluster and
// user renamed after it (coxswain/<name>) so they cannot clash with the
// user's kubeconfig in the merge. It has no current context.
func only(cfg *clientcmdapi.Config, context, name string) *clientcmdapi.Config {
	ctx := *cfg.Contexts[context]
	out := clientcmdapi.NewConfig()
	ref := "coxswain/" + name
	cl := *cfg.Clusters[ctx.Cluster]
	cl.LocationOfOrigin, cl.Extensions = "", nil
	out.Clusters[ref] = &cl
	if a := cfg.AuthInfos[ctx.AuthInfo]; a != nil {
		au := *a
		au.LocationOfOrigin, au.Extensions = "", nil
		out.AuthInfos[ref] = &au
		ctx.AuthInfo = ref
	}
	ctx.Cluster, ctx.LocationOfOrigin, ctx.Extensions = ref, "", nil
	out.Contexts[name] = &ctx
	return out
}

// AddCluster stores one context of src in the Store, as name (default:
// its own name). A name already used by the user's kubeconfig (or by a
// cluster added before) is refused. Nothing is run or contacted.
func (c *Clusters) AddCluster(src ClusterSource, context, name string) (ContextInfo, error) {
	store := c.Kubeconfig.Store
	if store == nil {
		return ContextInfo{}, errors.New("this Coxswain has no store for added clusters")
	}
	cfg, err := src.load()
	if err != nil {
		return ContextInfo{}, err
	}
	if context, err = choose(cfg, context); err != nil {
		return ContextInfo{}, err
	}
	if err := check(cfg, context, len(src.YAML) > 0); err != nil {
		return ContextInfo{}, err
	}
	if name == "" {
		name = context
	}
	if !ValidContextName(name) {
		return ContextInfo{}, fmt.Errorf("context name %q: use 1-253 characters without spaces", name)
	}
	existing, _, err := c.Kubeconfig.Load()
	if err != nil {
		return ContextInfo{}, fmt.Errorf("cannot check for clashes with your kubeconfig: %w", err)
	}
	if _, ok := existing.Contexts[name]; ok {
		if c.source(existing, name) == SourceCoxswain {
			return ContextInfo{}, fmt.Errorf("a cluster named %q was already added in Coxswain; remove it first, or rename this one", name)
		}
		return ContextInfo{}, fmt.Errorf("a context named %q is already in your kubeconfig, which Coxswain never changes; rename this one (it is renamed only in Coxswain's copy)", name)
	}
	ref := "coxswain/" + name
	if _, ok := existing.Clusters[ref]; ok {
		return ContextInfo{}, fmt.Errorf("your kubeconfig already has a cluster named %q; choose another name", ref)
	}
	if _, ok := existing.AuthInfos[ref]; ok {
		return ContextInfo{}, fmt.Errorf("your kubeconfig already has a user named %q; choose another name", ref)
	}
	b, err := clientcmd.Write(*only(cfg, context, name))
	if err != nil {
		return ContextInfo{}, err
	}
	if err := os.MkdirAll(store.Dir, 0o700); err != nil {
		return ContextInfo{}, err
	}
	if err := os.Chmod(store.Dir, 0o700); err != nil {
		return ContextInfo{}, err
	}
	path := store.file(name)
	err = filelock.With(filepath.Join(store.Dir, ".lock"), func() error {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("a cluster named %q was already added in Coxswain", name)
		}
		return filelock.WriteAtomic(path, b)
	})
	if err != nil {
		return ContextInfo{}, err
	}
	c.Kubeconfig.Invalidate()
	return c.Context(name)
}

// RemoveCluster deletes a cluster added in Coxswain (and its tags and
// note). Contexts of the user's kubeconfig are read-only.
func (c *Clusters) RemoveCluster(name string) error {
	cfg, _, err := c.Kubeconfig.Load()
	if err != nil {
		return err
	}
	ctx, ok := cfg.Contexts[name]
	if !ok {
		return noContext(name)
	}
	if c.source(cfg, name) != SourceCoxswain {
		return fmt.Errorf("context %q comes from your kubeconfig (%s), which is read-only for Coxswain; only clusters added in Coxswain can be removed here",
			name, ctx.LocationOfOrigin)
	}
	store := c.Kubeconfig.Store
	err = filelock.With(filepath.Join(store.Dir, ".lock"), func() error {
		return os.Remove(ctx.LocationOfOrigin)
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	c.Kubeconfig.Invalidate()
	return c.Meta.Update(func(m map[string]Meta) error {
		delete(m, name)
		return nil
	})
}

// Probe builds a client for one context of src in memory (running its
// credential plugin, bounded by CredentialTimeout) and asks the server
// for its version. Nothing is stored.
func (c *Clusters) Probe(ctx context.Context, src ClusterSource, context string) (*version.Info, error) {
	cfg, err := src.load()
	if err != nil {
		return nil, err
	}
	if context, err = choose(cfg, context); err != nil {
		return nil, err
	}
	if err := check(cfg, context, len(src.YAML) > 0); err != nil {
		return nil, err
	}
	rc, _, err := c.restConfig(cfg, context)
	if err != nil {
		return nil, Explain(err)
	}
	newClient := c.NewClient
	if newClient == nil {
		newClient = Connect
	}
	cl, err := newClient(context, rc)
	if err != nil {
		return nil, err
	}
	type result struct {
		v   *version.Info
		err error
	}
	done := make(chan result, 1)
	go func() {
		v, err := cl.Typed.Discovery().ServerVersion()
		done <- result{v, err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-done:
		return r.v, Explain(r.err)
	}
}

package k8s

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// ErrNoContext is returned for a context the kubeconfig does not have.
var ErrNoContext = errors.New("no such kubeconfig context")

// Client settings (per context).
const (
	QPS            = 20
	Burst          = 40
	RequestTimeout = 30 * time.Second
)

// Client is one context's connection to its cluster.
type Client struct {
	Context   string
	Namespace string // the context's default namespace ("default" if unset)
	// REST is the context's configuration without a request timeout, for
	// long-lived streams (exec, port-forward). Nil with fake clients.
	REST      *rest.Config
	Typed     kubernetes.Interface
	Dynamic   dynamic.Interface
	Resources *Resolver
	Exec      Executor
	Apply     Applier
	Forward   Forwarder

	expires time.Time // when its credentials expire (zero: unknown)
}

// Connect builds a Client for a context's rest.Config: API requests get
// QPS 20 / burst 40 and a 30s timeout; discovery is cached for 10 minutes.
func Connect(name string, rc *rest.Config) (*Client, error) {
	api := rest.CopyConfig(rc)
	api.QPS, api.Burst, api.Timeout = QPS, Burst, RequestTimeout
	typed, err := kubernetes.NewForConfig(api)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(api)
	if err != nil {
		return nil, err
	}
	stream := rest.CopyConfig(rc)
	stream.QPS, stream.Burst = QPS, Burst
	return &Client{
		Context:   name,
		REST:      stream,
		Typed:     typed,
		Dynamic:   dyn,
		Resources: NewResolver(typed.Discovery()),
		Exec:      &RemoteExecutor{Config: stream},
		Apply:     &DynamicApplier{Dynamic: dyn},
		Forward:   &SPDYForwarder{Config: stream},
	}, nil
}

// ContextInfo describes one kubeconfig context for people and agents.
type ContextInfo struct {
	Name      string            `json:"name"`
	Cluster   string            `json:"cluster"`
	Server    string            `json:"server,omitempty"`
	User      string            `json:"user,omitempty"`
	Namespace string            `json:"namespace"`
	Current   bool              `json:"current,omitempty"`
	Tags      map[string]string `json:"tags,omitempty"`
	Note      string            `json:"note,omitempty"`
}

// Clusters are the kubeconfig's contexts and a cached client for each.
type Clusters struct {
	Kubeconfig *Kubeconfig
	Meta       *MetaStore
	// NewClient builds a context's client; default Connect. Tests replace
	// it with fakes.
	NewClient func(name string, rc *rest.Config) (*Client, error)
	// CredentialTimeout bounds one run of a credential plugin; default 30s.
	CredentialTimeout time.Duration

	mu      sync.Mutex
	gen     int
	clients map[string]*Client
}

// Contexts lists the kubeconfig's contexts, sorted by name, with their
// tags and notes, and the current context's name.
func (c *Clusters) Contexts() (string, []ContextInfo, error) {
	cfg, _, err := c.Kubeconfig.Load()
	if err != nil {
		return "", nil, err
	}
	meta, err := c.Meta.All()
	if err != nil {
		return "", nil, err
	}
	out := make([]ContextInfo, 0, len(cfg.Contexts))
	for name := range cfg.Contexts {
		out = append(out, info(cfg, name, meta[name]))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return cfg.CurrentContext, out, nil
}

// Context returns one context with its tags and note.
func (c *Clusters) Context(name string) (ContextInfo, error) {
	cfg, _, err := c.Kubeconfig.Load()
	if err != nil {
		return ContextInfo{}, err
	}
	if _, ok := cfg.Contexts[name]; !ok {
		return ContextInfo{}, noContext(name)
	}
	meta, err := c.Meta.Get(name)
	if err != nil {
		return ContextInfo{}, err
	}
	return info(cfg, name, meta), nil
}

func noContext(name string) error {
	return fmt.Errorf("%w: %q (list contexts with `cox k8s contexts`)", ErrNoContext, name)
}

func info(cfg *clientcmdapi.Config, name string, meta Meta) ContextInfo {
	ctx := cfg.Contexts[name]
	ci := ContextInfo{Name: name, Cluster: ctx.Cluster, User: ctx.AuthInfo, Namespace: ctx.Namespace,
		Current: name == cfg.CurrentContext, Tags: meta.Tags, Note: meta.Note}
	if ci.Namespace == "" {
		ci.Namespace = "default"
	}
	if cl := cfg.Clusters[ctx.Cluster]; cl != nil {
		ci.Server = cl.Server
	}
	return ci
}

// Target is the audit target for a namespace of a context (k8s:ctx/ns),
// or for the whole cluster (k8s:ctx) if namespace is empty, carrying the
// context's tags.
func (c *Clusters) Target(context, namespace string) action.Target {
	id := "k8s:" + context
	if namespace != "" {
		id += "/" + namespace
	}
	meta, _ := c.Meta.Get(context)
	return action.Target{Kind: "k8s", ID: id, Tags: meta.Tags}
}

// Client returns the context's client, building it on first use. Clients
// are rebuilt after the kubeconfig changes and when their credentials
// expire. Building one may run the context's credential plugin, bounded
// by CredentialTimeout.
func (c *Clusters) Client(name string) (*Client, error) {
	cfg, gen, err := c.Kubeconfig.Load()
	if err != nil {
		return nil, err
	}
	if _, ok := cfg.Contexts[name]; !ok {
		return nil, noContext(name)
	}
	c.mu.Lock()
	if gen != c.gen || c.clients == nil {
		c.gen, c.clients = gen, map[string]*Client{}
	}
	if cl, ok := c.clients[name]; ok && (cl.expires.IsZero() || time.Now().Before(cl.expires)) {
		c.mu.Unlock()
		return cl, nil
	}
	c.mu.Unlock()

	rc, expires, err := c.restConfig(cfg, name)
	if err != nil {
		return nil, err
	}
	newClient := c.NewClient
	if newClient == nil {
		newClient = Connect
	}
	cl, err := newClient(name, rc)
	if err != nil {
		return nil, fmt.Errorf("context %q: %w", name, err)
	}
	cl.Context, cl.expires = name, expires
	if cl.Namespace == "" {
		cl.Namespace = info(cfg, name, Meta{}).Namespace
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if gen == c.gen {
		c.clients[name] = cl
	}
	return cl, nil
}

// Explain adds the login hint to errors that mean the credentials need
// the user.
func Explain(err error) error {
	if err == nil {
		return nil
	}
	var ce *CredentialError
	if errors.As(err, &ce) {
		return err // says so already
	}
	msg := err.Error()
	if strings.Contains(msg, "getting credentials") || strings.Contains(msg, "standard input is") ||
		strings.Contains(msg, "Unauthorized") {
		return fmt.Errorf("%w (%s)", err, loginHint)
	}
	return err
}

// restConfig is a context's client configuration, and when its
// credentials expire (zero: unknown). It is built without ConfigAccess,
// so nothing is ever written back to the kubeconfig. Credentials that
// would need client-go to run or refresh something are handled here
// instead: exec plugins run under a timeout without a terminal (see
// credPlugin), and OIDC id-tokens are used as they are, never refreshed.
func (c *Clusters) restConfig(cfg *clientcmdapi.Config, name string) (*rest.Config, time.Time, error) {
	rc, err := clientcmd.NewNonInteractiveClientConfig(*cfg, name, &clientcmd.ConfigOverrides{}, nil).ClientConfig()
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("context %q: %w", name, err)
	}
	rc.AuthConfigPersister = nil
	var expires time.Time
	if ap := rc.AuthProvider; ap != nil {
		rc.AuthProvider = nil
		if ap.Name != "oidc" {
			return nil, time.Time{}, fmt.Errorf("context %q: auth provider %q is not supported (use an exec credential plugin)", name, ap.Name)
		}
		tok, exp, err := oidcToken(ap.Config, time.Now())
		if err != nil {
			return nil, time.Time{}, fmt.Errorf("context %q: %w", name, err)
		}
		rc.BearerToken, rc.BearerTokenFile, expires = tok, "", exp
	}
	if ep := rc.ExecProvider; ep != nil {
		rc.ExecProvider = nil
		p := newCredPlugin(ep, rc, c.CredentialTimeout)
		cred, err := p.get(context.Background())
		if err != nil {
			return nil, time.Time{}, fmt.Errorf("context %q: %w", name, err)
		}
		if cred.cert != "" {
			// A client certificate: used until it expires, then the
			// client is built again (running the plugin again).
			rc.TLSClientConfig.CertData, rc.TLSClientConfig.KeyData = []byte(cred.cert), []byte(cred.key)
			rc.TLSClientConfig.CertFile, rc.TLSClientConfig.KeyFile = "", ""
			expires = cred.expiry
		}
		if cred.token != "" {
			rc.Wrap(func(rt http.RoundTripper) http.RoundTripper { return &tokenTransport{rt: rt, p: p} })
		}
	}
	return rc, expires, nil
}

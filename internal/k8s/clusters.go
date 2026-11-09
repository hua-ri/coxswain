package k8s

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth/oidc" // OIDC auth-provider kubeconfigs (tokens are refreshed in memory only)
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
// are rebuilt after the kubeconfig changes.
func (c *Clusters) Client(name string) (*Client, error) {
	cfg, gen, err := c.Kubeconfig.Load()
	if err != nil {
		return nil, err
	}
	if _, ok := cfg.Contexts[name]; !ok {
		return nil, noContext(name)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if gen != c.gen || c.clients == nil {
		c.gen, c.clients = gen, map[string]*Client{}
	}
	if cl, ok := c.clients[name]; ok {
		return cl, nil
	}
	rc, err := RESTConfig(cfg, name)
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
	cl.Context = name
	if cl.Namespace == "" {
		cl.Namespace = info(cfg, name, Meta{}).Namespace
	}
	c.clients[name] = cl
	return cl, nil
}

// loginHint is shown when a credential plugin needs an interactive login.
const loginHint = "cox server cannot answer a login prompt: log in in a terminal first " +
	"(e.g. run any kubectl command for this context there), then try again"

// Explain adds a hint to errors from credential plugins that could not
// get credentials, e.g. because they need an interactive login.
func Explain(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if strings.Contains(msg, "getting credentials") || strings.Contains(msg, "standard input is") {
		return fmt.Errorf("%w (%s)", err, loginHint)
	}
	return err
}

// RESTConfig is a context's client configuration. It is built without
// ConfigAccess, so refreshed credentials are never written back to the
// kubeconfig, and credential plugins never get a terminal to prompt on.
func RESTConfig(cfg *clientcmdapi.Config, name string) (*rest.Config, error) {
	rc, err := clientcmd.NewNonInteractiveClientConfig(*cfg, name, &clientcmd.ConfigOverrides{}, nil).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("context %q: %w", name, err)
	}
	rc.AuthConfigPersister = nil
	if rc.ExecProvider != nil {
		exec := *rc.ExecProvider // shared with the loaded config
		rc.ExecProvider = &exec
		rc.ExecProvider.StdinUnavailable = true
		rc.ExecProvider.StdinUnavailableMessage = loginHint
	}
	return rc, nil
}

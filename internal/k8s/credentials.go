package k8s

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"k8s.io/client-go/rest"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// DefaultCredentialTimeout bounds one run of a credential plugin.
const DefaultCredentialTimeout = 30 * time.Second

// loginHint is shown when credentials need the user.
const loginHint = "log in once in a terminal (e.g. run kubectl with this context), then retry"

// Limits of what a credential plugin may print.
const (
	maxPluginStdout = 1 << 20
	maxPluginStderr = 8 << 10
	// failureMemory is how long a failed plugin run is remembered, so a
	// burst of calls does not run (and wait for) it again and again.
	failureMemory = 10 * time.Second
)

// CredentialError is a credential plugin or provider that could not give
// credentials; its message says how to fix it.
type CredentialError struct{ msg string }

func (e *CredentialError) Error() string { return e.msg }

func credentialError(format string, args ...any) error {
	return &CredentialError{msg: fmt.Sprintf(format, args...) + "; " + loginHint}
}

// execCluster is the cluster a plugin is told about (provideClusterInfo).
type execCluster struct {
	Server                   string `json:"server"`
	TLSServerName            string `json:"tls-server-name,omitempty"`
	InsecureSkipTLSVerify    bool   `json:"insecure-skip-tls-verify,omitempty"`
	CertificateAuthorityData []byte `json:"certificate-authority-data,omitempty"`
	ProxyURL                 string `json:"proxy-url,omitempty"`
}

type execCredential struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Spec       struct {
		Interactive bool         `json:"interactive"`
		Cluster     *execCluster `json:"cluster,omitempty"`
	} `json:"spec"`
	Status *struct {
		Token                 string     `json:"token,omitempty"`
		ExpirationTimestamp   *time.Time `json:"expirationTimestamp,omitempty"`
		ClientCertificateData string     `json:"clientCertificateData,omitempty"`
		ClientKeyData         string     `json:"clientKeyData,omitempty"`
	} `json:"status,omitempty"`
}

// credentials are what a plugin returned.
type credentials struct {
	token, cert, key string
	expiry           time.Time // zero: none
}

// credPlugin runs a kubeconfig's exec credential plugin itself, instead
// of client-go, so that every run is bounded by a timeout (and by the
// caller's context), never gets a terminal, and its stderr ends up in the
// error the user sees.
type credPlugin struct {
	cfg     clientcmdapi.ExecConfig
	cluster *execCluster
	timeout time.Duration
	now     func() time.Time

	mu      sync.Mutex
	cached  *credentials
	running chan struct{} // closed when the current run ends
	lastErr error
	errAt   time.Time
}

func newCredPlugin(cfg *clientcmdapi.ExecConfig, rc *rest.Config, timeout time.Duration) *credPlugin {
	c := *cfg
	c.InteractiveMode = clientcmdapi.NeverExecInteractiveMode
	p := &credPlugin{cfg: c, timeout: timeout, now: time.Now}
	if p.timeout <= 0 {
		p.timeout = DefaultCredentialTimeout
	}
	if c.ProvideClusterInfo {
		ca := rc.TLSClientConfig.CAData
		if len(ca) == 0 && rc.TLSClientConfig.CAFile != "" {
			ca, _ = os.ReadFile(rc.TLSClientConfig.CAFile)
		}
		p.cluster = &execCluster{Server: rc.Host, TLSServerName: rc.TLSClientConfig.ServerName,
			InsecureSkipTLSVerify: rc.TLSClientConfig.Insecure, CertificateAuthorityData: ca}
	}
	return p
}

// valid reports whether c may still be used.
func (p *credPlugin) valid(c *credentials) bool {
	return c != nil && (c.expiry.IsZero() || p.now().Before(c.expiry.Add(-5*time.Second)))
}

// get returns cached credentials, or runs the plugin (once for all
// concurrent callers). Waiting callers give up when their ctx ends.
func (p *credPlugin) get(ctx context.Context) (credentials, error) {
	for {
		p.mu.Lock()
		if p.valid(p.cached) {
			c := *p.cached
			p.mu.Unlock()
			return c, nil
		}
		if p.lastErr != nil && p.now().Sub(p.errAt) < failureMemory {
			err := p.lastErr
			p.mu.Unlock()
			return credentials{}, err
		}
		if ch := p.running; ch != nil {
			p.mu.Unlock()
			select {
			case <-ch:
				continue
			case <-ctx.Done():
				return credentials{}, ctx.Err()
			}
		}
		ch := make(chan struct{})
		p.running = ch
		p.mu.Unlock()

		c, err := p.run(ctx)
		p.mu.Lock()
		p.running = nil
		switch {
		case err == nil:
			p.cached, p.lastErr = &c, nil
		case ctx.Err() == nil: // not just this caller giving up
			p.lastErr, p.errAt = err, p.now()
		}
		p.mu.Unlock()
		close(ch)
		return c, err
	}
}

// invalidate drops the cached credentials (the server rejected them).
func (p *credPlugin) invalidate() {
	p.mu.Lock()
	p.cached = nil
	p.mu.Unlock()
}

// capped keeps the first n bytes written to it.
type capped struct {
	b bytes.Buffer
	n int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.n - c.b.Len(); room > 0 {
		c.b.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

// errPluginTimeout ends a plugin run that took too long.
var errPluginTimeout = errors.New("credential plugin timed out")

// afterTimeout starts a plugin run's timeout; tests replace it to fire it
// by hand.
var afterTimeout = func(d time.Duration, f func()) (stop func() bool) { return time.AfterFunc(d, f).Stop }

func (p *credPlugin) run(parent context.Context) (credentials, error) {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	stop := afterTimeout(p.timeout, func() { cancel(errPluginTimeout) })
	defer stop()
	info := execCredential{APIVersion: p.cfg.APIVersion, Kind: "ExecCredential"}
	info.Spec.Cluster = p.cluster
	infoJSON, err := json.Marshal(info)
	if err != nil {
		return credentials{}, err
	}
	cmd := exec.CommandContext(ctx, p.cfg.Command, p.cfg.Args...)
	cmd.Env = os.Environ()
	for _, e := range p.cfg.Env {
		cmd.Env = append(cmd.Env, e.Name+"="+e.Value)
	}
	cmd.Env = append(cmd.Env, "KUBERNETES_EXEC_INFO="+string(infoJSON))
	stdout, stderr := &capped{n: maxPluginStdout}, &capped{n: maxPluginStderr}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, stdout, stderr // no stdin: it cannot prompt
	// Its own process group, so that whatever it started dies with it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	err = cmd.Run()
	said := ""
	if s := strings.TrimSpace(stderr.b.String()); s != "" {
		said = fmt.Sprintf("; it said: %q", s)
	}
	switch {
	case parent.Err() != nil:
		return credentials{}, parent.Err()
	case errors.Is(context.Cause(ctx), errPluginTimeout):
		return credentials{}, credentialError("credential plugin %s did not finish within %s (it may be waiting for a login)%s",
			p.cfg.Command, p.timeout, said)
	case errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist):
		return credentials{}, credentialError("credential plugin %s is not installed (%v)%s", p.cfg.Command, err, said)
	case err != nil:
		return credentials{}, credentialError("credential plugin %s failed (%v)%s", p.cfg.Command, err, said)
	}
	var cred execCredential
	if err := json.Unmarshal(stdout.b.Bytes(), &cred); err != nil {
		return credentials{}, credentialError("credential plugin %s printed no ExecCredential (%v)%s", p.cfg.Command, err, said)
	}
	switch {
	case p.cfg.APIVersion != "" && cred.APIVersion != p.cfg.APIVersion:
		return credentials{}, fmt.Errorf("credential plugin %s returned %s, not %s", p.cfg.Command, cred.APIVersion, p.cfg.APIVersion)
	case cred.Status == nil || (cred.Status.Token == "" && cred.Status.ClientCertificateData == ""):
		return credentials{}, credentialError("credential plugin %s returned no token or certificate%s", p.cfg.Command, said)
	case (cred.Status.ClientCertificateData == "") != (cred.Status.ClientKeyData == ""):
		return credentials{}, fmt.Errorf("credential plugin %s returned only a certificate or a key, not both", p.cfg.Command)
	}
	c := credentials{token: cred.Status.Token, cert: cred.Status.ClientCertificateData, key: cred.Status.ClientKeyData}
	if cred.Status.ExpirationTimestamp != nil {
		c.expiry = *cred.Status.ExpirationTimestamp
	}
	if c.cert != "" {
		pair, err := tls.X509KeyPair([]byte(c.cert), []byte(c.key))
		if err != nil {
			return credentials{}, fmt.Errorf("credential plugin %s: bad client certificate: %v", p.cfg.Command, err)
		}
		if leaf, err := x509.ParseCertificate(pair.Certificate[0]); err == nil && (c.expiry.IsZero() || leaf.NotAfter.Before(c.expiry)) {
			c.expiry = leaf.NotAfter
		}
	}
	return c, nil
}

// tokenTransport adds the plugin's bearer token to requests, fetching it
// (bounded by the request's context and the timeout) when needed.
type tokenTransport struct {
	rt http.RoundTripper
	p  *credPlugin
}

func (t *tokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Authorization") != "" {
		return t.rt.RoundTrip(req)
	}
	c, err := t.p.get(req.Context())
	if err != nil {
		return nil, err
	}
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := t.rt.RoundTrip(r)
	if err == nil && resp.StatusCode == http.StatusUnauthorized {
		t.p.invalidate()
	}
	return resp, err
}

func (t *tokenTransport) WrappedRoundTripper() http.RoundTripper { return t.rt }

// oidcToken returns an OIDC auth provider's id-token while it is valid.
// Coxswain never refreshes it: a refresh can rotate the user's refresh
// token (and with reuse detection, log them out everywhere).
func oidcToken(cfg map[string]string, now time.Time) (string, time.Time, error) {
	tok := cfg["id-token"]
	if tok == "" {
		return "", time.Time{}, credentialError("the OIDC login of this context has no id-token")
	}
	exp, err := jwtExpiry(tok)
	if err != nil {
		return "", time.Time{}, credentialError("the OIDC id-token of this context cannot be read (%v)", err)
	}
	if !exp.IsZero() && !now.Before(exp.Add(-10*time.Second)) {
		return "", time.Time{}, credentialError("the OIDC id-token of this context expired at %s (cox never refreshes it)",
			exp.Local().Format(time.RFC3339))
	}
	return tok, exp, nil
}

func jwtExpiry(tok string) (time.Time, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return time.Time{}, errors.New("not a JWT")
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}, err
	}
	var claims struct {
		Exp json.Number `json:"exp"`
	}
	if err := json.Unmarshal(b, &claims); err != nil {
		return time.Time{}, err
	}
	if claims.Exp == "" {
		return time.Time{}, nil
	}
	f, err := claims.Exp.Float64()
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(int64(f), 0), nil
}

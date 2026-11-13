package argo

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/hua-ri/coxswain/internal/secrets"
)

// Method is a way to log in to an instance.
type Method string

const (
	Password Method = "password" // Argo CD local account → session token
	Browser  Method = "browser"  // Argo CD SSO in the system browser, as argocd login --sso
	Token    Method = "token"    // a token the user has (Argo CD API token, Kubernetes or argo SSO token)
	None     Method = "none"     // Argo Workflows in server auth mode
)

// Credential is what is kept in the keychain for an instance. It never
// leaves the server: actions report a Status instead.
type Credential struct {
	Method       Method    `json:"method"`
	Token        string    `json:"token,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	SavedAt      time.Time `json:"saved_at"`
}

// Status describes an instance's credential without revealing it.
type Status struct {
	LoggedIn  bool       `json:"logged_in"`
	Method    Method     `json:"method,omitempty"`
	Identity  string     `json:"identity,omitempty"`   // from the token's claims, unverified
	ExpiresAt *time.Time `json:"expires_at,omitempty"` // nil: does not expire (or not a JWT)
	Renewable bool       `json:"renewable,omitempty"`  // has a refresh token
	Rejected  bool       `json:"rejected,omitempty"`   // the server refused it last time
	TokenHash string     `json:"token_sha256_prefix,omitempty"`
	Problem   string     `json:"problem,omitempty"`
}

// Sessions holds the instances and their credentials, and builds API
// clients for them.
type Sessions struct {
	Store   *Store
	Secrets secrets.Store
	// OpenBrowser opens a URL in the user's browser (open(1) by default;
	// tests replace it).
	OpenBrowser func(url string) error
	Now         func() time.Time

	mu       sync.Mutex
	rejected map[string]bool   // instance ID → the server answered 401
	logins   map[string]func() // instance ID → cancels a browser login in progress
}

func (s *Sessions) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Sessions) credential(i Instance) (Credential, error) {
	b, err := s.Secrets.Get(i.Account())
	if errors.Is(err, secrets.ErrNotFound) {
		return Credential{}, nil
	}
	if err != nil {
		return Credential{}, err
	}
	var c Credential
	if err := json.Unmarshal(b, &c); err != nil {
		return Credential{}, fmt.Errorf("%s: stored credential is unreadable; log in again", i.ID())
	}
	return c, nil
}

func (s *Sessions) save(i Instance, c Credential) error {
	c.SavedAt = s.now().UTC()
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if err := s.Secrets.Set(i.Account(), b); err != nil {
		return err
	}
	s.setRejected(i, false)
	return nil
}

// Logout forgets an instance's credential.
func (s *Sessions) Logout(i Instance) error {
	s.setRejected(i, false)
	return s.Secrets.Delete(i.Account())
}

func (s *Sessions) setRejected(i Instance, v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rejected == nil {
		s.rejected = map[string]bool{}
	}
	if v {
		s.rejected[i.ID()] = true
	} else {
		delete(s.rejected, i.ID())
	}
}

// Status reports an instance's credential.
func (s *Sessions) Status(i Instance) Status {
	c, err := s.credential(i)
	if err != nil {
		return Status{Problem: err.Error()}
	}
	s.mu.Lock()
	rejected := s.rejected[i.ID()]
	s.mu.Unlock()
	st := Status{Method: c.Method, Rejected: rejected, Renewable: c.RefreshToken != ""}
	switch {
	case c.Method == "":
		return Status{}
	case c.Method == None:
		st.LoggedIn = true
		return st
	}
	st.TokenHash = TokenHash(c.Token)
	cl := ParseClaims(c.Token)
	st.Identity = cl.Identity()
	st.ExpiresAt = cl.Expiry()
	st.LoggedIn = !rejected && (st.ExpiresAt == nil || st.ExpiresAt.After(s.now()) || st.Renewable)
	return st
}

// Claims are a JWT's claims, read without verifying it: only to tell the
// user who a token acts as and when it expires.
type Claims map[string]any

// ParseClaims returns the claims of a JWT, or nil if t is not one.
func ParseClaims(t string) Claims {
	parts := strings.Split(t, ".")
	if len(parts) != 3 {
		return nil
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil
	}
	var c Claims
	if json.Unmarshal(b, &c) != nil {
		return nil
	}
	return c
}

func (c Claims) str(k string) string {
	v, _ := c[k].(string)
	return v
}

// Identity is who the token acts as: Argo CD's own tokens carry the
// account in sub (admin, or admin:login), SSO tokens an email, and
// Kubernetes tokens system:serviceaccount:ns:name.
func (c Claims) Identity() string {
	if c == nil {
		return ""
	}
	if e := c.str("email"); e != "" {
		return e
	}
	if n := c.str("name"); n != "" && c.str("sub") == "" {
		return n
	}
	if sub := c.str("sub"); sub != "" {
		if user, _, ok := strings.Cut(sub, ":"); ok && user != "system" {
			return user // Argo CD: "admin:login", "admin:apiKey"
		}
		return sub
	}
	return ""
}

// Expiry is the token's exp, or nil if it has none.
func (c Claims) Expiry() *time.Time {
	exp, ok := c["exp"].(float64)
	if !ok {
		return nil
	}
	t := time.Unix(int64(exp), 0).UTC()
	return &t
}

// TokenHash is the first 8 hex digits of a token's sha256: enough to tell
// tokens apart in the audit log, useless for using one.
func TokenHash(t string) string {
	if t == "" {
		return ""
	}
	return shortHash(t)
}

// NormalizeToken strips a pasted "Bearer " prefix and whitespace.
func NormalizeToken(t string) string {
	t = strings.TrimSpace(t)
	if len(t) > 7 && strings.EqualFold(t[:7], "bearer ") {
		t = strings.TrimSpace(t[7:])
	}
	return t
}

// HTTPClient is a client for an instance, honouring its CA file and
// insecure_skip_verify.
func HTTPClient(i Instance) (*http.Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if i.CAFile != "" {
		pem, err := os.ReadFile(i.CAFile)
		if err != nil {
			return nil, fmt.Errorf("%s: ca_file: %w", i.ID(), err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s: ca_file %s has no PEM certificates", i.ID(), i.CAFile)
		}
		cfg.RootCAs = pool
	}
	cfg.InsecureSkipVerify = i.InsecureSkipVerify
	tr.TLSClientConfig = cfg
	return &http.Client{Transport: tr, Timeout: 60 * time.Second}, nil
}

// NeedsLogin is the error for a request the instance refused (401), or
// one made without a credential.
type NeedsLogin struct {
	Instance Instance
	Reason   string
}

func (e *NeedsLogin) Error() string {
	return fmt.Sprintf("%s instance %q needs login (%s); ask the user to log in in Coxswain, or see %s.login_methods",
		e.Instance.Kind, e.Instance.Name, e.Reason, e.Instance.Kind)
}

// Rename moves an instance's credential to its new name.
func (s *Sessions) Rename(old, n Instance) error {
	b, err := s.Secrets.Get(old.Account())
	if errors.Is(err, secrets.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := s.Secrets.Set(n.Account(), b); err != nil {
		return err
	}
	return s.Secrets.Delete(old.Account())
}

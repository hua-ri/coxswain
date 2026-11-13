package argo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// CDSettings is what an Argo CD server tells anyone about itself
// (GET /api/v1/settings, no credential needed).
type CDSettings struct {
	URL       string `json:"url"`
	DexConfig *struct {
		Connectors []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"connectors"`
	} `json:"dexConfig"`
	OIDCConfig *struct {
		Name          string          `json:"name"`
		Issuer        string          `json:"issuer"`
		ClientID      string          `json:"clientID"`
		CLIClientID   string          `json:"cliClientID"`
		Scopes        []string        `json:"scopes"`
		IDTokenClaims json.RawMessage `json:"idTokenClaims"`
	} `json:"oidcConfig"`
	UserLoginsDisabled bool `json:"userLoginsDisabled"`
}

// SSOName names the instance's SSO for people (the Dex connectors or the
// OIDC provider), or "" if it has none.
func (s CDSettings) SSOName() string {
	if s.OIDCConfig != nil && s.OIDCConfig.Issuer != "" {
		if s.OIDCConfig.Name != "" {
			return s.OIDCConfig.Name
		}
		return "OIDC"
	}
	if s.DexConfig != nil && len(s.DexConfig.Connectors) > 0 {
		names := []string{}
		for _, c := range s.DexConfig.Connectors {
			names = append(names, c.Name)
		}
		return strings.Join(names, ", ")
	}
	return ""
}

// MethodInfo is one way to log in, as offered to people and agents.
type MethodInfo struct {
	Method Method `json:"method"`
	Label  string `json:"label"`
	Detail string `json:"detail,omitempty"`
}

// Methods lists the login methods an instance offers.
type Methods struct {
	Instance string       `json:"instance"`
	URL      string       `json:"url"`
	Methods  []MethodInfo `json:"methods"`
	Status   Status       `json:"status"`
	Problem  string       `json:"problem,omitempty"` // could not ask the server
}

// cdRequest calls an Argo CD API with the instance's TLS settings; token
// may be empty.
func cdRequest(ctx context.Context, i Instance, method, path, token string, body, out any) error {
	hc, err := HTTPClient(i)
	if err != nil {
		return err
	}
	return doJSON(ctx, hc, i, method, i.URL+path, token, body, out)
}

// APIError is a non-2xx answer from an Argo server.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("HTTP %d", e.Status)
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message)
}

func doJSON(ctx context.Context, hc *http.Client, i Instance, method, u, token string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", i.ID(), err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return fmt.Errorf("%s: %w", i.ID(), err)
	}
	if resp.StatusCode/100 != 2 {
		return &APIError{Status: resp.StatusCode, Message: apiMessage(b)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("%s: unexpected answer from %s: %w", i.ID(), redactURL(u), err)
	}
	return nil
}

// apiMessage is the message of a grpc-gateway error body, or the start of
// a non-JSON one (an HTML error page from a proxy).
func apiMessage(b []byte) string {
	var e struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil {
		if e.Message != "" {
			return e.Message
		}
		if e.Error != "" {
			return e.Error
		}
	}
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

func redactURL(u string) string {
	if p, err := url.Parse(u); err == nil {
		p.RawQuery = ""
		return p.String()
	}
	return u
}

// CDSettingsOf asks an Argo CD instance for its settings.
func CDSettingsOf(ctx context.Context, i Instance) (CDSettings, error) {
	var s CDSettings
	err := cdRequest(ctx, i, http.MethodGet, "/api/v1/settings", "", nil, &s)
	if err != nil {
		return s, fmt.Errorf("%s: cannot read its settings (is %s an Argo CD server?): %w", i.ID(), i.URL, err)
	}
	return s, nil
}

// CDMethods lists how one can log in to an Argo CD instance.
func (s *Sessions) CDMethods(ctx context.Context, i Instance) Methods {
	m := Methods{Instance: i.Name, URL: i.URL, Status: s.Status(i)}
	set, err := CDSettingsOf(ctx, i)
	if err != nil {
		m.Problem = err.Error()
	}
	if sso := set.SSOName(); sso != "" {
		m.Methods = append(m.Methods, MethodInfo{Method: Browser, Label: "Browser (" + sso + ")",
			Detail: "opens the system browser on this Mac for the instance's single sign-on, as argocd login --sso; the user completes it there"})
	}
	if !set.UserLoginsDisabled {
		m.Methods = append(m.Methods, MethodInfo{Method: Password, Label: "Username and password",
			Detail: "an Argo CD local account (such as admin); the password is exchanged for a session token and not kept"})
	}
	m.Methods = append(m.Methods, MethodInfo{Method: Token, Label: "Token",
		Detail: "an Argo CD API token (account or project role token)"})
	return m
}

// LoginRequest is how to log in.
type LoginRequest struct {
	Method   Method
	Username string
	Password string
	Token    string
}

// CDLogin logs in to an Argo CD instance and keeps the credential. A
// browser login waits until the user finishes it, ctx ends, or 5 minutes.
func (s *Sessions) CDLogin(ctx context.Context, i Instance, r LoginRequest) (Status, error) {
	var c Credential
	switch r.Method {
	case Password:
		if r.Username == "" || r.Password == "" {
			return Status{}, errors.New("password login needs username and password")
		}
		var out struct {
			Token string `json:"token"`
		}
		err := cdRequest(ctx, i, http.MethodPost, "/api/v1/session", "",
			map[string]string{"username": r.Username, "password": r.Password}, &out)
		if err != nil {
			return Status{}, fmt.Errorf("%s: login failed: %w", i.ID(), err)
		}
		c = Credential{Method: Password, Token: out.Token}
	case Token:
		t := NormalizeToken(r.Token)
		if t == "" {
			return Status{}, errors.New("token login needs a token")
		}
		c = Credential{Method: Token, Token: t}
	case Browser:
		tok, refresh, err := s.cdBrowserLogin(ctx, i)
		if err != nil {
			return Status{}, err
		}
		c = Credential{Method: Browser, Token: tok, RefreshToken: refresh}
	default:
		return Status{}, fmt.Errorf("unknown method %q for Argo CD (password, browser or token)", r.Method)
	}
	// Check the token before keeping it.
	var info struct {
		LoggedIn bool   `json:"loggedIn"`
		Username string `json:"username"`
	}
	if err := cdRequest(ctx, i, http.MethodGet, "/api/v1/session/userinfo", c.Token, nil, &info); err != nil {
		return Status{}, fmt.Errorf("%s: the server did not accept the new token: %w", i.ID(), err)
	}
	if !info.LoggedIn {
		return Status{}, fmt.Errorf("%s: the server did not accept the new token", i.ID())
	}
	if err := s.save(i, c); err != nil {
		return Status{}, err
	}
	st := s.Status(i)
	if st.Identity == "" {
		st.Identity = info.Username
	}
	return st, nil
}

// CDToken returns a usable token for an Argo CD instance, renewing an
// expired SSO token with its refresh token as argocd does.
func (s *Sessions) CDToken(ctx context.Context, i Instance) (string, error) {
	c, err := s.credential(i)
	if err != nil {
		return "", err
	}
	if c.Token == "" {
		return "", &NeedsLogin{Instance: i, Reason: "not logged in"}
	}
	exp := ParseClaims(c.Token).Expiry()
	if exp == nil || exp.After(s.now().Add(30*time.Second)) {
		return c.Token, nil
	}
	if c.RefreshToken == "" {
		return "", &NeedsLogin{Instance: i, Reason: "the session expired at " + exp.Format(time.RFC3339)}
	}
	conf, _, err := s.cdOAuth(ctx, i)
	if err != nil {
		return "", err
	}
	tok, err := conf.TokenSource(ctx, &oauth2.Token{RefreshToken: c.RefreshToken}).Token()
	if err != nil {
		return "", &NeedsLogin{Instance: i, Reason: "the session expired and could not be renewed: " + err.Error()}
	}
	id, ok := tok.Extra("id_token").(string)
	if !ok {
		return "", &NeedsLogin{Instance: i, Reason: "the session expired; renewing it returned no id_token"}
	}
	refresh, _ := tok.Extra("refresh_token").(string)
	if refresh == "" {
		refresh = c.RefreshToken
	}
	if err := s.save(i, Credential{Method: c.Method, Token: id, RefreshToken: refresh}); err != nil {
		return "", err
	}
	return id, nil
}

// CD calls an Argo CD API with the instance's credential. A 401 marks the
// instance as needing login.
func (s *Sessions) CD(ctx context.Context, i Instance, method, path string, body, out any) error {
	tok, err := s.CDToken(ctx, i)
	if err != nil {
		return err
	}
	err = cdRequest(ctx, i, method, path, tok, body, out)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusUnauthorized {
		s.setRejected(i, true)
		return &NeedsLogin{Instance: i, Reason: "the server rejected the token"}
	}
	return err
}

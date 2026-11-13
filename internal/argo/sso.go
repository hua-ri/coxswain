package argo

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"math/big"
	"net"
	"net/http"
	"os/exec"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// What argocd login --sso uses (argo-cd v3.2 common/common.go, util/oidc).
const (
	cdCLIClientID      = "argo-cd-cli"
	cdDexPath          = "/api/dex"
	cdDexFederated     = "federated:id"
	DefaultSSOPort     = 8085
	BrowserLoginWindow = 5 * time.Minute
)

var defaultScopes = []string{"openid", "profile", "email", "groups"}

// cdOAuth is argocd's OIDCConfig: the OAuth2 client the CLI uses for an
// instance (its own client ID with an external OIDC provider, argo-cd-cli
// with the bundled Dex).
func (s *Sessions) cdOAuth(ctx context.Context, i Instance) (*oauth2.Config, CDSettings, error) {
	set, err := CDSettingsOf(ctx, i)
	if err != nil {
		return nil, set, err
	}
	hc, err := HTTPClient(i)
	if err != nil {
		return nil, set, err
	}
	ctx = oidc.ClientContext(ctx, hc)
	var clientID, issuer string
	var scopes []string
	switch {
	case set.OIDCConfig != nil && set.OIDCConfig.Issuer != "":
		clientID = set.OIDCConfig.CLIClientID
		if clientID == "" {
			clientID = set.OIDCConfig.ClientID
		}
		issuer = set.OIDCConfig.Issuer
		scopes = slices.Clone(set.OIDCConfig.Scopes)
		if len(scopes) == 0 {
			scopes = slices.Clone(defaultScopes)
		}
	case set.DexConfig != nil && len(set.DexConfig.Connectors) > 0:
		clientID = cdCLIClientID
		scopes = append(slices.Clone(defaultScopes), cdDexFederated)
		base := set.URL
		if base == "" {
			base = i.URL
		}
		issuer = base + cdDexPath
	default:
		return nil, set, fmt.Errorf("%s is not configured with SSO; log in with a password or a token", i.ID())
	}
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, set, fmt.Errorf("%s: failed to query the SSO provider %q: %w", i.ID(), issuer, err)
	}
	var disc struct {
		ScopesSupported        []string `json:"scopes_supported"`
		ResponseTypesSupported []string `json:"response_types_supported"`
	}
	if err := provider.Claims(&disc); err != nil {
		return nil, set, fmt.Errorf("%s: failed to parse the SSO provider's configuration: %w", i.ID(), err)
	}
	// scopes_supported is only recommended; without it assume offline_access.
	if len(disc.ScopesSupported) == 0 || slices.Contains(disc.ScopesSupported, oidc.ScopeOfflineAccess) {
		scopes = append(scopes, oidc.ScopeOfflineAccess)
	}
	conf := &oauth2.Config{ClientID: clientID, Scopes: scopes, Endpoint: provider.Endpoint()}
	if !slices.Contains(disc.ResponseTypesSupported, "code") {
		return nil, set, fmt.Errorf("%s: the SSO provider offers only the implicit flow, which Coxswain does not support; "+
			"log in with a token instead", i.ID())
	}
	return conf, set, nil
}

const verifierChars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"

func randomString(n int, chars string) (string, error) {
	b := make([]byte, n)
	for k := range b {
		v, err := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		if err != nil {
			return "", err
		}
		b[k] = chars[v.Int64()]
	}
	return string(b), nil
}

// ErrLoginCancelled is returned when the user cancels a browser login.
var ErrLoginCancelled = errors.New("browser login cancelled")

// CancelLogin cancels a browser login in progress for the instance.
func (s *Sessions) CancelLogin(i Instance) bool {
	s.mu.Lock()
	cancel := s.logins[i.ID()]
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return cancel != nil
}

// LoginPending tells whether a browser login is waiting for the user.
func (s *Sessions) LoginPending(i Instance) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.logins[i.ID()] != nil
}

// cdBrowserLogin is argocd's oauth2Login: an authorization code flow with
// PKCE, the user's browser, and a callback on localhost:<sso_port>.
func (s *Sessions) cdBrowserLogin(ctx context.Context, i Instance) (token, refresh string, err error) {
	conf, set, err := s.cdOAuth(ctx, i)
	if err != nil {
		return "", "", err
	}
	port := i.SSOPort
	if port == 0 {
		port = DefaultSSOPort
	}
	conf.RedirectURL = "http://localhost:" + strconv.Itoa(port) + "/auth/callback"

	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return "", "", fmt.Errorf("%s: browser login needs local port %d for the SSO callback, and it is in use "+
			"(another login, or argocd login --sso?); try again, or set the instance's sso_port: %w", i.ID(), port, err)
	}

	ctx, cancel := context.WithTimeout(ctx, BrowserLoginWindow)
	defer cancel()
	cancelled := make(chan struct{})
	var once sync.Once
	s.mu.Lock()
	if s.logins == nil {
		s.logins = map[string]func(){}
	}
	s.logins[i.ID()] = func() { once.Do(func() { close(cancelled) }) }
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.logins, i.ID())
		s.mu.Unlock()
	}()

	state, err := randomString(24, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz")
	if err != nil {
		return "", "", err
	}
	verifier, err := randomString(43, verifierChars)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	type result struct {
		token, refresh string
		err            error
	}
	done := make(chan result, 1)
	finish := func(r result) {
		select {
		case done <- r:
		default:
		}
	}
	hc, err := HTTPClient(i)
	if err != nil {
		ln.Close()
		return "", "", err
	}
	exchangeCtx := oidc.ClientContext(ctx, hc)
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		fail := func(msg string) {
			http.Error(w, html.EscapeString(msg), http.StatusBadRequest)
			finish(result{err: fmt.Errorf("%s: browser login failed: %s", i.ID(), msg)})
		}
		if e := r.FormValue("error"); e != "" {
			fail(e + ": " + r.FormValue("error_description"))
			return
		}
		if r.FormValue("state") != state {
			http.Error(w, "Unknown state nonce", http.StatusBadRequest)
			return // a stray request; keep waiting for the real one
		}
		code := r.FormValue("code")
		if code == "" {
			fail("no code in the callback")
			return
		}
		tok, err := conf.Exchange(exchangeCtx, code, oauth2.SetAuthURLParam("code_verifier", verifier))
		if err != nil {
			fail(err.Error())
			return
		}
		id, ok := tok.Extra("id_token").(string)
		if !ok {
			fail("no id_token in the token response")
			return
		}
		rt, _ := tok.Extra("refresh_token").(string)
		fmt.Fprint(w, successPage)
		finish(result{token: id, refresh: rt})
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	defer func() {
		sctx, c := context.WithTimeout(context.Background(), time.Second)
		defer c()
		srv.Shutdown(sctx)
	}()

	opts := []oauth2.AuthCodeOption{oauth2.AccessTypeOffline,
		oauth2.SetAuthURLParam("code_challenge", challenge),
		oauth2.SetAuthURLParam("code_challenge_method", "S256")}
	if set.OIDCConfig != nil && len(set.OIDCConfig.IDTokenClaims) > 0 && string(set.OIDCConfig.IDTokenClaims) != "null" {
		claims, _ := json.Marshal(map[string]json.RawMessage{"id_token": set.OIDCConfig.IDTokenClaims})
		opts = append(opts, oauth2.SetAuthURLParam("claims", string(claims)))
	}
	authURL := conf.AuthCodeURL(state, opts...)
	open := s.OpenBrowser
	if open == nil {
		open = func(u string) error { return exec.Command("/usr/bin/open", u).Run() }
	}
	if err := open(authURL); err != nil {
		return "", "", fmt.Errorf("%s: cannot open the browser: %w", i.ID(), err)
	}

	select {
	case r := <-done:
		return r.token, r.refresh, r.err
	case <-cancelled:
		return "", "", ErrLoginCancelled
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", "", fmt.Errorf("%s: browser login was not completed within %s", i.ID(), BrowserLoginWindow)
		}
		return "", "", ctx.Err()
	}
}

const successPage = `<!doctype html><meta charset="utf-8"><title>Coxswain</title>
<body style="font:15px -apple-system,sans-serif;margin:15vh auto;max-width:28em;text-align:center;color:#16181d">
<p style="font-size:18px;font-weight:600">Logged in</p><p>You can close this tab and return to Coxswain.</p>
<script>setTimeout(function(){window.close()},3000)</script></body>`

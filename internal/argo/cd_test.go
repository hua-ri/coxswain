package argo

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hua-ri/coxswain/internal/secrets"
)

// jwt makes an unsigned JWT with the claims (only ever parsed, never
// verified, by Coxswain).
func jwt(claims map[string]any) string {
	b, _ := json.Marshal(claims)
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc(b) + ".sig"
}

// fakeCD is an argocd-server with a Dex or external OIDC provider.
type fakeCD struct {
	srv       *httptest.Server
	settings  map[string]any
	mu        sync.Mutex
	valid     map[string]bool // accepted bearer tokens
	challenge string          // last PKCE challenge seen at /authorize
	clientID  string
	refreshes int
	idExp     time.Time
}

func newFakeCD(t *testing.T, sso string) *fakeCD {
	f := &fakeCD{valid: map[string]bool{}, idExp: time.Now().Add(time.Hour)}
	mux := http.NewServeMux()
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	issuer := f.srv.URL + "/api/dex"
	f.settings = map[string]any{"url": f.srv.URL}
	switch sso {
	case "dex":
		f.settings["dexConfig"] = map[string]any{"connectors": []any{map[string]any{"name": "GitHub", "type": "github"}}}
	case "oidc":
		issuer = f.srv.URL + "/oidc"
		f.settings["oidcConfig"] = map[string]any{"name": "Okta", "issuer": issuer, "clientID": "web", "cliClientID": "cli"}
	}
	mux.HandleFunc("GET /api/v1/settings", func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(f.settings) })
	mux.HandleFunc("POST /api/v1/session", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		json.NewDecoder(r.Body).Decode(&in)
		if in["username"] != "admin" || in["password"] != "pw" {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"error":"Invalid username or password","code":16,"message":"Invalid username or password"}`)
			return
		}
		tok := jwt(map[string]any{"sub": "admin:login", "exp": float64(time.Now().Add(24 * time.Hour).Unix())})
		f.allow(tok)
		json.NewEncoder(w).Encode(map[string]string{"token": tok})
	})
	mux.HandleFunc("GET /api/v1/session/userinfo", func(w http.ResponseWriter, r *http.Request) {
		if !f.ok(r) {
			w.WriteHeader(401)
			return
		}
		fmt.Fprint(w, `{"loggedIn":true,"username":"admin"}`)
	})
	mux.HandleFunc("GET /api/v1/applications", func(w http.ResponseWriter, r *http.Request) {
		if !f.ok(r) {
			w.WriteHeader(401)
			return
		}
		fmt.Fprint(w, `{"items":[]}`)
	})
	prefix := strings.TrimPrefix(issuer, f.srv.URL)
	mux.HandleFunc("GET "+prefix+"/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer": issuer, "authorization_endpoint": issuer + "/auth", "token_endpoint": issuer + "/token",
			"jwks_uri": issuer + "/keys", "response_types_supported": []string{"code"},
			"scopes_supported": []string{"openid", "offline_access"},
		})
	})
	mux.HandleFunc("POST "+prefix+"/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		f.clientID = r.Form.Get("client_id")
		if f.clientID == "" {
			f.clientID, _, _ = r.BasicAuth()
		}
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if r.Form.Get("code") != "the-code" || base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge {
				w.WriteHeader(400)
				fmt.Fprint(w, `{"error":"invalid_grant"}`)
				return
			}
		case "refresh_token":
			if r.Form.Get("refresh_token") != "rt-1" {
				w.WriteHeader(400)
				fmt.Fprint(w, `{"error":"invalid_grant"}`)
				return
			}
			f.refreshes++
		}
		id := jwt(map[string]any{"sub": "Cg", "email": "dev@example.com", "exp": float64(f.idExp.Unix())})
		f.valid[id] = true
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "bearer", "id_token": id, "refresh_token": "rt-1"})
	})
	return f
}

func (f *fakeCD) allow(tok string) {
	f.mu.Lock()
	f.valid[tok] = true
	f.mu.Unlock()
}

func (f *fakeCD) ok(r *http.Request) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.valid[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
}

func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func newSessions(t *testing.T) *Sessions {
	return &Sessions{Store: NewStore(filepath.Join(t.TempDir(), "argo.json")), Secrets: &secrets.Memory{}}
}

func TestCDMethods(t *testing.T) {
	s := newSessions(t)
	for sso, want := range map[string]string{"": "password,token", "dex": "browser,password,token", "oidc": "browser,password,token"} {
		f := newFakeCD(t, sso)
		m := s.CDMethods(context.Background(), Instance{Kind: CD, Name: "a", URL: f.srv.URL})
		got := []string{}
		for _, x := range m.Methods {
			got = append(got, string(x.Method))
		}
		if strings.Join(got, ",") != want || m.Problem != "" {
			t.Errorf("%s: methods %v, problem %q", sso, got, m.Problem)
		}
	}
	f := newFakeCD(t, "dex")
	f.settings["userLoginsDisabled"] = true
	m := s.CDMethods(context.Background(), Instance{Kind: CD, Name: "a", URL: f.srv.URL})
	if len(m.Methods) != 2 || m.Methods[0].Label != "Browser (GitHub)" {
		t.Errorf("logins disabled: %+v", m.Methods)
	}
}

func TestCDPasswordLogin(t *testing.T) {
	s := newSessions(t)
	f := newFakeCD(t, "")
	i := Instance{Kind: CD, Name: "a", URL: f.srv.URL}
	if _, err := s.CDLogin(context.Background(), i, LoginRequest{Method: Password, Username: "admin", Password: "nope"}); err == nil ||
		!strings.Contains(err.Error(), "Invalid username or password") {
		t.Fatalf("bad password: %v", err)
	}
	st, err := s.CDLogin(context.Background(), i, LoginRequest{Method: Password, Username: "admin", Password: "pw"})
	if err != nil || !st.LoggedIn || st.Identity != "admin" || st.Method != Password || st.ExpiresAt == nil || len(st.TokenHash) != 8 {
		t.Fatalf("login = %+v, %v", st, err)
	}
	stored, _ := s.Secrets.Get(i.Account())
	if strings.Contains(string(stored), "pw\"") {
		t.Fatal("password kept")
	}
	if err := s.CD(context.Background(), i, http.MethodGet, "/api/v1/applications", nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestCDTokenLoginRejected(t *testing.T) {
	s := newSessions(t)
	f := newFakeCD(t, "")
	i := Instance{Kind: CD, Name: "a", URL: f.srv.URL}
	if _, err := s.CDLogin(context.Background(), i, LoginRequest{Method: Token, Token: "Bearer bogus"}); err == nil {
		t.Fatal("bogus token accepted")
	}
	f.allow("good")
	if _, err := s.CDLogin(context.Background(), i, LoginRequest{Method: Token, Token: " Bearer good "}); err != nil {
		t.Fatal(err)
	}
	// The server revokes it: the next call says the user must log in.
	f.mu.Lock()
	delete(f.valid, "good")
	f.mu.Unlock()
	err := s.CD(context.Background(), i, http.MethodGet, "/api/v1/applications", nil, nil)
	var nl *NeedsLogin
	if !errors.As(err, &nl) || s.Status(i).LoggedIn {
		t.Fatalf("after revoke: %v, %+v", err, s.Status(i))
	}
}

// browser plays the user: it follows the authorization URL to the
// callback with a code, after checking what argocd would send.
func browser(t *testing.T, f *fakeCD, wantClient string, result chan<- error) func(string) error {
	return func(auth string) error {
		u, err := url.Parse(auth)
		if err != nil {
			return err
		}
		q := u.Query()
		if q.Get("client_id") != wantClient || q.Get("code_challenge_method") != "S256" || !strings.Contains(q.Get("scope"), "offline_access") {
			t.Errorf("authorization URL %s", auth)
		}
		f.mu.Lock()
		f.challenge = q.Get("code_challenge")
		f.mu.Unlock()
		go func() {
			// A stray request with the wrong state must not end the login.
			http.Get(q.Get("redirect_uri") + "?code=the-code&state=wrong")
			resp, err := http.Get(q.Get("redirect_uri") + "?code=the-code&state=" + url.QueryEscape(q.Get("state")))
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode != 200 {
					err = fmt.Errorf("callback answered %d", resp.StatusCode)
				}
			}
			result <- err
		}()
		return nil
	}
}

func TestCDBrowserLogin(t *testing.T) {
	for sso, client := range map[string]string{"dex": "argo-cd-cli", "oidc": "cli"} {
		t.Run(sso, func(t *testing.T) {
			s := newSessions(t)
			f := newFakeCD(t, sso)
			i := Instance{Kind: CD, Name: "a", URL: f.srv.URL, SSOPort: freePort(t)}
			cb := make(chan error, 1)
			s.OpenBrowser = browser(t, f, client, cb)
			st, err := s.CDLogin(context.Background(), i, LoginRequest{Method: Browser})
			if err != nil {
				t.Fatal(err)
			}
			if err := <-cb; err != nil {
				t.Fatal(err)
			}
			if !st.LoggedIn || st.Identity != "dev@example.com" || !st.Renewable || f.clientID != client {
				t.Fatalf("status %+v, token client %q", st, f.clientID)
			}
		})
	}
}

func TestCDBrowserLoginCancel(t *testing.T) {
	s := newSessions(t)
	f := newFakeCD(t, "dex")
	i := Instance{Kind: CD, Name: "a", URL: f.srv.URL, SSOPort: freePort(t)}
	opened := make(chan struct{})
	s.OpenBrowser = func(string) error { close(opened); return nil }
	errc := make(chan error, 1)
	go func() {
		_, err := s.CDLogin(context.Background(), i, LoginRequest{Method: Browser})
		errc <- err
	}()
	<-opened
	if !s.LoginPending(i) || !s.CancelLogin(i) {
		t.Fatal("no pending login to cancel")
	}
	if err := <-errc; !errors.Is(err, ErrLoginCancelled) {
		t.Fatalf("cancel: %v", err)
	}
	if s.LoginPending(i) {
		t.Fatal("still pending")
	}
}

func TestCDBrowserLoginPortInUse(t *testing.T) {
	s := newSessions(t)
	f := newFakeCD(t, "dex")
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	i := Instance{Kind: CD, Name: "a", URL: f.srv.URL, SSOPort: ln.Addr().(*net.TCPAddr).Port}
	s.OpenBrowser = func(string) error { t.Error("browser opened"); return nil }
	if _, err := s.CDLogin(context.Background(), i, LoginRequest{Method: Browser}); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("port in use: %v", err)
	}
}

func TestCDTokenRefresh(t *testing.T) {
	s := newSessions(t)
	f := newFakeCD(t, "dex")
	f.idExp = time.Now().Add(-time.Minute) // the provider hands out an already expired token first
	i := Instance{Kind: CD, Name: "a", URL: f.srv.URL, SSOPort: freePort(t)}
	cb := make(chan error, 1)
	s.OpenBrowser = browser(t, f, "argo-cd-cli", cb)
	// userinfo accepts it (the fake ignores exp), so the login is kept.
	if _, err := s.CDLogin(context.Background(), i, LoginRequest{Method: Browser}); err != nil {
		t.Fatal(err)
	}
	<-cb
	f.idExp = time.Now().Add(time.Hour)
	if err := s.CD(context.Background(), i, http.MethodGet, "/api/v1/applications", nil, nil); err != nil {
		t.Fatal(err)
	}
	if f.refreshes != 1 {
		t.Fatalf("refreshes = %d", f.refreshes)
	}
	if exp := s.Status(i).ExpiresAt; exp == nil || exp.Before(time.Now()) {
		t.Fatalf("token not renewed: %v", exp)
	}
}

func TestClaimsIdentity(t *testing.T) {
	cases := map[string]map[string]any{
		"admin":           {"sub": "admin:login"},
		"dev@example.com": {"sub": "x", "email": "dev@example.com"},
		"system:serviceaccount:kube-system:admin": {"sub": "system:serviceaccount:kube-system:admin"},
	}
	for want, c := range cases {
		if got := ParseClaims(jwt(c)).Identity(); got != want {
			t.Errorf("Identity(%v) = %q", c, got)
		}
	}
	if ParseClaims("v2:opaque") != nil {
		t.Error("non-JWT parsed")
	}
}

package argo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeWF is an argo-server; token "" means server auth mode.
func fakeWF(t *testing.T, token string) *httptest.Server {
	mux := http.NewServeMux()
	auth := func(w http.ResponseWriter, r *http.Request) bool {
		if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"code":16,"message":"token not valid. see https://argo-workflows.readthedocs.io/en/latest/faq/"}`)
			return false
		}
		return true
	}
	mux.HandleFunc("GET /api/v1/workflows/{ns}", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		if r.PathValue("ns") == "forbidden" {
			w.WriteHeader(403)
			fmt.Fprint(w, `{"code":7,"message":"workflows.argoproj.io is forbidden"}`)
			return
		}
		fmt.Fprint(w, `{"metadata":{},"items":null}`)
	})
	mux.HandleFunc("GET /api/v1/userinfo", func(w http.ResponseWriter, r *http.Request) {
		if auth(w, r) {
			fmt.Fprint(w, `{"serviceAccountName":"argo-ops","serviceAccountNamespace":"argo"}`)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestWFMethods(t *testing.T) {
	s := newSessions(t)
	client := fakeWF(t, "tok")
	m := s.WFMethods(context.Background(), Instance{Kind: Workflows, Name: "a", URL: client.URL, Namespace: "argo"})
	if m.Methods[0].Method != Token || m.Problem != "" {
		t.Fatalf("client mode: %+v", m)
	}
	server := fakeWF(t, "")
	m = s.WFMethods(context.Background(), Instance{Kind: Workflows, Name: "b", URL: server.URL, Namespace: "argo"})
	if m.Methods[0].Method != None {
		t.Fatalf("server mode: %+v", m)
	}
	m = s.WFMethods(context.Background(), Instance{Kind: Workflows, Name: "c", URL: "http://127.0.0.1:1", Namespace: "argo"})
	if m.Problem == "" || len(m.Methods) != 2 {
		t.Fatalf("unreachable: %+v", m)
	}
}

func TestWFLogin(t *testing.T) {
	s := newSessions(t)
	srv := fakeWF(t, "tok")
	i := Instance{Kind: Workflows, Name: "a", URL: srv.URL, Namespace: "argo"}
	if err := s.WF(context.Background(), i, http.MethodGet, wfListPath("argo", 0), nil, nil); !errors.As(err, new(*NeedsLogin)) {
		t.Fatalf("before login: %v", err)
	}
	if _, err := s.WFLogin(context.Background(), i, LoginRequest{Method: Token, Token: "wrong"}); err == nil || !strings.Contains(err.Error(), "token not valid") {
		t.Fatalf("wrong token: %v", err)
	}
	if _, err := s.WFLogin(context.Background(), i, LoginRequest{Method: None}); err == nil {
		t.Fatal("none accepted by a client-mode server")
	}
	st, err := s.WFLogin(context.Background(), i, LoginRequest{Method: Token, Token: "Bearer tok"})
	if err != nil || !st.LoggedIn || st.Identity != "system:serviceaccount:argo:argo-ops" {
		t.Fatalf("login = %+v, %v", st, err)
	}
	if err := s.WF(context.Background(), i, http.MethodGet, wfListPath("argo", 0), nil, nil); err != nil {
		t.Fatal(err)
	}
	forbidden := i
	forbidden.Namespace = "forbidden"
	if _, err := s.WFLogin(context.Background(), forbidden, LoginRequest{Method: Token, Token: "tok"}); err == nil || !strings.Contains(err.Error(), "may not list") {
		t.Fatalf("forbidden: %v", err)
	}
}

func TestWFServerMode(t *testing.T) {
	s := newSessions(t)
	srv := fakeWF(t, "")
	i := Instance{Kind: Workflows, Name: "a", URL: srv.URL, Namespace: "argo"}
	st, err := s.WFLogin(context.Background(), i, LoginRequest{Method: None})
	if err != nil || !st.LoggedIn || st.Method != None {
		t.Fatalf("none = %+v, %v", st, err)
	}
	if err := s.WF(context.Background(), i, http.MethodGet, wfListPath("argo", 0), nil, nil); err != nil {
		t.Fatal(err)
	}
}

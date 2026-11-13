package argo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// WFMethods lists how one can log in to an Argo Workflows instance. The
// server has no accounts: it takes a token (client or sso auth mode) or
// nothing (server auth mode). Coxswain asks once without a credential to
// suggest which; the user chooses.
func (s *Sessions) WFMethods(ctx context.Context, i Instance) Methods {
	m := Methods{Instance: i.Name, URL: i.URL, Status: s.Status(i)}
	tokenInfo := MethodInfo{Method: Token, Label: "Token",
		Detail: "argo-server in client mode takes a Kubernetes token (e.g. kubectl create token <service account>); " +
			"in sso mode, the token from the Argo UI's user info page (v2:…)"}
	noneInfo := MethodInfo{Method: None, Label: "No credential", Detail: "argo-server in server mode needs none"}
	err := s.wfRequest(ctx, i, http.MethodGet, wfListPath(i.Namespace, 1), "", nil, nil)
	var ae *APIError
	switch {
	case err == nil:
		m.Methods = []MethodInfo{noneInfo, tokenInfo}
	case errors.As(err, &ae) && (ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden):
		m.Methods = []MethodInfo{tokenInfo, noneInfo}
	default:
		m.Methods = []MethodInfo{tokenInfo, noneInfo}
		m.Problem = fmt.Sprintf("%s: cannot reach it (is %s an Argo Workflows server?): %v", i.ID(), i.URL, err)
	}
	return m
}

func wfListPath(ns string, limit int) string {
	q := url.Values{}
	if limit > 0 {
		q.Set("listOptions.limit", fmt.Sprint(limit))
	}
	q.Set("fields", "items.metadata.name")
	return "/api/v1/workflows/" + url.PathEscape(ns) + "?" + q.Encode()
}

func (s *Sessions) wfRequest(ctx context.Context, i Instance, method, path, token string, body, out any) error {
	hc, err := HTTPClient(i)
	if err != nil {
		return err
	}
	return doJSON(ctx, hc, i, method, i.URL+path, token, body, out)
}

// WFLogin keeps a credential for an Argo Workflows instance after
// checking it can list workflows in the default namespace.
func (s *Sessions) WFLogin(ctx context.Context, i Instance, r LoginRequest) (Status, error) {
	var c Credential
	switch r.Method {
	case Token:
		t := NormalizeToken(r.Token)
		if t == "" {
			return Status{}, errors.New("token login needs a token")
		}
		c = Credential{Method: Token, Token: t}
	case None:
		c = Credential{Method: None}
	default:
		return Status{}, fmt.Errorf("unknown method %q for Argo Workflows (token or none)", r.Method)
	}
	if err := s.wfRequest(ctx, i, http.MethodGet, wfListPath(i.Namespace, 1), c.Token, nil, nil); err != nil {
		var ae *APIError
		if errors.As(err, &ae) && ae.Status == http.StatusForbidden {
			return Status{}, fmt.Errorf("%s: the token works but may not list workflows in namespace %q: %w", i.ID(), i.Namespace, err)
		}
		return Status{}, fmt.Errorf("%s: the server did not accept it: %w", i.ID(), err)
	}
	if err := s.save(i, c); err != nil {
		return Status{}, err
	}
	st := s.Status(i)
	if st.Identity == "" && c.Token != "" {
		var who struct {
			Subject                 string `json:"subject"`
			Email                   string `json:"email"`
			ServiceAccountName      string `json:"serviceAccountName"`
			ServiceAccountNamespace string `json:"serviceAccountNamespace"`
		}
		if s.wfRequest(ctx, i, http.MethodGet, "/api/v1/userinfo", c.Token, nil, &who) == nil {
			switch {
			case who.Email != "":
				st.Identity = who.Email
			case who.ServiceAccountName != "":
				st.Identity = "system:serviceaccount:" + who.ServiceAccountNamespace + ":" + who.ServiceAccountName
			default:
				st.Identity = who.Subject
			}
		}
	}
	return st, nil
}

// WF calls an Argo Workflows API with the instance's credential.
func (s *Sessions) WF(ctx context.Context, i Instance, method, path string, body, out any) error {
	c, err := s.credential(i)
	if err != nil {
		return err
	}
	if c.Method == "" {
		return &NeedsLogin{Instance: i, Reason: "no token configured"}
	}
	err = s.wfRequest(ctx, i, method, path, c.Token, body, out)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusUnauthorized {
		s.setRejected(i, true)
		return &NeedsLogin{Instance: i, Reason: "the server rejected the token"}
	}
	return err
}

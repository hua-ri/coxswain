package argo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	i := Instance{Kind: CD, Name: "prod", URL: "argocd.example.com/"}
	if err := i.Normalize(); err != nil || i.URL != "https://argocd.example.com" {
		t.Fatalf("Normalize = %v, %q", err, i.URL)
	}
	bad := []Instance{
		{Kind: "x", Name: "a", URL: "https://a"},
		{Kind: CD, Name: "a b", URL: "https://a"},
		{Kind: CD, Name: "a", URL: "ftp://a"},
		{Kind: CD, Name: "a", URL: "https://u:p@a"},
		{Kind: CD, Name: "a", URL: "https://a", Namespace: "argo"},
		{Kind: Workflows, Name: "a", URL: "https://a", SSOPort: 8085},
		{Kind: CD, Name: "a", URL: "https://a", SSOPort: 80},
	}
	for _, b := range bad {
		if err := b.Normalize(); err == nil {
			t.Errorf("accepted %+v", b)
		}
	}
}

func TestStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "argo.json")
	s := NewStore(path)
	if list, err := s.List(""); err != nil || len(list) != 0 {
		t.Fatalf("empty List = %v, %v", list, err)
	}
	add := func(i Instance) error {
		return s.Update(func(l []Instance) ([]Instance, error) { return append(l, i), nil })
	}
	if err := add(Instance{Kind: Workflows, Name: "wf", URL: "http://localhost:2746", Namespace: "argo"}); err != nil {
		t.Fatal(err)
	}
	if err := add(Instance{Kind: CD, Name: "prod", URL: "https://cd/", Tags: map[string]string{"env": "prod"}}); err != nil {
		t.Fatal(err)
	}
	if err := add(Instance{Kind: CD, Name: "prod", URL: "https://other"}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate = %v", err)
	}
	cd, err := s.List(CD)
	if err != nil || len(cd) != 1 || cd[0].URL != "https://cd" {
		t.Fatalf("List(CD) = %+v, %v", cd, err)
	}
	if _, err := s.Get(Workflows, "prod"); err == nil {
		t.Fatal("Get across kinds should fail")
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
}

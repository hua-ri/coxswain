package ui

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestPick(t *testing.T) {
	built := fstest.MapFS{
		"dist/index.html":          {Data: []byte("placeholder")},
		"dist/app/index.html":      {Data: []byte("app")},
		"dist/app/assets/index.js": {Data: []byte("js")},
	}
	fsys, ok := pick(built)
	if b, _ := fsys.Open("index.html"); !ok || read(t, b) != "app" {
		t.Fatal("built app not picked")
	}
	if _, err := fsys.Open("assets/index.js"); err != nil {
		t.Fatal(err)
	}
	fsys, ok = pick(fstest.MapFS{"dist/index.html": {Data: []byte("placeholder")}})
	if b, _ := fsys.Open("index.html"); ok || read(t, b) != "placeholder" {
		t.Fatal("placeholder not picked")
	}
}

func read(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestHandler(t *testing.T) {
	w := httptest.NewRecorder()
	Handler().ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "<html") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Fatalf("csp %q", csp)
	}
	w = httptest.NewRecorder()
	Handler().ServeHTTP(w, httptest.NewRequest("POST", "/", nil))
	if w.Code != 405 {
		t.Fatalf("POST: %d", w.Code)
	}
}

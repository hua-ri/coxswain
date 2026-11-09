package k8stest

import (
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// Request is one request the fake API server received.
type Request struct {
	Method      string
	Path        string
	Query       string
	Auth        string // Authorization header
	ContentType string
	Body        string
}

// APIServer is a minimal (TLS) Kubernetes API server: discovery for core/v1 and
// apps/v1, a namespace list, and PATCH (server-side apply) that echoes
// the object back. Every request is recorded.
type APIServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []Request
	handlers map[string]http.HandlerFunc // "METHOD /path"
	objects  map[string]*stored          // by path: objects with server-side apply semantics
}

// stored is an object and who manages each of its fields, as
// "manager/operation" (Update or Apply), like managedFields.
type stored struct {
	obj    map[string]any
	owners map[string]string // ".spec.replicas" → "cox-ops/Update"
}

// Seed stores an object at path (e.g. /apis/apps/v1/namespaces/ns/deployments/x).
// GET returns it; merge patches update it; server-side apply (PATCH
// application/apply-patch+yaml) updates it with field ownership: a field
// another manager (or the same name with another operation) set to a
// different value is a conflict, as on a real API server.
func (s *APIServer) Seed(path string, obj map[string]any) {
	b, _ := json.Marshal(obj)
	var norm map[string]any
	json.Unmarshal(b, &norm)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.objects == nil {
		s.objects = map[string]*stored{}
	}
	s.objects[path] = &stored{obj: norm, owners: map[string]string{}}
}

// leaves flattens m into field paths (lists are leaves).
func leaves(m map[string]any, prefix string, out map[string]any) {
	for k, v := range m {
		if sub, ok := v.(map[string]any); ok {
			leaves(sub, prefix+"."+k, out)
			continue
		}
		out[prefix+"."+k] = v
	}
}

func mergeInto(dst, src map[string]any) {
	for k, v := range src {
		if sm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				mergeInto(dm, sm)
				continue
			}
		}
		dst[k] = v
	}
}

func lookup(m map[string]any, path string) any {
	var cur any = m
	for _, k := range strings.Split(strings.TrimPrefix(path, "."), ".") {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[k]
	}
	return cur
}

// serveStored handles GET and PATCH of a seeded object; false if path is
// not one.
func (s *APIServer) serveStored(w http.ResponseWriter, r *http.Request, body []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.objects[r.URL.Path]
	if st == nil {
		return false
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, st.obj)
		return true
	case http.MethodPatch:
	default:
		return false
	}
	var patch map[string]any
	if err := json.Unmarshal(body, &patch); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return true
	}
	q := r.URL.Query()
	manager, dry := q.Get("fieldManager"), q.Get("dryRun") == "All"
	op := "Update"
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/apply-patch") {
		op = "Apply"
		delete(patch, "apiVersion")
		delete(patch, "kind")
		if md, ok := patch["metadata"].(map[string]any); ok {
			delete(md, "name")
			delete(md, "namespace")
		}
	}
	fields := map[string]any{}
	leaves(patch, "", fields)
	key := manager + "/" + op
	if op == "Apply" {
		var causes []map[string]any
		var msgs []string
		for f, v := range fields {
			owner, ok := st.owners[f]
			if !ok || owner == key {
				continue
			}
			if cur := lookup(st.obj, f); reflect.DeepEqual(cur, v) {
				continue
			}
			name := strings.SplitN(owner, "/", 2)[0]
			msg := fmt.Sprintf("conflict with %q using apps/v1", name)
			causes = append(causes, map[string]any{"reason": "FieldManagerConflict", "message": msg, "field": f})
			msgs = append(msgs, msg+": "+f)
		}
		if len(causes) > 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Failure", "code": 409,
				"reason": "Conflict", "message": fmt.Sprintf("Apply failed with %d conflict(s): %s", len(causes), strings.Join(msgs, "; ")),
				"details": map[string]any{"causes": causes}})
			return true
		}
	}
	b, _ := json.Marshal(st.obj)
	var next map[string]any
	json.Unmarshal(b, &next)
	mergeInto(next, patch)
	if !dry {
		st.obj = next
		for f := range fields {
			st.owners[f] = key
		}
	}
	writeJSON(w, next)
	return true
}

// NewAPIServer starts a fake API server that stops with the test.
func NewAPIServer(t *testing.T) *APIServer {
	s := &APIServer{handlers: map[string]http.HandlerFunc{}}
	// TLS: clientcmd sends credentials only to https servers.
	s.Server = httptest.NewTLSServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// CAData is the server's certificate, base64 PEM as in a kubeconfig's
// certificate-authority-data.
func (s *APIServer) CAData() string {
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw})
	return base64.StdEncoding.EncodeToString(pemBytes)
}

// Handle overrides the response for one method and path.
func (s *APIServer) Handle(method, path string, h http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[method+" "+path] = h
}

// Requests returns what the server received so far.
func (s *APIServer) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

func (s *APIServer) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.requests = append(s.requests, Request{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
		Auth: r.Header.Get("Authorization"), ContentType: r.Header.Get("Content-Type"), Body: string(body)})
	h := s.handlers[r.Method+" "+r.URL.Path]
	s.mu.Unlock()
	if h != nil {
		h(w, r)
		return
	}
	if s.serveStored(w, r, body) {
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api":
		writeJSON(w, map[string]any{"kind": "APIVersions", "versions": []string{"v1"},
			"serverAddressByClientCIDRs": []any{map[string]any{"clientCIDR": "0.0.0.0/0", "serverAddress": "127.0.0.1"}}})
	case r.Method == http.MethodGet && r.URL.Path == "/apis":
		writeJSON(w, map[string]any{"kind": "APIGroupList", "apiVersion": "v1", "groups": []any{
			map[string]any{"name": "apps", "versions": []any{map[string]any{"groupVersion": "apps/v1", "version": "v1"}},
				"preferredVersion": map[string]any{"groupVersion": "apps/v1", "version": "v1"}},
		}})
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1":
		writeJSON(w, map[string]any{"kind": "APIResourceList", "groupVersion": "v1", "resources": []any{
			resource("pods", "Pod", true, "po"), resource("namespaces", "Namespace", false, "ns"),
			resource("secrets", "Secret", true), resource("configmaps", "ConfigMap", true, "cm"),
		}})
	case r.Method == http.MethodGet && r.URL.Path == "/apis/apps/v1":
		writeJSON(w, map[string]any{"kind": "APIResourceList", "groupVersion": "apps/v1", "resources": []any{
			resource("deployments", "Deployment", true, "deploy"),
		}})
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces":
		writeJSON(w, map[string]any{"kind": "NamespaceList", "apiVersion": "v1", "metadata": map[string]any{},
			"items": []any{map[string]any{"metadata": map[string]any{"name": "default"}}}})
	case r.Method == http.MethodPatch:
		// Server-side apply: echo the object with server fields added.
		var obj map[string]any
		if err := json.Unmarshal(body, &obj); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		md, _ := obj["metadata"].(map[string]any)
		if md == nil {
			md = map[string]any{}
			obj["metadata"] = md
		}
		md["resourceVersion"] = "1"
		md["uid"] = "uid-1"
		if parts := strings.Split(r.URL.Path, "/"); len(parts) > 4 && parts[len(parts)-3] == "namespaces" {
			md["namespace"] = parts[len(parts)-2]
		}
		writeJSON(w, obj)
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Failure",
			"reason": "NotFound", "code": 404, "message": r.URL.Path + " not found"})
	}
}

func resource(name, kind string, namespaced bool, short ...string) map[string]any {
	return map[string]any{"name": name, "singularName": strings.ToLower(kind), "kind": kind, "namespaced": namespaced,
		"shortNames": short, "verbs": []string{"get", "list", "create", "update", "patch", "delete"}}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

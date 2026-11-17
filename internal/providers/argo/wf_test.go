package argo

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

const buildWF = `{
 "metadata":{"name":"build-x7","namespace":"ci","creationTimestamp":"2026-11-13T09:00:00Z"},
 "spec":{"workflowTemplateRef":{"name":"build"},"arguments":{"parameters":[{"name":"branch","value":"main"}]}},
 "status":{"phase":"Failed","progress":"1/3","message":"child 'build-x7-2' failed",
  "startedAt":"2026-11-13T09:00:00Z","finishedAt":"2026-11-13T09:02:30Z",
  "nodes":{
   "build-x7":{"id":"build-x7","name":"build-x7","displayName":"build-x7","type":"Steps","phase":"Failed","children":["build-x7-1"]},
   "build-x7-1":{"id":"build-x7-1","name":"build-x7[0]","displayName":"[0]","type":"StepGroup","phase":"Failed","children":["build-x7-2","build-x7-3"]},
   "build-x7-2":{"id":"build-x7-2","name":"build-x7[0].compile","displayName":"compile","type":"Pod","templateName":"compile","phase":"Failed",
     "message":"OOMKilled (exit code 137)","outputs":{"exitCode":"137"},"hostNodeName":"node-9","startedAt":"2026-11-13T09:00:05Z","finishedAt":"2026-11-13T09:02:00Z"},
   "build-x7-3":{"id":"build-x7-3","name":"build-x7[0].lint","displayName":"lint","type":"Pod","templateName":"lint","phase":"Succeeded","outputs":{"exitCode":"0"}}
  }}}`

func (f *fixture) wfServer(t *testing.T) {
	t.Helper()
	f.mux.HandleFunc("GET /api/v1/workflows/ci", func(w http.ResponseWriter, r *http.Request) {
		if fields := r.URL.Query().Get("fields"); fields == "" {
			t.Error("list without fields would fetch whole node trees")
		}
		fmt.Fprintf(w, `{"items":[{"metadata":{"name":"old","creationTimestamp":"2026-11-01T00:00:00Z"},"spec":{},"status":{"phase":"Succeeded"}},%s]}`, buildWF)
	})
	f.mux.HandleFunc("GET /api/v1/workflows/ci/build-x7", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, buildWF) })
	f.mux.HandleFunc("GET /api/v1/workflows/ci/build-x7/{pod}/log", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("pod") != "build-x7-compile-1817961178" && r.PathValue("pod") != podOf("build-x7", "compile", "build-x7[0].compile") {
			w.WriteHeader(404)
			return
		}
		if r.URL.Query().Get("logOptions.container") != "main" {
			t.Errorf("container = %q", r.URL.Query().Get("logOptions.container"))
		}
		fmt.Fprint(w, `{"result":{"content":"compiling 4000 files","podName":"x"}}`+"\n"+`{"result":{"content":"fatal: out of memory","podName":"x"}}`+"\n")
	})
	f.mux.HandleFunc("GET /api/v1/workflow-templates/ci/build", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"metadata":{"name":"build"},"spec":{"arguments":{"parameters":[{"name":"branch"},{"name":"env","value":"dev","enum":["dev","prod"]}]}}}`)
	})
	f.mux.HandleFunc("POST /api/v1/workflows/ci/submit", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var in map[string]any
		json.Unmarshal(b, &in)
		opts := in["submitOptions"].(map[string]any)
		if in["resourceKind"] != "WorkflowTemplate" || in["resourceName"] != "build" || fmt.Sprint(opts["parameters"]) != "[branch=feat env=prod]" {
			t.Errorf("submit body %s", b)
		}
		fmt.Fprint(w, `{"metadata":{"name":"build-abcde","namespace":"ci"}}`)
	})
	for _, op := range []string{"suspend", "resume", "stop", "terminate", "retry", "resubmit"} {
		f.mux.HandleFunc("PUT /api/v1/workflows/ci/build-x7/"+op, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, buildWF) })
	}
	f.ok(t, human, "argo.add", map[string]any{"kind": "argowf", "name": "ci", "url": f.server.URL, "namespace": "ci"})
	f.ok(t, human, "argowf.login", map[string]any{"method": "none"})
}

// podOf mirrors argo's v2 pod names for the test's node.
func podOf(wf, tmpl, node string) string {
	h := uint32(0x811c9dc5)
	for _, b := range []byte(node) {
		h = (h ^ uint32(b)) * 0x01000193
	}
	return fmt.Sprintf("%s-%s-%d", wf, tmpl, h)
}

func TestWFListAndGet(t *testing.T) {
	f := setup(t)
	f.wfServer(t)
	list := f.ok(t, agent, "argowf.list", map[string]any{})
	if strings.Index(list, "build-x7") > strings.Index(list, `"old"`) || !strings.Contains(list, `"duration":"2m30s"`) || !strings.Contains(list, `"template":"build"`) {
		t.Fatalf("list = %s", list)
	}
	got := f.ok(t, agent, "argowf.get", map[string]any{"name": "build-x7"})
	if !strings.Contains(got, `"failed_nodes":[{"id":"build-x7-2","name":"compile","depth":2`) || !strings.Contains(got, `"exit_code":"137"`) ||
		!strings.Contains(got, `"branch":"main"`) || !strings.Contains(got, `"pod":"`+podOf("build-x7", "compile", "build-x7[0].compile")) {
		t.Fatalf("get = %s", got)
	}
}

func TestWFLogsByStepName(t *testing.T) {
	f := setup(t)
	f.wfServer(t)
	out := f.ok(t, agent, "argowf.logs", map[string]any{"name": "build-x7", "node": "compile"})
	if !strings.Contains(out, "fatal: out of memory") {
		t.Fatalf("logs = %s", out)
	}
	if o := f.call(t, agent, "argowf.logs", map[string]any{"name": "build-x7", "node": "[0]"}); o.Status == "ok" || !strings.Contains(o.Error, "not a pod") {
		t.Fatalf("logs of a step group: %+v", o)
	}
}

func TestWFDiagnose(t *testing.T) {
	f := setup(t)
	f.wfServer(t)
	out := f.ok(t, agent, "argowf.diagnose", map[string]any{"name": "build-x7"})
	for _, want := range []string{"out of memory (OOMKilled)", "fatal: out of memory", "fix the cause first"} {
		if !strings.Contains(out, want) {
			t.Errorf("diagnosis lacks %q: %s", want, out)
		}
	}
}

func TestWFSubmitChecksParameters(t *testing.T) {
	f := setup(t)
	f.wfServer(t)
	if o := f.call(t, agent, "argowf.submit", map[string]any{"template": "build"}); o.Status == "ok" || !strings.Contains(o.Error, "must be given: branch") {
		t.Fatalf("missing parameter: %+v", o)
	}
	if o := f.call(t, agent, "argowf.submit", map[string]any{"template": "build", "parameters": map[string]string{"branch": "x", "env": "qa"}}); o.Status == "ok" || !strings.Contains(o.Error, "one of dev, prod") {
		t.Fatalf("bad enum: %+v", o)
	}
	if o := f.call(t, agent, "argowf.submit", map[string]any{"template": "build", "parameters": map[string]string{"branch": "x", "nope": "1"}}); o.Status == "ok" || !strings.Contains(o.Error, "no parameter \"nope\"") {
		t.Fatalf("unknown parameter: %+v", o)
	}
	out := f.ok(t, agent, "argowf.submit", map[string]any{"template": "build", "parameters": map[string]string{"branch": "feat", "env": "prod"}})
	if !strings.Contains(out, "build-abcde") {
		t.Fatalf("submit = %s", out)
	}
}

func TestWFOpsRisk(t *testing.T) {
	f := setup(t)
	f.wfServer(t)
	for _, op := range []string{"suspend", "resume", "stop", "terminate", "retry", "resubmit"} {
		f.ok(t, agent, "argowf."+op, map[string]any{"name": "build-x7"})
	}
	audit := f.audit(t)
	for _, want := range []string{`"action":"argowf.stop","args":{"name":"build-x7"},"risk":"destructive"`, `"action":"argowf.retry","args":{"name":"build-x7"},"risk":"write"`,
		`"targets":[{"kind":"argowf","id":"argowf:ci/ci/build-x7"}]`} {
		if !strings.Contains(audit, want) {
			t.Errorf("audit lacks %s", want)
		}
	}
}

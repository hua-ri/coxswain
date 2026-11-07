package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/hua-ri/coxswain/internal/client"
	"github.com/hua-ri/coxswain/internal/instance"
	"github.com/hua-ri/coxswain/internal/server"
)

var actions = []server.ActionInfo{
	{
		ID: "demo.echo", Summary: "echo arguments",
		Input: map[string]any{"type": "object", "properties": map[string]any{
			"name":  map[string]any{"type": "string"},
			"words": map[string]any{"type": "string"},
			"count": map[string]any{"type": "integer"},
			"tags":  map[string]any{"type": "array"},
		}},
		Positional: []string{"name", "words"},
		RestArgs:   true,
	},
	{
		ID: "demo.tag", Summary: "tag",
		Input: map[string]any{"type": "object", "properties": map[string]any{
			"host": map[string]any{"type": "string"}, "set": map[string]any{"type": "array"}}},
		Positional: []string{"host"},
	},
	{ID: "demo.run", Summary: "run", Input: map[string]any{"type": "object"}},
}

// echoer returns its arguments as the result; demo.run returns a
// printable result with exit code 7.
type echoer struct {
	err error
	out *client.Outcome
}

func (e echoer) Invoke(_ context.Context, id string, args json.RawMessage) (client.Outcome, error) {
	if e.err != nil {
		return client.Outcome{}, e.err
	}
	if e.out != nil {
		return *e.out, nil
	}
	if id == "demo.run" {
		return client.Outcome{Status: "ok", Result: json.RawMessage(`{"text":"hi\n","code":7}`)}, nil
	}
	return client.Outcome{Status: "ok", Result: args}, nil
}

type printable struct {
	Text string `json:"text"`
	Code int    `json:"code"`
}

func (p printable) PrintCLI(stdout, _ io.Writer) int {
	io.WriteString(stdout, p.Text)
	return p.Code
}

type run struct {
	code           int
	stdout, stderr string
}

func execute(t *testing.T, inv Invoker, args ...string) run {
	t.Helper()
	var out, errb bytes.Buffer
	app := &App{Actions: actions, Client: inv, Stdout: &out, Stderr: &errb, Stdin: strings.NewReader(""),
		Printers: map[string]Decoder{"demo.run": PrintAs[printable]}}
	root := NewRoot(app, "test")
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	return run{Code(err), out.String(), errb.String()}
}

func TestPositionalAndFlags(t *testing.T) {
	r := execute(t, echoer{}, "demo", "echo", "--count", "2", "--tags", "a", "--tags", "b", "web", "--", "df", "-h")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	var got map[string]any
	json.Unmarshal([]byte(r.stdout), &got)
	if got["name"] != "web" || got["words"] != "df -h" || got["count"] != float64(2) || len(got["tags"].([]any)) != 2 {
		t.Fatalf("got %v", got)
	}
}

func TestFlagsAfterPositionalWithoutRest(t *testing.T) {
	r := execute(t, echoer{}, "demo", "tag", "web", "--set", "env=prod")
	if r.code != 0 || !strings.Contains(r.stdout, `"host": "web"`) || !strings.Contains(r.stdout, "env=prod") {
		t.Fatalf("%+v", r)
	}
	if r := execute(t, echoer{}, "demo", "tag", "a", "b"); r.code != ExitFailure {
		t.Fatalf("extra positional must fail: %+v", r)
	}
}

func TestPrinter(t *testing.T) {
	if r := execute(t, echoer{}, "demo", "run"); r.code != 7 || r.stdout != "hi\n" {
		t.Fatalf("%+v", r)
	}
	r := execute(t, echoer{}, "-o", "json", "demo", "run")
	if r.code != 7 || !strings.Contains(r.stdout, `"status": "ok"`) || !strings.Contains(r.stdout, `"code": 7`) {
		t.Fatalf("%+v", r)
	}
}

func TestErrorOutcome(t *testing.T) {
	r := execute(t, echoer{out: &client.Outcome{Status: "error", Error: "boom", Hint: "try x"}}, "demo", "run")
	if r.code != ExitFailure || !strings.Contains(r.stderr, "error: boom") || !strings.Contains(r.stderr, "try x") {
		t.Fatalf("%+v", r)
	}
}

func TestNotRunning(t *testing.T) {
	r := execute(t, echoer{err: &instance.NotRunningError{Cause: fmt.Errorf("refused")}}, "demo", "run")
	if r.code != ExitNotRunning || r.stderr != instance.ErrNotRunning.Error()+"\n" {
		t.Fatalf("%+v", r)
	}
}

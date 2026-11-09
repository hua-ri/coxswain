package terminal

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/engine"
	"github.com/hua-ri/coxswain/internal/eventlog"
	"github.com/hua-ri/coxswain/internal/inventory"
	"github.com/hua-ri/coxswain/internal/shaper"
	"github.com/hua-ri/coxswain/internal/sshx"
	"github.com/hua-ri/coxswain/internal/sshx/sshtest"
	"github.com/hua-ri/coxswain/internal/term"
)

var (
	codex   = action.Actor{Kind: action.Agent, Name: "codex", Session: "s1"}
	kimi    = action.Actor{Kind: action.Agent, Name: "kimi"}
	desktop = action.Actor{Kind: action.Human, Name: "desktop"}
	cli     = action.Actor{Kind: action.Human, Name: "cli"}
)

func setup(t *testing.T) (*engine.Engine, *term.Manager) {
	t.Helper()
	srv := sshtest.Start(t)
	dir := t.TempDir()
	hosts := inventory.NewStore(filepath.Join(dir, "hosts.json"))
	hosts.Merge([]inventory.Host{{Name: "prod", HostName: srv.Host, Port: srv.Port, IdentityFiles: []string{srv.KeyFile}}})
	hosts.Update(func(m map[string]inventory.Host) error {
		h := m["prod"]
		h.Tags, h.Note = map[string]string{"env": "prod"}, "no restarts"
		m["prod"] = h
		return nil
	})
	events := eventlog.Open(filepath.Join(dir, "events"))
	terms := &term.Manager{Hosts: hosts, Dialer: &sshx.Dialer{Hosts: hosts, KnownHosts: []string{srv.KnownHosts}}, Events: events}
	t.Cleanup(terms.CloseAll)
	reg := action.NewRegistry()
	(&Provider{Hosts: hosts, Terms: terms, Artifacts: shaper.NewArtifacts(filepath.Join(dir, "artifacts"))}).Register(reg)
	return &engine.Engine{Registry: reg, Events: events}, terms
}

func invoke(t *testing.T, e *engine.Engine, who action.Actor, id string, args any) engine.Outcome {
	t.Helper()
	b, _ := json.Marshal(args)
	return e.Invoke(context.Background(), who, id, b)
}

func mustOK(t *testing.T, out engine.Outcome) any {
	t.Helper()
	if out.Status != engine.StatusOK {
		t.Fatalf("%+v", out)
	}
	return out.Result
}

// readUntil calls terminal.read until its text contains want.
func readUntil(t *testing.T, e *engine.Engine, who action.Actor, id, want string, lines int) ReadOutput {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		res := mustOK(t, invoke(t, e, who, "terminal.read", map[string]any{"id": id, "lines": lines})).(ReadOutput)
		if strings.Contains(res.Output.Text, want) {
			return res
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiting for %q: %+v", want, res)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAnnotations(t *testing.T) {
	e, _ := setup(t)
	cases := []struct {
		id   string
		want action.Annotations
	}{
		{"terminal.list", action.Annotations{ReadOnly: true, Idempotent: true}},
		{"terminal.open", action.Annotations{Destructive: true, OpenWorld: true}},
		{"terminal.send", action.Annotations{Destructive: true, OpenWorld: true}},
		{"terminal.read", action.Annotations{ReadOnly: true}},
		{"terminal.close", action.Annotations{Destructive: true}},
	}
	for _, c := range cases {
		a, ok := e.Registry.Get(c.id)
		if !ok || a.Annotations != c.want {
			t.Errorf("%s: %+v", c.id, a)
		}
	}
	send, _ := e.Registry.Get("terminal.send")
	if !strings.Contains(send.Description, "ssh_exec") || !strings.Contains(send.Description, "user") {
		t.Errorf("send description: %s", send.Description)
	}
}

func TestOpenSendRead(t *testing.T) {
	e, terms := setup(t)
	open := mustOK(t, invoke(t, e, codex, "terminal.open", map[string]any{"host": "prod"})).(OpenOutput)
	if !strings.HasPrefix(open.ID, "t_") || open.Host != "prod" || open.Owner != (action.Actor{Kind: action.Agent, Name: "codex"}) || open.HostNote != "no restarts" ||
		open.HostTags["env"] != "prod" || open.Cols != 80 {
		t.Fatalf("%+v", open)
	}
	if list := terms.List(); len(list) != 1 || list[0].ID != open.ID {
		t.Fatalf("%+v", list)
	}
	mustOK(t, invoke(t, e, codex, "terminal.send", map[string]any{"id": open.ID, "text": "echo hi there"}))
	res := readUntil(t, e, codex, open.ID, "hi there", 100)
	if res.Host != "prod" || res.Closed || res.Output.Truncated {
		t.Fatalf("%+v", res)
	}
	// Without enter, the text is typed but not run.
	mustOK(t, invoke(t, e, codex, "terminal.send", map[string]any{"id": open.ID, "text": "rm -rf /tmp/nothing-here", "enter": false}))

	var risks []string
	evs, _ := e.Events.Read(time.Now())
	for _, ev := range evs {
		if ev.Type == eventlog.ActionRequested && ev.Action == "terminal.send" {
			if len(ev.Targets) != 1 || ev.Targets[0].ID != "prod" || ev.Targets[0].Tags["env"] != "prod" {
				t.Fatalf("targets %+v", ev.Targets)
			}
			risks = append(risks, ev.Risk)
		}
	}
	if strings.Join(risks, ",") != "read,destructive" {
		t.Fatalf("risks %v", risks)
	}

	list := mustOK(t, invoke(t, e, kimi, "terminal.list", nil)).([]term.Session)
	if len(list) != 1 || list[0].Owner.String() != "agent:codex" {
		t.Fatalf("%+v", list)
	}
}

func TestSendAndReadErrors(t *testing.T) {
	e, _ := setup(t)
	for _, c := range []struct {
		id   string
		args map[string]any
	}{
		{"terminal.send", map[string]any{"id": "t_000000000000", "text": "ls"}},
		{"terminal.read", map[string]any{"id": "nope"}},
		{"terminal.close", map[string]any{"id": "nope"}},
		{"terminal.open", map[string]any{"host": "nope"}},
		{"terminal.read", map[string]any{"id": "x", "lines": -1}},
	} {
		if out := invoke(t, e, codex, c.id, c.args); out.Status != engine.StatusError {
			t.Errorf("%s %v: %+v", c.id, c.args, out)
		}
	}
}

func TestReadShapedForAgents(t *testing.T) {
	e, _ := setup(t)
	open := mustOK(t, invoke(t, e, codex, "terminal.open", map[string]any{"host": "prod"})).(OpenOutput)
	mustOK(t, invoke(t, e, codex, "terminal.send", map[string]any{"id": open.ID, "text": "seq 1 1000"}))
	agent := readUntil(t, e, codex, open.ID, "1000", 2000)
	if !agent.Output.Truncated || agent.Output.ArtifactID == "" || agent.Output.TotalLines != 1000 {
		t.Fatalf("agent output not shaped: %+v", agent.Output)
	}
	human := readUntil(t, e, cli, open.ID, "1000", 2000)
	if human.Output.Truncated || !strings.HasPrefix(human.Output.Text, "1\n2\n") {
		t.Fatalf("human output shaped: %+v", human.Output)
	}
	last := readUntil(t, e, codex, open.ID, "1000", 3)
	if last.Output.Text != "998\n999\n1000" {
		t.Fatalf("%q", last.Output.Text)
	}
}

// TestCloseOwnership: agents may close only terminals they opened; the
// user may close any.
func TestCloseOwnership(t *testing.T) {
	e, terms := setup(t)
	mine, _ := terms.OpenHost(context.Background(), "prod", desktop, 80, 24)
	codexs := mustOK(t, invoke(t, e, codex, "terminal.open", map[string]any{"host": "prod"})).(OpenOutput)

	if out := invoke(t, e, codex, "terminal.close", map[string]any{"id": mine.ID}); out.Status != engine.StatusError ||
		!strings.Contains(out.Error, "human:desktop") {
		t.Fatalf("agent closed the user's terminal: %+v", out)
	}
	if out := invoke(t, e, kimi, "terminal.close", map[string]any{"id": codexs.ID}); out.Status != engine.StatusError {
		t.Fatalf("kimi closed codex's terminal: %+v", out)
	}
	// Same agent, another MCP session.
	other := codex
	other.Session = "s2"
	mustOK(t, invoke(t, e, other, "terminal.close", map[string]any{"id": codexs.ID}))
	mustOK(t, invoke(t, e, cli, "terminal.close", map[string]any{"id": mine.ID}))
	if list := terms.List(); len(list) != 0 {
		t.Fatalf("%+v", list)
	}
}

// echoPods opens echoing pod shells.
type echoPods struct{}

func (echoPods) OpenPod(_ context.Context, spec term.PodSpec, _, _ int, out io.Writer) (term.Backend, term.PodSpec, error) {
	if spec.Namespace == "" {
		spec.Namespace = "shop"
	}
	spec.Container = "app"
	out.Write([]byte("$ "))
	return &echoShell{out: out, done: make(chan struct{})}, spec, nil
}

func (echoPods) Target(ctx, ns string) action.Target {
	if ns == "" {
		ns = "shop"
	}
	return action.Target{Kind: "k8s", ID: "k8s:" + ctx + "/" + ns, Tags: map[string]string{"env": "prod"}}
}

type echoShell struct {
	out  io.Writer
	once sync.Once
	done chan struct{}
}

func (s *echoShell) Write(p []byte) (int, error) { return s.out.Write(p) }
func (s *echoShell) Resize(int, int) error       { return nil }
func (s *echoShell) Wait() *int                  { <-s.done; return nil }
func (s *echoShell) Close() error                { s.once.Do(func() { close(s.done) }); return nil }

func TestPodTerminal(t *testing.T) {
	e, terms := setup(t)
	terms.Pods = echoPods{}
	res := mustOK(t, invoke(t, e, codex, "terminal.open", map[string]any{"pod": map[string]any{"context": "prod", "pod": "web-1"}})).(OpenOutput)
	if res.Kind != term.KindPod || res.Context != "prod" || res.Namespace != "shop" || res.Pod != "web-1" || res.Container != "app" ||
		res.Host != "" || res.Owner.Session != "" || res.ContextTags["env"] != "prod" {
		t.Fatalf("%+v", res)
	}
	mustOK(t, invoke(t, e, codex, "terminal.send", map[string]any{"id": res.ID, "text": "rm -rf /tmp/x"}))
	readUntil(t, e, kimi, res.ID, "$ rm -rf /tmp/x", 10)
	evs, _ := e.Events.Read(time.Now())
	risks := map[string]string{}
	for _, ev := range evs {
		if ev.Type == eventlog.ActionRequested && strings.HasPrefix(ev.Action, "terminal.") && ev.Action != "terminal.list" {
			if len(ev.Targets) != 1 || ev.Targets[0].ID != "k8s:prod/shop" || ev.Targets[0].Tags["env"] != "prod" {
				t.Fatalf("%s targets %+v", ev.Action, ev.Targets)
			}
			risks[ev.Action] = ev.Risk
		}
	}
	if risks["terminal.open"] != "write" || risks["terminal.send"] != "destructive" || risks["terminal.read"] != "read" {
		t.Fatalf("risks %v", risks)
	}
	if out := invoke(t, e, kimi, "terminal.close", map[string]any{"id": res.ID}); out.Status != engine.StatusError {
		t.Fatalf("kimi closed codex's terminal: %+v", out)
	}
	mustOK(t, invoke(t, e, codex, "terminal.close", map[string]any{"id": res.ID}))

	for _, bad := range []map[string]any{
		{},
		{"host": "prod", "pod": map[string]any{"context": "prod", "pod": "web-1"}},
		{"pod": map[string]any{"context": "prod"}},
	} {
		if out := invoke(t, e, codex, "terminal.open", bad); out.Status != engine.StatusError {
			t.Errorf("%v: %+v", bad, out)
		}
	}
}

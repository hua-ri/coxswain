package ssh

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/engine"
	"github.com/hua-ri/coxswain/internal/eventlog"
	"github.com/hua-ri/coxswain/internal/inventory"
	"github.com/hua-ri/coxswain/internal/policy"
	"github.com/hua-ri/coxswain/internal/shaper"
	"github.com/hua-ri/coxswain/internal/sshx"
	"github.com/hua-ri/coxswain/internal/sshx/sshtest"
)

// batchHost is a host for batch tests: its server, whether it has the
// file "ok" in its root (so `test -f ok` succeeds there) and its tags.
type batchHost struct {
	name string
	ok   bool
	tags map[string]string
}

type batchFixture struct {
	e       *engine.Engine
	p       *Provider
	servers map[string]*sshtest.Server
	events  <-chan eventlog.Event
}

// setupBatch gives every host its own test SSH server.
func setupBatch(t *testing.T, hs ...batchHost) batchFixture {
	t.Helper()
	dir := t.TempDir()
	hosts := inventory.NewStore(filepath.Join(dir, "hosts.json"))
	var known []string
	servers := map[string]*sshtest.Server{}
	var list []inventory.Host
	for _, h := range hs {
		srv := sshtest.Start(t)
		servers[h.name] = srv
		known = append(known, srv.KnownHosts)
		if h.ok {
			os.WriteFile(filepath.Join(srv.Root, "ok"), nil, 0o644)
		}
		list = append(list, inventory.Host{Name: h.name, HostName: srv.Host, Port: srv.Port, IdentityFiles: []string{srv.KeyFile}})
	}
	hosts.Merge(list)
	hosts.Update(func(m map[string]inventory.Host) error {
		for _, h := range hs {
			x := m[h.name]
			x.Tags = h.tags
			if h.name == "web-1" {
				x.Note = "canary host"
			}
			m[h.name] = x
		}
		return nil
	})
	reg := action.NewRegistry()
	p := &Provider{
		Hosts:     hosts,
		Dialer:    &sshx.Dialer{Hosts: hosts, KnownHosts: known},
		Artifacts: shaper.NewArtifacts(filepath.Join(dir, "artifacts")),
		UserHome:  dir,
	}
	p.Pool = &sshx.Pool{Dialer: p.Dialer}
	t.Cleanup(func() { p.Pool.Close() })
	events := eventlog.Open(filepath.Join(dir, "events"))
	p.Events = events
	p.Register(reg)
	ch, cancel := events.Subscribe()
	t.Cleanup(cancel)
	return batchFixture{e: &engine.Engine{Registry: reg, Events: events}, p: p, servers: servers, events: ch}
}

func batch(t *testing.T, f batchFixture, who action.Actor, args map[string]any) engine.Outcome {
	t.Helper()
	b, _ := json.Marshal(args)
	return f.e.Invoke(context.Background(), who, "ssh.batch", b)
}

func batchOK(t *testing.T, out engine.Outcome) BatchOutput {
	t.Helper()
	if out.Status != engine.StatusOK {
		t.Fatalf("%+v", out)
	}
	return out.Result.(BatchOutput)
}

func web(n int, ok bool) batchHost {
	return batchHost{name: fmt.Sprintf("web-%d", n), ok: ok, tags: map[string]string{"role": "web", "env": "prod"}}
}

// statuses is "host:status" for every result, in order.
func statuses(res BatchOutput) string {
	var s []string
	for _, r := range res.Results {
		s = append(s, r.Host+":"+r.Status)
	}
	return strings.Join(s, " ")
}

// collect returns the batch events until batch.finished.
func collect(t *testing.T, ch <-chan eventlog.Event) []eventlog.Event {
	t.Helper()
	var out []eventlog.Event
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev := <-ch:
			if strings.HasPrefix(ev.Type, "batch.") {
				out = append(out, ev)
				if ev.Type == eventlog.BatchFinished {
					return out
				}
			}
		case <-timeout:
			t.Fatalf("no batch.finished; got %+v", out)
		}
	}
}

func TestBatchTargetsAndEvents(t *testing.T) {
	f := setupBatch(t, batchHost{name: "db", ok: true}, web(1, true), web(2, true), batchHost{name: "other", ok: true})
	res := batchOK(t, batch(t, f, agent, map[string]any{"hosts": []string{"db", "web-2"}, "tag": "role=web", "command": "echo hi", "concurrency": 1}))
	if statuses(res) != "db:ok web-2:ok web-1:ok" {
		t.Fatalf("targets: %s", statuses(res))
	}
	if res.Summary != (BatchSummary{Total: 3, OK: 3}) || res.Command != "echo hi" || !strings.HasPrefix(res.ID, "b_") {
		t.Fatalf("%+v", res)
	}
	r := res.Results[0]
	if r.Stdout != "hi\n" || r.ExitCode == nil || *r.ExitCode != 0 || r.Error != "" {
		t.Fatalf("%+v", r)
	}
	if res.HostNotes["web-1"] != "canary host" || res.ArtifactID == "" {
		t.Fatalf("%+v", res)
	}
	full, err := f.p.Artifacts.Read(res.ArtifactID, "^=== ", 0, 0)
	if err != nil || strings.Count(full, "===") != 6 || !strings.Contains(full, "=== db: exit 0") {
		t.Fatalf("artifact: %q %v", full, err)
	}

	evs := collect(t, f.events)
	if len(evs) != 5 || evs[0].Type != eventlog.BatchStarted || len(evs[0].Targets) != 3 || evs[0].Batch != res.ID || evs[0].Actor.Name != "codex" {
		t.Fatalf("events: %+v", evs)
	}
	for i, h := range []string{"db", "web-2", "web-1"} {
		ev := evs[1+i]
		var hr HostResult
		json.Unmarshal(ev.Detail, &hr)
		if ev.Type != eventlog.BatchHostDone || hr.Host != h || hr.Status != "ok" || ev.Targets[0].ID != h || hr.Stdout != "" {
			t.Fatalf("host_done %d: %+v %+v", i, ev, hr)
		}
	}
	var fin struct {
		Summary    BatchSummary `json:"summary"`
		ArtifactID string       `json:"artifact_id"`
	}
	json.Unmarshal(evs[4].Detail, &fin)
	if fin.Summary.OK != 3 || fin.ArtifactID != res.ArtifactID {
		t.Fatalf("finished: %s", evs[4].Detail)
	}

	logged, _ := f.e.Events.Read(time.Now())
	if logged[0].Type != eventlog.ActionRequested || logged[0].Risk != "read" || len(logged[0].Targets) != 3 || logged[0].Targets[2].Tags["env"] != "prod" {
		t.Fatalf("requested: %+v", logged[0])
	}
}

func TestBatchCanary(t *testing.T) {
	f := setupBatch(t, web(1, false), web(2, true), web(3, true))
	res := batchOK(t, batch(t, f, agent, map[string]any{"tag": "role=web", "command": "test -f ok"}))
	if statuses(res) != "web-1:failed web-2:skipped web-3:skipped" || res.Summary != (BatchSummary{Total: 3, Failed: 1, Skipped: 2}) ||
		!strings.Contains(res.Stopped, "canary") {
		t.Fatalf("%s %+v", statuses(res), res)
	}
	if *res.Results[0].ExitCode != 1 {
		t.Fatalf("%+v", res.Results[0])
	}
	// Without a canary, every host runs (stop_on_failure only stops
	// dispatching after a failure; with one at a time, the rest).
	res = batchOK(t, batch(t, f, agent, map[string]any{"tag": "role=web", "command": "test -f ok", "canary": false, "stop_on_failure": false}))
	if statuses(res) != "web-1:failed web-2:ok web-3:ok" {
		t.Fatalf("%s", statuses(res))
	}
}

func TestBatchStopOnFailure(t *testing.T) {
	f := setupBatch(t, web(1, true), web(2, true), web(3, false), web(4, true))
	res := batchOK(t, batch(t, f, agent, map[string]any{"tag": "role=web", "command": "test -f ok", "concurrency": 1}))
	if statuses(res) != "web-1:ok web-2:ok web-3:failed web-4:skipped" || !strings.Contains(res.Stopped, "web-3") {
		t.Fatalf("%s %q", statuses(res), res.Stopped)
	}
	res = batchOK(t, batch(t, f, agent, map[string]any{"tag": "role=web", "command": "test -f ok", "concurrency": 1, "stop_on_failure": false}))
	if statuses(res) != "web-1:ok web-2:ok web-3:failed web-4:ok" || res.Stopped != "" {
		t.Fatalf("%s", statuses(res))
	}
}

func TestBatchConnectionFailure(t *testing.T) {
	f := setupBatch(t, web(1, true), web(2, true))
	f.p.Hosts.Update(func(m map[string]inventory.Host) error {
		h := m["web-2"]
		h.Port = 1 // nothing listens
		m["web-2"] = h
		return nil
	})
	res := batchOK(t, batch(t, f, agent, map[string]any{"tag": "role=web", "command": "true"}))
	if statuses(res) != "web-1:ok web-2:failed" || res.Results[1].Error == "" || res.Results[1].ExitCode != nil {
		t.Fatalf("%+v", res.Results)
	}
}

func TestBatchShapesForAgents(t *testing.T) {
	f := setupBatch(t, web(1, true), web(2, true))
	res := batchOK(t, batch(t, f, agent, map[string]any{"tag": "role=web", "command": "seq 1 100; seq 1 50 >&2"}))
	r := res.Results[0]
	if !r.Truncated || strings.Count(r.Stdout, "\n") > 20 || !strings.HasPrefix(r.Stdout, "1\n") || !strings.HasSuffix(r.Stdout, "100") ||
		strings.Count(r.Stderr, "\n") > 20 {
		t.Fatalf("%+v", r)
	}
	if got, _ := f.p.Artifacts.Read(res.ArtifactID, "^77$", 0, 0); !strings.HasSuffix(got, ": 77") || strings.Count(got, "\n") != 1 {
		t.Fatalf("artifact lacks full output: %q", got)
	}
	hres := batchOK(t, batch(t, f, human, map[string]any{"tag": "role=web", "command": "seq 1 100"}))
	if hres.Results[0].Truncated || strings.Count(hres.Results[0].Stdout, "\n") != 100 {
		t.Fatal("human output must not be shaped")
	}
}

func TestBatchErrors(t *testing.T) {
	f := setupBatch(t, web(1, true))
	for _, c := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"command": "true"}, "no hosts"},
		{map[string]any{"tag": "role=db", "command": "true"}, "no hosts"},
		{map[string]any{"hosts": []string{"nope"}, "command": "true"}, "nope"},
		{map[string]any{"tag": "role", "command": "true"}, "key=value"},
		{map[string]any{"hosts": []string{"web-1"}, "command": " "}, "command"},
		{map[string]any{"hosts": []string{"web-1"}, "command": "true", "concurrency": 51}, "concurrency"},
	} {
		if out := batch(t, f, agent, c.args); out.Status != engine.StatusError || !strings.Contains(out.Error, c.want) {
			t.Errorf("%v: %+v", c.args, out)
		}
	}
}

// TestBatchConfirmCount: more than 50 hosts need confirm_count equal to
// the number of hosts.
func TestBatchConfirmCount(t *testing.T) {
	dir := t.TempDir()
	hosts := inventory.NewStore(filepath.Join(dir, "hosts.json"))
	var list []inventory.Host
	for i := range 51 {
		list = append(list, inventory.Host{Name: fmt.Sprintf("h%02d", i), HostName: "127.0.0.1", Tags: map[string]string{"fleet": "all"}})
	}
	hosts.Merge(list)
	hosts.Update(func(m map[string]inventory.Host) error {
		for k, h := range m {
			h.Tags = map[string]string{"fleet": "all"}
			m[k] = h
		}
		return nil
	})
	p := &Provider{Hosts: hosts}
	a := p.batch()
	label := func(args map[string]any) (action.Assessment, error) {
		b, _ := json.Marshal(args)
		return a.Label(context.Background(), b)
	}
	if _, err := label(map[string]any{"tag": "fleet=all", "command": "uptime"}); err == nil || !strings.Contains(err.Error(), "confirm_count=51") {
		t.Fatalf("%v", err)
	}
	if _, err := label(map[string]any{"tag": "fleet=all", "command": "uptime", "confirm_count": 50}); err == nil {
		t.Fatal("wrong confirm_count accepted")
	}
	as, err := label(map[string]any{"tag": "fleet=all", "command": "uptime", "confirm_count": 51})
	if err != nil || len(as.Targets) != 51 || as.Risk != action.Read {
		t.Fatalf("%+v %v", as, err)
	}
	cmd := "systemctl restart nginx"
	if as, _ := label(map[string]any{"tag": "fleet=all", "command": cmd, "confirm_count": 51}); as.Risk != policy.ClassifyCommand(cmd) {
		t.Fatalf("risk %v", as.Risk)
	}
}

// TestBatchRevoked: revoking the agent stops its batch: running commands
// are killed and no more hosts are started.
func TestBatchRevoked(t *testing.T) {
	f := setupBatch(t, web(1, true), web(2, true), web(3, true))
	done := make(chan engine.Outcome, 1)
	go func() {
		done <- batch(t, f, agent, map[string]any{"tag": "role=web", "command": "sleep 30", "canary": false, "concurrency": 1})
	}()
	if c := <-f.servers["web-1"].Commands; c != "sleep 30" {
		t.Fatalf("ran %q", c)
	}
	f.e.CancelClient("codex", engine.ErrClientRevoked)
	out := <-done
	if out.Status != engine.StatusError || out.Error != "client revoked" {
		t.Fatalf("%+v", out)
	}
	for _, h := range []string{"web-2", "web-3"} {
		select {
		case c := <-f.servers[h].Commands:
			t.Fatalf("%s ran %q after the revoke", h, c)
		default:
		}
	}
	evs := collect(t, f.events)
	if fin := evs[len(evs)-1]; fin.Error != "client revoked" {
		t.Fatalf("%+v", fin)
	}
}

// TestBatchRunsTheLabelledHosts: the hosts are resolved once, when the
// call is labelled (and audited); a tag added in between does not widen
// the run.
func TestBatchRunsTheLabelledHosts(t *testing.T) {
	f := setupBatch(t, web(1, true), web(2, true), batchHost{name: "db", ok: true})
	a, _ := f.e.Registry.Get("ssh.batch")
	label := a.Label
	a.Label = func(ctx context.Context, raw json.RawMessage) (action.Assessment, error) {
		as, err := label(ctx, raw)
		f.p.Hosts.Update(func(m map[string]inventory.Host) error {
			h := m["db"]
			h.Tags = map[string]string{"role": "web"}
			m["db"] = h
			return nil
		})
		return as, err
	}
	res := batchOK(t, batch(t, f, agent, map[string]any{"tag": "role=web", "command": "true"}))
	if statuses(res) != "web-1:ok web-2:ok" {
		t.Fatalf("%s", statuses(res))
	}
}

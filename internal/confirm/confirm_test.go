package confirm

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/approval"
	"github.com/hua-ri/coxswain/internal/engine"
)

var ap = approval.Approval{
	ID: "ap_0123456789ab", ActionID: "ssh.exec", Risk: "destructive",
	Actor:   action.Actor{Kind: action.Agent, Name: "codex"},
	Preview: `prod$ echo "hi" \ bye`,
}

func TestDialogScriptEscapes(t *testing.T) {
	s := DialogScript(ap, 45*time.Second)
	if !strings.Contains(s, `echo \"hi\" \\ bye`) || !strings.Contains(s, "giving up after 45") {
		t.Fatalf("script: %s", s)
	}
}

func TestParseDialog(t *testing.T) {
	cases := []struct {
		out  string
		err  error
		want engine.Verdict
	}{
		{"button returned:Approve, gave up:false\n", nil, engine.Approved},
		{"button returned:, gave up:true\n", nil, engine.Undecided},
		{"", &exec.ExitError{Stderr: []byte("execution error: User canceled. (-128)")}, engine.Denied},
		{"", errors.New("exec: osascript not found"), engine.Undecided},
	}
	for i, c := range cases {
		if got := ParseDialog(c.out, c.err); got != c.want {
			t.Errorf("case %d: got %v want %v", i, got, c.want)
		}
	}
}

func TestDialogUsesRunner(t *testing.T) {
	var script string
	c := dialog(time.Second, func(_ context.Context, s string) (string, error) {
		script = s
		return "button returned:Approve, gave up:false", nil
	})
	if c(context.Background(), ap) != engine.Approved || !strings.Contains(script, "ap_0123456789ab") {
		t.Fatal("dialog runner not used")
	}
}

func TestTerminal(t *testing.T) {
	var out bytes.Buffer
	if Terminal(strings.NewReader("y\n"), &out)(context.Background(), ap) != engine.Approved {
		t.Fatal("y must approve")
	}
	if Terminal(strings.NewReader("\n"), &out)(context.Background(), ap) != engine.Denied {
		t.Fatal("empty must deny")
	}
	if !strings.Contains(out.String(), "Approve? [y/N]") {
		t.Fatalf("prompt: %q", out.String())
	}
}

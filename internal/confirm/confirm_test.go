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
	v, by := c(context.Background(), ap)
	if v != engine.Approved || !strings.Contains(script, "ap_0123456789ab") {
		t.Fatal("dialog runner not used")
	}
	if by.Kind != action.Human || by.Session != "dialog" || by.Name == "" {
		t.Fatalf("approver: %+v", by)
	}
}

func TestTerminal(t *testing.T) {
	var out bytes.Buffer
	v, by := Terminal(strings.NewReader("y\n"), &out)(context.Background(), ap)
	if v != engine.Approved || by.Kind != action.Human || by.Session != "terminal" {
		t.Fatalf("y must approve: %v %+v", v, by)
	}
	if v, _ := Terminal(strings.NewReader("\n"), &out)(context.Background(), ap); v != engine.Denied {
		t.Fatal("empty must deny")
	}
	if !strings.Contains(out.String(), "Approve? [y/N]") {
		t.Fatalf("prompt: %q", out.String())
	}
}

func TestSanitize(t *testing.T) {
	cases := map[string]string{
		"rm -rf /data #\rls -la":  `rm -rf /data #\x0dls -la`,
		"\x1b[2Kls":               `\x1b[2Kls`,
		"a\nb\tc\x7f":             `a\x0ab\x09c\x7f`,
		"x\u0085y":                `x\u0085y`,
		"evil\u202etxt.exe\u2066": `evil\u202etxt.exe\u2066`,
		"中文 ok":                   "中文 ok",
		"bad\xffbyte":             `bad\xffbyte`,
	}
	for in, want := range cases {
		if got := Sanitize(in); got != want {
			t.Errorf("Sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMessageSanitized(t *testing.T) {
	a := ap
	a.Preview = "prod$ rm -rf /data #\r\x1b[2Kls -la"
	m := Message(a)
	if strings.ContainsAny(m, "\r\x1b") || !strings.Contains(m, `\x0d\x1b[2Kls`) {
		t.Fatalf("message: %q", m)
	}
}

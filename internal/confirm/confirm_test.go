package confirm

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestScriptEscapes(t *testing.T) {
	s := Script("Coxswain", `codex wants "x" \ y`+"\r\x1b[2K", 60*time.Second)
	for _, want := range []string{`codex wants \"x\" \\ y\\x0d\\x1b[2K`, `with title "Coxswain"`, "giving up after 60"} {
		if !strings.Contains(s, want) {
			t.Fatalf("script missing %q: %s", want, s)
		}
	}
	if strings.ContainsAny(s, "\r\x1b") {
		t.Fatalf("control characters reached the dialog: %q", s)
	}
}

func TestParse(t *testing.T) {
	cases := []struct {
		out     string
		err     error
		want    bool
		wantErr bool
	}{
		{"button returned:Allow, gave up:false\n", nil, true, false},
		{"button returned:, gave up:true\n", nil, false, false},
		{"", &exec.ExitError{Stderr: []byte("execution error: User canceled. (-128)")}, false, false},
		{"", errors.New("exec: osascript not found"), false, true},
	}
	for i, c := range cases {
		got, err := Parse(c.out, c.err)
		if got != c.want || (err != nil) != c.wantErr {
			t.Errorf("case %d: got %v %v", i, got, err)
		}
	}
}

func TestAskWithUsesRunner(t *testing.T) {
	var script string
	ok, err := AskWith(context.Background(), func(_ context.Context, s string) (string, error) {
		script = s
		return "button returned:Allow, gave up:false", nil
	}, "Coxswain", "codex wants to use Coxswain", time.Second)
	if !ok || err != nil || !strings.Contains(script, "codex wants to use Coxswain") {
		t.Fatalf("ok=%v err=%v script=%s", ok, err, script)
	}
}

func TestAskDisabled(t *testing.T) {
	t.Setenv("COX_NO_DIALOG", "1")
	if ok, err := Ask(context.Background(), "t", "m", time.Second); ok || err != nil {
		t.Fatalf("COX_NO_DIALOG must deny without a dialog: %v %v", ok, err)
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

// Return must not answer the dialog: the user has to click a button.
func TestScriptHasNoDefaultButton(t *testing.T) {
	if s := Script("Coxswain", "x", time.Second); strings.Contains(s, "default button") {
		t.Fatalf("dialog has a default button: %s", s)
	}
}

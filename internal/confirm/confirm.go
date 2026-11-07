// Package confirm asks a human to approve a pending action, either with a
// native macOS dialog or on the terminal.
package confirm

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/approval"
	"github.com/hua-ri/coxswain/internal/engine"
)

// Message is the text shown to the human for an approval. Fields come
// from the agent, so they are sanitised.
func Message(ap approval.Approval) string {
	return fmt.Sprintf("%s wants to run %s (%s risk):\n\n%s\n\nApproval id: %s",
		Sanitize(ap.Actor.String()), Sanitize(ap.ActionID), Sanitize(ap.Risk), Sanitize(ap.Preview), Sanitize(ap.ID))
}

// Sanitize escapes characters that could hide or rewrite what a human
// sees: C0/C1 control characters, DEL, Unicode bidi controls and invalid
// UTF-8 (as \xNN or \uNNNN).
func Sanitize(s string) string {
	var sb strings.Builder
	for i, w := 0, 0; i < len(s); i += w {
		r, size := utf8.DecodeRuneInString(s[i:])
		w = size
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&sb, `\x%02x`, s[i])
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&sb, `\x%02x`, r)
		case (r >= 0x80 && r <= 0x9f) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069):
			fmt.Fprintf(&sb, `\u%04x`, r)
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// human is the person at this Mac, as recorded for a decision.
func human(session string) action.Actor {
	u := os.Getenv("USER")
	if u == "" {
		u = "user"
	}
	return action.Actor{Kind: action.Human, Name: u, Session: session}
}

// Dialog shows a native dialog via osascript. Nobody answering within
// timeout yields Undecided, so the agent can fall back to `cox approve`.
func Dialog(timeout time.Duration) engine.Confirmer {
	return dialog(timeout, func(ctx context.Context, script string) (string, error) {
		out, err := exec.CommandContext(ctx, "osascript", "-e", script).Output()
		return string(out), err
	})
}

func dialog(timeout time.Duration, run func(context.Context, string) (string, error)) engine.Confirmer {
	return func(ctx context.Context, ap approval.Approval) (engine.Verdict, action.Actor) {
		ctx, cancel := context.WithTimeout(ctx, timeout+5*time.Second)
		defer cancel()
		out, err := run(ctx, DialogScript(ap, timeout))
		return ParseDialog(out, err), human("dialog")
	}
}

// DialogScript builds the AppleScript for an approval dialog.
func DialogScript(ap approval.Approval, timeout time.Duration) string {
	return fmt.Sprintf(`display dialog %s with title "Coxswain approval" buttons {"Deny", "Approve"} `+
		`default button "Deny" cancel button "Deny" giving up after %d with icon caution`,
		quote(Message(ap)), int(timeout.Seconds()))
}

// ParseDialog turns osascript's result into a verdict. The cancel button
// ("Deny") makes osascript exit non-zero.
func ParseDialog(out string, err error) engine.Verdict {
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return engine.Denied // "Deny" is the cancel button
		}
		return engine.Undecided // osascript unavailable or killed
	}
	switch {
	case strings.Contains(out, "gave up:true"):
		return engine.Undecided
	case strings.Contains(out, "button returned:Approve"):
		return engine.Approved
	}
	return engine.Denied
}

func quote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

// Terminal asks on a terminal. Only an explicit "y" or "yes" approves.
func Terminal(in io.Reader, out io.Writer) engine.Confirmer {
	return func(_ context.Context, ap approval.Approval) (engine.Verdict, action.Actor) {
		fmt.Fprintf(out, "%s\n\nApprove? [y/N] ", Message(ap))
		line, _ := bufio.NewReader(in).ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			return engine.Approved, human("terminal")
		}
		return engine.Denied, human("terminal")
	}
}

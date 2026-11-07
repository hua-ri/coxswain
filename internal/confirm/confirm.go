// Package confirm asks a human to approve a pending action, either with a
// native macOS dialog or on the terminal.
package confirm

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/hua-ri/coxswain/internal/approval"
	"github.com/hua-ri/coxswain/internal/engine"
)

// Message is the text shown to the human for an approval.
func Message(ap approval.Approval) string {
	return fmt.Sprintf("%s wants to run %s (%s risk):\n\n%s\n\nApproval id: %s",
		ap.Actor, ap.ActionID, ap.Risk, ap.Preview, ap.ID)
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
	return func(ctx context.Context, ap approval.Approval) engine.Verdict {
		ctx, cancel := context.WithTimeout(ctx, timeout+5*time.Second)
		defer cancel()
		out, err := run(ctx, DialogScript(ap, timeout))
		return ParseDialog(out, err)
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
	return func(_ context.Context, ap approval.Approval) engine.Verdict {
		fmt.Fprintf(out, "%s\n\nApprove? [y/N] ", Message(ap))
		line, _ := bufio.NewReader(in).ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			return engine.Approved
		}
		return engine.Denied
	}
}

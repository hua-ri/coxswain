// Package confirm asks the person at this Mac a yes/no question with a
// native macOS dialog.
package confirm

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"
)

// Runner runs an AppleScript and returns osascript's output. Tests inject
// a fake so no real dialog appears.
type Runner func(ctx context.Context, script string) (string, error)

// Osascript runs a script with /usr/bin/osascript.
func Osascript(ctx context.Context, script string) (string, error) {
	out, err := exec.CommandContext(ctx, "osascript", "-e", script).Output()
	return string(out), err
}

// Ask shows a dialog with Allow and Deny buttons. Deny, closing the
// dialog or nobody answering within timeout all yield false. With
// COX_NO_DIALOG set (tests, CI) it answers false without showing anything.
func Ask(ctx context.Context, title, message string, timeout time.Duration) (bool, error) {
	if os.Getenv("COX_NO_DIALOG") != "" {
		return false, nil
	}
	return AskWith(ctx, Osascript, title, message, timeout)
}

// AskWith is Ask with an explicit runner.
func AskWith(ctx context.Context, run Runner, title, message string, timeout time.Duration) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout+5*time.Second)
	defer cancel()
	return Parse(run(ctx, Script(title, message, timeout)))
}

// Script builds the AppleScript for a dialog. title and message are
// sanitised and quoted.
func Script(title, message string, timeout time.Duration) string {
	return fmt.Sprintf(`display dialog %s with title %s buttons {"Deny", "Allow"} `+
		`cancel button "Deny" giving up after %d with icon caution`,
		quote(Sanitize(message)), quote(Sanitize(title)), int(timeout.Seconds()))
}

// Parse turns osascript's result into an answer. The cancel button
// ("Deny") makes osascript exit non-zero; any other failure (osascript
// missing or killed) is an error.
func Parse(out string, err error) (bool, error) {
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return false, nil
		}
		return false, err
	}
	return strings.Contains(out, "button returned:Allow") && !strings.Contains(out, "gave up:true"), nil
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

func quote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

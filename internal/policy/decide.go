package policy

import (
	"strings"

	"github.com/hua-ri/coxswain/internal/action"
)

// Mode is how much an agent may do on a target without asking.
type Mode string

const (
	Always   Mode = "always"   // every agent write is confirmed
	Balanced Mode = "balanced" // only destructive agent actions are confirmed
	Auto     Mode = "auto"     // nothing is confirmed (sandboxes only)
)

var strictness = map[Mode]int{Auto: 0, Balanced: 1, Always: 2}

// Decision is the outcome of a policy check.
type Decision string

const (
	Allow Decision = "allow"
	Ask   Decision = "ask"
)

// ModeFor returns the strictest mode among the targets. A target's mode
// is its ai_policy tag if set, else Always for env=prod (or production),
// else Balanced. Tag values are compared case-insensitively.
func ModeFor(targets []action.Target) Mode {
	if len(targets) == 0 {
		return Balanced
	}
	mode := Auto
	for _, t := range targets {
		if m := targetMode(t); strictness[m] > strictness[mode] {
			mode = m
		}
	}
	return mode
}

func targetMode(t action.Target) Mode {
	if m := Mode(strings.ToLower(t.Tags["ai_policy"])); m == Always || m == Balanced || m == Auto {
		return m
	}
	if env := strings.ToLower(t.Tags["env"]); env == "prod" || env == "production" {
		return Always
	}
	return Balanced
}

// Decide applies the policy:
//   - reads always run;
//   - a human's writes run, a human's destructive actions are confirmed;
//   - an agent's writes follow the target mode.
func Decide(actor action.Actor, as action.Assessment) Decision {
	if as.Risk == action.Read {
		return Allow
	}
	if actor.Kind == action.Human {
		if as.Risk == action.Destructive {
			return Ask
		}
		return Allow
	}
	switch ModeFor(as.Targets) {
	case Auto:
		return Allow
	case Balanced:
		if as.Risk == action.Destructive {
			return Ask
		}
		return Allow
	default:
		return Ask
	}
}

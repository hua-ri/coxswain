package main

import (
	"testing"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/eventlog"
)

func TestEventLine(t *testing.T) {
	at := time.Date(2026, 11, 7, 9, 5, 7, 0, time.Local)
	kimi := &action.Actor{Kind: action.Agent, Name: "kimi"}
	cases := []struct {
		ev   eventlog.Event
		want string
	}{
		{eventlog.Event{Time: at, Type: eventlog.ActionRequested, Actor: kimi, Action: "ssh.exec", Risk: "read",
			Targets: []action.Target{{ID: "dev"}}, Args: []byte(`{"command":"df"}`)},
			`09:05:07 agent:kimi ssh.exec requested read dev {"command":"df"}`},
		{eventlog.Event{Time: at, Type: eventlog.ActionFinished, Actor: kimi, Action: "ssh.exec", DurationMs: 12},
			"09:05:07 agent:kimi ssh.exec finished 12ms"},
		{eventlog.Event{Time: at, Type: eventlog.ActionFailed, Actor: kimi, Action: "ssh.exec", DurationMs: 3, Error: "boom\r"},
			`09:05:07 agent:kimi ssh.exec failed 3ms: boom\x0d`},
		{eventlog.Event{Time: at, Type: eventlog.ActionRefused, Actor: kimi, Action: "ssh.exec", Reason: "revoked"},
			"09:05:07 agent:kimi ssh.exec refused: revoked"},
		{eventlog.Event{Time: at, Type: eventlog.ClientRevoked, Client: "codex"},
			"09:05:07 client codex revoked"},
	}
	for _, c := range cases {
		if got := eventLine(c.ev); got != c.want {
			t.Errorf("got  %q\nwant %q", got, c.want)
		}
	}
}

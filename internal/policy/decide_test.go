package policy

import (
	"testing"

	"github.com/hua-ri/coxswain/internal/action"
)

var (
	human = action.Actor{Kind: action.Human, Name: "king"}
	agent = action.Actor{Kind: action.Agent, Name: "codex"}
	dev   = action.Target{Kind: "host", ID: "dev-1", Tags: map[string]string{"env": "dev"}}
	prod  = action.Target{Kind: "host", ID: "prod-1", Tags: map[string]string{"env": "prod"}}
	sbx   = action.Target{Kind: "host", ID: "sbx", Tags: map[string]string{"ai_policy": "auto"}}
)

func TestModeFor(t *testing.T) {
	cases := []struct {
		targets []action.Target
		want    Mode
	}{
		{nil, Balanced},
		{[]action.Target{dev}, Balanced},
		{[]action.Target{prod}, Always},
		{[]action.Target{sbx}, Auto},
		{[]action.Target{sbx, prod}, Always},
		{[]action.Target{{Tags: map[string]string{"env": "prod", "ai_policy": "balanced"}}}, Balanced},
		{[]action.Target{{Tags: map[string]string{"ai_policy": "bogus"}}}, Balanced},
	}
	for i, c := range cases {
		if got := ModeFor(c.targets); got != c.want {
			t.Errorf("case %d: got %s, want %s", i, got, c.want)
		}
	}
}

func TestDecide(t *testing.T) {
	cases := []struct {
		name   string
		actor  action.Actor
		risk   action.Risk
		target action.Target
		want   Decision
	}{
		{"agent read prod", agent, action.Read, prod, Allow},
		{"agent write dev", agent, action.Write, dev, Allow},
		{"agent destructive dev", agent, action.Destructive, dev, Ask},
		{"agent write prod", agent, action.Write, prod, Ask},
		{"agent destructive sandbox", agent, action.Destructive, sbx, Allow},
		{"human write prod", human, action.Write, prod, Allow},
		{"human destructive dev", human, action.Destructive, dev, Ask},
	}
	for _, c := range cases {
		as := action.Assessment{Risk: c.risk, Targets: []action.Target{c.target}}
		if got := Decide(c.actor, as); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestModeForCaseInsensitive(t *testing.T) {
	cases := []struct {
		tags map[string]string
		want Mode
	}{
		{map[string]string{"env": "PROD"}, Always},
		{map[string]string{"env": "production"}, Always},
		{map[string]string{"env": "Production"}, Always},
		{map[string]string{"ai_policy": "AUTO"}, Auto},
		{map[string]string{"env": "Prod", "ai_policy": "Balanced"}, Balanced},
	}
	for _, c := range cases {
		if got := ModeFor([]action.Target{{Tags: c.tags}}); got != c.want {
			t.Errorf("%v: got %s, want %s", c.tags, got, c.want)
		}
	}
}

package action

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func testAction(id string) *Action {
	return &Action{
		ID:    id,
		Input: map[string]any{"type": "object"},
		Label: func(context.Context, json.RawMessage) (Assessment, error) {
			return Assessment{Risk: Read}, nil
		},
		Run: func(context.Context, json.RawMessage) (any, error) { return "ok", nil },
	}
}

func TestRegistryListSorted(t *testing.T) {
	r := NewRegistry()
	r.Register(testAction("ssh.exec"))
	r.Register(testAction("artifact.read"))
	got := r.List()
	if len(got) != 2 || got[0].ID != "artifact.read" || got[1].ID != "ssh.exec" {
		t.Fatalf("unexpected order: %v, %v", got[0].ID, got[1].ID)
	}
	if _, ok := r.Get("ssh.exec"); !ok {
		t.Fatal("Get(ssh.exec) not found")
	}
}

func TestRegistryRejectsBadActions(t *testing.T) {
	cases := map[string]*Action{
		"bad id":    testAction("Exec"),
		"no schema": func() *Action { a := testAction("a.b"); a.Input = nil; return a }(),
		"no run":    func() *Action { a := testAction("a.b"); a.Run = nil; return a }(),
		"no label":  func() *Action { a := testAction("a.b"); a.Label = nil; return a }(),
	}
	for name, a := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic")
				}
			}()
			NewRegistry().Register(a)
		})
	}
}

func TestRegistryRejectsDuplicate(t *testing.T) {
	r := NewRegistry()
	r.Register(testAction("a.b"))
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	r.Register(testAction("a.b"))
}

func TestRiskText(t *testing.T) {
	b, _ := json.Marshal(map[string]Risk{"r": Destructive})
	if string(b) != `{"r":"destructive"}` {
		t.Fatalf("got %s", b)
	}
}

func TestAnnotationsJSON(t *testing.T) {
	b, _ := json.Marshal(Annotations{ReadOnly: true, Idempotent: true})
	if string(b) != `{"read_only":true,"destructive":false,"idempotent":true,"open_world":false}` {
		t.Fatalf("got %s", b)
	}
}

func TestExactKeys(t *testing.T) {
	input := map[string]any{"type": "object", "properties": map[string]any{"content": map[string]any{}, "host": map[string]any{}}}
	if err := ExactKeys(json.RawMessage(`{"host":"a","content":"x"}`), input); err != nil {
		t.Fatal(err)
	}
	if err := ExactKeys(nil, input); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{"Content":"x"}`, `{"host":"a","extra":1}`, `[1]`} {
		if err := ExactKeys(json.RawMessage(raw), input); err == nil {
			t.Errorf("%s accepted", raw)
		}
	}
	if err := ExactKeys(json.RawMessage(`{"CONTENT":"x"}`), input); err == nil || !strings.Contains(err.Error(), `"content"`) {
		t.Fatalf("no hint: %v", err)
	}
}

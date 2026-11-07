// Package artifact exposes stored oversized outputs as an Action.
package artifact

import (
	"context"
	"encoding/json"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/shaper"
)

func Register(r *action.Registry, store *shaper.Artifacts) {
	r.Register(&action.Action{
		ID:      "artifact.read",
		Summary: "Read part of a stored large output",
		Description: "Read lines of an artifact that a previous tool call stored because its output was too large. " +
			"Use grep (a regular expression) and/or start/end line numbers to fetch only what you need. " +
			"Returns at most 500 numbered lines.",
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id":    map[string]any{"type": "string", "description": "artifact id, e.g. art_0123456789ab"},
				"grep":  map[string]any{"type": "string", "description": "only lines matching this regular expression"},
				"start": map[string]any{"type": "integer", "description": "first line (1-based)"},
				"end":   map[string]any{"type": "integer", "description": "last line (inclusive)"},
			},
			"required": []string{"id"},
		},
		Positional:  []string{"id"},
		Annotations: action.Annotations{ReadOnly: true, Idempotent: true},
		Label: func(context.Context, json.RawMessage) (action.Assessment, error) {
			return action.Assessment{Risk: action.Read}, nil
		},
		Run: func(_ context.Context, raw json.RawMessage) (any, error) {
			var in struct {
				ID, Grep   string
				Start, End int
			}
			if err := action.Decode(raw, &in); err != nil {
				return nil, err
			}
			return store.Read(in.ID, in.Grep, in.Start, in.End)
		},
	})
}

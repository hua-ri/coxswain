// Package mcp exposes every Action as an MCP tool (ssh.exec → ssh_exec),
// plus cox_approval_wait for the approval round-trip.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/engine"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const instructions = `Coxswain (cox) gives you the user's ops tools: SSH hosts and remote commands.
- Start with ssh_hosts_list to see hosts, their tags (env=prod is production) and notes. Respect host notes.
- You never handle credentials; never ask the user for passwords or keys.
- Some actions need the user's approval. The user usually decides in a macOS dialog while your call waits.
  If a result says "approval_required", tell the user what you want to do and why, then call
  cox_approval_wait with the approval_id; it runs the action once approved.
- If an action is denied, do not try to reach the same result another way.
- Large outputs are truncated; read more with artifact_read.`

// ToolName maps an Action ID to an MCP tool name.
func ToolName(id string) string { return strings.ReplaceAll(id, ".", "_") }

// NewServer builds the MCP server. confirm (may be nil) is asked first
// when an action needs approval, e.g. a macOS dialog.
func NewServer(e *engine.Engine, actor action.Actor, confirm engine.Confirmer, version string) *sdk.Server {
	s := sdk.NewServer(&sdk.Implementation{Name: "cox", Version: version}, &sdk.ServerOptions{Instructions: instructions})
	for _, a := range e.Registry.List() {
		a := a
		s.AddTool(&sdk.Tool{
			Name:        ToolName(a.ID),
			Title:       a.Summary,
			Description: a.Description,
			InputSchema: a.Input,
		}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return result(e.Invoke(ctx, actor, a.ID, req.Params.Arguments, confirm)), nil
		})
	}
	s.AddTool(&sdk.Tool{
		Name:  "cox_approval_wait",
		Title: "Wait for the user's approval",
		Description: "Wait until the user approves or denies a pending action (approval_id from an approval_required result). " +
			"If approved, the action runs and its result is returned. If still pending after timeout_sec, call again.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"approval_id": map[string]any{"type": "string"},
				"timeout_sec": map[string]any{"type": "integer", "minimum": 1, "maximum": 300, "description": "default 45"},
			},
			"required": []string{"approval_id"},
		},
	}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var in struct {
			ApprovalID string `json:"approval_id"`
			TimeoutSec int    `json:"timeout_sec"`
		}
		if err := action.Decode(req.Params.Arguments, &in); err != nil {
			return result(engine.Outcome{Status: engine.StatusError, Error: err.Error()}), nil
		}
		timeout := 45 * time.Second
		if in.TimeoutSec > 0 {
			timeout = time.Duration(min(in.TimeoutSec, 300)) * time.Second
		}
		if _, err := e.Wait(ctx, in.ApprovalID, timeout, 500*time.Millisecond); err != nil {
			return result(engine.Outcome{Status: engine.StatusError, Error: err.Error()}), nil
		}
		return result(e.RunApproved(ctx, actor, in.ApprovalID)), nil
	})
	return s
}

func result(out engine.Outcome) *sdk.CallToolResult {
	switch out.Status {
	case engine.StatusApprovalRequired:
		out.Hint = fmt.Sprintf("Needs the user's approval. Explain to the user what you want to do and why, then call "+
			"cox_approval_wait with approval_id=%s. The user can also approve in a terminal with `cox approve %s`.",
			out.ApprovalID, out.ApprovalID)
	case engine.StatusApprovalPending:
		out.Hint = "Still waiting for the user. Call cox_approval_wait again, or ask the user to decide."
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		b = []byte(fmt.Sprintf(`{"status":"error","error":%q}`, err.Error()))
	}
	return &sdk.CallToolResult{
		Content: []sdk.Content{&sdk.TextContent{Text: string(b)}},
		IsError: out.Status == engine.StatusError || out.Status == engine.StatusDenied,
	}
}

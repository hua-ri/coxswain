// Package mcp exposes every Action as an MCP tool (ssh.exec → ssh_exec).
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/clients"
	"github.com/hua-ri/coxswain/internal/engine"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const instructions = `Coxswain (cox) gives you the user's ops tools: SSH hosts and remote commands.
- Start with ssh_hosts_list to see hosts, their tags and notes. Respect host notes.
- Hosts tagged env=prod are production: confirm with the user before changing them.
- You never handle credentials; never ask the user for passwords or keys.
- Large outputs are truncated; read more with artifact_read.`

// ToolName maps an Action ID to an MCP tool name.
func ToolName(id string) string { return strings.ReplaceAll(id, ".", "_") }

// Gate reports whether a session's client may call tools. It is first
// called when the session is initialized (so the user is asked right
// away) and again before every tool call; implementations cache.
type Gate func(ctx context.Context, ss *sdk.ServerSession) bool

// NewServer builds the MCP server for one client. gate may be nil (allow).
func NewServer(e *engine.Engine, actor action.Actor, version string, gate Gate) *sdk.Server {
	opts := &sdk.ServerOptions{Instructions: instructions}
	if gate != nil {
		opts.InitializedHandler = func(_ context.Context, req *sdk.InitializedRequest) {
			go gate(context.Background(), req.Session)
		}
	}
	s := sdk.NewServer(&sdk.Implementation{Name: "cox", Version: version}, opts)
	for _, a := range e.Registry.List() {
		a := a
		s.AddTool(&sdk.Tool{
			Name:        ToolName(a.ID),
			Title:       a.Summary,
			Description: a.Description,
			InputSchema: a.Input,
			Annotations: annotations(a),
		}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			if gate != nil && !gate(ctx, req.Session) {
				return result(engine.Outcome{Status: engine.StatusError, Error: clients.NotAuthorized,
					Hint: clients.NotAuthorizedHint(actor.Name)}), nil
			}
			return result(e.Invoke(ctx, actor, a.ID, req.Params.Arguments)), nil
		})
	}
	return s
}

// annotations maps an Action's hints to MCP. Destructive and open-world
// hints default to true in MCP, so they are always set explicitly.
func annotations(a *action.Action) *sdk.ToolAnnotations {
	destructive, openWorld := a.Annotations.Destructive, a.Annotations.OpenWorld
	return &sdk.ToolAnnotations{
		Title:           a.Summary,
		ReadOnlyHint:    a.Annotations.ReadOnly,
		IdempotentHint:  a.Annotations.Idempotent,
		DestructiveHint: &destructive,
		OpenWorldHint:   &openWorld,
	}
}

func result(out engine.Outcome) *sdk.CallToolResult {
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		b = []byte(fmt.Sprintf(`{"status":"error","error":%q}`, err.Error()))
	}
	return &sdk.CallToolResult{
		Content: []sdk.Content{&sdk.TextContent{Text: string(b)}},
		IsError: out.Status != engine.StatusOK,
	}
}

// Package mcp exposes every Action as an MCP tool (ssh.exec → ssh_exec).
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hua-ri/coxswain/internal/action"
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

// NewServer builds the MCP server for one client.
func NewServer(e *engine.Engine, actor action.Actor, version string) *sdk.Server {
	s := sdk.NewServer(&sdk.Implementation{Name: "cox", Version: version}, &sdk.ServerOptions{Instructions: instructions})
	for _, a := range e.Registry.List() {
		a := a
		s.AddTool(&sdk.Tool{
			Name:        ToolName(a.ID),
			Title:       a.Summary,
			Description: a.Description,
			InputSchema: a.Input,
		}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return result(e.Invoke(ctx, actor, a.ID, req.Params.Arguments)), nil
		})
	}
	return s
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

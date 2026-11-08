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

const instructions = `Coxswain (cox) gives you the user's ops tools: SSH hosts, remote commands, shared terminals, files
and tunnels.
- Start with ssh_hosts_list to see hosts, their tags and notes. Respect host notes.
- Use ssh_exec for one-off commands. For long-running work or work the user should watch, open a shared
  terminal (terminal_open, terminal_send, terminal_read); the user sees it and can type in it too.
- ssh_batch runs one command on several hosts; a canary host runs first and a failure stops the rest.
- Use file_list, file_stat and file_read for remote files; file_write replaces a file atomically.
  For large or binary files use file_download: the file is saved locally and you get its path.
- tunnel_open forwards ports through a host (local, remote, socks); close your tunnels with tunnel_close when done.
- Hosts tagged env=prod are production: confirm with the user before changing them.
- You never handle credentials; never ask the user for passwords or keys.
- Large outputs are truncated; read more with artifact_read.`

// ToolName maps an Action ID to an MCP tool name.
func ToolName(id string) string { return strings.ReplaceAll(id, ".", "_") }

// Gate reports whether a session's client may call tools, and if not,
// why (a clients.Reason…). It is first called when the session is
// initialized (so the user is asked right away) and again before every
// tool call.
type Gate func(ctx context.Context, ss *sdk.ServerSession) (bool, string)

// NewServer builds the MCP server for one client. gate may be nil (allow).
func NewServer(e *engine.Engine, actor action.Actor, version string, gate Gate) *sdk.Server {
	opts := &sdk.ServerOptions{Instructions: instructions}
	if gate != nil {
		opts.InitializedHandler = func(_ context.Context, req *sdk.InitializedRequest) {
			go gate(context.Background(), req.Session)
		}
	}
	return build(e.Registry, version, opts, func(a *action.Action) sdk.ToolHandler {
		return func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			who := actor
			if req.Session != nil {
				who.Session = req.Session.ID()
			}
			if gate != nil {
				if ok, reason := gate(ctx, req.Session); !ok {
					e.Refuse(who, a.ID, reason)
					return result(engine.Outcome{Status: engine.StatusError, Error: clients.NotAuthorized,
						Hint: clients.NotAuthorizedHint(actor.Name, reason)}), nil
				}
			}
			return result(e.Invoke(ctx, who, a.ID, req.Params.Arguments)), nil
		}
	})
}

// NewOffline builds a server with the same tools and instructions whose
// tool calls all fail with message. `cox mcp` answers with it while the
// real server is not running, so the agent's handshake still succeeds.
func NewOffline(reg *action.Registry, version, message string) *sdk.Server {
	return build(reg, version, &sdk.ServerOptions{Instructions: instructions}, func(*action.Action) sdk.ToolHandler {
		return func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: message}}, IsError: true}, nil
		}
	})
}

// sessionless is the first MCP protocol version without sessions. Coxswain
// authorizes, revokes and (in `cox mcp`) re-establishes per session, so
// it offers only the session-based versions.
const sessionless = "2026-07-28"

// protocolVersions are the SDK's versions older than sessionless.
func protocolVersions() []string {
	var out []string
	for _, v := range sdk.SupportedProtocolVersions() {
		if v < sessionless {
			out = append(out, v)
		}
	}
	return out
}

// build registers one tool per Action with the given handlers.
func build(reg *action.Registry, version string, opts *sdk.ServerOptions, handler func(*action.Action) sdk.ToolHandler) *sdk.Server {
	opts.SupportedProtocolVersions = protocolVersions()
	s := sdk.NewServer(&sdk.Implementation{Name: "cox", Version: version}, opts)
	for _, a := range reg.List() {
		s.AddTool(&sdk.Tool{
			Name:        ToolName(a.ID),
			Title:       a.Summary,
			Description: a.Description,
			InputSchema: a.Input,
			Annotations: annotations(a),
		}, handler(a))
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

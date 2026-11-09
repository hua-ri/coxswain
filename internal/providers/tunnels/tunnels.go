// Package tunnels registers the tunnel Actions: agents open, list and
// close port forwards that the user sees (and can close) in the desktop
// app.
package tunnels

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/inventory"
	"github.com/hua-ri/coxswain/internal/tunnel"
)

type Provider struct {
	Hosts   *inventory.Store
	Tunnels *tunnel.Manager
}

func (p *Provider) Register(r *action.Registry) {
	r.Register(p.list())
	r.Register(p.open())
	r.Register(p.close())
}

func hostTarget(h inventory.Host) action.Target {
	return action.Target{Kind: "host", ID: h.Name, Tags: h.Tags}
}

func (p *Provider) list() *action.Action {
	return &action.Action{
		ID:      "tunnel.list",
		Summary: "List tunnels (port forwards)",
		Description: "List the open tunnels. Each has: id; type (local, remote, socks); host; listen (where connections " +
			"enter); target (where they go; empty for socks); owner (who opened it); created_at; status (open, or " +
			"reconnecting with error saying why); connections (open now) and total_connections; bytes_out (toward the " +
			"target) and bytes_in; idle_minutes (closes after that long unused, 0 = never); failed_connections (connections " +
			"that could not reach the target: the host refused the forward, e.g. sshd AllowTcpForwarding no, or the " +
			"target refused) with last_error and last_error_at for the latest one.",
		Input:       map[string]any{"type": "object", "properties": map[string]any{}},
		Annotations: action.Annotations{ReadOnly: true, Idempotent: true},
		Label: func(context.Context, json.RawMessage) (action.Assessment, error) {
			return action.Assessment{Risk: action.Read}, nil
		},
		Run: func(context.Context, json.RawMessage) (any, error) { return p.Tunnels.List(), nil },
	}
}

// OpenOutput is the result of tunnel.open.
type OpenOutput struct {
	tunnel.Tunnel
	HostNote string            `json:"host_note,omitempty"`
	HostTags map[string]string `json:"host_tags,omitempty"`
	Hint     string            `json:"hint"`
}

func (p *Provider) open() *action.Action {
	return &action.Action{
		ID:      "tunnel.open",
		Summary: "Open a tunnel (port forward) through a saved SSH host",
		Description: "Open a tunnel through a saved SSH host. Types: " +
			"local (like ssh -L): 127.0.0.1:local_port on this machine leads to remote_host:remote_port as seen from the host " +
			"(remote_host defaults to 127.0.0.1, i.e. a service on the host itself); " +
			"socks (like ssh -D): a SOCKS5 proxy on 127.0.0.1:local_port whose connections leave from the host; " +
			"remote (like ssh -R) exposes a port of this machine on the host: only the user can open those (desktop app or " +
			"`cox tunnel open`), so ask them. " +
			"Ports default to a free one; the result says which (listen). Local and socks tunnels listen on 127.0.0.1 only " +
			"(any local process can use them while open), reconnect by themselves and are shown to the user, who can close " +
			"them. Close yours with tunnel_close when done; yours close after idle_minutes without connections " +
			"(default 30, 1..240). At most 16 tunnels per agent. " +
			"Hosts tagged env=prod are production: confirm with the user first. Respect the host note.",
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"host":         map[string]any{"type": "string", "description": "saved host name (see ssh_hosts_list)"},
				"type":         map[string]any{"type": "string", "enum": []string{tunnel.Local, tunnel.Remote, tunnel.Socks}},
				"local_port":   map[string]any{"type": "integer", "minimum": 0, "maximum": 65535, "description": "port on this machine (local/socks: listen, default free; remote: required target)"},
				"remote_host":  map[string]any{"type": "string", "description": "local only: target host as seen from the SSH host, default 127.0.0.1"},
				"remote_port":  map[string]any{"type": "integer", "minimum": 0, "maximum": 65535, "description": "local: target port (required); remote: port to listen on at the host (127.0.0.1), default free"},
				"idle_minutes": map[string]any{"type": "integer", "minimum": 0, "description": "close after this many minutes without connections; agents: 1..240, default 30; people: default and 0 = never"},
			},
			"required": []string{"host", "type"},
		},
		Positional:  []string{"host", "type"},
		Annotations: action.Annotations{Destructive: true, OpenWorld: true},
		Label: func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
			var in tunnel.Spec
			if err := action.Decode(raw, &in); err != nil {
				return action.Assessment{}, err
			}
			h, err := p.Hosts.Get(in.Host)
			if err != nil {
				return action.Assessment{}, err
			}
			risk := action.Write
			if in.Type == tunnel.Remote {
				risk = action.Destructive // opens this machine to the host
			}
			return action.Assessment{Risk: risk, Targets: []action.Target{hostTarget(h)}, Preview: preview(in)}, nil
		},
		Run: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in tunnel.Spec
			if err := action.Decode(raw, &in); err != nil {
				return nil, err
			}
			h, err := p.Hosts.Get(in.Host)
			if err != nil {
				return nil, err
			}
			who, _ := action.ActorFrom(ctx)
			who.Session = "" // the owner is the client, not one of its sessions
			tn, err := p.Tunnels.Open(ctx, in, who)
			if err != nil {
				return nil, err
			}
			out := OpenOutput{Tunnel: tn, HostNote: h.Note, HostTags: h.Tags}
			switch tn.Type {
			case tunnel.Local:
				out.Hint = fmt.Sprintf("connect to %s to reach %s from %s", tn.Listen, tn.Target, tn.Host)
			case tunnel.Socks:
				out.Hint = fmt.Sprintf("use socks5h://%s as a proxy (e.g. curl -x socks5h://%s URL)", tn.Listen, tn.Listen)
			case tunnel.Remote:
				out.Hint = fmt.Sprintf("on %s, %s leads to %s on this machine", tn.Host, tn.Listen, tn.Target)
			}
			if tn.Type != tunnel.Remote {
				out.Hint += "; check the first connection with your own client (e.g. curl or nc): the host may refuse " +
					"forwards or the target may refuse, which shows only as a closed connection here — then tunnel_list " +
					"shows why in last_error"
			}
			out.Hint += fmt.Sprintf("; close it with tunnel_close %s when done", tn.ID)
			return out, nil
		},
	}
}

func preview(s tunnel.Spec) string {
	switch s.Type {
	case tunnel.Local:
		rh := s.RemoteHost
		if rh == "" {
			rh = "127.0.0.1"
		}
		return fmt.Sprintf("local tunnel 127.0.0.1:%d -> %s:%s:%d", s.LocalPort, s.Host, rh, s.RemotePort)
	case tunnel.Remote:
		return fmt.Sprintf("remote tunnel %s:127.0.0.1:%d -> 127.0.0.1:%d on this machine", s.Host, s.RemotePort, s.LocalPort)
	case tunnel.Socks:
		return fmt.Sprintf("socks proxy 127.0.0.1:%d via %s", s.LocalPort, s.Host)
	}
	return fmt.Sprintf("%s tunnel via %s", s.Type, s.Host)
}

type idInput struct {
	ID string `json:"id"`
}

func (p *Provider) close() *action.Action {
	return &action.Action{
		ID:      "tunnel.close",
		Summary: "Close a tunnel",
		Description: "Close a tunnel and the connections through it. Agents may close only tunnels they opened; " +
			"ask the user to close theirs.",
		Input: map[string]any{
			"type":       "object",
			"properties": map[string]any{"id": map[string]any{"type": "string", "description": "tunnel id (f_…), from tunnel_open or tunnel_list"}},
			"required":   []string{"id"},
		},
		Positional:  []string{"id"},
		Annotations: action.Annotations{Destructive: true, Idempotent: true},
		Label: func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
			var in idInput
			if err := action.Decode(raw, &in); err != nil {
				return action.Assessment{}, err
			}
			tn, ok := p.Tunnels.Get(in.ID)
			if !ok {
				return action.Assessment{}, fmt.Errorf("%w: %s", tunnel.ErrNotFound, in.ID)
			}
			t := action.Target{Kind: "host", ID: tn.Host}
			if h, err := p.Hosts.Get(tn.Host); err == nil {
				t.Tags = h.Tags
			}
			return action.Assessment{Risk: action.Write, Targets: []action.Target{t},
				Preview: fmt.Sprintf("close %s tunnel %s on %s (opened by %s)", tn.Type, tn.ID, tn.Host, tn.Owner)}, nil
		},
		Run: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in idInput
			if err := action.Decode(raw, &in); err != nil {
				return nil, err
			}
			tn, ok := p.Tunnels.Get(in.ID)
			if !ok {
				return nil, fmt.Errorf("%w: %s", tunnel.ErrNotFound, in.ID)
			}
			who, _ := action.ActorFrom(ctx)
			if who.Kind != action.Human && (who.Kind != tn.Owner.Kind || who.Name != tn.Owner.Name) {
				return nil, fmt.Errorf("tunnel %s was opened by %s; agents may close only tunnels they opened", tn.ID, tn.Owner)
			}
			if err := p.Tunnels.Close(in.ID, who); err != nil {
				return nil, err
			}
			return map[string]any{"id": in.ID, "closed": true}, nil
		},
	}
}

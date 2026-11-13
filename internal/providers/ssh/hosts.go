package ssh

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/inventory"
	"github.com/hua-ri/coxswain/internal/sshx"
)

// agentFacing reports whether a tag key tells agents how to behave
// (env=prod, ai=…): changing one is rated destructive, like the note.
func agentFacing(key string) bool {
	return key == "env" || key == "ai" || strings.HasPrefix(key, "ai.")
}

// hostFields are the fields of ssh.hosts.add and ssh.hosts.update; unset
// fields are left as they are by update.
type hostFields struct {
	Name          string    `json:"name"`
	NewName       *string   `json:"new_name"`
	HostName      *string   `json:"hostname"`
	Port          *int      `json:"port"`
	User          *string   `json:"user"`
	IdentityFiles *[]string `json:"identity_files"`
	ProxyJump     *string   `json:"proxy_jump"`
	Tags          *[]string `json:"tags"`
	Note          *string   `json:"note"`
}

func hostProperties(update bool) map[string]any {
	str := func(d string) map[string]any { return map[string]any{"type": "string", "description": d} }
	props := map[string]any{
		"name":           str("host name, as used with ssh_exec: letters, digits, '.', '_' and '-'"),
		"hostname":       str("address: DNS name or IP"),
		"port":           map[string]any{"type": "integer", "minimum": 1, "maximum": 65535, "description": "default 22"},
		"user":           str("login user; default: the user's local name"),
		"identity_files": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "private key files (~ allowed); default: ssh-agent and ~/.ssh/id_*"},
		"proxy_jump":     str("jump host: a saved host's name or user@host:port (comma-separated for a chain)"),
		"tags":           map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "key=value; env=prod marks production"},
		"note":           str("shown to agents with every result on this host"),
	}
	if update {
		props["new_name"] = str("rename the host")
		props["tags"] = map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "replace all tags, key=value"}
	}
	return props
}

func parseTags(list []string) (map[string]string, error) {
	tags := map[string]string{}
	for _, kv := range list {
		k, v, ok := strings.Cut(kv, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" || strings.ContainsAny(k, " \t") {
			return nil, fmt.Errorf("tag %q must be key=value", kv)
		}
		tags[k] = strings.TrimSpace(v)
	}
	if len(tags) == 0 {
		return nil, nil
	}
	return tags, nil
}

// apply sets in's fields on h; names of identity files get ~ expanded.
func (p *Provider) apply(h inventory.Host, in hostFields) (inventory.Host, error) {
	if in.HostName != nil {
		h.HostName = strings.TrimSpace(*in.HostName)
	}
	if in.Port != nil {
		h.Port = *in.Port
		if h.Port < 1 || h.Port > 65535 {
			return h, fmt.Errorf("port %d is out of range (1-65535)", h.Port)
		}
	}
	if in.User != nil {
		h.User = strings.TrimSpace(*in.User)
	}
	if in.ProxyJump != nil {
		h.ProxyJump = strings.TrimSpace(*in.ProxyJump)
	}
	if in.IdentityFiles != nil {
		h.IdentityFiles = nil
		for _, f := range *in.IdentityFiles {
			f = strings.TrimSpace(f)
			if f == "" {
				return h, fmt.Errorf("empty identity file name")
			}
			h.IdentityFiles = append(h.IdentityFiles, p.expand(f))
		}
	}
	if in.Tags != nil {
		tags, err := parseTags(*in.Tags)
		if err != nil {
			return h, err
		}
		h.Tags = tags
	}
	if in.Note != nil {
		h.Note = strings.TrimSpace(*in.Note)
	}
	return h, nil
}

func (p *Provider) expand(f string) string {
	if f == "~" {
		return p.UserHome
	}
	if strings.HasPrefix(f, "~/") {
		return p.UserHome + "/" + f[2:]
	}
	return f
}

// riskOfChange rates a change from old to new (old is zero for an add):
// agent-facing tags and the note instruct agents, so changing them is
// destructive.
func riskOfChange(old, h inventory.Host) action.Risk {
	if h.Note != old.Note {
		return action.Destructive
	}
	keys := slices.Collect(maps.Keys(h.Tags))
	keys = append(keys, slices.Collect(maps.Keys(old.Tags))...)
	for _, k := range keys {
		if agentFacing(k) && h.Tags[k] != old.Tags[k] {
			return action.Destructive
		}
	}
	return action.Write
}

// planned is a validated add or update: the host as it will be saved.
type planned struct {
	old     inventory.Host // zero for an add
	host    inventory.Host
	renamed []string // hosts whose ProxyJump follows a rename
}

func (p *Provider) planAdd(raw json.RawMessage) (planned, error) {
	var in hostFields
	if err := action.Decode(raw, &in); err != nil {
		return planned{}, err
	}
	if in.HostName == nil {
		return planned{}, fmt.Errorf("hostname is required")
	}
	all, err := p.hostMap()
	if err != nil {
		return planned{}, err
	}
	if _, ok := all[in.Name]; ok {
		return planned{}, fmt.Errorf("a host named %q already exists (edit it with ssh_hosts_update)", in.Name)
	}
	h, err := p.apply(inventory.Host{Name: strings.TrimSpace(in.Name), Source: "manual"}, in)
	if err != nil {
		return planned{}, err
	}
	if err := inventory.Validate(h, all); err != nil {
		return planned{}, err
	}
	return planned{host: h}, nil
}

func (p *Provider) planUpdate(raw json.RawMessage) (planned, error) {
	var in hostFields
	if err := action.Decode(raw, &in); err != nil {
		return planned{}, err
	}
	all, err := p.hostMap()
	if err != nil {
		return planned{}, err
	}
	old, ok := all[in.Name]
	if !ok {
		return planned{}, fmt.Errorf("%w: %s (list hosts with ssh_hosts_list)", inventory.ErrNotFound, in.Name)
	}
	h, err := p.apply(old, in)
	if err != nil {
		return planned{}, err
	}
	pl := planned{old: old}
	if in.NewName != nil && *in.NewName != old.Name {
		h.Name = strings.TrimSpace(*in.NewName)
		if _, ok := all[h.Name]; ok {
			return planned{}, fmt.Errorf("a host named %q already exists", h.Name)
		}
		for _, o := range all {
			if slices.Contains(jumps(o.ProxyJump), old.Name) {
				pl.renamed = append(pl.renamed, o.Name)
			}
		}
		sort.Strings(pl.renamed)
	}
	others := maps.Clone(all)
	delete(others, old.Name)
	if err := inventory.Validate(h, others); err != nil {
		return planned{}, err
	}
	pl.host = h
	return pl, nil
}

func jumps(spec string) []string {
	var out []string
	for _, s := range strings.Split(spec, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (p *Provider) hostMap() (map[string]inventory.Host, error) {
	list, err := p.Hosts.List()
	if err != nil {
		return nil, err
	}
	m := make(map[string]inventory.Host, len(list))
	for _, h := range list {
		m[h.Name] = h
	}
	return m, nil
}

func (p *Provider) hostsAdd() *action.Action {
	return &action.Action{
		ID:      "ssh.hosts.add",
		Summary: "Add an SSH host",
		Description: "Save a new SSH host. Tags (env=prod marks production) and the note tell agents how to treat it; " +
			"set them only as the user says. Before the first connection the user has to trust the host's key " +
			"(in cox desktop, or with `cox ssh hosts trust_key`); agents cannot.",
		Input:       map[string]any{"type": "object", "properties": hostProperties(false), "required": []string{"name", "hostname"}},
		Positional:  []string{"name", "hostname"},
		Annotations: action.Annotations{Destructive: true},
		Label: func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
			pl, err := p.planAdd(raw)
			if err != nil {
				return action.Assessment{}, err
			}
			return action.Assessment{Risk: riskOfChange(inventory.Host{}, pl.host), Targets: []action.Target{hostTarget(pl.host)},
				Preview: fmt.Sprintf("add host %s (%s)", pl.host.Name, describe(pl.host))}, nil
		},
		Run: func(_ context.Context, raw json.RawMessage) (any, error) {
			pl, err := p.planAdd(raw)
			if err != nil {
				return nil, err
			}
			err = p.Hosts.Update(func(m map[string]inventory.Host) error {
				if _, ok := m[pl.host.Name]; ok {
					return fmt.Errorf("a host named %q already exists", pl.host.Name)
				}
				m[pl.host.Name] = pl.host
				return nil
			})
			return pl.host, err
		},
	}
}

func (p *Provider) hostsUpdate() *action.Action {
	return &action.Action{
		ID:      "ssh.hosts.update",
		Summary: "Change a saved SSH host",
		Description: "Change a saved host's address, port, user, keys, jump host, tags or note, or rename it (new_name). " +
			"Fields you leave out stay as they are; tags replaces all tags. Change tags and the note only when the user asks.",
		Input:       map[string]any{"type": "object", "properties": hostProperties(true), "required": []string{"name"}},
		Positional:  []string{"name"},
		Annotations: action.Annotations{Destructive: true, Idempotent: true},
		Label: func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
			pl, err := p.planUpdate(raw)
			if err != nil {
				return action.Assessment{}, err
			}
			targets := []action.Target{hostTarget(pl.old)}
			preview := fmt.Sprintf("change host %s: %s", pl.old.Name, describe(pl.host))
			if pl.host.Name != pl.old.Name {
				preview = fmt.Sprintf("rename host %s to %s: %s", pl.old.Name, pl.host.Name, describe(pl.host))
			}
			for _, n := range pl.renamed {
				h, _ := p.Hosts.Get(n)
				targets = append(targets, hostTarget(h))
			}
			if pl.host.Note != pl.old.Note {
				preview += fmt.Sprintf(", note %q", pl.host.Note)
			}
			return action.Assessment{Risk: riskOfChange(pl.old, pl.host), Targets: targets, Preview: preview}, nil
		},
		Run: func(_ context.Context, raw json.RawMessage) (any, error) {
			pl, err := p.planUpdate(raw)
			if err != nil {
				return nil, err
			}
			err = p.Hosts.Update(func(m map[string]inventory.Host) error {
				if _, ok := m[pl.old.Name]; !ok {
					return fmt.Errorf("%w: %s", inventory.ErrNotFound, pl.old.Name)
				}
				if pl.host.Name != pl.old.Name {
					if _, ok := m[pl.host.Name]; ok {
						return fmt.Errorf("a host named %q already exists", pl.host.Name)
					}
					delete(m, pl.old.Name)
					for name, o := range m {
						js := jumps(o.ProxyJump)
						if i := slices.Index(js, pl.old.Name); i >= 0 {
							js[i] = pl.host.Name
							o.ProxyJump = strings.Join(js, ",")
							m[name] = o
						}
					}
				}
				m[pl.host.Name] = pl.host
				return nil
			})
			return pl.host, err
		},
	}
}

func (p *Provider) hostsRemove() *action.Action {
	type input struct {
		Name string `json:"name"`
	}
	check := func(raw json.RawMessage) (inventory.Host, error) {
		var in input
		if err := action.Decode(raw, &in); err != nil {
			return inventory.Host{}, err
		}
		all, err := p.hostMap()
		if err != nil {
			return inventory.Host{}, err
		}
		h, ok := all[in.Name]
		if !ok {
			return h, fmt.Errorf("%w: %s", inventory.ErrNotFound, in.Name)
		}
		var users []string
		for _, o := range all {
			if slices.Contains(jumps(o.ProxyJump), h.Name) {
				users = append(users, o.Name)
			}
		}
		if len(users) > 0 {
			sort.Strings(users)
			return h, fmt.Errorf("%s is the jump host of %s; change or remove those first", h.Name, strings.Join(users, ", "))
		}
		return h, nil
	}
	return &action.Action{
		ID:      "ssh.hosts.remove",
		Summary: "Remove a saved SSH host",
		Description: "Remove a saved host (and its tags and note) from Coxswain. Open terminals and tunnels on it stay " +
			"until they are closed. Remove hosts only when the user asks.",
		Input: map[string]any{"type": "object", "properties": map[string]any{
			"name": map[string]any{"type": "string"},
		}, "required": []string{"name"}},
		Positional:  []string{"name"},
		Annotations: action.Annotations{Destructive: true, Idempotent: true},
		Label: func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
			h, err := check(raw)
			if err != nil {
				return action.Assessment{}, err
			}
			return action.Assessment{Risk: action.Destructive, Targets: []action.Target{hostTarget(h)},
				Preview: fmt.Sprintf("remove host %s (%s)", h.Name, describe(h))}, nil
		},
		Run: func(_ context.Context, raw json.RawMessage) (any, error) {
			h, err := check(raw)
			if err != nil {
				return nil, err
			}
			err = p.Hosts.Update(func(m map[string]inventory.Host) error {
				delete(m, h.Name)
				return nil
			})
			return map[string]any{"removed": h.Name}, err
		},
	}
}

// keyInput names a saved host, or describes one not saved yet (hostname
// set): the desktop's add form checks a host before saving it.
type keyInput struct {
	hostFields
	Fingerprint string `json:"fingerprint"`
	Address     string `json:"address"`
}

func keyProperties(extra map[string]any) map[string]any {
	props := hostProperties(false)
	props["name"] = map[string]any{"type": "string", "description": "saved host (or, with hostname, a host not saved yet)"}
	delete(props, "tags")
	delete(props, "note")
	maps.Copy(props, extra)
	return props
}

// keyHost is the host a key action is about, and whether it is saved.
func (p *Provider) keyHost(raw json.RawMessage) (inventory.Host, keyInput, bool, error) {
	var in keyInput
	if err := action.Decode(raw, &in); err != nil {
		return inventory.Host{}, in, false, err
	}
	if in.HostName == nil {
		h, err := p.Hosts.Get(in.Name)
		return h, in, true, err
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = strings.TrimSpace(*in.HostName)
	}
	all, err := p.hostMap()
	if err != nil {
		return inventory.Host{}, in, false, err
	}
	h, err := p.apply(inventory.Host{Name: name}, in.hostFields)
	if err != nil {
		return h, in, false, err
	}
	delete(all, h.Name)
	if err := inventory.Validate(h, all); err != nil {
		return h, in, false, err
	}
	return h, in, false, nil
}

func keyTarget(h inventory.Host, saved bool) []action.Target {
	if saved {
		return []action.Target{hostTarget(h)}
	}
	return []action.Target{{Kind: "host", ID: h.Name}}
}

func (p *Provider) hostsScanKey() *action.Action {
	return &action.Action{
		ID:      "ssh.hosts.scan_key",
		Summary: "Show a host's SSH key fingerprint",
		Description: "Connect to a host (through its jump hosts) and return its host key type and SHA256 fingerprint, " +
			"and whether it is trusted, unknown or changed, without logging in. With a jump host whose key is not " +
			"trusted yet, that jump host's key is returned (host names it). Only the user can trust a key.",
		Input:       map[string]any{"type": "object", "properties": keyProperties(nil), "required": []string{"name"}},
		Positional:  []string{"name"},
		Annotations: action.Annotations{ReadOnly: true, Idempotent: true, OpenWorld: true},
		Label: func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
			h, _, saved, err := p.keyHost(raw)
			if err != nil {
				return action.Assessment{}, err
			}
			return action.Assessment{Risk: action.Read, Targets: keyTarget(h, saved),
				Preview: fmt.Sprintf("scan the host key of %s (%s)", h.Name, describe(h))}, nil
		},
		Run: func(ctx context.Context, raw json.RawMessage) (any, error) {
			h, _, _, err := p.keyHost(raw)
			if err != nil {
				return nil, err
			}
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			return p.Dialer.ScanKey(ctx, h)
		},
	}
}

const trustHint = "check the fingerprint and trust it in cox desktop (Edit host → Test connection) or with `cox ssh hosts trust_key`"

func (p *Provider) hostsTrustKey() *action.Action {
	return &action.Action{
		ID:      "ssh.hosts.trust_key",
		Summary: "Trust a host's SSH key (you only)",
		Description: "For the user only; agents are refused. Scans the host again and records its key in " +
			"~/.coxswain/known_hosts if the fingerprint is the one given. A changed key is never trusted: remove the old " +
			"one first (forget_key). ~/.ssh/known_hosts is never written.",
		Input: map[string]any{"type": "object", "properties": keyProperties(map[string]any{
			"fingerprint": map[string]any{"type": "string", "description": "SHA256:… as shown by scan_key, after checking it"},
		}), "required": []string{"name", "fingerprint"}},
		Positional:  []string{"name"},
		Annotations: action.Annotations{Destructive: true, Idempotent: true, OpenWorld: true},
		Label: func(ctx context.Context, raw json.RawMessage) (action.Assessment, error) {
			if err := action.HumanOnly(ctx, "trusting a host key", trustHint); err != nil {
				return action.Assessment{}, err
			}
			h, in, saved, err := p.keyHost(raw)
			if err != nil {
				return action.Assessment{}, err
			}
			if !strings.HasPrefix(in.Fingerprint, "SHA256:") {
				return action.Assessment{}, fmt.Errorf("fingerprint must be SHA256:… as shown by scan_key")
			}
			return action.Assessment{Risk: action.Write, Targets: keyTarget(h, saved),
				Preview: fmt.Sprintf("trust host key %s for %s (%s)", in.Fingerprint, h.Name, describe(h))}, nil
		},
		Run: func(ctx context.Context, raw json.RawMessage) (any, error) {
			h, in, _, err := p.keyHost(raw)
			if err != nil {
				return nil, err
			}
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			return p.Dialer.TrustKey(ctx, h, in.Fingerprint)
		},
	}
}

func (p *Provider) hostsForgetKey() *action.Action {
	return &action.Action{
		ID:      "ssh.hosts.forget_key",
		Summary: "Remove a host's recorded SSH key from Coxswain (you only)",
		Description: "For the user only; agents are refused. Removes the host's entries (or those of address, one of " +
			"its jump hosts) from ~/.coxswain/known_hosts, e.g. after the host was reinstalled. Keys in ~/.ssh/known_hosts " +
			"are the user's: remove them with ssh-keygen -R.",
		Input: map[string]any{"type": "object", "properties": keyProperties(map[string]any{
			"address": map[string]any{"type": "string", "description": "host:port to forget; default the host's own"},
		}), "required": []string{"name"}},
		Positional:  []string{"name"},
		Annotations: action.Annotations{Destructive: true, Idempotent: true},
		Label: func(ctx context.Context, raw json.RawMessage) (action.Assessment, error) {
			if err := action.HumanOnly(ctx, "removing a recorded host key", "remove it in cox desktop or with `cox ssh hosts forget_key`"); err != nil {
				return action.Assessment{}, err
			}
			h, in, saved, err := p.keyHost(raw)
			if err != nil {
				return action.Assessment{}, err
			}
			what := h.Name
			if in.Address != "" {
				what += " (" + in.Address + ")"
			}
			return action.Assessment{Risk: action.Destructive, Targets: keyTarget(h, saved), Preview: "forget the host key of " + what}, nil
		},
		Run: func(_ context.Context, raw json.RawMessage) (any, error) {
			h, in, _, err := p.keyHost(raw)
			if err != nil {
				return nil, err
			}
			n, err := p.Dialer.ForgetKey(h, in.Address)
			return map[string]any{"host": h.Name, "removed": n}, err
		},
	}
}

// TestOutput is the result of ssh.hosts.test.
type TestOutput struct {
	Host       string `json:"host"`
	OS         string `json:"os"` // uname -s
	DurationMs int64  `json:"duration_ms"`
}

func (p *Provider) hostsTest() *action.Action {
	return &action.Action{
		ID:      "ssh.hosts.test",
		Summary: "Check that a host accepts a login",
		Description: "Log in to a host (saved, or described by hostname etc.) and run `uname -s`. Fails if its key is " +
			"not trusted yet or no key is accepted.",
		Input:       map[string]any{"type": "object", "properties": keyProperties(nil), "required": []string{"name"}},
		Positional:  []string{"name"},
		Annotations: action.Annotations{ReadOnly: true, Idempotent: true, OpenWorld: true},
		Label: func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
			h, _, saved, err := p.keyHost(raw)
			if err != nil {
				return action.Assessment{}, err
			}
			return action.Assessment{Risk: action.Read, Targets: keyTarget(h, saved),
				Preview: fmt.Sprintf("%s (%s)$ uname -s", h.Name, describe(h))}, nil
		},
		Run: func(ctx context.Context, raw json.RawMessage) (any, error) {
			h, _, _, err := p.keyHost(raw)
			if err != nil {
				return nil, err
			}
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			start := time.Now()
			conn, err := p.Dialer.Dial(ctx, h) // never pooled: the host may not be saved
			if err != nil {
				return nil, err
			}
			defer conn.Close()
			res, err := sshx.Exec(ctx, conn.Client, "uname -s", 4096)
			if err != nil {
				return nil, err
			}
			if res.ExitCode != 0 {
				return nil, fmt.Errorf("logged in, but `uname -s` exited with %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
			}
			return TestOutput{Host: h.Name, OS: strings.TrimSpace(res.Stdout), DurationMs: time.Since(start).Milliseconds()}, nil
		},
	}
}

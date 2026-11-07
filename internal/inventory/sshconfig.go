package inventory

import (
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kevinburke/ssh_config"
)

// ImportSSHConfig reads an OpenSSH client config and returns one Host for
// every concrete Host alias (patterns with *, ? or ! are skipped).
// Paths starting with ~ are expanded against home.
func ImportSSHConfig(r io.Reader, home string) ([]Host, error) {
	cfg, err := ssh_config.Decode(r)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []Host
	for _, h := range cfg.Hosts {
		for _, p := range h.Patterns {
			alias := p.String()
			if alias == "" || strings.ContainsAny(alias, "*?!") || seen[alias] {
				continue
			}
			seen[alias] = true
			host := Host{Name: alias, HostName: alias, Port: 22, Source: "ssh_config"}
			if v, _ := cfg.Get(alias, "HostName"); v != "" {
				host.HostName = strings.ReplaceAll(v, "%h", alias)
			}
			if v, _ := cfg.Get(alias, "Port"); v != "" {
				if n, err := strconv.Atoi(v); err == nil {
					host.Port = n
				}
			}
			host.User, _ = cfg.Get(alias, "User")
			host.ProxyJump, _ = cfg.Get(alias, "ProxyJump")
			if host.ProxyJump == "none" {
				host.ProxyJump = ""
			}
			files, _ := cfg.GetAll(alias, "IdentityFile")
			for _, f := range files {
				host.IdentityFiles = append(host.IdentityFiles, expandHome(f, home))
			}
			out = append(out, host)
		}
	}
	return out, nil
}

func expandHome(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

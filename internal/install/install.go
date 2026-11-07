// Package install wires cox into Claude Code, Codex and Kimi Code:
// it registers `cox mcp` as an MCP server and installs the cox Skill.
package install

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed skill/SKILL.md
var Skill []byte

type Installer struct {
	Exe      string // absolute path of the cox binary
	UserHome string
	KimiHome string // $KIMI_CODE_HOME, default ~/.kimi-code
	LookPath func(string) (string, error)
	Run      func(name string, args ...string) error
}

type Result struct {
	Agent string
	MCP   string // what was done, or why it was skipped
	Skill string // path written
	Err   error
}

// All installs into every agent found on this machine.
func (i *Installer) All() []Result {
	return []Result{i.claude(), i.codex(), i.kimi()}
}

func (i *Installer) claude() Result {
	r := Result{Agent: "Claude Code"}
	if _, err := i.LookPath("claude"); err != nil {
		r.MCP = "skipped: claude not found"
		return r
	}
	i.Run("claude", "mcp", "remove", "-s", "user", "cox") // ignore "not found"
	if r.Err = i.Run("claude", "mcp", "add", "-s", "user", "cox", "--", i.Exe, "mcp", "--actor", "claude-code"); r.Err != nil {
		return r
	}
	r.MCP = "registered (user scope)"
	r.Skill, r.Err = writeSkill(filepath.Join(i.UserHome, ".claude", "skills"))
	return r
}

func (i *Installer) codex() Result {
	r := Result{Agent: "Codex"}
	if _, err := i.LookPath("codex"); err != nil {
		r.MCP = "skipped: codex not found"
		return r
	}
	i.Run("codex", "mcp", "remove", "cox")
	if r.Err = i.Run("codex", "mcp", "add", "cox", "--", i.Exe, "mcp", "--actor", "codex"); r.Err != nil {
		return r
	}
	r.MCP = "registered"
	r.Skill, r.Err = writeSkill(filepath.Join(i.UserHome, ".codex", "skills"))
	return r
}

// kimi edits <KIMI_CODE_HOME>/mcp.json directly: Kimi Code has no
// `mcp add` command. Unrelated entries are preserved.
func (i *Installer) kimi() Result {
	r := Result{Agent: "Kimi Code"}
	home := i.KimiHome
	if home == "" {
		home = filepath.Join(i.UserHome, ".kimi-code")
	}
	if _, err := i.LookPath("kimi"); err != nil {
		if _, statErr := os.Stat(home); statErr != nil {
			r.MCP = "skipped: kimi not found"
			return r
		}
	}
	path := filepath.Join(home, "mcp.json")
	cfg := map[string]any{}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		r.Err = err
		return r
	case len(b) > 0:
		if err := json.Unmarshal(b, &cfg); err != nil {
			r.Err = fmt.Errorf("%s is not valid JSON, not touching it: %w", path, err)
			return r
		}
	}
	servers, _ := cfg["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	servers["cox"] = map[string]any{"command": i.Exe, "args": []string{"mcp", "--actor", "kimi"}}
	cfg["mcpServers"] = servers
	out, _ := json.MarshalIndent(cfg, "", "  ")
	if r.Err = atomicWrite(path, append(out, '\n')); r.Err != nil {
		return r
	}
	r.MCP = "registered in " + path
	r.Skill, r.Err = writeSkill(filepath.Join(home, "skills"))
	return r
}

func writeSkill(skillsDir string) (string, error) {
	path := filepath.Join(skillsDir, "cox", "SKILL.md")
	return path, atomicWrite(path, Skill)
}

func atomicWrite(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

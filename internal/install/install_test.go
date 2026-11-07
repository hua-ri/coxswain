package install

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newInstaller(t *testing.T, found ...string) (*Installer, *[]string) {
	var calls []string
	have := map[string]bool{}
	for _, f := range found {
		have[f] = true
	}
	return &Installer{
		Exe:      "/usr/local/bin/cox",
		UserHome: t.TempDir(),
		LookPath: func(n string) (string, error) {
			if have[n] {
				return "/bin/" + n, nil
			}
			return "", errors.New("not found")
		},
		Run: func(name string, args ...string) error {
			calls = append(calls, name+" "+strings.Join(args, " "))
			return nil
		},
	}, &calls
}

func TestClaudeAndCodex(t *testing.T) {
	i, calls := newInstaller(t, "claude", "codex")
	res := i.All()
	want := []string{
		"claude mcp remove -s user cox",
		"claude mcp add -s user cox -- /usr/local/bin/cox mcp --actor claude-code",
		"codex mcp remove cox",
		"codex mcp add cox -- /usr/local/bin/cox mcp --actor codex",
	}
	if strings.Join(*calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s", strings.Join(*calls, "\n"))
	}
	for _, r := range res[:2] {
		if r.Err != nil {
			t.Fatal(r.Err)
		}
		if b, err := os.ReadFile(r.Skill); err != nil || !strings.HasPrefix(string(b), "---\nname: cox") {
			t.Fatalf("skill %s: %v", r.Skill, err)
		}
	}
	if !strings.HasPrefix(res[2].MCP, "skipped") {
		t.Fatalf("kimi: %+v", res[2])
	}
}

func TestKimiPreservesOtherServers(t *testing.T) {
	i, _ := newInstaller(t, "kimi")
	i.KimiHome = filepath.Join(i.UserHome, ".kimi-code")
	os.MkdirAll(i.KimiHome, 0o700)
	os.WriteFile(filepath.Join(i.KimiHome, "mcp.json"), []byte(`{"mcpServers":{"other":{"command":"x"}},"keep":1}`), 0o600)

	r := i.All()[2]
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	var cfg map[string]any
	b, _ := os.ReadFile(filepath.Join(i.KimiHome, "mcp.json"))
	json.Unmarshal(b, &cfg)
	servers := cfg["mcpServers"].(map[string]any)
	cox := servers["cox"].(map[string]any)
	if servers["other"] == nil || cfg["keep"] == nil || cox["command"] != "/usr/local/bin/cox" {
		t.Fatalf("cfg = %s", b)
	}
	if _, err := os.Stat(filepath.Join(i.KimiHome, "skills", "cox", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
}

func TestKimiRefusesBrokenJSON(t *testing.T) {
	i, _ := newInstaller(t, "kimi")
	i.KimiHome = filepath.Join(i.UserHome, ".kimi-code")
	os.MkdirAll(i.KimiHome, 0o700)
	os.WriteFile(filepath.Join(i.KimiHome, "mcp.json"), []byte(`{broken`), 0o600)
	if r := i.All()[2]; r.Err == nil {
		t.Fatal("expected error")
	}
	if b, _ := os.ReadFile(filepath.Join(i.KimiHome, "mcp.json")); string(b) != "{broken" {
		t.Fatal("broken file was overwritten")
	}
}

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/hua-ri/coxswain/internal/client"
	"github.com/hua-ri/coxswain/internal/clients"
	"github.com/hua-ri/coxswain/internal/engine"
	"github.com/hua-ri/coxswain/internal/install"
	"github.com/hua-ri/coxswain/internal/instance"
	"github.com/hua-ri/coxswain/internal/server"
	"github.com/hua-ri/coxswain/internal/surface/mcpbridge"
	"github.com/spf13/cobra"
)

func mcpCommand(coxHome string) *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Connect an AI agent (over stdio) to the running Coxswain server",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !clients.ValidName(name) {
				return fmt.Errorf("--client must name the agent (claude-code, codex, kimi), got %q", name)
			}
			return mcpbridge.Run(cmd.Context(), os.Stdin, os.Stdout, coxHome, name)
		},
	}
	cmd.Flags().StringVar(&name, "client", "", "agent name: claude-code, codex or kimi")
	// v0.1 agent configs run `cox mcp --actor <name>`.
	cmd.Flags().StringVar(&name, "actor", "", "")
	cmd.Flags().MarkHidden("actor")
	return cmd
}

func clientsCommand(coxHome string) *cobra.Command {
	cmd := &cobra.Command{Use: "clients", Short: "Agents you allowed to use Coxswain"}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List allowed agents",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client.Discover(cmd.Context(), coxHome, "cli")
			if err != nil {
				return err
			}
			list, err := c.Clients(cmd.Context())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(list) == 0 {
				fmt.Fprintln(out, "no agents allowed yet")
			}
			for _, cl := range list {
				fmt.Fprintf(out, "%-16s allowed %s\n", cl.Name, cl.AuthorizedAt.Local().Format(time.DateTime))
			}
			return nil
		},
	}, &cobra.Command{
		Use:   "revoke <name>",
		Short: "Withdraw an agent's access and disconnect it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client.Discover(cmd.Context(), coxHome, "cli")
			if err != nil {
				return err
			}
			if err := c.Revoke(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "revoked %s\n", args[0])
			return nil
		},
	})
	return cmd
}

func versionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  cobra.NoArgs,
		Run:   func(cmd *cobra.Command, _ []string) { fmt.Fprintln(cmd.OutOrStdout(), version) },
	}
}

func initCommand(coxHome, userHome string) *cobra.Command {
	var skipImport, skipAgents bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Import ~/.ssh/config and connect Claude Code, Codex and Kimi Code",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			if !skipImport {
				if _, err := os.Stat(filepath.Join(userHome, ".ssh", "config")); err == nil {
					res, err := importHosts(cmd, coxHome, userHome)
					if err != nil {
						return fmt.Errorf("import ~/.ssh/config: %w", err)
					}
					fmt.Fprintf(out, "hosts: imported %s (added %d, updated %d)\n", res.Path, res.Added, res.Updated)
				}
			}
			if skipAgents {
				return nil
			}
			exe, err := exePath()
			if err != nil {
				return err
			}
			i := &install.Installer{
				Exe: exe, UserHome: userHome, KimiHome: os.Getenv("KIMI_CODE_HOME"),
				LookPath: exec.LookPath,
				Run: func(name string, args ...string) error {
					c := exec.CommandContext(cmd.Context(), name, args...)
					if b, err := c.CombinedOutput(); err != nil {
						return fmt.Errorf("%s %v: %w: %s", name, args, err, b)
					}
					return nil
				},
			}
			failed := false
			for _, r := range i.All() {
				switch {
				case r.Err != nil:
					failed = true
					fmt.Fprintf(out, "%-12s error: %v\n", r.Agent, r.Err)
				case r.Skill != "":
					fmt.Fprintf(out, "%-12s MCP %s; skill %s\n", r.Agent, r.MCP, r.Skill)
				default:
					fmt.Fprintf(out, "%-12s %s\n", r.Agent, r.MCP)
				}
			}
			if failed {
				return errors.New("some agents were not set up")
			}
			fmt.Fprintln(out, "done. Start the server with `cox server`, then restart your agents to load the cox tools.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&skipImport, "skip-import", false, "do not import ~/.ssh/config")
	cmd.Flags().BoolVar(&skipAgents, "skip-agents", false, "do not configure AI agents")
	return cmd
}

type importResult struct {
	Path    string `json:"path"`
	Added   int    `json:"added"`
	Updated int    `json:"updated"`
}

// importHosts imports ~/.ssh/config through the running server, or with a
// local engine when none runs (init comes before the first `cox server`).
func importHosts(cmd *cobra.Command, coxHome, userHome string) (importResult, error) {
	var res importResult
	var raw []byte
	c, err := client.Discover(cmd.Context(), coxHome, "cli")
	switch {
	case err == nil:
		out, err := c.Invoke(cmd.Context(), "ssh.hosts.import", nil)
		if err != nil {
			return res, err
		}
		if out.Status != engine.StatusOK {
			return res, errors.New(out.Error)
		}
		raw = out.Result
	case errors.Is(err, instance.ErrNotRunning):
		out := newEngine(coxHome, userHome, nil).Invoke(cmd.Context(), server.Actor("cli"), "ssh.hosts.import", nil)
		if out.Status != engine.StatusOK {
			return res, errors.New(out.Error)
		}
		if raw, err = json.Marshal(out.Result); err != nil {
			return res, err
		}
	default:
		return res, err
	}
	return res, json.Unmarshal(raw, &res)
}

// exePath prefers the PATH entry for cox (e.g. /opt/homebrew/bin/cox) over
// the resolved binary, so agent configs survive `brew upgrade`.
func exePath() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	if p, err := exec.LookPath("cox"); err == nil {
		a, errA := os.Stat(p)
		b, errB := os.Stat(self)
		if errA == nil && errB == nil && os.SameFile(a, b) {
			if abs, err := filepath.Abs(p); err == nil {
				return abs, nil
			}
		}
	}
	return self, nil
}

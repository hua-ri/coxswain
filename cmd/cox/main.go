// Command cox is Coxswain: an ops toolbox for people and AI agents.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/actor"
	"github.com/hua-ri/coxswain/internal/approval"
	"github.com/hua-ri/coxswain/internal/confirm"
	"github.com/hua-ri/coxswain/internal/engine"
	"github.com/hua-ri/coxswain/internal/eventlog"
	"github.com/hua-ri/coxswain/internal/install"
	"github.com/hua-ri/coxswain/internal/inventory"
	"github.com/hua-ri/coxswain/internal/providers/artifact"
	sshprov "github.com/hua-ri/coxswain/internal/providers/ssh"
	"github.com/hua-ri/coxswain/internal/shaper"
	"github.com/hua-ri/coxswain/internal/sshx"
	"github.com/hua-ri/coxswain/internal/surface/cli"
	mcpsurf "github.com/hua-ri/coxswain/internal/surface/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// version is set at release time with -ldflags "-X main.version=…".
var version = "dev"

func main() { os.Exit(run()) }

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	userHome, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return cli.ExitFailure
	}
	coxHome := os.Getenv("COX_HOME")
	if coxHome == "" {
		coxHome = filepath.Join(userHome, ".coxswain")
	}
	e := newEngine(coxHome, userHome)

	tty := term.IsTerminal(int(os.Stdin.Fd()))
	who := actor.Detect(tty)
	var ask engine.Confirmer
	switch {
	case who.Kind == action.Agent:
		ask = dialog()
	case tty:
		ask = confirm.Terminal(os.Stdin, os.Stderr)
	}
	var approve engine.Confirmer
	if os.Getenv("COX_NO_DIALOG") == "" {
		approve = confirm.Dialog(120 * time.Second)
	}
	app := &cli.App{
		Engine: e, Actor: who, Confirm: ask, Approve: approve, HumanTTY: who.Kind == action.Human && tty,
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
	}
	root := cli.NewRoot(app, version, mcpCommand(e), initCommand(app, userHome))
	err = root.ExecuteContext(ctx)
	var exit cli.ExitError
	if err != nil && !errors.As(err, &exit) {
		fmt.Fprintln(os.Stderr, "error:", err)
	}
	return cli.Code(err)
}

func newEngine(coxHome, userHome string) *engine.Engine {
	hosts := inventory.NewStore(filepath.Join(coxHome, "hosts.json"))
	artifacts := shaper.NewArtifacts(filepath.Join(coxHome, "artifacts"))
	ssh := filepath.Join(userHome, ".ssh")
	reg := action.NewRegistry()
	(&sshprov.Provider{
		Hosts: hosts,
		Dialer: &sshx.Dialer{
			Hosts:       hosts,
			KnownHosts:  []string{filepath.Join(ssh, "known_hosts"), filepath.Join(ssh, "known_hosts2")},
			DefaultKeys: []string{filepath.Join(ssh, "id_ed25519"), filepath.Join(ssh, "id_ecdsa"), filepath.Join(ssh, "id_rsa")},
			AgentSocket: os.Getenv("SSH_AUTH_SOCK"),
		},
		Artifacts: artifacts,
		UserHome:  userHome,
	}).Register(reg)
	artifact.Register(reg, artifacts)
	return &engine.Engine{
		Registry:  reg,
		Approvals: approval.NewStore(filepath.Join(coxHome, "approvals"), 30*time.Minute),
		Events:    eventlog.Open(filepath.Join(coxHome, "events")),
	}
}

// dialog returns the macOS approval dialog, or nil when COX_NO_DIALOG is
// set (tests, CI). Codex times tool calls out after 60s by default.
func dialog() engine.Confirmer {
	if os.Getenv("COX_NO_DIALOG") != "" {
		return nil
	}
	return confirm.Dialog(45 * time.Second)
}

func mcpCommand(e *engine.Engine) *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Serve Coxswain tools to an AI agent over MCP (stdio)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var b [4]byte
			rand.Read(b[:])
			who := action.Actor{Kind: action.Agent, Name: name, Session: hex.EncodeToString(b[:])}
			return mcpsurf.NewServer(e, who, dialog(), version).Run(cmd.Context(), &sdk.StdioTransport{})
		},
	}
	cmd.Flags().StringVar(&name, "actor", "mcp", "agent name recorded in the audit log (claude-code, codex, kimi)")
	return cmd
}

func initCommand(app *cli.App, userHome string) *cobra.Command {
	var skipImport, skipAgents bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Import ~/.ssh/config and connect Claude Code, Codex and Kimi Code",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			if !skipImport {
				if _, err := os.Stat(filepath.Join(userHome, ".ssh", "config")); err == nil {
					res := app.Engine.Invoke(cmd.Context(), app.Actor, "ssh.hosts.import", nil, app.Confirm)
					if res.Status != engine.StatusOK {
						return fmt.Errorf("import ~/.ssh/config: %s %s", res.Status, res.Error)
					}
					fmt.Fprintf(out, "hosts: imported ~/.ssh/config %v\n", res.Result)
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
			fmt.Fprintln(out, "done. Restart your agents to load the cox tools.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&skipImport, "skip-import", false, "do not import ~/.ssh/config")
	cmd.Flags().BoolVar(&skipAgents, "skip-agents", false, "do not configure AI agents")
	return cmd
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

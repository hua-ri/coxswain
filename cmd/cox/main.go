// Command cox is Coxswain: an ops toolbox for people and AI agents.
// `cox server` holds all state; every other command is a client of it.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"syscall"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/client"
	"github.com/hua-ri/coxswain/internal/engine"
	"github.com/hua-ri/coxswain/internal/eventlog"
	"github.com/hua-ri/coxswain/internal/instance"
	"github.com/hua-ri/coxswain/internal/inventory"
	"github.com/hua-ri/coxswain/internal/providers/artifact"
	sshprov "github.com/hua-ri/coxswain/internal/providers/ssh"
	"github.com/hua-ri/coxswain/internal/shaper"
	"github.com/hua-ri/coxswain/internal/sshx"
	"github.com/hua-ri/coxswain/internal/surface/cli"
)

// version is set at release time with -ldflags "-X main.version=…".
var version = "dev"

// localCommands work without a running server.
var localCommands = []string{"server", "mcp", "init", "clients", "version", "help", "completion"}

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

	app := &cli.App{
		Printers: map[string]cli.Decoder{"ssh.exec": cli.PrintAs[sshprov.ExecOutput]},
		Stdin:    os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
	}
	if name := commandName(os.Args[1:]); !slices.Contains(localCommands, name) {
		// Action commands come from the server; without one, only the
		// local commands exist.
		c, err := client.Discover(ctx, coxHome, "cli")
		if err == nil {
			app.Client = c
			app.Actions, err = c.Actions(ctx)
		}
		if err != nil && name != "" {
			return report(err)
		}
	}
	root := cli.NewRoot(app, version,
		serverCommand(coxHome, userHome),
		mcpCommand(coxHome),
		initCommand(coxHome, userHome),
		clientsCommand(coxHome),
		versionCommand(),
	)
	return report(root.ExecuteContext(ctx))
}

// report prints err (unless a command already did) and returns the exit code.
func report(err error) int {
	var exit cli.ExitError
	switch {
	case err == nil, errors.As(err, &exit):
	case errors.Is(err, instance.ErrNotRunning):
		fmt.Fprintln(os.Stderr, instance.ErrNotRunning.Error())
	default:
		fmt.Fprintln(os.Stderr, "error:", err)
	}
	return cli.Code(err)
}

// commandName is the first argument that is not a flag (skipping the
// value of -o/--output), or "" if there is none.
func commandName(args []string) string {
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-o" || a == "--output":
			i++
		case len(a) > 0 && a[0] == '-':
		default:
			return a
		}
	}
	return ""
}

// newEngine builds the engine with every provider. pool may be nil.
func newEngine(coxHome, userHome string, pool func(*sshx.Dialer) *sshx.Pool) *engine.Engine {
	hosts := inventory.NewStore(filepath.Join(coxHome, "hosts.json"))
	artifacts := shaper.NewArtifacts(filepath.Join(coxHome, "artifacts"))
	ssh := filepath.Join(userHome, ".ssh")
	dialer := &sshx.Dialer{
		Hosts:       hosts,
		KnownHosts:  []string{filepath.Join(ssh, "known_hosts"), filepath.Join(ssh, "known_hosts2")},
		DefaultKeys: []string{filepath.Join(ssh, "id_ed25519"), filepath.Join(ssh, "id_ecdsa"), filepath.Join(ssh, "id_rsa")},
		AgentSocket: os.Getenv("SSH_AUTH_SOCK"),
	}
	p := &sshprov.Provider{Hosts: hosts, Dialer: dialer, Artifacts: artifacts, UserHome: userHome}
	if pool != nil {
		p.Pool = pool(dialer)
	}
	reg := action.NewRegistry()
	p.Register(reg)
	artifact.Register(reg, artifacts)
	return &engine.Engine{Registry: reg, Events: eventlog.Open(filepath.Join(coxHome, "events"))}
}

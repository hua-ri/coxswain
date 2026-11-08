// Command cox is Coxswain: an ops toolbox for people and AI agents.
// `cox server` holds all state; every other command is a client of it.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/actor"
	"github.com/hua-ri/coxswain/internal/client"
	"github.com/hua-ri/coxswain/internal/engine"
	"github.com/hua-ri/coxswain/internal/eventlog"
	"github.com/hua-ri/coxswain/internal/instance"
	"github.com/hua-ri/coxswain/internal/inventory"
	"github.com/hua-ri/coxswain/internal/providers/artifact"
	"github.com/hua-ri/coxswain/internal/providers/file"
	sshprov "github.com/hua-ri/coxswain/internal/providers/ssh"
	"github.com/hua-ri/coxswain/internal/providers/terminal"
	"github.com/hua-ri/coxswain/internal/shaper"
	"github.com/hua-ri/coxswain/internal/sshx"
	"github.com/hua-ri/coxswain/internal/surface/cli"
	"github.com/hua-ri/coxswain/internal/term"
	"github.com/hua-ri/coxswain/internal/transfer"
)

// version is set at release time with -ldflags "-X main.version=…".
var version = "dev"

// agentAncestors turns on recognising agents by their processes. The
// end-to-end tests build with -X main.agentAncestors=off because they run
// under whatever agent runs the tests. (A link-time switch, not an
// environment variable, so agents cannot turn it off.)
var agentAncestors = "on"

// clientName is who this cox process is to the server.
func clientName() string { return actor.Client(actor.StdinTTY(), agentAncestors == "on") }

// localCommands work without a running server.
var localCommands = []string{"server", "mcp", "init", "clients", "desktop", "version", "help", "completion"}

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
		Printers: map[string]cli.Decoder{
			"ssh.exec":      cli.PrintAs[sshprov.ExecOutput],
			"terminal.read": cli.PrintAs[terminal.ReadOutput],
			"file.read":     cli.PrintAs[file.ReadOutput],
		},
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
	}
	if name := commandName(os.Args[1:]); !slices.Contains(localCommands, name) {
		// Action commands come from the server; without one, only the
		// local commands exist.
		c, err := client.Discover(ctx, coxHome, clientName())
		if err == nil {
			c.Session = sessionID(rand.Read)
			app.Client = c
			app.Actions, err = c.Actions(ctx)
		}
		if err != nil && name != "" {
			return report(err)
		}
	}
	root := cli.NewRoot(app, version,
		serverCommand(coxHome, userHome),
		mcpCommand(coxHome, userHome),
		initCommand(coxHome, userHome),
		clientsCommand(coxHome),
		desktopCommand(coxHome),
		versionCommand(),
	)
	return report(root.ExecuteContext(ctx))
}

// sessionID returns a random id for one cox process, or a timestamp-based
// one if the random source fails.
func sessionID(read func([]byte) (int, error)) string {
	var b [4]byte
	if _, err := read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
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

// newRegistry returns every action, e.g. to offer the tools while the
// server is down.
func newRegistry(coxHome, userHome string) *action.Registry {
	return newEngine(coxHome, userHome, nil).Engine.Registry
}

// services are the engine and the stateful things the server shares.
type services struct {
	Engine    *engine.Engine
	Terminals *term.Manager
	Transfers *transfer.Manager
}

// newEngine builds the engine with every provider, and the terminal
// sessions and transfers the server shares. pool may be nil.
func newEngine(coxHome, userHome string, pool func(*sshx.Dialer) *sshx.Pool) services {
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
	events := eventlog.Open(filepath.Join(coxHome, "events"))
	terms := &term.Manager{Hosts: hosts, Dialer: dialer, Events: events}
	reg := action.NewRegistry()
	p.Register(reg)
	artifact.Register(reg, artifacts)
	(&terminal.Provider{Hosts: hosts, Terms: terms, Artifacts: artifacts}).Register(reg)
	transfers := &transfer.Manager{Events: events}
	(&file.Provider{Hosts: hosts, Dialer: dialer, Pool: p.Pool, Artifacts: artifacts, Transfers: transfers,
		DownloadDir: filepath.Join(coxHome, "downloads"), UserHome: userHome}).Register(reg)
	return services{Engine: &engine.Engine{Registry: reg, Events: events}, Terminals: terms, Transfers: transfers}
}

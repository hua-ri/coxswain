package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hua-ri/coxswain/internal/clients"
	"github.com/hua-ri/coxswain/internal/confirm"
	"github.com/hua-ri/coxswain/internal/eventlog"
	"github.com/hua-ri/coxswain/internal/instance"
	"github.com/hua-ri/coxswain/internal/server"
	"github.com/hua-ri/coxswain/internal/sshx"
	"github.com/spf13/cobra"
)

// authorizeTimeout is how long the "allow this client?" dialog waits.
const authorizeTimeout = 60 * time.Second

func serverCommand(coxHome, userHome string) *cobra.Command {
	var port int
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Run the Coxswain server in this terminal (Ctrl-C stops it)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var pool *sshx.Pool
			svc := newEngine(coxHome, userHome, func(d *sshx.Dialer) *sshx.Pool {
				pool = &sshx.Pool{Dialer: d}
				return pool
			})
			e := svc.Engine
			defer pool.Close()

			events, stop := e.Events.Subscribe()
			defer stop()
			printed := make(chan struct{})
			go func() { printEvents(os.Stderr, events); close(printed) }()

			err := server.Run(cmd.Context(), server.Config{
				Engine:    e,
				Terminals: svc.Terminals,
				Transfers: svc.Transfers,
				Tunnels:   svc.Tunnels,
				Clients: &clients.Authorizer{
					Store:  clients.NewStore(filepath.Join(coxHome, "clients.json")),
					Ask:    clients.Dialog(authorizeTimeout),
					Events: e.Events,
				},
				Version: version,
				Port:    port,
				Ready: func(info instance.Info) {
					fmt.Fprintf(os.Stderr, "Coxswain %s listening on %s (pid %d). Press Ctrl-C to stop.\n",
						info.Version, info.Addr, info.PID)
				},
			}, coxHome)
			stop()
			<-printed
			if err == nil {
				fmt.Fprintln(os.Stderr, "Coxswain server stopped.")
			}
			return err
		},
	}
	cmd.Flags().IntVar(&port, "port", 0, "port on 127.0.0.1 (0: the previous one if free, else any)")
	return cmd
}

// printEvents writes one line per audit event until the channel closes.
// Transfer progress is left out.
func printEvents(w io.Writer, events <-chan eventlog.Event) {
	for ev := range events {
		if ev.Type == eventlog.TransferProgress {
			continue
		}
		fmt.Fprintln(w, eventLine(ev))
	}
}

// eventLine renders an event, e.g.
// "15:04:05 agent:codex ssh.exec requested destructive dev {"command":"…"}".
func eventLine(ev eventlog.Event) string {
	parts := []string{ev.Time.Local().Format("15:04:05")}
	if c, ok := strings.CutPrefix(ev.Type, "client."); ok {
		parts = append(parts, "client", ev.Client, c)
		return confirm.Sanitize(strings.Join(parts, " "))
	}
	if ev.Actor != nil {
		parts = append(parts, ev.Actor.String())
	}
	if kind, phase, ok := strings.Cut(ev.Type, "."); ok && kind != "action" {
		id := map[string]string{"terminal": ev.Terminal, "transfer": ev.Transfer, "tunnel": ev.Tunnel, "batch": ev.Batch}[kind]
		parts = append(parts, kind, id, phase)
		if len(ev.Targets) > 3 {
			parts = append(parts, fmt.Sprintf("%d hosts", len(ev.Targets)))
		} else {
			for _, tg := range ev.Targets {
				parts = append(parts, tg.ID)
			}
		}
		if ev.Reason != "" {
			parts = append(parts, ev.Reason)
		}
		if ev.ExitCode != nil {
			parts = append(parts, fmt.Sprintf("(exit code %d)", *ev.ExitCode))
		}
		line := strings.Join(parts, " ")
		if ev.Error != "" {
			line += ": " + ev.Error
		}
		return confirm.Sanitize(line)
	}
	parts = append(parts, ev.Action)
	phase := strings.TrimPrefix(ev.Type, "action.")
	switch ev.Type {
	case eventlog.ActionRequested:
		parts = append(parts, phase, ev.Risk)
		for _, t := range ev.Targets {
			parts = append(parts, t.ID)
		}
		if a := string(ev.Args); a != "" && a != "{}" {
			if len(a) > 200 {
				a = a[:200] + "…"
			}
			parts = append(parts, a)
		}
	case eventlog.ActionRefused:
		parts = append(parts, phase+": "+ev.Reason)
	case eventlog.ActionFailed:
		parts = append(parts, phase, fmt.Sprintf("%dms: %s", ev.DurationMs, ev.Error))
	default:
		parts = append(parts, phase, fmt.Sprintf("%dms", ev.DurationMs))
	}
	return confirm.Sanitize(strings.Join(parts, " "))
}

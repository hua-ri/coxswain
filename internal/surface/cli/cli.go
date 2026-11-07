// Package cli builds the `cox` command tree from the running server's
// action list (ssh.exec → `cox ssh exec`) and invokes actions through it.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/hua-ri/coxswain/internal/client"
	"github.com/hua-ri/coxswain/internal/instance"
	"github.com/hua-ri/coxswain/internal/server"
	"github.com/spf13/cobra"
)

// Exit codes.
const (
	ExitOK         = 0
	ExitFailure    = 1
	ExitNotRunning = 2 // no server to talk to
)

// ExitError carries a process exit code out of a command.
type ExitError struct{ Code int }

func (e ExitError) Error() string { return fmt.Sprintf("exit %d", e.Code) }

// Printer lets a result render itself on a terminal and choose the exit
// code (ssh.exec prints remote output and returns the remote exit code).
type Printer interface {
	PrintCLI(stdout, stderr io.Writer) int
}

// Decoder turns an action's JSON result into a Printer.
type Decoder func(json.RawMessage) (Printer, error)

// PrintAs is a Decoder for results of type T.
func PrintAs[T Printer](raw json.RawMessage) (Printer, error) {
	var v T
	err := json.Unmarshal(raw, &v)
	return v, err
}

// Invoker runs an action on the server.
type Invoker interface {
	Invoke(ctx context.Context, id string, args json.RawMessage) (client.Outcome, error)
}

type App struct {
	Actions  []server.ActionInfo
	Client   Invoker
	Printers map[string]Decoder // by action ID
	Stdin    io.Reader
	Stdout   io.Writer
	Stderr   io.Writer
	output   string
}

// NewRoot returns the root command with one command per action plus any
// extra (local) commands.
func NewRoot(app *App, version string, extra ...*cobra.Command) *cobra.Command {
	root := &cobra.Command{
		Use:           "cox",
		Short:         "Coxswain: an ops toolbox for you and your AI agents",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVarP(&app.output, "output", "o", "text", "output format: text or json")
	root.SetIn(app.Stdin)
	root.SetOut(app.Stdout)
	root.SetErr(app.Stderr)

	for _, a := range app.Actions {
		parent := root
		parts := strings.Split(a.ID, ".")
		for _, name := range parts[:len(parts)-1] {
			parent = child(parent, name)
		}
		parent.AddCommand(actionCommand(app, a, parts[len(parts)-1]))
	}
	root.AddCommand(extra...)
	return root
}

// child returns parent's group command called name, creating it.
func child(parent *cobra.Command, name string) *cobra.Command {
	for _, c := range parent.Commands() {
		if c.Name() == name {
			return c
		}
	}
	c := &cobra.Command{Use: name, Short: name + " commands"}
	parent.AddCommand(c)
	return c
}

func actionCommand(app *App, a server.ActionInfo, name string) *cobra.Command {
	props, _ := a.Input["properties"].(map[string]any)
	use := name
	for _, p := range a.Positional {
		use += " <" + p + ">"
	}
	cmd := &cobra.Command{Use: use, Short: a.Summary, Long: a.Description, Args: cobra.ArbitraryArgs}
	if a.RestArgs {
		cmd.Flags().SetInterspersed(false) // `cox ssh exec host df -h`: -h belongs to df
	}

	type flagVal struct {
		prop string
		get  func() any
	}
	var flags []flagVal
	names := make([]string, 0, len(props))
	for k := range props {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, prop := range names {
		schema, _ := props[prop].(map[string]any)
		desc, _ := schema["description"].(string)
		flag := strings.ReplaceAll(prop, "_", "-")
		switch schema["type"] {
		case "integer":
			v := cmd.Flags().Int(flag, 0, desc)
			flags = append(flags, flagVal{prop, func() any { return *v }})
		case "boolean":
			v := cmd.Flags().Bool(flag, false, desc)
			flags = append(flags, flagVal{prop, func() any { return *v }})
		case "array":
			v := cmd.Flags().StringArray(flag, nil, desc)
			flags = append(flags, flagVal{prop, func() any { return *v }})
		default:
			v := cmd.Flags().String(flag, "", desc)
			flags = append(flags, flagVal{prop, func() any { return *v }})
		}
	}

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		in := map[string]any{}
		for _, f := range flags {
			if cmd.Flags().Changed(strings.ReplaceAll(f.prop, "_", "-")) {
				in[f.prop] = f.get()
			}
		}
		if err := mapPositional(a.Positional, a.RestArgs, args, in); err != nil {
			return err
		}
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		out, err := app.Client.Invoke(cmd.Context(), a.ID, raw)
		if err != nil {
			return app.fail(err)
		}
		return app.render(a.ID, out)
	}
	return cmd
}

// mapPositional assigns args to the Positional properties. With rest,
// the last property collects every remaining argument, joined by spaces.
func mapPositional(names []string, rest bool, args []string, in map[string]any) error {
	for i, arg := range args {
		switch {
		case rest && i == len(names)-1:
			restArgs := args[i:]
			if restArgs[0] == "--" { // flag parsing already stopped, so `--` is still here
				restArgs = restArgs[1:]
			}
			in[names[i]] = strings.Join(restArgs, " ")
			return nil
		case i >= len(names):
			return fmt.Errorf("unexpected arguments: %v", args[i:])
		}
		in[names[i]] = arg
	}
	return nil
}

// fail reports an error talking to the server.
func (app *App) fail(err error) error {
	if errors.Is(err, instance.ErrNotRunning) {
		fmt.Fprintln(app.Stderr, instance.ErrNotRunning.Error())
		return ExitError{ExitNotRunning}
	}
	return err
}

func (app *App) render(id string, out client.Outcome) error {
	var p Printer
	if dec := app.Printers[id]; dec != nil && out.Status == "ok" {
		var err error
		if p, err = dec(out.Result); err != nil {
			return fmt.Errorf("decode %s result: %w", id, err)
		}
	}
	if app.output == "json" {
		enc := json.NewEncoder(app.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(out)
		return ExitError{exitCode(out, p)}
	}
	switch {
	case out.Status != "ok":
		fmt.Fprintln(app.Stderr, "error: "+out.Error)
		if out.Hint != "" {
			fmt.Fprintln(app.Stderr, out.Hint)
		}
	case p != nil:
		return ExitError{p.PrintCLI(app.Stdout, app.Stderr)}
	default:
		var s string
		if json.Unmarshal(out.Result, &s) == nil {
			fmt.Fprintln(app.Stdout, s)
			break
		}
		var buf bytes.Buffer
		if json.Indent(&buf, out.Result, "", "  ") != nil {
			buf.Reset()
			buf.Write(out.Result)
		}
		fmt.Fprintln(app.Stdout, buf.String())
	}
	return ExitError{exitCode(out, p)}
}

func exitCode(out client.Outcome, p Printer) int {
	switch {
	case out.Status != "ok":
		return ExitFailure
	case p != nil:
		return p.PrintCLI(io.Discard, io.Discard)
	}
	return ExitOK
}

// Code extracts the exit code for main.
func Code(err error) int {
	if err == nil {
		return ExitOK
	}
	var e ExitError
	if errors.As(err, &e) {
		return e.Code
	}
	if errors.Is(err, instance.ErrNotRunning) {
		return ExitNotRunning
	}
	return ExitFailure
}

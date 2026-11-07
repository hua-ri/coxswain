// Package cli builds the `cox` command tree. Every registered Action
// becomes a command (ssh.exec → `cox ssh exec`).
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/engine"
	"github.com/spf13/cobra"
)

// Exit codes.
const (
	ExitOK      = 0
	ExitFailure = 1
)

// ExitError carries a process exit code out of a command.
type ExitError struct{ Code int }

func (e ExitError) Error() string { return fmt.Sprintf("exit %d", e.Code) }

// Printer lets a result render itself on a terminal and choose the exit
// code (ssh.exec prints remote output and returns the remote exit code).
type Printer interface {
	PrintCLI(stdout, stderr io.Writer) int
}

type App struct {
	Engine *engine.Engine
	Actor  action.Actor
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	output string
}

// NewRoot returns the root command with one command per Action plus any
// extra commands.
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

	for _, a := range app.Engine.Registry.List() {
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

func actionCommand(app *App, a *action.Action, name string) *cobra.Command {
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
		out := app.Engine.Invoke(cmd.Context(), app.Actor, a.ID, raw)
		return app.render(out)
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

func (app *App) render(out engine.Outcome) error {
	if app.output == "json" {
		enc := json.NewEncoder(app.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(out)
		return ExitError{exitCode(out)}
	}
	switch out.Status {
	case engine.StatusOK:
		switch r := out.Result.(type) {
		case Printer:
			return ExitError{r.PrintCLI(app.Stdout, app.Stderr)}
		case string:
			fmt.Fprintln(app.Stdout, r)
		default:
			enc := json.NewEncoder(app.Stdout)
			enc.SetIndent("", "  ")
			enc.Encode(r)
		}
	default:
		fmt.Fprintln(app.Stderr, "error: "+out.Error)
	}
	return ExitError{exitCode(out)}
}

func exitCode(out engine.Outcome) int {
	switch out.Status {
	case engine.StatusOK:
		if p, ok := out.Result.(Printer); ok {
			return p.PrintCLI(io.Discard, io.Discard)
		}
		return ExitOK
	}
	return ExitFailure
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
	return ExitFailure
}

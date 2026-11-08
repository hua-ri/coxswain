package main

import (
	"runtime"

	"github.com/hua-ri/coxswain/internal/desktop"
	"github.com/spf13/cobra"
)

// The desktop window must run on the main thread: keep the main
// goroutine there.
func init() { runtime.LockOSThread() }

func desktopCommand(coxHome string) *cobra.Command {
	var debug bool
	cmd := &cobra.Command{
		Use:   "desktop",
		Short: "Open the desktop app on the running Coxswain server",
		Long: "Open the desktop app: hosts, live activity of you and your agents, and shared terminals.\n" +
			"It connects to the running `cox server`; closing the window leaves the server running.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return desktop.Run(cmd.Context(), coxHome, debug)
		},
	}
	cmd.Flags().BoolVar(&debug, "debug", false, "enable the web inspector")
	cmd.Flags().MarkHidden("debug")
	return cmd
}

package cli

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/approval"
	"github.com/hua-ri/coxswain/internal/confirm"
	"github.com/hua-ri/coxswain/internal/engine"
	"github.com/spf13/cobra"
)

// errNotHuman is returned when an agent (or a script) tries to decide an
// approval. Agents must never approve their own actions.
var errNotHuman = errors.New("approvals can only be decided by a person at an interactive terminal")

// errNoDialog is returned by `cox approve` when no approval dialog is
// available (COX_NO_DIALOG is set).
var errNoDialog = errors.New("cox approve needs the macOS approval dialog, which is disabled (COX_NO_DIALOG)")

func approvalCommands(app *App) []*cobra.Command {
	var all bool
	list := &cobra.Command{
		Use:   "approvals",
		Short: "List approvals waiting for your decision",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			aps, err := app.Engine.Approvals.List()
			if err != nil {
				return err
			}
			shown := 0
			for _, ap := range aps {
				if !all && ap.Status != approval.Pending {
					continue
				}
				shown++
				fmt.Fprintf(app.Stdout, "%s  %-9s %-8s %-11s %s\n    %s\n",
					ap.ID, ap.Status, confirm.Sanitize(ap.Actor.Name), ap.Risk, ap.CreatedAt.Format("15:04:05"),
					confirm.Sanitize(ap.Preview))
			}
			if shown == 0 {
				fmt.Fprintln(app.Stdout, "no pending approvals")
			}
			return nil
		},
	}
	list.Flags().BoolVar(&all, "all", false, "include decided and expired approvals")

	var timeout time.Duration
	wait := &cobra.Command{
		Use:   "wait <approval-id>",
		Short: "Wait for a decision, then run the approved action",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := app.Engine.Wait(cmd.Context(), args[0], timeout, 500*time.Millisecond); err != nil {
				return err
			}
			return app.render(app.Engine.RunApproved(cmd.Context(), app.Actor, args[0]))
		},
	}
	wait.Flags().DurationVar(&timeout, "timeout", 2*time.Minute, "how long to wait")
	list.AddCommand(wait)

	// Approving needs a click in the macOS dialog: a typed "y" could come
	// from an agent driving a pseudo-terminal. Denying is always safe.
	decide := func(approve bool) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, args []string) error {
			if app.Actor.Kind != action.Human || !app.HumanTTY || (approve && os.Getenv("COX_ACTOR") != "") {
				return errNotHuman
			}
			ap, err := app.Engine.Approvals.Get(args[0])
			if err != nil {
				return err
			}
			if ap.Status != approval.Pending {
				return fmt.Errorf("approval %s is %s", ap.ID, ap.Status)
			}
			by := app.Actor
			if approve {
				if app.Approve == nil {
					return errNoDialog
				}
				fmt.Fprintf(app.Stdout, "%s\n\nApprove or deny in the dialog…\n", confirm.Message(ap))
				var verdict engine.Verdict
				verdict, by = app.Approve(cmd.Context(), ap)
				switch verdict {
				case engine.Undecided:
					fmt.Fprintln(app.Stdout, "not approved")
					return nil
				case engine.Denied:
					approve = false
				}
			}
			if _, err := app.Engine.Decide(ap.ID, approve, by); err != nil {
				return err
			}
			verb := map[bool]string{true: "approved", false: "denied"}[approve]
			fmt.Fprintf(app.Stdout, "%s %s; the waiting agent will continue\n", verb, ap.ID)
			return nil
		}
	}
	return []*cobra.Command{
		list,
		{Use: "approve <approval-id>", Short: "Approve a pending action", Args: cobra.ExactArgs(1), RunE: decide(true)},
		{Use: "deny <approval-id>", Short: "Deny a pending action", Args: cobra.ExactArgs(1), RunE: decide(false)},
	}
}

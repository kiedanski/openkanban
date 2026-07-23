package cmd

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/project"
)

var (
	ticketMoveID      string
	ticketMoveProject string
	ticketMoveStatus  string
	ticketMoveForce   bool
)

var ticketMoveCmd = &cobra.Command{
	Use:   "move",
	Short: "Move a ticket to an arbitrary status",
	Long: `Move an existing ticket to any status:
backlog, next, in_progress, in_review, done, or archived.

Selects the ticket by --id (exact UUID, unique UUID prefix, or unique
title slug) within the project named by --project.

When moving a ticket out of in_progress, the live daemon session for
the ticket is terminated (best-effort) so the 1:1 ticket↔session
invariant is preserved. Moving to in_review or done also marks the
agent status as completed.

This command does NOT autostart the daemon: a scripted invocation
must remain quiet when the daemon happens to be down.

When run from inside a spawned agent session (OPENKANBAN_SESSION or
OPENKANBAN_TICKET_ID is set), this command is refused unless --force is
passed. Tickets are advanced by the user after reviewing agent work,
not by the agent itself.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if ticketMoveProject == "" {
			return fmt.Errorf("--project is required")
		}
		if ticketMoveID == "" {
			return fmt.Errorf("--id is required")
		}
		if ticketMoveStatus == "" {
			return fmt.Errorf("--status is required")
		}

		target, err := board.ParseStatus(ticketMoveStatus)
		if err != nil {
			return fmt.Errorf("--status %s", err)
		}

		registry, err := project.LoadRegistry()
		if err != nil {
			return fmt.Errorf("load project registry: %w", err)
		}
		proj, err := resolveProject(registry, ticketMoveProject)
		if err != nil {
			return err
		}

		state := project.MigrationStateFor(proj.ID)
		if state == project.MigrationPending {
			return fmt.Errorf("project %q (%s) has legacy single-file ticket "+
				"storage; launch openkanban once to migrate it before moving",
				proj.Name, shortID(proj.ID))
		}

		store, err := project.LoadTicketStore(proj)
		if err != nil {
			return fmt.Errorf("load ticket store: %w", err)
		}
		ticket, err := resolveTicket(store, registry, ticketMoveID)
		if err != nil {
			return err
		}

		// Idempotency: avoid restamping timestamps on a no-op move.
		if ticket.Status == target {
			fmt.Printf("ticket %s already %s\n", shortID(string(ticket.ID)), target)
			return nil
		}

		if err := guardAgentStatusChange(target, ticketMoveForce); err != nil {
			return err
		}

		// Workflow prerequisite gate: an implement/review ticket can't START
		// (move to in_progress) until its upstream is ready — an implement
		// needs a done spec, a review needs an implement in in_review+. This
		// is an advisory PRACTICE gate, always overridable with --force.
		if target == board.StatusInProgress {
			if err := checkStartPrerequisite(registry, ticket, ticketMoveForce); err != nil {
				return err
			}
		}

		current := ticket.Status

		promoted, pruned, err := store.Move(ticket.ID, target)
		if err != nil {
			return fmt.Errorf("move ticket %s: %w", ticket.ID, err)
		}

		// AgentStatus gating — independent of daemon teardown.
		// Sticky-terminal guard: AgentCompleted is dropped by later live
		// events, so only stamp it for genuinely terminal destinations.
		// Re-queue resets to idle so a re-spawned card doesn't falsely
		// render as completed.
		switch {
		case target == board.StatusInReview || target == board.StatusDone:
			ticket.SetAgentStatus(board.AgentCompleted)
		case current == board.StatusInProgress &&
			(target == board.StatusBacklog || target == board.StatusNext):
			ticket.SetAgentStatus(board.AgentNone)
		}

		if err := store.SaveTicket(ticket); err != nil {
			return fmt.Errorf("save ticket %s: %w", ticket.ID, err)
		}

		if n := len(promoted); n > 0 {
			fmt.Fprintf(os.Stderr, "openkanban: promoted %d claude approval(s) to repo defaults\n", n)
		}
		if n := len(pruned); n > 0 {
			fmt.Fprintf(os.Stderr, "openkanban: pruned %d stale allowlist entr(y/ies) (see .claude/.pruned-log)\n", n)
		}

		// Daemon teardown: any exit from in_progress terminates the live
		// PTY to preserve the 1:1 ticket↔session invariant. TicketDone is
		// a no-op on miss and best-effort on failure.
		if current == board.StatusInProgress && target != board.StatusInProgress {
			if derr := notifyDaemonTicketDoneCLI(string(ticket.ID), 3*time.Second); derr != nil {
				fmt.Fprintf(os.Stderr, "openkanbankd: ticket_done for %s: %v\n", ticket.ID, derr)
			}
		}

		fmt.Printf("moved %s → %s\n", shortID(string(ticket.ID)), target)
		return nil
	},
}

func init() {
	ticketCmd.AddCommand(ticketMoveCmd)
	ticketMoveCmd.Flags().StringVar(&ticketMoveProject, "project", "", "project name, ID, or unique prefix (required)")
	ticketMoveCmd.Flags().StringVar(&ticketMoveID, "id", "", "ticket ID, unique prefix, or title slug (required)")
	ticketMoveCmd.Flags().StringVar(&ticketMoveStatus, "status", "", "target status: backlog, next, in_progress, in_review, done, archived (required)")
	ticketMoveCmd.Flags().BoolVar(&ticketMoveForce, "force", false,
		"allow status change from inside an agent session (only use when the user explicitly asked)")
}

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/techdufus/openkanban/internal/agent"
	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/config"
	"github.com/techdufus/openkanban/internal/daemon"
	"github.com/techdufus/openkanban/internal/daemonclient"
	"github.com/techdufus/openkanban/internal/git"
	"github.com/techdufus/openkanban/internal/project"
	"github.com/techdufus/openkanban/internal/ticketsvc"
	"github.com/techdufus/openkanban/internal/workflow"
)

var (
	ticketNewProject         string
	ticketNewTitle           string
	ticketNewDescription     string
	ticketNewDescriptionFile string
	ticketNewStatus          string
	ticketNewType            string
	ticketNewLabels          string
	ticketNewPriority        int
	ticketNewNoWorktree      bool
	ticketNewWorktree        bool
	ticketNewJSON            bool
	ticketNewAllowMigration  bool
	ticketNewSession         string
	ticketNewMigrate         bool
	ticketNewForce           bool
	ticketNewCreatedBy       string
	ticketNewBlockedBy       string
	ticketNewWorktreeFrom    string

	ticketDeleteProject string
	ticketDeleteID      string
)

var ticketCmd = &cobra.Command{
	Use:   "ticket",
	Short: "Manage tickets from the command line",
	Long: `Create and (eventually) edit tickets without launching the TUI.

The primary use case is scripted ticket creation from another agent
or session — e.g. a parent Claude session that wants to spin off a
subtask as its own openkanban ticket and pass the resulting file
path to a child session for context.`,
}

var ticketNewCmd = &cobra.Command{
	Use:   "new",
	Short: "Create a new ticket",
	Long: `Create a new ticket in a project.

Output (plain): an "id=<uuid>" line, then (if --worktree) a
"worktree=<path>" line, then the .md file path as the FINAL line so
existing consumers that captured stdout as the path keep working. Use
--json for a stable machine-readable object instead.

The --project flag accepts an exact project name, an exact UUID, or a
unique UUID prefix of at least 4 characters. On ambiguous prefix the
command exits non-zero and lists the candidates.

--worktree provisions the git worktree + branch immediately (the same
derivation the TUI uses at spawn, so spawn reuses it). This differs
from --no-worktree, which only flips a lazy spawn-time hint and
provisions nothing; passing both is an error.

If the project still uses legacy single-file storage (tickets/<id>.json),
ticket new refuses to migrate it on its own so it can't race a running
TUI mid-migration. Launch the TUI once first to migrate, or pass
--allow-migration to migrate here.

Description sources (mutually exclusive, in priority order):
  1. --description "<inline text>"
  2. --description-file <path>
  3. stdin, if piped (i.e. not a TTY)
  4. empty`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		title := strings.TrimSpace(ticketNewTitle)
		if title == "" {
			return fmt.Errorf("--title must not be empty")
		}
		if ticketNewPriority < 0 || ticketNewPriority > 5 {
			return fmt.Errorf("--priority must be 0-5 (0 = use default)")
		}
		if ticketNewStatus != "" {
			if _, err := board.ParseStatus(ticketNewStatus); err != nil {
				return fmt.Errorf("--status %s", err)
			}
		}
		if ticketNewWorktree && ticketNewNoWorktree {
			return fmt.Errorf("--worktree and --no-worktree are contradictory")
		}
		if ticketNewWorktreeFrom != "" {
			if ticketNewWorktree {
				return fmt.Errorf("--worktree-from adopts an existing worktree; it is contradictory with --worktree (which creates a new one)")
			}
			if ticketNewNoWorktree {
				return fmt.Errorf("--worktree-from and --no-worktree are contradictory")
			}
		}

		registry, err := project.LoadRegistry()
		if err != nil {
			return fmt.Errorf("load project registry: %w", err)
		}

		// Resolve the project. When --project is omitted but we're running
		// inside an openkanban-spawned session ($OPENKANBAN_TICKET_ID set),
		// derive it from the current ticket so an agent can create sibling
		// tickets without knowing the project name/id (the spin-off/fan-out
		// skills rely on this). Human invocations still require --project.
		projectRef := ticketNewProject
		if projectRef == "" {
			if envTID := os.Getenv("OPENKANBAN_TICKET_ID"); envTID != "" {
				if src, _, found := findTicketAcrossProjects(registry, board.TicketID(envTID)); found {
					projectRef = src.ProjectID
				}
			}
		}
		if projectRef == "" {
			return fmt.Errorf("--project is required (or run inside an openkanban session so it is derived from $OPENKANBAN_TICKET_ID)")
		}

		proj, err := resolveProject(registry, projectRef)
		if err != nil {
			return err
		}

		state := project.MigrationStateFor(proj.ID)
		if state == project.MigrationPending && !ticketNewAllowMigration {
			return fmt.Errorf("project %q (%s) has legacy single-file ticket storage; "+
				"launch openkanban once to migrate it, or re-run with --allow-migration",
				proj.Name, shortID(proj.ID))
		}

		store, err := project.LoadTicketStore(proj)
		if err != nil {
			return fmt.Errorf("load ticket store: %w", err)
		}

		desc, err := resolveTicketDescription()
		if err != nil {
			return err
		}

		ticket := board.NewTicket(title, proj.ID)
		ticket.Description = desc
		if ticketNewStatus != "" {
			target := board.TicketStatus(ticketNewStatus)
			if target == board.StatusInProgress || target == board.StatusDone {
				if err := guardAgentStatusChange(target, ticketNewForce); err != nil {
					return err
				}
			}
			ticket.Status = target
		}
		if ticketNewPriority > 0 {
			ticket.Priority = ticketNewPriority
		}
		if ticketNewLabels != "" {
			for _, l := range strings.Split(ticketNewLabels, ",") {
				l = strings.TrimSpace(l)
				if l != "" {
					ticket.Labels = append(ticket.Labels, l)
				}
			}
		}
		if ticketNewNoWorktree {
			ticket.UseWorktree = false
		}
		if ticketNewType != "" {
			tt, terr := board.ParseTicketType(ticketNewType)
			if terr != nil {
				return fmt.Errorf("--type %s", terr)
			}
			ticket.Type = tt
		}
		// Worktree policy by type: research/spec are read-only report stages,
		// so they default to no-worktree (run in the main repo, no branch
		// churn) unless the user explicitly asked for one. An explicit
		// --worktree / --worktree-from / --no-worktree always wins.
		if (ticket.Type == board.TypeResearch || ticket.Type == board.TypeSpec) &&
			!ticketNewWorktree && !ticketNewNoWorktree && ticketNewWorktreeFrom == "" {
			ticket.UseWorktree = false
		}

		// Build the global store BEFORE applySessionFlags so LinkSession
		// can scan all tickets across all projects for the uniqueness
		// check. LoadGlobalTicketStore is read-only (no mutations); we
		// reuse the same `store` (single-project) for the actual write.
		globalStore, gerr := project.LoadGlobalTicketStore(registry)
		if gerr != nil {
			return fmt.Errorf("load global ticket store: %w", gerr)
		}

		if err := applySessionFlags(ticket, globalStore); err != nil {
			return err
		}

		// --blocked-by: record dependency links (this ticket depends on the
		// named tickets). Each id is validated across all projects so a typo
		// fails loudly rather than dangling. The links are informational at
		// the data layer today; workflow gates consume them separately.
		if ticketNewBlockedBy != "" {
			for _, raw := range strings.Split(ticketNewBlockedBy, ",") {
				dep := strings.TrimSpace(raw)
				if dep == "" {
					continue
				}
				if board.TicketID(dep) == ticket.ID {
					return fmt.Errorf("--blocked-by: a ticket cannot block itself")
				}
				if _, _, found := findTicketAcrossProjects(registry, board.TicketID(dep)); !found {
					return fmt.Errorf("--blocked-by: ticket %q not found in any project", dep)
				}
				ticket.BlockedBy = append(ticket.BlockedBy, board.TicketID(dep))
			}
		}

		// Workflow prerequisite gate. STARTING a typed ticket (--status
		// in_progress) with an unmet upstream is blocked unless --force;
		// merely CREATING one in a resting column is a non-blocking warning —
		// hard-block on start, warn on create. Runs after --blocked-by so the
		// gate sees the links.
		if perr := workflow.CheckPrerequisite(ticket, ticketGraphLookup{registry}); perr != nil {
			if ticket.Status == board.StatusInProgress && !ticketNewForce {
				return perr
			}
			fmt.Fprintf(os.Stderr, "openkanban: warning: %v\n", perr)
		}

		// --worktree-from: adopt an existing ticket's worktree + branch instead
		// of provisioning a new one, so a spin-off can continue a feature in
		// place. This only RECORDS the shared location; the worktree-exclusivity
		// safety gate (enforced at spawn) prevents two live agents from editing
		// it at once. Mutually exclusive with --worktree / --no-worktree above.
		if ticketNewWorktreeFrom != "" {
			src, _, found := findTicketAcrossProjects(registry, board.TicketID(ticketNewWorktreeFrom))
			if !found {
				return fmt.Errorf("--worktree-from: ticket %q not found", ticketNewWorktreeFrom)
			}
			if src.ProjectID != proj.ID {
				return fmt.Errorf("--worktree-from: ticket %q belongs to a different project", ticketNewWorktreeFrom)
			}
			if src.WorktreePath == "" {
				return fmt.Errorf("--worktree-from: ticket %q has no worktree to reuse", ticketNewWorktreeFrom)
			}
			ticket.UseWorktree = true
			ticket.WorktreePath = src.WorktreePath
			ticket.BranchName = src.BranchName
			ticket.BaseBranch = src.BaseBranch
		}

		// Opt-in worktree provisioning (sibling ticket 954ed3e8). Done
		// BEFORE SaveTicket so a provisioning failure leaves no ticket
		// behind — either the worktree-backed ticket is created whole, or
		// nothing is. The branch name comes from the SAME derivation the
		// TUI spawn path uses (project.BranchNameForTitle), so spawn later
		// reuses this worktree instead of creating a duplicate.
		if ticketNewWorktree {
			cfg, cerr := config.Load("")
			if cerr != nil {
				return fmt.Errorf("load config: %w", cerr)
			}
			mgr := git.NewWorktreeManager(proj)
			base, _ := mgr.GetDefaultBranch()
			branch := project.BranchNameForTitle(ticket.Title, proj, cfg.Defaults)
			wtPath, werr := mgr.CreateWorktree(branch, base)
			if werr != nil {
				return fmt.Errorf("provision worktree for %s: %w", ticket.ID, werr)
			}
			if serr := agent.SeedClaudeSettings(wtPath, proj.RepoPath); serr != nil {
				fmt.Fprintf(os.Stderr, "openkanban: seed claude settings (%s): %v\n", wtPath, serr)
			}
			ticket.WorktreePath = wtPath
			ticket.BranchName = branch
			ticket.BaseBranch = base
		}

		if err := store.SaveTicket(ticket); err != nil {
			// If we provisioned a worktree above, roll it back so a
			// failed save doesn't leave an orphaned worktree+branch with
			// no ticket referencing it (keeps the "all or nothing"
			// guarantee true for SaveTicket failures too).
			if ticket.WorktreePath != "" {
				if rerr := git.NewWorktreeManager(proj).RemoveWorktree(ticket.WorktreePath); rerr != nil {
					fmt.Fprintf(os.Stderr, "openkanban: roll back worktree %s: %v\n", ticket.WorktreePath, rerr)
				}
			}
			return fmt.Errorf("save ticket: %w", err)
		}

		// Reproduce SaveTicket's path computation so the CLI doesn't
		// have to depend on private fields of TicketStore.
		ticketsRoot, err := configTicketsDir()
		if err != nil {
			return err
		}
		path := filepath.Join(ticketsRoot, proj.ID, project.TicketFilename(ticket))

		blockedBy := make([]string, 0, len(ticket.BlockedBy))
		for _, id := range ticket.BlockedBy {
			blockedBy = append(blockedBy, string(id))
		}
		result := ticketNewResult{
			ID:           string(ticket.ID),
			Path:         path,
			Slug:         board.Slugify(ticket.Title, 40),
			Status:       string(ticket.Status),
			Type:         string(ticket.Type),
			ProjectID:    proj.ID,
			WorktreePath: ticket.WorktreePath,
			BranchName:   ticket.BranchName,
			BaseBranch:   ticket.BaseBranch,
			BlockedBy:    blockedBy,
		}
		if ticketNewJSON {
			enc, jerr := json.MarshalIndent(result, "", "  ")
			if jerr != nil {
				return fmt.Errorf("marshal json: %w", jerr)
			}
			fmt.Println(string(enc))
			return nil
		}

		// Human/scripted output. The ticket id is emitted first; the .md
		// path stays the FINAL line for back-compat with consumers that
		// captured stdout as the path.
		fmt.Printf("id=%s\n", ticket.ID)
		if ticket.WorktreePath != "" {
			fmt.Printf("worktree=%s\n", ticket.WorktreePath)
		}
		fmt.Println(path)
		return nil
	},
}

// ticketNewResult is the stable --json schema for `ticket new`. Every field is
// always present (no omitempty); worktree_path/branch_name/base_branch are
// empty strings unless --worktree provisioned a worktree.
type ticketNewResult struct {
	ID           string `json:"id"`
	Path         string `json:"path"`
	Slug         string `json:"slug"`
	Status       string `json:"status"`
	Type         string `json:"type"`
	ProjectID    string `json:"project_id"`
	WorktreePath string `json:"worktree_path"`
	BranchName   string `json:"branch_name"`
	BaseBranch   string `json:"base_branch"`
	// BlockedBy is always present (empty slice, never null) so consumers can
	// range over it unconditionally.
	BlockedBy []string `json:"blocked_by"`
}

var ticketDeleteCmd = &cobra.Command{
	Use:   "delete",
	Short: "Delete a ticket from a project",
	Long: `Delete a ticket from a project, removing both the ticket's .md
file and (if the daemon is up and currently owns the agent session) the
running daemon-side session.

The --project flag follows the same matching rules as 'ticket new': an
exact name, an exact UUID, or a unique UUID prefix of at least 4
characters.

If the daemon is down or doesn't own the ticket's AgentSessionID, no
foreign Claude process is killed — the lsof / SIGTERM dance from
'ticket new --migrate --force' is NOT replicated here, since deleting a
ticket should not, on its own, kill a foreign Claude process.

If the ticket has a worktree, it is torn down per config.Cleanup
(delete_worktree removes the worktree; delete_branch deletes the branch
only when it is fully merged). Teardown is best-effort: failures are
warned to stderr and never block the delete.

This command does NOT autostart the daemon: a scripted invocation must
remain quiet when the daemon happens to be down.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if ticketDeleteProject == "" {
			return fmt.Errorf("--project is required")
		}
		if ticketDeleteID == "" {
			return fmt.Errorf("--id is required")
		}

		registry, err := project.LoadRegistry()
		if err != nil {
			return fmt.Errorf("load project registry: %w", err)
		}
		proj, err := resolveProject(registry, ticketDeleteProject)
		if err != nil {
			return err
		}

		state := project.MigrationStateFor(proj.ID)
		if state == project.MigrationPending {
			return fmt.Errorf("project %q (%s) has legacy single-file ticket "+
				"storage; launch openkanban once to migrate it before deleting",
				proj.Name, shortID(proj.ID))
		}

		store, err := project.LoadTicketStore(proj)
		if err != nil {
			return fmt.Errorf("load ticket store: %w", err)
		}
		t, err := resolveTicket(store, registry, ticketDeleteID)
		if err != nil {
			return err
		}

		// Daemon-side cleanup BEFORE the file-system delete: if we
		// remove the .md first and then the daemon RPC fails, we've
		// orphaned the daemon session AND lost the on-disk record.
		// Order: kill the live daemon session, then unlink the ticket.
		//
		// Two layered RPCs:
		//
		//  1. UUID-keyed Owns/Kill (only when AgentSessionID is set):
		//     terminates the session via its Claude session UUID. This
		//     is the legacy path and is the cheapest hit when the UUID
		//     is already back-filled on the ticket.
		//
		//  2. TicketID-keyed TicketDone (unconditional): catches the
		//     freshly-spawned-but-not-yet-backfilled case, where the
		//     daemon DOES have a live session for the ticket but the
		//     ticket .md still has AgentSessionID="". Also a safe
		//     no-op when (1) already killed the session. Note:
		//     AgentSessionID may be set even after a ticket is done
		//     (the expected-exit handler preserves it for resume); (1)
		//     will probe the daemon, find the session already gone,
		//     and skip — (2) catches the remaining case.
		if t.AgentSessionID != "" {
			ownsResp, daemonUp, daemonOwns, perr := probeDaemonOwnership(t.AgentSessionID)
			if perr != nil {
				return fmt.Errorf("probe daemon ownership for session %s: %w", t.AgentSessionID, perr)
			}
			if daemonUp && daemonOwns {
				if kerr := killDaemonSession(ownsResp.SessionID, 3*time.Second); kerr != nil {
					return fmt.Errorf("kill daemon session %s: %w", ownsResp.SessionID, kerr)
				}
			}
		}
		if derr := notifyDaemonTicketDoneCLI(string(t.ID), 3*time.Second); derr != nil {
			// Best-effort: log + continue. The .md unlink below is the
			// authoritative "ticket is gone" signal; a transient daemon
			// failure must not block deletion.
			fmt.Fprintf(os.Stderr, "openkanbankd: ticket_done for %s: %v\n", t.ID, derr)
		}

		// Worktree + branch teardown BEFORE the file-system delete, so a
		// deleted ticket doesn't leave an orphaned worktree/branch behind —
		// the orphan is what later collides with a new ticket's spawn (the
		// branch name is derived deterministically from the title). All of
		// this is best-effort: warnings go to stderr, but teardown failure
		// never aborts the delete. Honors config.Cleanup gates, matching the
		// TUI's performTicketCleanup (model.go:3824).
		cfg, cerr := config.Load("")
		if cerr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not load config for worktree cleanup: %v\n", cerr)
		} else {
			mgr := git.NewWorktreeManager(proj)
			if t.WorktreePath != "" && cfg.Cleanup.DeleteWorktree {
				if rerr := mgr.RemoveWorktree(t.WorktreePath); rerr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not remove worktree %s: %v\n", t.WorktreePath, rerr)
				}
			}
			if cfg.Cleanup.DeleteBranch && t.BranchName != "" {
				// Mirror model.go's safe-delete: DeleteMergedBranch refuses an
				// unmerged branch rather than force-deleting it.
				if deleted, derr := mgr.DeleteMergedBranch(t.BranchName); derr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not delete branch %s: %v\n", t.BranchName, derr)
				} else if !deleted {
					fmt.Fprintf(cmd.ErrOrStderr(), "note: branch %q has unmerged work; not deleted\n", t.BranchName)
				}
			}
		}

		if err := store.Delete(t.ID); err != nil {
			return fmt.Errorf("delete ticket: %w", err)
		}
		fmt.Printf("deleted %s\n", t.ID)
		return nil
	},
}

// resolveProject matches the CLI --project arg against the registry.
//
// Match precedence:
//   1. exact name match
//   2. exact UUID match
//   3. unique UUID prefix (min 4 chars)
//
// Ambiguous prefix and zero-match both return errors with hints.
func resolveProject(reg *project.ProjectRegistry, arg string) (*project.Project, error) {
	if arg == "" {
		return nil, fmt.Errorf("--project value is empty")
	}

	var exact, prefix []*project.Project
	for _, p := range reg.List() {
		if p.Name == arg || p.ID == arg {
			exact = append(exact, p)
			continue
		}
		if len(arg) >= 4 && strings.HasPrefix(p.ID, arg) {
			prefix = append(prefix, p)
		}
	}

	if len(exact) == 1 {
		return exact[0], nil
	}
	if len(exact) > 1 {
		return nil, fmt.Errorf("multiple projects match %q exactly:\n%s",
			arg, formatProjectMatches(exact))
	}
	if len(prefix) == 1 {
		return prefix[0], nil
	}
	if len(prefix) > 1 {
		return nil, fmt.Errorf("project prefix %q is ambiguous (%d matches); specify more characters:\n%s",
			arg, len(prefix), formatProjectMatches(prefix))
	}
	return nil, fmt.Errorf("no project matches %q; run 'openkanban list' to see available projects", arg)
}

func formatProjectMatches(ps []*project.Project) string {
	lines := make([]string, 0, len(ps))
	for _, p := range ps {
		lines = append(lines, fmt.Sprintf("  %s  %s", shortID(p.ID), p.Name))
	}
	return strings.Join(lines, "\n")
}

// resolveTicket selects a ticket from an already-resolved project's store.
// Because the delete path always has --project, resolution is scoped to one
// project — there is no cross-project ambiguity.
//
// Match precedence (mirrors resolveProject):
//  1. exact ticket id
//  2. unique id prefix (>=4 chars; this also covers the filename short-hash,
//     which is just the first 8 chars of the id)
//  3. unique title slug (board.Slugify of the title)
//
// On no match, if the arg looks like a PROJECT id (the common footgun — the
// directory UUID in the printed .md path is the project id, not the ticket
// id), the error says so and points at `ticket list`.
func resolveTicket(store *project.TicketStore, registry *project.ProjectRegistry, arg string) (*board.Ticket, error) {
	if arg == "" {
		return nil, fmt.Errorf("--id value is empty")
	}
	all := store.All()

	for _, t := range all {
		if string(t.ID) == arg {
			return t, nil
		}
	}

	var idPrefix, slugMatch []*board.Ticket
	if len(arg) >= 4 {
		for _, t := range all {
			if strings.HasPrefix(string(t.ID), arg) {
				idPrefix = append(idPrefix, t)
			}
		}
	}
	lowerArg := strings.ToLower(arg)
	for _, t := range all {
		if board.Slugify(t.Title, 40) == lowerArg {
			slugMatch = append(slugMatch, t)
		}
	}

	for _, tier := range []struct {
		kind    string
		matches []*board.Ticket
	}{
		{"id prefix", idPrefix},
		{"title slug", slugMatch},
	} {
		switch len(tier.matches) {
		case 1:
			return tier.matches[0], nil
		case 0:
			// fall through to the next tier
		default:
			return nil, fmt.Errorf("%q matches %d tickets by %s; pass the full id:\n%s",
				arg, len(tier.matches), tier.kind, formatTicketMatches(tier.matches))
		}
	}

	if hint := projectIDHint(registry, arg); hint != "" {
		return nil, fmt.Errorf("%s", hint)
	}
	return nil, fmt.Errorf("no ticket matches %q in this project; run 'openkanban ticket list --project %s' to see ticket ids",
		arg, ticketDeleteProject)
}

func formatTicketMatches(ts []*board.Ticket) string {
	lines := make([]string, 0, len(ts))
	for _, t := range ts {
		lines = append(lines, fmt.Sprintf("  %s  %s", shortID(string(t.ID)), t.Title))
	}
	return strings.Join(lines, "\n")
}

// projectIDHint returns a corrective message when arg is (or uniquely
// prefixes) a known PROJECT id rather than a ticket id; empty string
// otherwise.
func projectIDHint(registry *project.ProjectRegistry, arg string) string {
	for _, p := range registry.List() {
		if p.ID == arg {
			return fmt.Sprintf("%s is a project id, not a ticket id — run 'openkanban ticket list --project %s' to find ticket ids", arg, arg)
		}
	}
	if len(arg) >= 4 {
		for _, p := range registry.List() {
			if strings.HasPrefix(p.ID, arg) {
				return fmt.Sprintf("%q looks like a project id prefix, not a ticket id — run 'openkanban ticket list --project %s' to find ticket ids", arg, arg)
			}
		}
	}
	return ""
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func resolveTicketDescription() (string, error) {
	if ticketNewDescription != "" && ticketNewDescriptionFile != "" {
		return "", fmt.Errorf("--description and --description-file are mutually exclusive")
	}
	if ticketNewDescription != "" {
		return ticketNewDescription, nil
	}
	if ticketNewDescriptionFile != "" {
		data, err := os.ReadFile(ticketNewDescriptionFile)
		if err != nil {
			return "", fmt.Errorf("read description file %q: %w", ticketNewDescriptionFile, err)
		}
		return string(data), nil
	}
	// Stdin fallback: only consume if piped (i.e. not a TTY).
	stat, err := os.Stdin.Stat()
	if err == nil && (stat.Mode()&os.ModeCharDevice) == 0 {
		data, rerr := io.ReadAll(os.Stdin)
		if rerr != nil {
			return "", fmt.Errorf("read stdin: %w", rerr)
		}
		return string(data), nil
	}
	return "", nil
}

// configTicketsDir replicates internal/project.ticketsDir() without
// importing the unexported helper. Keeps the CLI's view of the
// ticket root in sync with what the store writes.
func configTicketsDir() (string, error) {
	return ticketsRootDir()
}

// applySessionFlags validates the --session/--migrate/--force/--created-by
// combination and stamps the corresponding fields on the ticket.
//
// Order of checks (cheap → expensive):
//  1. Argument-shape sanity (combinations + UUID format).
//  2. File-existence in the Claude projects dir.
//  3. Ownership probe (only when --migrate):
//     a. Ask the daemon (if reachable, no autostart) whether IT owns
//        the session.
//     b. If the daemon owns it, --force routes the SIGTERM through the
//        daemon's Kill RPC so the daemon's bookkeeping stays consistent
//        (sessions map, subscribers, last-client shutdown).
//     c. Otherwise fall back to the lsof + ForceExitSession path. This
//        covers daemon-down, daemon-up-but-doesn't-own (the session is
//        held by some foreign Claude process), or both.
//
// We deliberately do NOT autostart the daemon for this probe — the
// caller may be running `openkanban ticket new` from a script, and
// surprise-spawning a long-lived background daemon would be hostile.
//
// The audit field is independent of the operational fields and is
// applied unconditionally if set.
func applySessionFlags(ticket *board.Ticket, globalStore *project.GlobalTicketStore) error {
	if ticketNewMigrate && ticketNewSession == "" {
		return fmt.Errorf("--migrate requires --session")
	}
	// --force used to require --migrate (kill the lsof holder). Now
	// --force also acts on --session alone: claim a uuid that's already
	// linked to a different ticket by clearing the other ticket's link
	// first. Both behaviors stack: --migrate --force kills the process
	// AND clears the conflicting ticket link.
	if ticketNewForce && !ticketNewMigrate && ticketNewSession == "" {
		return fmt.Errorf("--force requires --migrate or --session")
	}

	if ticketNewSession != "" {
		uuid := strings.TrimSpace(ticketNewSession)
		if !agent.SessionUUIDPattern.MatchString(uuid) {
			return fmt.Errorf("--session %q is not a Claude Code session UUID; "+
				"session ref must be a Claude Code session UUID; "+
				"find yours via /cost or the claude --resume picker", uuid)
		}
		path, err := agent.SessionPath(uuid)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("--session %s: no Claude Code session file "+
					"found at ~/.claude/projects/*/%s.jsonl", uuid, uuid)
			}
			return fmt.Errorf("locate session %s: %w", uuid, err)
		}

		if ticketNewMigrate {
			ownsResp, daemonUp, daemonOwns, err := probeDaemonOwnership(uuid)
			if err != nil {
				return err
			}

			switch {
			case daemonUp && daemonOwns:
				if !ticketNewForce {
					return fmt.Errorf("--migrate refused: session %s is "+
						"currently held by openkanbankd (daemon session "+
						"%s); re-run with --force to terminate it via "+
						"the daemon (any unsubmitted prompt will be lost)",
						uuid, ownsResp.SessionID)
				}
				if err := killDaemonSession(ownsResp.SessionID, 3*time.Second); err != nil {
					return fmt.Errorf("force-exit daemon session %s: %w",
						ownsResp.SessionID, err)
				}
			default:
				// Daemon down, or daemon up but doesn't own. Fall back
				// to the lsof probe + direct SIGTERM path.
				holder, err := agent.SessionActive(uuid)
				if err != nil {
					return fmt.Errorf("check session %s in-use: %w", uuid, err)
				}
				if holder.PID != 0 {
					if !ticketNewForce {
						return fmt.Errorf("--migrate refused: session %s is "+
							"currently held by pid %d (%s); exit that "+
							"session first, or re-run with --force to "+
							"terminate it (any unsubmitted prompt will be lost)",
							uuid, holder.PID, path)
					}
					if err := agent.ForceExitSession(uuid, 3*time.Second); err != nil {
						return fmt.Errorf("force-exit session %s (pid %d): %w",
							uuid, holder.PID, err)
					}
				}
			}
		}

		// 1:1 invariant gate: refuse if uuid is already linked to a
		// DIFFERENT ticket. Storage layer tolerates duplicates by
		// policy, but creation must not produce new ones — see
		// [[openkanban-one-to-one-ticket-session-invariant]]. --force
		// here means "claim the uuid from the other ticket" (clear
		// other ticket's AgentSessionID before claiming).
		opts := ticketsvc.LinkOpts{Force: ticketNewForce}
		if _, err := ticketsvc.LinkSession(globalStore, ticket, uuid, opts); err != nil {
			var conflict *ticketsvc.ErrSessionAlreadyLinked
			if errors.As(err, &conflict) {
				return fmt.Errorf("--session refused: %w; "+
					"re-run with --force to claim the session from the "+
					"other ticket(s), clearing their AgentSessionID first",
					conflict)
			}
			return fmt.Errorf("link session %s: %w", uuid, err)
		}
		// SessionOwned removed: every spawn is migrate-on-resume.
		// --migrate's lsof/daemon-owns kill above is still meaningful
		// (clears the JSONL holder), but there's no link-vs-migrate
		// bit on the ticket anymore.
	}

	if ticketNewCreatedBy != "" {
		ticket.CreatedBySession = ticketNewCreatedBy
	}
	return nil
}

// probeDaemonOwnership asks openkanbankd, if it is reachable, whether
// the daemon currently owns the given Claude session UUID. Crucially
// it uses Dial (not DialOrStart) so a script-driven `ticket new`
// doesn't unexpectedly autostart a background daemon.
//
// Return tuple:
//   - ownsResp:  the OwnsResp from the daemon (zero value when daemon down)
//   - daemonUp:  true iff the dial + Hello succeeded
//   - daemonOwns: convenience alias for ownsResp.Owned when daemonUp
//   - err:       only set if the daemon was reachable but the RPC itself
//     failed (encode / decode / unexpected error). Daemon-not-running
//     is NOT an error here — it's the expected fallback path.
func probeDaemonOwnership(uuid string) (daemon.OwnsResp, bool, bool, error) {
	// Brief timeout: this is a quick probe on a local socket, and we
	// must not stall ticket creation if something is wedged.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	conn, err := daemonclient.Dial(ctx)
	if err != nil {
		if errors.Is(err, daemonclient.ErrDaemonUnavailable) {
			return daemon.OwnsResp{}, false, false, nil
		}
		return daemon.OwnsResp{}, false, false, fmt.Errorf("dial daemon: %w", err)
	}

	client, err := daemonclient.NewWithConn(ctx, conn)
	if err != nil {
		// NewWithConn closes the conn on Hello failure. Treat a hello
		// timeout against a wedged daemon as "not reachable" rather
		// than a hard error — same conservative stance as Dial-not-up.
		return daemon.OwnsResp{}, false, false, nil
	}
	defer client.Close()

	resp, err := client.Owns(ctx, uuid)
	if err != nil {
		return daemon.OwnsResp{}, true, false, fmt.Errorf("daemon owns query: %w", err)
	}
	return resp, true, resp.Owned, nil
}

// notifyDaemonTicketDoneCLI fires a TicketDone RPC keyed by TicketID
// against a running daemon (no autostart). Used by `openkanban ticket
// delete` as the second layer of daemon-side cleanup: it catches the
// freshly-spawned-but-not-yet-backfilled case where the daemon owns a
// live session for this ticket but the ticket .md still has
// AgentSessionID="" (so the UUID-keyed Owns/Kill path above can't fire).
// Also acts as a no-op safety net when AgentSessionID IS set (preserved
// from a prior expected exit for resume) but the daemon already cleaned
// up — the RPC simply reports Killed=false and returns.
//
// Same conservative dial contract as probeDaemonOwnership: a scripted
// `ticket delete` must NOT autostart a background daemon. A daemon-down
// dial is treated as success (there are by definition no sessions to
// clean up). Other RPC failures bubble up so the caller can log them,
// but the caller is expected to treat the .md unlink as authoritative
// and not block deletion on a transient daemon hiccup.
//
// `resp.Killed=false` is NOT an error — it just means the daemon had
// no live session matching the TicketID, which is the common case.
func notifyDaemonTicketDoneCLI(ticketID string, grace time.Duration) error {
	// Allow grace plus a bit for the RPC round-trip, matching the
	// killDaemonSession budget.
	ctx, cancel := context.WithTimeout(context.Background(), grace+2*time.Second)
	defer cancel()

	conn, err := daemonclient.Dial(ctx)
	if err != nil {
		if errors.Is(err, daemonclient.ErrDaemonUnavailable) {
			return nil
		}
		return fmt.Errorf("dial daemon: %w", err)
	}

	client, err := daemonclient.NewWithConn(ctx, conn)
	if err != nil {
		// NewWithConn closes the conn on Hello failure. Treat a wedged
		// daemon the same way probeDaemonOwnership does — no-op.
		return nil
	}
	defer client.Close()

	if _, err := client.TicketDone(ctx, ticketID); err != nil {
		return fmt.Errorf("ticket_done RPC: %w", err)
	}
	return nil
}

// killDaemonSession opens a short-lived client connection (no autostart)
// and asks the daemon to terminate the named session with the given
// grace window. Returns ErrDaemonUnavailable if the daemon is down by
// the time we try to dial — caller has already proven it was up via
// probeDaemonOwnership, but races happen.
func killDaemonSession(sessionID string, grace time.Duration) error {
	// Allow grace plus a bit for the RPC round-trip.
	ctx, cancel := context.WithTimeout(context.Background(), grace+2*time.Second)
	defer cancel()

	conn, err := daemonclient.Dial(ctx)
	if err != nil {
		return err
	}

	client, err := daemonclient.NewWithConn(ctx, conn)
	if err != nil {
		return err
	}
	defer client.Close()

	return client.Kill(ctx, sessionID, grace)
}

func init() {
	ticketCmd.AddCommand(ticketNewCmd)
	ticketCmd.AddCommand(ticketDeleteCmd)

	ticketDeleteCmd.Flags().StringVar(&ticketDeleteProject, "project", "",
		"Project name, UUID, or unique 4+ char UUID prefix (required)")
	ticketDeleteCmd.Flags().StringVar(&ticketDeleteID, "id", "",
		"Ticket ID to delete (required)")
	_ = ticketDeleteCmd.MarkFlagRequired("project")
	_ = ticketDeleteCmd.MarkFlagRequired("id")

	ticketNewCmd.Flags().StringVar(&ticketNewProject, "project", "",
		"Project name, UUID, or unique 4+ char UUID prefix (required)")
	ticketNewCmd.Flags().StringVar(&ticketNewTitle, "title", "",
		"Ticket title (required)")
	ticketNewCmd.Flags().StringVar(&ticketNewDescription, "description", "",
		"Ticket description (markdown body of the resulting file)")
	ticketNewCmd.Flags().StringVar(&ticketNewDescriptionFile, "description-file", "",
		"Read description from this file path instead of --description")
	ticketNewCmd.Flags().StringVar(&ticketNewStatus, "status", "",
		"Initial status: backlog (default), next, in_progress, in_review, done, archived")
	ticketNewCmd.Flags().StringVar(&ticketNewLabels, "labels", "",
		"Comma-separated labels")
	ticketNewCmd.Flags().StringVar(&ticketNewType, "type", "",
		"Pipeline type: research, spec, implement, review (empty = freeform). Binds a specialized agent role at spawn; implement/review START is gated on a linked upstream")
	ticketNewCmd.Flags().IntVar(&ticketNewPriority, "priority", 0,
		"Priority 1-5 (0 = use default, which is 3)")
	ticketNewCmd.Flags().BoolVar(&ticketNewNoWorktree, "no-worktree", false,
		"Mark the ticket so its agent spawn won't use a git worktree (a lazy hint; provisions nothing now)")
	ticketNewCmd.Flags().BoolVar(&ticketNewWorktree, "worktree", false,
		"Provision the git worktree + branch now (not lazily at spawn) and print its path; contradicts --no-worktree")
	ticketNewCmd.Flags().BoolVar(&ticketNewJSON, "json", false,
		"Emit a JSON object {id, path, slug, status, type, project_id, worktree_path, branch_name, base_branch, blocked_by} instead of plain lines")
	ticketNewCmd.Flags().BoolVar(&ticketNewAllowMigration, "allow-migration", false,
		"Allow migrating legacy single-file ticket storage instead of refusing")
	ticketNewCmd.Flags().StringVar(&ticketNewSession, "session", "",
		"Link a Claude Code session UUID to this ticket. On first spawn, openkanban resumes it (forking by default)")
	ticketNewCmd.Flags().BoolVar(&ticketNewMigrate, "migrate", false,
		"Treat --session as a migration: openkanban will resume in place (no fork) on spawn. Refuses if the session is currently in use unless --force is also set")
	ticketNewCmd.Flags().BoolVar(&ticketNewForce, "force", false,
		"With --migrate: kill the process currently holding the session JSONL (SIGTERM, 3s grace, SIGKILL). Unsubmitted prompts in that session are lost")
	ticketNewCmd.Flags().StringVar(&ticketNewCreatedBy, "created-by", "",
		"Free-form name of the session that created this ticket (audit/provenance only)")
	ticketNewCmd.Flags().StringVar(&ticketNewBlockedBy, "blocked-by", "",
		"Comma-separated ticket IDs this ticket depends on (records dependency links; each id must exist)")
	ticketNewCmd.Flags().StringVar(&ticketNewWorktreeFrom, "worktree-from", "",
		"Reuse the worktree+branch of an existing ticket (same project) instead of creating a new one; contradicts --worktree/--no-worktree")

	// --project is intentionally NOT MarkFlagRequired: when omitted inside an
	// openkanban session it is derived from $OPENKANBAN_TICKET_ID (see RunE).
	// The RunE guard still errors when it can be neither supplied nor derived.
	_ = ticketNewCmd.MarkFlagRequired("title")

	rootCmd.AddCommand(ticketCmd)
}

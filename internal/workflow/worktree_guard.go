// Package workflow holds openkanban's cross-cutting "gate" logic — the rules
// that decide whether a ticket may advance or an agent may start. The logic is
// kept pure and dependency-light (no UI, no daemon, no git) so the CLI, the
// TUI, and the daemon can all enforce identical rules and unit-test them in
// isolation.
//
// Two classes of gate live here:
//
//   - SAFETY gates protect against data corruption and have NO override. The
//     worktree-exclusivity gate below is one: two agents editing one worktree
//     clobber each other, so the only escapes are "use a different worktree" or
//     "wait for the occupant to finish".
//   - PRACTICE gates (added later, e.g. "no implement ticket without a done
//     plan") are advisory nudges that a --force / confirm can override.
package workflow

// LiveWorktree pairs a ticket with the worktree its currently-live agent
// occupies. Callers build this set from whatever "an agent is live" signal
// they own (in the TUI: membership in the pane map, which tracks open daemon
// sessions), so the pure gate below stays free of runtime dependencies.
type LiveWorktree struct {
	TicketID     string
	Title        string
	WorktreePath string
}

// WorktreeConflict reports the live agent occupying worktreePath, if any,
// excluding selfTicketID (so a ticket resuming its OWN worktree never
// conflicts). Returns nil when the worktree is free, or when worktreePath is
// empty (a ticket that runs directly in the main repo has no worktree to
// contend for — that collision is guarded separately, per project).
//
// This is the enforcement point for the "one live agent per worktree"
// invariant. It matters because the daemon's Spawn is idempotent per TICKET,
// not per worktree: two distinct tickets pointed at the same worktree (e.g. a
// spin-off created with `ticket new --worktree-from`) could otherwise both go
// live in the same directory and corrupt each other's edits.
func WorktreeConflict(worktreePath, selfTicketID string, live []LiveWorktree) *LiveWorktree {
	if worktreePath == "" {
		return nil
	}
	for i := range live {
		if live[i].WorktreePath == worktreePath && live[i].TicketID != selfTicketID {
			return &live[i]
		}
	}
	return nil
}

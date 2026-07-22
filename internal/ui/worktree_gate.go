package ui

import (
	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/workflow"
)

// worktreeOccupiedByOther returns the live agent (if any) currently holding
// the same worktree as `ticket`, excluding the ticket itself — the enforcement
// adapter for the "one live agent per worktree" safety gate.
//
// "Live" == membership in m.panes: a pane exists in the map iff its daemon
// session is open (panes are Close()d + deleted on every "exited" event), so
// this counts genuinely-live sessions, not the stale pane.Running() flag. The
// pure decision lives in workflow.WorktreeConflict so it stays unit-testable;
// this method only translates m.panes into its input.
//
// A ticket resuming its OWN worktree never trips this (its pane returns early
// at the spawn call sites before we get here, and it's excluded by TicketID
// besides). Fresh tickets whose worktree is provisioned later (WorktreePath
// still "") also can't collide — their worktree is derived uniquely from the
// slug. The gate therefore fires only for the deliberate-reuse case, e.g. a
// spin-off created with `ticket new --worktree-from`.
func (m *Model) worktreeOccupiedByOther(ticket *board.Ticket) *workflow.LiveWorktree {
	if ticket == nil || ticket.WorktreePath == "" {
		return nil
	}
	var live []workflow.LiveWorktree
	for otherID := range m.panes {
		if otherID == ticket.ID {
			continue
		}
		other, _ := m.globalStore.Get(otherID)
		if other != nil && other.WorktreePath != "" {
			live = append(live, workflow.LiveWorktree{
				TicketID:     string(other.ID),
				Title:        other.Title,
				WorktreePath: other.WorktreePath,
			})
		}
	}
	return workflow.WorktreeConflict(ticket.WorktreePath, string(ticket.ID), live)
}

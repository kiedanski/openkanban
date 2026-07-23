package cmd

import (
	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/project"
	"github.com/techdufus/openkanban/internal/workflow"
)

// ticketGraphLookup adapts the cross-project ticket search to
// workflow.TicketLookup, so the prerequisite gate can resolve a ticket's
// BlockedBy links even when they live in another project. It reuses
// findTicketAcrossProjects (ticket_done.go) — read-only, no store mutation.
type ticketGraphLookup struct{ registry *project.ProjectRegistry }

func (l ticketGraphLookup) Get(id board.TicketID) (*board.Ticket, error) {
	if t, _, found := findTicketAcrossProjects(l.registry, id); found {
		return t, nil
	}
	return nil, board.ErrTicketNotFound
}

// checkStartPrerequisite enforces the workflow PRACTICE gate when a ticket is
// about to START (move to in_progress / spawn). It is overridable: force
// short-circuits to nil. The returned error already hints "--force", matching
// the house style of guardAgentStatusChange.
func checkStartPrerequisite(registry *project.ProjectRegistry, ticket *board.Ticket, force bool) error {
	if force {
		return nil
	}
	return workflow.CheckPrerequisite(ticket, ticketGraphLookup{registry})
}

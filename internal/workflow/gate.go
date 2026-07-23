package workflow

import (
	"errors"

	"github.com/techdufus/openkanban/internal/board"
)

// TicketLookup resolves a ticket by ID. project.GlobalTicketStore.Get
// satisfies it directly. The interface is kept intentionally minimal so the
// gate below stays pure (no store, no git, no UI) and trivially fakeable in
// tests — the same discipline as WorktreeConflict in worktree_guard.go.
type TicketLookup interface {
	Get(board.TicketID) (*board.Ticket, error)
}

// ErrMissingSpec / ErrMissingImpl are the PRACTICE-gate refusals returned by
// CheckPrerequisite. They are advisory: callers always offer an override
// (--force on the CLI, the confirm/offer in the TUI).
var (
	ErrMissingSpec = errors.New("an implement ticket needs a linked, done spec before it can start (link one with --blocked-by, or override with --force)")
	ErrMissingImpl = errors.New("a review ticket needs a linked implement in in_review or later before it can start (link one with --blocked-by, or override with --force)")
)

// CheckPrerequisite reports why a ticket may not START yet — i.e. move to
// in_progress / spawn its agent — or nil if it may. It enforces the
// research → spec → implement → review pipeline ordering over the existing
// board.Ticket.BlockedBy edges:
//
//   - implement can't start without a linked spec ticket that is done.
//   - review can't start without a linked implement ticket in in_review+.
//
// freeform/research/spec have no upstream prerequisite and always pass, so
// untyped tickets (TypeFreeform) behave exactly as before this gate existed.
//
// This is a PRACTICE gate: unlike the WorktreeConflict SAFETY gate (which has
// no override because two agents in one worktree corrupt each other), a
// missing prerequisite is a nudge the user can always override. The gate is
// pure so the CLI, the TUI, and the daemon can all enforce it identically.
func CheckPrerequisite(t *board.Ticket, lk TicketLookup) error {
	if t == nil || lk == nil {
		return nil
	}
	switch t.Type {
	case board.TypeImplement:
		if !hasUpstream(t, lk, board.TypeSpec, board.StatusDone) {
			return ErrMissingSpec
		}
	case board.TypeReview:
		if !hasUpstream(t, lk, board.TypeImplement, board.StatusInReview) {
			return ErrMissingImpl
		}
	}
	return nil
}

// hasUpstream reports whether any of t's BlockedBy links resolves to a ticket
// of type wantType whose status is at least minStatus in the pipeline order.
// Links that don't resolve (deleted / cross-project-not-found) are skipped —
// a dangling link can't satisfy a prerequisite.
func hasUpstream(t *board.Ticket, lk TicketLookup, wantType board.TicketType, minStatus board.TicketStatus) bool {
	for _, id := range t.BlockedBy {
		up, err := lk.Get(id)
		if err != nil || up == nil {
			continue
		}
		if up.Type == wantType && statusRank(up.Status) >= statusRank(minStatus) {
			return true
		}
	}
	return false
}

// statusRank orders statuses along the workflow progression so a gate can ask
// "at least in_review". archived ranks with done: both are terminal, so an
// archived (finished-and-filed) upstream still satisfies a "done" prerequisite.
func statusRank(s board.TicketStatus) int {
	switch s {
	case board.StatusBacklog:
		return 0
	case board.StatusNext:
		return 1
	case board.StatusInProgress:
		return 2
	case board.StatusInReview:
		return 3
	case board.StatusDone, board.StatusArchived:
		return 4
	default:
		return 0
	}
}

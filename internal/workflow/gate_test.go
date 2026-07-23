package workflow

import (
	"errors"
	"testing"

	"github.com/techdufus/openkanban/internal/board"
)

// fakeLookup is a pure in-memory TicketLookup for gate tests.
type fakeLookup map[board.TicketID]*board.Ticket

func (f fakeLookup) Get(id board.TicketID) (*board.Ticket, error) {
	if t, ok := f[id]; ok {
		return t, nil
	}
	return nil, board.ErrTicketNotFound
}

func mk(id board.TicketID, typ board.TicketType, status board.TicketStatus) *board.Ticket {
	return &board.Ticket{ID: id, Type: typ, Status: status}
}

func TestCheckPrerequisite(t *testing.T) {
	tests := []struct {
		name    string
		subject *board.Ticket
		store   fakeLookup
		wantErr error
	}{
		{
			name:    "freeform always passes",
			subject: &board.Ticket{Type: board.TypeFreeform, BlockedBy: nil},
			store:   fakeLookup{},
			wantErr: nil,
		},
		{
			name:    "research always passes",
			subject: &board.Ticket{Type: board.TypeResearch},
			store:   fakeLookup{},
			wantErr: nil,
		},
		{
			name:    "spec always passes (no upstream requirement)",
			subject: &board.Ticket{Type: board.TypeSpec},
			store:   fakeLookup{},
			wantErr: nil,
		},
		{
			name:    "implement with no links is blocked",
			subject: &board.Ticket{Type: board.TypeImplement},
			store:   fakeLookup{},
			wantErr: ErrMissingSpec,
		},
		{
			name:    "implement with a done spec passes",
			subject: &board.Ticket{Type: board.TypeImplement, BlockedBy: []board.TicketID{"spec1"}},
			store:   fakeLookup{"spec1": mk("spec1", board.TypeSpec, board.StatusDone)},
			wantErr: nil,
		},
		{
			name:    "implement with an archived spec passes (terminal)",
			subject: &board.Ticket{Type: board.TypeImplement, BlockedBy: []board.TicketID{"spec1"}},
			store:   fakeLookup{"spec1": mk("spec1", board.TypeSpec, board.StatusArchived)},
			wantErr: nil,
		},
		{
			name:    "implement with a spec still in progress is blocked",
			subject: &board.Ticket{Type: board.TypeImplement, BlockedBy: []board.TicketID{"spec1"}},
			store:   fakeLookup{"spec1": mk("spec1", board.TypeSpec, board.StatusInProgress)},
			wantErr: ErrMissingSpec,
		},
		{
			name:    "implement linked to a done ticket of the WRONG type is blocked",
			subject: &board.Ticket{Type: board.TypeImplement, BlockedBy: []board.TicketID{"r1"}},
			store:   fakeLookup{"r1": mk("r1", board.TypeResearch, board.StatusDone)},
			wantErr: ErrMissingSpec,
		},
		{
			name:    "implement with a dangling link is blocked",
			subject: &board.Ticket{Type: board.TypeImplement, BlockedBy: []board.TicketID{"missing"}},
			store:   fakeLookup{},
			wantErr: ErrMissingSpec,
		},
		{
			name:    "implement passes when at least one of several links is a done spec",
			subject: &board.Ticket{Type: board.TypeImplement, BlockedBy: []board.TicketID{"x", "spec1"}},
			store: fakeLookup{
				"x":     mk("x", board.TypeResearch, board.StatusDone),
				"spec1": mk("spec1", board.TypeSpec, board.StatusDone),
			},
			wantErr: nil,
		},
		{
			name:    "review with no links is blocked",
			subject: &board.Ticket{Type: board.TypeReview},
			store:   fakeLookup{},
			wantErr: ErrMissingImpl,
		},
		{
			name:    "review with an implement in in_review passes",
			subject: &board.Ticket{Type: board.TypeReview, BlockedBy: []board.TicketID{"impl1"}},
			store:   fakeLookup{"impl1": mk("impl1", board.TypeImplement, board.StatusInReview)},
			wantErr: nil,
		},
		{
			name:    "review with an implement already done passes",
			subject: &board.Ticket{Type: board.TypeReview, BlockedBy: []board.TicketID{"impl1"}},
			store:   fakeLookup{"impl1": mk("impl1", board.TypeImplement, board.StatusDone)},
			wantErr: nil,
		},
		{
			name:    "review with an implement still in progress is blocked",
			subject: &board.Ticket{Type: board.TypeReview, BlockedBy: []board.TicketID{"impl1"}},
			store:   fakeLookup{"impl1": mk("impl1", board.TypeImplement, board.StatusInProgress)},
			wantErr: ErrMissingImpl,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckPrerequisite(tt.subject, tt.store)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("CheckPrerequisite() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestCheckPrerequisite_NilSafe(t *testing.T) {
	if err := CheckPrerequisite(nil, fakeLookup{}); err != nil {
		t.Errorf("nil ticket: got %v, want nil", err)
	}
	if err := CheckPrerequisite(&board.Ticket{Type: board.TypeImplement}, nil); err != nil {
		t.Errorf("nil lookup: got %v, want nil", err)
	}
}

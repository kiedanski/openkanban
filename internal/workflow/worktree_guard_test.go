package workflow

import "testing"

func TestWorktreeConflict(t *testing.T) {
	live := []LiveWorktree{
		{TicketID: "a", Title: "Phase 1", WorktreePath: "/wt/feature"},
		{TicketID: "b", Title: "Other", WorktreePath: "/wt/other"},
	}

	tests := []struct {
		name         string
		worktreePath string
		selfTicketID string
		live         []LiveWorktree
		wantConflict string // "" = expect nil, else the conflicting TicketID
	}{
		{
			name:         "free worktree returns nil",
			worktreePath: "/wt/fresh",
			selfTicketID: "z",
			live:         live,
			wantConflict: "",
		},
		{
			name:         "occupied by another ticket conflicts",
			worktreePath: "/wt/feature",
			selfTicketID: "z",
			live:         live,
			wantConflict: "a",
		},
		{
			name:         "resuming own worktree does not conflict",
			worktreePath: "/wt/feature",
			selfTicketID: "a",
			live:         live,
			wantConflict: "",
		},
		{
			name:         "empty worktree path never conflicts",
			worktreePath: "",
			selfTicketID: "z",
			live:         live,
			wantConflict: "",
		},
		{
			name:         "no live agents returns nil",
			worktreePath: "/wt/feature",
			selfTicketID: "z",
			live:         nil,
			wantConflict: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := WorktreeConflict(tt.worktreePath, tt.selfTicketID, tt.live)
			if tt.wantConflict == "" {
				if got != nil {
					t.Errorf("expected no conflict, got ticket %q", got.TicketID)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected conflict with %q, got nil", tt.wantConflict)
			}
			if got.TicketID != tt.wantConflict {
				t.Errorf("got conflict %q, want %q", got.TicketID, tt.wantConflict)
			}
		})
	}
}

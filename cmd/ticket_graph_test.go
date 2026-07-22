package cmd

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/project"
)

// runTicketNew resets ticketNew* flags, applies mutate (which sets the flags
// under test, including --json), invokes ticketNewCmd.RunE, and returns
// captured stdout + the RunE error. Config isolation is the caller's job
// (newTicketTestProject sets OPENKANBAN_CONFIG_DIR).
func runTicketNew(t *testing.T, mutate func()) (string, error) {
	t.Helper()
	resetTicketNewFlags()
	ticketNewProject, ticketNewTitle = "", ""
	t.Cleanup(resetTicketNewFlags)
	mutate()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	runErr := ticketNewCmd.RunE(ticketNewCmd, nil)
	os.Stdout = orig
	w.Close()
	out, _ := io.ReadAll(r)
	return string(out), runErr
}

func parseTicketResult(t *testing.T, out string) ticketNewResult {
	t.Helper()
	var res ticketNewResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &res); err != nil {
		t.Fatalf("unmarshal ticket result %q: %v", out, err)
	}
	return res
}

func TestTicketNew_BlockedBy_LinksValidatesAndPersists(t *testing.T) {
	proj := newTicketTestProject(t, "graph")

	outA, err := runTicketNew(t, func() {
		ticketNewProject, ticketNewTitle, ticketNewJSON = proj.ID, "Plan the feature", true
	})
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	a := parseTicketResult(t, outA)

	outB, err := runTicketNew(t, func() {
		ticketNewProject, ticketNewTitle, ticketNewJSON = proj.ID, "Implement the feature", true
		ticketNewBlockedBy = a.ID
	})
	if err != nil {
		t.Fatalf("create B: %v", err)
	}
	b := parseTicketResult(t, outB)

	if len(b.BlockedBy) != 1 || b.BlockedBy[0] != a.ID {
		t.Fatalf("B.blocked_by = %v, want [%s]", b.BlockedBy, a.ID)
	}

	// Persisted to disk, not just echoed.
	registry, err := project.LoadRegistry()
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	reloaded, _, found := findTicketAcrossProjects(registry, board.TicketID(b.ID))
	if !found {
		t.Fatalf("reload B %s: not found", b.ID)
	}
	if len(reloaded.BlockedBy) != 1 || string(reloaded.BlockedBy[0]) != a.ID {
		t.Errorf("persisted BlockedBy = %v, want [%s]", reloaded.BlockedBy, a.ID)
	}
}

func TestTicketNew_BlockedBy_UnknownIDErrors(t *testing.T) {
	proj := newTicketTestProject(t, "graph")

	_, err := runTicketNew(t, func() {
		ticketNewProject, ticketNewTitle, ticketNewJSON = proj.ID, "Implement", true
		ticketNewBlockedBy = "00000000-0000-4000-8000-000000000000"
	})
	if err == nil {
		t.Fatal("expected error for unknown --blocked-by id, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %v, want it to mention 'not found'", err)
	}
}

func TestTicketNew_WorktreeFrom_AdoptsWorktree(t *testing.T) {
	proj := newTicketTestProject(t, "graph")

	outA, err := runTicketNew(t, func() {
		ticketNewProject, ticketNewTitle, ticketNewJSON = proj.ID, "Phase one", true
		ticketNewWorktree = true // provision a real worktree
	})
	if err != nil {
		t.Fatalf("create A with worktree: %v", err)
	}
	a := parseTicketResult(t, outA)
	if a.WorktreePath == "" {
		t.Fatalf("A has no worktree path; --worktree did not provision")
	}

	outB, err := runTicketNew(t, func() {
		ticketNewProject, ticketNewTitle, ticketNewJSON = proj.ID, "Phase two", true
		ticketNewWorktreeFrom = a.ID
	})
	if err != nil {
		t.Fatalf("create B with worktree-from: %v", err)
	}
	b := parseTicketResult(t, outB)

	if b.WorktreePath != a.WorktreePath {
		t.Errorf("B worktree = %q, want adopted %q", b.WorktreePath, a.WorktreePath)
	}
	if b.BranchName != a.BranchName {
		t.Errorf("B branch = %q, want adopted %q", b.BranchName, a.BranchName)
	}
}

func TestTicketNew_WorktreeFrom_ContradictsWorktree(t *testing.T) {
	proj := newTicketTestProject(t, "graph")

	_, err := runTicketNew(t, func() {
		ticketNewProject, ticketNewTitle, ticketNewJSON = proj.ID, "Bad combo", true
		ticketNewWorktree = true
		ticketNewWorktreeFrom = "some-id"
	})
	if err == nil || !strings.Contains(err.Error(), "contradictory") {
		t.Fatalf("expected contradiction error, got %v", err)
	}
}

func TestTicketNew_DerivesProjectFromSessionEnv(t *testing.T) {
	proj := newTicketTestProject(t, "graph")

	// Seed a ticket in the project to act as the "current session" ticket.
	outA, err := runTicketNew(t, func() {
		ticketNewProject, ticketNewTitle, ticketNewJSON = proj.ID, "Session ticket", true
	})
	if err != nil {
		t.Fatalf("create seed ticket: %v", err)
	}
	a := parseTicketResult(t, outA)

	// Now create WITHOUT --project, as an agent inside that session would.
	t.Setenv("OPENKANBAN_TICKET_ID", a.ID)
	outB, err := runTicketNew(t, func() {
		ticketNewProject, ticketNewTitle, ticketNewJSON = "", "Spun-off ticket", true
	})
	if err != nil {
		t.Fatalf("create with derived project: %v", err)
	}
	b := parseTicketResult(t, outB)
	if b.ProjectID != proj.ID {
		t.Errorf("derived project = %q, want %q", b.ProjectID, proj.ID)
	}
}

func TestTicketNew_NoProjectNoSessionErrors(t *testing.T) {
	newTicketTestProject(t, "graph") // sets isolated config + clears session env

	_, err := runTicketNew(t, func() {
		ticketNewProject, ticketNewTitle, ticketNewJSON = "", "Orphan", true
	})
	if err == nil || !strings.Contains(err.Error(), "--project is required") {
		t.Fatalf("expected --project required error, got %v", err)
	}
}

package ui

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/config"
	"github.com/techdufus/openkanban/internal/daemonclient"
	"github.com/techdufus/openkanban/internal/project"
)

func TestEditorArgv(t *testing.T) {
	const path = "/tmp/openkanban-ticket-x.md"
	tests := []struct {
		name   string
		visual string
		editor string
		want   []string
	}{
		{name: "VISUAL wins over EDITOR", visual: "nvim", editor: "vi", want: []string{"nvim", path}},
		{name: "EDITOR used when VISUAL empty", visual: "", editor: "vim", want: []string{"vim", path}},
		{name: "both unset falls back to vi", visual: "", editor: "", want: []string{"vi", path}},
		{name: "editor with args is split", visual: "", editor: "code --wait", want: []string{"code", "--wait", path}},
		{name: "whitespace-only VISUAL ignored", visual: "   ", editor: "emacs", want: []string{"emacs", path}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("VISUAL", tt.visual)
			t.Setenv("EDITOR", tt.editor)
			got := editorArgv(path)
			if len(got) != len(tt.want) {
				t.Fatalf("editorArgv = %v, want %v", got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("editorArgv = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

// newEditBodyTestModel builds a Model with a single ticket in an isolated
// store, ready for applyEditorResult. Returns the model and the ticket.
func newEditBodyTestModel(t *testing.T) (*Model, *board.Ticket) {
	t.Helper()
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	proj := &project.Project{ID: "test", RepoPath: t.TempDir()}
	globalStore := project.NewGlobalTicketStore(nil)
	globalStore.AddProject(proj)

	ticket := &board.Ticket{ID: "T-edit", Title: "edit me", ProjectID: "test", Status: board.StatusBacklog, Description: "original"}
	if err := globalStore.Add(ticket); err != nil {
		t.Fatalf("Add: %v", err)
	}

	cols := board.DefaultColumns()
	m := &Model{
		globalStore:     globalStore,
		panes:           map[board.TicketID]*daemonclient.PaneView{},
		columns:         cols,
		columnTickets:   make([][]*board.Ticket, len(cols)),
		columnOffsets:   make([]int, len(cols)),
		mode:            ModeNormal,
		sortMode:        SortPriority,
		width:           120,
		height:          40,
		config:          &config.Config{Agents: map[string]config.AgentConfig{}},
		selectedProject: proj,
	}
	m.refreshColumnTickets()
	return m, ticket
}

// writeTempFile writes content to a fresh temp .md and returns its path. It is
// NOT cleaned up here — applyEditorResult removes it, which the tests verify.
func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "edited.md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func TestApplyEditorResult_ChangedContentPersists(t *testing.T) {
	m, ticket := newEditBodyTestModel(t)
	path := writeTempFile(t, "a much longer\nmarkdown body\n")

	if !m.applyEditorResult(editorFinishedMsg{ticketID: ticket.ID, path: path}) {
		t.Fatal("expected applyEditorResult to report a change")
	}
	if got := ticket.Description; got != "a much longer\nmarkdown body" {
		t.Fatalf("Description = %q, want trailing-newline-trimmed body", got)
	}
	// Persisted to disk: reload from the store's file should carry the edit.
	reloaded, err := m.globalStore.Get(ticket.ID)
	if err != nil || reloaded == nil {
		t.Fatalf("Get after save: %v", err)
	}
	if reloaded.Description != "a much longer\nmarkdown body" {
		t.Fatalf("stored Description = %q", reloaded.Description)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("temp file %s was not removed", path)
	}
}

func TestApplyEditorResult_AbortLeavesUnchanged(t *testing.T) {
	m, ticket := newEditBodyTestModel(t)
	path := writeTempFile(t, "this edit should be ignored")

	if m.applyEditorResult(editorFinishedMsg{ticketID: ticket.ID, path: path, err: os.ErrProcessDone}) {
		t.Fatal("expected no change on editor abort")
	}
	if ticket.Description != "original" {
		t.Fatalf("Description = %q, want unchanged", ticket.Description)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("temp file %s was not removed on abort", path)
	}
}

func TestApplyEditorResult_EmptyLeavesUnchanged(t *testing.T) {
	m, ticket := newEditBodyTestModel(t)
	path := writeTempFile(t, "   \n\n\t\n")

	if m.applyEditorResult(editorFinishedMsg{ticketID: ticket.ID, path: path}) {
		t.Fatal("expected no change on empty/whitespace-only edit")
	}
	if ticket.Description != "original" {
		t.Fatalf("Description = %q, want unchanged", ticket.Description)
	}
}

func TestApplyEditorResult_IdenticalLeavesUnchanged(t *testing.T) {
	m, ticket := newEditBodyTestModel(t)
	// Same content (with a trailing newline the editor would add) is a no-op.
	path := writeTempFile(t, "original\n")

	if m.applyEditorResult(editorFinishedMsg{ticketID: ticket.ID, path: path}) {
		t.Fatal("expected no change when content is identical")
	}
	if ticket.Description != "original" {
		t.Fatalf("Description = %q, want unchanged", ticket.Description)
	}
}

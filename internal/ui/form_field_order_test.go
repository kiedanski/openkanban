package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textinput"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/project"
	"github.com/techdufus/openkanban/internal/testutil"
)

// formFieldOrderModel builds a Model with every form input wired so the
// ticket form can be rendered and navigated end-to-end.
func formFieldOrderModel(t *testing.T) *Model {
	t.Helper()
	testutil.NewTestEnv(t)
	gs := project.NewGlobalTicketStore(nil)
	proj := &project.Project{ID: "p1", Name: "demo", RepoPath: "/tmp/demo"}
	gs.AddProject(proj)

	m := newCreateModel(t, proj, gs)
	m.projectInput = textinput.New()
	m.blockerFilterInput = textinput.New()
	return m
}

// TestFormFieldTabOrder pins the keyboard (Tab) navigation order. Project
// must sit between Description and Branch, and the cycle must wrap from the
// last field (Blocked By) back to Title. This guards the constant numbering
// in model.go AND the wrap bounds in nextFormField/prevFormField.
func TestFormFieldTabOrder(t *testing.T) {
	m := formFieldOrderModel(t)

	want := []int{
		formFieldTitle,
		formFieldDescription,
		formFieldProject,
		formFieldBranch,
		formFieldLabels,
		formFieldPriority,
		formFieldType,
		formFieldWorktree,
		formFieldBlockedBy,
	}

	// Forward cycle, including the wrap back to Title.
	m.ticketFormField = formFieldTitle
	for i := 1; i <= len(want); i++ {
		m.nextFormField(false)
		expected := want[i%len(want)]
		if m.ticketFormField != expected {
			t.Fatalf("nextFormField step %d: got field %d, want %d", i, m.ticketFormField, expected)
		}
	}

	// Backward from Title must wrap to the last field (Blocked By).
	m.ticketFormField = formFieldTitle
	m.prevFormField(false)
	if m.ticketFormField != formFieldBlockedBy {
		t.Fatalf("prevFormField from Title: got %d, want formFieldBlockedBy (%d)", m.ticketFormField, formFieldBlockedBy)
	}
}

// TestFormRenderOrder pins the VISUAL order of the rendered form: the
// Project block must appear after Description and before Branch. This guards
// the literal append sequence in renderTicketForm(), which is independent of
// the constant numbering.
func TestFormRenderOrder(t *testing.T) {
	m := formFieldOrderModel(t)
	m.ticketFormField = formFieldTitle

	out := m.renderTicketForm()

	descIdx := strings.Index(out, "Description")
	projIdx := strings.Index(out, "Project")
	branchIdx := strings.Index(out, "Branch")

	if descIdx < 0 || projIdx < 0 || branchIdx < 0 {
		t.Fatalf("missing field labels in form: desc=%d proj=%d branch=%d\n%s", descIdx, projIdx, branchIdx, out)
	}
	if !(descIdx < projIdx && projIdx < branchIdx) {
		t.Fatalf("field order wrong: want Description(%d) < Project(%d) < Branch(%d)", descIdx, projIdx, branchIdx)
	}
}

var _ = board.StatusBacklog

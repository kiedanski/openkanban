package ui

import (
	"os"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/techdufus/openkanban/internal/board"
)

// editorFinishedMsg is delivered by the tea.ExecProcess callback after the
// external editor exits. It carries the ticket being edited, the temp file the
// editor wrote, and any launch/exit error. Wired through Update (never View).
type editorFinishedMsg struct {
	ticketID board.TicketID
	path     string
	err      error
}

// editorArgv resolves the editor command (respecting $VISUAL then $EDITOR,
// falling back to vi) and returns the full argv with the target path appended.
// strings.Fields lets values like "code --wait" or "emacsclient -nw" work; it
// does not handle shell-quoted paths with spaces, which is acceptable for the
// terminal-editor use case this targets. Never returns fewer than 2 elements.
func editorArgv(path string) []string {
	editor := os.Getenv("VISUAL")
	if strings.TrimSpace(editor) == "" {
		editor = os.Getenv("EDITOR")
	}
	parts := strings.Fields(editor)
	if len(parts) == 0 {
		parts = []string{"vi"}
	}
	return append(parts, path)
}

// editTicketBodyInEditor opens the selected ticket's description in $EDITOR on a
// temp markdown file. The temp file holds ONLY the description body (never the
// ticket file's YAML frontmatter), so the user edits pure markdown. The result
// is applied in the editorFinishedMsg handler on the Update loop.
func (m *Model) editTicketBodyInEditor() (tea.Model, tea.Cmd) {
	ticket := m.selectedTicket()
	if ticket == nil {
		m.notify("No ticket selected")
		return m, nil
	}

	tmp, err := os.CreateTemp("", "openkanban-ticket-*.md")
	if err != nil {
		m.notify("Could not open editor: " + err.Error())
		return m, nil
	}
	if _, err := tmp.WriteString(ticket.Description); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		m.notify("Could not open editor: " + err.Error())
		return m, nil
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		m.notify("Could not open editor: " + err.Error())
		return m, nil
	}

	path := tmp.Name()
	ticketID := ticket.ID
	argv := editorArgv(path)
	c := exec.Command(argv[0], argv[1:]...)
	return m, tea.ExecProcess(c, func(err error) tea.Msg {
		return editorFinishedMsg{ticketID: ticketID, path: path, err: err}
	})
}

// applyEditorResult reads the edited temp file back and, if it holds a real
// change, writes it onto the ticket's description and persists. It removes the
// temp file regardless. Returns true when the ticket was changed + saved.
//
// No-change cases (ticket left untouched, matching the acceptance criteria):
//   - editor aborted / exited non-zero (msg.err != nil, e.g. vim :q!)
//   - temp file unreadable
//   - empty/whitespace-only result (defends against an editor that writes
//     nothing on a botched exit — clearing a description stays possible via the
//     inline textarea)
//   - result identical to the current description (a no-op edit)
func (m *Model) applyEditorResult(msg editorFinishedMsg) bool {
	defer os.Remove(msg.path)

	if msg.err != nil {
		m.notify("Edit cancelled")
		return false
	}

	data, err := os.ReadFile(msg.path)
	if err != nil {
		m.notify("Could not read edited description: " + err.Error())
		return false
	}

	// Match ticket_md.go MarshalTicket normalization (trailing newlines
	// stripped) so a no-op edit compares equal to the stored description.
	newDesc := strings.TrimRight(string(data), "\n")
	if strings.TrimSpace(newDesc) == "" {
		return false
	}

	ticket, _ := m.globalStore.Get(msg.ticketID)
	if ticket == nil {
		return false
	}
	if newDesc == ticket.Description {
		return false
	}

	ticket.Description = newDesc
	ticket.Touch()
	m.saveTicket(ticket)
	// Keep focus on the edited ticket — mirrors saveTicketForm's edit branch.
	m.refreshColumnTickets()
	m.selectTicketByID(ticket.ID)
	m.notify("Updated description: " + ticket.Title)
	return true
}

package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/config"
	"github.com/techdufus/openkanban/internal/daemonclient"
)

// hintsAt renders the footer hint line for m at the given width budget, mirroring
// the sep/hintStyle the real renderStatusBar call site uses.
func hintsAt(m *Model, budget int) string {
	sep := lipgloss.NewStyle().Foreground(m.colors.overlay).Render(" │ ")
	hintStyle := lipgloss.NewStyle().Foreground(m.colors.subtext)
	return m.contextualHints(hintStyle, sep, budget)
}

const truncCue = "…"

// TestContextualHints_NormalTruncation walks the default ModeNormal hint line
// (no ticket, no filter) across every width from tiny to oversized and asserts
// the width-aware packing invariants.
func TestContextualHints_NormalTruncation(t *testing.T) {
	run := func(t *testing.T, m *Model) {
		fullW := lipgloss.Width(hintsAt(m, 1_000_000)) // everything, no cue
		floorW := lipgloss.Width(hintsAt(m, 1))        // pinned-only + cue (best effort)

		for budget := 1; budget <= fullW+5; budget++ {
			out := hintsAt(m, budget)

			// Pin invariant: ? help and q quit survive at every width.
			if !strings.Contains(out, "help") {
				t.Fatalf("budget=%d: pinned 'help' missing: %q", budget, out)
			}
			if !strings.Contains(out, "quit") {
				t.Fatalf("budget=%d: pinned 'quit' missing: %q", budget, out)
			}

			if budget >= fullW {
				if strings.Contains(out, truncCue) {
					t.Errorf("budget=%d >= fullW=%d: unexpected truncation cue: %q", budget, fullW, out)
				}
				for _, lbl := range []string{"nav", "settings", "sidebar", "global"} {
					if !strings.Contains(out, lbl) {
						t.Errorf("budget=%d: expected full line to contain %q: %q", budget, lbl, out)
					}
				}
				continue
			}

			// Truncated: cue must be shown.
			if !strings.Contains(out, truncCue) {
				t.Errorf("budget=%d < fullW=%d: missing truncation cue: %q", budget, fullW, out)
			}
			// Cue must sit left of the pinned `? help` so it reads "… │ ? help".
			if ci, hi := strings.Index(out, truncCue), strings.Index(out, "help"); ci >= hi {
				t.Errorf("budget=%d: cue index %d not before help index %d: %q", budget, ci, hi, out)
			}
			// Above the pinned floor the line must actually fit the budget —
			// this is the mechanical no-mid-key-clip guarantee.
			if budget >= floorW {
				if w := lipgloss.Width(out); w > budget {
					t.Errorf("budget=%d: rendered width %d exceeds budget: %q", budget, w, out)
				}
			}
		}
	}

	t.Run("zero-value colors", func(t *testing.T) {
		run(t, &Model{mode: ModeNormal})
	})
	t.Run("real theme (exercises SGR width accounting)", func(t *testing.T) {
		run(t, &Model{mode: ModeNormal, colors: newUIColors(config.DefaultConfig().GetTheme())})
	})
}

// TestContextualHints_NormalDropOrder asserts the lowest-priority tail keys drop
// first while higher-priority core keys survive.
func TestContextualHints_NormalDropOrder(t *testing.T) {
	m := &Model{mode: ModeNormal}
	fullW := lipgloss.Width(hintsAt(m, 1_000_000))

	// Just under full width: only the lowest-prio hint (E $EDITOR, prio 0)
	// drops; the next-lowest (O settings) now survives.
	out := hintsAt(m, fullW-1)
	if strings.Contains(out, "EDITOR") {
		t.Errorf("at fullW-1 'E $EDITOR' (lowest prio) should drop: %q", out)
	}
	for _, lbl := range []string{"settings", "sidebar", "search", "nav"} {
		if !strings.Contains(out, lbl) {
			t.Errorf("at fullW-1 higher-prio %q should survive: %q", lbl, out)
		}
	}

	// Very narrow: the core nav + pinned help/quit remain; tail is gone.
	narrow := hintsAt(m, 24)
	for _, gone := range []string{"settings", "sidebar", "global", "sort", "filter"} {
		if strings.Contains(narrow, gone) {
			t.Errorf("at width 24 %q should be dropped: %q", gone, narrow)
		}
	}
	for _, kept := range []string{"help", "quit"} {
		if !strings.Contains(narrow, kept) {
			t.Errorf("at width 24 pinned %q should survive: %q", kept, narrow)
		}
	}
}

// TestContextualHints_FloorToZero proves the pin invariant holds even when the
// budget is squeezed to zero (e.g. a wide notification badge crowds the bar).
func TestContextualHints_FloorToZero(t *testing.T) {
	m := &Model{mode: ModeNormal}
	out := hintsAt(m, 0)
	for _, kept := range []string{"help", "quit"} {
		if !strings.Contains(out, kept) {
			t.Errorf("budget=0: pinned %q must still render: %q", kept, out)
		}
	}
	// Nothing non-pinned should survive a zero budget.
	for _, gone := range []string{"settings", "nav", "new"} {
		if strings.Contains(out, gone) {
			t.Errorf("budget=0: %q should be dropped: %q", gone, out)
		}
	}
}

// TestContextualHints_CueBoundary checks the cue width is reserved from the
// rendered cue string (not a guessed constant): across the truncation range the
// rendered width never exceeds the budget once we're above the pinned floor.
func TestContextualHints_CueBoundary(t *testing.T) {
	m := &Model{mode: ModeNormal}
	fullW := lipgloss.Width(hintsAt(m, 1_000_000))
	floorW := lipgloss.Width(hintsAt(m, 1))

	for budget := floorW; budget < fullW; budget++ {
		out := hintsAt(m, budget)
		if !strings.Contains(out, truncCue) {
			t.Errorf("budget=%d: expected cue: %q", budget, out)
		}
		if w := lipgloss.Width(out); w > budget {
			t.Errorf("budget=%d: width %d exceeds budget (cue not reserved correctly): %q", budget, w, out)
		}
	}
}

// TestContextualHints_OtherModes spot-checks non-normal modes truncate without
// panic, keep their pinned exit key, and drop their lowest-priority item first.
func TestContextualHints_OtherModes(t *testing.T) {
	tests := []struct {
		name    string
		mode    Mode
		budget  int
		keep    []string // must survive
		dropped []string // must be gone
	}{
		{
			name:    "filter drops @project hint first",
			mode:    ModeFilter,
			budget:  20,
			keep:    []string{"cancel"},
			dropped: []string{"@project"},
		},
		{
			name:    "agent view drops shift+click note first",
			mode:    ModeAgentView,
			budget:  16,
			keep:    []string{"board"},
			dropped: []string{"Shift+click"},
		},
		{
			name:    "settings keeps close",
			mode:    ModeSettings,
			budget:  12,
			keep:    []string{"close"},
			dropped: []string{"navigate"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Model{mode: tt.mode}
			out := hintsAt(m, tt.budget)
			if !strings.Contains(out, truncCue) {
				t.Errorf("expected truncation cue at budget=%d: %q", tt.budget, out)
			}
			for _, lbl := range tt.keep {
				if !strings.Contains(out, lbl) {
					t.Errorf("expected %q to survive: %q", lbl, out)
				}
			}
			for _, lbl := range tt.dropped {
				if strings.Contains(out, lbl) {
					t.Errorf("expected %q to be dropped: %q", lbl, out)
				}
			}
			// No-mid-key-clip guarantee for non-Normal modes too: above the
			// pinned floor the rendered line must fit the budget.
			if floorW := lipgloss.Width(hintsAt(m, 1)); tt.budget >= floorW {
				if w := lipgloss.Width(out); w > tt.budget {
					t.Errorf("width %d exceeds budget %d: %q", w, tt.budget, out)
				}
			}
		})
	}
}

// TestContextualHints_AutoMode locks in the footer surfaces for Auto mode:
// the agent-view Ctrl+G label flips to "next waiter (Auto)" when armed, and the
// board surfaces the 'a' toggle with an on/off label. Rendered at full width so
// the (non-pinned) board 'a' hint isn't dropped by packing.
func TestContextualHints_AutoMode(t *testing.T) {
	const wide = 1_000_000

	t.Run("agent-view Ctrl+G label reflects Auto state", func(t *testing.T) {
		off := hintsAt(&Model{mode: ModeAgentView}, wide)
		if !strings.Contains(off, "board") {
			t.Errorf("Auto off: Ctrl+G should read 'board': %q", off)
		}
		if strings.Contains(off, "next waiter") {
			t.Errorf("Auto off: 'next waiter' must not appear: %q", off)
		}

		on := hintsAt(&Model{mode: ModeAgentView, autoAttach: true}, wide)
		if !strings.Contains(on, "next waiter (Auto)") {
			t.Errorf("Auto on: Ctrl+G should read 'next waiter (Auto)': %q", on)
		}
		if strings.Contains(on, "board") {
			t.Errorf("Auto on: 'board' must not appear (Ctrl+G repurposed): %q", on)
		}
	})

	t.Run("board surfaces 'a' toggle with on/off label", func(t *testing.T) {
		off := hintsAt(&Model{mode: ModeNormal}, wide)
		if !strings.Contains(off, "auto") {
			t.Errorf("Auto off: board footer should surface the 'a' auto hint: %q", off)
		}
		if strings.Contains(off, "auto on") {
			t.Errorf("Auto off: label should be 'auto', not 'auto on': %q", off)
		}

		on := hintsAt(&Model{mode: ModeNormal, autoAttach: true}, wide)
		if !strings.Contains(on, "auto on") {
			t.Errorf("Auto on: board footer should read 'auto on': %q", on)
		}
	})

	t.Run("in_progress ticket branch also surfaces the auto hint", func(t *testing.T) {
		ticket := &board.Ticket{ID: board.NewTicketID(), Status: board.StatusInProgress}
		m := &Model{
			mode:          ModeNormal,
			autoAttach:    true,
			columnTickets: [][]*board.Ticket{{ticket}},
		}
		out := hintsAt(m, wide)
		if !strings.Contains(out, "spawn agent") {
			t.Fatalf("expected the in_progress branch (anchor 'spawn agent'): %q", out)
		}
		if !strings.Contains(out, "auto on") {
			t.Errorf("in_progress branch should surface 'auto on' when armed: %q", out)
		}
	})
}

// TestContextualHints_NormalTicketBranches covers the two ModeNormal branches
// that depend on a selected ticket (a spawned pane vs. an in_progress ticket
// without one). Both pin `? help`; neither carries `q quit`. These branches see
// the most keybinding churn, so lock in the pin + truncation invariants.
func TestContextualHints_NormalTicketBranches(t *testing.T) {
	ticket := &board.Ticket{ID: board.NewTicketID(), Status: board.StatusInProgress}
	base := func() *Model {
		return &Model{
			mode:          ModeNormal,
			columnTickets: [][]*board.Ticket{{ticket}},
		}
	}

	tests := []struct {
		name       string
		model      *Model
		wantAnchor string // a key only this branch surfaces
	}{
		{
			name:       "in_progress ticket without pane",
			model:      base(),
			wantAnchor: "spawn agent",
		},
		{
			name: "ticket with a spawned pane",
			model: func() *Model {
				m := base()
				m.panes = map[board.TicketID]*daemonclient.PaneView{ticket.ID: nil}
				return m
			}(),
			wantAnchor: "open agent",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := tt.model
			// Full width: the branch-specific anchor is present, no cue.
			full := hintsAt(m, 1_000_000)
			if !strings.Contains(full, tt.wantAnchor) {
				t.Fatalf("expected %q in full line: %q", tt.wantAnchor, full)
			}
			if strings.Contains(full, truncCue) {
				t.Errorf("unexpected cue at full width: %q", full)
			}
			fullW := lipgloss.Width(full)
			floorW := lipgloss.Width(hintsAt(m, 1))

			for budget := floorW; budget < fullW; budget++ {
				out := hintsAt(m, budget)
				if !strings.Contains(out, "help") {
					t.Fatalf("budget=%d: pinned 'help' missing: %q", budget, out)
				}
				if !strings.Contains(out, truncCue) {
					t.Errorf("budget=%d: missing cue: %q", budget, out)
				}
				if w := lipgloss.Width(out); w > budget {
					t.Errorf("budget=%d: width %d exceeds budget: %q", budget, w, out)
				}
			}
		})
	}
}

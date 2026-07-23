# Edit ticket body in $EDITOR (vim) from the TUI

<!-- openkanban:card-notes start -->
## Notes (from openkanban card)

# Edit a ticket's body in $EDITOR (vim) from the TUI

## Goal
Let the user press a key in openkanban to open the current ticket's description
in $EDITOR (vim), edit it as markdown, save, and have the ticket updated on
return. The inline textarea is cramped for longer task descriptions; editing in
vim (full markdown + motions) is the ask.

## Current state (verified)
- Description field is a Bubble Tea `textarea` (internal/ui/model.go: descInput,
  formFieldDescription) -- inline multiline, no external editor.
- NO in-TUI $EDITOR launching exists (no tea.Exec/ExecProcess/EDITOR in internal/ui).
- cmeid already stores each ticket as .md and has fsnotify HOT-RELOAD
  (internal/watch/watcher.go): editing the .md in vim in ANOTHER terminal already
  refreshes the running TUI (~100ms). This ticket is about doing it FROM the TUI
  with one keypress.

## Approach
- Standard Bubble Tea `tea.ExecProcess($EDITOR tmpfile, callback)`: suspend the
  TUI, run $EDITOR (fallback vi) on a temp .md seeded with the current description
  (or open the ticket's real tickets/<slug>.md and lean on hot-reload), read it
  back on exit, load into descInput / save the ticket.
- Keybinding: `e` is already "edit ticket"; add e.g. `E`, or a key inside the
  create/edit form on formFieldDescription, to "open in $EDITOR".
- Respect $VISUAL/$EDITOR; fall back to vi. Aborted/empty edit => no change.

## Relevant files
- internal/ui/model.go -- descInput (textarea), create/edit form + keybindings,
  saveTicketForm. Add the tea.ExecProcess flow + a Msg handler for the result.
- internal/ui/CLAUDE.md -- BOTH keybinding doc surfaces (contextualHints +
  renderHelp) must be updated when adding a key.
- internal/watch/watcher.go -- hot-reload (relevant if opening the real .md).

## Gotchas
- Keep both keybinding doc surfaces in sync (internal/ui/CLAUDE.md).
- tea.ExecProcess result is a Msg -- wire through Update, never View.
- Do NOT trigger a modal/dialog that blocks the PTY.

## Out of scope
- Markdown rendering/preview (glamour) -- separate ticket if wanted.
- Editing arbitrary fields in $EDITOR -- description/body only.

## Acceptance
- Pressing the bound key opens $EDITOR on the ticket's markdown; on save+quit the
  description reflects the edit.
- $EDITOR unset falls back to vi; aborting leaves the ticket unchanged.
- go build ./... && go test ./... green.
<!-- openkanban:card-notes end -->

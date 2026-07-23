# Task types -> agent roles + workflow gates

<!-- openkanban:card-notes start -->
## Notes (from openkanban card)

# Task types -> agent roles + workflow gates (the "crown jewel")

## Goal
Add first-class task TYPES to openkanban (research / spec / implement / review)
that (a) spawn a specialized Claude agent ROLE per type and (b) enforce workflow
GATES between them (e.g. can't start an implement ticket without a done spec),
always overridable. Surface it in the TUI so the human is guided, not just the CLI.
Full design: ~/workspace/openkanban-fork-plan/1-ORCHESTRATION_DESIGN.md

## Scope
1. Data model: add `Type TicketType` to board.Ticket (research/spec/implement/
   review/freeform), persisted in .md frontmatter; empty parses as freeform.
2. Type -> role: add claude-research / claude-spec / claude-review to
   defaultAgents() (Command:"claude", differ by Args + InitPrompt). Bind
   ticket.Type -> AgentType at spawn. spec/research use plan mode (already the
   default for new claude sessions).
3. Gates: extend cmd/status_gate.go pattern into internal/workflow.CheckPrerequisite
   - implement can't START without a linked done spec (BlockedBy already exists)
   - review can't START without a linked implement in in_review+
   - override via --force (CLI) / ModeConfirm (TUI)
4. TUI surfacing (what Diego keeps expecting to see):
   - type picker in the create-ticket popup (new form field, after Priority)
   - worktree new-or-reuse choice at spawn
   - gate surfaces as an OFFER (new worktree / wait / override), not a bare block

## Open decisions (resolve with Diego first)
- Do research/spec tickets run read-only / no-worktree, or full worktree?
- Gate strictness: hard-block-with-override on START, warn-only on CREATE? softer?

## Builds on (shipped, branch feat/ticket-graph-and-worktree-gate)
- --blocked-by / --worktree-from CLI + project derivation from $OPENKANBAN_TICKET_ID
- worktree-exclusivity safety gate (internal/workflow.WorktreeConflict)
- spin-off-a-ticket / fan-out-a-plan skills

## Acceptance
- Creating a ticket in the TUI asks for its type.
- Starting an implement ticket with no done spec is blocked (with override).
- Each type spawns its role's agent (verify the InitPrompt differs).
- go build ./... && go test ./... green; new tests for CheckPrerequisite + type map.
<!-- openkanban:card-notes end -->

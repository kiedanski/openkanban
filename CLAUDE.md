# OpenKanban

Terminal-based kanban board with integrated AI agent spawning for ticket work.

## ⚠️ Before you edit code: get your own worktree (top-level sessions)

This clone — `/Users/cmeid/manifold/dev/openkanban` — is the **shared primary
working tree**. Many concurrent Claude sessions touch it at once. Agents that
openkanban spawns for a ticket already run in isolated worktrees; **any other
session must isolate itself the same way before editing code.** A sibling can
`git checkout`/`reset` this clone out from under you mid-task, and your commits
will silently land on the wrong branch (this has happened — see memory
`feedback_shared_clone_branch_hijack`).

**If your current directory is this primary clone (i.e. NOT a
`…/openkanban-worktrees/…` path), do this first — before any edit, build, or
commit:**

1. Create a ticket, which also provisions a dedicated worktree + branch:
   ```bash
   openkanban ticket new --project openkanban --title "<short task title>"
   ```
   Note the worktree path it reports (under `openkanban-worktrees/<slug>`), then
   `cd` into it. If openkanban isn't tracking this repo as a project, or you just
   need a branch fast, fall back to:
   ```bash
   git worktree add ../okwt-<slug> -b <branch> origin/main
   ```
2. Do **all** edits, builds, tests, commits, and the PR from that worktree.
3. Treat the primary clone as read-only — `git status` / `git log` are fine;
   `git checkout`, `git commit`, `git reset` are not.

(Sessions already running inside a ticket worktree are isolated — this doesn't
apply to you; carry on.)

## ⚠️ Remotes: `origin` must be your fork

openkanban treats the remote named **`origin`** as authoritative and has no
per-project setting to override it:

- New-ticket worktrees are cut from whatever `origin/HEAD` points to —
  `GetDefaultBranch()` runs `git symbolic-ref refs/remotes/origin/HEAD`
  (`internal/git/worktree.go`), falling back to local `main`/`master`.
- `openkanban update` self-updates by pulling `origin main`
  (`cmd/update.go`).

So in a fork-based clone, **`origin` must be the fork you develop and merge on**
(here `git@github.com:kiedanski/openkanban.git`), and the upstream you forked
from is kept as **`upstream`** (here `https://github.com/cmeid/openkanban.git`).
If `origin` points at upstream instead, every new ticket is cut from upstream
and the binary self-updates from upstream — so features you merged into your
fork never show up in new tickets. If you re-clone or re-add remotes, restore
this arrangement (and `git remote set-head origin -a`) before creating tickets.

## Stack

Go 1.21+, BubbleTea (TUI), creack/pty, charmbracelet/x/vt (terminal emulation; see [Terminal Emulator](docs/AGENT_INTEGRATION.md#architecture-terminal-emulator))

## Development

```bash
go build ./...        # Build (in-place; doesn't touch installed binary)
go test ./...         # Test
go run .              # Run (carries no install-time ldflags — fine for ad-hoc iteration)

./scripts/install.sh  # INSTALL — always use this; never bare `go install .`
                      # Run from main clone → updates $GOBIN/openkanban (global).
                      # Run from a worktree → builds ./openkanban locally; global install untouched.
openkanban update     # Update an existing install (pull + rebuild via the same path)
```

`scripts/install.sh` and `openkanban update` inject the `BuildMarker=official` ldflag that
`cmd/build_guard.go` requires. Bare `go install .` skips it and produces a stub that refuses
every command except `version` — and the guard fires at *invoke* time, not *build* time, so
an agent that runs `go install .` to verify a sibling change never sees the failure it
caused. It surfaces hours later in someone else's Stop hook. If you genuinely need a
quick install equivalent without the script, replicate the ldflags it sets (`SourcePath`,
`Commit`, `BuildMarker`) — see `scripts/install.sh`.

## Where to Look

| Task | Location |
|------|----------|
| Add CLI command | cmd/ |
| Modify UI/keybindings | internal/ui/ |
| Change agent behavior | internal/agent/ |
| Terminal/PTY handling | internal/terminal/ |
| Board/ticket logic | internal/board/ |
| Project management | internal/project/ |
| Configuration | internal/config/ |
| Git operations | internal/git/ |

## Architecture

```
cmd/           CLI entry (cobra)
internal/
  ui/          BubbleTea Model - central orchestrator
  agent/       Agent config, status detection, spawning prep
  terminal/    PTY management, charm/x/vt emulator wrapper, scrollback
  board/       Ticket/column data structures
  project/     Multi-project registry, settings cascade
  config/      JSON config, validation, themes
  git/         Worktree operations
```

## Key Flows

**Ticket → Agent spawn:** the TUI does **not** fork the agent directly. It sends a `Spawn` RPC to `openkanbankd`, which owns the PTY for the agent's lifetime; the TUI attaches via the daemon's binary stream. This is what lets a TUI restart without killing in-progress agents.

```
ui.spawnAgent() → m.daemon.Spawn (RPC) → openkanbankd handleSpawn → pty.Start → agent
                ↓
                m.daemon.Attach (binary stream) → PaneView → TUI rendering
```

`Spawn` is **idempotent per TicketID** — a second Spawn for an already-owned ticket returns the existing SessionID instead of creating a duplicate. This enforces the 1:1 ticket↔session invariant at the daemon. The TUI's quick-move / drag promotion out of `in_progress` mirrors the same invariant by sending `TicketDone` to the daemon so the session is wound down rather than orphaned. See `internal/daemon/CLAUDE.md` for the two-phase lock pattern and `internal/ui/CLAUDE.md` "Status-mutation wrap-up" for the TUI-side teardown.

**Settings cascade:**
ticket.Field → project.Settings.Field → config.Defaults.Field

## Agent Workflow

Scout finds → Librarian reads → You plan → Worker implements → Validator checks

## Guidance

Context-specific guidance lives in nested CLAUDE.md files:
- internal/CLAUDE.md - Go patterns, imports, testing
- internal/ui/CLAUDE.md - BubbleTea patterns, status-mutation wrap-up, daemonAPI shape
- internal/daemon/CLAUDE.md - openkanbankd internals, Spawn idempotency invariant
- internal/agent/CLAUDE.md - Agent integration
- internal/terminal/CLAUDE.md - PTY/terminal handling

## Relevant workspace memories

When spawned to work on this repo (e.g. from an openkanban ticket via a git worktree), the calling user's project-scoped memory directory may carry context that's not in this repo. For Chris (this fork's maintainer), those memories live at `~/.claude/projects/-Users-cmeid-manifold-dev/memory/`. Read these after `/prime` if they exist — they describe how openkanban is actually used here, not just how the code works:

- `project_openkanban_personal_fork.md` — the fork's diverged state, key features beyond upstream, why there's no upstream PR pending
- `feedback_openkanban_session_linking.md` — `openkanban ticket new --session` link vs migrate semantics, and the "don't `--migrate --force` from inside the session you're migrating" trap
- `feedback_openkanban_store_volatile.md` — openkanban's on-disk store is operational state, not source of truth; canonical ticket briefs live in repo `tickets/<slug>.md` files
- `reference_openkanban_dev_loop.md` — `update-openkanban` install script, fork remote setup, branch + commit conventions, the 50/72/no-AI commit hook

# UI Package

BubbleTea-based terminal UI with vim-style navigation.

## Model Structure

Single `Model` struct implements `tea.Model`:
- `Init()` - startup commands
- `Update(msg) (Model, Cmd)` - message handling
- `View() string` - render output

## Mode State Machine

`Mode` type controls behavior routing:
```go
type Mode string
const (
    ModeNormal    Mode = "normal"
    ModeAgentView Mode = "agent_view"
    ModeSettings  Mode = "settings"
    // ...
)
```

Key handlers dispatch by mode: `handleNormalMode()`, `handleAgentViewMode()`

## Key Bindings

Vim-style navigation:
- `h/j/k/l` - movement
- `g/G` - jump to start/end
- `n` - new item (in_review/done columns route the new ticket to in_progress)
- `d` - delete
- `Enter` - select/confirm
- `Esc` - cancel/back

`:` is **intentionally unhandled** in normal mode — it falls through `handleNormalMode`'s switch to a no-op. It once entered a `ModeCommand` stub (a husk present since the initial commit whose only behaviors bounced straight back to `ModeNormal`), removed as dead code. The key is deliberately left free so a future command-palette feature (`:q`, `:w`, `:e <ticket>`, …) can claim it cleanly. Don't re-add a no-op handler or repurpose `:` for an unrelated binding.

Inside ModeAgentView, these keys are intercepted before the PTY child (claude, etc.) sees them:
- `Ctrl+]` / `Ctrl+\` - cycle focus to next / prev open, unattached session
- `Ctrl+g` - leave this session. Destination is parameterized by **Auto mode** (`m.autoAttach`, toggled by board key `a`): Auto off → back to the board (the default); Auto on → jump to and **attach directly** to the session that has *needed attention* longest — `needsAttention` = waiting OR idle OR stuck (FIFO by `StatusChangedAt`), NOT working/none/completed/error — skipping sessions another TUI holds, falling through to the board when none qualifies. Direct attach (not the cycle Peek modal) is safe precisely because the attached-elsewhere skip means the target is never a peer's session. See `oldestWaitingPeer` / `needsAttention` / the `ExitFocusMsg` branch in `model.go`, the `AUTO` badge in `renderAgentView` (the agent view doesn't render `contextualHints`), and memory `reference_openkanban_auto_mode_feature_map`.
- `Enter` - **conditionally** intercepted via `shouldRetryAttachOnEnter`: only when the focused pane has `LastAttachErr() != nil` AND `State() != PaneViewAttached`. In that state Enter retries `attachExisting`; otherwise it falls through to the PTY child as normal. The predicate is gated the same way as `PaneView.View()`'s failure-overlay branch — keep them locked together, otherwise the user sees the "Enter retries" hint but pressing Enter does nothing.

`Ctrl+[` cannot be used: in bubbletea v1.3.x (no Kitty keyboard protocol enabled here) it is bytewise indistinguishable from `Esc`. Any new ctrl-combo binding should be verified against `~/golang/pkg/mod/github.com/charmbracelet/bubbletea@<ver>/key.go` before promising it.

**Getting a goroutine dump from a hung TUI:** the TUI has a built-in **stall watchdog** (`stallwatch.go`) — prefer it. `Update`/`View` stamp a phase + heartbeat into atomics; a 1s watchdog dumps all goroutine stacks + counters to `~/.cache/openkanban/tui-stall.log` (override `OPENKANBAN_TUI_STALL_LOG`) when a single Update/View runs >3s (`kind=in-call`) or Update idles >3s while the daemon keeps pushing events (`kind=starved`, i.e. parked outside Update/View on a `tea.Batch`/`tea.Sequence` `g.Wait`). One dump per episode; a marker also lands in `tui.log`. On demand: `kill -USR2 $(pgrep -n openkanban)`. `OPENKANBAN_DEBUG_STALL_MS` injects a one-shot synthetic in-call stall (test only). The watchdog is created in `NewModel` but only armed by `StartStallMonitor` (from `app.go`) so tests don't leak the ticker; `Cleanup` stops it. Why a file and not SIGQUIT: nothing traps SIGQUIT (bubbletea v1.3.10 `tea.go` Notifies only SIGINT/SIGTERM; `app.go:122` mirrors it), the runtime default prints stacks to stderr, but `app.go:124`'s `tea.WithAltScreen()` means those bytes land on the alt-screen buffer the terminal discards on exit — invisible. The **daemon** has the analogous SIGUSR1 → `daemon.log` handler (`internal/daemon/server.go:272`). Full recipe + fallbacks (pre-redirect stderr + `kill -QUIT`, `dlv attach`) in `docs/AGENT_INTEGRATION.md` → "Diagnosing a hung TUI".

The cycle-attach modal renders OVER the focused pane's agent view (chrome stays visible behind), via `renderAgentViewWithCycleModal`. Do not switch it back to `renderWithOverlay`, which uses a blank background and hides the state needed to make the cycle decision. `cycleUnattachedSession` **Peeks** the target peer if it's Unattached (one-shot snapshot via `PaneView.Peek`, no attach) so the modal backdrop shows its content without silently taking the session over from another TUI; the cycle iterates ALL open peers, not just Unattached ones. (It used to *auto-attach* the target — that silently displaced a peer TUI, which the takeover-warning work replaced with Peek.)

**Takeover-warning modal.** Attaching to a session attached in another TUI no longer silently takes over. The attach path probes with a plain `Attach` (`attachExisting` → `doAttach(...,false)`); the daemon rejects an already-attached session with `daemonclient.ErrAlreadyAttached` (the peer is left undisturbed), `doAttach` returns `attachConflictMsg`, and `armTakeoverPrompt` raises a confirm-default-cancel modal over the agent view (`renderAgentViewWithTakeoverModal`, intercepted in `handleAgentViewMode` before the cycle modal). Enter/y → `doAttach(...,true)` forced takeover (no re-probe); Esc/anything → board. TWO attach paths must stay covered: `attachExisting` (P1) AND the Owns cold-start fast-path `attachExistingFastPath`/`retryAttach` (P2) — the latter bypasses `attachExisting` and has its own `ErrAlreadyAttached`→`attachConflictMsg` arm. Full map: memory [[project_openkanban_takeover_warning_and_peek]].

While the modal is open (`cycleAttachPrompt == true`), `handleAgentViewMode` routes EVERY key to `handleCycleAttachPromptKey` before any pane dispatch — so a key the modal doesn't explicitly handle is swallowed, never reaching the PTY or the normal agent-view bindings. It handles `enter` (attach), `esc`/`ctrl+g` (exit to board), and `ctrl+]`/`ctrl+\` (keep cycling). `ctrl+g` MUST stay listed: it's the documented agent-view exit gesture, and without an explicit case the act of cycling silently disabled it until the user pressed `esc`.

**Invariant: `cycleAttachPrompt` must never outlive agent-view focus.** Every path that drops `m.focusedPane` goes through the `exitToBoard()` chokepoint (sets `mode=ModeNormal`, `focusedPane=""`, `cycleAttachPrompt=false`) — the keyboard exits AND the four async daemon paths that can fire while the modal is open: session `"exited"` (`daemon_subscribe.go`), `PaneDetachedMsg`, `PaneExitMsg`, `DaemonDisconnectedMsg`. If a new focus-drop path resets `mode`/`focusedPane` inline instead of calling `exitToBoard()`, the flag strands true and resurfaces as a phantom modal on the next agent-view entry — `ctrl+g` (and every other key) swallowed though the user never cycled. Pinned by `TestDaemonExitedClearsStaleCycleAttachPrompt`.

### Keep both doc surfaces synced

Every keybinding has **two doc surfaces** in `view.go`:

1. `contextualHints()` — the mode-aware footer line that's always visible. Surfaces the most relevant keys for the current mode/state. **Width-aware:** each hint is a `hintSpec{key, label, prio, pinned}` in an ordered `[]hintSpec`, and `packHints()` drops the lowest-`prio` non-`pinned` hints to fit the available width (no longer just packed-and-clipped). When anything drops, a dim `…` cue renders just before the first pinned hint (`? help`/`q quit` are pinned in ModeNormal), reading `… │ ? help │ q quit`. So adding a key here means picking its `prio` and deciding `pinned` — not just appending a hint.
2. `renderHelp()` — the `?` modal, the canonical "every shortcut" reference. Must list every binding.

When you add, remove, or rebind a key, update **both** functions in the same change. The modal must stay complete; the footer must surface the key (with a `prio`) in any mode where it's relevant. They live ~50 lines apart on purpose — see one, edit the other.

Keys that only apply while the **sidebar is focused** have a **third** surface: a hint line rendered directly inside `renderSidebar()` (e.g. `"  j/k ⏎toggle a/d o:open"`). It's width-budgeted to `m.sidebarWidth`, so keep tokens terse (`o:open`, not `o open only`). Sidebar-focused keys (handled in `handleSidebarNav`) must update all three: this in-sidebar line, the `contextualHints()` `sidebarFocused` branch, and the `renderHelp()` Sidebar section.

## View Composition

Separate render methods composed in `View()`:
- `renderHeader()`, `renderBoard()`, `renderColumn()`
- `renderTicket()`, `renderStatusBar()`

## Styling

All styling via lipgloss with theme-based `uiColors` struct.
Never use raw ANSI codes in UI rendering.

**Ticket card border color** (`renderTicket` → `ticketBorderColor`) is resolved
by precedence, highest first: **stuck** (red) > **selected** (static bright white,
`lipgloss.Color("15")`) > **viewed in another TUI** (amber, `m.daemonViewing[id] > 0` —
same signal as the `◉` badge) > **hovered** (overlay) > default (surface). It is
NOT gated on `pane.Running()` — that stale-on-the-board flag drove a removed green
"running" border (see git log `drop green running-session card border`). The
left-edge accent (`BorderLeftForeground`) is a separate signal driven by
`ticket.AgentStatus` (working=warning, waiting, idle, …). Precedence is pinned by
`TestTicketBorderColor`.

The **selected** border is intentionally **decoupled from `columnColor`** — it is a
static white regardless of which column the card is in or which project it belongs to.
`columnColor(status)` still drives the column header text and the active-column border
and follows the deliberate "color = meaning" scheme: backlog=overlay (quiet/neutral),
next=info, in_progress=**success (green)**, in_review=secondary, done=muted (grey).
in_progress is green and NOT `warning`/amber **on purpose** — amber is reserved for
the viewed-elsewhere border (and the working left-edge accent). Don't revert
in_progress to `warning`: that reintroduces the collision `TestColumnColorScheme`
guards against (for column headers / active-column border).

The board header **activity chip** (`renderHeader`) shows `<icon> N sessions · <breakdown>` —
the total `N` is the number of **open sessions** and the breakdown lists every non-zero status
bucket (working/waiting/idle/starting/stuck/error/done), so the counts always sum to `N`.

- **Count by `m.panes` membership, NOT `pane.Running()`.** A pane exists in `m.panes` iff the
  daemon session is open (panes are `Close()`d + deleted on every `"exited"` event — see
  `daemon_subscribe.go`). `pane.Running()` is the *cached* `lastInfo.Running` for an unattached
  pane; it goes stale while the user sits on the board (it isn't refreshed without an attach/list)
  and previously dropped genuinely-live sessions from the chip while their cards still rendered
  `working` (cards read `ticket.AgentStatus` directly, with no liveness gate). Do NOT reintroduce
  a `Running()` gate here. Pinned by `TestRenderHeaderActivityChipCountsAllOpenSessions` (its
  `w2` case is a stale-`Running()==false` working pane that must still count).
- **Bucket every status.** The old `switch` only credited working/waiting/idle, silently dropping
  live sessions in error/none/stuck/completed. The total must equal open sessions, so unknown/none
  maps to `starting`.
- The chip is intentionally pushed left of the right-edge help text by `const chipBannerGap` so it
  clears the top-right corner where macOS notification banners land. The chip is **right-anchored**
  (right edge = `width - help - gap`), so a longer breakdown grows leftward and the gap before
  `? help  q quit` is preserved. That gap is load-bearing — don't "tidy" it away; tune the constant
  to adjust banner clearance. Pinned by `TestRenderHeaderActivityChipClearsCorner`.

**Daemon wedge banner.** When `m.daemonWedged`, `renderHeader` replaces the `? help q quit`
right cluster with a red "⚠ daemon wedged — run: openkanban daemon restart" banner. It's the
same single header line, so there's NO board-layout impact (the column-height math is unchanged).
The flag is set by the `daemon_wedged` SessionEvent (and `HelloResp.SuspectedWedged` at startup,
read in `NewModel`) and cleared by `daemon_unwedged` — both handled in `applyDaemonSessionEvent`
(`daemon_subscribe.go`) before the per-ticket block since they carry no TicketID. The daemon
does NOT self-restart on a wedge (that would kill live sessions), so recovery is operator-driven.
Pinned by `TestApplyDaemonSessionEvent_WedgeBannerToggles` / `TestRenderHeader_ShowsWedgeBanner`.

## Messages

Custom messages for async operations:
```go
type spawnReadyMsg struct {...}
type agentStatusMsg struct {...}
```

Return `tea.Cmd` from `Update()` for async work.

## Startup daemon calls must stay bounded (no synchronous/unbounded RPC)

`NewModel` must NOT issue a daemon RPC of its own. The startup daemon
interaction is gated by a **bounded preflight** in `internal/app/app.go`,
run BEFORE the TUI is built: `ui.PreflightListSessions(client)` does a
short `List` (budget = `startupReconcileAttempts * startupReconcileTimeout`
in `daemon_resync.go`, kept ≤10s — pinned by
`TestPreflightBudgetStaysFastFail`). On success its snapshot is passed into
`NewModel` as `ownedByDaemon`; on failure (dial error, version skew, or a
wedged daemon that completes hello but stalls on `List`) `app.go` prints
`daemon.UnresponsiveHint()` (PID + `kill -9` when the pidfile names a live
process) and **exits** — it does not launch a daemon-less board.

`Subscribe` is the only other startup RPC and is armed **asynchronously**
from `Init` via `subscribeDaemonEventsCmd` under `startupSubscribeTimeout`;
`daemonSubscribeReadyMsg` installs the channel once the bounded handshake
returns. This is the regression that bit us: the old code called
`client.Subscribe(context.Background())` synchronously in `NewModel`, and a
wedged daemon blocked startup forever (before the bubbletea loop, so the
TUI never painted and the stall watchdog never armed). **Never reintroduce
a synchronous or `context.Background()` daemon call on the startup path.**

## Spawn overlay label (Starting → Attaching)

`renderSpawning` flips its label from "Starting <agent>" to "Attaching to
<agent>…" once the spawn has been in `ModeSpawning` longer than
`spawnAttachLabelDelay` (= the 5s `spawnCtx` timeout in `prepareSpawnWith`).
Rationale: Spawn and `attachWithRetry` (~8.6s) run inside **one** tea.Cmd,
so the Update loop never sees the Spawn→Attach boundary; past the 5s spawn
budget the RPC has almost certainly returned, so the remaining wait is
attach (the clock is stamped a hair before spawnCtx is armed, so the
boundary is heuristic, not exact — see the const comment in view.go).
The flip is a **View-layer time heuristic** — `m.spawnStartedAt` is stamped
in `prepareSpawnWith`'s synchronous prologue (the one chokepoint all four
ModeSpawning entry points funnel through; skipped for unattached spawns)
and read in `renderSpawning`. `spinner.TickMsg` already re-renders
ModeSpawning each tick, so the label flips with no extra plumbing.

**Deliberately NOT built** (ticket `ui-spinner-for-long-running-daemon-ops`,
after advisor + scope red-team): the generalized `inflightOps` map, the
periodic-resync footer breadcrumb, and splitting the spawn closure into two
Cmds for an exact (vs. heuristic) phase signal. The map had one consumer;
the resync is an invisible 3s-bounded background reconcile (`daemon_resync.go`)
with no user-perceived symptom; the closure split risked the dense
ModeSpawning race switch (`model.go` ~814-940) for a label the user can't
distinguish from the heuristic. The ticket's original headline item —
async startup-reconcile spinner — was already obsoleted by the
preflight-and-exit work (see "Startup daemon calls must stay bounded"
above). If real long-running ops or observed resync stalls appear later,
revisit with that evidence rather than re-deriving the map from scratch.

## Column Viewport Scopes

Vertical scroll per column lives in `m.columnOffsets[i]`. Three functions touch it, each with a different scope — don't confuse them:

- `refreshColumnTickets` (model.go) rebuilds `m.columnTickets` (all columns) — the chokepoint that every filter mutator AND ticket move flows through. Its last step calls `compactColumnOffsets`.
- `compactColumnOffsets` walks **all columns** and reduces stale offsets so filtered columns fill the screen instead of stranding cards behind `▲ N more`. Only reduces — never pushes the user down.
- `ensureTicketVisible` operates on the **active column only**, scrolling to keep `m.activeTicket` in view (used on cursor move and `selectTicketByID`).

Card-height arithmetic in any path that runs *inside or after* `refreshColumnTickets` (and before the next render) must use the `ticketHeight` constant — NOT the `columnTicketHeights` cache. The cache is keyed to pre-refresh indices; after a filter shifts the ticket list, index `j` likely points at a different card. Reading it post-refresh is actively wrong, not just stale.

### Keep focus on the acted-on ticket

Selection is by **index** (`m.activeColumn`/`m.activeTicket`), but `refreshColumnTickets` re-sorts every column and does NOT preserve which ticket was selected. Any path that *moves, creates, or edits* a ticket must call `m.selectTicketByID(ticket.ID)` **after** `refreshColumnTickets` to re-anchor focus on that ticket by its stable UUID. All five mutation paths follow this: forward/backward quick-move, drag-drop (`dropTicket`), create and edit branches of `saveTicketForm`.

The **background re-sort paths** follow the same rule, capturing `m.selectedTicket()` *before* the refresh and re-anchoring after: `board_resync.go`'s `handleBoardResyncMsg` (1s polling safety net) and `reload.go`'s `handleFsChanged` (fsnotify). Without this, an external change that re-sorts a column — e.g. an agent flip bumps `StatusChangedAt` under `SortStatusChange`, or a CLI `ticket done` moves a card to another column — leaves the cursor pinned to its old index, so the highlight silently jumps to a different ticket. These paths follow the selection across columns too (an external status change moves it), so they call `ensureColumnVisible()` after `selectTicketByID` like `dropTicket`. They do **not** call `revealThroughFilters` (see below) — an external change must not yank the user's filter; if the selection gets filtered out, `selectTicketByID`'s clamp fallback degrades gracefully. Pinned by `TestBoardResync_PreservesSelectionAcrossReorder` / `…_FocusFollowsTicketToNewColumn` / `TestFsReload_PreservesSelectionAcrossReorder` (`focus_resync_test.go`).

Do **not** push `selectTicketByID` into `refreshColumnTickets` itself. That chokepoint is shared with **filter** flows (`handleFilterMode`, `clearFilter`, `toggleProjectFilter`, `toggleAllProjects`) that intentionally rely on `selectTicketByID`'s clamp-degrade fallback to gracefully drop a selection a filter just hid — centralizing the re-select would regress filter UX. Keep the call inline at each call site that wants it (mutation + background-refresh), and leave the filter mutators alone. `selectTicketByID` handles vertical scroll (`ensureTicketVisible`) but not horizontal — keep an explicit `ensureColumnVisible()` where the active column may change (e.g. `dropTicket`, the background-refresh paths).

**Interactive create reveals the new ticket through active filters.** `selectTicketByID` re-anchoring is necessary but not sufficient on create: a brand-new ticket has no daemon session and an arbitrary title, so `ticketMatchesFilter` hides it under an open-only session filter, a non-matching search query, or a project narrow — and the clamp fallback then leaves focus elsewhere. The `n`-create branch of `saveTicketForm` calls `revealThroughFilters(ticket)` first, which relaxes *only* the dimensions that would hide that ticket (clears the query, flips `SessionFilterOpen`→`All`, adds the ticket's project to a narrow rather than wiping it). This is **interactive-create only** — the async board-resync (`board_resync.go`) and CLI-create paths must never call it: an externally-arriving ticket must not yank the user's filter state out from under an active session. The edit path is also intentionally excluded.

## Terminal Panes

`panes map[board.TicketID]*daemonclient.PaneView` — one per spawned agent.

PaneView is the client-side handle; the PTY itself lives in openkanbankd. Lifecycle is daemon-driven: `Spawn` happens server-side at construction time, `Attach` / `Detach` swap which TUI is the one attached client, and `daemonclient.PaneViewAttached` vs `PaneViewUnattached` describe what this TUI sees, not whether the agent is alive (the agent can be alive in the daemon while every TUI is `Unattached`). Methods preserve the old `*terminal.Pane` surface — see `internal/daemonclient/paneview.go` for the full 13-method shape and the unattached-state behavior table.

**Attach is coupled to viewing, not session-lifetime.** `exitToBoard()` calls `pane.Detach()` on the focused pane when leaving the agent view, so a TUI sitting on the board does NOT hold the session's single daemon attach slot. Before this, a backgrounded TUI kept the slot for its whole connection life, so a second TUI got `ErrAlreadyAttached` and (with no `lastAttachErr` set) a blank pane — the 2026-06-22 report. Re-entering the view re-attaches with a fresh snapshot. `Detach()` is a no-op on an unattached pane, so the async focus-drop paths (session exited, pane detached/exited, daemon disconnected) that also funnel through `exitToBoard` are unaffected. NOTE: session→session focus switches (cycle / Auto) do not yet detach the previous pane — tracked as a follow-up.

`Detach()` and `Close()` are non-blocking as of 2026-06-16. State mutations (state=Unattached, emulator teardown, detachCh swap) happen eagerly under `p.mu`; the underlying `attachLoopWG.Wait` runs in a goroutine with a 5s warning / 30s deadline watchdog. `PaneDetachedMsg` arrives whenever the read loop actually drains (not synchronously with the caller). `emitTeaMsg` and `Close` are serialised by a `teaMu sync.Mutex` so the goroutine can't send on a closed `teaMsgs` channel. Required reading before any teardown edit: memory [[reference_openkanban_paneview_detach_concurrency]].

**`readNextMsg`'s poll MUST watch `detachCh`, and MUST NOT arm while detached.** The per-attach poll `select`s on `teaMsgs` (output), this attach's `detachCh`, and the daemon-wide `client.closeCh`. The `detachCh` case is load-bearing: a detach closes neither `teaMsgs` (only `Close()` does) nor `closeCh` (only a full daemon disconnect does), so without it the poll parked at detach-time — and its bubbletea `execBatchMsg` parent — leak forever, one pair per agent-view enter/exit. Over a long session that reached 1600+ goroutines and surfaced as a multi-second, uptime-dependent TUI "freeze" the stall watchdog never caught (the Update loop stays healthy; the cost is GC/scheduler tax on the leaked stacks). `readNextMsg` snapshots `detachCh` under `p.mu` and early-returns nil unless `state == PaneViewAttached` (the state gate closes the race where a final buffered output msg re-arms the poll just after detach). Pinned by `TestPaneView_readNextMsg_ReturnsOnDetach` (PR #143).

### `teaMsgs` has exactly ONE reader — don't double-arm

A **second**, independent way to leak parked `teaMsgs` readers (distinct from the `readNextMsg`/`detachCh` leak above, which it compounds — both showed up in the same SIGUSR2 dump). `teaMsgs` is a single-reader channel: each event must re-arm exactly one reader, or output stops flowing. Two surfaces re-arm it and must form a **partition** over the pane-scoped message types (`paneIDOf`):

- `PaneView.Update` returns `readNextMsg()` for the messages where it consumed output (`PaneOutputMsg`, `PaneAttachedMsg`). `readNextMsg` selects on `teaMsgs`, `detachCh`, and `closeCh`.
- `handleTerminalMsg` bridges `m.listenPaneMessages(pv)` **only when `Update` did not** (`PaneRenderTickMsg`, `PaneDetachedMsg`, which return nil) — gated by the `rearmed` flag keyed on the addressed pane. `listenPaneMessages` has **no** escape channel, so a loser of a two-reader race parks forever.

Arming **both** for one message leaks a permanently-parked `listenPaneMessages` reader — and its parent `execBatchMsg` WaitGroup waiter — per output event; a long-lived session accumulates thousands (the SIGUSR2 stall dump that surfaced this showed 181 parked `listenPaneMessages` readers + 1008 `execBatchMsg` waiters). If you add a fifth pane-scoped message, decide which surface re-arms it and keep the two comments in sync. Pinned by `TestHandleTerminalMsg_PaneOutputArmsSingleReader` / `…_RenderTickStillRearms`.

### Two title surfaces — keep their fallbacks straight

The session header (`renderAgentView`) and the host terminal title (`computeWindowTitle`) resolve the title differently — don't unify them:

- **In-app header bar** uses `m.globalStore.Get(m.focusedPane)` → `ticket.Title`, then `pane.TicketTitle()` (the pane's cached last-known-good ticket title, stamped at build/focus), then the literal `"Agent"`. It does NOT use the OSC title — the inner program's window title isn't the ticket title.
- **Host terminal title** uses `pane.Title()` (the inner program's OSC 0/2 title) first, then `ticket.Title`, then `"openkanban"`.

The `pane.TicketTitle()` fallback exists because a `PaneView` can outlive its store ticket: `GlobalTicketStore.ReloadTicket`'s `os.IsNotExist` branch drops the ticket from `allTickets` (the only silent runtime removal — now logged) when a file path vanishes from a board-resync snapshot, while the daemon session and pane stay live. Stamp `SetTicketTitle` wherever a pane is built or re-focused for a known ticket; never from `View()`. Memory: [[reference_openkanban_agent_view_title_resolution]].

### Attach-failure overlay

When `attachWithRetry` (post-spawn or B4 fast-path) exhausts its retries, the closure calls `pv.SetLastAttachErr(err)` before returning the `spawnReadyMsg`. `PaneView.View()` then renders an actionable overlay instead of `blankPaneView` — same `cols × rows` contract so the chrome composition doesn't shift, pure ASCII so byte count == display cell count. Successful `Attach()` clears `lastAttachErr` automatically, so the overlay disappears on the next View() pass. The `shouldRetryAttachOnEnter` predicate (see Key Bindings above) gates Enter-retry on the SAME state pair, so the overlay's "Enter retries" hint is actually wired up.

## Brief-change chooser

The brief-chooser modal in `spawnAgent` fires on a SINGLE signal:

- `wouldChange == true` — the openkanban card's Description has diverged from the on-disk `<worktree>/tickets/<slug>.md` managed block. The on-disk brief is the snapshot written at the last merge/spawn, so `wouldChange` is true exactly when the user edited the card after the session was last active. Message: "Brief was updated since this session started. What should I do?"

The three choice closures (`d`/`u`/`n` → `spawnPlan{ForceFresh}`/`{InjectResumeNotice}`/`{SkipMerge}`) are unchanged.

**Do NOT re-add a `StatusChangedAt`-based "pulled back" trigger.** A prior `pulledBack := ticket.AgentSpawnedAt != nil && ticket.StatusChangedAt.After(*ticket.AgentSpawnedAt)` arm was removed because it fired the chooser on *every* re-spawn of any old session: `SetAgentStatus` stamps `StatusChangedAt` on each working↔waiting flip (`internal/board/board.go`), so any session that did work has `StatusChangedAt > AgentSpawnedAt`. It is NOT a "user reopened the card" signal — agent activity bumps it constantly. (If a genuine pull-back signal is ever needed, use `StartedAt.After(*AgentSpawnedAt)` — `StartedAt` is stamped only by `SetStatus(StatusInProgress)`, never by `SetAgentStatus` — but the current design intentionally fires only on a real brief change.) The tradeoff: re-spawning a shipped, pulled-back ticket (empty Description, no brief file → `wouldChange=false`) silently `--resume`s the prior JSONL with no resume-vs-fresh prompt; this is accepted.

**Esc must be routed explicitly.** The chooser is shown via `m.showChoice = true` while `m.mode` stays `ModeNormal`. In `handleKey`, the global key arms (`esc`/`q`/`ctrl+c`/`?`) run BEFORE the `m.showChoice → handleChoice` dispatch. The `ModeNormal` `esc` arm resets `mode`/`showHelp`/`showConfirm` but NOT `showChoice`, so without an explicit `if m.showChoice { return m.handleChoice(msg) }` guard at the top of the `esc` case, Esc is swallowed and the modal stays stuck open. Any future overlay shown via a bool flag (not a `Mode`) while `mode==ModeNormal` must be routed the same way in every global key arm it should answer to.

## Status-file lookup key

`pollAgentStatusesAsync` looks up `~/.cache/openkanban-status/<key>.status` using `pane.SessionName()` — the value the daemon stamped into the agent's `OPENKANBAN_SESSION` env var at spawn time, and what the status hook reads back when it calls `openkanban status set <state>`. The detector splits this from `apiSessionID` (the back-filled Claude/opencode UUID, used only for opencode's HTTP lookup) via `DetectStatusWithActivity(agentType, fileSessionName, apiSessionID, ...)`.

**Don't substitute `ticket.AgentSessionID` for the file lookup.** The UUID gets back-filled mid-session by `FindClaudeSession`, while `OPENKANBAN_SESSION` stays whatever was baked at original spawn. Conflating them creates a divergence where the hook keeps writing under the env var (the branch name) but the UI reads under the UUID, the file is missing, the detector falls through to terminal-content scraping, and Claude's `━` prompt-border heuristic mis-classifies idle/waiting sessions as "working". `sessionNameFor(ticket, branchName)` is now `branchName > ticketID` (no UUID), and `OwnsResp.SessionName` carries the daemon's stored value so the Owns fast-path doesn't need to recompute. See `[[reference_openkanban_status_file_key_invariant]]`.

## Status-mutation wrap-up

When the user moves a ticket OUT of `in_progress` to a terminal status (`in_review` or `done`) via the **board** (quick-move keys or drag), `wrapUpSessionForTicket` runs BEFORE `m.globalStore.Move(...)`. The pre-Move ordering matters — the helper's gate ("is the ticket leaving in_progress?") reads the **current** status, which `Move`'s call to `SetStatus` mutates in place.

`wrapUpSessionForTicket` returns a `tea.Cmd`. **Local state mutations stay synchronous** in the helper (pane map delete, focus unwind, `SetAgentStatus(AgentCompleted)`) so the next render reflects the wrap-up immediately. **Daemon-side work** — `pane.Stop()`, `pane.Close()`, `TicketDone(ctx, ticketID)` — runs in the returned Cmd's goroutine, off the Update loop. Pre-2026-06-16 these were inline with 5s + 2s context timeouts, which was the multi-second freeze users saw on `/quit` and `openkanban ticket done`. Callers (drag-drop, forward and backward quick-move) thread the returned Cmd into their `(model, cmd)` return value; tests that assert daemon-side effects capture and invoke the Cmd inline. Memory: [[reference_openkanban_wrap_up_returns_cmd]].

The Cmd's closure captures pane handle, daemon API, and ticket ID into locals before launching — per the "tea.Cmd goroutines must not touch shared Model state" rule below. The underlying primitive that makes this cheap is `PaneView.detach()`'s own non-blocking refactor: memory [[reference_openkanban_paneview_detach_concurrency]].

This closes the historical asymmetry where the CLI tore down sessions on transition but the TUI didn't — leaving a live daemon PTY whose ticket's status no longer matched.

Two seams worth knowing about:

- **`daemonGuardAPI` interface** (`exit_guard.go`) — extended with `TicketDone(ctx, ticketID)` so UI tests can substitute a fake without spinning up a real daemon. New daemon RPCs needed from UI code should be added here for the same reason. **The Model field is `m.daemon` (type `daemonAPI`, which embeds `daemonGuardAPI`)** — `m.guardAPI` is a vestige of the pre-PR-#39 name and won't compile. If you copy a call site from a stale branch or older PR diff, sanity-check the receiver before merging.
- **`handleDaemonSessionEvent("exited")` link preservation** (`daemon_subscribe.go`) — `AgentSessionID` and `AgentSpawnedAt` are preserved on **both** expected and unexpected exits. Only the PTY dies; the JSONL transcript outlives the session process, so pulling a ticket back from `done` resumes the same conversation via `--resume`. `AgentStatus` is the meaningful difference: `Expected=true` → `AgentCompleted`, `Expected=false` → `AgentNone`. See commit `c718699` (original unexpected-exit rationale) and PR #158 (extended the same invariant to expected exits).

## tea.Cmd goroutines must not touch shared Model state

Returning a `tea.Cmd` from Update causes the framework to run it in a goroutine. That goroutine can run concurrently with subsequent Update calls, so it MUST NOT touch state that Update mutates — in particular `m.projectRegistry` and `m.globalStore`. Reading them is also a race if Update writes them (e.g. `handleFsChanged`'s `projectRegistry.ReloadFromDisk`, ticket-creation paths).

Discipline:

- **Goroutine:** read-only filesystem work; load registry/state from disk into *local* fresh copies (e.g. `project.LoadRegistry()`).
- **Update handler:** the only place that mutates `m.projectRegistry`, `m.globalStore`, `m.panes`, etc.

The race detector only catches observed concurrency. A test that drives the cmd synchronously (`cmd()` inline) will miss the race. See `board_resync.go` for the canonical shape — goroutine loads its own `*ProjectRegistry`; the handler reloads the model's registry. `daemon_resync.go` follows the same rule: its goroutine reads only `api` (externally synchronized) and never touches `m`.

## Agent identity is project-pinned (no per-ticket / global picker)

Which agent (and thus which Claude profile / `CLAUDE_CONFIG_DIR`) launches is
chosen **per project, and nowhere else**. The mechanism:

- `project.Settings.DefaultAgent` is the pin. The sidebar `g` key cycles it
  (`cycleProjectAgent`, over `enabledAgentNames()`) and persists via `registry.Update`.
- The sidebar `e` key opens `ModeEditProject` (`internal/ui/project_edit.go`) — a
  unified editor for the project (name + pin → projects.json) AND the shared
  agent registry (label/command/args/env/enabled → config.json via `config.Save`).
  Editing two config files in one screen is intentional. It uses ONE reused
  `peInput` bound to the focused text field with a working copy (`peAgents`);
  `peSyncFromField`/`peSyncToField` move values in/out on field change.
- Agent availability: `config.AgentConfig.IsEnabled()` is a tri-state — `Enabled
  *bool` override (`&true`/`&false`) else PATH auto-detect (`exec.LookPath`).
  `enabledAgentNames()` filters the pin cycle so uninstalled agents don't clutter
  it (falls back to all if none qualify). A disabled agent a project is pinned to
  still spawns — disabled only hides it from selection.
- Every spawn site resolves through `Model.resolveSpawnAgent(ticket, proj)`:
  `ticket.AgentType` (resume continuity) → `config.RoleForType(ticket.Type)`
  (pipeline role, if the ticket is typed) → `proj.Settings.DefaultAgent` →
  `errNoProjectAgent`. There is **no** fallback to `config.Defaults.DefaultAgent`.
  An unpinned, untyped project refuses to spawn — that's the guard against
  accidentally launching the wrong Claude. The type→role step is the ONE
  sanctioned per-ticket agent override (the "task types → agent roles"
  feature): a `research`/`spec`/`implement`/`review` ticket binds
  `claude-research`/`claude-spec`/`claude`/`claude-review`. Those role presets
  ship with an empty `Env`, so they run the DEFAULT claude profile and differ
  only by `InitPrompt` — a typed ticket in a custom-profile-pinned project
  runs the role on the default profile (composing role InitPrompt with a
  pinned profile's `Env` is a v2 follow-up). The create-form **Type** picker
  is distinct from the (deliberately absent) agent picker; do not add an agent
  picker — `TestNoGlobalDefaultAgentInSpawnPath` still guards the
  `Defaults.DefaultAgent` path.
- Behavior (arg wrapping at `buildSpawnReq`'s `switch`, daemon status) keys off
  `agentCfg.Command` (basename), not the config map key. So two presets with
  `command: "claude"` (e.g. `claude` + `claude-custom`) both get Claude
  treatment; they differ only by `Env` (`CLAUDE_CONFIG_DIR`). Per-agent `Env` is
  injected at spawn with a leading `~/` expanded (`expandLeadingTilde`).

Every claude-class spawn also gets `CLAUDE_CODE_ENABLE_PROMPT_SUGGESTION=false`
appended to `SpawnReq.Env` in `buildSpawnReq` (before the per-agent `Env` loop, so
an `AgentConfig.Env` override still wins). This kills Claude Code's model-generated
"Prompt suggestions" — the next-prompt drafts it drops into the input box after
each turn — which are noise in a ticket-scoped agent. The env var wins over Claude's
`promptSuggestionEnabled` setting and rides the same `req.Env` path that survives the
daemon's `buildCleanEnv` `CLAUDE_*` strip. This is unrelated to the up-arrow history
ring / `PurgeClaudePrimingHistory` (a separate surface).

Do NOT reintroduce a ticket-form agent picker or a global Default-Agent setting
— that reopens the accidental-wrong-Claude path the project pin closes.
`TestNoGlobalDefaultAgentInSpawnPath` (a static guard) fails if `model.go`
reads `Defaults.DefaultAgent` again. The struct field still exists and is read
by `internal/app` only for the OpenCode-server autostart decision.

**Per-project model override (`project.Settings.Model`):** when set, `buildSpawnReq`
emits `--model <value>` as the first flag in the `case "claude":` arm (covering
both new-session and resume paths). Empty = no flag (claude's own default). The
model is read from project settings at every spawn — NOT stored on the ticket —
so changing a project's model and re-spawning takes effect without any ticket-level
migration. Do NOT add a global `config.Defaults.Model` or a per-ticket model field;
project-scoped only, claude-class agents only.

## Anti-Patterns

- Don't block in Update() - use Cmd for async
- Don't render directly in Update() - only in View()
- Don't store computed strings - recompute in View()
- Don't access panes without nil check
- Don't mutate `m.*` from a tea.Cmd goroutine — see the section above

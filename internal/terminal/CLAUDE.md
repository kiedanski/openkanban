# Terminal Package

**This package is now the daemon-side library.** PR7 cut the TUI over to `internal/daemonclient.PaneView`, which mirrors the surface (Title/Start/Stop/StopGraceful/SetSize/HandleKey/HandleMouse/Running/SetWorkdir/GetWorkdir/SetSessionName/GetContent/Update) but uses its own local emulator fed by the daemon's binary stream. The `*terminal.Pane` type defined here is instantiated inside `openkanbankd` (see `internal/daemon/session.go`) to own the actual PTY, vt drain goroutine, and mode flags. UI code should not import this package — go through `daemonclient` instead.

PTY management and terminal emulation for agent processes.

## Core Components

- **Pane** - manages single PTY + virtual terminal
- **SelectionState** - text selection state machine
- **Glyph** - internal cell representation; emulator-agnostic

## PTY Handling

Uses `creack/pty`:
```go
pty.StartWithSize(cmd, &Winsize{...})  // fork with size, atomic
pty.Setsize(f, ws)                     // resize (on host window change only)
```

`StartWithSize` is preferred over `Start + Setsize` to avoid a TIOCSWINSZ race; see the commit fixing that race for context.

## Terminal Emulation

Uses `github.com/charmbracelet/x/vt` (`SafeEmulator`) for escape sequence parsing and screen state:
- Cursor management
- Cell-based rendering
- Color/attribute handling
- Scroll regions, alt-screen, scrollback — **native scrollback is authoritative on both sides.** The **client** `daemonclient.PaneView` renders from charm's native scrollback directly (`vt.ScrollbackLen`/`ScrollbackCellAt` via `RenderVTNativeScrollback`), and the **daemon's attach snapshot** reads native scrollback too (`Pane.SnapshotScrollback` → `vt.ScrollbackLen`/`ScrollbackCellAt`). The `scrollbackSize` field (set in `New`, clamped to 10000 minimum) is applied to the emulator via `p.vt.SetScrollbackSize(p.scrollbackSize)` immediately after each emulator creation site; `ScrollbackSize()` exposes it read-only so `SessionInfo.Scrollback` can propagate the value to `daemonclient.PaneView`, which calls `SetScrollbackSize` on its own local emulator in `initEmulatorLocked`. This means `config.UI.ScrollbackLines` is a real end-to-end knob: config → `SpawnReq.Scrollback` → daemon emulator → `SessionInfo.Scrollback` → client emulator.

charm/x/vt emits *responses* (DA queries, cursor reports, etc.) through `Emulator.Read()`. We MUST spawn a goroutine that loops `Read()` → writes to the PTY, or the emulator deadlocks on the first query. See `Pane.startDrainUnlocked` and `stopDrainUnlocked`.

See [docs/AGENT_INTEGRATION.md#architecture-terminal-emulator](../../docs/AGENT_INTEGRATION.md#architecture-terminal-emulator) for the rationale behind choosing charm/x/vt over the previous library (hinshun/vt10x).

## Internal Glyph Type

`glyph.go` defines openkanban's `Glyph` (a value-typed cell with Char/Bold/Italic/.../FG/BG/Width). The rest of the package — scrollback, selection, render — uses `Glyph` exclusively. Only `pane.go` touches the emulator's native `*uv.Cell`, and only at the boundary (`cellToGlyph`).

This is intentional: it decouples scrollback/selection/render code from the emulator choice, so a future swap doesn't ripple through every file.

### Glyph.Width semantics

`Glyph.Width` carries the cell's monospaced display width and is load-bearing for any code that iterates columns and emits one rune per cell:

- **1** — normal single-cell glyph.
- **2** — leading half of a wide (CJK / emoji) glyph; the cell to its right is the continuation.
- **0** — continuation cell of a preceding wide glyph. ultraviolet stores these as zero-value `Cell{}`; `CellToGlyph` preserves the 0 so writers can recognize them.

**Any cell-iterating writer MUST skip Width=0 cells.** Emitting a space for the continuation shifts every glyph after the wide one one column right in the destination — the bug that surfaced as "garbled initial render on session attach" across `daemon/redraw.go:writeRow`, `daemon/redraw.go:writeGlyphRow` (the `SerializeScrollback` row writer), `terminal/render.go:renderLiveRow`, and `terminal/render.go:renderGlyphLine`. Regression guards are `TestSerializeRedraw_RoundTrip/wide_chars_round_trip` and `TestSerializeScrollback_WideCharRoundTrip` in `internal/daemon/redraw_test.go`.

## Message Types

BubbleTea integration:
- `OutputMsg` - new terminal output
- `ExitMsg` - process terminated
- `RenderTickMsg` - throttled render trigger

## Rendering

- Throttled at 50ms intervals
- `dirty` flag tracks when re-render needed
- Cached view string until dirty
- `glyphANSI(g)` emits SGR escape sequences from a Glyph

## Key Translation

`translateKey()` converts BubbleTea `KeyMsg` to PTY bytes:
- Arrow keys → escape sequences
- Ctrl+C → 0x03
- Enter → \r

## Environment

`buildCleanEnv()`:
- Sets `TERM=xterm-256color`
- Strips agent-related env vars (`OPENCODE*`/`CLAUDE*`/`GEMINI*`/`CODEX*`, and any inherited `OPENKANBAN_*`)
- Synthesizes `OPENKANBAN_SESSION` / `OPENKANBAN_TICKET_ID` for THIS pane
- Preserves HOME, USER

### Self-binary PATH injection (`openkanbanSelfBin` / `selfBinResolves`)

`buildCleanEnv` runs inside `openkanbankd`, so `os.Executable()` is the build
that owns the board. It front-loads that binary's **directory** onto the spawned
agent's `PATH` and exports `OPENKANBAN_BIN=<full path>`, so a bare
`openkanban …` in the agent's **non-interactive** shell (e.g. the
`spin-off-a-ticket` / `fan-out-a-plan` skills' `openkanban ticket new`) resolves
to the same build — not to a stale/older `openkanban` earlier on the user's
PATH. This is the fix for the `unknown command "ticket"` failure a
Homebrew-release-plus-`okb`-shell-alias split produced (the release binary
predated the `ticket` subcommand; the alias is invisible to the Bash tool).

The injection is **surgical, not unconditional**: `selfBinResolves(binPath)`
runs `exec.LookPath("openkanban")` against the inherited PATH and, if it already
resolves to this build, PATH is left untouched (no reorder, no duplicate). So a
single-install setup is a no-op; only the shadowed/split-install case rewrites
PATH. `OPENKANBAN_BIN` is always exported when the self-binary is resolvable
(it's synthesized fresh, hence exempt from the `OPENKANBAN_*` strip). Both
helpers fail safe: an unresolvable `os.Executable()` leaves PATH as inherited
and omits `OPENKANBAN_BIN`. `selfExecutable` is a package var so tests can pin a
deterministic path (`TestBuildCleanEnv_InjectsSelfBinOntoPath` /
`…_SkipsInjectWhenSelfAlreadyOnPath` / `…_SelfBinUnresolvableLeavesPath`).

## Escape Sequence Detection

Byte scanning at the openkanban layer (independent of the emulator) for:
- Mouse mode: `\x1b[?1000h`
- Alt screen: `\x1b[?1049h`

These flags drive openkanban's own mouse-forwarding and selection behavior. charm/x/vt also tracks them internally; we keep our own scanner for the few places we need the state synchronously during byte processing.

## Cursor Visibility

charm/x/vt does not expose a public `CursorVisible()` getter (the `Cursor` struct with `Hidden bool` lives on the private `Screen`). We track DECTCEM state via `Callbacks.CursorVisibility` into an `atomic.Bool` on the Pane (`cursorHidden`), which the renderer reads lock-free.

The same pattern is used for **DECCKM** (application cursor keys, `\x1b[?1h` / `\x1b[?1l`): `Callbacks.EnableMode` / `DisableMode` fire when modes flip and store into `Pane.cursorAppMode atomic.Bool`. `translateKey` reads the flag and emits `\x1bOA/B/C/D` (SS3) instead of `\x1b[A/B/C/D` (CSI) when set — matching iTerm2/xterm/charm-vt's own `SendKey`. Same convention applies to any future DEC mode mirror: vt has no generic `IsModeSet` getter, so use `Callbacks.EnableMode/DisableMode` + atomic. The reentrancy rule below applies — these callbacks fire synchronously inside `vt.Write`, must not take `p.mu`.

## OSC Handlers — Reentrancy Convention

`charm/x/vt` dispatches OSC handlers registered via `RegisterOscHandler(cmd, fn)` **synchronously inside `vt.Write(bytes)`**. The handler runs on the same goroutine that called `vt.Write` — which, in this package, is `handleOutput` while it holds `p.mu`.

**Any OSC handler MUST be lock-free with respect to `p.mu`.** Taking `p.mu` from inside an OSC handler causes a reentrant lock-up — the daemon's TUI fan-out wedges on the first frame carrying an OSC sequence. We hit this once already with the OSC 0/2 title handler; see commit `7dcb7f7` for the symptom and fix.

The currently-registered handlers and their lock-free state slots:

- **OSC 0 / OSC 2** (window title) — store into `Pane.cachedTitle atomic.Value // string`
- **OSC 9** (desktop notification) — gated by `Pane.forwardNotifications atomic.Bool`; on fire, calls `internal/notify.Send(payload)` (cgo NSUserNotification via the OpenKanban.app bundle). The toggle's source of truth is `config.Behavior.ForwardAgentNotifications`, plumbed via `SpawnReq.ForwardNotifications` and applied by `pane.SetForwardNotifications` before `StartHeadless`.

Future handlers (e.g. OSC 7 cwd, OSC 133 prompt-marks, OSC 52 clipboard) must follow the same convention: use `atomic.Value` / `atomic.Bool` / channel-based dispatch; never `p.mu`.

## Anti-Patterns

- Don't write to PTY without checking if alive
- Don't skip resize handling - causes display corruption
- Don't render on every output - use throttling
- Don't leak PTY file descriptors - always close
- Don't `Read()` the emulator from anywhere other than the drain goroutine — its response pipe is a single consumer
- Don't expose `*uv.Cell` outside of pane.go's boundary — translate to `Glyph`
- **Don't hold `p.mu` across a blocking PTY write.** That pinned the lock in production and cascaded into a daemon-wide freeze. `WriteInput` enqueues onto the single per-pane writer goroutine (non-blocking) and reports `ErrInputBackpressure`; only that goroutine ever blocks in `pty.Write`, and teardown's `f.Close()` unblocks it.
- **Keep `Info`-reachable accessors (`Size`/`Running`/`PID`) lock-free** — they read atomic mirrors, never `p.mu`. `Session.Info` calls them from the daemon; taking a per-pane lock there re-arms the cascade amplifier (one stuck pane → all RPCs frozen). Two regression guards enforce this: `TestSessionInfo_LockFree` (`pane_wedge_test.go`) holds `p.mu` and asserts `Size`/`Running`/`PID` return fast; `TestWedgedSession_HealthFastListBounded` (`internal/daemon/wedge_regression_test.go`) asserts that a session whose `Info()` is wedged must not freeze the Health RPC, and that List is deadline-bounded.
- **Wake the response-drain only via `InputPipe().(*io.PipeWriter).CloseWithError(io.EOF)`** — never a sentinel-byte write (deadlocks on the synchronous `io.Pipe` with no reader) and never `Emulator.Close()` (races `e.closed` under `-race`).

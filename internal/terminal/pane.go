package terminal

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"

	"github.com/atotto/clipboard"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	xvt "github.com/charmbracelet/x/vt"
	"github.com/creack/pty"

	"github.com/techdufus/openkanban/internal/notify"
)

const (
	renderInterval = 50 * time.Millisecond
	readBufferSize = 65536

	// subscriberChanCapacity is the buffer depth of channels returned by
	// Pane.Subscribe. Sized for short bursts of OutputEvents at typical
	// agent throughput; if a subscriber falls further behind, the read
	// loop drops events for that subscriber rather than block the PTY
	// pipeline. See Subscribe / startReadLoop.
	subscriberChanCapacity = 256
)

// --- Event subscription API ---
//
// Subscribe returns a channel that receives every Event observed by the
// pane: OutputEvent for each PTY read, ExitEvent on PTY close, and
// TitleEvent / ModeEvent when the child mutates window title or
// mouse / alt-screen / cursor-visibility state.
//
// Lives ALONGSIDE the existing tea.Cmd path (OutputMsg / ExitMsg) so
// non-BubbleTea consumers — namely the daemon process in PR4 — can
// observe pane traffic without dragging tea.Program along.

// Event is the sum type emitted to Subscribers. The unexported
// paneEvent() marker forces variants to live in this package.
type Event interface {
	paneEvent()
}

// OutputEvent carries a chunk of bytes read from the PTY. The byte
// slice is unique to this event; subscribers may retain it.
type OutputEvent struct {
	Data []byte
}

// ExitEvent is published once when the PTY read returns an error
// (typically io.EOF after the child process exits).
type ExitEvent struct {
	Err error
}

// TitleEvent fires when the child sets the OSC 0/2 window title.
type TitleEvent struct {
	Title string
}

// ModeEvent reports the current value of the three booleans whenever
// any of them transitions. Carries the full snapshot so a fresh
// subscriber doesn't need to query the pane to learn the state.
type ModeEvent struct {
	Mouse        bool
	AltScreen    bool
	CursorHidden bool
}

func (OutputEvent) paneEvent() {}
func (ExitEvent) paneEvent()   {}
func (TitleEvent) paneEvent()  {}
func (ModeEvent) paneEvent()   {}

type Pane struct {
	id          string
	vt          *xvt.SafeEmulator
	pty         *os.File
	cmd         *exec.Cmd
	mu          sync.Mutex
	running     bool
	exitErr     error
	workdir     string
	sessionName string
	ticketID    string
	width       int
	height      int

	// --- lock-free read mirrors (Task 1) ---
	//
	// running / pid / dims mirror the plain fields above so the
	// Info-reachable accessors (Running / PID / Size) can be read
	// WITHOUT taking p.mu. This is what keeps handleList → Session.Info
	// (which iterates every session under sessionsMu.RLock) from ever
	// blocking on a pane whose p.mu is held by a stuck WriteInput /
	// teardown — the daemon-wide deadlock these mirrors exist to
	// prevent. Mirrors the existing lock-free Title() pattern.
	//
	// dims packs width<<32 | height into ONE atomic.Uint64 so Size()
	// reads a single atomic and a concurrent resize can never produce a
	// torn cols/rows pair. See packDims / unpackDims.
	runningAtomic atomic.Bool
	pid           atomic.Int64
	dims          atomic.Uint64

	// pgid is the child's process-group id, captured at spawn via
	// syscall.Getpgid. creack/pty sets Setsid so pgid == child pid.
	// Used by signalGroup to reap the whole agent process tree on
	// teardown (Task 3). Lock-free so teardown never needs p.mu to read
	// it. Zero until a successful spawn.
	pgid atomic.Int64

	// --- input-writer goroutine state (Task 2) ---
	//
	// inputCh feeds the single per-pane PTY-writer goroutine. WriteInput
	// does a NON-BLOCKING send onto it, so p.mu never spans the
	// (potentially blocking) PTY write — that decoupling is the
	// root-cause fix for the paste-flood deadlock. inputStop signals the
	// writer to exit; we NEVER close(inputCh) (a racing WriteInput send
	// would panic). Both channels are write-once: assigned only in
	// startInputWriterUnlocked under p.mu before the pane is observable,
	// never reassigned, so WriteInput's lock-free read is safe.
	inputCh       chan []byte
	inputStop     chan struct{}
	inputStopOnce sync.Once
	inputWG       sync.WaitGroup

	// teardownOnce ensures teardownUnlocked fires at most once. A second
	// Stop() or a Stop() racing a natural exit must be a safe no-op: a
	// re-issued signalGroup(SIGKILL) against a stale pgid risks hitting a
	// recycled process (PID/PGID reuse). After the first teardown fires,
	// pgid is zeroed so signalGroup's ≤0 guard catches any late caller.
	teardownOnce sync.Once

	// wedge tracking for the daemon watchdog (Task 5). WriteInput stamps
	// these on input backpressure (the child stopped draining stdin and
	// the bounded buffer filled). The watchdog reads them via
	// WedgedSince() — atomics only, so it can never block on the wedged
	// pane.
	//
	//   wedgedSinceNs — start of the current backpressure EPISODE.
	//   lastWedgeNs   — time of the most recent backpressure.
	//
	// A single successful enqueue does NOT end the episode: a child that
	// isn't draining still backpressures almost every write but the PTY
	// buffer has enough slack that the odd chunk slips through. If a lone
	// success reset the episode the start time would keep advancing and
	// the watchdog could never observe a sustained wedge. Instead the
	// episode is keyed off lastWedgeNs recency: WedgedSince reports the
	// episode start while backpressure stays recent, and reports zero
	// (recovered) once no backpressure has happened for wedgeRecency.
	wedgedSinceNs atomic.Int64
	lastWedgeNs   atomic.Int64

	// expectedCompletedExit is true when the TUI initiated a
	// StopGraceful as a reaction to AgentCompleted on a Done ticket
	// (the `openkanban ticket done` flow). The ExitMsg handler reads
	// this to know it should preserve AgentCompleted on the ticket
	// instead of resetting AgentStatus to AgentNone like a normal
	// pane exit.
	expectedCompletedExit bool

	cachedView      string
	lastRender      time.Time
	dirty           bool
	renderScheduled bool

	mouseEnabled bool // tracks if child process has enabled mouse tracking

	// Scrollback and viewport state (Issue #95)
	altScreenActive bool            // tracks if child process is in alternate screen mode
	scrollbackSize  int             // configured scrollback buffer size
	selection       *SelectionState // mouse text selection state

	// cursorHidden tracks DECTCEM state for the live emulator. charm/x/vt
	// does not expose a getter on Emulator, so we maintain our own flag
	// via the Callbacks.CursorVisibility hook. Atomic so the goroutine
	// that drives the callback can safely write while renderers read.
	cursorHidden atomic.Bool

	// cursorAppMode tracks DECCKM (application cursor keys mode). When
	// the inner agent enables it via ESC[?1h, arrow keys must be encoded
	// as SS3 (ESC O A/B/C/D) instead of CSI (ESC [ A/B/C/D). Maintained
	// via the EnableMode/DisableMode callbacks below, which fire
	// SYNCHRONOUSLY inside vt.Write — must stay lock-free re: p.mu, hence
	// the atomic.
	cursorAppMode atomic.Bool

	// forwardNotifications gates the OSC 9 handler. When true, an OSC 9
	// sequence emitted by the agent is forwarded to notify.Send (which
	// fires a desktop notification on darwin and is a no-op elsewhere).
	// Atomic because the handler runs synchronously inside vt.Write
	// under p.mu — taking p.mu in the handler would deadlock. The
	// daemon flips this via SetForwardNotifications based on the spawn
	// request's ForwardNotifications field.
	forwardNotifications atomic.Bool

	// lastActivityNs is the unix-nanosecond timestamp of the last
	// observed PTY output for this pane — stamped from handleOutput on
	// every non-empty vt.Write. Used by the daemon's activity broadcaster
	// to push "activity" SessionEvents to subscribers, which the UI uses
	// to distinguish "stuck at waiting" (no bytes flowing) from "actively
	// working" (Claude's spinner / tool output streaming). atomic.Int64
	// so daemon goroutines can read without taking p.mu.
	//
	// Why bytes-flowed rather than grid-changed: cursor blinks are
	// terminal-side (not PTY output), and Claude's "Cogitating…" /
	// "Combobulating…" spinner emits bytes throughout tool execution.
	// Hashing the grid added cost on every handleOutput without
	// catching anything bytes-flowed misses in practice.
	lastActivityNs atomic.Int64

	// drainStop stops the goroutine that pipes emulator-emitted responses
	// (DA queries, etc.) back to the PTY. Without that drain charm/x/vt
	// deadlocks on its first device-attributes write.
	drainStop chan struct{}
	drainWG   sync.WaitGroup

	// paneTitle holds the most recent title the child set via OSC 0/2.
	// Updated from the emulator callback (sync, during Write); read by
	// the model when computing the host-window title. atomic.Value lets
	// the read be lock-free.
	paneTitle atomic.Value // string

	// Event subscription state (see Subscribe / startReadLoop). The
	// subscribers slice is mutated only under subscribersMu; the read
	// loop holds subscribersMu briefly during fan-out, so subscribersMu
	// MUST NOT be acquired while p.mu is held (the read loop drops p.mu
	// before fan-out to keep the lock order one-way).
	subscribers     []chan Event
	subscribersMu   sync.Mutex
	readLoopOnce    sync.Once
	readLoopStop    chan struct{}
	readLoopWG      sync.WaitGroup
	stopReadLoopMu  sync.Mutex // serializes one-shot close of readLoopStop
	readLoopStopped bool

	// teaBridgeCh is the dedicated subscriber feeding readOutputUnlocked's
	// tea.Cmd. We allocate it once per Pane lifetime (during the first
	// readOutputUnlocked call) so the tea path doesn't race with consumer
	// Subscribe()s and so re-arming the Cmd doesn't churn subscriptions.
	teaBridgeCh   <-chan Event
	teaBridgeOnce sync.Once
}

func New(id string, width, height int, scrollbackSize int) *Pane {
	if scrollbackSize <= 0 {
		scrollbackSize = 10000
	}
	p := &Pane{
		id:             id,
		width:          width,
		height:         height,
		scrollbackSize: scrollbackSize,
	}
	// Seed the lock-free dims mirror so Size() is accurate before the
	// pane is started (SetSize / spawn update it thereafter).
	p.dims.Store(packDims(width, height))
	return p
}

// ID returns the pane's identifier
func (p *Pane) ID() string {
	return p.id
}

// SetWorkdir sets the working directory for commands
func (p *Pane) SetWorkdir(dir string) {
	p.workdir = dir
}

func (p *Pane) GetWorkdir() string {
	return p.workdir
}

// SetSessionName sets the session name for OPENKANBAN_SESSION env var
func (p *Pane) SetSessionName(name string) {
	p.sessionName = name
}

// SetTicketID sets the openkanban ticket id for OPENKANBAN_TICKET_ID env var.
// The child reads this to resolve back to the ticket .md when running
// `openkanban ticket done` from inside the session.
func (p *Pane) SetTicketID(id string) {
	p.ticketID = id
}

// MarkExpectedCompletedExit flags this pane as being stopped because
// its ticket was marked done by the agent. The ExitMsg handler reads
// this to preserve AgentCompleted instead of resetting to AgentNone.
func (p *Pane) MarkExpectedCompletedExit() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.expectedCompletedExit = true
}

// ExpectedCompletedExit reports whether StopGraceful was initiated via
// the ticket-done path.
func (p *Pane) ExpectedCompletedExit() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.expectedCompletedExit
}

// stampStartedUnlocked seeds the lock-free read mirrors right after a
// successful fork. Must be called with p.mu held and after p.pty and
// p.cmd are set. Captures the child's process-group id for the
// teardown group-kill (creack/pty sets Setsid, so pgid == child pid).
func (p *Pane) stampStartedUnlocked() {
	p.runningAtomic.Store(true)
	p.dims.Store(packDims(p.width, p.height))
	if p.cmd != nil && p.cmd.Process != nil {
		pid := p.cmd.Process.Pid
		p.pid.Store(int64(pid))
		if pgid, err := syscall.Getpgid(pid); err == nil {
			p.pgid.Store(int64(pgid))
		}
	}
}

// packDims packs width and height into a single uint64 (width in the
// high 32 bits, height in the low 32). Negative or oversized values are
// clamped to the uint32 range; pane dimensions are always small
// positive ints in practice.
func packDims(width, height int) uint64 {
	if width < 0 {
		width = 0
	}
	if height < 0 {
		height = 0
	}
	return uint64(uint32(width))<<32 | uint64(uint32(height))
}

// unpackDims is the inverse of packDims.
func unpackDims(v uint64) (width, height int) {
	return int(uint32(v >> 32)), int(uint32(v))
}

// Running returns whether the pane has a running process. Lock-free
// (reads the atomic mirror) so it never blocks on a stuck pane — see
// the runningAtomic field comment.
func (p *Pane) Running() bool {
	return p.runningAtomic.Load()
}

// ExitErr returns any error from the process exit
func (p *Pane) ExitErr() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exitErr
}

func (p *Pane) SetSize(width, height int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// No-op when dimensions haven't changed. Otherwise every entry
	// into agent view fires an unnecessary SIGWINCH at the child
	// process; over a few cycles of leave/re-enter or AskUserQuestion
	// open/close, ink's layout cache gets re-invalidated repeatedly
	// and can land in a state where bottom-anchored UI renders at the
	// top. Skip when there's nothing actually to resize.
	if p.width == width && p.height == height {
		return
	}

	p.width = width
	p.height = height
	// Mirror into the lock-free packed atomic so Size() reflects the
	// resize without taking p.mu.
	p.dims.Store(packDims(width, height))
	p.dirty = true
	p.cachedView = ""

	// Clear selection on resize (coordinates become invalid)
	if p.selection != nil && p.selection.IsActive() {
		p.selection.Clear()
	}

	if p.vt != nil {
		p.vt.Resize(width, height)
	}

	if p.pty != nil && p.running {
		pty.Setsize(p.pty, &pty.Winsize{
			Rows: uint16(height),
			Cols: uint16(width),
		})
	}
}

// Size returns the current dimensions. Lock-free: reads a single packed
// atomic so a concurrent resize can never yield a torn cols/rows pair,
// and it never blocks on a stuck pane (see the dims field comment).
func (p *Pane) Size() (width, height int) {
	return unpackDims(p.dims.Load())
}

// ScrollbackSize returns the configured native scrollback line count.
// The field is set once in New and never mutated — safe to read without p.mu.
func (p *Pane) ScrollbackSize() int {
	return p.scrollbackSize
}

// SnapshotScrollback returns the emulator's native scrollback history,
// oldest line first, as materialized Glyph rows. Returns nil when there
// is no emulator or no history. Used by the daemon to ship scrollback to
// attaching clients.
//
// We read the emulator's OWN scrollback (vt.ScrollbackLen /
// ScrollbackCellAt) rather than the CaptureTopRow/PushScrolledLine ring:
// that ring snapshots only one row per write, so a single write that
// scrolls many rows off the grid (a burst of agent output, or a re-attach
// drain after a detached period) lost all but one scrolled-off row. The
// emulator wraps lines and tracks every scrolled-off row, so it is the
// authoritative history — the same reason the client renders from native
// scrollback (see daemonclient.PaneView / RenderVTNativeScrollback). The
// ring remains only for the legacy daemon-side Pane.View render path.
func (p *Pane) SnapshotScrollback() [][]Glyph {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.vt == nil {
		return nil
	}
	n := p.vt.ScrollbackLen()
	if n == 0 {
		return nil
	}
	cols := p.vt.Width()
	if cols <= 0 {
		return nil
	}
	out := make([][]Glyph, n)
	for idx := 0; idx < n; idx++ {
		row := make([]Glyph, cols)
		for col := 0; col < cols; col++ {
			row[col] = CellToGlyph(p.vt.ScrollbackCellAt(col, idx))
		}
		out[idx] = row
	}
	return out
}

// IsAltScreenActive returns whether the terminal is in alternate screen mode.
func (p *Pane) IsAltScreenActive() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.altScreenActive
}

// SnapshotState returns the emulator and the three modal booleans the
// daemon's redraw serializer needs to reproduce the pane's current
// view. The returned emulator pointer is the live one — callers MUST
// only read from it via SafeEmulator's locked methods (CellAt,
// CursorPosition, Width, Height); they must not mutate it.
//
// The cursor-visibility / mouse / alt-screen booleans are tracked on
// Pane (not on the emulator), so the daemon couldn't reconstruct them
// from vt alone. This getter is the single seam through which the
// daemon's snapshot path reads them; PR7 will fold it into a richer
// Pane.View interface.
func (p *Pane) SnapshotState() (vt *xvt.SafeEmulator, cursorVisible, mouseEnabled, altScreen bool, title string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	vt = p.vt
	mouseEnabled = p.mouseEnabled
	altScreen = p.altScreenActive
	cursorVisible = !p.cursorHidden.Load()
	title = ""
	if v, ok := p.paneTitle.Load().(string); ok {
		title = v
	}
	return vt, cursorVisible, mouseEnabled, altScreen, title
}

// --- Bubbletea Messages ---

// OutputMsg carries data read from the PTY
type OutputMsg struct {
	PaneID string
	Data   []byte
}

// ExitMsg indicates the process has exited
type ExitMsg struct {
	PaneID string
	Err    error
}

// RenderTickMsg triggers a throttled render
type RenderTickMsg struct {
	PaneID string
}

// ExitFocusMsg signals to return to board view
type ExitFocusMsg struct{}

// --- PTY Lifecycle (Issue #13) ---

// Start launches a command in a PTY and returns a Cmd to begin reading
func (p *Pane) Start(command string, args ...string) tea.Cmd {
	return func() tea.Msg {
		p.mu.Lock()
		// No `defer Unlock` here — the final `readOutputUnlocked()()`
		// invocation blocks waiting for the first event from the
		// subscription channel, which is filled by a goroutine that
		// must acquire p.mu (handleOutput → p.mu). We MUST release
		// p.mu before invoking the bridge Cmd.

		// Build command
		p.cmd = exec.Command(command, args...)
		p.cmd.Env = buildCleanEnv(p.sessionName, p.ticketID)

		// Set working directory if specified
		if p.workdir != "" {
			p.cmd.Dir = p.workdir
		}

		// Fork the child with the correct PTY size from the start.
		// pty.Start + pty.Setsize would race: the child renders its
		// first frame at the OS-default 80x24 before SIGWINCH arrives,
		// causing bottom-anchored UI (input bars, status lines) to
		// pin to row 24 of the child's coordinate space — which lands
		// at the wrong row in our actual-sized emulator buffer.
		// StartWithSize sets TIOCSWINSZ atomically with the fork.
		ptmx, err := pty.StartWithSize(p.cmd, &pty.Winsize{
			Rows: uint16(p.height),
			Cols: uint16(p.width),
		})
		if err != nil {
			p.exitErr = err
			p.mu.Unlock()
			return ExitMsg{PaneID: p.id, Err: err}
		}
		p.pty = ptmx
		p.running = true
		p.exitErr = nil
		// Wire the input-writer goroutine (which assigns inputCh) BEFORE
		// stampStartedUnlocked publishes runningAtomic. WriteInput reads
		// inputCh lock-free after an acquire-load of runningAtomic, so
		// publishing running last gives that read its happens-before edge.
		p.startInputWriterUnlocked()
		// Stamp the lock-free mirrors so Running/PID/Size are accurate
		// without p.mu the instant the pane becomes observable.
		p.stampStartedUnlocked()

		// Create the virtual terminal. charm/x/vt emits responses
		// (device attributes, cursor reports, etc.) via Read(); we
		// MUST drain that pipe and forward to the PTY or the
		// emulator deadlocks on the first DA query.
		p.vt = xvt.NewSafeEmulator(p.width, p.height)
		p.vt.SetCallbacks(xvt.Callbacks{
			CursorVisibility: func(visible bool) {
				newHidden := !visible
				prev := p.cursorHidden.Swap(newHidden)
				if prev != newHidden {
					// We're inside p.vt.Write, which runs under
					// p.mu inside handleOutput. The mouse/alt-screen
					// flags are stable while p.mu is held, so it's
					// safe to read them directly here. publish takes
					// only subscribersMu (lock order: p.mu →
					// subscribersMu).
					p.publish(ModeEvent{
						Mouse:        p.mouseEnabled,
						AltScreen:    p.altScreenActive,
						CursorHidden: newHidden,
					})
				}
			},
			EnableMode: func(mode ansi.Mode) {
				if mode == ansi.ModeCursorKeys {
					p.cursorAppMode.Store(true)
				}
			},
			DisableMode: func(mode ansi.Mode) {
				if mode == ansi.ModeCursorKeys {
					p.cursorAppMode.Store(false)
				}
			},
		})
		p.cursorHidden.Store(false)
		p.cursorAppMode.Store(false)
		p.registerTitleHandlersUnlocked()
		p.startDrainUnlocked()
		p.vt.SetScrollbackSize(p.scrollbackSize)

		p.selection = NewSelectionState()

		// Build the read Cmd while we still hold p.mu (so the nil-
		// check on p.pty sees a stable value), then release the lock
		// BEFORE invoking it. The Cmd lazily Subscribes — which
		// kicks off the read goroutine — and then blocks on the
		// channel. The read goroutine grabs p.mu for handleOutput,
		// so we cannot still be holding it here.
		readCmd := p.readOutputUnlocked()
		p.mu.Unlock()
		return readCmd()
	}
}

// StartHeadless launches a command in a PTY exactly like Start, but
// returns synchronously and does not use BubbleTea's tea.Cmd. The read
// loop is spawned via the same subscription machinery as Start; the
// caller is expected to Subscribe() if they want to observe output.
//
// The optional env slice fully replaces the per-process env (after the
// pane's buildCleanEnv pass). Pass nil to use the inherited environment
// with the OPENKANBAN_SESSION addition.
//
// Used by the openkanbankd daemon, which owns Panes without a tea
// runtime to drive the Cmd indirection. Mirrors Start's body byte-for-
// byte aside from the tea.Cmd plumbing at the end.
func (p *Pane) StartHeadless(command string, args []string, extraEnv []string) error {
	p.mu.Lock()

	p.cmd = exec.Command(command, args...)
	p.cmd.Env = buildCleanEnv(p.sessionName, p.ticketID)
	if len(extraEnv) > 0 {
		// extraEnv is appended AFTER buildCleanEnv, so a duplicate key here
		// can shadow the clean value. In particular it must NOT carry PATH:
		// that would defeat buildCleanEnv's self-binary PATH injection. No
		// production caller sets PATH (buildSpawnReq only emits OPENKANBAN_*,
		// per-agent env, and a --model flag); keep it that way.
		p.cmd.Env = append(p.cmd.Env, extraEnv...)
	}

	if p.workdir != "" {
		p.cmd.Dir = p.workdir
	}

	ptmx, err := pty.StartWithSize(p.cmd, &pty.Winsize{
		Rows: uint16(p.height),
		Cols: uint16(p.width),
	})
	if err != nil {
		p.exitErr = err
		p.mu.Unlock()
		return err
	}
	p.pty = ptmx
	p.running = true
	p.exitErr = nil
	// Wire the input-writer goroutine (which assigns inputCh) BEFORE
	// stampStartedUnlocked publishes runningAtomic. WriteInput reads
	// inputCh lock-free after an acquire-load of runningAtomic, so
	// publishing running last gives that read its happens-before edge.
	p.startInputWriterUnlocked()
	// Stamp the lock-free mirrors so Running/PID/Size are accurate
	// without p.mu the instant the pane becomes observable.
	p.stampStartedUnlocked()

	p.vt = xvt.NewSafeEmulator(p.width, p.height)
	p.vt.SetCallbacks(xvt.Callbacks{
		CursorVisibility: func(visible bool) {
			newHidden := !visible
			prev := p.cursorHidden.Swap(newHidden)
			if prev != newHidden {
				p.publish(ModeEvent{
					Mouse:        p.mouseEnabled,
					AltScreen:    p.altScreenActive,
					CursorHidden: newHidden,
				})
			}
		},
		EnableMode: func(mode ansi.Mode) {
			if mode == ansi.ModeCursorKeys {
				p.cursorAppMode.Store(true)
			}
		},
		DisableMode: func(mode ansi.Mode) {
			if mode == ansi.ModeCursorKeys {
				p.cursorAppMode.Store(false)
			}
		},
	})
	p.cursorHidden.Store(false)
	p.cursorAppMode.Store(false)
	p.registerTitleHandlersUnlocked()
	p.startDrainUnlocked()
	p.vt.SetScrollbackSize(p.scrollbackSize)

	p.selection = NewSelectionState()

	p.mu.Unlock()

	// Kick the read loop without going through the tea bridge. Any
	// Subscribe call (including the very first one a daemon-side
	// caller makes) will hit readLoopOnce, but we start the loop
	// here eagerly so the PTY is being drained immediately even if
	// no one has Subscribed yet. This matches Start's behavior
	// (where the returned tea.Cmd Subscribes lazily on first call).
	p.readLoopOnce.Do(p.startReadLoop)
	return nil
}

// PID returns the OS pid of the child process, or 0 if the pane has
// not started. Lock-free (reads the atomic mirror) so it never blocks
// on a stuck pane. Stamped at spawn and never cleared — a caller that
// needs liveness should pair this with Running().
func (p *Pane) PID() int {
	return int(p.pid.Load())
}

// Title returns the most recent title the child process set via OSC 0/2
// escape sequences. Empty string if the child has not set a title (yet).
func (p *Pane) Title() string {
	if v, ok := p.paneTitle.Load().(string); ok {
		return v
	}
	return ""
}

// parseOscTitlePayload extracts the title from an OSC 0/1/2 payload as
// delivered by charm/x/vt. The handler sees the raw payload bytes
// including the leading "<cmd>;" parameter (e.g. "2;hello-title"),
// because charm/x/vt's handleTitle does its own bytes.Split on ';'.
// We strip a leading run of digits followed by ';' if present.
func parseOscTitlePayload(data []byte) string {
	i := 0
	for i < len(data) && data[i] >= '0' && data[i] <= '9' {
		i++
	}
	if i > 0 && i < len(data) && data[i] == ';' {
		return string(data[i+1:])
	}
	return string(data)
}

// registerTitleHandlersUnlocked wires OSC 0/2 handlers on the emulator
// to capture the child process's window title, plus an OSC 9 handler
// that forwards desktop notifications via the notify package when
// forwardNotifications is set. OSC 0 sets both window title and icon
// name; OSC 2 sets only the window title. Both feed the same Pane
// field — we only care about the window title.
//
// Must be called with p.mu held and after p.vt has been constructed.
func (p *Pane) registerTitleHandlersUnlocked() {
	if p.vt == nil {
		return
	}
	handler := func(data []byte) bool {
		title := parseOscTitlePayload(data)
		p.paneTitle.Store(title)
		// Title handlers fire from within p.vt.Write, which is
		// invoked under p.mu by handleOutput. publish takes only
		// subscribersMu — see lock-order note on publish().
		p.publish(TitleEvent{Title: title})
		return true
	}
	p.vt.RegisterOscHandler(0, handler)
	p.vt.RegisterOscHandler(2, handler)
	p.vt.RegisterOscHandler(9, p.forwardNotificationHandler)
}

// forwardNotificationHandler is the OSC 9 callback registered on the
// emulator. The agent (typically claude code) emits OSC 9 with a
// payload of "9;<body>" to request the host terminal raise a desktop
// notification. We strip the "9;" prefix using parseOscTitlePayload
// (which handles any "<digits>;" prefix) and forward the body to
// notify.Send.
//
// Lock discipline: this runs SYNCHRONOUSLY inside p.vt.Write which is
// invoked under p.mu by handleOutput. Taking p.mu here would deadlock.
// We only touch the forwardNotifications atomic.Bool, so the handler
// is lock-free.
//
// Returns false when forwarding is disabled or the stripped payload is
// empty (so an unhandled-OSC fallback in the emulator doesn't surface
// the payload as a stray title); returns true when notify.Send was
// invoked, regardless of any error notify.Send returned.
func (p *Pane) forwardNotificationHandler(data []byte) bool {
	if !p.forwardNotifications.Load() {
		return false
	}
	body := parseOscTitlePayload(data)
	if body == "" {
		return false
	}
	// iTerm2's OSC 9 progress-bar protocol shares the OSC 9 cmd
	// namespace with simple-text notifications. The progress form is
	// "\x1b]9;<state>;<value>\x07" where state ∈ 0..4 (0 clear, 1 set
	// percent, 2 indeterminate, 3 error, 4 warning); Claude Code emits
	// these to drive the terminal's progress indicator, NOT to raise a
	// desktop notification. After parseOscTitlePayload strips the
	// leading "9;", a progress payload looks like "4;3;" / "1;50" /
	// "2" — digits + semicolons only, no letters. Discriminate by
	// checking for any alphabetic rune: real notification text always
	// contains letters; progress control payloads don't.
	if !payloadContainsLetter(body) {
		return false
	}
	// Errors from notify.Send are swallowed: there's no actionable
	// recovery from the emulator callback, and a logging side-effect
	// would surface inside vt.Write under p.mu. The notify package
	// itself is responsible for any necessary observability.
	_ = notify.Send(body)
	return true
}

// payloadContainsLetter reports whether s has any Unicode letter rune.
// Used by forwardNotificationHandler to discriminate notification text
// from iTerm2 OSC 9 progress-bar control sequences (which are digits +
// semicolons only — no alphabetic characters).
func payloadContainsLetter(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}

// SetForwardNotifications toggles OSC 9 → desktop notification
// forwarding for this pane. Safe to call from any goroutine; the
// underlying field is atomic.Bool. The daemon calls this once during
// session construction based on SpawnReq.ForwardNotifications.
func (p *Pane) SetForwardNotifications(enabled bool) {
	if p == nil {
		return
	}
	p.forwardNotifications.Store(enabled)
}

// startDrainUnlocked spawns the goroutine that forwards emulator
// responses back to the PTY. Must be called with p.mu held.
func (p *Pane) startDrainUnlocked() {
	p.drainStop = make(chan struct{})
	stop := p.drainStop
	vt := p.vt
	ptyFile := p.pty

	p.drainWG.Add(1)
	go func() {
		defer p.drainWG.Done()
		buf := make([]byte, 4096)
		for {
			select {
			case <-stop:
				return
			default:
			}
			n, err := vt.Read(buf)
			if n > 0 && ptyFile != nil {
				_, _ = ptyFile.Write(buf[:n])
			}
			if err != nil {
				if err == io.EOF {
					return
				}
				// Other errors: stop draining; the pane is being torn down.
				return
			}
		}
	}()
}

// inputChanCapacity bounds the per-pane input queue. A child that stops
// draining stdin fills the kernel PTY buffer; once the writer goroutine
// parks inside f.Write, this many queued chunks accumulate before
// WriteInput starts reporting ErrInputBackpressure. 256 is generous for
// interactive typing and bursty pastes while keeping the backlog
// bounded.
const inputChanCapacity = 256

// startInputWriterUnlocked spawns the SINGLE per-pane goroutine that
// performs every PTY input write. Must be called with p.mu held and
// after p.pty is set (mirrors startDrainUnlocked / startReadLoop). It
// captures the *os.File once (the same idiom the read loop and drain
// use) so the write path never touches p.pty — and never p.mu — again.
//
// Decoupling the write from p.mu is the root-cause fix for the
// paste-flood deadlock: WriteInput now does a non-blocking enqueue onto
// inputCh and returns immediately, so p.mu can never span the
// (potentially forever-blocking) PTY write. Only THIS goroutine blocks
// on a non-draining child; teardown's f.Close() unblocks it with EBADF.
//
// inputCh / inputStop are assigned exactly here, under p.mu, before the
// pane is observable, and never reassigned. Start/StartHeadless call this
// BEFORE stampStartedUnlocked publishes runningAtomic, so WriteInput's
// acquire-load of runningAtomic happens-after these assignments — its
// lock-free reads of inputCh/inputStop are therefore race-free.
func (p *Pane) startInputWriterUnlocked() {
	p.inputCh = make(chan []byte, inputChanCapacity)
	p.inputStop = make(chan struct{})
	ch := p.inputCh
	stop := p.inputStop
	f := p.pty

	p.inputWG.Add(1)
	go func() {
		defer p.inputWG.Done()
		for {
			select {
			case <-stop:
				return
			case data := <-ch:
				if f == nil {
					return
				}
				if _, err := f.Write(data); err != nil {
					// Write error (EBADF after teardown closes the fd,
					// EIO after the slave collapses, etc.): the pane is
					// gone. Exit — no point draining further input.
					return
				}
			}
		}
	}()
}

// Subscribe registers a new event subscriber and returns the receive
// end of a buffered channel plus an unsubscribe func. Calling the
// unsubscribe func removes the channel from the registry and closes
// it; calling it more than once is a no-op.
//
// Safe to call before or after Start. The first call (regardless of
// who makes it — public consumer or the internal tea bridge) starts
// the PTY-read goroutine via sync.Once. Subsequent calls share the
// same loop.
func (p *Pane) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, subscriberChanCapacity)

	p.subscribersMu.Lock()
	p.subscribers = append(p.subscribers, ch)
	p.subscribersMu.Unlock()

	p.readLoopOnce.Do(p.startReadLoop)

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			p.subscribersMu.Lock()
			defer p.subscribersMu.Unlock()
			for i, c := range p.subscribers {
				if c == ch {
					p.subscribers = append(p.subscribers[:i], p.subscribers[i+1:]...)
					close(ch)
					return
				}
			}
			// Not found means the read loop already closed it on
			// shutdown; nothing to do.
		})
	}
	return ch, cancel
}

// startReadLoop spawns the single PTY-read goroutine. Called via
// p.readLoopOnce.Do, so exactly one runs across a Pane's lifetime.
// Callers MUST ensure Pane.Start has completed before the first
// Subscribe (which triggers this) — otherwise p.pty is nil and the
// loop publishes an immediate ExitEvent.
//
// The loop reads bytes from p.pty, hands them to handleOutput (which
// takes p.mu) to update emulator/scrollback/mode flags, then fans the
// resulting OutputEvent out to all subscribers. ModeEvent / TitleEvent
// are emitted from handleOutput's helpers and the emulator callbacks,
// not from this function directly.
//
// On p.pty.Read error (typically io.EOF after the child exits) the
// loop publishes a final ExitEvent and returns.
//
// IMPORTANT: This function does NOT take p.mu. It would be a deadlock
// when invoked from Subscribe under sync.Once during Start's critical
// section (Start holds p.mu while calling the returned tea.Cmd which
// in turn Subscribes). The read of p.pty here is safe because
// sync.Once / Subscribe is only called via the tea.Cmd returned from
// Start, which the tea runtime invokes after the Cmd closure returns
// — by which point Start's p.mu critical section has finished
// publishing p.pty.
//
// We DO acquire p.mu briefly to allocate readLoopStop, but only via
// stopReadLoopMu (a finer-grained lock dedicated to lifecycle). This
// avoids the Start re-entry deadlock.
func (p *Pane) startReadLoop() {
	p.stopReadLoopMu.Lock()
	p.readLoopStop = make(chan struct{})
	stop := p.readLoopStop
	p.stopReadLoopMu.Unlock()

	ptyFile := p.pty
	if ptyFile == nil {
		// Defensive: Subscribe was called before Start finished.
		// Publish exit so subscribers don't hang.
		p.publishExit(fmt.Errorf("pane %s: read loop started before PTY ready", p.id))
		return
	}

	p.readLoopWG.Add(1)
	go func() {
		defer p.readLoopWG.Done()
		buf := make([]byte, readBufferSize)
		for {
			select {
			case <-stop:
				// Teardown closed the stop channel. Publish a final
				// ExitEvent and close subscribers so observers
				// (watchSessionExit, the tea bridge) always learn the
				// loop ended — they can't tell "stopped" from "child
				// exited", and either way the session is gone.
				// publishExit is safe to race with the Read-error path
				// below: it nils the subscriber slice, so a second call
				// is a no-op. Without this, a teardown that closes the
				// fd and the stop channel near-simultaneously can win
				// the stop race here and leave subscribers blocked on a
				// never-closed channel (the daemon-side exit watcher
				// would then never run removeSession).
				p.publishExit(io.EOF)
				return
			default:
			}

			n, err := ptyFile.Read(buf)
			if n > 0 {
				// Copy bytes — buf is reused on the next iteration
				// and we want each subscriber to own the slice it
				// receives.
				data := make([]byte, n)
				copy(data, buf[:n])

				p.handleOutput(data)
				p.publish(OutputEvent{Data: data})
			}
			if err != nil {
				p.publishExit(err)
				return
			}
		}
	}()
}

// publish fans an event out to all current subscribers. Subscribers
// whose buffer is full receive a dropped event (logged once per drop
// to stderr) — the read loop never blocks on a slow consumer.
//
// Safe to call with p.mu held: publish takes only subscribersMu and
// performs only non-blocking channel sends. Lock order is
// p.mu → subscribersMu (never the reverse).
func (p *Pane) publish(ev Event) {
	p.subscribersMu.Lock()
	defer p.subscribersMu.Unlock()
	for _, ch := range p.subscribers {
		select {
		case ch <- ev:
		default:
			fmt.Fprintf(os.Stderr, "pane %s: dropped event for slow subscriber\n", p.id)
		}
	}
}

// publishExit emits a final ExitEvent and closes every subscriber
// channel. After this returns no further publishes will succeed
// (subscribers slice is nil'd).
func (p *Pane) publishExit(err error) {
	p.subscribersMu.Lock()
	defer p.subscribersMu.Unlock()
	for _, ch := range p.subscribers {
		// Best-effort delivery of the terminal event. If the buffer
		// is full, drop it: subscribers learn about exit via the
		// channel close that follows.
		select {
		case ch <- ExitEvent{Err: err}:
		default:
			fmt.Fprintf(os.Stderr, "pane %s: dropped ExitEvent for slow subscriber\n", p.id)
		}
		close(ch)
	}
	p.subscribers = nil
}

// stopReadLoop closes the stop channel exactly once and waits for the
// read goroutine to exit. Safe to call from Stop / StopGraceful (which
// also close p.pty, unblocking any in-flight Read). Must be called
// with p.mu released to avoid deadlocking with publish().
func (p *Pane) stopReadLoop() {
	p.stopReadLoopMu.Lock()
	if !p.readLoopStopped && p.readLoopStop != nil {
		close(p.readLoopStop)
		p.readLoopStopped = true
	}
	p.stopReadLoopMu.Unlock()
	p.readLoopWG.Wait()
}

// stopDrainUnlocked terminates the response-drain goroutine. Must be
// called with p.mu held.
//
// To unblock the drain goroutine's vt.Read we write a sentinel byte
// into the emulator's response pipe (InputPipe is the writer end of
// pr/pw; pr is what the drain reads). vt.Read returns with the
// byte, the drain loop iterates, sees drainStop closed, and exits.
//
// This avoids calling Emulator.Close(), which mutates an internal
// `closed` field without a lock — a benign race in practice but one
// the -race detector trips on against the concurrent Read.
func (p *Pane) stopDrainUnlocked() {
	if p.drainStop == nil {
		return
	}
	p.signalDrainStopUnlocked()
	// Wait without holding p.mu — currently callers already hold
	// p.mu, but the drain goroutine doesn't touch p.mu so the Wait
	// is safe. (We did NOT take p.mu in the goroutine itself.)
	p.drainWG.Wait()
}

// signalDrainStopUnlocked closes drainStop and wakes the drain
// goroutine's blocked vt.Read by closing its pipe writer (io.EOF), but
// does NOT Wait.
// Must be called with p.mu held. The teardown sequence in Stop /
// StopGraceful uses this to signal the drain to stop WITHOUT blocking
// under p.mu; the bounded drainWG.Wait runs later, off p.mu and AFTER
// the PTY fd is closed (close-before-wait — see Stop()).
//
// Idempotent: a nil drainStop (already signalled) is a no-op.
func (p *Pane) signalDrainStopUnlocked() {
	if p.drainStop == nil {
		return
	}
	close(p.drainStop)
	if p.vt != nil {
		// Close the emulator's pipe writer with io.EOF to unblock the
		// drain's vt.Read. A sentinel-byte wakeup DEADLOCKS: charm/x/vt is
		// backed by a SYNCHRONOUS io.Pipe, so writing into it blocks forever
		// whenever the drain already observed drainStop and stopped reading
		// (no reader) — and since teardown's drainWG.Wait is bounded but the
		// write isn't, that block would wedge the pane. CloseWithError never
		// blocks, makes Read return (0, io.EOF) (no garbage PTY write), and
		// touches no unsynchronized emulator state (unlike Emulator.Close,
		// which races e.closed under -race). Ported from PR #97. Regression:
		// TestPane_StopDrainDoesNotDeadlock.
		if pw, ok := p.vt.Emulator.InputPipe().(*io.PipeWriter); ok {
			_ = pw.CloseWithError(io.EOF)
		}
	}
	p.drainStop = nil
}

// waitOrTimeout waits for wg with an upper bound. Returns true if wg
// completed within d, false on timeout. On timeout the goroutine is
// NOT abandoned-with-leak intent: it will exit later once its blocking
// op (fd close / process death) finally returns — we simply refuse to
// let it hold teardown hostage. This is what makes Stop() return
// unconditionally even in the pathological case where neither f.Close()
// nor the group SIGKILL releases a goroutine promptly.
func waitOrTimeout(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// teardownWaitTimeout bounds each post-close goroutine Wait in the
// teardown sequence. Generous enough that a healthy goroutine always
// finishes well within it; short enough that a wedged one can't freeze
// the daemon.
const teardownWaitTimeout = 2 * time.Second

// signalGroup sends sig to the child's entire process group so the
// agent's whole process tree (including children holding the PTY slave
// open) is reaped, not just the immediate child. Guards against blast
// radius / PID reuse: it refuses a non-positive pgid, the daemon's own
// process group, and the init group (1). creack/pty sets Setsid at
// spawn so the child leads its own group (pgid == child pid) captured
// in stampStartedUnlocked.
func (p *Pane) signalGroup(sig syscall.Signal) {
	pgid := int(p.pgid.Load())
	if pgid > 0 && pgid != syscall.Getpgrp() && pgid != 1 {
		_ = syscall.Kill(-pgid, sig)
		return
	}
	// pgid is missing/unsafe (Getpgid failed at spawn, or the value would
	// hit our own group or init). Fall back to signaling just the child
	// pid — the guarantee the pre-rebase Stop had via proc.Kill() — so a
	// child that ignores the PTY-close SIGHUP is still reaped. pid is the
	// lock-free mirror, so this stays off p.mu.
	if pid := int(p.pid.Load()); pid > 0 {
		_ = syscall.Kill(pid, sig)
		return
	}
	log.Printf("openkanban pane %s: no usable pgid/pid to signal (pgid=%d)", p.id, pgid)
}

// teardownUnlocked performs the load-bearing close-before-wait teardown
// shared by Stop and StopGraceful. The ordering is exact and must not be
// reordered (see Task 3 / docs):
//
//  1. signalGroup(SIGKILL) — reap the agent's whole process tree
//     (children holding the slave). Skipped by callers that already
//     SIGKILLed (StopGraceful does it post-grace).
//  2. Under p.mu: capture f := p.pty, nil p.pty, clear the running
//     mirror, signal (NOT wait) the drain stop, and signal the input
//     writer to stop. Unlock.
//  3. f.Close() OUTSIDE p.mu — the primary unblock: returns the
//     in-flight writer Write (EBADF), the drain Write, and the read
//     loop Read.
//  4. Bounded waits OFF p.mu for drain, input writer, and read loop —
//     each waitOrTimeout so teardown returns unconditionally.
func (p *Pane) teardownUnlocked(alreadyKilled bool) {
	p.teardownOnce.Do(func() {
		p.doTeardown(alreadyKilled)
	})
}

func (p *Pane) doTeardown(alreadyKilled bool) {
	if !alreadyKilled {
		p.signalGroup(syscall.SIGKILL)
	}
	// Zero pgid after the group signal fires so any late caller to
	// signalGroup (e.g. a concurrent second Stop()) hits the ≤0 guard and
	// is a safe no-op. This closes the pgid-reuse window: once the group
	// is killed, the pgid may be recycled by the OS.
	p.pgid.Store(0)

	p.mu.Lock()
	f := p.pty
	p.pty = nil
	p.running = false
	p.runningAtomic.Store(false)
	p.signalDrainStopUnlocked()
	if p.inputStop != nil {
		p.inputStopOnce.Do(func() { close(p.inputStop) })
	}
	p.mu.Unlock()

	// Close the master fd OUTSIDE p.mu. This is the primary unblock for
	// every PTY goroutine (each captured the same *os.File handle): the
	// parked writer Write returns EBADF, the drain Write and read-loop
	// Read return too. Closing needs no lock.
	if f != nil {
		_ = f.Close()
	}

	// Bounded waits, all OFF p.mu (the read goroutine calls handleOutput
	// which takes p.mu, so Wait()ing under p.mu would self-deadlock).
	// Each is bounded so Stop() returns even if a goroutine somehow
	// stays blocked — it will exit later when its fd/process finally
	// tears down, never blocking teardown.
	if !waitOrTimeout(&p.drainWG, teardownWaitTimeout) {
		log.Printf("openkanban pane %s: drain goroutine did not exit within %s during teardown", p.id, teardownWaitTimeout)
	}
	if !waitOrTimeout(&p.inputWG, teardownWaitTimeout) {
		log.Printf("openkanban pane %s: input-writer goroutine did not exit within %s during teardown", p.id, teardownWaitTimeout)
	}
	p.stopReadLoop()
}

func (p *Pane) Stop() error {
	p.teardownUnlocked(false)
	return nil
}

// StopGraceful sends SIGTERM to the child's process group, waits up to
// timeout for it to exit, then SIGKILLs the group and tears down. The
// teardown (close-before-wait) is shared with Stop via teardownUnlocked.
func (p *Pane) StopGraceful(timeout time.Duration) error {
	p.mu.Lock()
	if !p.running || p.cmd == nil || p.cmd.Process == nil {
		p.mu.Unlock()
		return nil
	}
	proc := p.cmd.Process
	p.mu.Unlock()

	// SIGTERM the whole group so children get a chance to clean up.
	p.signalGroup(syscall.SIGTERM)

	done := make(chan error, 1)
	go func() {
		_, err := proc.Wait()
		done <- err
	}()

	select {
	case <-done:
	case <-time.After(timeout):
	}

	// SIGKILL the group regardless — if the child exited within the
	// grace window the group is already gone and this is a harmless
	// no-op; otherwise it reaps the tree. teardownUnlocked then does the
	// close-before-wait fd teardown.
	p.signalGroup(syscall.SIGKILL)
	p.teardownUnlocked(true)
	return nil
}

var ErrPaneNotRunning = fmt.Errorf("pane is not running")

// ErrInputBackpressure is returned by WriteInput when the bounded input
// queue is full because the child has stopped draining stdin. The chunk
// is dropped whole (never partially written) and the caller decides how
// to react — the daemon's attach loop treats it as non-fatal (drop the
// chunk, stay attached) and the watchdog surfaces persistent
// backpressure as a "stuck" session.
var ErrInputBackpressure = fmt.Errorf("pane input buffer full (child not draining stdin)")

// WriteInput queues data for the child's stdin via the single per-pane
// writer goroutine. It NEVER holds p.mu across the PTY write — the
// non-blocking enqueue below is what guarantees p.mu can't span a
// blocking syscall, the root-cause fix for the paste-flood deadlock.
//
// A chunk is all-or-nothing: on backpressure the WHOLE chunk is dropped
// and ErrInputBackpressure is returned (never a partial write that would
// corrupt the input stream).
func (p *Pane) WriteInput(data []byte) (int, error) {
	if !p.runningAtomic.Load() {
		return 0, ErrPaneNotRunning
	}
	ch := p.inputCh
	if ch == nil {
		return 0, ErrPaneNotRunning
	}
	// Copy the payload — the caller (attach binary loop) reuses its
	// frame buffer, and the writer goroutine reads the slice later.
	buf := append([]byte(nil), data...)
	select {
	case ch <- buf:
		// A lone success does NOT clear the wedge episode — see the
		// wedgedSinceNs/lastWedgeNs field comment. The episode ages out
		// of WedgedSince() naturally once backpressure stops recurring.
		return len(data), nil
	default:
		// Bounded backpressure: the child isn't draining stdin and the
		// queue is full. Open a new episode only if the previous one has
		// gone quiet (no backpressure for wedgeRecency); otherwise extend
		// the current one by refreshing lastWedgeNs while keeping the
		// original start.
		now := time.Now().UnixNano()
		last := p.lastWedgeNs.Load()
		if last == 0 || now-last > int64(wedgeRecency) {
			p.wedgedSinceNs.Store(now)
		}
		p.lastWedgeNs.Store(now)
		return 0, ErrInputBackpressure
	}
}

// wedgeRecency is how long after the last backpressure a pane is still
// considered "in a wedge episode". A child not draining stdin lets the
// occasional chunk through (PTY slack), so backpressure recurs in bursts
// rather than continuously; this window bridges those gaps so the
// episode start (wedgedSinceNs) is stable, while a genuinely-recovered
// pane (no backpressure for longer than this) reports un-wedged.
const wedgeRecency = 2 * time.Second

// WedgedSince returns the start of the current input-backpressure episode
// (the time WriteInput first backpressured in this episode), or the zero
// time if the pane is not currently wedged — i.e. no backpressure has
// occurred within wedgeRecency. Lock-free (atomics only) so the daemon
// watchdog can poll it without ever blocking on the wedged pane.
func (p *Pane) WedgedSince() time.Time {
	last := p.lastWedgeNs.Load()
	if last == 0 || time.Since(time.Unix(0, last)) > wedgeRecency {
		return time.Time{} // never wedged, or recovered
	}
	start := p.wedgedSinceNs.Load()
	if start == 0 {
		return time.Time{}
	}
	return time.Unix(0, start)
}

// readOutput returns a Cmd that reads from the PTY
func (p *Pane) readOutput() tea.Cmd {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.readOutputUnlocked()
}

// readOutputUnlocked must be called with mu held.
//
// Bridges the new channel-based subscription API back into BubbleTea's
// Cmd model. Returns a Cmd that, on first invocation, lazily
// Subscribes the Pane (which kicks off startReadLoop the first time
// any subscriber appears) and caches the channel. Subsequent
// invocations read one event from that cached channel and translate
// it to an OutputMsg or ExitMsg. Title/ModeEvents are silently
// dropped by this bridge — the UI does not yet wire them through
// tea.Msg.
//
// The lazy Subscribe is deliberately deferred into the Cmd closure
// (not done eagerly here). The closure runs from the tea runtime
// without p.mu held; Subscribe → startReadLoop wants p.mu briefly,
// which would self-deadlock if we Subscribed inline while Start still
// holds the lock.
func (p *Pane) readOutputUnlocked() tea.Cmd {
	if p.pty == nil {
		return nil
	}

	paneID := p.id

	return func() tea.Msg {
		p.teaBridgeOnce.Do(func() {
			ch, _ := p.Subscribe()
			p.teaBridgeCh = ch
		})
		ch := p.teaBridgeCh

		for ev := range ch {
			switch e := ev.(type) {
			case OutputEvent:
				return OutputMsg{PaneID: paneID, Data: e.Data}
			case ExitEvent:
				return ExitMsg{PaneID: paneID, Err: e.Err}
			default:
				// TitleEvent / ModeEvent: drop. The UI learns
				// about these through other paths (Pane.Title()
				// is polled by the model; mouse/alt-screen are
				// checked at use-time).
				continue
			}
		}
		// Channel closed without ExitEvent (e.g. Stop torn the
		// pane down). Synthesize an exit so the UI cleans up.
		return ExitMsg{PaneID: paneID, Err: io.EOF}
	}
}

// --- Update Handler ---

// Update handles messages for this pane, returns commands to execute
func (p *Pane) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case OutputMsg:
		if msg.PaneID != p.id {
			return nil
		}
		// Bytes were already processed by the read goroutine
		// (handleOutput is invoked there before fan-out). The
		// OutputMsg arriving here is purely a signal to schedule a
		// render tick and re-arm the Cmd that reads from the tea
		// bridge channel.
		return tea.Batch(p.readOutput(), p.scheduleRenderTick())

	case RenderTickMsg:
		if msg.PaneID != p.id {
			return nil
		}
		p.mu.Lock()
		p.renderScheduled = false
		p.mu.Unlock()
		return nil

	case ExitMsg:
		if msg.PaneID != p.id {
			return nil
		}
		// Record the exit error first (not covered by teardownOnce).
		p.mu.Lock()
		p.exitErr = msg.Err
		p.mu.Unlock()

		// Route through teardownOnce so this path and Stop()/StopGraceful()
		// are mutually exclusive — avoids the legacy anti-pattern of closing
		// p.pty under p.mu and leaking the input-writer goroutine. The read
		// loop has already exited (it produced this ExitMsg), so
		// teardownUnlocked's stopReadLoop call is a cheap idempotent no-op.
		// alreadyKilled=true because the process already exited naturally.
		p.teardownUnlocked(true)
		return nil
	}

	return nil
}

func (p *Pane) handleOutput(data []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.vt == nil {
		return
	}

	// Diagnostic: capture the raw byte stream when OPENKANBAN_PTY_DEBUG_LOG
	// is set. No-op overhead when disabled (a single nil-check).
	ptyDebugLog(p.id, data)

	p.detectMouseModeChanges(data)
	p.detectAltScreenChanges(data)

	p.vt.Write(data)

	// Stamp activity for the daemon's status broadcaster. Lock-free
	// write — readers (the broadcaster goroutine) use atomic.Load.
	if len(data) > 0 {
		p.lastActivityNs.Store(time.Now().UnixNano())
	}

	p.dirty = true
}

// LastActivity returns the timestamp of the most recent PTY output
// observed by this pane, or the zero time if the pane has produced no
// output yet. Safe to call from any goroutine; reads atomically without
// taking p.mu so the daemon's activity broadcaster doesn't contend with
// the read loop.
func (p *Pane) LastActivity() time.Time {
	if p == nil {
		return time.Time{}
	}
	ns := p.lastActivityNs.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// detectMouseModeChanges scans output for mouse tracking mode escape sequences.
// Called with mutex held. Emits a ModeEvent on transition so subscribers
// learn about mouse-tracking flips without parsing bytes themselves.
func (p *Pane) detectMouseModeChanges(data []byte) {
	// Mouse tracking enable sequences (any of these enables mouse mode)
	enableSeqs := [][]byte{
		[]byte("\x1b[?1000h"), // X10 mouse tracking
		[]byte("\x1b[?1002h"), // Button-event tracking
		[]byte("\x1b[?1003h"), // Any-event tracking
		[]byte("\x1b[?1006h"), // SGR extended mode
	}

	// Mouse tracking disable sequences
	disableSeqs := [][]byte{
		[]byte("\x1b[?1000l"),
		[]byte("\x1b[?1002l"),
		[]byte("\x1b[?1003l"),
		[]byte("\x1b[?1006l"),
	}

	// Check for enable sequences
	for _, seq := range enableSeqs {
		if bytes.Contains(data, seq) {
			if !p.mouseEnabled {
				p.mouseEnabled = true
				p.publishModeEventLocked()
			}
			return
		}
	}

	// Check for disable sequences
	for _, seq := range disableSeqs {
		if bytes.Contains(data, seq) {
			if p.mouseEnabled {
				p.mouseEnabled = false
				p.publishModeEventLocked()
			}
			return
		}
	}
}

// publishModeEventLocked snapshots the three flags and publishes a
// ModeEvent. Must be called with p.mu held. publish only takes
// subscribersMu, so this is safe under p.mu (see publish docstring).
func (p *Pane) publishModeEventLocked() {
	p.publish(ModeEvent{
		Mouse:        p.mouseEnabled,
		AltScreen:    p.altScreenActive,
		CursorHidden: p.cursorHidden.Load(),
	})
}

// detectAltScreenChanges scans output for alternate screen mode escape sequences.
// Called with mutex held. Emits a ModeEvent on transition so subscribers
// can react without re-parsing bytes.
func (p *Pane) detectAltScreenChanges(data []byte) {
	// Alternate screen enable sequences (smcup)
	enableSeqs := [][]byte{
		[]byte("\x1b[?1049h"), // Save cursor + switch to alt screen
		[]byte("\x1b[?47h"),   // Switch to alt screen (legacy)
	}

	// Alternate screen disable sequences (rmcup)
	disableSeqs := [][]byte{
		[]byte("\x1b[?1049l"), // Restore cursor + switch from alt screen
		[]byte("\x1b[?47l"),   // Switch from alt screen (legacy)
	}

	// Check for enable sequences
	for _, seq := range enableSeqs {
		if bytes.Contains(data, seq) {
			if !p.altScreenActive {
				p.altScreenActive = true
				p.publishModeEventLocked()
			}
			return
		}
	}

	// Check for disable sequences
	for _, seq := range disableSeqs {
		if bytes.Contains(data, seq) {
			if p.altScreenActive {
				p.altScreenActive = false
				p.publishModeEventLocked()
			}
			return
		}
	}
}

// scheduleRenderTick returns a Cmd to trigger render after throttle interval
func (p *Pane) scheduleRenderTick() tea.Cmd {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.renderScheduled {
		return nil
	}
	p.renderScheduled = true

	timeSinceLastRender := time.Since(p.lastRender)
	delay := renderInterval - timeSinceLastRender
	if delay < 0 {
		delay = 0
	}

	paneID := p.id
	return tea.Tick(delay, func(time.Time) tea.Msg {
		return RenderTickMsg{PaneID: paneID}
	})
}

// --- Key Handling (Issue #15) ---

func (p *Pane) HandleMouse(msg tea.MouseMsg) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.running || p.pty == nil {
		return
	}

	// Selection processing runs regardless of whether the child has
	// mouse tracking enabled, so the user can always drag-to-select
	// and Cmd+C-to-copy text from any pane. When the child also has
	// mouse tracking on, the event is ALSO forwarded to it below
	// (unless Shift is held — see the Shift bypass at the end).
	//
	// A bare click without drag does not produce a persistent
	// selection (SelectionState.Finish transitions to Idle when
	// anchor==cursor), so menu clicks still work normally.
	if p.selection != nil {
		switch msg.Button {
		case tea.MouseButtonLeft:
			pos := p.viewportToLogical(msg.X, msg.Y)
			switch msg.Action {
			case tea.MouseActionPress:
				p.selection.Start(pos)
				p.dirty = true
			case tea.MouseActionMotion:
				p.selection.Update(pos)
				p.dirty = true
			case tea.MouseActionRelease:
				p.selection.Finish()
				p.dirty = true
			}
		case tea.MouseButtonNone:
			if p.selection.Mode == SelectionSelecting {
				pos := p.viewportToLogical(msg.X, msg.Y)
				p.selection.Update(pos)
				p.dirty = true
			}
		case tea.MouseButtonRight, tea.MouseButtonMiddle:
			if p.selection.IsActive() {
				p.selection.Clear()
				p.dirty = true
			}
		case tea.MouseButtonWheelUp, tea.MouseButtonWheelDown:
			// Scrolling invalidates the pinned selection coordinates.
			if p.selection.IsActive() {
				p.selection.Clear()
				p.dirty = true
			}
		}
	}

	if !p.mouseEnabled {
		return
	}

	// Mouse tracking is enabled. Shift held = the user is claiming the
	// event for openkanban (selection already handled above); don't
	// also pass it to the child.
	if msg.Shift {
		return
	}

	var seq []byte
	x, y := msg.X+1, msg.Y+1
	if x > 223 {
		x = 223
	}
	if y > 223 {
		y = 223
	}

	switch msg.Button {
	case tea.MouseButtonWheelUp:
		seq = []byte{'\x1b', '[', 'M', byte(64 + 32), byte(x + 32), byte(y + 32)}
	case tea.MouseButtonWheelDown:
		seq = []byte{'\x1b', '[', 'M', byte(65 + 32), byte(x + 32), byte(y + 32)}
	case tea.MouseButtonLeft:
		seq = []byte{'\x1b', '[', 'M', byte(0 + 32), byte(x + 32), byte(y + 32)}
	case tea.MouseButtonRight:
		seq = []byte{'\x1b', '[', 'M', byte(2 + 32), byte(x + 32), byte(y + 32)}
	case tea.MouseButtonMiddle:
		seq = []byte{'\x1b', '[', 'M', byte(1 + 32), byte(x + 32), byte(y + 32)}
	}

	if len(seq) > 0 {
		p.pty.Write(seq)
	}
}

// viewportToLogical converts viewport coordinates to logical position
// Logical position: negative row = scrollback, 0+ = live screen
// Called with mutex held.
func (p *Pane) viewportToLogical(x, y int) Position {
	return Position{Row: y, Col: x}
}

// HandleKey processes a key event and sends to PTY
func (p *Pane) HandleKey(msg tea.KeyMsg) tea.Msg {
	if msg.String() == "ctrl+g" {
		return ExitFocusMsg{}
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.running || p.pty == nil {
		return nil
	}

	key := msg.String()

	// Check for selection copy FIRST (before forwarding Ctrl+C to PTY)
	if p.selection != nil && p.selection.IsActive() {
		if key == "ctrl+c" || key == "cmd+c" {
			p.copySelectionUnlocked()
			return nil
		}
	}

	// Clear selection on any keyboard input (except copy)
	if p.selection != nil && p.selection.IsActive() {
		p.selection.Clear()
		p.dirty = true
	}

	input := p.translateKey(msg)
	if len(input) > 0 {
		p.pty.Write(input)
	}

	return nil
}

// copySelectionUnlocked copies selected text to clipboard
// Called with mutex held.
func (p *Pane) copySelectionUnlocked() {
	if p.selection == nil || !p.selection.IsActive() {
		return
	}

	// Get live screen accessor. SafeEmulator handles its own locking
	// for CellAt, so the closure is safe to use directly.
	liveRows := p.vt.Height()
	liveScreen := func(col, row int) Glyph {
		return cellToGlyph(p.vt.CellAt(col, row))
	}

	text := p.selection.ExtractText(nil, liveScreen, liveRows, 0)

	if text != "" {
		clipboard.WriteAll(text)
	}

	// Clear selection after copy
	p.selection.Clear()
	p.dirty = true
}


// translateKey converts Bubbletea KeyMsg to PTY byte sequences.
//
// Cursor/arrow keys honor DECCKM (application cursor keys mode): when
// the inner emulator has it set (CC and most modern TUIs enable it via
// ESC[?1h), we emit SS3 sequences (ESC O A/B/C/D) instead of CSI
// (ESC [ A/B/C/D). This matches iTerm2 / xterm / charm/x/vt's own
// SendKey behavior; without it, arrows fall into whatever default the
// inner app applies to a "normal-mode" arrow keystroke.
func (p *Pane) translateKey(msg tea.KeyMsg) []byte {
	key := msg.String()

	// Handle modifier combinations
	switch {
	// Ctrl+A through Ctrl+Z → 0x01-0x1A
	case len(key) == 6 && key[:5] == "ctrl+" && key[5] >= 'a' && key[5] <= 'z':
		return []byte{byte(key[5] - 'a' + 1)}

	// Alt+letter → ESC + letter
	case len(key) == 5 && key[:4] == "alt+" && key[4] >= 'a' && key[4] <= 'z':
		return []byte{27, key[4]}
	}

	appCursor := p.cursorAppMode.Load()

	// Handle special keys
	switch msg.Type {
	case tea.KeyEnter:
		// Bubbletea v1 has no Shift field; terminals configured to
		// send shift+enter emit ESC+CR, which bubbletea reports as
		// Alt+Enter. Pass that through verbatim so the inner agent
		// (Claude Code, etc.) sees the meta-Enter and inserts a
		// newline instead of submitting.
		if msg.Alt {
			return []byte{27, '\r'}
		}
		return []byte("\r")
	case tea.KeyBackspace:
		return []byte{127}
	case tea.KeyTab:
		return []byte("\t")
	case tea.KeyShiftTab:
		return []byte("\x1b[Z")
	case tea.KeyUp:
		if appCursor {
			return []byte("\x1bOA")
		}
		return []byte("\x1b[A")
	case tea.KeyDown:
		if appCursor {
			return []byte("\x1bOB")
		}
		return []byte("\x1b[B")
	case tea.KeyRight:
		if appCursor {
			return []byte("\x1bOC")
		}
		return []byte("\x1b[C")
	case tea.KeyLeft:
		if appCursor {
			return []byte("\x1bOD")
		}
		return []byte("\x1b[D")
	case tea.KeyEscape:
		return []byte{27}
	case tea.KeyHome:
		return []byte("\x1b[H")
	case tea.KeyEnd:
		return []byte("\x1b[F")
	case tea.KeyPgUp:
		return []byte("\x1b[5~")
	case tea.KeyPgDown:
		return []byte("\x1b[6~")
	case tea.KeyDelete:
		return []byte("\x1b[3~")
	case tea.KeySpace:
		return []byte(" ")
	case tea.KeyRunes:
		return []byte(string(msg.Runes))
	}

	return nil
}

// GetContent returns the current terminal content as plain text for analysis.
func (p *Pane) GetContent() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.contentLocked()
}

// GetContentTry is a non-blocking GetContent: it returns ("", false) if
// p.mu is currently held rather than waiting for it. Callers on a hot
// path that must NOT block on pane teardown (e.g. the daemon's status
// broadcaster, where a stuck Stop() holding p.mu would otherwise freeze
// every session's heartbeat) use this and treat !ok as "no reading this
// tick" rather than blocking.
func (p *Pane) GetContentTry() (string, bool) {
	if !p.mu.TryLock() {
		return "", false
	}
	defer p.mu.Unlock()
	return p.contentLocked(), true
}

// contentLocked renders the visible grid to text. Caller must hold p.mu.
func (p *Pane) contentLocked() string {
	if p.vt == nil {
		return ""
	}

	cols := p.vt.Width()
	rows := p.vt.Height()
	if cols <= 0 || rows <= 0 {
		return ""
	}

	var result strings.Builder
	for row := 0; row < rows; row++ {
		if row > 0 {
			result.WriteByte('\n')
		}
		for col := 0; col < cols; col++ {
			ch := cellToGlyph(p.vt.CellAt(col, row)).Char
			if ch == 0 {
				ch = ' '
			}
			result.WriteRune(ch)
		}
	}

	return result.String()
}

// --- Rendering (Issue #14) ---


// selfExecutable resolves the path of the currently-running binary. It is a
// package var only so tests can substitute a deterministic path; production
// code always uses os.Executable.
var selfExecutable = os.Executable

// openkanbanSelfBin returns the directory and full path of the currently-
// running openkanban binary (symlinks resolved). Both are "" when the
// executable can't be determined — callers then skip the PATH/OPENKANBAN_BIN
// injection and leave the inherited PATH untouched.
//
// buildCleanEnv runs inside openkanbankd, so this is the same build that owns
// the board: putting its directory first on the spawned agent's PATH makes a
// bare `openkanban ...` invocation resolve to it, not to a stale/older
// `openkanban` earlier on the user's PATH (e.g. a Homebrew/upstream install
// that predates the `ticket` subcommand).
func openkanbanSelfBin() (dir, path string) {
	exe, err := selfExecutable()
	if err != nil || exe == "" {
		return "", ""
	}
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil && resolved != "" {
		exe = resolved
	}
	return filepath.Dir(exe), exe
}

// selfBinResolves reports whether a bare `openkanban` on the inherited PATH
// already resolves to binPath (symlinks resolved). buildCleanEnv uses this to
// keep the PATH injection surgical: when this build is already the one a
// spawned agent would find, leave PATH untouched rather than reorder it.
func selfBinResolves(binPath string) bool {
	if binPath == "" {
		return false
	}
	p, err := exec.LookPath("openkanban")
	if err != nil {
		return false
	}
	if resolved, rerr := filepath.EvalSymlinks(p); rerr == nil && resolved != "" {
		p = resolved
	}
	return p == binPath
}

func buildCleanEnv(sessionName, ticketID string) []string {
	// Resolve our own binary up front so we can front-load its directory onto
	// the spawned agent's PATH (see openkanbanSelfBin). Skills that create
	// sibling tickets (spin-off-a-ticket, fan-out-a-plan) shell out to
	// `openkanban ticket new`; without this the non-interactive agent shell
	// would resolve `openkanban` to whatever the daemon inherited — which for
	// a user who installed a release via Homebrew and runs the fork through a
	// shell alias is a stale binary with no `ticket` command.
	binDir, binPath := openkanbanSelfBin()
	// Only rewrite PATH when a bare `openkanban` would NOT already resolve to
	// this build — i.e. an older/other `openkanban` shadows us, or none is on
	// PATH. This keeps the common single-install case a no-op (no PATH
	// reordering) and acts only for the split-install case this fixes: a stale
	// Homebrew/upstream `openkanban` on PATH while the fork build runs the
	// board. When it does inject, the directory is typically a dedicated one
	// (e.g. the app bundle or a GOBIN), so the reordering is benign.
	injectDir := ""
	if binDir != "" && !selfBinResolves(binPath) {
		injectDir = binDir
	}

	var env []string
	pathSeen := false
	for _, e := range os.Environ() {
		key := strings.Split(e, "=")[0]
		if key == "OPENCODE" || strings.HasPrefix(key, "OPENCODE_") {
			continue
		}
		if key == "CLAUDE" || strings.HasPrefix(key, "CLAUDE_") {
			continue
		}
		if key == "GEMINI" || strings.HasPrefix(key, "GEMINI_") {
			continue
		}
		if key == "CODEX" || strings.HasPrefix(key, "CODEX_") {
			continue
		}
		// Strip any inherited OPENKANBAN_* so each spawn gets exactly the
		// session + ticket id configured for THIS pane and nothing else.
		// Without this, nesting openkanban inside an openkanban-spawned
		// shell would leak the outer pane's identity into the inner child.
		// (OPENKANBAN_BIN is re-synthesized fresh below for the same reason.)
		if strings.HasPrefix(key, "OPENKANBAN_") {
			continue
		}
		if key == "PATH" {
			pathSeen = true
			if injectDir != "" {
				// Prepend our binary's directory so `openkanban` resolves to
				// the build running the board, not a stale one earlier on PATH.
				env = append(env, "PATH="+injectDir+string(os.PathListSeparator)+strings.TrimPrefix(e, "PATH="))
				continue
			}
		}
		env = append(env, e)
	}
	// If the inherited env had no PATH at all, still make our binary findable.
	if injectDir != "" && !pathSeen {
		env = append(env, "PATH="+injectDir)
	}
	env = append(env, "TERM=xterm-256color")
	// Explicit pointer to the running build so a script/skill can prefer it
	// over PATH resolution if it wants to be certain which openkanban it runs.
	if binPath != "" {
		env = append(env, "OPENKANBAN_BIN="+binPath)
	}
	if sessionName != "" {
		env = append(env, "OPENKANBAN_SESSION="+sessionName)
	}
	if ticketID != "" {
		env = append(env, "OPENKANBAN_TICKET_ID="+ticketID)
	}
	return env
}

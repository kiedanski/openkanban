package ui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/techdufus/openkanban/internal/agent"
	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/config"
	"github.com/techdufus/openkanban/internal/daemon"
	"github.com/techdufus/openkanban/internal/daemonclient"
	"github.com/techdufus/openkanban/internal/git"
	"github.com/techdufus/openkanban/internal/project"
	"github.com/techdufus/openkanban/internal/terminal"
	"github.com/techdufus/openkanban/internal/ticketsvc"
	"github.com/techdufus/openkanban/internal/update"
	"github.com/techdufus/openkanban/internal/workflow"
)

const agentPortBase = 4097

type Mode string

const (
	ModeNormal        Mode = "NORMAL"
	ModeInsert        Mode = "INSERT"
	ModeHelp          Mode = "HELP"
	ModeConfirm       Mode = "CONFIRM"
	ModeConfirmExit   Mode = "CONFIRM_EXIT"
	ModeCreateTicket  Mode = "CREATE"
	ModeEditTicket    Mode = "EDIT"
	ModeAgentView     Mode = "AGENT"
	ModeSettings      Mode = "SETTINGS"
	ModeShuttingDown  Mode = "SHUTTING_DOWN"
	ModeSpawning      Mode = "SPAWNING"
	ModeFilter        Mode = "FILTER"
	ModeCreateProject Mode = "NEW_PROJECT"
	ModeEditProject   Mode = "EDIT_PROJECT"
)

// SortMode controls the order tickets appear within each column.
// SortDefault preserves the store's natural (map-iteration) order so
// existing behavior is unchanged until the user opts in with `o`.
type SortMode string

const (
	SortDefault      SortMode = ""
	SortName         SortMode = "name"
	SortAge          SortMode = "age"
	SortStatusChange SortMode = "status_change"
	SortPriority     SortMode = "priority"
)

// sortModes is the cycle order the `o` keybinding walks. Kept here so
// the cycle and the label/help text stay in sync.
var sortModes = []SortMode{SortDefault, SortName, SortAge, SortStatusChange, SortPriority}

func nextSortMode(s SortMode) SortMode {
	for i, m := range sortModes {
		if m == s {
			return sortModes[(i+1)%len(sortModes)]
		}
	}
	return SortDefault
}

func sortModeLabel(s SortMode) string {
	switch s {
	case SortName:
		return "name (A→Z)"
	case SortAge:
		return "age (newest first)"
	case SortStatusChange:
		return "status change (newest first)"
	case SortPriority:
		return "priority (highest first)"
	default:
		return "default"
	}
}

// SessionFilter narrows the board to tickets matching a session-state
// predicate. "Open" means the ticket has a live daemon session
// (daemonOwned). Session-only (not persisted) — same lifetime
// convention as sortMode.
type SessionFilter string

const (
	SessionFilterAll  SessionFilter = ""
	SessionFilterOpen SessionFilter = "open"
)

var sessionFilters = []SessionFilter{SessionFilterAll, SessionFilterOpen}

func nextSessionFilter(f SessionFilter) SessionFilter {
	for i, sf := range sessionFilters {
		if sf == f {
			return sessionFilters[(i+1)%len(sessionFilters)]
		}
	}
	return SessionFilterAll
}

func sessionFilterLabel(f SessionFilter) string {
	switch f {
	case SessionFilterOpen:
		return "open sessions"
	default:
		return "all"
	}
}

const (
	minColumnWidth = 20
	columnOverhead = 5

	// ticketHeight is the fallback estimate of a ticket card's rendered
	// height in rows, used by ensureTicketVisible and hitTestTicket only
	// when the per-render Model.columnTicketHeights cache is empty (e.g.
	// before the first View() call after a window-size change). The actual
	// rendered height varies (8 for a single-row title, 9 when the title
	// wraps to 2 rows) and is measured by renderColumn via lipgloss.Height.
	ticketHeight       = 8
	columnHeaderHeight = 3
	// indicatorReserveRows reserves vertical space for the "▲ N more" and
	// "▼ N more" overflow indicators rendered inside a column. Kept as a
	// constant for the small handful of callers (e.g. ensureTicketVisible
	// fallback) that still want a worst-case reservation without walking
	// columnTicketHeights.
	indicatorReserveRows = 2

	formFieldTitle       = 0
	formFieldDescription = 1
	formFieldProject     = 2
	formFieldBranch      = 3
	formFieldLabels      = 4
	formFieldPriority    = 5
	formFieldType        = 6
	formFieldWorktree    = 7
	formFieldBlockedBy   = 8
)

// ticketTypeOptions is the ordered set the create/edit form's Type picker
// cycles through (freeform first = today's default). Shared by handleTypeNav
// and renderTypeSelector so the order stays in one place.
var ticketTypeOptions = []board.TicketType{
	board.TypeFreeform,
	board.TypeResearch,
	board.TypeSpec,
	board.TypeImplement,
	board.TypeReview,
}

// defaultWorktreeForType returns the sensible UseWorktree default for a type:
// research/spec are read-only report stages (no worktree, no branch churn);
// implement/review/freeform get a worktree. The form applies this when the
// Type picker changes, and the user can still flip the Worktree field after.
func defaultWorktreeForType(t board.TicketType) bool {
	return t != board.TypeResearch && t != board.TypeSpec
}

type choiceItem struct {
	Key   rune
	Label string
	Fn    func() tea.Cmd
}

// spawnPlan is the user's chosen direction when openkanban detects a
// stale-brief situation (prior session exists AND the merge would
// change the brief on disk). It is passed by value into prepareSpawnWith
// so the snapshot is unambiguous across the tea.Cmd goroutine boundary —
// do NOT convert this to a pointer.
type spawnPlan struct {
	SkipMerge          bool // option 'n' — don't write the brief
	ForceFresh         bool // option 'd' — caller has already cleared AgentSpawnedAt
	InjectResumeNotice bool // option 'u' — append a "brief updated" message after --continue
	// Unattached (ctrl+space) spawns the session but does NOT attach: the
	// closure builds an Unattached PaneView, skips the Owns fast-path and
	// attachWithRetry, and returns a spawnUnattachedReadyMsg so the TUI
	// stays on the board (ModeNormal) instead of switching to ModeAgentView.
	Unattached bool
}

type Model struct {
	config *config.Config
	theme  config.Theme
	colors uiColors

	globalStore      *project.GlobalTicketStore
	projectRegistry  *project.ProjectRegistry
	columns          []board.Column
	filterProjectIDs map[string]bool

	worktreeMgrs   map[string]*git.WorktreeManager
	agentMgr       *agent.Manager
	opencodeServer *agent.OpencodeServer

	mode          Mode
	activeColumn  int
	activeTicket  int
	width         int
	height        int
	spinner       spinner.Model
	scrollOffset  int
	columnOffsets []int

	dragging         bool
	dragSourceColumn int
	dragSourceTicket int
	dragTargetColumn int

	hoverColumn int
	hoverTicket int

	lastClickTime   time.Time
	lastClickColumn int
	lastClickTicket int

	columnTickets [][]*board.Ticket

	// columnTicketHeights mirrors columnTickets and holds the measured
	// rendered height (lipgloss.Height) of every ticket in every column at
	// the current width. Populated by renderColumn each render. Consumed by
	// ensureTicketVisible and hitTestTicket to translate between ticket
	// index and vertical-pixel space when ticket heights vary (e.g. 2-line
	// title wraps). May be nil/short before the first render — callers
	// fall back to the ticketHeight constant in that case.
	columnTicketHeights [][]int

	// sortMode is the user-selected sort applied to each column in
	// refreshColumnTickets. Session-only (not persisted); SortDefault
	// preserves the store's natural order.
	sortMode SortMode

	// sessionFilter narrows the board to tickets with a live daemon
	// session ("open"). Same session-only lifetime as sortMode; toggled
	// with 'w'.
	sessionFilter SessionFilter

	// alwaysShowWorking, when true, exempts daemon-owned ("open")
	// sessions from the project and text-search filters so working
	// sessions remain visible across project narrowing. The session
	// filter ('w') still applies on top. Session-only lifetime;
	// toggled with 'W'.
	alwaysShowWorking bool

	showHelp    bool
	showConfirm bool
	confirmMsg  string
	confirmFn   func() tea.Cmd

	showChoice bool
	choiceMsg  string
	choices    []choiceItem

	// gateOverrides records tickets for which the user chose "override &
	// start" at the workflow prerequisite offer (see startGateAllows). The
	// entry is consumed on the next spawn attempt so the override applies
	// exactly once, then the gate re-arms.
	gateOverrides map[board.TicketID]bool

	// stuckActionPrompt gates the stuck-session recover/destroy modal.
	// Shown via this bool while m.mode stays ModeNormal (the exit-guard
	// / showChoice overlay pattern); routed in handleKey's global arms
	// before the ModeNormal dispatch. stuckActionTicket is the ticket
	// whose wedged session the modal acts on.
	stuckActionPrompt bool
	stuckActionTicket board.TicketID

	titleInput         textinput.Model
	descInput          textarea.Model
	branchInput        textinput.Model
	labelsInput        textinput.Model
	ticketPriority     int
	ticketType         board.TicketType
	ticketUseWorktree  bool
	projectInput       textinput.Model
	ticketFormField    int
	editingTicketID    board.TicketID
	branchLocked       bool
	selectedProject    *project.Project
	projectListIndex   int
	showAddProjectForm bool
	addProjectPath     textinput.Model

	blockerCandidates  []*board.Ticket
	selectedBlockers   map[board.TicketID]bool
	blockerListIndex   int
	blockerFilterInput textinput.Model

	formScrollOffset int
	formFieldLines   map[int]int

	// Project-edit form (ModeEditProject): unified editor for project name +
	// pin (projects.json) and the shared agent registry (config.json).
	editingProjectID string
	peName           string
	peProjectAgent   string
	peModel          string
	peBriefs         string
	peAgents         []peAgentRow
	peField          int
	peScrollOffset   int
	peInput          textinput.Model

	notification string
	notifyTime   time.Time

	panes          map[board.TicketID]*daemonclient.PaneView
	focusedPane    board.TicketID
	statusDetector *agent.StatusDetector

	// cycleAttachPrompt is set when the user has used Ctrl+] / Ctrl+\
	// to cycle focus to a peer session that this TUI is not yet
	// attached to. View() renders an "Enter to attach" modal over the
	// agent view; handleAgentViewMode swallows all keys until the user
	// confirms (Enter), cancels (Esc → board), or cycles further. The
	// modal exists specifically to absorb the user's "I want to switch
	// to this session" keystroke so it doesn't get eaten by the
	// AttachFirstMsg handshake the first time they type into the pane.
	cycleAttachPrompt bool

	// autoAttach is the "Auto" mode toggle (board key 'a'). When true,
	// leaving the current session (Ctrl+G) does not return to the board —
	// it jumps to the session that has been WAITING the longest (FIFO by
	// StatusChangedAt), skipping sessions a sibling TUI is attached to. If
	// no such waiter exists it falls through to the board (the always-
	// available off-ramp, since the board is where the toggle lives). In-
	// memory and session-only by design — a persisted auto-pilot is a
	// multi-TUI footgun. See oldestWaitingPeer.
	autoAttach bool

	// takeoverPrompt is set when an attach probe was rejected because the
	// session is currently attached in ANOTHER openkanban TUI. View()
	// renders a confirm-default-cancel warning over the agent view;
	// handleAgentViewMode routes keys to handleTakeoverPromptKey until the
	// user confirms (Enter/y → take over) or cancels (Esc/anything →
	// board). takeoverPending carries the target so the confirm path can
	// re-issue the attach with Takeover forced (no re-probe).
	takeoverPrompt  bool
	takeoverPending struct {
		ticketID board.TicketID
		pv       *daemonclient.PaneView
	}

	// daemonWedged is set when the daemon's wedge watchdog reports a
	// suspected dispatch wedge (a "daemon_wedged" SessionEvent, or
	// HelloResp.SuspectedWedged at startup) and cleared on "daemon_unwedged".
	// Drives a warning banner with the recovery hint. The daemon does NOT
	// self-restart on a wedge (that would kill live sessions), so recovery is
	// operator-driven: `openkanban daemon restart`.
	daemonWedged bool

	// daemonClient is the long-lived control connection to openkanbankd.
	// nil when the daemon couldn't be reached at startup — every call
	// site MUST nil-check before use (the TUI degrades to a no-spawn
	// state in that case). It is reconstructed mid-session by the
	// reconnect path (daemon_reconnect.go) when the daemon restarts
	// (e.g. the stale-binary upgrade respawn) and the existing client
	// goes Closed(); both m.daemonClient and m.daemon are swapped to the
	// fresh client in handleDaemonReconnectedMsg.
	daemonClient *daemonclient.Client

	// daemonAutostart mirrors the app.go startup choice (New vs
	// NewNoAutostart). The reconnect path uses it so a launchd-managed /
	// --no-launch-daemon setup is never force-autostarted by a re-dial.
	daemonAutostart bool

	// daemonReconnecting is true while an async reconnect attempt is in
	// flight. Set AND cleared only in the Update handler (never the cmd
	// goroutine) so the 30s resync tick can't launch overlapping dials.
	daemonReconnecting bool

	// daemonEvents is the push channel returned by
	// daemonClient.Subscribe. nil when the daemon is unreachable or
	// the subscription has ended. daemonUnsub is its cancel func.
	// daemonConnected reflects whether the subscription is currently
	// live — the status-file poll honors this flag to enforce the
	// daemon-wins precedence rule.
	daemonEvents    <-chan daemon.SessionEvent
	daemonUnsub     func()
	daemonConnected atomic.Bool

	// daemonOwned tracks tickets that currently have a live daemon
	// session — populated from the startup List() and maintained by
	// handleDaemonSessionEvent on "started" / "exited". Used by the
	// file-poll precedence rule so daemon-pushed AgentStatus wins
	// regardless of whether THIS TUI is the one with an attached
	// PaneView. Without this, a second TUI watching a session spawned
	// elsewhere falls back to the on-disk status file and clobbers the
	// daemon-pushed "working" with the file's stale "idle".
	daemonOwned map[board.TicketID]struct{}

	// daemonViewing counts how many TUI clients are currently focused on
	// each ticket's daemon session (in ModeAgentView) — a ticket renders
	// the "viewing" indicator when its count is >0. Populated at startup
	// from SessionInfo.ViewerCount and maintained by daemon-pushed
	// "viewing" / "unviewing" SessionEvents (driven by SetViewing RPC
	// calls from every connected TUI's mode transitions). Reset on
	// daemon disconnect.
	daemonViewing map[board.TicketID]int

	// lastPTYActivity tracks the most recent PTY-output timestamp per
	// ticket, populated from SessionEvent.LastActivityAt on every event
	// the daemon emits. The status detector consults this to override a
	// stale file-based "waiting" → "working": Claude Code emits no hook
	// between Notification (permission granted) and PostToolUse (tool
	// finished), so during a long-running tool the file says "waiting"
	// for the whole duration even though the agent is producing output.
	// Cleared on "exited" so the map can't grow unboundedly across the
	// TUI's lifetime.
	lastPTYActivity map[board.TicketID]time.Time

	// viewingSessionID is the daemon SessionID this TUI most recently
	// told the daemon it was viewing (via SetViewing(true)). Used by
	// reconcileViewing to emit SetViewing(prev,false) / SetViewing(new,
	// true) only when the TUI's current view target actually changes —
	// dispatched at the end of every Update so any mode/focusedPane
	// transition gets caught without scattering RPC calls through every
	// case branch.
	viewingSessionID string

	// daemon is the testable seam for daemonclient.Client RPCs that
	// the UI uses (exit-guard handshake, Owns / List queries, Spawn /
	// Kill / TicketDone lifecycle). Held as an interface so tests can
	// substitute a fake without standing up a real daemon. Set from
	// daemonClient in NewModel; nil when the daemon is unreachable.
	// See daemon_api.go for the full surface and its sub-interface
	// decomposition.
	daemon daemonAPI

	// monitor is always-on diagnostic instrumentation for the bubbletea
	// main goroutine — see stallwatch.go. Captures a goroutine dump when
	// Update/View stalls (the intermittent freeze-on-session-close bug).
	monitor *stallMonitor

	// confirmExit carries modal state for ModeConfirmExit. Populated by
	// handlePrepareExitResult when the user requests quit, cleared when
	// the modal exits (either to ModeNormal on cancel or to tea.Quit).
	confirmExit confirmExitState

	spawningTicketID board.TicketID
	spawningAgent    string
	// spawnStartedAt marks when the current spawn entered ModeSpawning.
	// renderSpawning reads it to flip the overlay label from "Starting"
	// to "Attaching" once the spawn RPC's bounded window has elapsed.
	spawnStartedAt time.Time

	settingsIndex   int
	settingsEditing bool
	settingsInput   textinput.Model
	themeListIndex  int

	filterInput textinput.Model
	filterQuery string

	sidebarVisible  bool
	sidebarFocused  bool
	sidebarIndex    int
	sidebarWidth    int
	sidebarOpenOnly bool // when true, sidebar counts exclude done+archived tickets

	// binaryStaleNotified records whether the user has already been
	// shown the "binary has been updated on disk" notification for the
	// current stale-transition. Set when the periodic check first
	// detects update.BinaryStale() == true; reset back to false if the
	// check returns false (defensive — mtime can't go backwards in
	// practice, but rebuilding atop the running binary while it's open
	// could in theory drop us out of stale, and we want a clean
	// re-trigger if it ever does).
	binaryStaleNotified bool

	// recentSelfWrites tracks (mtime, size, deadline) per path for
	// suppressing fsnotify echoes of the TUI's own SaveTicket calls.
	// See internal/ui/reload.go.
	recentSelfWrites map[string]selfWriteRecord

	// boardResyncSnap is the prior tick's snapshot of every known
	// ticket file's mtime+size+projectID, keyed by absolute path. The
	// periodic board resync (internal/ui/board_resync.go) diffs the
	// current scan against this map to decide which paths to reload
	// and which to drop. Nil until the first tick completes.
	boardResyncSnap map[string]boardFileMeta

	// lastWindowTitle is the most recent value passed to
	// tea.SetWindowTitle, used to dedupe redundant title updates.
	// See computeWindowTitle / maybeSetWindowTitle.
	lastWindowTitle string
}

func NewModel(cfg *config.Config, globalStore *project.GlobalTicketStore, projectRegistry *project.ProjectRegistry, agentMgr *agent.Manager, opencodeServer *agent.OpencodeServer, filterProjectID string, ownedByDaemon map[board.TicketID]daemon.SessionInfo, daemonClient *daemonclient.Client, autostartDaemon bool) *Model {
	ti := textinput.New()
	ti.Placeholder = "Enter ticket title..."
	ti.CharLimit = 100
	ti.Width = 40

	di := textarea.New()
	di.Placeholder = "Optional description..."
	di.CharLimit = 0
	di.SetWidth(40)
	di.SetHeight(4)
	di.ShowLineNumbers = false

	bi := textinput.New()
	bi.Placeholder = "Auto-generated from title..."
	bi.CharLimit = 100
	bi.Width = 40

	li := textinput.New()
	li.Placeholder = "bug, urgent, frontend (comma-separated)"
	li.CharLimit = 200
	li.Width = 40

	pi := textinput.New()
	pi.Placeholder = "Select project..."
	pi.CharLimit = 100
	pi.Width = 40

	si := textinput.New()
	si.CharLimit = 200
	si.Width = 40

	fi := textinput.New()
	fi.Placeholder = "Search tickets..."
	fi.CharLimit = 100
	fi.Width = 30

	ap := textinput.New()
	ap.Placeholder = "/path/to/repository"
	ap.CharLimit = 256
	ap.Width = 40

	bf := textinput.New()
	bf.Placeholder = "Filter tickets..."
	bf.CharLimit = 100
	bf.Width = 30

	sp := spinner.New()
	sp.Spinner = spinner.Dot

	worktreeMgrs := make(map[string]*git.WorktreeManager)
	for _, p := range globalStore.Projects() {
		worktreeMgrs[p.ID] = git.NewWorktreeManager(p)
	}

	var selectedProject *project.Project
	projects := globalStore.Projects()
	if len(projects) > 0 {
		if filterProjectID != "" {
			selectedProject = globalStore.GetProject(filterProjectID)
		}
		if selectedProject == nil {
			selectedProject = projects[0]
		}
	}

	theme := cfg.GetTheme()
	m := &Model{
		config:             cfg,
		theme:              theme,
		colors:             newUIColors(theme),
		globalStore:        globalStore,
		projectRegistry:    projectRegistry,
		columns:            board.DefaultColumns(),
		filterProjectIDs:   make(map[string]bool),
		worktreeMgrs:       worktreeMgrs,
		agentMgr:           agentMgr,
		opencodeServer:     opencodeServer,
		mode:               ModeNormal,
		titleInput:         ti,
		descInput:          di,
		branchInput:        bi,
		labelsInput:        li,
		ticketPriority:     3,
		projectInput:       pi,
		settingsInput:      si,
		filterInput:        fi,
		addProjectPath:     ap,
		blockerFilterInput: bf,
		selectedBlockers:   make(map[board.TicketID]bool),
		formFieldLines:     make(map[int]int),
		spinner:            sp,
		panes:              make(map[board.TicketID]*daemonclient.PaneView),
		daemonOwned:        make(map[board.TicketID]struct{}),
		daemonViewing:      make(map[board.TicketID]int),
		lastPTYActivity:    make(map[board.TicketID]time.Time),
		statusDetector:     agent.NewStatusDetector(),
		selectedProject:    selectedProject,
		sidebarVisible:     cfg.UI.SidebarVisible,
		sidebarWidth:       24,
		hoverColumn:        -1,
		hoverTicket:        -1,
		daemonClient:       daemonClient,
		daemonAutostart:    autostartDaemon,
	}
	if daemonClient != nil {
		m.daemon = daemonClient
	}
	if filterProjectID != "" {
		m.filterProjectIDs[filterProjectID] = true
	}

	// Startup reconciliation. Replaces the old unconditional
	// "wipe all AgentStatus" pass: the daemon may already own live
	// sessions from a previous TUI run (or a sibling TUI), so blindly
	// resetting status would lie about the world.
	//
	// ownedByDaemon is the daemon's session snapshot, fetched by the
	// caller's bounded preflight List (internal/app) and passed in.
	// NewModel performs NO daemon RPC of its own — that synchronous
	// reconcile used to block startup for up to ~30s, and the unbounded
	// Subscribe right after it could hang forever against a wedged daemon.
	// The preflight has already gated launch-vs-exit on the daemon's
	// health, so by the time we get here the snapshot is trustworthy.
	//
	// Algorithm:
	//   1. Record every session in the snapshot as daemon-owned (and its
	//      viewer count) so the indicators render correctly.
	//   2. For every ticket whose ID matches a live session, construct a
	//      PaneView in Unattached state and keep any status we can read
	//      from the on-disk marker. For every ticket NOT owned by the
	//      daemon, wipe any stale "working/waiting/etc" status.
	for tid, s := range ownedByDaemon {
		m.daemonOwned[tid] = struct{}{}
		if s.ViewerCount > 0 {
			m.daemonViewing[tid] = s.ViewerCount
		}
	}

	for _, ticket := range globalStore.All() {
		if info, ok := ownedByDaemon[ticket.ID]; ok {
			pv := daemonclient.NewPaneView(daemonClient, string(ticket.ID), info.SessionID, &info)
			if info.Workdir != "" {
				pv.SetWorkdir(info.Workdir)
			}
			if info.SessionName != "" {
				pv.SetSessionName(info.SessionName)
			}
			pv.SetTicketTitle(ticket.Title)
			m.panes[ticket.ID] = pv
			// Best-effort status read from the existing on-disk marker.
			// PR9 will replace this with push events.
			if st := agent.ReadAgentStatus(info.SessionName); st != board.AgentNone {
				if ticket.SetAgentStatus(st) {
					globalStore.Save(ticket)
				}
			}
			continue
		}
		if ticket.SetAgentStatus(board.AgentNone) {
			globalStore.Save(ticket)
		}
	}

	m.refreshColumnTickets()

	// Subscribe to daemon push events is armed ASYNCHRONOUSLY from Init
	// (subscribeDaemonEventsCmd), NOT here. The handshake used to run
	// synchronously in NewModel under context.Background(); a wedged
	// daemon never answered it and blocked startup forever, before the
	// bubbletea loop began. daemonSubscribeReadyMsg installs the channel
	// once the bounded handshake completes; until then the startup
	// snapshot + periodic resync cover the brief gap.

	// Diagnostic stall monitor — created here so Update/View stamping has
	// a target, but the watchdog goroutine + SIGUSR2 handler are only
	// armed by StartStallMonitor (from app.go), keeping NewModel side in
	// tests free of leaked goroutines.
	var diag func() (uint64, uint64)
	if daemonClient != nil {
		diag = daemonClient.DiagCounters
	}
	m.monitor = newStallMonitor(diag)

	// If the daemon already suspected a wedge when we dialed in, surface the
	// banner from the first frame (the daemon_wedged push only fires on the
	// transition, so a TUI that connects mid-episode wouldn't otherwise know).
	if daemonClient != nil && daemonClient.SuspectedWedgedAtHello() {
		m.daemonWedged = true
	}

	return m
}

func (m *Model) Init() tea.Cmd {
	cmds := []tea.Cmd{
		tickAgentStatus(m.agentMgr.StatusPollInterval()),
		m.spinner.Tick,
		m.maybeSetWindowTitle(),
		checkBinaryStaleness(),
	}
	// Arm the daemon Subscribe handshake asynchronously (bounded ctx, off
	// the Update goroutine) so a wedged daemon can't block startup. The
	// Ready handler installs the channel and arms readNextDaemonEvent.
	if m.daemonClient != nil {
		cmds = append(cmds, subscribeDaemonEventsCmd(m.daemonClient))
	}
	// Arm the periodic daemon-state resync. NewModel's synchronous
	// startup reconcile populated m.panes / m.daemonOwned from the
	// initial List; this tick keeps that view in sync as the daemon's
	// session set drifts (sibling TUI spawns, daemon restart, external
	// kills). Returns nil when m.daemon is missing — batch absorbs it.
	if cmd := m.scheduleDaemonResync(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	// Arm the periodic board-state resync. Reconciles in-memory
	// tickets + projects against the on-disk source of truth so
	// changes made by a sibling TUI (or any process editing ticket
	// .md files) surface even when the fsnotify watcher missed the
	// event or never subscribed to the affected project. See
	// internal/ui/board_resync.go.
	cmds = append(cmds, m.scheduleBoardResync())
	return tea.Batch(cmds...)
}

// computeWindowTitle returns the title we want the host terminal to
// display. Reflects the focused pane's OSC-set title when in agent
// view, falling back to the ticket title, with an "openkanban: "
// prefix. Returns "openkanban" outside of agent view.
func (m *Model) computeWindowTitle() string {
	const prefix = "openkanban"
	if m.mode != ModeAgentView || m.focusedPane == "" {
		return prefix
	}
	pane, ok := m.panes[m.focusedPane]
	var sub string
	if ok {
		sub = pane.Title()
	}
	if sub == "" {
		if ticket, _ := m.globalStore.Get(m.focusedPane); ticket != nil {
			sub = ticket.Title
		}
	}
	if sub == "" {
		return prefix
	}
	return prefix + ": " + sub
}

// maybeSetWindowTitle returns a tea.Cmd to update the host terminal's
// window title if the desired title has changed. Returns nil if no
// change.
func (m *Model) maybeSetWindowTitle() tea.Cmd {
	want := m.computeWindowTitle()
	if want == m.lastWindowTitle {
		return nil
	}
	m.lastWindowTitle = want
	return tea.SetWindowTitle(want)
}

// Update is the BubbleTea entry point. The actual case-dispatch lives
// in dispatchUpdate; this wrapper reconciles the daemon's viewing
// state (a single "what session is this TUI focused on right now"
// signal) against whatever mode/focusedPane mutation the inner
// dispatch may have performed. Centralizing the reconcile here means
// no individual case branch has to remember to fire SetViewing —
// the diff between viewingSessionID and the post-update truth covers
// every transition path.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	m.monitor.enterUpdate(reflect.TypeOf(msg).String(), string(m.mode), len(m.panes), len(m.daemonOwned))
	defer m.monitor.exitUpdate()
	maybeDebugStall()
	next, cmd := m.dispatchUpdate(msg)
	nm, ok := next.(*Model)
	if !ok {
		return next, cmd
	}
	if viewingCmd := nm.reconcileViewing(); viewingCmd != nil {
		return nm, tea.Batch(cmd, viewingCmd)
	}
	return nm, cmd
}

// reconcileViewing computes the session this TUI is currently focused
// on (ModeAgentView + a pane for the focused ticket), compares it to
// the last value we told the daemon, and returns the fire-and-forget
// tea.Cmd that calls SetViewing(prev,false) and/or SetViewing(new,
// true) for the diff. Returns nil when nothing changed.
//
// Two calls when both prev and new are non-empty (a focus change
// between sessions); one call on enter-from-board and one on
// leave-to-board; zero work in the steady state.
func (m *Model) reconcileViewing() tea.Cmd {
	target := ""
	if m.mode == ModeAgentView && m.focusedPane != "" {
		if pv, ok := m.panes[m.focusedPane]; ok && pv != nil {
			target = pv.SessionID()
		}
	}
	if target == m.viewingSessionID {
		return nil
	}
	prev := m.viewingSessionID
	m.viewingSessionID = target
	var cmds []tea.Cmd
	if prev != "" {
		cmds = append(cmds, m.setViewingCmd(prev, false))
	}
	if target != "" {
		cmds = append(cmds, m.setViewingCmd(target, true))
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

// setViewingCmd returns a fire-and-forget tea.Cmd that calls
// daemonClient.SetViewing. The RPC is idempotent on the daemon side
// (duplicate true / duplicate false is a silent no-op), so callers
// don't need to gate. Returns nil when there's no daemon client or
// the sessionID is empty.
func (m *Model) setViewingCmd(sessionID string, viewing bool) tea.Cmd {
	if m.daemonClient == nil || sessionID == "" {
		return nil
	}
	client := m.daemonClient
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		if _, err := client.SetViewing(ctx, sessionID, viewing); err != nil {
			log.Printf("openkanban: SetViewing(%s, %v) failed: %v", sessionID, viewing, err)
		}
		return nil
	}
}

func (m *Model) dispatchUpdate(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Daemon push events must be handled regardless of mode so the
	// readNextDaemonEvent listener is always re-armed. If we let these
	// fall through to a mode-specific switch (ModeSpawning, ModeShuttingDown)
	// that doesn't list them as a case, the msg is silently dropped and
	// handleDaemonSessionEvent never fires — which means the re-arm cmd
	// is never returned and every subsequent push event piles up in the
	// subscriber channel buffer with no reader.
	switch msg := msg.(type) {
	case daemonSessionEventsMsg:
		return m.handleDaemonSessionEvents(msg)
	case daemonSubscribeReadyMsg:
		return m.handleDaemonSubscribeReady(msg)
	case daemonSubscribeFailedMsg:
		return m.handleDaemonSubscribeFailed(msg)
	case daemonSubscribeEndedMsg:
		return m.handleDaemonSubscribeEnded(msg)
	case daemonResyncTickMsg:
		return m.handleDaemonResyncTick()
	case daemonResyncMsg:
		return m.handleDaemonResyncMsg(msg)
	case boardResyncTickMsg:
		return m.handleBoardResyncTick()
	case boardResyncMsg:
		return m.handleBoardResyncMsg(msg)
	}

	if m.mode == ModeShuttingDown {
		switch msg := msg.(type) {
		case shutdownCompleteMsg:
			return m, tea.Quit
		case spinner.TickMsg:
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			return m, cmd
		}
		return m, nil
	}

	if m.mode == ModeSpawning {
		switch msg := msg.(type) {
		case agentStatusMsg:
			return m, tea.Batch(
				m.pollAgentStatusesAsync(),
				tickAgentStatus(m.agentMgr.StatusPollInterval()),
			)
		case spawnReadyMsg:
			if msg.ticketID != m.spawningTicketID {
				return m, nil
			}

			ticket, _ := m.globalStore.Get(msg.ticketID)
			if ticket != nil {
				ticket.AgentType = m.spawningAgent
				// Don't clobber AgentStatus here. The daemon's "started"
				// SessionEvent (which handleDaemonSessionEvent sets to
				// AgentWorking) can race with spawnReadyMsg and arrive
				// first; a blind reset here would replace the correct
				// "working" with AgentNone and leave the card blank.
				// The daemon push is authoritative for AgentStatus.
				if ticket.AgentSpawnedAt == nil {
					now := time.Now()
					ticket.AgentSpawnedAt = &now
				}
				if msg.worktreePath != "" && ticket.WorktreePath == "" {
					ticket.WorktreePath = msg.worktreePath
					ticket.BranchName = msg.branchName
					ticket.BaseBranch = msg.baseBranch
				}
				m.saveTicket(ticket)
			}

			m.panes[msg.ticketID] = msg.pane
			// Spawn just succeeded — there genuinely IS a daemon-side
			// session for this ticket, so register it in daemonOwned
			// immediately rather than waiting for the next periodic
			// resync (~30s). Without this, the 'w' session filter and
			// the 'W' "always show working" bypass miss freshly-spawned
			// sessions, which is the most user-visible window since
			// users tend to interact with sessions right after spawn.
			m.daemonOwned[msg.ticketID] = struct{}{}
			m.focusedPane = msg.ticketID
			if msg.notice != "" {
				m.notify(msg.notice)
			}
			// Switch to attached view and start listening for pane msgs.
			// The pane is already attached (Spawn returned + Attach
			// happened on the goroutine), so we just need to drain its
			// tea channel.
			m.mode = ModeAgentView
			m.spawningTicketID = ""
			m.spawningAgent = ""
			return m, tea.Batch(
				m.listenPaneMessages(msg.pane),
				m.maybeSetWindowTitle(),
			)

		case spawnErrorMsg:
			// Mid-spawn error path: clear the ModeSpawning bookkeeping
			// and toast. Kept inside this block (rather than relying on
			// the top-level handler) because the ModeSpawning switch
			// ends with `return m, nil` for unhandled cases, which would
			// otherwise swallow the message before it ever reached the
			// top-level handler below.
			if msg.ticketID == m.spawningTicketID {
				m.mode = ModeNormal
				m.spawningTicketID = ""
				m.spawningAgent = ""
				m.notify(msg.err)
			}
			return m, nil

		case attachConflictMsg:
			// Owns cold-start fast path probed a session that's attached in
			// another TUI. Leave the spawning splash and raise the takeover
			// warning instead of silently retrying then failing.
			if msg.ticketID == m.spawningTicketID {
				m.spawningTicketID = ""
				m.spawningAgent = ""
			}
			m.armTakeoverPrompt(msg)
			return m, m.maybeSetWindowTitle()

		case daemonclient.PaneOutputMsg:
			// Pane started producing output while we're still showing
			// the "Spawning…" splash (rare — Spawn usually returns
			// spawnReadyMsg before any output arrives). Promote to
			// attached view so the user actually sees it.
			if board.TicketID(msg.PaneID) == m.spawningTicketID {
				m.mode = ModeAgentView
				m.spawningTicketID = ""
				m.spawningAgent = ""
			}
			return m.handleTerminalMsg(msg)

		case daemonclient.PaneExitMsg:
			if board.TicketID(msg.PaneID) == m.spawningTicketID {
				m.resetSpawnState(board.TicketID(msg.PaneID))
				if msg.Err != nil {
					m.notify("Agent failed: " + msg.Err.Error())
				} else {
					m.notify("Agent exited unexpectedly")
				}
			}
			return m, nil

		case spinner.TickMsg:
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			return m, cmd

		case tea.KeyMsg:
			if msg.String() == "esc" {
				if pane, ok := m.panes[m.spawningTicketID]; ok {
					pane.Stop()
					delete(m.panes, m.spawningTicketID)
				}
				m.mode = ModeNormal
				m.spawningTicketID = ""
				m.spawningAgent = ""
				m.notify("Spawn cancelled")
				return m, nil
			}
		}
		return m, nil
	}

	// Bubbletea v1 silently drops CSI sequences it doesn't have in its
	// hardcoded table — notably xterm modifyOtherKeys
	// (\x1b[27;<mod>;<key>~), which Ghostty emits for shift+enter,
	// ctrl+enter, etc. When the user is attached to a pane, forward
	// the raw bytes so the inner agent (Claude Code, etc.) can
	// interpret the sequence natively.
	if m.mode == ModeAgentView {
		if raw := daemonclient.ExtractRawCSIBytes(msg); raw != nil {
			if pane, ok := m.panes[m.focusedPane]; ok {
				pane.WriteRaw(raw)
			}
			return m, m.maybeSetWindowTitle()
		}
	}

	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.handleKey(msg)

	case spawnUnattachedReadyMsg:
		// ctrl+space spawn completed without attaching. Register the
		// Unattached pane and stamp the ticket, mirroring the persist/
		// bookkeeping half of the attached spawnReadyMsg handler — but
		// WITHOUT the ModeAgentView switch, focusedPane, or pane-message
		// listener. The TUI stays on the board; the user attaches later
		// via s/Enter (which takes the PaneViewUnattached arm of spawnAgent).
		ticket, _ := m.globalStore.Get(msg.ticketID)
		if ticket != nil {
			if ticket.AgentSpawnedAt == nil {
				now := time.Now()
				ticket.AgentSpawnedAt = &now
			}
			if msg.worktreePath != "" && ticket.WorktreePath == "" {
				ticket.WorktreePath = msg.worktreePath
				ticket.BranchName = msg.branchName
				ticket.BaseBranch = msg.baseBranch
			}
			m.saveTicket(ticket)
		}
		m.panes[msg.ticketID] = msg.pane
		m.daemonOwned[msg.ticketID] = struct{}{}
		m.notify("Agent launched in background")
		return m, nil

	case spawnErrorMsg:
		// Surface attach/spawn errors arriving outside ModeSpawning. The
		// ModeSpawning case is handled in the block above (since that
		// block's trailing `return m, nil` would otherwise swallow this
		// message before it reached here). What's left to handle is
		// every OTHER caller of attachExisting — the Unattached and
		// Detached arms of spawnAgent (B6) flip to ModeAgentView before
		// kicking off the attach, and AttachFirstMsg / cycleAttachPrompt
		// never enter ModeSpawning at all. Before the fix those attach
		// failures had no handler at any level and dropped silently,
		// parking the user with a dead pane.
		//
		// Choice rationale (option a from review): dropping the
		// ticketID gate is safer than coupling the attach-from-attach
		// path to the spawn state machine via m.spawningTicketID —
		// that coupling would be load-bearing-but-misleading. Spawn
		// errors are always worth surfacing; if a future flow returns
		// spawnErrorMsg and does NOT want a toast, it can introduce a
		// separate message type.
		m.notify(msg.err)
		return m, nil

	case attachConflictMsg:
		// A plain attach probe was rejected — the session is attached in
		// another TUI instance. Warn before taking over (P1 paths, which
		// are already in ModeAgentView).
		m.armTakeoverPrompt(msg)
		return m, m.maybeSetWindowTitle()

	case cyclePeekedMsg:
		// A cycle Peek finished; processing this message is enough to
		// re-render the backdrop. Nothing else to do.
		return m, nil

	case QuitRequestedMsg:
		return m.handleQuitRequested()

	case StallRecoverMsg:
		return m.handleStallRecover()

	case prepareExitResultMsg:
		return m.handlePrepareExitResult(msg)

	case prepareExitFailedMsg:
		return m.handlePrepareExitFailed(msg)

	case sessionKilledMsg:
		return m.handleSessionKilled(msg)

	case sessionKillFailedMsg:
		return m.handleSessionKillFailed(msg)

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		// A resize that shrinks the terminal can strand m.columnOffsets[i]
		// at a value the new (smaller) budget can no longer justify — same
		// underlying bug as the filter path that compactColumnOffsets was
		// added for. Safe to call before the first ticket load: the method
		// no-ops when m.columnTickets is empty and when the budget is ≤ 0.
		m.compactColumnOffsets()
		if m.focusedPane != "" {
			if pane, ok := m.panes[m.focusedPane]; ok {
				pane.SetSize(m.width, m.height-2)
			}
		}
		return m, nil

	case tea.MouseMsg:
		if m.mode == ModeNormal {
			return m.handleMouse(msg)
		}
		if m.mode == ModeAgentView {
			return m.handleAgentViewMouse(msg)
		}
		if m.mode == ModeCreateTicket || m.mode == ModeEditTicket {
			return m.handleTicketFormMouse(msg)
		}
		if m.mode == ModeFilter {
			return m.handleFilterMouse(msg)
		}
		if m.mode == ModeSettings {
			return m.handleSettingsMouse(msg)
		}
		if m.showHelp {
			if msg.Action == tea.MouseActionPress {
				m.showHelp = false
			}
			return m, nil
		}
		if m.showConfirm {
			return m.handleConfirmMouse(msg)
		}
		return m, nil

	case daemonclient.PaneOutputMsg, daemonclient.PaneRenderTickMsg, daemonclient.PaneAttachedMsg:
		return m.handleTerminalMsg(msg)

	case daemonclient.PaneDetachedMsg:
		// Detach is the signal we get when the binary attach conn
		// closes — either the agent exited (claude /q), the daemon
		// killed the session, or another TUI took over. In all three
		// cases, the local PaneView's vt has been torn down and View()
		// returns "" (blank pane). Surface that by returning the user
		// to the board if they're currently focused on this pane.
		// They can re-enter via Enter if the session is still alive in
		// the daemon (e.g. takeover case).
		ticketID := board.TicketID(msg.PaneID)
		if m.focusedPane == ticketID {
			m.exitToBoard()
			m.selectTicketByID(ticketID)
		}
		return m.handleTerminalMsg(msg)

	case daemonclient.PaneExitMsg:
		ticketID := board.TicketID(msg.PaneID)
		// The PaneExitMsg path fires when the local PaneView's binary
		// reader saw the conn close (e.g. detach + remote pane death).
		// The authoritative "was this exit expected?" signal arrives
		// separately as daemonSessionEventMsg{Event:"exited", Expected:...}
		// and is handled in handleDaemonSessionEvent, which preserves
		// AgentCompleted when appropriate. From this path we cannot
		// know intent, so we conservatively reset to AgentNone here;
		// when the daemon event lands (which it will, before or after
		// this msg) it will overwrite to AgentCompleted if Expected=true.
		if pv, ok := m.panes[ticketID]; ok {
			if pv != nil {
				_ = pv.Close()
			}
			delete(m.panes, ticketID)
		}
		if ticket, _ := m.globalStore.Get(ticketID); ticket != nil {
			// Only reset if not already AgentCompleted — the daemon
			// session-event may have raced ahead and set Completed,
			// in which case we must not clobber it.
			if ticket.AgentStatus != board.AgentCompleted {
				if ticket.SetAgentStatus(board.AgentNone) {
					m.saveTicket(ticket)
				}
			}
		}
		if m.focusedPane == ticketID {
			m.exitToBoard()
			m.notify("Agent exited")
			m.selectTicketByID(ticketID)
		}
		return m, m.maybeSetWindowTitle()

	case daemonclient.AttachFirstMsg:
		// HandleKey returned this because the user typed into an
		// unattached pane. Attach (or takeover, if another client is
		// already attached) and resume listening.
		ticketID := board.TicketID(msg.PaneID)
		if pv, ok := m.panes[ticketID]; ok {
			cmd := m.attachExisting(ticketID, pv)
			return m, cmd
		}
		return m, nil

	case daemonclient.DaemonDisconnectedMsg:
		// Daemon vanished mid-session. Detach every PaneView; the model
		// keeps running but with no live attaches. A reconnect is driven
		// below (and, as the always-on fallback, by the resync tick) so
		// the control conn is re-established when the daemon comes back.
		for id, pv := range m.panes {
			_ = pv.Close()
			delete(m.panes, id)
		}
		if m.focusedPane != "" {
			m.exitToBoard()
		}
		// Daemon push channel is gone; the file-poll takes over as the
		// AgentStatus source. Clear the subscribe handles so we don't
		// keep dangling references, and drop daemonOwned so the poll
		// can reassert authority on the next tick.
		m.daemonConnected.Store(false)
		if m.daemonUnsub != nil {
			m.daemonUnsub()
			m.daemonUnsub = nil
		}
		m.daemonEvents = nil
		for id := range m.daemonOwned {
			delete(m.daemonOwned, id)
		}
		for id := range m.daemonViewing {
			delete(m.daemonViewing, id)
		}
		m.viewingSessionID = ""
		if msg.Err != nil {
			m.notify("Daemon disconnected: " + msg.Err.Error())
		} else {
			m.notify("Daemon disconnected")
		}
		// Faster-path reconnect for the attached-pane case (the resync
		// tick is the no-pane fallback). Gated on Closed() so a transient
		// attach-stream tear-down that left the control conn live does
		// not replace a healthy client; if the control conn isn't dead
		// yet, the resync tick picks it up once it is.
		cmds := []tea.Cmd{m.maybeSetWindowTitle()}
		if m.daemonClient != nil && m.daemonClient.Closed() {
			if rc := m.maybeReconnectDaemon(); rc != nil {
				cmds = append(cmds, rc)
			}
		}
		return m, tea.Batch(cmds...)

	case daemonReconnectedMsg:
		return m.handleDaemonReconnectedMsg(msg)

	case terminal.ExitFocusMsg:
		m.exitToBoard()
		return m, m.maybeSetWindowTitle()

	case agentStatusMsg:
		return m, tea.Batch(
			m.pollAgentStatusesAsync(),
			tickAgentStatus(m.agentMgr.StatusPollInterval()),
		)

	case agentStatusResultMsg:
		// The file-poll value is the authoritative source for the
		// intra-session transitions Claude Code's hooks emit
		// (working / idle / waiting / error), regardless of whether
		// the ticket is daemon-owned. Two narrow guards:
		//
		//   - A poll value of AgentNone means the file is absent and
		//     the terminal scrape produced no hit. That's "I don't
		//     know," not a transition — preserve whatever was set
		//     (typically AgentWorking from the daemon's "started"
		//     SessionEvent, or AgentCompleted from ticket-done).
		//
		//   - AgentCompleted is terminal. Only another terminal value
		//     (Completed, Error) may overwrite it. This mirrors the
		//     symmetric guard in cmd/status.go that prevents Claude's
		//     Stop hook racing TicketDone during the SIGTERM grace
		//     window from downgrading the completion signal to idle.
		//
		// The pre-fix rule unconditionally skipped poll values for
		// daemon-owned tickets, which froze AgentStatus at the daemon's
		// "started" event (AgentWorking) for the entire session — hooks
		// updated the file but the TUI never reflected it.
		for ticketID, status := range msg {
			if status == board.AgentNone {
				continue
			}
			ticket, _ := m.globalStore.Get(ticketID)
			if ticket == nil {
				continue
			}
			if ticket.AgentStatus == board.AgentCompleted &&
				status != board.AgentCompleted &&
				status != board.AgentError {
				continue
			}
			// AgentSubagents is a grid-only verdict: it can only be derived
			// from the live PTY grid (the daemon's), never from the hook
			// status file. This poll's local grid is empty for an unattached
			// session, so backgroundWaitVisible is false and the detector
			// falls through to the stale file value ("waiting" from the
			// delegating turn's approved prompt, or "working") — which would
			// fight the daemon's authoritative AgentSubagents push every ~2s
			// (the flap). While the ticket is AgentSubagents, the poll may
			// apply only the FRESH hook-written quiescent/terminal signals it
			// legitimately owns (idle/completed/error, which the
			// activity-gated daemon goes silent on); the daemon-push
			// (applyDaemonStatus), which has the grid, owns the live
			// working/waiting/subagents transitions.
			if ticket.AgentStatus == board.AgentSubagents &&
				(status == board.AgentWorking || status == board.AgentWaiting) {
				continue
			}
			// In-memory only — this poll loop refreshes AgentStatus for
			// visibility and intentionally does not persist (see comment
			// block above). SetAgentStatus is used so StatusChangedAt is
			// stamped alongside; it will land on disk on the next save
			// from any other path.
			ticket.SetAgentStatus(status)

			// T2 of the integration plan removed the edge-triggered
			// auto-stop on AgentCompleted: ticket-done now flows
			// CLI → daemon (TicketDoneReq) → SessionEvent broadcast,
			// and the daemon's authoritative Expected=true signal lands
			// via handleDaemonSessionEvent. The poll's job is reduced
			// to refreshing AgentStatus for visibility — it no longer
			// kills panes.
		}

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case notificationMsg:
		if time.Since(m.notifyTime) > 3*time.Second {
			m.notification = ""
		}
		return m, nil

	case binaryStaleCheckMsg:
		// Periodic self-staleness check. The binary may have been
		// replaced under us by `go install` / `openkanban update`
		// running in another shell; long-lived TUI sessions otherwise
		// have no signal that an upgrade has landed. We surface the
		// notification once per stale-transition (not every 30s) and
		// re-arm the tick unconditionally. See update.BinaryStale.
		if update.BinaryStale() {
			if !m.binaryStaleNotified {
				m.notify("openkanban binary updated on disk — press Ctrl-R to restart, or 'q' to quit and relaunch")
				m.binaryStaleNotified = true
			}
		} else {
			m.binaryStaleNotified = false
		}
		return m, checkBinaryStaleness()

	case FsChangedMsg:
		m.handleFsChanged(msg)
		return m, nil

	case editorFinishedMsg:
		m.applyEditorResult(msg)
		return m, nil
	}

	return m, nil
}

func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// ModeConfirmExit owns the keyboard entirely while the exit-guard
	// modal is up — including q / Ctrl-C / Esc / ?. Route to its
	// dedicated handler before the global key map runs, so the modal's
	// own bindings (x kill, X kill-all, Esc cancel) take precedence.
	if m.mode == ModeConfirmExit {
		return m.handleConfirmExitMode(msg)
	}

	switch msg.String() {
	case "ctrl+c", "q":
		if m.mode == ModeNormal {
			return m.handleQuit()
		}
	case "esc":
		// A choice modal (e.g. the brief-change chooser) is shown via
		// m.showChoice while m.mode stays ModeNormal. Route Esc to its
		// handler before the ModeNormal reset arm below — that arm
		// clears mode/help/confirm but not showChoice, so it would
		// otherwise swallow Esc and leave the modal stuck open.
		if m.showChoice {
			return m.handleChoice(msg)
		}
		// Same overlay-routing rule for the stuck-action modal (also a
		// bool flag with m.mode==ModeNormal). Route Esc to it before the
		// reset arm so Esc dismisses it instead of being swallowed.
		if m.stuckActionPrompt {
			return m.handleStuckActionKey(msg)
		}
		if m.mode == ModeAgentView {
			break
		}
		if m.mode == ModeNormal && (m.filterQuery != "" || len(m.filterProjectIDs) > 0) {
			m.clearFilter()
			m.notify("Filter cleared")
			return m, nil
		}
		m.mode = ModeNormal
		m.showHelp = false
		m.showConfirm = false
		m.titleInput.Blur()
		return m, nil
	case "?":
		if m.mode == ModeNormal || m.mode == ModeHelp {
			m.showHelp = !m.showHelp
			return m, nil
		}
	}

	if m.showHelp {
		m.showHelp = false
		return m, nil
	}

	if m.showChoice {
		return m.handleChoice(msg)
	}

	// Stuck-action modal owns every non-global key while open (PR #70
	// routing: the global arms above — esc/q/ctrl+c/? — already ran).
	if m.stuckActionPrompt {
		return m.handleStuckActionKey(msg)
	}

	if m.showConfirm {
		return m.handleConfirm(msg)
	}

	switch m.mode {
	case ModeNormal:
		return m.handleNormalMode(msg)
	case ModeCreateTicket:
		return m.handleCreateTicketMode(msg)
	case ModeEditTicket:
		return m.handleEditTicketMode(msg)
	case ModeEditProject:
		return m.handleProjectEditForm(msg)
	case ModeAgentView:
		return m.handleAgentViewMode(msg)
	case ModeSettings:
		return m.handleSettingsMode(msg)
	case ModeFilter:
		return m.handleFilterMode(msg)
	case ModeCreateProject:
		return m.handleCreateProjectMode(msg)
	}

	return m, nil
}

func (m *Model) handleNormalMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "tab":
		if m.sidebarVisible {
			m.sidebarFocused = !m.sidebarFocused
			return m, nil
		}
	case "[":
		m.sidebarVisible = !m.sidebarVisible
		if !m.sidebarVisible {
			m.sidebarFocused = false
		}
		return m, nil
	}

	if m.sidebarFocused {
		return m.handleSidebarNav(msg)
	}

	switch msg.String() {
	case "ctrl+r":
		// Stale-binary restart shortcut. We only honor this when the
		// on-disk binary has actually been replaced — otherwise an
		// errant Ctrl-R would silently kill the TUI. The exit path
		// reuses the existing quit-with-guard flow so live agent
		// sessions are not orphaned; the user re-launches with
		// `openkanban` after the guard clears.
		if update.BinaryStale() {
			m.notify("Restarting to pick up new binary — re-launch with `openkanban`")
			return m.handleQuit()
		}
		return m, nil
	case "h", "left":
		if m.activeColumn == 0 && m.sidebarVisible {
			m.sidebarFocused = true
			return m, nil
		}
		m.moveColumn(-1)
	case "l", "right":
		m.moveColumn(1)
	case "j", "down":
		m.moveTicket(1)
	case "k", "up":
		m.moveTicket(-1)
	case "g":
		m.activeTicket = 0
		m.ensureTicketVisible()
	case "G":
		if len(m.columnTickets) > m.activeColumn {
			m.activeTicket = max(len(m.columnTickets[m.activeColumn])-1, 0)
		}
		m.ensureTicketVisible()

	case "n":
		return m.createNewTicket()
	case "e":
		return m.editTicket()
	case "E":
		return m.editTicketBodyInEditor()
	case "enter", "s":
		// Single entry point: spawnAgent dispatches to spawn or
		// re-attach based on the current pane state. Pre-consolidation,
		// Enter only attached and 's' only spawned, which produced
		// cross-key bounces ("press Enter to attach" / "press 's' to
		// spawn") for the user.
		return m.spawnAgent()
	case "d":
		return m.confirmDeleteTicket()
	case " ":
		return m.quickMoveTicket()
	case "ctrl+@":
		// ctrl+space (bubbletea reports ctrl+space as "ctrl+@"): promote
		// the ticket to in_progress and launch its agent unattached.
		return m.promoteAndSpawnUnattached()
	case "-", "backspace":
		return m.quickMoveTicketBackward()
	case "S":
		return m.stopAgent()
	case "r":
		// Open the recover/destroy modal for a stuck session. No-op
		// unless the selected card is AgentStuck.
		return m.openStuckActionModal()

	case "K":
		return m.adjustPriority(-1)
	case "J":
		return m.adjustPriority(1)
	case "o":
		return m.cycleSortMode()
	case "w":
		return m.cycleSessionFilter()
	case "W":
		return m.toggleAlwaysShowWorking()
	case "a":
		return m.toggleAutoAttach()

	case "/":
		m.filterInput.SetValue(m.filterQuery)
		m.filterInput.Focus()
		m.mode = ModeFilter

	case "O":
		m.mode = ModeSettings
		m.settingsIndex = 0
		m.settingsEditing = false
	}

	return m, nil
}

func (m *Model) openAddProjectForm() (tea.Model, tea.Cmd) {
	m.addProjectPath.SetValue("")
	m.addProjectPath.Focus()
	m.mode = ModeCreateProject
	m.notification = ""
	return m, textinput.Blink
}

func (m *Model) sidebarAllY() int          { return 2 }
func (m *Model) sidebarProjectStartY() int { return 4 }
func (m *Model) sidebarAddProjectY(projectCount int) int {
	return m.sidebarProjectStartY() + projectCount + 1
}

func (m *Model) handleSidebarMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	y := msg.Y - m.headerHeight()

	if y < 0 {
		return m, nil
	}

	projects := m.globalStore.Projects()

	if y == m.sidebarAllY() {
		m.sidebarIndex = 0
		m.toggleAllProjects()
		return m, nil
	}

	for i := range projects {
		if y == m.sidebarProjectStartY()+i {
			m.sidebarIndex = i + 1
			m.toggleProjectFilter(projects[i].ID)
			return m, nil
		}
	}

	if y == m.sidebarAddProjectY(len(projects)) {
		return m.openAddProjectForm()
	}

	m.sidebarFocused = true
	return m, nil
}

func (m *Model) handleSidebarNav(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	projects := m.globalStore.Projects()
	addIndex := len(projects) + 1

	switch msg.String() {
	case "j", "down":
		if m.sidebarIndex < addIndex {
			m.sidebarIndex++
		}
	case "k", "up":
		if m.sidebarIndex > 0 {
			m.sidebarIndex--
		}
	case "enter", " ":
		if m.sidebarIndex == 0 {
			m.toggleAllProjects()
		} else if m.sidebarIndex == addIndex {
			return m.openAddProjectForm()
		} else {
			idx := m.sidebarIndex - 1
			if idx < len(projects) {
				m.toggleProjectFilter(projects[idx].ID)
			}
		}
	case "l", "right":
		m.sidebarFocused = false
		return m, nil
	case "a":
		return m.openAddProjectForm()
	case "d":
		if m.sidebarIndex > 0 && m.sidebarIndex <= len(projects) {
			m.confirmDeleteProject(projects[m.sidebarIndex-1])
		}
		return m, nil
	case "o":
		m.sidebarOpenOnly = !m.sidebarOpenOnly
	case "g":
		if m.sidebarIndex > 0 && m.sidebarIndex <= len(projects) {
			m.cycleProjectAgent(projects[m.sidebarIndex-1])
		}
		return m, nil
	case "e":
		if m.sidebarIndex > 0 && m.sidebarIndex <= len(projects) {
			return m.editProject(projects[m.sidebarIndex-1])
		}
		return m, nil
	case "esc":
		m.sidebarFocused = false
	}

	return m, nil
}

// cycleProjectAgent advances a project's pinned agent to the next configured
// agent (by config key) and persists it to the registry. The per-project pin is
// the ONLY place agent identity is chosen, and it governs every spawn in the
// project (an unpinned project refuses to spawn — see resolveSpawnAgent). The
// status-bar toast names the agent's Label so the binding is visible.
func (m *Model) cycleProjectAgent(proj *project.Project) {
	if proj == nil {
		return
	}
	names := m.enabledAgentNames()
	if len(names) == 0 {
		return
	}
	next := names[0]
	for i, n := range names {
		if n == proj.Settings.DefaultAgent {
			next = names[(i+1)%len(names)]
			break
		}
	}
	proj.Settings.DefaultAgent = next
	if err := m.projectRegistry.Update(proj); err != nil {
		m.notify("Failed to save project agent: " + err.Error())
		return
	}
	m.notify("Project agent: " + m.agentLabel(next))
}

// agentLabel returns the human-facing label for an agent key, falling back to
// the key itself when the config has no Label.
func (m *Model) agentLabel(name string) string {
	if m.config != nil {
		if cfg, ok := m.config.Agents[name]; ok && cfg.Label != "" {
			return cfg.Label
		}
	}
	return name
}

// sidebarTicketCount counts tickets for the sidebar. projectID=="" counts
// across all projects. When sidebarOpenOnly is set, terminal-status tickets
// (done, archived) are excluded so the count reflects open work only.
func (m *Model) sidebarTicketCount(projectID string) int {
	count := 0
	for _, t := range m.globalStore.All() {
		if projectID != "" && t.ProjectID != projectID {
			continue
		}
		if m.sidebarOpenOnly && (t.Status == board.StatusDone || t.Status == board.StatusArchived) {
			continue
		}
		count++
	}
	return count
}

func (m *Model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	switch msg.Action {
	case tea.MouseActionPress:
		if msg.Button == tea.MouseButtonLeft {
			if m.hitTestHeader(msg.X, msg.Y) {
				return m, nil
			}
			if m.sidebarVisible && msg.X < m.sidebarWidth {
				return m.handleSidebarMouse(msg)
			}
			col, ticket := m.hitTest(msg.X, msg.Y)
			if col >= 0 {
				m.sidebarFocused = false
				m.activeColumn = col
				if ticket >= 0 {
					now := time.Now()
					isDoubleClick := ticket == m.lastClickTicket &&
						col == m.lastClickColumn &&
						now.Sub(m.lastClickTime) < 400*time.Millisecond

					if isDoubleClick {
						m.lastClickTime = time.Time{}
						m.lastClickColumn = -1
						m.lastClickTicket = -1
						return m.handleDoubleClick()
					}

					m.lastClickTime = now
					m.lastClickColumn = col
					m.lastClickTicket = ticket

					m.activeTicket = ticket
					m.dragging = true
					m.dragSourceColumn = col
					m.dragSourceTicket = ticket
					m.dragTargetColumn = col
				}
				m.ensureColumnVisible()
			}
		}

	case tea.MouseActionMotion:
		if m.dragging && msg.Button == tea.MouseButtonLeft {
			col, _ := m.hitTest(msg.X, msg.Y)
			if col >= 0 {
				m.dragTargetColumn = col
			}
		} else {
			if m.sidebarVisible && msg.X < m.sidebarWidth {
				m.hoverColumn = -1
				m.hoverTicket = -1
			} else {
				col, ticket := m.hitTest(msg.X, msg.Y)
				m.hoverColumn = col
				m.hoverTicket = ticket
			}
		}

	case tea.MouseActionRelease:
		if m.dragging {
			if m.dragTargetColumn != m.dragSourceColumn && m.dragTargetColumn >= 0 {
				return m.dropTicket()
			}
			m.dragging = false
			m.dragTargetColumn = 0
		}
		col, ticket := m.hitTest(msg.X, msg.Y)
		m.hoverColumn = col
		m.hoverTicket = ticket

	default:
		switch msg.Button {
		case tea.MouseButtonWheelUp:
			m.moveTicket(-1)
		case tea.MouseButtonWheelDown:
			m.moveTicket(1)
		}
	}

	return m, nil
}

func (m *Model) hitTestHeader(x, y int) bool {
	if y > 2 {
		return false
	}

	if m.filterQuery != "" || len(m.filterProjectIDs) > 0 {
		clearStart := 20 + len(m.filterQuery) + 15
		if x >= clearStart && x <= clearStart+10 {
			m.clearFilter()
			return true
		}
	}

	if x >= 15 && x <= 30 {
		m.filterInput.SetValue(m.filterQuery)
		m.filterInput.Focus()
		m.mode = ModeFilter
		return true
	}

	return false
}

func (m *Model) hitTest(x, y int) (column, ticket int) {
	if m.width == 0 || len(m.columns) == 0 {
		return -1, -1
	}

	if m.sidebarVisible {
		x = x - m.sidebarWidth - 1
	}

	headerHeight := 2
	if y < headerHeight {
		return -1, -1
	}

	columnWidth := m.calcColumnWidth()
	visibleCols := m.visibleColumnCount(columnWidth)
	numVisible := visibleCols
	if m.scrollOffset+visibleCols > len(m.columns) {
		numVisible = len(m.columns) - m.scrollOffset
	}

	baseWidth, remainder := m.distributeWidth(numVisible)

	hasLeftIndicator := m.scrollOffset > 0
	startX := 0
	if hasLeftIndicator {
		startX = 2
	}

	for i := 0; i < numVisible; i++ {
		colWidth := baseWidth + 3
		if i < remainder {
			colWidth++
		}

		if x >= startX && x < startX+colWidth {
			actualCol := m.scrollOffset + i
			ticketIdx := m.hitTestTicket(y-headerHeight, actualCol)
			return actualCol, ticketIdx
		}
		startX += colWidth
	}

	return -1, -1
}

func (m *Model) hitTestTicket(relativeY, column int) int {
	if column < 0 || column >= len(m.columnTickets) {
		return -1
	}

	tickets := m.columnTickets[column]
	if len(tickets) == 0 {
		return -1
	}

	ticketY := relativeY - columnHeaderHeight
	if ticketY < 0 {
		return -1
	}

	offset := 0
	if column < len(m.columnOffsets) {
		offset = m.columnOffsets[column]
	}

	// Walk per-ticket heights from `offset`, accumulating until cumulative
	// height crosses ticketY. With a "▲ N more" indicator above (when
	// offset > 0) the first card sits one row down, so consume that row
	// from ticketY before walking.
	//
	// Example (offset=0, heights = [8, 9, 8], ticketY=12):
	//   cum=0  → 8  : 12 not in [0,8)   → index 1 candidate
	//   cum=8  → 17 : 12 in [8,17)      → return 1
	if offset > 0 {
		ticketY-- // ▲ N more row
		if ticketY < 0 {
			return -1
		}
	}

	var heights []int
	if column < len(m.columnTicketHeights) {
		heights = m.columnTicketHeights[column]
	}
	heightOf := func(i int) int {
		if i < len(heights) && heights[i] > 0 {
			return heights[i]
		}
		return ticketHeight
	}

	cum := 0
	for i := offset; i < len(tickets); i++ {
		next := cum + heightOf(i)
		if ticketY < next {
			return i
		}
		cum = next
	}
	return -1
}

func (m *Model) dropTicket() (tea.Model, tea.Cmd) {
	if len(m.columnTickets) <= m.dragSourceColumn {
		m.dragging = false
		return m, nil
	}

	tickets := m.columnTickets[m.dragSourceColumn]
	if len(tickets) <= m.dragSourceTicket {
		m.dragging = false
		return m, nil
	}

	ticket := tickets[m.dragSourceTicket]
	targetStatus := m.columns[m.dragTargetColumn].Status

	if targetStatus == board.StatusInProgress && ticket.WorktreePath == "" {
		if ticket.UseWorktree {
			if err := m.setupWorktree(ticket); err != nil {
				m.notify("Worktree failed: " + err.Error())
				m.dragging = false
				return m, nil
			}
		} else {
			if err := m.setupMainRepoBranch(ticket); err != nil {
				m.notify("Branch setup failed: " + err.Error())
				m.dragging = false
				return m, nil
			}
		}
	}

	// Wrap up any live session BEFORE Move (see quickMoveTicket). The
	// returned Cmd performs the daemon-side Stop + TicketDone in a
	// background goroutine — must not be dropped or the Update loop
	// freezes for up to ~7s on the daemon RPCs.
	wrapUpCmd := m.wrapUpSessionForTicket(ticket, targetStatus)

	promoted, pruned, _ := m.globalStore.Move(ticket.ID, targetStatus)
	m.refreshColumnTickets()
	m.saveTicket(ticket)

	// Follow the dropped ticket to its new column/position instead of
	// blindly landing on index 0 of the target column. selectTicketByID
	// sets activeColumn/activeTicket and handles vertical scroll
	// (ensureTicketVisible); ensureColumnVisible covers horizontal scroll.
	// If an active filter hides the ticket in its new status the find
	// misses and focus stays in the source column (same clamp-degrade as
	// the quick-move paths) — acceptable for that self-inflicted state.
	m.selectTicketByID(ticket.ID)
	m.ensureColumnVisible()

	m.notify(moveAndPromoteMsg(targetStatus, promoted, pruned))
	m.dragging = false
	m.dragTargetColumn = 0

	return m, wrapUpCmd
}

func (m *Model) handleConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		m.showConfirm = false
		if m.confirmFn != nil {
			return m, m.confirmFn()
		}
	case "n", "N", "esc":
		m.showConfirm = false
	}
	return m, nil
}

func (m *Model) handleChoice(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if key == "esc" {
		m.showChoice = false
		m.choices = nil
		m.choiceMsg = ""
		return m, nil
	}
	for _, c := range m.choices {
		if string(c.Key) == key {
			fn := c.Fn
			m.showChoice = false
			m.choices = nil
			m.choiceMsg = ""
			if fn != nil {
				return m, fn()
			}
			return m, nil
		}
	}
	// Non-matching keys are no-ops while a choice modal is active.
	return m, nil
}

func (m *Model) handleQuit() (tea.Model, tea.Cmd) {
	// If a daemon client is wired up, defer to the daemon-aware exit
	// guard so we never silently kill (or orphan) live agent sessions.
	// The guard's PrepareExit RPC tells us the authoritative ClientCount
	// + Sessions snapshot, and the modal (ModeConfirmExit) gates exit on
	// the user explicitly killing them. See handleQuitRequested.
	if m.daemon != nil {
		return m.handleQuitRequested()
	}

	// Daemon unreachable — fall back to the legacy local-only path so the
	// user is never trapped in the TUI when the daemon is missing.
	runningCount := m.RunningAgentCount()
	if runningCount == 0 {
		return m, tea.Quit
	}

	if !m.config.Behavior.ConfirmQuitWithAgents {
		m.mode = ModeShuttingDown
		return m, tea.Batch(m.spinner.Tick, m.cleanupAsync())
	}

	m.showConfirm = true
	m.confirmMsg = fmt.Sprintf("%d agent(s) running. Quit anyway? [y/N]", runningCount)
	m.confirmFn = func() tea.Cmd {
		m.mode = ModeShuttingDown
		m.showConfirm = false
		return tea.Batch(m.spinner.Tick, m.cleanupAsync())
	}
	return m, nil
}

func (m *Model) cleanupAsync() tea.Cmd {
	return func() tea.Msg {
		m.Cleanup()
		return shutdownCompleteMsg{}
	}
}

// shouldRetryAttachOnEnter is the predicate handleAgentViewMode uses
// to decide whether to intercept Enter for an attach retry vs. forward
// it to the PTY child. The two conditions together capture exactly the
// "stuck post-spawn" state PaneView.View() renders the failure overlay
// for: there's a recorded attach error AND the local view hasn't
// transitioned to Attached (which would clear lastAttachErr anyway).
//
// Extracted as a free function so the keystroke-routing decision can
// be exercised in tests without standing up a real daemonClient (the
// rest of handleAgentViewMode threads through attachExisting, which
// requires a live Client to do anything observable).
func shouldRetryAttachOnEnter(pane *daemonclient.PaneView) bool {
	if pane == nil {
		return false
	}
	return pane.LastAttachErr() != nil && pane.State() != daemonclient.PaneViewAttached
}

// exitToBoard returns the user from agent view to the board, enforcing
// the invariant that the cycle-attach modal flag never outlives the
// focus that justified it. Every path that drops m.focusedPane — the
// keyboard exit (Ctrl+g / Esc) AND the async daemon paths (session
// exited, pane detached/exited, daemon disconnected) — must go through
// here. A stranded cycleAttachPrompt would otherwise resurface as a
// phantom modal on the next agent-view entry, swallowing Ctrl+g though
// the user never cycled.
func (m *Model) exitToBoard() {
	// Release the daemon attach when leaving a session's agent view. The
	// daemon allows ONE attached client per session, and attach used to be
	// held for the connection's whole life — so a TUI that merely backed out
	// to the board kept the slot hostage and a second TUI got ErrAlreadyAttached
	// and a blank pane (the 2026-06-22 report). Detaching here couples attach
	// to viewing: re-entering re-attaches with a fresh snapshot. Detach() is
	// non-blocking and a no-op when the pane isn't attached, so the async
	// focus-drop paths (session exited, pane detached/exited, daemon
	// disconnected) that also funnel through here are unaffected.
	if m.focusedPane != "" {
		if pv := m.panes[m.focusedPane]; pv != nil {
			_ = pv.Detach()
		}
	}
	m.mode = ModeNormal
	m.focusedPane = ""
	m.cycleAttachPrompt = false
}

func (m *Model) handleAgentViewMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// The takeover warning modal swallows all keys until the user
	// resolves it (Enter/y take over, anything else cancels). Checked
	// before the cycle modal and any pane dispatch so the PTY never sees
	// these keys and so it wins if both flags are somehow set.
	if m.takeoverPrompt {
		return m.handleTakeoverPromptKey(msg)
	}
	// The cycle-attach modal swallows all keys until the user resolves
	// it (Enter attaches, Esc returns to the board, Ctrl+\ / Ctrl+]
	// continue cycling). Handled before any pane dispatch so the PTY
	// never sees these keys.
	if m.cycleAttachPrompt {
		return m.handleCycleAttachPromptKey(msg)
	}

	pane, ok := m.panes[m.focusedPane]
	if !ok {
		m.exitToBoard()
		return m, m.maybeSetWindowTitle()
	}

	// Session-cycle bindings are intercepted before the pane forwards
	// keystrokes to the PTY child — otherwise claude/whatever-agent
	// would consume them. Ctrl+\ and Ctrl+] are the closest survivable
	// pair to the original Ctrl+[/Ctrl+] request (Ctrl+[ is bytewise
	// indistinguishable from Esc in this bubbletea build).
	switch msg.String() {
	case "ctrl+]":
		return m.cycleUnattachedSession(1)
	case "ctrl+\\":
		return m.cycleUnattachedSession(-1)
	case "enter":
		// Pane stuck in post-spawn attach-failure state — Enter
		// retries attach, matching the overlay's footer hint. Gated
		// on the exact same predicate PaneView.View() uses to render
		// the overlay, so a normally-attached pane still forwards
		// Enter to the PTY child. attach() clears lastAttachErr on
		// success, which pops the overlay back to the live emulator
		// on the next View() pass. The predicate is factored out so
		// it can be unit-tested without a live daemon.
		if shouldRetryAttachOnEnter(pane) {
			cmd := m.attachExisting(m.focusedPane, pane)
			return m, tea.Batch(cmd, m.maybeSetWindowTitle())
		}
	}

	if result := pane.HandleKey(msg); result != nil {
		switch r := result.(type) {
		case terminal.ExitFocusMsg:
			// Auto mode: instead of returning to the board, jump to the
			// session that has been waiting the longest (skipping any held
			// by a sibling TUI). If none qualifies we fall through to the
			// board below — the always-available off-ramp.
			if m.autoAttach {
				// One List snapshot serves both the attached-elsewhere
				// filter and the subsequent attach's takeover decision, so
				// the jump does a single List, not two.
				sessions := m.liveSessions()
				var elsewhere map[board.TicketID]bool
				if m.daemonClient != nil {
					elsewhere = attachedElsewhereSet(sessions, m.daemonClient.ClientID())
				}
				if id, ok := m.oldestWaitingPeer(elsewhere); ok {
					// Auto attaches DIRECTLY (no preview modal) — the spec is
					// "automatically attaches". oldestWaitingPeer already
					// filtered out sessions another TUI holds, so a plain
					// attach is safe: it can't displace a peer. A race that
					// lost the skip surfaces the takeover warning via
					// attachExistingSnap's gentle probe (correct, not silent).
					// Stay in ModeAgentView, focus the target, do NOT set
					// cycleAttachPrompt. If the pane vanished since selection,
					// fall through to the board.
					if pv := m.panes[id]; pv != nil {
						log.Printf("openkanban model: Auto mode jump -> %s", id)
						m.focusedPane = id
						pv.SetSize(m.width, m.height-2)
						var cmd tea.Cmd
						if pv.State() == daemonclient.PaneViewUnattached {
							cmd = m.attachExistingSnap(id, pv, sessions)
						}
						return m, tea.Batch(cmd, m.maybeSetWindowTitle())
					}
				}
			}
			log.Printf("openkanban model: ExitFocusMsg received, mode -> ModeNormal")
			m.exitToBoard()
		case daemonclient.AttachFirstMsg:
			// User typed into an unattached pane — attach now and
			// re-deliver the key would be nice but is out of scope for
			// PR8. The model just kicks off the attach; the user can
			// retype after the snapshot arrives.
			cmd := m.attachExisting(board.TicketID(r.PaneID), pane)
			return m, tea.Batch(cmd, m.maybeSetWindowTitle())
		}
	}

	return m, m.maybeSetWindowTitle()
}

// handleCycleAttachPromptKey resolves the modal opened by
// cycleUnattachedSession. Enter attaches to the currently focused
// pane and clears the modal; Esc / Ctrl+g cancel and return to the
// board; Ctrl+\ / Ctrl+] keep cycling; every other key is swallowed so
// the modal can't be bypassed by a stray keystroke landing in the PTY.
//
// Ctrl+g is the documented agent-view "exit to board" gesture. Without
// an explicit case here it fell through to the swallow at the bottom,
// so the act of cycling silently disabled Ctrl+g until the user pressed
// Esc — it stays an intentional exit gesture, not a PTY-bound key, so
// it resolves the modal rather than bypassing it.
func (m *Model) handleCycleAttachPromptKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		pv, ok := m.panes[m.focusedPane]
		if !ok || pv == nil {
			m.exitToBoard()
			return m, m.maybeSetWindowTitle()
		}
		m.cycleAttachPrompt = false
		cmd := m.attachExisting(m.focusedPane, pv)
		return m, tea.Batch(cmd, m.maybeSetWindowTitle())
	case "esc", "ctrl+g":
		m.exitToBoard()
		return m, m.maybeSetWindowTitle()
	case "ctrl+]":
		return m.cycleUnattachedSession(1)
	case "ctrl+\\":
		return m.cycleUnattachedSession(-1)
	}
	return m, nil
}

// armTakeoverPrompt switches to the agent view and raises the takeover
// warning modal for a session attached in another TUI. It registers the
// pane when the conflict came from the Owns cold-start fast path (P2),
// which builds a PaneView not yet in m.panes; P1 callers already have
// the pane registered and we reuse it.
func (m *Model) armTakeoverPrompt(msg attachConflictMsg) {
	pv := m.panes[msg.ticketID]
	if pv == nil {
		pv = msg.pv
	}
	if pv != nil {
		m.panes[msg.ticketID] = pv
		// The session genuinely exists daemon-side (it rejected us
		// because someone else holds it), so record ownership now rather
		// than waiting for the next resync.
		m.daemonOwned[msg.ticketID] = struct{}{}
	}
	m.takeoverPending.ticketID = msg.ticketID
	m.takeoverPending.pv = pv
	m.takeoverPrompt = true
	m.focusedPane = msg.ticketID
	m.mode = ModeAgentView
}

// handleTakeoverPromptKey resolves the warning modal raised when an
// attach probe found the session attached in another TUI. Enter / y
// take over (forced Takeover, no re-probe — re-probing would loop back
// into this prompt); Esc or any other key cancels back to the board
// without disturbing the other window.
func (m *Model) handleTakeoverPromptKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter", "y", "Y":
		pending := m.takeoverPending
		m.takeoverPrompt = false
		m.takeoverPending.ticketID = ""
		m.takeoverPending.pv = nil
		return m, tea.Batch(
			m.doAttach(pending.ticketID, pending.pv, true),
			m.maybeSetWindowTitle(),
		)
	default:
		// Esc / n / anything else: cancel. Default-to-cancel is the safe
		// choice — we never displace the other TUI without explicit
		// confirmation.
		m.takeoverPrompt = false
		m.takeoverPending.ticketID = ""
		m.takeoverPending.pv = nil
		m.mode = ModeNormal
		m.focusedPane = ""
		return m, m.maybeSetWindowTitle()
	}
}

func (m *Model) handleAgentViewMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	pane, ok := m.panes[m.focusedPane]
	if !ok {
		return m, nil
	}
	adjusted, action := routeAgentViewMouse(msg, m.width, m.agentViewChromeHeight())
	switch action {
	case agentViewMouseCloseModal:
		m.mode = ModeNormal
		return m, m.maybeSetWindowTitle()
	case agentViewMouseForward:
		pane.HandleMouse(adjusted)
	}
	return m, nil
}

// agentViewMouseAction is the routing decision for an incoming mouse
// event in the agent view modal.
type agentViewMouseAction int

const (
	agentViewMouseDrop agentViewMouseAction = iota
	agentViewMouseForward
	agentViewMouseCloseModal
)

// routeAgentViewMouse decides what handleAgentViewMouse should do
// with a mouse event: close the modal (close-button hit), forward
// to the pane with pane-relative coordinates, or drop it.
//
// BubbleTea reports mouse Y relative to the host terminal (row 0
// = top of TUI). The pane's content sits below the agent view's
// chrome (1-row header, plus optional 1-row deps line). We subtract
// chrome height so the pane sees pane-relative coordinates;
// otherwise selection lands one (or two) rows below the cursor.
//
// Position-sensitive events (click/drag) landing on the chrome
// rows are dropped. Wheel events are position-insensitive — the
// user expects scroll to work regardless of which row the cursor
// happens to sit on — so they are clamped to row 0 and forwarded.
func routeAgentViewMouse(msg tea.MouseMsg, width, chrome int) (tea.MouseMsg, agentViewMouseAction) {
	if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft {
		if msg.Y == 0 && msg.X >= width-25 {
			return msg, agentViewMouseCloseModal
		}
	}
	adjusted := msg
	adjusted.Y = msg.Y - chrome
	if adjusted.Y < 0 {
		if !tea.MouseEvent(msg).IsWheel() {
			return msg, agentViewMouseDrop
		}
		adjusted.Y = 0
	}
	return adjusted, agentViewMouseForward
}

// agentViewChromeHeight returns the height in rows of the non-pane
// content rendered above the agent terminal pane: 1 for the header,
// plus 1 for the deps line when the focused ticket has any
// BlockedBy / Blocks relationships. Used to translate host-terminal
// mouse coords into pane-relative coords.
func (m *Model) agentViewChromeHeight() int {
	ticket, _ := m.globalStore.Get(m.focusedPane)
	return agentChromeHeight(ticketHasDeps(m.globalStore, ticket))
}

// ticketHasDeps reports whether a ticket has any incoming or outgoing
// dependency relationships (i.e. the agent view should render its
// deps line for this ticket).
func ticketHasDeps(g *project.GlobalTicketStore, t *board.Ticket) bool {
	if g == nil || t == nil {
		return false
	}
	return len(g.GetBlockedBy(t.ID)) > 0 || len(g.GetBlocks(t.ID)) > 0
}

// agentChromeHeight is the pure mapping from "does the deps line
// render?" to chrome height. Kept separate from the Model so it can
// be unit-tested without constructing a store.
//
// Layout: 1 row header + (0 or 1) deps row + 1 row heavy rule. The
// rule is the visual boundary between openkanban chrome and the
// embedded PTY, so mouse coords must account for it when translating
// host-terminal Y into pane-relative Y.
func agentChromeHeight(hasDeps bool) int {
	if hasDeps {
		return 3
	}
	return 2
}

func (m *Model) handleTicketFormMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		m.formScrollOffset -= 3
		if m.formScrollOffset < 0 {
			m.formScrollOffset = 0
		}
		return m, nil
	case tea.MouseButtonWheelDown:
		m.formScrollOffset += 3
		return m, nil
	}

	formWidth := 50
	formLeft := (m.width - formWidth) / 2
	formRight := formLeft + formWidth

	if msg.X < formLeft || msg.X > formRight {
		return m, nil
	}

	formTop := (m.height - 28) / 2
	relY := msg.Y - formTop

	// Bands map screen rows to form fields in VISUAL order (see
	// renderTicketForm). Project sits between Description and Branch. These
	// fixed bands are a best-effort convenience for clicking-to-focus the
	// upper fields; keyboard navigation (Tab / arrows / Enter) is the
	// canonical, layout-accurate path. Because Project's list is
	// variable-height once focused, clicks on list entries below its band
	// are not resolved here — use the keyboard to pick a project.
	var clickedField int = -1
	switch {
	case relY >= 3 && relY <= 4:
		clickedField = formFieldTitle
	case relY >= 6 && relY <= 9:
		clickedField = formFieldDescription
	case relY >= 11 && relY <= 13:
		clickedField = formFieldProject
	case relY >= 15 && relY <= 17:
		clickedField = formFieldBranch
	case relY >= 19 && relY <= 21:
		clickedField = formFieldLabels
	case relY >= 23:
		clickedField = formFieldPriority
	}

	if clickedField >= 0 && msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft {
		m.blurAllFormFields()
		m.ticketFormField = clickedField
		m.focusCurrentField()

		if clickedField == formFieldProject && !m.showAddProjectForm {
			projects := m.globalStore.Projects()
			// Project label is at relY 11, its description at 12, so the
			// first project list row renders at 13 → maps to index 0.
			projectRelY := relY - 13
			if projectRelY >= 0 && projectRelY <= len(projects) {
				m.projectListIndex = projectRelY
				if projectRelY == len(projects) {
					m.showAddProjectForm = true
					m.addProjectPath.SetValue("")
					m.addProjectPath.Focus()
					return m, textinput.Blink
				}
				if projectRelY < len(projects) {
					m.selectedProject = projects[projectRelY]
				}
			}
		}
	}

	var cmd tea.Cmd
	switch m.ticketFormField {
	case formFieldTitle:
		m.titleInput, cmd = m.titleInput.Update(msg)
	case formFieldDescription:
		m.descInput, cmd = m.descInput.Update(msg)
	case formFieldBranch:
		if !m.branchLocked {
			m.branchInput, cmd = m.branchInput.Update(msg)
		}
	case formFieldLabels:
		m.labelsInput, cmd = m.labelsInput.Update(msg)
	}

	return m, cmd
}

func (m *Model) handleCreateTicketMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	return m.handleTicketForm(msg, false)
}

func (m *Model) handleEditTicketMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	return m.handleTicketForm(msg, true)
}

func (m *Model) handleTicketForm(msg tea.KeyMsg, isEdit bool) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		m.mode = ModeNormal
		m.blurAllFormFields()
		m.editingTicketID = ""
		m.branchLocked = false
		m.showAddProjectForm = false
		return m, nil

	case "tab":
		if m.showAddProjectForm && m.addProjectPath.Value() != "" {
			m.createProjectFromPath()
			if m.showAddProjectForm {
				return m, nil
			}
		} else if m.showAddProjectForm {
			m.showAddProjectForm = false
		}
		return m.nextFormField(isEdit), nil
	case "shift+tab":
		if m.showAddProjectForm {
			m.showAddProjectForm = false
		}
		return m.prevFormField(isEdit), nil

	case "ctrl+s":
		return m.saveTicketForm(isEdit)

	case "enter":
		if m.ticketFormField == formFieldTitle {
			return m.saveTicketForm(isEdit)
		}
		if m.ticketFormField == formFieldProject {
			return m.handleProjectSelection()
		}

	case "esc":
		if m.showAddProjectForm {
			m.showAddProjectForm = false
			m.addProjectPath.Blur()
			return m, nil
		}
		m.mode = ModeNormal
		m.blurAllFormFields()
		m.editingTicketID = ""
		m.branchLocked = false
		return m, nil
	}

	var cmd tea.Cmd
	switch m.ticketFormField {
	case formFieldTitle:
		m.titleInput, cmd = m.titleInput.Update(msg)
	case formFieldDescription:
		m.descInput, cmd = m.descInput.Update(msg)
	case formFieldBranch:
		if !m.branchLocked {
			m.branchInput, cmd = m.branchInput.Update(msg)
		}
	case formFieldLabels:
		m.labelsInput, cmd = m.labelsInput.Update(msg)
	case formFieldPriority:
		cmd = m.handlePriorityNav(msg)
	case formFieldType:
		cmd = m.handleTypeNav(msg)
	case formFieldWorktree:
		cmd = m.handleWorktreeToggle(msg)
	case formFieldBlockedBy:
		cmd = m.handleBlockerNav(msg)
	case formFieldProject:
		if m.showAddProjectForm {
			m.addProjectPath, cmd = m.addProjectPath.Update(msg)
		} else {
			cmd = m.handleProjectListNav(msg)
		}
	}
	return m, cmd
}

func (m *Model) handlePriorityNav(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "j", "down", "l", "right":
		m.ticketPriority++
		if m.ticketPriority > 5 {
			m.ticketPriority = 1
		}
	case "k", "up", "h", "left":
		m.ticketPriority--
		if m.ticketPriority < 1 {
			m.ticketPriority = 5
		}
	case "1", "2", "3", "4", "5":
		m.ticketPriority = int(msg.String()[0] - '0')
	}
	return nil
}

// handleTypeNav cycles the create/edit form's Type picker through
// ticketTypeOptions with the same j/k/arrow gestures as the priority selector.
// Changing the type also resets the Worktree default for that type
// (research/spec → no worktree); the user can still flip the Worktree field
// afterward since it renders right below.
func (m *Model) handleTypeNav(msg tea.KeyMsg) tea.Cmd {
	idx := 0
	for i, t := range ticketTypeOptions {
		if t == m.ticketType {
			idx = i
			break
		}
	}
	switch msg.String() {
	case "j", "down", "l", "right":
		idx = (idx + 1) % len(ticketTypeOptions)
	case "k", "up", "h", "left":
		idx = (idx - 1 + len(ticketTypeOptions)) % len(ticketTypeOptions)
	}
	m.ticketType = ticketTypeOptions[idx]
	m.ticketUseWorktree = defaultWorktreeForType(m.ticketType)
	return nil
}

func (m *Model) handleWorktreeToggle(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case " ", "enter", "h", "l", "left", "right":
		m.ticketUseWorktree = !m.ticketUseWorktree
	case "y", "Y":
		m.ticketUseWorktree = true
	case "n", "N":
		m.ticketUseWorktree = false
	}
	return nil
}

func (m *Model) handleProjectListNav(msg tea.KeyMsg) tea.Cmd {
	projects := m.globalStore.Projects()
	maxIndex := len(projects)

	switch msg.String() {
	case "j", "down":
		m.projectListIndex++
		if m.projectListIndex > maxIndex {
			m.projectListIndex = 0
		}
	case "k", "up":
		m.projectListIndex--
		if m.projectListIndex < 0 {
			m.projectListIndex = maxIndex
		}
	case "d":
		if m.projectListIndex < len(projects) {
			m.confirmDeleteProject(projects[m.projectListIndex])
		}
	}

	// Auto-select the highlighted project (if not on "+ Add project" option)
	if m.projectListIndex < len(projects) {
		m.selectedProject = projects[m.projectListIndex]
	}

	return nil
}

func (m *Model) handleBlockerNav(msg tea.KeyMsg) tea.Cmd {
	visibleCandidates := m.getFilteredBlockerCandidates()

	switch msg.Type {
	case tea.KeyDown, tea.KeyCtrlN:
		if len(visibleCandidates) > 0 {
			m.blockerListIndex++
			if m.blockerListIndex >= len(visibleCandidates) {
				m.blockerListIndex = 0
			}
		}
		return nil
	case tea.KeyUp, tea.KeyCtrlP:
		if len(visibleCandidates) > 0 {
			m.blockerListIndex--
			if m.blockerListIndex < 0 {
				m.blockerListIndex = len(visibleCandidates) - 1
			}
		}
		return nil
	case tea.KeySpace, tea.KeyEnter:
		if m.blockerListIndex < len(visibleCandidates) {
			ticket := visibleCandidates[m.blockerListIndex]
			if m.selectedBlockers[ticket.ID] {
				delete(m.selectedBlockers, ticket.ID)
			} else {
				m.selectedBlockers[ticket.ID] = true
			}
		}
		return nil
	}

	var cmd tea.Cmd
	m.blockerFilterInput, cmd = m.blockerFilterInput.Update(msg)

	newVisible := m.getFilteredBlockerCandidates()
	if m.blockerListIndex >= len(newVisible) && len(newVisible) > 0 {
		m.blockerListIndex = len(newVisible) - 1
	} else if len(newVisible) == 0 {
		m.blockerListIndex = 0
	}

	return cmd
}

func (m *Model) getFilteredBlockerCandidates() []*board.Ticket {
	filterVal := m.blockerFilterInput.Value()
	if filterVal == "" {
		return m.blockerCandidates
	}

	var visible []*board.Ticket
	for _, t := range m.blockerCandidates {
		if strings.Contains(strings.ToLower(t.Title), strings.ToLower(filterVal)) {
			visible = append(visible, t)
		}
	}
	return visible
}

func (m *Model) initBlockerCandidates(excludeTicketID board.TicketID) {
	m.blockerCandidates = nil
	for _, ticket := range m.globalStore.All() {
		if ticket.ID == excludeTicketID {
			continue
		}
		if ticket.Status == board.StatusArchived {
			continue
		}
		m.blockerCandidates = append(m.blockerCandidates, ticket)
	}
	sort.Slice(m.blockerCandidates, func(i, j int) bool {
		return m.blockerCandidates[i].Title < m.blockerCandidates[j].Title
	})
}

func (m *Model) collectSelectedBlockers() []board.TicketID {
	var blockers []board.TicketID
	for id := range m.selectedBlockers {
		blockers = append(blockers, id)
	}
	sort.Slice(blockers, func(i, j int) bool {
		return string(blockers[i]) < string(blockers[j])
	})
	return blockers
}

func (m *Model) confirmDeleteProject(p *project.Project) {
	ticketCount := 0
	for _, t := range m.globalStore.All() {
		if t.ProjectID == p.ID {
			ticketCount++
		}
	}

	if ticketCount > 0 {
		m.confirmMsg = fmt.Sprintf("Delete '%s' and its %d ticket(s)?", p.Name, ticketCount)
	} else {
		m.confirmMsg = fmt.Sprintf("Delete project '%s'?", p.Name)
	}

	m.showConfirm = true
	m.confirmFn = func() tea.Cmd {
		if err := m.globalStore.RemoveProject(p.ID); err != nil {
			m.notify("Failed to delete: " + err.Error())
			return nil
		}
		delete(m.worktreeMgrs, p.ID)

		projects := m.globalStore.Projects()
		if len(projects) > 0 {
			if m.projectListIndex >= len(projects) {
				m.projectListIndex = len(projects) - 1
			}
			m.selectedProject = projects[m.projectListIndex]
		} else {
			m.selectedProject = nil
		}

		delete(m.filterProjectIDs, p.ID)

		m.notify("Deleted: " + p.Name)
		return nil
	}
}

func (m *Model) handleProjectSelection() (tea.Model, tea.Cmd) {
	projects := m.globalStore.Projects()

	if m.showAddProjectForm {
		return m.createProjectFromPath()
	}

	if m.projectListIndex < len(projects) {
		m.selectedProject = projects[m.projectListIndex]
		return m, nil
	}

	m.showAddProjectForm = true
	m.addProjectPath.SetValue("")
	m.addProjectPath.Focus()
	return m, textinput.Blink
}

func (m *Model) createProjectFromPath() (tea.Model, tea.Cmd) {
	path := strings.TrimSpace(m.addProjectPath.Value())
	if path == "" {
		m.notify("Path cannot be empty")
		return m, nil
	}

	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			path = filepath.Join(home, path[2:])
		}
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		m.notify("Invalid path: " + err.Error())
		return m, nil
	}

	gitDir := filepath.Join(absPath, ".git")
	if _, err := os.Stat(gitDir); err != nil {
		m.notify("Not a git repository")
		return m, nil
	}

	name := filepath.Base(absPath)

	newProject := project.NewProject(name, absPath)
	// Project settings only store explicit user overrides; empty string/int
	// values cascade to global config via GetBranchPrefix() etc. (Agent identity
	// does NOT cascade — it is pinned per project via the sidebar 'g' key.)

	if err := m.projectRegistry.Add(newProject); err != nil {
		m.notify("Failed to save: " + err.Error())
		return m, nil
	}

	m.globalStore.AddProject(newProject)
	m.worktreeMgrs[newProject.ID] = git.NewWorktreeManager(newProject)
	m.selectedProject = newProject
	m.showAddProjectForm = false
	m.addProjectPath.Blur()
	m.projectListIndex = len(m.globalStore.Projects()) - 1

	if m.mode == ModeCreateProject {
		m.mode = ModeNormal
	}

	m.notify("Added project: " + name)
	return m, nil
}

func (m *Model) nextFormField(isEdit bool) *Model {
	m.blurAllFormFields()
	m.ticketFormField++

	for {
		if m.ticketFormField > formFieldBlockedBy {
			m.ticketFormField = formFieldTitle
		}
		if m.ticketFormField == formFieldBranch && m.branchLocked {
			m.ticketFormField++
			continue
		}
		break
	}
	m.focusCurrentField()
	return m
}

func (m *Model) prevFormField(isEdit bool) *Model {
	m.blurAllFormFields()
	m.ticketFormField--

	for {
		if m.ticketFormField < formFieldTitle {
			m.ticketFormField = formFieldBlockedBy
		}
		if m.ticketFormField == formFieldBranch && m.branchLocked {
			m.ticketFormField--
			continue
		}
		break
	}
	m.focusCurrentField()
	return m
}

func (m *Model) blurAllFormFields() {
	m.titleInput.Blur()
	m.descInput.Blur()
	m.branchInput.Blur()
	m.labelsInput.Blur()
	m.blockerFilterInput.Blur()
	m.projectInput.Blur()
}

func (m *Model) focusCurrentField() {
	switch m.ticketFormField {
	case formFieldTitle:
		m.titleInput.Focus()
	case formFieldDescription:
		m.descInput.Focus()
	case formFieldBranch:
		m.branchInput.Focus()
	case formFieldLabels:
		m.labelsInput.Focus()
	case formFieldPriority:
		break
	case formFieldType:
		break
	case formFieldWorktree:
		break
	case formFieldBlockedBy:
		m.blockerFilterInput.Focus()
	case formFieldProject:
		m.projectInput.Focus()
	}
}

func (m *Model) saveTicketForm(isEdit bool) (tea.Model, tea.Cmd) {
	title := strings.TrimSpace(m.titleInput.Value())
	if title == "" {
		m.notify("Title cannot be empty")
		return m, nil
	}

	if m.selectedProject == nil {
		m.notify("No project selected")
		return m, nil
	}

	desc := strings.TrimSpace(m.descInput.Value())
	branchName := strings.TrimSpace(m.branchInput.Value())
	if branchName == "" {
		branchName = m.generateBranchNameFromTitle(title, m.selectedProject)
	}

	labels := m.parseLabels(m.labelsInput.Value())

	blockedBy := m.collectSelectedBlockers()

	if isEdit && m.editingTicketID != "" {
		ticket, _ := m.globalStore.Get(m.editingTicketID)
		if ticket != nil {
			if ticket.ProjectID != m.selectedProject.ID {
				if err := m.globalStore.MoveProject(ticket.ID, m.selectedProject.ID); err != nil {
					switch err {
					case project.ErrTicketHasWorktree:
						m.notify("Cannot change project: ticket has an active worktree")
					default:
						m.notify("Failed to change project: " + err.Error())
					}
					return m, nil
				}
			}
			ticket.Title = title
			ticket.Description = desc
			if !m.branchLocked {
				ticket.BranchName = branchName
			}
			ticket.Labels = labels
			ticket.Priority = m.ticketPriority
			ticket.Type = m.ticketType
			ticket.UseWorktree = m.ticketUseWorktree
			ticket.BlockedBy = blockedBy
			ticket.Touch()
			m.saveTicket(ticket)
			m.refreshColumnTickets()
			// Keep focus on the edited ticket: a changed priority (or
			// project) can reorder/relocate it, leaving the stale index
			// pointing at a different card. Mirrors the create branch.
			m.selectTicketByID(ticket.ID)
			m.notify("Updated: " + title)
		}
	} else {
		ticket := board.NewTicket(title, m.selectedProject.ID)
		ticket.Description = desc
		ticket.BranchName = branchName
		ticket.Labels = labels
		ticket.Priority = m.ticketPriority
		ticket.Type = m.ticketType
		ticket.UseWorktree = m.ticketUseWorktree
		ticket.BlockedBy = blockedBy
		// in_review and done are "outbound" columns — landing a brand new
		// ticket there is almost never intentional, so fall back to
		// in_progress. backlog and in_progress keep the focused column.
		status := m.columns[m.activeColumn].Status
		if status == board.StatusInReview || status == board.StatusDone {
			status = board.StatusInProgress
		}
		ticket.Status = status
		m.globalStore.Add(ticket)
		// A brand-new ticket has no daemon session and an arbitrary
		// title, so an active filter (open-only, search query, or a
		// project narrow) would hide it — leaving selectTicketByID
		// nothing to land on. Clear whatever would hide THIS ticket so
		// the thing the user just created is always visible and selected.
		m.revealThroughFilters(ticket)
		m.refreshColumnTickets()
		m.selectTicketByID(ticket.ID)
		m.ensureColumnVisible()
		m.saveTicket(ticket)
		m.notify("Created: " + title)
	}

	m.mode = ModeNormal
	m.blurAllFormFields()
	m.editingTicketID = ""
	m.branchLocked = false
	return m, nil
}

func (m *Model) parseLabels(input string) []string {
	if strings.TrimSpace(input) == "" {
		return []string{}
	}
	parts := strings.Split(input, ",")
	var labels []string
	for _, p := range parts {
		label := strings.TrimSpace(p)
		if label != "" {
			labels = append(labels, label)
		}
	}
	return labels
}

type settingsField struct {
	key         string
	label       string
	kind        string
	description string
}

var settingsFields = []settingsField{
	{"theme", "Theme", "theme", "Color theme for the UI"},
	{"confirm_quit", "Confirm Quit", "toggle", "Prompt before quitting with running agents"},
	{"branch_prefix", "Branch Prefix", "text", "Prefix for auto-generated branch names (e.g. task/, feature/)"},
	{"delete_worktree", "Delete Worktree", "toggle", "Remove git worktree when deleting tickets"},
	{"delete_branch", "Delete Branch", "toggle", "Delete git branch when deleting tickets"},
	{"force_cleanup", "Force Cleanup", "toggle", "Force worktree removal even with uncommitted changes"},
	{"sidebar_visible", "Show Sidebar", "toggle", "Toggle the project sidebar visibility"},
	{"filter_project", "Filter Project", "project", "Show only tickets from a specific project"},
}

func (m *Model) handleSettingsMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.settingsEditing {
		return m.handleSettingsEdit(msg)
	}

	switch msg.String() {
	case "j", "down":
		m.settingsIndex++
		if m.settingsIndex >= len(settingsFields) {
			m.settingsIndex = len(settingsFields) - 1
		}
	case "k", "up":
		m.settingsIndex--
		m.settingsIndex = max(m.settingsIndex, 0)
	case "enter", " ":
		return m.enterSettingsEdit()
	case "esc", "q":
		m.mode = ModeNormal
	}
	return m, nil
}

func (m *Model) handleSettingsEdit(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	field := settingsFields[m.settingsIndex]

	if field.kind == "project" {
		m.filterInput.SetValue(m.filterQuery)
		m.filterInput.Focus()
		m.mode = ModeFilter
		return m, textinput.Blink
	}

	if field.kind == "theme" {
		return m.handleThemeNav(msg)
	}

	switch msg.String() {
	case "enter":
		m.applySettingsValue(field.key, m.settingsInput.Value())
		m.settingsEditing = false
		m.settingsInput.Blur()
		m.notify("Settings saved")
		return m, nil
	case "esc":
		m.settingsEditing = false
		m.settingsInput.Blur()
		return m, nil
	}

	var cmd tea.Cmd
	m.settingsInput, cmd = m.settingsInput.Update(msg)
	return m, cmd
}

func (m *Model) handleThemeNav(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	themes := config.ThemeNames()
	if len(themes) == 0 {
		return m, nil
	}

	switch msg.String() {
	case "j", "down":
		m.themeListIndex++
		if m.themeListIndex >= len(themes) {
			m.themeListIndex = 0
		}
		m.applySettingsValue("theme", themes[m.themeListIndex])
	case "k", "up":
		m.themeListIndex--
		if m.themeListIndex < 0 {
			m.themeListIndex = len(themes) - 1
		}
		m.applySettingsValue("theme", themes[m.themeListIndex])
	case "enter":
		m.settingsEditing = false
		m.notify("Theme: " + themes[m.themeListIndex])
	case "esc":
		m.settingsEditing = false
	}

	return m, nil
}

func (m *Model) handleSettingsMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
		return m, nil
	}

	formTop := (m.height - 10) / 2
	relY := msg.Y - formTop - 3

	if relY >= 0 && relY < len(settingsFields) {
		m.settingsIndex = relY
		return m.enterSettingsEdit()
	}

	return m, nil
}

func (m *Model) handleConfirmMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
		return m, nil
	}

	formCenterY := m.height / 2
	formCenterX := m.width / 2

	yesX := formCenterX - 10
	noX := formCenterX + 5

	if msg.Y == formCenterY+2 {
		if msg.X >= yesX && msg.X <= yesX+5 {
			m.showConfirm = false
			if m.confirmFn != nil {
				return m, m.confirmFn()
			}
		}
		if msg.X >= noX && msg.X <= noX+4 {
			m.showConfirm = false
		}
	}

	return m, nil
}

func (m *Model) enterSettingsEdit() (tea.Model, tea.Cmd) {
	field := settingsFields[m.settingsIndex]

	switch field.kind {
	case "project":
		m.filterInput.SetValue(m.filterQuery)
		m.filterInput.Focus()
		m.mode = ModeFilter
		return m, textinput.Blink

	case "toggle":
		m.applySettingsValue(field.key, "")
		status := m.getSettingsValue(field.key)
		m.notify(field.label + ": " + status)
		return m, nil

	case "theme":
		themes := config.ThemeNames()
		current := m.config.UI.Theme
		m.themeListIndex = 0
		for i, t := range themes {
			if t == current {
				m.themeListIndex = i
				break
			}
		}
		m.settingsEditing = true
		return m, nil

	case "text":
		m.settingsEditing = true
		m.settingsInput.SetValue(m.getSettingsValue(field.key))
		m.settingsInput.Focus()
		return m, textinput.Blink

	default:
		m.settingsEditing = true
		m.settingsInput.SetValue(m.getSettingsValue(field.key))
		m.settingsInput.Focus()
		return m, textinput.Blink
	}
}

func (m *Model) getSettingsValue(key string) string {
	switch key {
	case "theme":
		return m.config.UI.Theme
	case "confirm_quit":
		if m.config.Behavior.ConfirmQuitWithAgents {
			return "On"
		}
		return "Off"
	case "branch_prefix":
		return m.config.Defaults.BranchPrefix
	case "delete_worktree":
		if m.config.Cleanup.DeleteWorktree {
			return "On"
		}
		return "Off"
	case "delete_branch":
		if m.config.Cleanup.DeleteBranch {
			return "On"
		}
		return "Off"
	case "force_cleanup":
		if m.config.Cleanup.ForceWorktreeRemoval {
			return "On"
		}
		return "Off"
	case "filter_project":
		count := len(m.filterProjectIDs)
		if count == 0 {
			return "All Projects"
		}
		return fmt.Sprintf("%d selected", count)
	case "sidebar_visible":
		if m.sidebarVisible {
			return "On"
		}
		return "Off"
	}
	return ""
}

func (m *Model) applySettingsValue(key, value string) {
	switch key {
	case "theme":
		m.config.UI.Theme = value
		m.theme = m.config.GetTheme()
		m.colors = newUIColors(m.theme)
		m.config.Save("")
	case "confirm_quit":
		m.config.Behavior.ConfirmQuitWithAgents = !m.config.Behavior.ConfirmQuitWithAgents
		m.config.Save("")
	case "branch_prefix":
		m.config.Defaults.BranchPrefix = value
		m.config.Save("")
	case "delete_worktree":
		m.config.Cleanup.DeleteWorktree = !m.config.Cleanup.DeleteWorktree
		m.config.Save("")
	case "delete_branch":
		m.config.Cleanup.DeleteBranch = !m.config.Cleanup.DeleteBranch
		m.config.Save("")
	case "force_cleanup":
		m.config.Cleanup.ForceWorktreeRemoval = !m.config.Cleanup.ForceWorktreeRemoval
		m.config.Save("")
	case "sidebar_visible":
		m.sidebarVisible = !m.sidebarVisible
		m.config.UI.SidebarVisible = m.sidebarVisible
		if !m.sidebarVisible {
			m.sidebarFocused = false
		}
		m.config.Save("")
	}
}

func (m *Model) handleFilterMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		m.filterInput.Blur()
		m.mode = ModeNormal
		return m, nil
	case "esc":
		m.filterQuery = ""
		m.filterInput.SetValue("")
		m.filterInput.Blur()
		m.mode = ModeNormal
		m.refreshColumnTickets()
		return m, nil
	}

	var cmd tea.Cmd
	m.filterInput, cmd = m.filterInput.Update(msg)
	m.filterQuery = m.filterInput.Value()
	m.refreshColumnTickets()
	return m, cmd
}

func (m *Model) handleFilterMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	m.filterInput, cmd = m.filterInput.Update(msg)
	return m, cmd
}

func (m *Model) handleCreateProjectMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		return m.createProjectFromPath()
	case "esc":
		m.mode = ModeNormal
		m.addProjectPath.Blur()
		return m, nil
	case "ctrl+c":
		m.mode = ModeNormal
		m.addProjectPath.Blur()
		return m, nil
	}

	var cmd tea.Cmd
	m.addProjectPath, cmd = m.addProjectPath.Update(msg)
	return m, cmd
}

func (m *Model) clearFilter() {
	m.filterQuery = ""
	m.filterProjectIDs = make(map[string]bool)
	m.refreshColumnTickets()
}

func (m *Model) toggleProjectFilter(projectID string) {
	if m.filterProjectIDs[projectID] {
		delete(m.filterProjectIDs, projectID)
	} else {
		m.filterProjectIDs[projectID] = true
	}
	m.filterQuery = ""
	m.refreshColumnTickets()
}

func (m *Model) toggleAllProjects() {
	projects := m.globalStore.Projects()
	allSelected := len(m.filterProjectIDs) == len(projects) && len(projects) > 0
	for _, p := range projects {
		if !m.filterProjectIDs[p.ID] {
			allSelected = false
			break
		}
	}

	if allSelected || len(m.filterProjectIDs) == 0 {
		m.filterProjectIDs = make(map[string]bool)
		for _, p := range projects {
			m.filterProjectIDs[p.ID] = true
		}
		m.notify("All projects selected")
	} else {
		m.filterProjectIDs = make(map[string]bool)
		m.notify("All projects deselected")
	}
	m.filterQuery = ""
	m.refreshColumnTickets()
}

func (m *Model) moveColumn(delta int) {
	m.activeColumn += delta
	m.activeColumn = max(m.activeColumn, 0)
	if m.activeColumn >= len(m.columns) {
		m.activeColumn = len(m.columns) - 1
	}
	m.activeTicket = 0
	m.ensureColumnVisible()
	m.ensureTicketVisible()
}

func (m *Model) ensureColumnVisible() {
	colWidth := m.calcColumnWidth()
	visibleCols := m.visibleColumnCount(colWidth)

	if m.activeColumn < m.scrollOffset {
		m.scrollOffset = m.activeColumn
	} else if m.activeColumn >= m.scrollOffset+visibleCols {
		m.scrollOffset = m.activeColumn - visibleCols + 1
	}

	maxOffset := max(len(m.columns)-visibleCols, 0)
	if m.scrollOffset > maxOffset {
		m.scrollOffset = maxOffset
	}
}

func (m *Model) headerHeight() int {
	const (
		content      = 1
		borderBottom = 1
		spacing      = 2
	)
	return content + borderBottom + spacing
}

func (m *Model) calcColumnWidth() int {
	boardW := m.boardWidth()
	if boardW == 0 || len(m.columns) == 0 {
		return minColumnWidth
	}

	numCols := len(m.columns)
	totalOverhead := numCols * columnOverhead
	colWidth := (boardW - totalOverhead) / numCols

	return max(colWidth, minColumnWidth)
}

func (m *Model) visibleColumnCount(colWidth int) int {
	boardW := m.boardWidth()
	if boardW == 0 {
		return len(m.columns)
	}
	visible := boardW / (colWidth + columnOverhead)
	visible = max(visible, 1)
	if visible > len(m.columns) {
		visible = len(m.columns)
	}
	return visible
}

func (m *Model) distributeWidth(numCols int) (baseWidth, remainder int) {
	boardW := m.boardWidth()
	if numCols == 0 || boardW == 0 {
		return minColumnWidth, 0
	}
	borders := numCols * 2
	margins := numCols - 1
	available := boardW - borders - margins
	baseWidth = available / numCols
	remainder = available % numCols
	if baseWidth < minColumnWidth {
		baseWidth = minColumnWidth
		remainder = 0
	}
	return baseWidth, remainder
}

func (m *Model) moveTicket(delta int) {
	if len(m.columnTickets) <= m.activeColumn {
		return
	}
	tickets := m.columnTickets[m.activeColumn]
	m.activeTicket += delta
	m.activeTicket = max(m.activeTicket, 0)
	if m.activeTicket >= len(tickets) {
		m.activeTicket = max(len(tickets)-1, 0)
	}
	m.ensureTicketVisible()
}

// boardAreaHeight is the vertical space available for the column row, between
// the header (with its trailing newline) and the status bar (with its
// preceding newline). headerHeight() already includes its own padding/border.
func (m *Model) boardAreaHeight() int {
	const (
		newlineAfterHeader     = 1
		newlineBeforeStatusBar = 1
		statusBarHeight        = 1
	)
	return m.height - m.headerHeight() - newlineAfterHeader - newlineBeforeStatusBar - statusBarHeight
}

func (m *Model) columnContentHeight() int {
	const (
		columnBottomBorder = 1
	)
	return m.boardAreaHeight() - columnHeaderHeight - columnBottomBorder
}

func (m *Model) ensureTicketVisible() {
	if m.activeColumn < 0 || m.activeColumn >= len(m.columnOffsets) {
		return
	}

	offset := m.columnOffsets[m.activeColumn]

	// Scroll up if the active ticket is above the visible window.
	if m.activeTicket < offset {
		m.columnOffsets[m.activeColumn] = m.activeTicket
		return
	}

	// Scroll down if the active ticket would fall outside the rendered
	// budget at the current offset. Walk per-ticket heights (or fall back
	// to ticketHeight if the heights cache hasn't been populated yet, e.g.
	// before the first View() after a window-size change).
	var heights []int
	if m.activeColumn < len(m.columnTicketHeights) {
		heights = m.columnTicketHeights[m.activeColumn]
	}
	heightOf := func(i int) int {
		if i < len(heights) && heights[i] > 0 {
			return heights[i]
		}
		return ticketHeight
	}

	budget := m.columnContentHeight()
	if offset > 0 {
		budget -= 1 // ▲ indicator
	}

	// From the current offset, see if activeTicket fits. If not, advance
	// the offset one ticket at a time until it does.
	for {
		fits := false
		used := 0
		// Account for trailing ▼ indicator if there's anything after the
		// active ticket — keep the active card off the indicator row.
		for i := offset; i < len(m.columnTickets[m.activeColumn]); i++ {
			cost := heightOf(i)
			reserve := 0
			if i < len(m.columnTickets[m.activeColumn])-1 {
				reserve = 1
			}
			if used+cost+reserve > budget {
				break
			}
			used += cost
			if i == m.activeTicket {
				fits = true
				break
			}
		}
		if fits || offset >= m.activeTicket {
			break
		}
		offset++
		// Recompute budget: once offset > 0 the ▲ indicator row is reserved.
		budget = m.columnContentHeight() - 1
	}

	m.columnOffsets[m.activeColumn] = max(offset, 0)
}

func (m *Model) createNewTicket() (tea.Model, tea.Cmd) {
	m.mode = ModeCreateTicket
	m.ticketFormField = formFieldTitle
	m.editingTicketID = ""
	m.branchLocked = false
	m.showAddProjectForm = false

	if len(m.filterProjectIDs) == 1 {
		for id := range m.filterProjectIDs {
			m.selectedProject = m.globalStore.GetProject(id)
			break
		}
	} else if m.selectedProject == nil {
		projects := m.globalStore.Projects()
		if len(projects) > 0 {
			m.selectedProject = projects[0]
		}
	}

	m.projectListIndex = 0
	if m.selectedProject != nil {
		for i, p := range m.globalStore.Projects() {
			if p.ID == m.selectedProject.ID {
				m.projectListIndex = i
				break
			}
		}
	}

	m.titleInput.Reset()
	m.descInput.Reset()
	m.branchInput.Reset()
	m.labelsInput.Reset()
	m.ticketPriority = 3
	m.ticketType = board.TypeFreeform
	m.ticketUseWorktree = true

	m.initBlockerCandidates("")
	m.selectedBlockers = make(map[board.TicketID]bool)
	m.blockerListIndex = 0
	m.blockerFilterInput.Reset()
	m.formScrollOffset = 0

	m.blurAllFormFields()
	m.titleInput.Focus()
	return m, m.titleInput.Cursor.BlinkCmd()
}

func (m *Model) editTicket() (tea.Model, tea.Cmd) {
	ticket := m.selectedTicket()
	if ticket == nil {
		m.notify("No ticket selected")
		return m, nil
	}

	m.mode = ModeEditTicket
	m.ticketFormField = formFieldTitle
	m.editingTicketID = ticket.ID
	m.branchLocked = ticket.WorktreePath != ""
	m.selectedProject = m.globalStore.GetProjectForTicket(ticket)
	m.projectListIndex = 0
	if m.selectedProject != nil {
		for i, p := range m.globalStore.Projects() {
			if p.ID == m.selectedProject.ID {
				m.projectListIndex = i
				break
			}
		}
	}
	m.showAddProjectForm = false
	m.titleInput.SetValue(ticket.Title)
	m.descInput.SetValue(ticket.Description)
	if ticket.BranchName != "" {
		m.branchInput.SetValue(ticket.BranchName)
	} else if m.selectedProject != nil {
		m.branchInput.SetValue(m.generateBranchNameFromTitle(ticket.Title, m.selectedProject))
	}
	m.labelsInput.SetValue(strings.Join(ticket.Labels, ", "))
	m.ticketPriority = ticket.Priority
	if m.ticketPriority < 1 || m.ticketPriority > 5 {
		m.ticketPriority = 3
	}
	m.ticketType = ticket.Type
	m.ticketUseWorktree = ticket.UseWorktree

	m.initBlockerCandidates(ticket.ID)
	m.selectedBlockers = make(map[board.TicketID]bool)
	for _, blockerID := range ticket.BlockedBy {
		m.selectedBlockers[blockerID] = true
	}
	m.blockerListIndex = 0
	m.blockerFilterInput.Reset()
	m.formScrollOffset = 0

	m.blurAllFormFields()
	m.titleInput.Focus()
	return m, m.titleInput.Cursor.BlinkCmd()
}

// attachExisting performs the daemon attach (or takeover, if another
// client owns the binary stream) for a PaneView the model already
// holds. Returns a tea.Cmd that runs the attach in the background and
// then arms the pane's tea message reader.
//
// Used by:
//   - spawnAgent (Enter/s on a daemon-owned ticket from board view)
//   - handleAgentViewMode (AttachFirstMsg fallback when the user types
//     into an unattached pane)
//   - Update's AttachFirstMsg routing.
func (m *Model) attachExisting(ticketID board.TicketID, pv *daemonclient.PaneView) tea.Cmd {
	return m.attachExistingSnap(ticketID, pv, nil)
}

// attachExistingSnap is attachExisting with an optional pre-fetched session
// list, used ONLY to Refresh the pane's SessionInfo without a redundant List
// (Auto mode passes the same snapshot it already fetched for its attached-
// elsewhere filter). The attach-vs-takeover decision is NOT made from the
// snapshot anymore — doAttach probes the daemon, which rejects an already-
// attached session (peer untouched) so we can warn before displacing it.
// Auto mode already skips attached-elsewhere sessions, so its probe normally
// succeeds; a race that lost the skip surfaces the warning, which is correct.
func (m *Model) attachExistingSnap(ticketID board.TicketID, pv *daemonclient.PaneView, sessions []daemon.SessionInfo) tea.Cmd {
	if pv == nil || m.daemonClient == nil {
		return nil
	}
	for _, s := range sessions {
		if s.SessionID == pv.SessionID() {
			pv.Refresh(s)
			break
		}
	}
	// Probe gently: a plain Attach. If another TUI holds the binary
	// stream the daemon rejects it (leaving that peer untouched) and
	// doAttach surfaces attachConflictMsg so we can warn before
	// displacing the other window.
	return m.doAttach(ticketID, pv, false)
}

// doAttach attaches pv to its daemon session. With takeover=false it
// probes gently — the daemon rejects an already-attached session with
// ErrAlreadyAttached (the existing attacher is NOT disturbed) and we
// return attachConflictMsg so the caller can warn the user before
// taking over. With takeover=true it unconditionally displaces the
// current attacher; this is used only after the user confirms the
// warning, so it must NOT re-probe (that would loop back into the
// prompt). Returns nil when there's nothing to attach.
func (m *Model) doAttach(ticketID board.TicketID, pv *daemonclient.PaneView, takeover bool) tea.Cmd {
	if pv == nil || m.daemonClient == nil {
		return nil
	}
	id := ticketID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var err error
		if takeover {
			err = pv.Takeover(ctx)
		} else {
			err = pv.Attach(ctx)
		}
		if err != nil {
			if !takeover && errors.Is(err, daemonclient.ErrAlreadyAttached) {
				// Record the error so the agent-view backdrop behind the
				// takeover modal renders the actionable overlay ("attached in
				// another TUI") instead of a blank pane. A successful Takeover
				// clears it automatically.
				pv.SetLastAttachErr(err)
				return attachConflictMsg{ticketID: id, pv: pv}
			}
			return spawnErrorMsg{ticketID: id, err: "attach failed: " + err.Error()}
		}
		// Drain one message from the pane's tea channel so the update
		// loop keeps spinning. Skip stale PaneDetachedMsg: detach()'s
		// WG goroutine emits one after the old attachLoop drains; if
		// it's still buffered when the next attach starts, returning it
		// triggers exitToBoard() and immediately undoes this attach.
		timeout := time.NewTimer(50 * time.Millisecond)
		defer timeout.Stop()
		for {
			select {
			case msg, ok := <-pv.TeaMessages():
				if !ok {
					return daemonclient.PaneExitMsg{PaneID: pv.ID(), Err: io.EOF}
				}
				if _, isDetach := msg.(daemonclient.PaneDetachedMsg); isDetach {
					continue
				}
				return msg
			case <-timeout.C:
				// No event yet — return a synthetic attached message so
				// the model arms the reader.
				return daemonclient.PaneAttachedMsg{PaneID: pv.ID()}
			}
		}
	}
}

func (m *Model) handleDoubleClick() (tea.Model, tea.Cmd) {
	// spawnAgent now dispatches to spawn vs attach based on the pane
	// state itself, so the double-click handler doesn't need to
	// pre-decide. Same behavior as 's' / Enter on the board view.
	return m.spawnAgent()
}

func (m *Model) confirmDeleteTicket() (tea.Model, tea.Cmd) {
	ticket := m.selectedTicket()
	if ticket == nil {
		return m, nil
	}

	proj := m.globalStore.GetProjectForTicket(ticket)
	hasUncommitted := false
	if ticket.WorktreePath != "" && m.config.Cleanup.DeleteWorktree && proj != nil {
		if mgr := m.worktreeMgrs[proj.ID]; mgr != nil {
			var err error
			hasUncommitted, err = mgr.HasUncommittedChanges(ticket.WorktreePath)
			if err != nil {
				hasUncommitted = false
			}
		}
	}

	// performTicketCleanup honors config.Cleanup.DeleteBranch silently.
	// When that flag is false (the default), the branch survives and we
	// chain a follow-on confirm so the user has an obvious moment to
	// drop it without editing config first. Capture proj here — the
	// store entry is gone by the time the second confirm fires.
	doDelete := func() tea.Cmd {
		// Capture the worktree's actual branch before cleanup removes the
		// worktree — it may differ from ticket.BranchName (a follow-up
		// branch the worktree was switched to).
		worktreeBranch := ""
		if !m.config.Cleanup.DeleteBranch && ticket.WorktreePath != "" && proj != nil {
			if mgr := m.worktreeMgrs[proj.ID]; mgr != nil {
				worktreeBranch, _ = mgr.BranchForWorktree(ticket.WorktreePath)
			}
		}
		m.performTicketCleanup(ticket)
		if !m.config.Cleanup.DeleteBranch && proj != nil {
			projID := proj.ID
			ticketBranch := ticket.BranchName
			divergent := ""
			if worktreeBranch != "" && worktreeBranch != ticketBranch {
				divergent = worktreeBranch
			}
			if ticketBranch != "" || divergent != "" {
				m.showConfirm = true
				m.confirmMsg = branchDeletePrompt(ticketBranch, divergent)
				m.confirmFn = func() tea.Cmd {
					if ticketBranch != "" {
						m.deleteBranchOnly(projID, ticketBranch)
					}
					m.deleteDivergentBranchOnly(projID, divergent, ticketBranch)
					return nil
				}
			}
		}
		return nil
	}

	if hasUncommitted && !m.config.Cleanup.ForceWorktreeRemoval {
		m.showConfirm = true
		m.confirmMsg = "Worktree has uncommitted changes. Force delete?"
		m.confirmFn = doDelete
	} else {
		m.showConfirm = true
		m.confirmMsg = "Delete ticket: " + ticket.Title + "?"
		m.confirmFn = doDelete
	}
	return m, nil
}

// deleteBranchOnly removes the branch via the project's worktree
// manager. Invoked from the follow-on confirm after a ticket delete
// when config.Cleanup.DeleteBranch is false. Tolerates a missing
// manager (project unloaded between confirms) by no-op'ing silently.
func (m *Model) deleteBranchOnly(projID string, branchName string) {
	mgr := m.worktreeMgrs[projID]
	if mgr == nil {
		return
	}
	if err := mgr.DeleteBranch(branchName); err != nil {
		m.notify("Failed to delete branch: " + err.Error())
		return
	}
	m.notify("Deleted branch: " + branchName)
}

// branchDeletePrompt builds the follow-on confirm message for the
// branches a ticket delete leaves behind: the ticket's recorded branch
// and, when the worktree was switched to a different one, that divergent
// branch too. Either may be empty.
func branchDeletePrompt(ticketBranch, divergent string) string {
	switch {
	case ticketBranch != "" && divergent != "":
		return "Also delete branches '" + ticketBranch + "' and '" + divergent + "'? [y/N]"
	case divergent != "":
		return "Also delete branch '" + divergent + "'? [y/N]"
	default:
		return "Also delete branch '" + ticketBranch + "'? [y/N]"
	}
}

// deleteDivergentBranchOnly cleans up a branch the worktree was checked
// out on when it differs from the ticket's recorded branch — the gap that
// otherwise orphans follow-up (fix/) branches, since cleanup only targets
// ticket.BranchName. Safe-deletes only (merged branches): an unmerged
// divergent branch is left in place and surfaced, never force-dropped,
// because openkanban didn't create it and can't assume it's disposable.
func (m *Model) deleteDivergentBranchOnly(projID, worktreeBranch, ticketBranch string) {
	if worktreeBranch == "" || worktreeBranch == ticketBranch {
		return
	}
	mgr := m.worktreeMgrs[projID]
	if mgr == nil {
		return
	}
	deleted, err := mgr.DeleteMergedBranch(worktreeBranch)
	switch {
	case err != nil:
		m.notify("Failed to delete branch " + worktreeBranch + ": " + err.Error())
	case deleted:
		m.notify("Deleted branch: " + worktreeBranch)
	default:
		m.notify("Branch " + worktreeBranch + " has unmerged commits — left in place")
	}
}

func (m *Model) performTicketCleanup(ticket *board.Ticket) {
	ticketTitle := ticket.Title // Capture before deletion

	if pane, ok := m.panes[ticket.ID]; ok {
		pane.Stop()
		delete(m.panes, ticket.ID)
	}

	// Unconditional daemon notify: when the local pane exists, pane.Stop
	// above already killed the writer process and handleTicketDone is a
	// no-op (returns Killed=false on miss). When the local pane does NOT
	// exist but the daemon owns a session (sibling-TUI window — the
	// 30s resync hasn't yet imported it), this is the rescue path that
	// keeps daemon-owned sessions from being orphaned by TUI ticket
	// deletion. Best-effort; failures logged but not surfaced — same
	// contract as wrapUpSessionForTicket.
	if m.daemon != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if _, err := m.daemon.TicketDone(ctx, string(ticket.ID)); err != nil {
			log.Printf("openkanban: TicketDone(%s) on ticket cleanup: %v", ticket.ID, err)
		}
		cancel()
	}

	proj := m.globalStore.GetProjectForTicket(ticket)
	if proj != nil {
		mgr := m.worktreeMgrs[proj.ID]
		if mgr != nil {
			// Capture the branch actually checked out in the worktree
			// BEFORE removing it. It can differ from ticket.BranchName when
			// the worktree was switched to a follow-up branch (e.g. a
			// post-merge fix/ branch) — without this it's orphaned, since
			// cleanup otherwise only knows the ticket's recorded branch.
			worktreeBranch := ""
			if m.config.Cleanup.DeleteBranch && ticket.WorktreePath != "" {
				worktreeBranch, _ = mgr.BranchForWorktree(ticket.WorktreePath)
			}

			if ticket.WorktreePath != "" && m.config.Cleanup.DeleteWorktree {
				err := mgr.RemoveWorktree(ticket.WorktreePath)
				if err != nil {
					m.notify("Failed to remove worktree: " + err.Error())
				}
			}

			if m.config.Cleanup.DeleteBranch {
				if ticket.BranchName != "" {
					if err := mgr.DeleteBranch(ticket.BranchName); err != nil {
						m.notify("Failed to delete branch: " + err.Error())
					}
				}
				m.deleteDivergentBranchOnly(proj.ID, worktreeBranch, ticket.BranchName)
			}
		}
	}

	// Every ticket conceptually OWNS its session now that forking is
	// eliminated (task/enforce-one-to-one-session). The pre-fix
	// SessionOwned gate distinguished link-mode (don't delete JSONL on
	// ticket-delete; the spawning agent owns it) from migrate-mode
	// (delete JSONL since the ticket owned it). With every spawn
	// migrate-on-resume, the ticket always owns the session; if it's
	// being deleted, the JSONL goes with it. The pane.Stop above has
	// already killed the writer process, so unlink is safe.
	if ticket.AgentSessionID != "" {
		path, err := agent.SessionPath(ticket.AgentSessionID)
		switch {
		case err == nil:
			if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
				m.notify("Failed to remove session file: " + rmErr.Error())
			}
		case !os.IsNotExist(err):
			m.notify("Failed to locate session file: " + err.Error())
		}
	}

	m.globalStore.RemoveBlockerReferences(ticket.ID)
	m.globalStore.Delete(ticket.ID)
	m.refreshColumnTickets()
	m.globalStore.SaveAll()
	m.notify("Deleted: " + ticketTitle)
}

func (m *Model) quickMoveTicket() (tea.Model, tea.Cmd) {
	ticket := m.selectedTicket()
	if ticket == nil {
		return m, nil
	}

	nextStatus := m.nextStatus(ticket.Status)
	if nextStatus == ticket.Status {
		return m, nil
	}

	if nextStatus == board.StatusInProgress && ticket.WorktreePath == "" {
		if ticket.UseWorktree {
			if err := m.setupWorktree(ticket); err != nil {
				m.notify("Worktree failed: " + err.Error())
				return m, nil
			}
		} else {
			if err := m.setupMainRepoBranch(ticket); err != nil {
				m.notify("Branch setup failed: " + err.Error())
				return m, nil
			}
		}
	}

	// Wrap up any live session BEFORE Move mutates ticket.Status —
	// the helper's pre-move-status check is what gates the teardown.
	// No-op when the ticket isn't leaving in_progress for a terminal.
	// The returned Cmd runs the daemon RPCs off the Update loop.
	wrapUpCmd := m.wrapUpSessionForTicket(ticket, nextStatus)

	promoted, pruned, _ := m.globalStore.Move(ticket.ID, nextStatus)
	m.refreshColumnTickets()
	m.selectTicketByID(ticket.ID)
	m.saveTicket(ticket)
	m.notify(moveAndPromoteMsg(nextStatus, promoted, pruned))

	return m, wrapUpCmd
}

// promoteAndSpawnUnattached implements ctrl+space (matched as "ctrl+@"): move
// the focused ticket to in_progress from ANY column (no-op move if already
// there) and launch its agent session UNATTACHED — the TUI stays on the board
// rather than switching to ModeAgentView. This is the move-forward +
// background-spawn combo; plain Space moves without spawning, and s/Enter
// spawns AND attaches.
//
// The ticket Move + column refresh run synchronously here (Update thread);
// only daemon I/O is deferred to the returned Cmd, per the rule that tea.Cmd
// goroutines must not touch m.globalStore / m.panes.
func (m *Model) promoteAndSpawnUnattached() (tea.Model, tea.Cmd) {
	ticket := m.selectedTicket()
	if ticket == nil {
		return m, nil
	}

	// Don't double-spawn: if this TUI already holds a pane for the ticket,
	// a session already exists (attached or unattached) — leave it be.
	if _, exists := m.panes[ticket.ID]; exists {
		m.notify("Agent already running for this ticket")
		return m, nil
	}

	// Resolve project + pinned agent BEFORE any promotion side effects, so an
	// unpinned project (or an unknown agent) refuses cleanly without creating a
	// worktree or moving the ticket out of its column. Replicated from spawnAgent
	// (rather than extracted) to keep zero blast radius on that heavily-tested path.
	proj := m.globalStore.GetProjectForTicket(ticket)
	if proj == nil {
		m.notify("Project not found for this ticket")
		return m, nil
	}
	// Worktree-exclusivity safety gate: refuse to start a second live agent in
	// a worktree another ticket already occupies (create a fresh worktree or
	// wait). See worktree_gate.go / workflow.WorktreeConflict.
	if occ := m.worktreeOccupiedByOther(ticket); occ != nil {
		m.notify("Worktree busy: \"" + occ.Title + "\" is live here — use a new worktree or wait")
		return m, nil
	}
	if !ticket.UseWorktree {
		for otherID := range m.panes {
			if otherID == ticket.ID {
				continue
			}
			other, _ := m.globalStore.Get(otherID)
			if other != nil && !other.UseWorktree {
				otherProj := m.globalStore.GetProjectForTicket(other)
				if otherProj != nil && otherProj.ID == proj.ID {
					m.notify("Another main-repo agent is running in this project")
					return m, nil
				}
			}
		}
	}
	agentType, agentErr := m.resolveSpawnAgent(ticket, proj)
	if agentErr != nil {
		m.notify(noProjectAgentMsg)
		return m, nil
	}
	agentCfg, ok := m.config.Agents[agentType]
	if !ok {
		m.notify("Agent '" + agentType + "' not configured")
		return m, nil
	}
	if agentType == "opencode" {
		_ = m.opencodeServer.Start() // best effort
	}

	// Workflow prerequisite offer (overridable): refuse to START an
	// implement/review whose upstream isn't ready, before the promotion Move
	// and spawn. Sits after the worktree-exclusivity SAFETY gate above.
	if !m.startGateAllows(ticket, m.promoteAndSpawnUnattached) {
		return m, nil
	}

	// Promote into in_progress from any column (no-op when already there).
	if ticket.Status != board.StatusInProgress {
		if ticket.WorktreePath == "" {
			if ticket.UseWorktree {
				if err := m.setupWorktree(ticket); err != nil {
					m.notify("Worktree failed: " + err.Error())
					return m, nil
				}
			} else {
				if err := m.setupMainRepoBranch(ticket); err != nil {
					m.notify("Branch setup failed: " + err.Error())
					return m, nil
				}
			}
		}
		m.globalStore.Move(ticket.ID, board.StatusInProgress)
		m.refreshColumnTickets()
		m.selectTicketByID(ticket.ID)
		m.saveTicket(ticket)
	}

	// Persist the resolved agent type so status detection (and a later
	// attach) see it. The attached spawn path stamps this in its
	// spawnReadyMsg handler (via m.spawningAgent); the unattached path has
	// no ModeSpawning bookkeeping, so stamp it here in the Update thread.
	if ticket.AgentType != agentType {
		ticket.AgentType = agentType
		m.saveTicket(ticket)
	}

	// Spawn unattached — stay on the board (no ModeSpawning, no spinner).
	return m, m.prepareSpawnWith(ticket, proj, agentCfg, spawnPlan{Unattached: true})
}

// adjustPriority shifts the active ticket's priority by delta (negative
// = raise, positive = lower) within the valid 1..5 range. Priority 1 is
// the highest, so "raise" maps to a smaller number. The selected ticket
// stays selected after the column rebuilds even when a priority sort
// would otherwise drift it under the cursor.
func (m *Model) adjustPriority(delta int) (tea.Model, tea.Cmd) {
	ticket := m.selectedTicket()
	if ticket == nil {
		m.notify("No ticket selected")
		return m, nil
	}

	current := effectivePriority(ticket.Priority)
	next := current + delta
	if next < 1 {
		m.notify("Already at highest priority")
		return m, nil
	}
	if next > 5 {
		m.notify("Already at lowest priority")
		return m, nil
	}

	ticket.Priority = next
	ticket.Touch()
	m.saveTicket(ticket)
	m.refreshColumnTickets()
	m.selectTicketByID(ticket.ID)

	if delta < 0 {
		m.notify(fmt.Sprintf("Priority raised to %d", next))
	} else {
		m.notify(fmt.Sprintf("Priority lowered to %d", next))
	}
	return m, nil
}

// cycleUnattachedSession moves the agent-view focus to the next
// (delta=+1) or previous (delta=-1) open session in board order.
//
// "Open" means the local PaneView is in Attached or Unattached state
// (the daemon owns a live session for it). PaneViewDetached panes are
// excluded — they're not open in any meaningful sense. The currently-
// focused pane is excluded so the user actually moves; landing back
// on yourself wouldn't be a cycle.
//
// On arrival at an Unattached peer, the helper auto-attaches via
// `attachExisting` so the modal's backdrop populates with live session
// content as soon as the snapshot arrives. The attach is async; the
// modal renders synchronously on a blank pane for the first frame,
// then the PaneAttachedMsg triggers a re-render with content. When
// cycling to an already-Attached peer (a previously-cycled-through
// pane, or the originally-attached pane on wrap-around) no new attach
// is needed; the pane already holds the live snapshot.
//
// The function name predates auto-attach (the original ship was
// strictly Unattached-only — see [[openkanban-cycle-unattached-sessions-decisions]]
// for the history); kept as-is to preserve git blame continuity. The
// caller-visible semantics are now "cycle through all open peers."
//
// Sets cycleAttachPrompt so View dispatches to
// renderAgentViewWithCycleModal — the modal absorbs the user's
// switch-to-session keystroke so it doesn't get eaten by the
// AttachFirstMsg handshake (only relevant for the first frame before
// the auto-attach completes).
func (m *Model) cycleUnattachedSession(delta int) (tea.Model, tea.Cmd) {
	if delta != 1 && delta != -1 {
		return m, nil
	}

	// Build the ordered list of all open peers in board order, including
	// the focused pane. Excluding the focused pane up front would make
	// the wrap-around math (cur+delta) ambiguous when the focused pane
	// sits between two candidates: "first after me" needs me-in-the-list
	// to be defined. Detached panes are excluded — they have no daemon
	// session and pane.View() would be blank no matter what.
	var allOpen []board.TicketID
	for _, col := range m.columnTickets {
		for _, t := range col {
			pv, ok := m.panes[t.ID]
			if !ok || pv == nil {
				continue
			}
			switch pv.State() {
			case daemonclient.PaneViewAttached, daemonclient.PaneViewUnattached:
				allOpen = append(allOpen, t.ID)
			}
		}
	}

	cur := -1
	for i, id := range allOpen {
		if id == m.focusedPane {
			cur = i
			break
		}
	}
	// Count peers that aren't the focused pane. If the focused pane is
	// in allOpen we subtract it; if it isn't (we're focused on a
	// Detached pane or a ticket with no session at all) every entry is
	// a peer. Zero peers → nothing to cycle to.
	otherPeers := len(allOpen)
	if cur != -1 {
		otherPeers--
	}
	if otherPeers == 0 {
		m.notify("No other open sessions")
		return m, nil
	}
	var next board.TicketID
	if cur == -1 {
		// Focused pane isn't in the open set (unusual — means we're
		// focused on a Detached pane, e.g. daemon went away). Pick the
		// nearest end in the requested direction.
		if delta > 0 {
			next = allOpen[0]
		} else {
			next = allOpen[len(allOpen)-1]
		}
	} else {
		next = allOpen[(cur+delta+len(allOpen))%len(allOpen)]
	}

	return m, m.focusAndPromptAttach(next)
}

// focusAndPromptAttach focuses the target peer pane, opens the cycle-attach
// modal over it, and — if the pane is Unattached — kicks off the attach so
// the modal backdrop shows live PTY content instead of bare chrome.
//
// Shared by Ctrl+]/Ctrl+\ cycling (cycleUnattachedSession) and Auto mode's
// oldest-waiter jump (the ExitFocusMsg branch in handleAgentViewMode) so the
// SetSize(-2) sizing and the cycleAttachPrompt handshake can't drift between
// the two entry points. Callers build target from m.panes; the defensive
// pv==nil bail still sets the modal flag so the user sees the prompt with
// the bare ticket title if the map raced out from under us.
func (m *Model) focusAndPromptAttach(target board.TicketID) tea.Cmd {
	return m.focusAndPromptAttachSnap(target, nil)
}

// focusAndPromptAttachSnap is focusAndPromptAttach with an optional pre-fetched
// session list threaded into the attach (see attachExistingSnap). Auto mode
// passes the snapshot it already Listed; cycling passes nil (List as before).
func (m *Model) focusAndPromptAttachSnap(target board.TicketID, sessions []daemon.SessionInfo) tea.Cmd {
	m.focusedPane = target
	pv, ok := m.panes[target]
	if !ok || pv == nil {
		m.cycleAttachPrompt = true
		return m.maybeSetWindowTitle()
	}
	pv.SetSize(m.width, m.height-2)
	m.cycleAttachPrompt = true

	// Peek the target so the modal backdrop shows its live content WITHOUT
	// attaching. Cycling / Auto-mode preview is not a commitment — a plain
	// attach here would silently take the session over from another TUI just
	// to render a backdrop, which the takeover-warning work prevents. The
	// user commits explicitly with Enter, which routes through attachExisting
	// → the takeover warning if the session is held elsewhere. Already-
	// Attached peers already have a live vt; only Peek the unattached ones.
	// (sessions is no longer threaded into the attach — Peek needs only the
	// pane; Auto mode still uses its snapshot for the attached-elsewhere skip.)
	_ = sessions
	var backdropCmd tea.Cmd
	if pv.State() == daemonclient.PaneViewUnattached {
		peekPV := pv
		backdropCmd = func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := peekPV.Peek(ctx); err != nil {
				log.Printf("openkanban model: cycle peek failed session=%s: %v", peekPV.SessionID(), err)
			}
			// Trigger a re-render so the freshly-peeked vt is shown. A
			// dedicated no-op message (not PaneOutputMsg) so we don't arm
			// a pane-message listener on an unattached pane.
			return cyclePeekedMsg{}
		}
	}
	return tea.Batch(backdropCmd, m.maybeSetWindowTitle())
}

// needsAttention reports whether an agent status means the session needs the
// user: waiting (blocked on input/permission), idle (agent finished its turn
// and is at rest), or stuck (the daemon watchdog's "pane wedged" verdict — the
// single most attention-needing live state). It deliberately excludes
// "working" — a session actively producing output doesn't need you yet — which
// in a busy swarm is the common case, so targeting only "waiting" left Auto
// with almost nothing to jump to. Also excludes none/completed/error (unknown
// or terminal, not a live "come help me" signal). The activity override only
// refines waiting→working; idle/stuck pass through untouched.
//
// Deliberately also excludes AgentSubagents: a foreground agent awaiting its
// own background sub-agents is idle-but-occupied and needs nothing from the
// user, so Auto-mode must NOT jump to it. (Before that status existed the
// same session was classified AgentWaiting and Auto DID jump to it — the bug
// this status fixes.) It shares idle's muted color but NOT idle's needs-you
// semantics; when the sub-agents finish, detection re-derives a real status
// (working/idle/waiting) and this picks it up then. Do not add it here.
func needsAttention(s board.AgentStatus) bool {
	return s == board.AgentWaiting || s == board.AgentIdle || s == board.AgentStuck
}

// oldestWaitingPeer returns the open peer session that has NEEDED ATTENTION the
// longest (FIFO by StatusChangedAt), for Auto mode's un-attach jump. "Needs
// attention" = waiting OR idle (see needsAttention) — not actively working. A
// ticket qualifies iff it has a live pane (Attached/Unattached), its
// activity-overridden AgentStatus needs attention (the poll writes the
// overridden value back onto ticket.AgentStatus, so this matches what the
// card renders), it is not the session being left (m.focusedPane — else
// Auto would re-attach you to the one you just left), it has a non-nil
// StatusChangedAt, and no other TUI client holds it (attachedElsewhere,
// built by the caller from a List snapshot — skipping these avoids
// taking a session away from a sibling TUI). Ties on StatusChangedAt
// resolve to board order (m.columnTickets walk order). Returns ok=false
// when nothing qualifies, which the caller treats as "fall through to the
// board" — the always-available off-ramp.
func (m *Model) oldestWaitingPeer(attachedElsewhere map[board.TicketID]bool) (board.TicketID, bool) {
	var best board.TicketID
	var bestTime time.Time
	found := false
	for _, col := range m.columnTickets {
		for _, t := range col {
			if t == nil || t.ID == m.focusedPane {
				continue
			}
			if attachedElsewhere[t.ID] {
				continue
			}
			// AgentStatus here is the activity-overridden value the poll
			// writes back (model.go agentStatusResultMsg handler), so this
			// matches what the card renders. columnTickets holds the same
			// *board.Ticket the store mutates, so no globalStore lookup.
			if !needsAttention(t.AgentStatus) || t.StatusChangedAt == nil {
				continue
			}
			pv, ok := m.panes[t.ID]
			if !ok || pv == nil {
				continue
			}
			switch pv.State() {
			case daemonclient.PaneViewAttached, daemonclient.PaneViewUnattached:
			default:
				continue
			}
			if !found || t.StatusChangedAt.Before(bestTime) {
				best = t.ID
				bestTime = *t.StatusChangedAt
				found = true
			}
		}
	}
	return best, found
}

// liveSessions returns a single daemon List snapshot (1s timeout), or nil
// when the daemon is unreachable. Auto mode fetches it once and threads it
// into both attachedElsewhereSet (the skip filter) and the attach
// (attachExistingSnap), so a jump does one List rather than two.
func (m *Model) liveSessions() []daemon.SessionInfo {
	if m.daemonClient == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	resp, err := m.daemonClient.List(ctx)
	if err != nil {
		return nil
	}
	return resp.Sessions
}

// attachedElsewhereSet returns the set of tickets whose daemon session is held
// by a client other than myClientID. Pure (no I/O) so it's unit-testable;
// Auto mode uses it to skip sibling-TUI sessions so a jump never displaces
// another viewer.
func attachedElsewhereSet(sessions []daemon.SessionInfo, myClientID uint16) map[board.TicketID]bool {
	out := make(map[board.TicketID]bool)
	for _, s := range sessions {
		if s.AttachedClient != 0 && s.AttachedClient != myClientID {
			out[board.TicketID(s.TicketID)] = true
		}
	}
	return out
}

// cycleSortMode advances to the next sort mode and re-renders the board.
// The currently-selected ticket stays selected so the cursor doesn't
// jump to whatever happens to land at its old index after sorting.
func (m *Model) cycleSortMode() (tea.Model, tea.Cmd) {
	ticket := m.selectedTicket()
	m.sortMode = nextSortMode(m.sortMode)
	m.refreshColumnTickets()
	if ticket != nil {
		m.selectTicketByID(ticket.ID)
	}
	m.notify("Sort: " + sortModeLabel(m.sortMode))
	return m, nil
}

// cycleSessionFilter toggles the session filter (all ⇄ open) and
// re-renders the board. Preserves selection like cycleSortMode; the
// selected ticket may scroll off-screen if it no longer matches the
// active filter, but its identity is retained so a subsequent toggle
// back to "all" restores the cursor where it was.
func (m *Model) cycleSessionFilter() (tea.Model, tea.Cmd) {
	ticket := m.selectedTicket()
	m.sessionFilter = nextSessionFilter(m.sessionFilter)
	m.refreshColumnTickets()
	if ticket != nil {
		m.selectTicketByID(ticket.ID)
	}
	m.notify("Filter: " + sessionFilterLabel(m.sessionFilter))
	return m, nil
}

func (m *Model) toggleAlwaysShowWorking() (tea.Model, tea.Cmd) {
	ticket := m.selectedTicket()
	m.alwaysShowWorking = !m.alwaysShowWorking
	m.refreshColumnTickets()
	if ticket != nil {
		m.selectTicketByID(ticket.ID)
	}
	state := "off"
	if m.alwaysShowWorking {
		state = "on"
	}
	m.notify("Always show working: " + state)
	return m, nil
}

// toggleAutoAttach flips Auto mode (board key 'a'). See the autoAttach
// field doc and oldestWaitingPeer for the behavior it gates.
func (m *Model) toggleAutoAttach() (tea.Model, tea.Cmd) {
	m.autoAttach = !m.autoAttach
	state := "off"
	if m.autoAttach {
		state = "on"
	}
	m.notify("Auto-attach: " + state)
	return m, nil
}

func (m *Model) quickMoveTicketBackward() (tea.Model, tea.Cmd) {
	ticket := m.selectedTicket()
	if ticket == nil {
		return m, nil
	}

	prevStatus := m.previousStatus(ticket.Status)
	if prevStatus == ticket.Status {
		return m, nil
	}

	// Symmetric with the forward path: if the user is demoting back
	// through a terminal (rare — only fires from done→in_review since
	// previousStatus(in_review) is in_progress, not "leaving" it), the
	// helper's pre-condition keeps this a no-op. The call is harmless
	// here but kept for parity with quickMoveTicket so a future change
	// to status ordering doesn't silently introduce an asymmetry.
	wrapUpCmd := m.wrapUpSessionForTicket(ticket, prevStatus)

	promoted, pruned, _ := m.globalStore.Move(ticket.ID, prevStatus)
	m.refreshColumnTickets()
	m.selectTicketByID(ticket.ID)
	m.saveTicket(ticket)
	m.notify(moveAndPromoteMsg(prevStatus, promoted, pruned))

	return m, wrapUpCmd
}

func (m *Model) setupWorktree(ticket *board.Ticket) error {
	proj := m.globalStore.GetProjectForTicket(ticket)
	if proj == nil {
		return fmt.Errorf("project not found for ticket")
	}

	mgr := m.worktreeMgrs[proj.ID]
	if mgr == nil {
		return fmt.Errorf("worktree manager not found")
	}

	branchName := m.generateBranchName(ticket, proj)
	baseBranch, _ := mgr.GetDefaultBranch()

	path, err := mgr.CreateWorktree(branchName, baseBranch)
	if err != nil {
		return err
	}

	if err := agent.SeedClaudeSettings(path, proj.RepoPath); err != nil {
		log.Printf("openkanban: seed claude settings (%s): %v", path, err)
	}

	ticket.WorktreePath = path
	ticket.BranchName = branchName
	ticket.BaseBranch = baseBranch
	return nil
}

func (m *Model) setupMainRepoBranch(ticket *board.Ticket) error {
	proj := m.globalStore.GetProjectForTicket(ticket)
	if proj == nil {
		return fmt.Errorf("project not found for ticket")
	}

	mgr := m.worktreeMgrs[proj.ID]
	if mgr == nil {
		return fmt.Errorf("worktree manager not found")
	}

	branchName := m.generateBranchName(ticket, proj)
	baseBranch, _ := mgr.GetDefaultBranch()

	ticket.WorktreePath = proj.RepoPath
	ticket.BranchName = branchName
	ticket.BaseBranch = baseBranch
	return nil
}

func (m *Model) generateBranchNameFromTitle(title string, proj *project.Project) string {
	return project.BranchNameForTitle(title, proj, m.config.Defaults)
}

func (m *Model) generateBranchName(ticket *board.Ticket, proj *project.Project) string {
	if ticket.BranchName != "" {
		return ticket.BranchName
	}
	return m.generateBranchNameFromTitle(ticket.Title, proj)
}

func (m *Model) allocateAgentPort() int {
	usedPorts := make(map[int]bool)
	for _, t := range m.globalStore.All() {
		if t.AgentPort > 0 {
			usedPorts[t.AgentPort] = true
		}
	}

	port := agentPortBase
	for usedPorts[port] {
		port++
	}
	return port
}

// ownsProbeTimeout is the cap we put on the daemon Owns RPC during the
// spawn-path dead-session gate. The probe is best-effort — if the
// daemon is slow or unreachable we fall back to the on-disk dead-check.
// Keep this small: the user just pressed Enter on a ticket and is
// waiting for the spawn flow to proceed.
const ownsProbeTimeout = 500 * time.Millisecond

// shouldCleanupDeadSession decides whether spawnAgent should fire the
// IsClaudeSessionDead / DeleteClaudeSession cleanup for ticket's prior
// session. Returns (cleanup, deadJSONLPath).
//
// The decision tree is:
//  1. If the daemon owns a live PTY for ticket.AgentSessionID, return
//     (false, "") — never delete the JSONL of a session the daemon is
//     actively writing. The on-disk transcript may look "dead" because
//     the assistant hasn't replied yet; deleting it would break a
//     future `--continue`.
//  2. Otherwise, fall through to agent.IsClaudeSessionDead and report
//     its verdict (and the JSONL path so the caller can unlink it).
//
// The Owns probe is bounded by ownsProbeTimeout; on timeout / RPC
// error we conservatively fall through to the disk check (and log so
// the timeout is visible).
func (m *Model) shouldCleanupDeadSession(ticket *board.Ticket) (bool, string) {
	if ticket == nil {
		return false, ""
	}
	if m.daemon != nil && ticket.AgentSessionID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), ownsProbeTimeout)
		resp, err := m.daemon.Owns(ctx, ticket.AgentSessionID)
		cancel()
		switch {
		case err == nil && resp.Owned:
			// Daemon owns the live session — skip the dead-session
			// cleanup entirely. The wouldChange/modal path in
			// spawnAgent still runs.
			return false, ""
		case err != nil:
			// Probe failed (timeout, connection refused, etc.). Log
			// and fall through to the on-disk check — refusing to
			// spawn because we couldn't reach the daemon would be
			// strictly worse than the rare edge case of cleaning up
			// a session whose ownership we couldn't confirm.
			log.Printf("openkanban: spawn-gate Owns(%s) failed: %v", ticket.AgentSessionID, err)
		}
	}
	dead, deadPath, _ := agent.IsClaudeSessionDead(ticket.WorktreePath)
	return dead, deadPath
}

// spawnAgent is the single entry point for the "open this ticket's
// agent" action: both 's' and Enter on the board view route here, as
// does double-click. It dispatches based on the current pane state:
//
//   - no pane / PaneViewDetached  → spawn a fresh session
//   - PaneViewUnattached          → attach to the daemon-owned session
//   - PaneViewAttached            → just switch to the agent view
//
// The pre-consolidation behavior split this between spawnAgent and
// attachToAgent and produced "press the OTHER key" bounce
// notifications when the user pressed the wrong one for the current
// state.
func (m *Model) spawnAgent() (tea.Model, tea.Cmd) {
	ticket := m.selectedTicket()
	if ticket == nil {
		m.notify("No ticket selected")
		return m, nil
	}

	if existing, exists := m.panes[ticket.ID]; exists {
		// Refresh the pane's cached title on every re-focus so a later
		// store drop still has a current title to fall back to.
		existing.SetTicketTitle(ticket.Title)
		switch existing.State() {
		case daemonclient.PaneViewAttached:
			// Already attached in this TUI — just switch to its view.
			m.mode = ModeAgentView
			m.focusedPane = ticket.ID
			existing.SetSize(m.width, m.height-2)
			return m, m.maybeSetWindowTitle()
		case daemonclient.PaneViewUnattached:
			// Daemon owns it (likely from a prior TUI run or sibling
			// instance). Re-attach instead of spawning a duplicate.
			m.mode = ModeAgentView
			m.focusedPane = ticket.ID
			existing.SetSize(m.width, m.height-2)
			cmd := m.attachExisting(ticket.ID, existing)
			return m, tea.Batch(cmd, m.maybeSetWindowTitle())
		case daemonclient.PaneViewDetached:
			// Local view lost its binary stream, but the daemon may
			// still own the session. Try to re-attach before spawning
			// — historically this branch fell through to spawn, which
			// PR #34's idempotent Spawn made harmless but still wasted
			// an RPC roundtrip and a daemon-side fork attempt. If the
			// pane has a SessionID we can target, kick off an
			// attachExisting cmd; the async attach result will either
			// land us in ModeAgentView cleanly or surface a
			// spawnErrorMsg ("attach failed: ...") the user can react
			// to manually. Falling through to spawn on attach failure
			// would be the older, more aggressive behaviour; we
			// deliberately don't, because the daemon may genuinely own
			// the session and a fresh Spawn (even idempotent) would be
			// surprising.
			//
			// If the pane has no SessionID (purely synthetic, no daemon
			// session ever existed for it), there is nothing to attach
			// to — fall through to the spawn path.
			if existing.SessionID() != "" {
				m.mode = ModeAgentView
				m.focusedPane = ticket.ID
				existing.SetSize(m.width, m.height-2)
				cmd := m.attachExisting(ticket.ID, existing)
				return m, tea.Batch(cmd, m.maybeSetWindowTitle())
			}
		}
	}

	// From here on we are spawning fresh, which requires the ticket to
	// actually be ready to work on. Putting the in-progress check after
	// the attach branches lets Enter/s on an already-running session
	// reach the view without the user first having to clear this gate.
	if ticket.Status != board.StatusInProgress {
		m.notify("Press Space to move to In Progress first")
		return m, nil
	}

	proj := m.globalStore.GetProjectForTicket(ticket)
	if proj == nil {
		m.notify("Project not found for this ticket")
		return m, nil
	}

	// Worktree-exclusivity safety gate: refuse to start a second live agent in
	// a worktree another ticket already occupies (create a fresh worktree or
	// wait). See worktree_gate.go / workflow.WorktreeConflict.
	if occ := m.worktreeOccupiedByOther(ticket); occ != nil {
		m.notify("Worktree busy: \"" + occ.Title + "\" is live here — use a new worktree or wait")
		return m, nil
	}

	if !ticket.UseWorktree {
		for otherID := range m.panes {
			if otherID == ticket.ID {
				continue
			}
			other, _ := m.globalStore.Get(otherID)
			if other != nil && !other.UseWorktree {
				otherProj := m.globalStore.GetProjectForTicket(other)
				if otherProj != nil && otherProj.ID == proj.ID {
					m.notify("Another main-repo agent is running in this project")
					return m, nil
				}
			}
		}
	}

	agentType, agentErr := m.resolveSpawnAgent(ticket, proj)
	if agentErr != nil {
		m.notify(noProjectAgentMsg)
		return m, nil
	}
	agentCfg, ok := m.config.Agents[agentType]
	if !ok {
		m.notify("Agent '" + agentType + "' not configured")
		return m, nil
	}

	// Workflow prerequisite offer (overridable): refuse to START an
	// implement/review whose upstream isn't ready. Sits after the
	// worktree-exclusivity SAFETY gate above, before the spawn.
	if !m.startGateAllows(ticket, m.spawnAgent) {
		return m, nil
	}

	// Start opencode server on-demand if spawning opencode agent
	if agentType == "opencode" {
		_ = m.opencodeServer.Start() // Best effort, ignore errors
	}

	// Stale-brief detection (claude only): if there's a prior journal
	// the agent could resume into AND merging the openkanban card's
	// description into the on-disk brief would change it, ask the user
	// how to proceed before transitioning to ModeSpawning.
	//
	// Two resume paths qualify:
	//   1. Regular re-spawn (AgentSpawnedAt != nil) — this TUI spawned
	//      the prior session itself. Includes the dead-session cleanup
	//      step that reaps abandoned journals before the chooser would
	//      otherwise fire on them.
	//   2. External resume (AgentSpawnedAt == nil, AgentSessionID set
	//      to a valid UUID via `openkanban ticket new --session <uuid>`
	//      or the status-poll back-fill) — a journal from an externally
	//      created or back-filled session exists; we DON'T cleanup-on-
	//      dead here because the journal isn't ours to delete.
	if agentType == "claude" {
		offerChooser := false

		if ticket.AgentSpawnedAt != nil {
			// T3: Dead-session auto-cleanup is gated by daemon ownership.
			// If the daemon currently owns the live PTY for this session
			// UUID, the on-disk JSONL may legitimately look "dead" (no
			// assistant content yet, mid-write) while the runtime session
			// is fine. Deleting the JSONL in that case would break a
			// future `--continue`. shouldCleanupDeadSession encapsulates
			// the Owns probe + IsClaudeSessionDead decision.
			shouldCleanup, deadPath := m.shouldCleanupDeadSession(ticket)
			if shouldCleanup {
				if deadPath != "" {
					_ = agent.DeleteClaudeSession(deadPath)
				}
				ticket.AgentSpawnedAt = nil
				m.saveTicket(ticket)
				// fall through to the empty-plan spawn path below
			} else {
				offerChooser = true
			}
		} else if agent.SessionUUIDPattern.MatchString(ticket.AgentSessionID) {
			// External resume: the chooser is only meaningful if a journal
			// exists on disk — otherwise there's no prior context for the
			// brief-change to matter against. FindClaudeSession returns ""
			// when the journal is missing or never engaged (no real
			// assistant turn), which is the right early-exit.
			if agent.FindClaudeSession(ticket.WorktreePath) != "" {
				offerChooser = true
			}
		}

		if offerChooser {
			_, _, wouldChange, _, _ := agent.PreviewBriefMerge(ticket, ticket.WorktreePath)
			// The chooser fires ONLY when the card description has
			// diverged from the on-disk brief since the session was
			// last active (wouldChange). The on-disk brief is the
			// snapshot written at the last merge/spawn, so wouldChange
			// is true exactly when the user edited the card after the
			// session started — the only moment "resume, re-read, or
			// start fresh?" is worth asking.
			//
			// It deliberately does NOT key off StatusChangedAt vs
			// AgentSpawnedAt. SetAgentStatus stamps StatusChangedAt on
			// every working↔waiting flip (internal/board/board.go), so
			// any old session that did any work has StatusChangedAt >
			// AgentSpawnedAt — a status-based gate fired the chooser on
			// every re-spawn regardless of whether anything changed.
			// See the "Brief-change chooser" note in internal/ui/CLAUDE.md.
			if wouldChange {
				// Capture ticket/proj/agentCfg into each callback. Each option
				// sets its own plan and proceeds with the existing tea.Batch.
				m.showChoice = true
				m.choiceMsg = "Brief was updated since this session started. What should I do?"
				ticketCopy := ticket // pointer — fine, the closures don't outlive the ticket
				projCopy := proj
				cfgCopy := agentCfg
				m.choices = []choiceItem{
					{
						Key:   'd',
						Label: "Discard prior session, start fresh",
						Fn: func() tea.Cmd {
							ticketCopy.AgentSpawnedAt = nil
							m.saveTicket(ticketCopy)
							m.mode = ModeSpawning
							m.spawningTicketID = ticketCopy.ID
							m.spawningAgent = agentType
							return tea.Batch(m.spinner.Tick, m.prepareSpawnWith(ticketCopy, projCopy, cfgCopy, spawnPlan{ForceFresh: true}))
						},
					},
					{
						Key:   'u',
						Label: "Resume; tell agent the brief changed",
						Fn: func() tea.Cmd {
							m.mode = ModeSpawning
							m.spawningTicketID = ticketCopy.ID
							m.spawningAgent = agentType
							return tea.Batch(m.spinner.Tick, m.prepareSpawnWith(ticketCopy, projCopy, cfgCopy, spawnPlan{InjectResumeNotice: true}))
						},
					},
					{
						Key:   'n',
						Label: "Resume; leave brief unchanged",
						Fn: func() tea.Cmd {
							m.mode = ModeSpawning
							m.spawningTicketID = ticketCopy.ID
							m.spawningAgent = agentType
							return tea.Batch(m.spinner.Tick, m.prepareSpawnWith(ticketCopy, projCopy, cfgCopy, spawnPlan{SkipMerge: true}))
						},
					},
				}
				return m, nil
			}
		}
	}

	m.mode = ModeSpawning
	m.spawningTicketID = ticket.ID
	m.spawningAgent = agentType

	return m, tea.Batch(m.spinner.Tick, m.prepareSpawnWith(ticket, proj, agentCfg, spawnPlan{}))
}

// resolveBrief decides what to do with the in-repo brief file at
// tickets/<slug>.md based on the spawnPlan:
//
//   - SkipMerge=false (default / ForceFresh / InjectResumeNotice):
//     call agent.MergeTicketBrief which writes the card description
//     into the brief file (atomic rename).
//   - SkipMerge=true: leave the file's bytes untouched, but still stat
//     it so the prompt template's {{if .HasBrief}} branch behaves
//     correctly.
//
// Returns the worktree-relative path (or "" if no brief), whether the
// file exists on disk after the operation, and any merge error.
// Extracted as a separate function so the SkipMerge "file bytes
// preserved" property can be unit-tested in isolation from the rest
// of prepareSpawnWith.
func resolveBrief(ticket *board.Ticket, worktreePath string, plan spawnPlan) (string, bool, error) {
	if !plan.SkipMerge {
		return agent.MergeTicketBrief(ticket, worktreePath)
	}
	slug := agent.BranchSlug(ticket.BranchName)
	if slug == "" || worktreePath == "" {
		return "", false, nil
	}
	rel := "tickets/" + slug + ".md"
	full := filepath.Join(worktreePath, "tickets", slug+".md")
	if _, statErr := os.Stat(full); statErr != nil {
		return "", false, nil
	}
	return rel, true, nil
}

// spawnReqInputs collects the resolved inputs needed to construct a
// daemon.SpawnReq. All fields are values (no Model receiver, no live
// filesystem lookups) so buildSpawnReq below is a pure function and
// can be unit-tested per spawnPlan branch in isolation.
//
// Callers (today: prepareSpawnWith's closure) are responsible for
// performing the filesystem I/O — agent.FindOpencodeSession,
// agent.MergeTicketBrief, etc. — and passing the resolved values in.
// That separation keeps the SpawnReq shape decisions in one tested
// place; the I/O side-effects live in the closure.
type spawnReqInputs struct {
	ticket         *board.Ticket
	plan           spawnPlan
	sessionName    string
	command        string
	workdir        string
	cols           int
	rows           int
	agentType      string
	agentEnv       map[string]string // per-agent Env (config.AgentConfig.Env), injected at spawn with leading "~/" expanded
	cleanArgs      []string          // agentCfg.Args with empty entries stripped
	model          string            // resolved from proj.Settings.Model (trimmed); empty = no --model flag
	isNewSession   bool
	promptTemplate string
	ctxData        agent.ContextData
	agentPort      int
	// Session IDs resolved by the caller via agent.Find{Opencode,Gemini,Codex}Session.
	// Empty when the corresponding session-file isn't present on disk.
	opencodeSessionID string
	geminiSessionID   string
	codexSessionID    string
	// forwardNotifications is the effective config.Behavior.ForwardAgentNotifications
	// value at spawn time. Threaded through SpawnReq so the daemon's
	// terminal.Pane gates its OSC 9 → desktop-notification handler on
	// this per session, rather than relying on a process-wide global.
	forwardNotifications bool
	// scrollback is config.UI.ScrollbackLines at spawn time — sizes the native
	// vt emulator on the daemon side (and propagated client-side via SessionInfo).
	scrollback int
}

// noProjectAgentMsg is shown when a spawn is attempted in a project with no
// pinned agent. Agent identity is chosen at the project level (sidebar 'g');
// there is no global fallback by design — this guards against accidentally
// launching the wrong agent in an unpinned project.
const noProjectAgentMsg = "Pin a Claude for this project first — press g in the sidebar"

// errNoProjectAgent signals a spawn was attempted in a project with no pinned agent.
var errNoProjectAgent = errors.New("no project agent pinned")

// resolveSpawnAgent returns the agent key to spawn for a ticket. Resolution
// order: (1) an AgentType already on the ticket wins (resume continuity —
// never re-role a live session); (2) a pipeline Type binds its specialized
// role via config.RoleForType (research/spec/implement/review); (3) the
// project's pinned DefaultAgent. An unpinned, untyped project returns
// errNoProjectAgent and the caller must refuse the spawn (no global fallback).
//
// The role agents (claude-research/spec/review) ship with an empty Env, so
// they launch the DEFAULT claude profile — they differ from the project pin
// only by InitPrompt, not by CLAUDE_CONFIG_DIR. A typed ticket in a project
// pinned to a custom profile (e.g. claude-lean) therefore runs the role on the
// default profile; composing role InitPrompt with a pinned profile's Env is a
// deliberate v2 follow-up, not v1.
func (m *Model) resolveSpawnAgent(ticket *board.Ticket, proj *project.Project) (string, error) {
	if ticket != nil && ticket.AgentType != "" {
		return ticket.AgentType, nil
	}
	if ticket != nil {
		if role := config.RoleForType(ticket.Type); role != "" {
			return role, nil
		}
	}
	if proj != nil && proj.Settings.DefaultAgent != "" {
		return proj.Settings.DefaultAgent, nil
	}
	return "", errNoProjectAgent
}

// startGateAllows reports whether `ticket` may START now under the workflow
// prerequisite PRACTICE gate (an implement needs a done spec; a review needs an
// implement in in_review+). It returns true — proceed — when the gate passes,
// when the ticket is already associated with a session (a resume is never
// re-gated), or when the user previously chose "override & start" (consumed
// once here). When the gate blocks, it arms a choice OFFER — override / cancel
// — and returns false so the caller halts; `retry` is the spawn entry re-run if
// the user overrides. Unlike the WorktreeConflict SAFETY gate this is always
// overridable, matching the CLI's --force. The pure decision lives in
// workflow.CheckPrerequisite; m.globalStore satisfies its TicketLookup.
//
// "Already started" covers BOTH resume paths (see the two-resume taxonomy in
// spawnAgent): a TUI-native spawn stamps AgentSpawnedAt, while an external
// resume (`ticket new --session …`) carries an AgentSessionID with
// AgentSpawnedAt still nil. Either means work already exists behind the ticket,
// so the gate — which is about STARTING fresh work — must not fire.
func (m *Model) startGateAllows(ticket *board.Ticket, retry func() (tea.Model, tea.Cmd)) bool {
	if ticket == nil || ticket.AgentSpawnedAt != nil || ticket.AgentSessionID != "" {
		return true
	}
	if m.gateOverrides[ticket.ID] {
		delete(m.gateOverrides, ticket.ID)
		return true
	}
	err := workflow.CheckPrerequisite(ticket, m.globalStore)
	if err == nil {
		return true
	}
	id := ticket.ID
	m.showChoice = true
	m.choiceMsg = "Can't start this " + string(ticket.Type) + " ticket yet — " + err.Error()
	m.choices = []choiceItem{
		{Key: 'o', Label: "Override & start anyway", Fn: func() tea.Cmd {
			if m.gateOverrides == nil {
				m.gateOverrides = map[board.TicketID]bool{}
			}
			m.gateOverrides[id] = true
			_, cmd := retry()
			return cmd
		}},
		{Key: 'c', Label: "Cancel — create the upstream ticket first", Fn: func() tea.Cmd { return nil }},
	}
	return false
}

// expandLeadingTilde expands a leading "~/" in an env value to the user's
// home directory. Only a leading "~/" is expanded; "~user" and mid-string
// "~" are left untouched. Returns the input unchanged if HOME can't resolve.
// Used so a per-agent env like CLAUDE_CONFIG_DIR=~/.claude-personal points at
// a real path (environment variables are not shell-expanded).
func expandLeadingTilde(v string) string {
	if !strings.HasPrefix(v, "~/") {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return v
	}
	return filepath.Join(home, v[2:])
}

// buildSpawnReq constructs the daemon.SpawnReq for a ticket given the
// chosen spawnPlan. Pure function — no I/O, no Model receiver. Tested
// separately from the prepareSpawnWith integration path so each
// spawnPlan branch's argv + env shape is pinned by the test suite and
// cannot regress.
//
// The Env field carries OPENKANBAN_SESSION and OPENKANBAN_TICKET_ID
// explicitly so the wire-level SpawnReq is self-describing. The daemon
// side ALSO synthesizes them from req.SessionName / req.TicketID via
// terminal.Pane.SetSessionName / SetTicketID + buildCleanEnv — the two
// paths agree, and a downstream consumer of SpawnReq.Env (e.g. a
// future RPC log) sees the env contract on the wire.
func buildSpawnReq(in spawnReqInputs) daemon.SpawnReq {
	args := make([]string, len(in.cleanArgs))
	copy(args, in.cleanArgs)

	switch in.agentType {
	case "claude":
		if in.model != "" {
			args = append(args, "--model", in.model)
		}
		if in.isNewSession {
			// New claude sessions always start in plan mode so the user
			// reviews the proposed approach before any tree mutation.
			// Strip anything that would conflict — --dangerously-skip-permissions
			// (alias for bypassPermissions) and any pre-existing
			// --permission-mode pair from the user's config — then
			// append --permission-mode plan as the single authority.
			args = stripPermissionFlags(args)
			args = append(args, "--permission-mode", "plan")
			if !hasClaudeNameFlag(args) && strings.TrimSpace(in.ticket.Title) != "" {
				args = append(args, "-n", in.ticket.Title)
			}
			if in.ticket.AgentSessionID != "" && agent.SessionUUIDPattern.MatchString(in.ticket.AgentSessionID) {
				// Always migrate-on-resume; the divergent-fork option was
				// eliminated in task/enforce-one-to-one-session because
				// silent divergence broke the 1:1 ticket↔session
				// invariant the daemon enforces at the PTY layer. The
				// grep guard at internal/ui/forksession_guard_test.go
				// pins this invariant at build time. See
				// [[openkanban-one-to-one-ticket-session-invariant]].
				args = append(args, "--resume", in.ticket.AgentSessionID)
			}
			if in.promptTemplate != "" {
				prompt := agent.BuildContextPrompt(in.promptTemplate, in.ctxData)
				if prompt != "" {
					args = append(args, prompt)
				}
			}
		} else {
			// Honor a user-config preset that's already chosen the resume
			// flag (e.g. `--continue` in their agent config Args). Also
			// guards against double-injecting --resume on repeated calls.
			hasFlag := false
			for _, arg := range args {
				if arg == "--continue" || arg == "-c" || arg == "--resume" {
					hasFlag = true
					break
				}
			}
			if !hasFlag {
				// Prefer --resume <uuid> when the status-poll loop has
				// back-filled a known UUID (see internal/ui/model.go's
				// claude back-fill ~line 4522 and agent.FindClaudeSession).
				// --resume is deterministic; --continue is positional
				// ("most recent journal in cwd") and silently picks the
				// wrong journal after a ForceFresh re-spawn. Fall back
				// to --continue for tickets whose poll hasn't tagged
				// them yet (pre-back-fill tickets, or a session whose
				// first turn hasn't landed on disk).
				if agent.SessionUUIDPattern.MatchString(in.ticket.AgentSessionID) {
					args = append(args, "--resume", in.ticket.AgentSessionID)
				} else {
					args = append(args, "--continue")
				}
				// plan.InjectResumeNotice (option 'u'): append a positional
				// message after the resume flag so the resumed claude
				// session sees the brief-updated notice as the first new
				// user turn. Works for both --resume <uuid> and --continue.
				if in.plan.InjectResumeNotice {
					slug := agent.BranchSlug(in.ticket.BranchName)
					if slug != "" {
						args = append(args, fmt.Sprintf("Brief updated at tickets/%s.md — please re-read before continuing.", slug))
					}
				}
			}
		}
	case "opencode":
		args = []string{in.workdir, "--port", fmt.Sprintf("%d", in.agentPort)}
		if in.isNewSession {
			if in.promptTemplate != "" {
				prompt := agent.BuildContextPrompt(in.promptTemplate, in.ctxData)
				if prompt != "" {
					args = append(args, "--prompt", prompt)
				}
			}
		} else if in.opencodeSessionID != "" {
			args = append(args, "--session", in.opencodeSessionID)
		} else {
			args = append(args, "--continue")
		}
	case "gemini":
		if !in.isNewSession {
			if in.geminiSessionID != "" {
				args = append(args, "--resume")
			}
		} else if in.promptTemplate != "" {
			prompt := agent.BuildContextPrompt(in.promptTemplate, in.ctxData)
			if prompt != "" {
				args = append(args, "-i", prompt)
			}
		}
	case "codex":
		if !in.isNewSession {
			if in.codexSessionID != "" {
				if in.codexSessionID == "last" {
					args = []string{"resume", "--last"}
				} else {
					args = []string{"resume", in.codexSessionID}
				}
				args = append(args, in.cleanArgs...)
			}
		} else if in.promptTemplate != "" {
			prompt := agent.BuildContextPrompt(in.promptTemplate, in.ctxData)
			if prompt != "" {
				args = append(args, prompt)
			}
		}
	}

	// SpawnReq.Env duplicates OPENKANBAN_SESSION + OPENKANBAN_TICKET_ID
	// that the daemon-side buildCleanEnv would synthesize anyway. That
	// redundancy is intentional: the wire shape now carries the env
	// contract explicitly, so the test asserts on req.Env directly and
	// any future caller of Spawn (not just this closure) automatically
	// inherits the same env without a separate plumbing step.
	var env []string
	if in.sessionName != "" {
		env = append(env, "OPENKANBAN_SESSION="+in.sessionName)
	}
	ticketIDStr := string(in.ticket.ID)
	if ticketIDStr != "" {
		env = append(env, "OPENKANBAN_TICKET_ID="+ticketIDStr)
	}
	// Disable Claude Code's model-generated "Prompt suggestions" — the
	// next-prompt drafts it drops into the input box after each turn — for
	// openkanban-spawned sessions, where they're noise in a ticket-scoped agent.
	// Env name verified against the claude 2.1.177 binary; the env var wins over
	// the promptSuggestionEnabled setting. Emitted before the per-agent Env loop
	// so a user's explicit override in AgentConfig.Env takes precedence, and it
	// survives the daemon's buildCleanEnv CLAUDE_* strip for the same reason the
	// per-agent vars below do (req.Env is appended after the strip).
	if in.agentType == "claude" {
		env = append(env, "CLAUDE_CODE_ENABLE_PROMPT_SUGGESTION=false")
	}
	// Per-agent Env (e.g. CLAUDE_CONFIG_DIR for a custom Claude profile).
	// Appended after the OPENKANBAN_* vars; the daemon's buildCleanEnv strips
	// inherited CLAUDE_*/GEMINI_*/etc. before appending SpawnReq.Env, so these
	// survive. A leading "~/" is expanded to $HOME (env vars don't shell-expand).
	for k, v := range in.agentEnv {
		if k == "" {
			continue
		}
		env = append(env, k+"="+expandLeadingTilde(v))
	}

	return daemon.SpawnReq{
		TicketID:             ticketIDStr,
		SessionName:          in.sessionName,
		Command:              in.command,
		Args:                 args,
		Workdir:              in.workdir,
		Env:                  env,
		Cols:                 in.cols,
		Rows:                 in.rows,
		Scrollback:           in.scrollback,
		AgentSessionUUID:     in.ticket.AgentSessionID,
		AgentType:            in.agentType,
		ForwardNotifications: in.forwardNotifications,
	}
}

// prepareSpawnWith returns a tea.Cmd that performs the actual spawn off
// the event loop. The `plan` value is captured by value into the
// returned closure — keep it a value type (not a pointer) so future
// callers cannot accidentally mutate it after the modal callback fires.
func (m *Model) prepareSpawnWith(ticket *board.Ticket, proj *project.Project, agentCfg config.AgentConfig, plan spawnPlan) tea.Cmd {
	ticketID := ticket.ID
	worktreePath := ticket.WorktreePath
	branchName := ticket.BranchName
	baseBranch := ticket.BaseBranch
	useWorktree := ticket.UseWorktree
	width, height := m.width, m.height-2

	// Stamp the spawn-overlay start time here — the single chokepoint all
	// four ModeSpawning entry points funnel through — so the label-flip
	// clock can't drift across call sites. Unattached (ctrl+space) spawns
	// show no overlay, so they don't need it.
	if !plan.Unattached {
		m.spawnStartedAt = time.Now()
	}

	agentType := agentCfg.Command
	if strings.Contains(agentType, "/") {
		agentType = filepath.Base(agentType)
	}

	agentPort := ticket.AgentPort
	if agentPort == 0 && agentType == "opencode" {
		agentPort = m.allocateAgentPort()
		ticket.AgentPort = agentPort
		m.saveTicket(ticket)
	}

	mgr := m.worktreeMgrs[proj.ID]
	cfg := m.config
	daemonClient := m.daemonClient
	// Capture m.daemon by value so the closure routes Spawn/Owns through
	// the same testable seam as the exit-guard and TicketDone paths.
	// nil in two cases the closure must tolerate:
	//   1. daemon never reachable at startup (m.daemon = nil)
	//   2. tests that exercise non-daemon branches
	// The daemonClient nil-check below covers (1); the fast-path Owns
	// call also nil-checks before dereferencing.
	//
	// Bound to `dapi` (rather than `daemon`) so we don't shadow the
	// `daemon` package import the closure uses below.
	dapi := m.daemon

	return func() tea.Msg {
		// 1:1 invariant gate: before either the fast-path attach OR a
		// fresh Spawn, prove that the Claude session UUID this ticket
		// would resume into is safe to claim. ticketsvc.GateAttach
		// refuses when lsof shows the JSONL is held externally, when
		// the daemon owns the session for a DIFFERENT ticket, or when
		// the daemon reports a multi-match conflict. Allows when the
		// daemon owns for THIS ticket (idempotent re-attach) or for
		// the wire-compat case where OwnedByTicketID is empty (old
		// daemon mid-upgrade — degrade to "trust the single match").
		//
		// Runs BEFORE the mgr nil-check so the gate's refusal short-
		// circuits without needing a worktree manager (a foreign-held
		// session would never legitimately reach Spawn anyway).
		//
		// The Owns response gathered here is reused immediately below
		// for the fast-path attach so we don't double the daemon RPC.
		// Guarded on ticket.AgentSessionID being set: tickets without a
		// linked UUID have nothing to gate; fall through to Spawn.
		//
		// The Unattached (ctrl+space) path deliberately skips the
		// fast-path attach below: it must NEVER attach, even to an
		// already-owned session. handleSpawn is idempotent per TicketID,
		// so the regular Spawn call later returns the existing SessionID
		// which we then wrap in an Unattached PaneView. The gate itself
		// still runs — Unattached doesn't get to bypass the 1:1
		// invariant; a foreign-held UUID must refuse regardless.
		var ownsResp daemon.OwnsResp
		ownsValid := false
		if ticket.AgentSessionID != "" {
			if dapi != nil {
				ownsCtx, ownsCancel := context.WithTimeout(context.Background(), ownsProbeTimeout)
				var ownsErr error
				ownsResp, ownsErr = dapi.Owns(ownsCtx, ticket.AgentSessionID)
				ownsCancel()
				if ownsErr == nil {
					ownsValid = true
				} else {
					log.Printf("openkanban spawn-gate: daemon Owns(%s) failed: %v — degrading to lsof-only", ticket.AgentSessionID, ownsErr)
				}
			}
			lsofHolder, lerr := agent.SessionActive(ticket.AgentSessionID)
			if lerr != nil && !os.IsNotExist(lerr) {
				log.Printf("openkanban spawn-gate: lsof probe (%s): %v — treating as not held", ticket.AgentSessionID, lerr)
				lsofHolder = agent.SessionHolder{}
			}
			probe := func(_ string) (agent.SessionHolder, *daemon.OwnsResp, error) {
				if ownsValid {
					return lsofHolder, &ownsResp, nil
				}
				return lsofHolder, nil, nil
			}
			if gerr := ticketsvc.GateAttach(probe, ticket.AgentSessionID, ticketID); gerr != nil {
				var inUse *ticketsvc.ErrSessionInUse
				if errors.As(gerr, &inUse) {
					return spawnErrorMsg{ticketID: ticketID, err: "session in use: " + inUse.Error()}
				}
				return spawnErrorMsg{ticketID: ticketID, err: "spawn gate: " + gerr.Error()}
			}
		}

		// Fast path (B4): with the gate passed, if the daemon already
		// owns the session, skip the Spawn RPC entirely and build a
		// PaneView against the existing daemon-side session. Idempotent
		// Spawn (PR #34) would handle the duplicate at the daemon level,
		// but that still costs an RPC roundtrip and forces the daemon
		// to walk the dedupe table.
		//
		// Reuses the OwnsResp gathered for the gate above — single
		// daemon round-trip. The Unattached path skips this entirely so
		// it falls through to the normal Spawn → Unattached PaneView
		// flow below.
		if !plan.Unattached && ownsValid && ownsResp.Owned && ownsResp.SessionID != "" {
			// Prefer the daemon's stored SessionName over a local
			// recompute via sessionNameFor(). The daemon's value is
			// what the live agent's OPENKANBAN_SESSION env var holds
			// (baked at original spawn, possibly by a pre-fix binary
			// that used the UUID priority) and is therefore the
			// correct status-file lookup key. Empty SessionName
			// (e.g. older daemon that doesn't return the field)
			// falls back to local computation — same behavior as
			// before the field existed.
			resolvedName := ownsResp.SessionName
			if resolvedName == "" {
				resolvedName = sessionNameFor(ticket, branchName)
			}
			return attachExistingFastPath(
				ticketID,
				ownsResp.SessionID,
				resolvedName,
				ticket.Title,
				worktreePath,
				branchName,
				baseBranch,
				width,
				height,
				daemonClient,
			)
		}

		if mgr == nil {
			return spawnErrorMsg{ticketID: ticketID, err: "worktree manager not found"}
		}
		if daemonClient == nil {
			return spawnErrorMsg{ticketID: ticketID, err: "daemon unreachable — cannot spawn agent"}
		}

		generatedBranch := branchName
		if generatedBranch == "" {
			generatedBranch = m.generateBranchNameFromTitle(ticket.Title, proj)
		}

		base, _ := mgr.GetDefaultBranch()
		if baseBranch != "" {
			base = baseBranch
		}

		if useWorktree {
			if worktreePath == "" {
				path, err := mgr.CreateWorktree(generatedBranch, base)
				if err != nil {
					return spawnErrorMsg{ticketID: ticketID, err: "worktree failed: " + err.Error()}
				}
				if err := agent.SeedClaudeSettings(path, proj.RepoPath); err != nil {
					log.Printf("openkanban: seed claude settings (%s): %v", path, err)
				}
				worktreePath = path
			}
		} else {
			if err := mgr.SetupBranch(generatedBranch, base); err != nil {
				return spawnErrorMsg{ticketID: ticketID, err: "branch setup failed: " + err.Error()}
			}
			worktreePath = proj.RepoPath
		}
		branchName = generatedBranch
		baseBranch = base

		// Ensure this ticket's Claude transcript lives in the bucket of
		// the directory we're about to launch from. Claude Code resolves
		// `--resume <uuid>` only within the launch cwd's project bucket;
		// a session started elsewhere and later linked (ticket new
		// --session) is filed under its original cwd's bucket, where the
		// resume can't find it ("No conversation found"). Relocating it
		// here makes resume directory-independent. Idempotent — a no-op
		// for openkanban-created sessions, which already start in
		// worktreePath. Non-fatal, like SeedClaudeSettings above: a
		// failure logs and degrades to the prior launch-from-worktree
		// behavior.
		relocatedSession := false
		// resumeUnresolved is set when we're about to launch
		// `claude --resume <uuid>` but the transcript is NOT resolvable in
		// the launch cwd's bucket — either missing entirely (a genuinely
		// lost session) or a relocation that was skipped/failed just above.
		// Claude then prints "No conversation found" and exits within ~2s;
		// the daemon records that as an unexpected exit with no captured
		// output, so the failure is otherwise invisible. We surface a
		// visible notice (below) but still spawn — non-aborting, because a
		// blank session is recoverable while a silent 2s death is not
		// diagnosable.
		resumeUnresolved := false
		if agentType == "claude" && agent.SessionUUIDPattern.MatchString(ticket.AgentSessionID) {
			moved, nerr := agent.NormalizeSessionBucket(ticket.AgentSessionID, worktreePath)
			if nerr != nil {
				log.Printf("openkanban: normalize session bucket (%s → %s): %v",
					ticket.AgentSessionID, worktreePath, nerr)
			}
			relocatedSession = moved
			resumeUnresolved = !agent.ResumeResolvable(ticket.AgentSessionID, worktreePath)
		}

		// Session name for terminal identification (priority:
		// branch > ticket). The daemon picks this up in SpawnReq.SessionName
		// and wires it into OPENKANBAN_SESSION via the terminal pane's
		// buildCleanEnv. AgentSessionID is intentionally NOT used here:
		// the Claude UUID is for `--resume`, not for hook identification,
		// and using it would couple OPENKANBAN_SESSION to a value that
		// can be back-filled mid-session — diverging from the env var
		// the live agent's hook process already has. See sessionNameFor().
		sessionName := sessionNameFor(ticket, branchName)
		// sessionName + ticketID flow through SpawnReq below; daemon-side
		// pane.SetSessionName + pane.SetTicketID happen in StartHeadless.

		// Clean up any stale status file from previous sessions that may not have
		// been properly cleaned up (e.g., if the app was closed while an agent was running).
		// Also scrub the legacy UUID-keyed path: pre-fix spawns may have written
		// `<UUID>.status` files that the new poll won't read but which would
		// linger on disk indefinitely.
		agent.CleanupStatusFile(sessionName)
		if ticket.AgentSessionID != "" && ticket.AgentSessionID != sessionName {
			agent.CleanupStatusFile(ticket.AgentSessionID)
		}

		isNewSession := ticket.AgentSpawnedAt == nil
		// cleanArgs strips empty-string entries from the configured args so a
		// user can omit a default flag by leaving an empty placeholder without
		// poisoning argv (claude in particular gets confused by a leading "").
		cleanArgs := make([]string, 0, len(agentCfg.Args))
		for _, a := range agentCfg.Args {
			if a != "" {
				cleanArgs = append(cleanArgs, a)
			}
		}

		promptTemplate := cfg.GetEffectiveInitPrompt(agentType)

		// Sync the openkanban card's description into the in-repo brief
		// file at tickets/<slug>.md (worktree-relative) before rendering
		// the priming prompt. A brief write failure is logged but does
		// not abort the spawn — the agent can still proceed with the
		// inline title/description from the prompt. Stays CLIENT-side
		// because the daemon doesn't touch the worktree filesystem; the
		// brief must be written before Spawn so the resumed agent sees
		// it.
		briefRelPath, hasBrief, briefErr := resolveBrief(ticket, worktreePath, plan)
		if briefErr != nil {
			fmt.Fprintf(os.Stderr, "openkanban: merge brief failed: %v\n", briefErr)
		}
		if proj != nil && proj.Settings.IgnoreTicketBriefs && worktreePath != "" {
			if err := agent.EnsureTicketsGitExcluded(worktreePath); err != nil {
				fmt.Fprintf(os.Stderr, "openkanban: brief-exclude: %v\n", err)
			}
		}

		// readyNotice is surfaced via m.notify() in the spawnReadyMsg
		// handler. For option 'u' (InjectResumeNotice), we toast the user
		// so they know the brief was rewritten under the resumed session.
		var readyNotice string
		if plan.InjectResumeNotice {
			if slug := agent.BranchSlug(ticket.BranchName); slug != "" {
				readyNotice = fmt.Sprintf("Brief at tickets/%s.md updated.", slug)
			}
		}
		if relocatedSession {
			msg := "Relocated session transcript to this ticket's directory."
			if readyNotice != "" {
				readyNotice = msg + " " + readyNotice
			} else {
				readyNotice = msg
			}
		}
		if resumeUnresolved {
			msg := fmt.Sprintf("Resume target %s not found in this directory's Claude bucket — session may start blank.", ticket.AgentSessionID)
			if readyNotice != "" {
				readyNotice = msg + " " + readyNotice
			} else {
				readyNotice = msg
			}
		}
		// External resume: spawn was given an AgentSessionID up front
		// (via `openkanban ticket new --session <uuid>`), so this is the
		// first openkanban-spawn but the underlying claude session is
		// already populated with prior context. The template uses this
		// to shorten the priming preamble.
		isExternalResume := isNewSession && ticket.AgentSessionID != "" && agent.SessionUUIDPattern.MatchString(ticket.AgentSessionID)
		ctxData := agent.NewContextData(ticket, briefRelPath, hasBrief, isExternalResume)

		command := agentCfg.Command

		// Resolve agent-specific session IDs from the worktree
		// filesystem. These are inputs to buildSpawnReq below — the
		// helper itself is pure, so the I/O happens here.
		var opencodeSessionID, geminiSessionID, codexSessionID string
		switch agentType {
		case "opencode":
			opencodeSessionID = agent.FindOpencodeSession(worktreePath)
		case "gemini":
			if !isNewSession {
				geminiSessionID = agent.FindGeminiSession(worktreePath)
			}
		case "codex":
			if !isNewSession {
				codexSessionID = agent.FindCodexSession(worktreePath)
			}
		}

		// Hand the spawn off to the daemon. The daemon runs the PTY in
		// its own process; we then build a PaneView and attach
		// immediately so the snapshot frames flow into the local
		// emulator before the model sees the spawnReadyMsg.
		var projModel string
		if proj != nil {
			projModel = strings.TrimSpace(proj.Settings.Model)
		}
		req := buildSpawnReq(spawnReqInputs{
			ticket:               ticket,
			plan:                 plan,
			sessionName:          sessionName,
			command:              command,
			workdir:              worktreePath,
			cols:                 width,
			rows:                 height,
			agentType:            agentType,
			agentEnv:             agentCfg.Env,
			cleanArgs:            cleanArgs,
			isNewSession:         isNewSession,
			promptTemplate:       promptTemplate,
			ctxData:              ctxData,
			agentPort:            agentPort,
			opencodeSessionID:    opencodeSessionID,
			geminiSessionID:      geminiSessionID,
			codexSessionID:       codexSessionID,
			forwardNotifications: cfg.Behavior.ForwardAgentNotifications,
			model:                projModel,
			scrollback:           cfg.UI.ScrollbackLines,
		})
		spawnCtx, spawnCancel := context.WithTimeout(context.Background(), 5*time.Second)
		var (
			resp daemon.SpawnResp
			err  error
		)
		if dapi != nil {
			resp, err = dapi.Spawn(spawnCtx, req)
		} else {
			resp, err = daemonClient.Spawn(spawnCtx, req)
		}
		spawnCancel()
		if err != nil {
			return spawnErrorMsg{ticketID: ticketID, err: "spawn failed: " + err.Error()}
		}

		// Unattached (ctrl+space): the session is alive on the daemon, but
		// we do NOT attach. Build an Unattached PaneView and hand it back via
		// spawnUnattachedReadyMsg so the Update loop registers it without
		// switching to ModeAgentView. No attachWithRetry.
		if plan.Unattached {
			pv := newUnattachedPane(daemonClient, ticketID, resp.SessionID, sessionName, worktreePath, width, height)
			return spawnUnattachedReadyMsg{
				ticketID:     ticketID,
				pane:         pv,
				worktreePath: worktreePath,
				branchName:   branchName,
				baseBranch:   baseBranch,
			}
		}

		pv := daemonclient.NewPaneView(daemonClient, string(ticketID), resp.SessionID, nil)
		pv.SetWorkdir(worktreePath)
		pv.SetSessionName(sessionName)
		pv.SetTicketTitle(ticket.Title)
		pv.SetSize(width, height)

		// B7: retry attach after spawn with backoff. The daemon-side
		// session is alive once Spawn returns; a local Attach failure
		// (timeout reading the hello, transient connect issue, etc.)
		// shouldn't strand the user into spawning a duplicate next
		// time. attachWithRetry returns the LAST attach error (or nil
		// on success) so we can decide whether to surface a notice.
		attachErr := attachWithRetry(pv)
		if attachErr != nil {
			log.Printf("openkanban model: attach failed after spawn+retry ticket=%s session=%s err=%v",
				ticketID, resp.SessionID, attachErr)
			// Spawn succeeded but we couldn't get a binary channel.
			// Keep the PaneView so the user can retry attach; publish
			// the error to the pane so View() renders the failure
			// overlay (rather than the blank grid that left users
			// stranded in ModeAgentView with no signal). The notice
			// stays as a redundant toast hint — the overlay is the
			// primary surface.
			pv.SetLastAttachErr(attachErr)
			return spawnReadyMsg{
				ticketID:     ticketID,
				pane:         pv,
				worktreePath: worktreePath,
				branchName:   branchName,
				baseBranch:   baseBranch,
				notice:       "Spawned but attach failed — press Enter to retry",
			}
		}

		return spawnReadyMsg{
			ticketID:     ticketID,
			pane:         pv,
			worktreePath: worktreePath,
			branchName:   branchName,
			baseBranch:   baseBranch,
			notice:       readyNotice,
		}
	}
}

// attachWithRetry calls pv.Attach() up to spawnAttachMaxRetries+1
// times with linear backoff between tries. Returns nil on success or
// the final error if every attempt failed. See spawnAttachRetrySchedule
// for the exact timeouts/sleeps; the backoff is short because the
// failure modes we care about — transient socket-level hiccup, brief
// daemon goroutine contention — resolve quickly; longer waits would
// just punish the user.
func attachWithRetry(pv *daemonclient.PaneView) error {
	return retryAttach(func(ctx context.Context) error { return pv.Attach(ctx) })
}

// retryStep is one entry in the post-Spawn attach retry schedule. sleep
// is taken before this attempt fires (so the first step's sleep is the
// gap between the initial attempt and the first retry); timeout is the
// context bound for the attach call on this step.
type retryStep struct {
	sleep   time.Duration
	timeout time.Duration
}

// spawnAttachRetrySchedule is the post-Spawn attach retry schedule.
// Worst-case wall clock on full failure:
//
//	initial(5s) + 200ms + retry1(1.5s) + 400ms + retry2(1.5s) ≈ 8.6s
//
// Previously the retry timeouts were 3s each, giving a ~11.6s ceiling
// under a misleading "Spawning…" splash. The splash work is queued as
// a separate ticket (ui-spinner-for-long-running-daemon-ops); for now
// we just tighten the budget. Pulled into a named variable so future
// tuning (or unit testing of the schedule itself) is straightforward.
var spawnAttachRetrySchedule = []retryStep{
	{sleep: 200 * time.Millisecond, timeout: 1500 * time.Millisecond},
	{sleep: 400 * time.Millisecond, timeout: 1500 * time.Millisecond},
}

// retryAttach is the testable core of attachWithRetry — same retry
// schedule, but takes the attach call as a function so tests can drop
// in a fake without standing up a real daemon socket. The error of the
// last attempt is returned; nil means at least one call succeeded.
func retryAttach(attach func(ctx context.Context) error) error {
	attachCtx, attachCancel := context.WithTimeout(context.Background(), 5*time.Second)
	attachErr := attach(attachCtx)
	attachCancel()
	if attachErr == nil {
		return nil
	}
	// An "already attached" rejection is deterministic — the session is
	// held by another TUI and retrying won't change that. Bail out
	// immediately so the caller can warn instead of burning the full
	// backoff schedule (~8.6s) before surfacing a misleading failure.
	if errors.Is(attachErr, daemonclient.ErrAlreadyAttached) {
		return attachErr
	}
	for _, step := range spawnAttachRetrySchedule {
		if attachErr == nil {
			break
		}
		time.Sleep(step.sleep)
		retryCtx, retryCancel := context.WithTimeout(context.Background(), step.timeout)
		attachErr = attach(retryCtx)
		retryCancel()
		if errors.Is(attachErr, daemonclient.ErrAlreadyAttached) {
			break
		}
	}
	return attachErr
}

// spawnAttachMaxRetries is the cap on post-Spawn attach retry attempts
// (additional tries beyond the initial one). Kept in sync with
// len(spawnAttachRetrySchedule) so callers and tests that count
// attempts have a single named constant to anchor on.
const spawnAttachMaxRetries = 2

// sessionNameFor mirrors the priority used inside the prepareSpawnWith
// closure (branchName > ticketID) so the Owns fast-path constructs a
// PaneView with the same identity the regular spawn path would.
// Exposed as a helper so the fast-path closure branch and the
// spawn-path closure branch agree.
//
// AgentSessionID (the Claude UUID) is intentionally NOT in the
// priority chain. It identifies a journal for `--resume`, not the
// agent's OPENKANBAN_SESSION env var — and conflating them creates a
// mid-session divergence where the back-fill (status-poll) silently
// changes which file the UI tries to read while the live hook keeps
// writing to the original branch-keyed path.
func sessionNameFor(ticket *board.Ticket, branchName string) string {
	name := string(ticket.ID)
	if branchName != "" {
		name = branchName
	}
	return name
}

// attachExistingFastPath builds a PaneView pointing at the daemon-owned
// session that Owns reported and tries to attach to it (with retry,
// matching the regular spawn path). Returned as a tea.Msg so the
// prepareSpawnWith closure can `return attachExistingFastPath(...)` —
// the result is structurally identical to the post-Spawn spawnReadyMsg
// the Update loop already knows how to consume.
//
// Cross-cuts:
//   - The constructed PaneView shares the closure's worktreePath /
//     branchName / baseBranch so the Update loop's onSpawnReady seam
//     stamps the ticket exactly as the regular path would.
//   - The notice field carries the attach-retry diagnostic (see B7)
//     so the user can tell a fast-path-attach-failure apart from a
//     true daemon-unreachable state.
//
// newUnattachedPane builds a daemon-owned, NOT-yet-attached PaneView for a
// freshly-spawned session — the ctrl+space (spawnPlan.Unattached) path. It is
// extracted from the prepareSpawnWith closure so it can be unit-tested
// directly: the inline construction site sits after the daemonClient-nil guard
// and is unreachable from any daemonless test, so this helper is the only
// place the Running:true invariant can be exercised in isolation.
//
// Running:true is REQUIRED — NewPaneView only flips the pane to
// PaneViewUnattached when info.Running is true (internal/daemonclient/
// paneview.go); drop it and the pane stays PaneViewDetached, which renders a
// "press Enter to retry" overlay instead of the unattached chrome.
func newUnattachedPane(client *daemonclient.Client, ticketID board.TicketID, sessionID, sessionName, workdir string, cols, rows int) *daemonclient.PaneView {
	info := daemon.SessionInfo{
		SessionID:   sessionID,
		TicketID:    string(ticketID),
		SessionName: sessionName,
		Workdir:     workdir,
		Cols:        cols,
		Rows:        rows,
		Running:     true,
	}
	return daemonclient.NewPaneView(client, string(ticketID), sessionID, &info)
}

func attachExistingFastPath(
	ticketID board.TicketID,
	sessionID string,
	sessionName string,
	ticketTitle string,
	worktreePath string,
	branchName string,
	baseBranch string,
	width int,
	height int,
	daemonClient *daemonclient.Client,
) tea.Msg {
	pv := daemonclient.NewPaneView(daemonClient, string(ticketID), sessionID, nil)
	pv.SetWorkdir(worktreePath)
	pv.SetSessionName(sessionName)
	pv.SetTicketTitle(ticketTitle)
	pv.SetSize(width, height)

	attachErr := attachWithRetry(pv)
	if attachErr != nil {
		// The session is alive and attached in another TUI — warn before
		// taking it over instead of showing a misleading "stream failed"
		// notice. Carry the freshly-built pv so the Update handler can
		// register it (it isn't in m.panes yet) and arm the modal.
		if errors.Is(attachErr, daemonclient.ErrAlreadyAttached) {
			// Record the error so the backdrop behind the takeover modal
			// shows the actionable overlay, not a blank pane (the "nothing
			// visible in the session" report). Cleared on a successful Takeover.
			pv.SetLastAttachErr(attachErr)
			return attachConflictMsg{ticketID: ticketID, pv: pv}
		}
		log.Printf("openkanban model: fast-path attach failed ticket=%s session=%s err=%v",
			ticketID, sessionID, attachErr)
		pv.SetLastAttachErr(attachErr)
		return spawnReadyMsg{
			ticketID:     ticketID,
			pane:         pv,
			worktreePath: worktreePath,
			branchName:   branchName,
			baseBranch:   baseBranch,
			notice:       "Attached to existing session but stream failed — press Enter to retry",
		}
	}
	return spawnReadyMsg{
		ticketID:     ticketID,
		pane:         pv,
		worktreePath: worktreePath,
		branchName:   branchName,
		baseBranch:   baseBranch,
	}
}

func (m *Model) stopAgent() (tea.Model, tea.Cmd) {
	ticket := m.selectedTicket()
	if ticket == nil {
		return m, nil
	}

	if pane, ok := m.panes[ticket.ID]; ok {
		pane.Stop()
		delete(m.panes, ticket.ID)
	}

	// Preserve AgentCompleted on a Done ticket — manually stopping the
	// pane after the agent reported completion shouldn't wipe the badge.
	if ticket.Status != board.StatusDone {
		ticket.SetAgentStatus(board.AgentNone)
	}
	m.saveTicket(ticket)
	m.notify("Agent stopped")
	return m, nil
}

// wrapUpSessionForTicket performs the TUI-side equivalent of the CLI's
// wrapUpSessionTicketAt (cmd/ticket_done.go): when the ticket is leaving
// in_progress for a terminal status (in_review or done) and has a live
// daemon session, stop the local pane, ask the daemon to kill the
// session via TicketDone, and stamp AgentStatus=Completed so the card
// renders correctly even before the daemon's "exited" event lands.
//
// Returns a tea.Cmd that performs the daemon-side teardown (Stop +
// TicketDone) in a background goroutine. The Update loop must not block
// on these RPCs — together they have a worst-case ~7s timeout (5s Kill
// + 2s TicketDone), which is what the multi-second freeze on session
// exit was actually about. Local state mutations (pane map delete,
// focus clear, AgentStatus stamp) stay synchronous so the next render
// reflects the wrap-up immediately.
//
// Safe to call when no daemon session exists for the ticket — returns
// nil in that case. Must be called BEFORE m.globalStore.Move so the
// ticket.Status check ("are we leaving in_progress?") runs against the
// pre-move status; Move's SetStatus mutates ticket.Status in place.
//
// The returned Cmd's closure captures the pane handle and daemon API
// into locals BEFORE the goroutine runs, per the "tea.Cmd goroutines
// must not touch shared Model state" discipline in internal/ui/CLAUDE.md.
func (m *Model) wrapUpSessionForTicket(ticket *board.Ticket, newStatus board.TicketStatus) tea.Cmd {
	if ticket == nil {
		return nil
	}
	// Only wrap up when crossing OUT of in_progress to a terminal
	// status. Backlog→in_progress and other transitions don't have a
	// session to tear down (or shouldn't tear it down if they do).
	if ticket.Status != board.StatusInProgress {
		return nil
	}
	if newStatus != board.StatusInReview && newStatus != board.StatusDone {
		return nil
	}

	// Capture the local pane handle and remove it from m.panes so
	// subsequent Update ticks don't see a stale entry. The goroutine
	// below stops the captured handle off-loop.
	var capturedPane *daemonclient.PaneView
	if pane, ok := m.panes[ticket.ID]; ok {
		capturedPane = pane
		delete(m.panes, ticket.ID)
	}
	if m.focusedPane == ticket.ID {
		m.mode = ModeNormal
		m.focusedPane = ""
	}

	// AgentStatus discipline. SetAgentStatus stamps StatusChangedAt;
	// use it, don't assign directly (see Ticket.SetAgentStatus in
	// internal/board/board.go and the SetAgentStatus memory note).
	// The caller's saveTicket call after Move will persist this.
	ticket.SetAgentStatus(board.AgentCompleted)

	// Capture daemon API + ticket ID into locals so the goroutine has
	// no dependency on m.*.
	api := m.daemon
	ticketID := string(ticket.ID)
	if capturedPane == nil && api == nil {
		return nil
	}

	return func() tea.Msg {
		t0 := time.Now()
		if capturedPane != nil {
			_ = capturedPane.Stop()
			// Close after Stop so teaMsgs is drained and the PaneView
			// goroutines (drainWG, detach watchdog) can wind down
			// instead of lingering until GC. The model has already
			// removed this pane from m.panes synchronously, so no
			// in-flight handler will dispatch new messages to it.
			_ = capturedPane.Close()
		}
		if api != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if _, err := api.TicketDone(ctx, ticketID); err != nil {
				log.Printf("openkanban: TicketDone(%s) on board promotion: %v", ticketID, err)
			}
		}
		log.Printf("openkanban: wrapUpSessionForTicket(%s) daemon teardown took %s", ticketID, time.Since(t0))
		return nil
	}
}

func (m *Model) selectedTicket() *board.Ticket {
	if len(m.columnTickets) <= m.activeColumn {
		return nil
	}
	tickets := m.columnTickets[m.activeColumn]
	if len(tickets) <= m.activeTicket {
		return nil
	}
	return tickets[m.activeTicket]
}

// revealThroughFilters clears any active board filter that would hide t,
// so a ticket the user just created interactively is always visible and
// selectable. It only relaxes the dimensions that actually hide THIS
// ticket: a search query is cleared, the open-only session filter falls
// back to "all", and a project narrow gains t's project (rather than
// being wiped, preserving a deliberate multi-project view). No-op when t
// already passes the filter.
//
// Scope: interactive creation only. The async board-resync / CLI-create
// paths must NOT call this — they must never disturb the user's filter
// state out from under an active session.
func (m *Model) revealThroughFilters(t *board.Ticket) {
	if t == nil || m.ticketMatchesFilter(t) {
		return
	}
	if m.filterQuery != "" {
		m.filterQuery = ""
		m.filterInput.SetValue("")
	}
	if m.sessionFilter == SessionFilterOpen {
		m.sessionFilter = SessionFilterAll
	}
	if len(m.filterProjectIDs) > 0 && !m.filterProjectIDs[t.ProjectID] {
		m.filterProjectIDs[t.ProjectID] = true
	}
}

func (m *Model) selectTicketByID(ticketID board.TicketID) {
	for colIdx, tickets := range m.columnTickets {
		for ticketIdx, t := range tickets {
			if t.ID == ticketID {
				m.activeColumn = colIdx
				m.activeTicket = ticketIdx
				m.ensureTicketVisible()
				return
			}
		}
	}
	// Target no longer visible (filtered out by a refresh). Clamp
	// activeTicket to the current column's bounds so callers don't
	// see an out-of-range index. Without this, toggling a filter that
	// hides the selected ticket leaves the cursor pointing past the
	// end of a now-shorter column.
	if m.activeColumn >= 0 && m.activeColumn < len(m.columnTickets) {
		if n := len(m.columnTickets[m.activeColumn]); m.activeTicket >= n {
			if n > 0 {
				m.activeTicket = n - 1
			} else {
				m.activeTicket = 0
			}
		}
	}
}

func (m *Model) refreshColumnTickets() {
	m.columnTickets = make([][]*board.Ticket, len(m.columns))
	for i, col := range m.columns {
		allForStatus := m.globalStore.GetByStatus(col.Status)
		var filtered []*board.Ticket
		for _, t := range allForStatus {
			if !m.ticketMatchesFilter(t) {
				continue
			}
			filtered = append(filtered, t)
		}
		sortTickets(filtered, m.sortMode)
		m.columnTickets[i] = filtered
	}

	if len(m.columnOffsets) != len(m.columns) {
		m.columnOffsets = make([]int, len(m.columns))
	}
	if len(m.columnTicketHeights) != len(m.columns) {
		m.columnTicketHeights = make([][]int, len(m.columns))
	}
	m.compactColumnOffsets()
}

// compactColumnOffsets reduces each column's vertical scroll offset to the
// smallest value that still keeps the tail card visible inside the current
// column budget. After a filter shrinks a column, this guarantees the user
// sees as many post-filter cards as the screen has room for — never less.
//
// Offsets only decrease; an in-range offset that already fits its tail
// stays put. Uses the ticketHeight fallback (not the columnTicketHeights
// cache) because the cache is keyed to the pre-refresh ticket order — a
// post-refresh index may point at a different card.
func (m *Model) compactColumnOffsets() {
	budget := m.columnContentHeight()
	if budget <= 0 {
		// Pre-WindowSizeMsg or terminal too small. Leave offsets alone;
		// the next render will be clipped by MaxHeight anyway.
		return
	}
	for i := range m.columnTickets {
		if i >= len(m.columnOffsets) {
			break
		}
		n := len(m.columnTickets[i])
		if n == 0 {
			m.columnOffsets[i] = 0
			continue
		}
		// Find the smallest offset such that the tail [target..n-1] fits
		// in budget. Walk backward from n-1, accumulating ticketHeight
		// per card; reserve the ▲ row whenever there's still anything
		// above the candidate offset (j > 0). The reserve participates
		// in the fit check only — it's one shared row above the visible
		// window, not one row per card.
		target := 0
		used := 0
		for j := n - 1; j >= 0; j-- {
			cost := ticketHeight
			reserve := 0
			if j > 0 {
				reserve = 1
			}
			if used+cost+reserve > budget {
				target = j + 1
				break
			}
			used += cost
		}
		if target > m.columnOffsets[i] {
			continue // never push the user down
		}
		m.columnOffsets[i] = target
	}
}

// sortTickets reorders the slice in place per the given mode. Priority 0
// (unset) is treated as the default value 3 so cards predating the
// priority field don't all clump at one end of the sort.
func sortTickets(tickets []*board.Ticket, mode SortMode) {
	switch mode {
	case SortName:
		sort.SliceStable(tickets, func(i, j int) bool {
			return strings.ToLower(tickets[i].Title) < strings.ToLower(tickets[j].Title)
		})
	case SortAge:
		sort.SliceStable(tickets, func(i, j int) bool {
			return tickets[i].CreatedAt.After(tickets[j].CreatedAt)
		})
	case SortStatusChange:
		sort.SliceStable(tickets, func(i, j int) bool {
			ai := statusChangedAtOrFallback(tickets[i])
			aj := statusChangedAtOrFallback(tickets[j])
			if !ai.Equal(aj) {
				return ai.After(aj)
			}
			return tickets[i].CreatedAt.After(tickets[j].CreatedAt)
		})
	case SortPriority:
		sort.SliceStable(tickets, func(i, j int) bool {
			a := effectivePriority(tickets[i].Priority)
			b := effectivePriority(tickets[j].Priority)
			if a != b {
				return a < b
			}
			return tickets[i].CreatedAt.After(tickets[j].CreatedAt)
		})
	}
}

func effectivePriority(p int) int {
	if p < 1 || p > 5 {
		return 3
	}
	return p
}

// statusChangedAtOrFallback returns the ticket's StatusChangedAt or
// falls back to UpdatedAt when nil. Backfill in validateFrontmatter
// guarantees non-nil after load, so the fallback only matters for
// in-memory tickets constructed outside NewTicket (e.g. tests).
func statusChangedAtOrFallback(t *board.Ticket) time.Time {
	if t.StatusChangedAt != nil {
		return *t.StatusChangedAt
	}
	return t.UpdatedAt
}

func (m *Model) ticketMatchesFilter(t *board.Ticket) bool {
	_, isOpenSession := m.daemonOwned[t.ID]
	bypassProjectAndQuery := m.alwaysShowWorking && isOpenSession

	if !bypassProjectAndQuery && len(m.filterProjectIDs) > 0 && !m.filterProjectIDs[t.ProjectID] {
		return false
	}
	if m.sessionFilter == SessionFilterOpen && !isOpenSession {
		return false
	}
	if bypassProjectAndQuery {
		return true
	}
	if m.filterQuery == "" {
		return true
	}

	query := strings.ToLower(m.filterQuery)

	if strings.HasPrefix(query, "@") {
		parts := strings.SplitN(query, " ", 2)
		projectName := strings.TrimPrefix(parts[0], "@")
		proj := m.globalStore.GetProjectForTicket(t)
		if proj == nil || !strings.Contains(strings.ToLower(proj.Name), projectName) {
			return false
		}
		if len(parts) == 1 {
			return true
		}
		query = strings.TrimSpace(parts[1])
	}

	title := strings.ToLower(t.Title)
	desc := strings.ToLower(t.Description)
	return strings.Contains(title, query) || strings.Contains(desc, query)
}

func (m *Model) nextStatus(current board.TicketStatus) board.TicketStatus {
	switch current {
	case board.StatusBacklog:
		return board.StatusNext
	case board.StatusNext:
		return board.StatusInProgress
	case board.StatusInProgress:
		return board.StatusInReview
	case board.StatusInReview:
		return board.StatusDone
	default:
		return current
	}
}

func (m *Model) previousStatus(current board.TicketStatus) board.TicketStatus {
	switch current {
	case board.StatusDone:
		return board.StatusInReview
	case board.StatusInReview:
		return board.StatusInProgress
	case board.StatusInProgress:
		return board.StatusNext
	case board.StatusNext:
		return board.StatusBacklog
	default:
		return current
	}
}

// moveAndPromoteMsg formats the post-Move status-bar toast. Three
// parts joined with " · ":
//  1. core: "Moved to <status>" (always)
//  2. promoted: "promoted N approval(s) to repo defaults" (when promoted>0)
//  3. pruned: "pruned N stale entr(y/ies)" (when pruned>0)
//
// Pruned entry strings are NOT inlined — they live in <repo>/.claude/.pruned-log
// for the user to inspect. Toast is count-only to stay scannable.
func moveAndPromoteMsg(target board.TicketStatus, promoted []string, pruned []agent.PruneRecord) string {
	msg := "Moved to " + string(target)
	switch n := len(promoted); n {
	case 0:
	case 1:
		msg += " · promoted 1 approval to repo defaults"
	default:
		msg += fmt.Sprintf(" · promoted %d approvals to repo defaults", n)
	}
	switch n := len(pruned); n {
	case 0:
	case 1:
		msg += " · pruned 1 stale entry"
	default:
		msg += fmt.Sprintf(" · pruned %d stale entries", n)
	}
	return msg
}

func (m *Model) notify(msg string) {
	m.notification = msg
	m.notifyTime = time.Now()
	// Mirror every notification to stderr so the user has a durable
	// record after the TUI exits — the in-UI toast disappears on
	// timeout (and is hard to even select for copy without the click
	// hitting another control). With stderr logging, the same message
	// is in /tmp/<wherever-the-user-redirects>.log.
	log.Printf("openkanban notify: %s", msg)
}

func (m *Model) saveTicket(ticket *board.Ticket) {
	if err := m.globalStore.Save(ticket); err != nil {
		log.Printf("openkanban saveTicket: ticket=%s err: %v", ticket.ID, err)
		m.notify("Failed to save: " + err.Error())
		return
	}
	m.recordSavedTicket(ticket)
}

// hasClaudeNameFlag returns true if the args slice already contains
// a Claude Code session-name flag (-n or --name). Used to avoid
// double-naming when the user pre-set it in their agent config.
func hasClaudeNameFlag(args []string) bool {
	for _, a := range args {
		if a == "-n" || a == "--name" || strings.HasPrefix(a, "--name=") {
			return true
		}
	}
	return false
}

// stripPermissionFlags removes any claude permission-related flags the
// user may have configured (--dangerously-skip-permissions, any form
// of --permission-mode) so the caller can install its own authoritative
// permission mode without ambiguity.
func stripPermissionFlags(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--dangerously-skip-permissions" {
			continue
		}
		if a == "--permission-mode" {
			// Skip the flag and its value (if present).
			if i+1 < len(args) {
				i++
			}
			continue
		}
		if strings.HasPrefix(a, "--permission-mode=") {
			continue
		}
		out = append(out, a)
	}
	return out
}

func (m *Model) resetSpawnState(ticketID board.TicketID) {
	if ticket, _ := m.globalStore.Get(ticketID); ticket != nil {
		ticket.AgentSpawnedAt = nil
		// Same rule as stopAgent: a Done ticket keeps its completed badge.
		if ticket.Status != board.StatusDone {
			ticket.SetAgentStatus(board.AgentNone)
		}
		m.saveTicket(ticket)
	}
	m.mode = ModeNormal
	m.spawningTicketID = ""
	m.spawningAgent = ""
	delete(m.panes, ticketID)
}

func (m *Model) RunningAgentCount() int {
	count := 0
	for _, pane := range m.panes {
		if pane.Running() {
			count++
		}
	}
	return count
}

func (m *Model) getAgentNames() []string {
	names := make([]string, 0, len(m.config.Agents))
	for name := range m.config.Agents {
		names = append(names, name)
	}
	if len(names) == 0 {
		return []string{"opencode", "claude", "claude-custom", "gemini", "codex", "aider"}
	}
	sort.Strings(names)
	return names
}

// enabledAgentNames returns the configured agent keys that are enabled
// (PATH-auto-detected or explicitly overridden via AgentConfig.Enabled), sorted.
// This is the set offered in the per-project pin cycle, so uninstalled agents
// don't clutter it. Falls back to all configured agents if none qualify (so a
// machine with an unusual PATH never strands the user with an empty cycle).
func (m *Model) enabledAgentNames() []string {
	all := m.getAgentNames()
	enabled := make([]string, 0, len(all))
	for _, name := range all {
		if cfg, ok := m.config.Agents[name]; ok && cfg.IsEnabled() {
			enabled = append(enabled, name)
		}
	}
	if len(enabled) == 0 {
		return all
	}
	return enabled
}

func (m *Model) getBranchPrefix(proj *project.Project) string {
	if proj != nil && proj.Settings.BranchPrefix != "" {
		return proj.Settings.BranchPrefix
	}
	if m.config.Defaults.BranchPrefix != "" {
		return m.config.Defaults.BranchPrefix
	}
	return "task/"
}

func (m *Model) getBranchTemplate(proj *project.Project) string {
	if proj != nil && proj.Settings.BranchTemplate != "" {
		return proj.Settings.BranchTemplate
	}
	if m.config.Defaults.BranchTemplate != "" {
		return m.config.Defaults.BranchTemplate
	}
	return "{prefix}{slug}"
}

func (m *Model) getSlugMaxLength(proj *project.Project) int {
	if proj != nil && proj.Settings.SlugMaxLength > 0 {
		return proj.Settings.SlugMaxLength
	}
	if m.config.Defaults.SlugMaxLength > 0 {
		return m.config.Defaults.SlugMaxLength
	}
	return 40
}

// T2 of the integration plan removed maybeAutoStopCompletedPane.
// Ticket-done now flows CLI → daemon (TicketDoneReq) → SessionEvent
// broadcast; subscribed TUIs react via handleDaemonSessionEvent with
// the authoritative Expected=true signal. No per-TUI poll-driven kill
// path remains.

// Cleanup detaches every pane this TUI holds from its daemon-side
// session. It does NOT kill the underlying agents: daemon sessions
// outlive any single TUI, and other TUIs may still be attached. The
// daemon's last-client-disconnect handler (server.go) is the only
// place sessions die on TUI exit, and it only fires when the actual
// last connection drops.
func (m *Model) Cleanup() {
	m.monitor.stop()
	for _, pane := range m.panes {
		_ = pane.Close()
	}
}

// StartStallMonitor arms the diagnostic stall watchdog (ticker goroutine
// + SIGUSR2 handler). Called from app.go for the real TUI; left unarmed
// in tests that construct a Model directly. Stopped by Cleanup.
func (m *Model) StartStallMonitor() {
	m.monitor.start()
}

// StallRecoverMsg is injected by the stall watchdog (via program.Send) when
// it detects a sustained "starved" stall — the Update loop parked OUTSIDE
// Update/View while the daemon keeps pushing events. It asks the loop to
// detach the focused agent view back to the board, turning a wedge into a
// recoverable blip. (Only "starved" stalls trigger it: program.Send is
// actually processed in that shape, and it's a genuine wedge rather than a
// transient slow render.)
type StallRecoverMsg struct{}

// SetStallRecoverySink wires the watchdog's recovery action to the running
// program. Called from app.go AFTER tea.NewProgram so the watchdog can inject
// a StallRecoverMsg on the goroutine-safe program.Send path. The closure is
// the only thing the (tea-agnostic) monitor needs to know.
func (m *Model) SetStallRecoverySink(send func(tea.Msg)) {
	if m.monitor == nil {
		return
	}
	m.monitor.setRecover(func() { send(StallRecoverMsg{}) })
}

// handleStallRecover detaches a stalled agent view back to the board. The
// session keeps running on the daemon; the user can re-enter to resume. A
// no-op outside agent view (a stall on the board has nothing to detach).
func (m *Model) handleStallRecover() (tea.Model, tea.Cmd) {
	if m.mode != ModeAgentView {
		return m, nil
	}
	m.exitToBoard()
	m.notify("Recovered a stalled session view — detached to the board; re-enter to resume")
	return m, m.maybeSetWindowTitle()
}

func (m *Model) pollAgentStatusesAsync() tea.Cmd {
	type paneInfo struct {
		ticketID board.TicketID
		// fileSessionName mirrors OPENKANBAN_SESSION in the live agent's
		// env — what the status hook used to choose its file path. It
		// comes from PaneView.SessionName(), which is sourced from the
		// daemon's SessionInfo at spawn (or resync), so it cannot drift
		// from the env var. Don't substitute ticket.AgentSessionID here:
		// the Claude UUID back-fill (commit c718699) writes that field
		// MID-session, while OPENKANBAN_SESSION stays whatever was baked
		// in at spawn. Using the UUID for the file lookup is the bug
		// this whole struct exists to fix.
		fileSessionName string
		agentType       string
		worktreePath    string
		branchName      string
		agentPort       int
		// agentSessionID is the back-filled Claude/opencode UUID. Used
		// for the opencode HTTP API lookup (where the API id is the
		// match key in the response), NOT for the file lookup.
		agentSessionID  string
		running         bool
		terminalContent string
		lastActivity    time.Time
	}

	var panes []paneInfo
	for ticketID, pane := range m.panes {
		ticket, _ := m.globalStore.Get(ticketID)
		if ticket == nil {
			continue
		}
		worktreePath := pane.GetWorkdir()
		if worktreePath == "" {
			worktreePath = ticket.WorktreePath
		}
		// Snapshot PaneView.SessionName() now (UI goroutine) so the
		// async closure below doesn't reach into the pane from another
		// goroutine.
		fileSessionName := pane.SessionName()
		panes = append(panes, paneInfo{
			ticketID:        ticketID,
			fileSessionName: fileSessionName,
			agentType:       ticket.AgentType,
			worktreePath:    worktreePath,
			branchName:      ticket.BranchName,
			agentPort:       ticket.AgentPort,
			agentSessionID:  ticket.AgentSessionID,
			running:         pane.Running(),
			terminalContent: pane.GetContent(),
			lastActivity:    m.lastPTYActivity[ticketID],
		})
	}

	detector := m.statusDetector
	globalStore := m.globalStore

	return func() tea.Msg {
		results := make(agentStatusResultMsg)
		for _, p := range panes {
			if !p.running {
				results[p.ticketID] = board.AgentNone
				continue
			}

			// Back-fill the agent's persistent session UUID into the
			// ticket if it isn't already pinned. This is unrelated to
			// the file-lookup key (fileSessionName) — see the field
			// comments above. Keeping it here so --resume picks up the
			// UUID on the next spawn / external-resume detection.
			apiSessionID := p.agentSessionID
			if apiSessionID == "" {
				home, _ := os.UserHomeDir()
				// backfillAgentSession enforces the 1:1 invariant via
				// ticketsvc.LinkSession(BestEffort). On claim conflict
				// (UUID already held by a different ticket) the back-fill
				// silently no-ops: returns empty, no save, no purge.
				// See internal/ui/backfill_session.go.
				if id := backfillAgentSession(
					globalStore,
					p.ticketID,
					p.agentType,
					p.worktreePath,
					home,
					agent.FindOpencodeSession,
					agent.FindClaudeSession,
					agent.PurgeClaudePrimingHistory,
				); id != "" {
					apiSessionID = id
				}
			}

			// fileSessionName is what the status hook wrote with. If
			// PaneView didn't have one (legacy spawn path, or a
			// detached/resynced pane that never received a name), fall
			// back to branch / ticketID — same priority the spawn code
			// uses for sessionName.
			fileKey := p.fileSessionName
			if fileKey == "" {
				fileKey = p.branchName
			}
			if fileKey == "" {
				fileKey = string(p.ticketID)
			}

			status := detector.DetectStatusWithActivity(p.agentType, fileKey, apiSessionID, p.worktreePath, p.agentPort, true, p.terminalContent, p.lastActivity)
			results[p.ticketID] = status
		}
		return results
	}
}

func (m *Model) handleTerminalMsg(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	pid, addressed := paneIDOf(msg)
	rearmed := false
	for _, pane := range m.panes {
		if cmd := pane.Update(msg); cmd != nil {
			cmds = append(cmds, cmd)
			// PaneView.Update returns a follow-up readNextMsg Cmd for
			// exactly the messages where it consumed pane output
			// (PaneOutputMsg/PaneAttachedMsg). That Cmd already reads the
			// pane's single-reader teaMsgs channel, so the bridge below
			// must not arm a second reader for this pane. (Which messages
			// re-arm here vs. via the bridge is a partition over the
			// pane-scoped types — keep it in sync with PaneView.Update.)
			if addressed && pane.ID() == pid {
				rearmed = true
			}
		}
	}
	// Re-arm the listener on the addressed pane ONLY when PaneView.Update
	// did not already do so (e.g. PaneRenderTickMsg/PaneDetachedMsg return
	// nil and would otherwise leave the pane with no reader). Double-arming
	// a single-reader channel leaks a permanently-parked reader — and its
	// parent execBatchMsg WaitGroup waiter — per event; see
	// TestHandleTerminalMsg_PaneOutputArmsSingleReader.
	if addressed && !rearmed {
		if pv, exists := m.panes[board.TicketID(pid)]; exists {
			cmds = append(cmds, m.listenPaneMessages(pv))
		}
	}
	// The child may have emitted an OSC title sequence in this batch
	// of output — reflect any change in the host window title. Also
	// runs on RenderTickMsg as a steady-state safety net.
	if cmd := m.maybeSetWindowTitle(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	return m, tea.Batch(cmds...)
}

// paneIDOf returns the PaneID of any daemonclient pane-scoped message,
// or "" for messages that aren't pane-scoped. Lets handleTerminalMsg
// re-arm the right pane's listener without a giant type switch.
func paneIDOf(msg tea.Msg) (string, bool) {
	switch m := msg.(type) {
	case daemonclient.PaneOutputMsg:
		return m.PaneID, true
	case daemonclient.PaneRenderTickMsg:
		return m.PaneID, true
	case daemonclient.PaneAttachedMsg:
		return m.PaneID, true
	case daemonclient.PaneDetachedMsg:
		return m.PaneID, true
	}
	return "", false
}

// listenPaneMessages returns a tea.Cmd that reads one event from pv's
// teaMsgs channel and returns it as a tea.Msg. The model's Update
// re-arms the listener every time it consumes a pane-scoped message
// (see handleTerminalMsg).
//
// The Cmd is also resilient to channel closure (pane Close()) — the
// reader returns PaneExitMsg in that case so the model can clean up
// the entry.
func (m *Model) listenPaneMessages(pv *daemonclient.PaneView) tea.Cmd {
	if pv == nil {
		return nil
	}
	id := pv.ID()
	ch := pv.TeaMessages()
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return daemonclient.PaneExitMsg{PaneID: id, Err: io.EOF}
		}
		return msg
	}
}

type agentStatusMsg time.Time
type agentStatusResultMsg map[board.TicketID]board.AgentStatus
type notificationMsg time.Time
type shutdownCompleteMsg struct{}

// binaryStaleCheckMsg fires every update.BinaryStaleCheckInterval to
// trigger a re-stat of os.Executable() against the captured process
// start time. Handled by the main Update loop, which surfaces a
// one-shot notification when the binary on disk is newer than the
// running process. See checkBinaryStaleness.
type binaryStaleCheckMsg struct{}

type spawnReadyMsg struct {
	ticketID     board.TicketID
	pane         *daemonclient.PaneView
	worktreePath string
	branchName   string
	baseBranch   string
	// notice, if non-empty, is shown via m.notify() once the spawn-ready
	// handler runs. Used to surface a "Brief at tickets/<slug>.md updated."
	// toast for option 'u' (InjectResumeNotice) — the closure that emits
	// this message cannot safely call m.notify() itself, so it routes
	// through this field.
	notice string
}

type spawnErrorMsg struct {
	ticketID board.TicketID
	err      string
}

// spawnUnattachedReadyMsg is the ctrl+space counterpart to spawnReadyMsg: the
// daemon spawned the session but the closure did NOT attach. Its handler
// registers the Unattached pane and leaves the TUI on the board (ModeNormal)
// — it never switches to ModeAgentView, sets focusedPane, or starts a pane
// message listener (the listener begins later, if the user attaches via s/Enter).
type spawnUnattachedReadyMsg struct {
	ticketID     board.TicketID
	pane         *daemonclient.PaneView
	worktreePath string
	branchName   string
	baseBranch   string
}

// attachConflictMsg is emitted when an attach probe is rejected because
// the session is attached in another TUI instance. It carries the pv so
// the Update handler can register the pane (the Owns cold-start fast
// path builds a pane not yet in m.panes) and arm the takeover warning.
type attachConflictMsg struct {
	ticketID board.TicketID
	pv       *daemonclient.PaneView
}

// cyclePeekedMsg is a no-op signal returned after a cycle Peek completes,
// purely to trigger a View() re-render so the freshly-peeked backdrop
// shows. It carries no pane ID so it doesn't arm a pane-message listener.
type cyclePeekedMsg struct{}

func tickAgentStatus(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg {
		return agentStatusMsg(t)
	})
}

// checkBinaryStaleness returns a tea.Cmd that fires a
// binaryStaleCheckMsg after update.BinaryStaleCheckInterval. The Update
// handler re-arms it on every receipt; the work itself (an os.Stat of
// the executable) happens on the bubbletea goroutine and is effectively
// free.
func checkBinaryStaleness() tea.Cmd {
	return tea.Tick(update.BinaryStaleCheckInterval, func(t time.Time) tea.Msg {
		return binaryStaleCheckMsg{}
	})
}

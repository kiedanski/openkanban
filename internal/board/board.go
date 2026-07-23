package board

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

var nonAlphanumericRegex = regexp.MustCompile(`[^a-z0-9-]+`)

func Slugify(s string, maxLen int) string {
	if maxLen <= 0 {
		maxLen = 40
	}

	slug := strings.ToLower(s)

	slug = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		}
		return '-'
	}, slug)

	slug = nonAlphanumericRegex.ReplaceAllString(slug, "-")
	slug = strings.Trim(slug, "-")

	for strings.Contains(slug, "--") {
		slug = strings.ReplaceAll(slug, "--", "-")
	}

	if len(slug) > maxLen {
		slug = slug[:maxLen]
		slug = strings.TrimRight(slug, "-")
	}

	return slug
}

type TicketID string

func NewTicketID() TicketID {
	return TicketID(uuid.New().String())
}

type TicketStatus string

const (
	StatusBacklog    TicketStatus = "backlog"
	StatusNext       TicketStatus = "next"
	StatusInProgress TicketStatus = "in_progress"
	StatusInReview   TicketStatus = "in_review"
	StatusDone       TicketStatus = "done"
	StatusArchived   TicketStatus = "archived"
)

// ParseStatus converts a raw string to a TicketStatus, returning a
// descriptive error for unrecognised values. Used to validate the
// --status flag across the ticket new, list, and move commands.
func ParseStatus(s string) (TicketStatus, error) {
	switch TicketStatus(s) {
	case StatusBacklog, StatusNext, StatusInProgress, StatusInReview, StatusDone, StatusArchived:
		return TicketStatus(s), nil
	default:
		return "", fmt.Errorf("%q is not one of: backlog, next, in_progress, in_review, done, archived", s)
	}
}

// TicketType classifies a ticket by its stage in the
// research → spec → implement → review pipeline. It is orthogonal to
// Status: a ticket has both a Status (which column it sits in) and a Type
// (which agent role spawns for it and which workflow gates apply). The
// empty value is TypeFreeform — today's behavior, no role binding and no
// gates — so tickets created before this field existed keep working.
type TicketType string

const (
	TypeFreeform  TicketType = ""          // default; no role binding, no gates
	TypeResearch  TicketType = "research"  // explore & report (findings.md)
	TypeSpec      TicketType = "spec"      // produce a plan (plan.md)
	TypeImplement TicketType = "implement" // write the code + open the PR
	TypeReview    TicketType = "review"    // critique the diff (review.md)
)

// ParseTicketType converts a raw string to a TicketType, returning a
// descriptive error for unrecognised values. The empty string is valid and
// maps to TypeFreeform (backward-compat with tickets that predate the field);
// the literal "freeform" is accepted as an alias so the CLI can name the same
// option the TUI picker labels "Freeform". Mirrors ParseStatus; used to
// validate the --type flag and .md frontmatter.
func ParseTicketType(s string) (TicketType, error) {
	switch TicketType(s) {
	case TypeFreeform, TypeResearch, TypeSpec, TypeImplement, TypeReview:
		return TicketType(s), nil
	case "freeform":
		return TypeFreeform, nil
	default:
		return "", fmt.Errorf("%q is not one of: research, spec, implement, review, freeform (or empty for freeform)", s)
	}
}

type AgentStatus string

const (
	AgentNone      AgentStatus = "none"
	AgentIdle      AgentStatus = "idle"
	AgentWorking   AgentStatus = "working"
	AgentWaiting   AgentStatus = "waiting"
	AgentCompleted AgentStatus = "completed"
	AgentError     AgentStatus = "error"
	// AgentStuck is a transient daemon-derived state: the daemon's
	// watchdog observed the pane wedged on input backpressure (the child
	// stopped draining stdin). It is NOT a terminal status — it is not
	// persisted as a wrap-up state like AgentCompleted, and clears once
	// the session drains input (or the user destroys it). SetAgentStatus
	// handles it like any other value; the TUI surfaces it as a red card
	// with a recover/destroy modal.
	AgentStuck AgentStatus = "stuck"
	// AgentSubagents marks a foreground agent that is idle but occupied:
	// it spawned background sub-agents and is awaiting them (Claude Code's
	// "✻ Waiting for N background agent(s) to finish"). It is NOT blocked on
	// the user — distinct from AgentWaiting — so the UI renders it calm/gray
	// and Auto-mode (needsAttention) deliberately skips it. Detection-derived
	// from the live screen (no hook writes it); not a terminal state. If
	// Claude's status-line wording drifts, detection fails safe back to
	// today's AgentWaiting behavior — see backgroundAgentSignatures in
	// internal/agent/status.go.
	AgentSubagents AgentStatus = "subagents"
)

type Ticket struct {
	ID          TicketID     `json:"id"`
	ProjectID   string       `json:"project_id"`
	Title       string       `json:"title"`
	Description string       `json:"description,omitempty"`
	Status      TicketStatus `json:"status"`

	UseWorktree  bool   `json:"use_worktree"`
	WorktreePath string `json:"worktree_path,omitempty"`
	BranchName   string `json:"branch_name,omitempty"`
	BaseBranch   string `json:"base_branch,omitempty"`

	AgentType      string      `json:"agent_type,omitempty"`
	AgentStatus    AgentStatus `json:"agent_status"`
	AgentSpawnedAt *time.Time  `json:"agent_spawned_at,omitempty"`
	AgentPort      int         `json:"agent_port,omitempty"`
	AgentSessionID string      `json:"agent_session_id,omitempty"`
	// CreatedBySession is the free-form audit field — e.g., a session
	// name the user typed.
	//
	// The pre-2026-06-17 SessionOwned bool was removed in
	// task/enforce-one-to-one-session: forking was eliminated entirely
	// (every openkanban spawn is migrate-on-resume), so the
	// link-vs-migrate distinction had no readers. Old .md files with
	// `session_owned: true` still parse — see
	// internal/project/ticket_md.go for the dormant frontmatter field
	// preserved for backward-compat.
	CreatedBySession string `json:"created_by_session,omitempty"`

	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	// StatusChangedAt records the last time Status or AgentStatus
	// transitioned. Bumped by SetStatus and SetAgentStatus, not by
	// Touch — ordinary edits (rename, priority) must not move it.
	// Used by the "sort by status change" mode.
	StatusChangedAt *time.Time `json:"status_changed_at,omitempty"`

	Labels   []string          `json:"labels,omitempty"`
	Priority int               `json:"priority,omitempty"`
	Meta     map[string]string `json:"meta,omitempty"`

	// Type is the ticket's pipeline stage (research/spec/implement/review).
	// Empty = TypeFreeform (no role binding, no gates). See TicketType.
	Type TicketType `json:"type,omitempty"`

	// Dependencies - tickets that block this one (informational only, no enforcement)
	BlockedBy []TicketID `json:"blocked_by,omitempty"`
}

func NewTicket(title, projectID string) *Ticket {
	now := time.Now()
	return &Ticket{
		ID:              NewTicketID(),
		ProjectID:       projectID,
		Title:           title,
		Status:          StatusBacklog,
		AgentStatus:     AgentNone,
		UseWorktree:     true,
		Priority:        3,
		CreatedAt:       now,
		UpdatedAt:       now,
		StatusChangedAt: &now,
		Labels:          []string{},
		Meta:            map[string]string{},
	}
}

func (t *Ticket) Touch() {
	t.UpdatedAt = time.Now()
}

func (t *Ticket) SetStatus(status TicketStatus) {
	now := time.Now()
	t.Status = status
	t.UpdatedAt = now
	t.StatusChangedAt = &now

	switch status {
	case StatusInProgress:
		t.StartedAt = &now
	case StatusDone:
		t.CompletedAt = &now
	}

	// Re-entering an active column means a prior terminal "done" no longer
	// describes the ticket; clear the stale done-flags so the board stops
	// badging it as complete. AgentWorking and other live states are left
	// untouched; in_review deliberately keeps the badge.
	switch status {
	case StatusBacklog, StatusNext, StatusInProgress:
		if t.AgentStatus == AgentCompleted {
			t.AgentStatus = AgentNone
		}
		t.CompletedAt = nil
	}
}

// SetAgentStatus updates AgentStatus and stamps the transition.
// Returns true if the status actually changed (caller should
// persist), false on no-op. Mirrors SetStatus for the agent
// dimension.
func (t *Ticket) SetAgentStatus(as AgentStatus) bool {
	if t.AgentStatus == as {
		return false
	}
	now := time.Now()
	t.AgentStatus = as
	t.UpdatedAt = now
	t.StatusChangedAt = &now
	return true
}

type Column struct {
	ID     string       `json:"id"`
	Name   string       `json:"name"`
	Status TicketStatus `json:"status"`
	Color  string       `json:"color"`
	Limit  int          `json:"limit"`
}

func DefaultColumns() []Column {
	return []Column{
		{ID: "backlog", Name: "Backlog", Status: StatusBacklog, Color: "#89b4fa", Limit: 0},
		{ID: "next", Name: "Next", Status: StatusNext, Color: "#94e2d5", Limit: 0},
		{ID: "in-progress", Name: "In Progress", Status: StatusInProgress, Color: "#f9e2af", Limit: 3},
		{ID: "in-review", Name: "In Review", Status: StatusInReview, Color: "#cba6f7", Limit: 0},
		{ID: "done", Name: "Done", Status: StatusDone, Color: "#a6e3a1", Limit: 0},
	}
}

var (
	ErrTicketNotFound = &BoardError{Message: "ticket not found"}
)

type BoardError struct {
	Message string
}

func (e *BoardError) Error() string {
	return e.Message
}

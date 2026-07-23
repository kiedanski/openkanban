package config

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/techdufus/openkanban/internal/board"
)

const defaultGlobalPrompt = `You have been spawned by OpenKanban to work on a ticket.

**Title:** {{.Title}}

**Description:**
{{.Description}}

**Branch:** {{.BranchName}} (from {{.BaseBranch}})

Focus on completing this ticket. Ask clarifying questions if the description is unclear.`

// defaultAgentPrompt is the canonical priming prompt OpenKanban sends to
// every spawned Claude-class agent. It is embedded from
// agent_prompt.tmpl so the source is editable markdown rather than
// backtick-escaped Go string literals. The binary ships with this
// content baked in; per-user customization still flows through
// config.json's `agents.<name>.init_prompt` override.
//
//go:embed agent_prompt.tmpl
var defaultAgentPrompt string

// DefaultAgentPrompt returns the shipped priming template. Exposed so
// tests in sibling packages can render it and assert invariants
// (e.g. that the leading phrases still match agent.ClaudePrimingPrefixes).
func DefaultAgentPrompt() string {
	return defaultAgentPrompt
}

// defaultLeanAgentPrompt is the token-optimized priming template for the
// `claude-lean` preset. It drops the mandatory `/prime` (a lean session has
// no global CLAUDE.md / auto-memory to load) while preserving the brief-read,
// scope/status, and `finishing-an-openkanban-ticket` close-out directives.
// See docs/TOKEN_OPTIMIZATION.md.
//
//go:embed agent_prompt_lean.tmpl
var defaultLeanAgentPrompt string

const defaultAiderPrompt = `OpenKanban Ticket: {{.Title}}

Description:
{{.Description}}

Branch: {{.BranchName}} (from {{.BaseBranch}})

This is your assigned task. Implement what the description specifies.`

// Role headers are prepended to defaultAgentPrompt for the pipeline task
// types (research/spec/implement/review). Layering rather than duplicating
// the ~80-line shared briefing keeps /prime, brief-reading, ticket
// discipline, and the finishing-skill close-out in one place, while giving
// each spawned agent a stage-specific mission — the differentiator the
// board's type→role binding depends on. `implement` uses the plain
// defaultAgentPrompt (it IS the default coding role). See board.TicketType
// and RoleForType.
const researchRoleHeader = "## Role: Research (explore & report)\n\n" +
	"You are the **research** stage of the research → spec → implement → review\n" +
	"pipeline. EXPLORE and REPORT — do not change the system.\n\n" +
	"- Produce a findings document (write `findings.md` at the repo root): what you\n" +
	"  found, with file:line evidence; open questions; and a recommendation.\n" +
	"- DO NOT modify code and DO NOT write an implementation plan — the spec stage\n" +
	"  owns the plan. Stay in plan mode.\n" +
	"- Work read-only: research doesn't commit code. A downstream spec ticket will\n" +
	"  consume your findings.\n\n---\n\n"

const specRoleHeader = "## Role: Spec (plan the work)\n\n" +
	"You are the **spec** stage of the research → spec → implement → review\n" +
	"pipeline. Produce a PLAN — do not implement.\n\n" +
	"- Write a plan document (`plan.md` at the repo root): ordered steps, the exact\n" +
	"  files to touch, risks/edge cases, and a test strategy.\n" +
	"- DO NOT modify code beyond the plan file. Stay in plan mode.\n" +
	"- Consume any upstream `findings.md`. A downstream implement ticket, gated on\n" +
	"  this spec being done, will consume your `plan.md`.\n\n---\n\n"

const reviewRoleHeader = "## Role: Review (critique the diff)\n\n" +
	"You are the **review** stage of the research → spec → implement → review\n" +
	"pipeline. CRITIQUE the implementation — do not re-implement it.\n\n" +
	"- Read the diff against the base branch and produce a review document\n" +
	"  (`review.md` at the repo root): correctness, risks, test gaps, and clear\n" +
	"  verdicts. Fix only trivial nits inline.\n" +
	"- The upstream implement ticket (linked via BlockedBy) carries the branch/diff\n" +
	"  you are reviewing.\n\n---\n\n"

// RoleForType maps a ticket's pipeline Type to the agent config key that
// should spawn for it. TypeImplement maps to the plain "claude" default
// coding role; TypeFreeform (and any unknown value) returns "" so the spawn
// path falls through to the project's configured default agent — preserving
// today's behavior for untyped tickets.
func RoleForType(t board.TicketType) string {
	switch t {
	case board.TypeResearch:
		return "claude-research"
	case board.TypeSpec:
		return "claude-spec"
	case board.TypeImplement:
		return "claude"
	case board.TypeReview:
		return "claude-review"
	default:
		return ""
	}
}

// AgentPriority defines the order in which agents are preferred when auto-detecting.
// The first available agent in this list becomes the default.
var AgentPriority = []string{"opencode", "claude", "claude-custom", "gemini", "codex", "aider"}

// DetectAvailableAgent returns the first agent from the priority list
// whose command is available in PATH. Falls back to the first priority
// agent if none are found (user may install later).
func DetectAvailableAgent(agents map[string]AgentConfig) string {
	for _, name := range AgentPriority {
		if agent, exists := agents[name]; exists {
			if _, err := exec.LookPath(agent.Command); err == nil {
				return name
			}
		}
	}
	// Fallback to first in priority list
	return AgentPriority[0]
}

// Config holds the global application configuration
type Config struct {
	Defaults BoardSettings          `json:"defaults"`
	Agents   map[string]AgentConfig `json:"agents"`
	UI       UIConfig               `json:"ui"`
	Cleanup  CleanupSettings        `json:"cleanup"`
	Behavior BehaviorSettings       `json:"behavior"`
	Daemon   DaemonSettings         `json:"daemon"`
	Opencode OpencodeSettings       `json:"opencode"`
	Keys     map[string]string      `json:"keys,omitempty"`
}

// OpencodeSettings controls OpenCode server integration
type OpencodeSettings struct {
	ServerEnabled  bool `json:"server_enabled"`  // Start opencode server for enhanced status detection
	ServerPort     int  `json:"server_port"`     // Port for opencode server (default: 4096)
	PollInterval   int  `json:"poll_interval"`   // Status polling interval in seconds (default: 1)
	StartupTimeout int  `json:"startup_timeout"` // Server startup timeout in seconds (default: 10)
}

// BoardSettings contains default settings for boards
type BoardSettings struct {
	DefaultAgent     string `json:"default_agent"`
	WorktreeBase     string `json:"worktree_base"`
	AutoSpawnAgent   bool   `json:"auto_spawn_agent"`
	AutoCreateBranch bool   `json:"auto_create_branch"`
	BranchPrefix     string `json:"branch_prefix"`
	BranchNaming     string `json:"branch_naming"`   // "template" | "ai" | "prompt"
	BranchTemplate   string `json:"branch_template"` // e.g., "{prefix}{slug}"
	SlugMaxLength    int    `json:"slug_max_length"` // default: 40
	InitPrompt       string `json:"init_prompt"`
}

// AgentConfig defines how to spawn and monitor an AI agent
type AgentConfig struct {
	Label   string   `json:"label,omitempty"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
	// Enabled is a tri-state override of PATH auto-detection: nil = auto (show
	// when Command is on PATH), &true = force-shown, &false = force-hidden.
	// Controls whether the agent appears in the per-project pin cycle / editor.
	Enabled    *bool             `json:"enabled,omitempty"`
	Env        map[string]string `json:"env"`
	StatusFile string            `json:"status_file"`
	InitPrompt string            `json:"init_prompt"`
}

// IsEnabled reports whether this agent should be offered for selection. An
// explicit Enabled override wins; otherwise it's auto-detected by whether
// Command resolves on PATH (so uninstalled agents like aider/codex don't
// clutter the picker).
func (a AgentConfig) IsEnabled() bool {
	if a.Enabled != nil {
		return *a.Enabled
	}
	_, err := exec.LookPath(a.Command)
	return err == nil
}

// UIConfig holds UI-related preferences
type UIConfig struct {
	Theme           string       `json:"theme"`
	CustomColors    *ThemeColors `json:"custom_colors,omitempty"`
	ShowAgentStatus bool         `json:"show_agent_status"`
	RefreshInterval int          `json:"refresh_interval"`
	ColumnWidth     int          `json:"column_width"`
	TicketHeight    int          `json:"ticket_height"`
	SidebarVisible  bool         `json:"sidebar_visible"`
	ScrollbackLines int          `json:"scrollback_lines"`
}

// CleanupSettings controls cleanup behavior when deleting tickets
type CleanupSettings struct {
	DeleteWorktree       bool `json:"delete_worktree"`        // Remove git worktree on ticket delete
	DeleteBranch         bool `json:"delete_branch"`          // Delete git branch after worktree removal
	ForceWorktreeRemoval bool `json:"force_worktree_removal"` // Force removal even with uncommitted changes
}

// BehaviorSettings controls application behavior preferences
type BehaviorSettings struct {
	ConfirmQuitWithAgents     bool `json:"confirm_quit_with_agents"`    // Prompt before quitting with running agents
	CheckForUpdatesOnLaunch   bool `json:"check_for_updates_on_launch"` // Quick update check before entering the TUI
	ForwardAgentNotifications bool `json:"forward_agent_notifications"` // Re-emit OSC 9 notifications from wrapped agents to the host terminal
}

// DaemonSettings controls how the TUI interacts with openkanbankd at
// launch. Defaults preserve historical behavior — the TUI forks a
// daemon on demand. Set Autostart=false (or pass --no-launch-daemon)
// when openkanbankd is managed externally (e.g. by launchd) so the
// TUI does not race the service for the pidlock.
type DaemonSettings struct {
	Autostart bool `json:"autostart"` // TUI autostarts the daemon if not already running
}

func defaultAgents() map[string]AgentConfig {
	return map[string]AgentConfig{
		"claude": {
			Label:      "Claude (Default)",
			Command:    "claude",
			Args:       []string{"--dangerously-skip-permissions"},
			Env:        map[string]string{},
			StatusFile: ".claude/status.json",
			InitPrompt: defaultAgentPrompt,
		},
		"claude-custom": {
			Label:      "Claude (Custom)",
			Command:    "claude",
			Args:       []string{"--dangerously-skip-permissions"},
			Env:        map[string]string{"CLAUDE_CONFIG_DIR": "~/.claude-personal"},
			StatusFile: ".claude/status.json",
			InitPrompt: defaultAgentPrompt,
		},
		// claude-lean spawns a token-optimized Claude worker (~50% of a
		// default session's context — roughly half; see
		// docs/TOKEN_OPTIMIZATION.md). It points at a slimmed
		// CLAUDE_CONFIG_DIR (no plugins, no global CLAUDE.md, no auto-memory
		// — the user sets ~/.claude-lean up once, same one-time pattern as
		// claude-custom's ~/.claude-personal), disables auto-memory, forbids
		// MCP servers (--strict-mcp-config), and relocates per-machine system-
		// prompt sections into the first user message for cross-session
		// prompt-cache reuse (--exclude-dynamic-system-prompt-sections).
		// It stays capability-complete (all built-in tools, incl. WebSearch);
		// the further ~30% cut via --tools is a documented opt-in (it trades
		// worker capability) — see docs/TOKEN_OPTIMIZATION.md, not baked here.
		// Command:"claude" so it inherits all Claude-class spawn behavior
		// (plan mode, prompt-suggestion disable) through buildSpawnReq's
		// basename switch — no model.go change. Opt in by pinning a project
		// to "claude-lean".
		"claude-lean": {
			Label:   "Claude (Lean)",
			Command: "claude",
			Args: []string{
				"--dangerously-skip-permissions",
				"--strict-mcp-config",
				"--exclude-dynamic-system-prompt-sections",
			},
			Env: map[string]string{
				"CLAUDE_CONFIG_DIR":               "~/.claude-lean",
				"CLAUDE_CODE_DISABLE_AUTO_MEMORY": "1",
			},
			StatusFile: ".claude/status.json",
			InitPrompt: defaultLeanAgentPrompt,
		},
		// Pipeline role agents. All Command:"claude" so they inherit every
		// Claude-class spawn behavior (plan mode, prompt-suggestion disable)
		// through buildSpawnReq's basename switch — no model.go change needed.
		// They differ from the default "claude" only by InitPrompt (a
		// stage-specific role header layered on the shared briefing). Bound to
		// a ticket by its Type via RoleForType; not in AgentPriority, so they
		// never become a project's auto-detected default.
		"claude-research": {
			Label:      "Claude (Research)",
			Command:    "claude",
			Args:       []string{"--dangerously-skip-permissions"},
			Env:        map[string]string{},
			StatusFile: ".claude/status.json",
			InitPrompt: researchRoleHeader + defaultAgentPrompt,
		},
		"claude-spec": {
			Label:      "Claude (Spec)",
			Command:    "claude",
			Args:       []string{"--dangerously-skip-permissions"},
			Env:        map[string]string{},
			StatusFile: ".claude/status.json",
			InitPrompt: specRoleHeader + defaultAgentPrompt,
		},
		"claude-review": {
			Label:      "Claude (Review)",
			Command:    "claude",
			Args:       []string{"--dangerously-skip-permissions"},
			Env:        map[string]string{},
			StatusFile: ".claude/status.json",
			InitPrompt: reviewRoleHeader + defaultAgentPrompt,
		},
		"opencode": {
			Label:      "OpenCode",
			Command:    "opencode",
			Args:       []string{},
			Env:        map[string]string{},
			StatusFile: ".opencode/status.json",
			InitPrompt: defaultAgentPrompt,
		},
		"aider": {
			Label:      "Aider",
			Command:    "aider",
			Args:       []string{"--yes"},
			Env:        map[string]string{},
			StatusFile: "",
			InitPrompt: defaultAiderPrompt,
		},
		"gemini": {
			Label:      "Gemini",
			Command:    "gemini",
			Args:       []string{"--yolo"},
			Env:        map[string]string{},
			StatusFile: "",
			InitPrompt: defaultAgentPrompt,
		},
		"codex": {
			Label:      "Codex",
			Command:    "codex",
			Args:       []string{"--full-auto"},
			Env:        map[string]string{},
			StatusFile: "",
			InitPrompt: defaultAgentPrompt,
		},
	}
}

// DefaultConfig returns the default configuration
func DefaultConfig() *Config {
	agents := defaultAgents()
	return &Config{
		Defaults: BoardSettings{
			DefaultAgent:     DetectAvailableAgent(agents),
			WorktreeBase:     "",
			AutoSpawnAgent:   true,
			AutoCreateBranch: true,
			BranchPrefix:     "task/",
			BranchNaming:     "template",
			BranchTemplate:   "{prefix}{slug}",
			SlugMaxLength:    40,
			InitPrompt:       defaultGlobalPrompt,
		},
		Agents: agents,
		UI: UIConfig{
			Theme:           "catppuccin-mocha",
			ShowAgentStatus: true,
			RefreshInterval: 5,
			ColumnWidth:     40,
			TicketHeight:    4,
			SidebarVisible:  true,
			ScrollbackLines: 10000,
		},
		Cleanup: CleanupSettings{
			DeleteWorktree:       true,
			DeleteBranch:         false,
			ForceWorktreeRemoval: false,
		},
		Behavior: BehaviorSettings{
			ConfirmQuitWithAgents:     true,
			CheckForUpdatesOnLaunch:   true,
			ForwardAgentNotifications: true,
		},
		Daemon: DaemonSettings{
			Autostart: true,
		},
		Opencode: OpencodeSettings{
			ServerEnabled:  true,
			ServerPort:     4096,
			PollInterval:   1,
			StartupTimeout: 10,
		},
	}
}

// ConfigDir returns the configuration directory path.
// Priority: OPENKANBAN_CONFIG_DIR > XDG_CONFIG_HOME/openkanban > ~/.config/openkanban
func ConfigDir() (string, error) {
	// Explicit override (testing, CI, multiple instances)
	if dir := os.Getenv("OPENKANBAN_CONFIG_DIR"); dir != "" {
		return dir, nil
	}

	// XDG standard
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "openkanban"), nil
	}

	// Default fallback
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "openkanban"), nil
}

// guardedRealDirs captures the real openkanban dirs ONCE at init, before
// any test mutates $HOME. NOTE: this comparison is textual (filepath.Clean
// + HasPrefix), not EvalSymlinks-based — acceptable for a test-only guard.
var guardedRealDirs = computeGuardedDirs()

func computeGuardedDirs() []string {
	// Guard the REAL $HOME-based openkanban dirs ONLY — deliberately NOT
	// ConfigDir(), which honors OPENKANBAN_CONFIG_DIR / XDG_CONFIG_HOME.
	// Those env overrides ARE isolation (tests, CI, multi-instance): a write
	// they redirect is intentional and must not trip the guard. We protect
	// the default locations a forgotten/unisolated test would silently
	// corrupt. Keep the status literal in sync with agent.StatusDir()
	// (config must NOT import agent — would cycle).
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{
		filepath.Clean(filepath.Join(home, ".config", "openkanban")),
		filepath.Clean(filepath.Join(home, ".cache", "openkanban")),
		filepath.Clean(filepath.Join(home, ".cache", "openkanban-status")),
	}
}

func underAny(clean string, dirs []string) bool {
	for _, d := range dirs {
		if d != "" && (clean == d || strings.HasPrefix(clean, d+string(os.PathSeparator))) {
			return true
		}
	}
	return false
}

// GuardHomeWrite panics if, while running under `go test`, path is at/under
// a real openkanban dir. It is a NO-OP in production (testing.Testing()==false)
// — it can NEVER fire in the daemon/TUI/CLI. Do NOT widen the detector with
// env checks; that would reintroduce production reachability.
func GuardHomeWrite(path string) {
	if !testing.Testing() {
		return
	}
	if underAny(filepath.Clean(path), guardedRealDirs) {
		panic("openkanban: test wrote under a REAL user dir: " + filepath.Clean(path) +
			" — isolate it: testutil.NewTestEnv(t) or t.Setenv(\"OPENKANBAN_CONFIG_DIR\", t.TempDir()). See internal/CLAUDE.md > Testing.")
	}
}

// ConfigPath returns the default config file path
func ConfigPath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// Load reads configuration from file or returns defaults
func Load(path string) (*Config, error) {
	if path == "" {
		var err error
		path, err = ConfigPath()
		if err != nil {
			return DefaultConfig(), nil
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultConfig(), nil
		}
		return nil, err
	}

	cfg := DefaultConfig()
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, err
	}

	cfg.mergeAgentDefaults()

	return cfg, nil
}

// ReloadFromDisk re-reads the config from disk and replaces the
// contents of c in place. Existing pointers to c remain valid.
// Existing agent processes are unaffected; only newly spawned agents
// see updated AgentConfig values.
//
// Safe to call from a Bubble Tea Update goroutine.
func (c *Config) ReloadFromDisk() error {
	fresh, err := Load("")
	if err != nil {
		return err
	}
	*c = *fresh
	return nil
}

func (c *Config) mergeAgentDefaults() {
	defaults := DefaultConfig()

	if c.Agents == nil {
		c.Agents = make(map[string]AgentConfig)
	}

	for name, defaultCfg := range defaults.Agents {
		if userCfg, exists := c.Agents[name]; exists {
			// Backfill missing sub-fields for existing user agents.
			if userCfg.Label == "" {
				userCfg.Label = defaultCfg.Label
			}
			if userCfg.StatusFile == "" {
				userCfg.StatusFile = defaultCfg.StatusFile
			}
			if userCfg.Env == nil {
				userCfg.Env = defaultCfg.Env
			}
			if userCfg.InitPrompt == "" {
				userCfg.InitPrompt = defaultCfg.InitPrompt
			}
			c.Agents[name] = userCfg
		} else {
			// Add brand-new default agent keys missing from the user config.
			c.Agents[name] = defaultCfg
		}
	}
}

func (c *Config) GetEffectiveInitPrompt(agentType string) string {
	if agentCfg, ok := c.Agents[agentType]; ok && agentCfg.InitPrompt != "" {
		return agentCfg.InitPrompt
	}
	if c.Defaults.InitPrompt != "" {
		return c.Defaults.InitPrompt
	}
	return defaultGlobalPrompt
}

func (c *Config) GetTheme() Theme {
	return GetTheme(c.UI.Theme, c.UI.CustomColors)
}

// Save writes configuration to file
func (c *Config) Save(path string) error {
	if path == "" {
		var err error
		path, err = ConfigPath()
		if err != nil {
			return err
		}
	}

	GuardHomeWrite(path)

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0644)
}

// LoadWithValidation loads config and returns structured validation result
func LoadWithValidation(path string) (*Config, *ValidationResult, error) {
	if path == "" {
		var err error
		path, err = ConfigPath()
		if err != nil {
			return DefaultConfig(), nil, nil
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			cfg := DefaultConfig()
			return cfg, cfg.Validate(), nil
		}
		return nil, nil, err
	}

	cfg := DefaultConfig()
	if err := json.Unmarshal(data, cfg); err != nil {
		result := &ValidationResult{}
		if jsonErr := formatJSONError(err); jsonErr != "" {
			result.AddError("json", "", jsonErr, nil)
		} else {
			result.AddError("json", "", err.Error(), nil)
		}
		return nil, result, err
	}

	cfg.mergeAgentDefaults()
	result := cfg.Validate()

	return cfg, result, nil
}

// formatJSONError attempts to provide better JSON error context
func formatJSONError(err error) string {
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return fmt.Sprintf("invalid JSON at byte %d: %s", syntaxErr.Offset, syntaxErr.Error())
	}

	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return fmt.Sprintf("field %q expects %s but got %s", typeErr.Field, typeErr.Type, typeErr.Value)
	}

	return ""
}

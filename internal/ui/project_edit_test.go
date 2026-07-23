package ui

import (
	"testing"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/config"
	"github.com/techdufus/openkanban/internal/project"
)

// editTextField simulates the real focus→edit→leave cycle for one text field:
// move the cursor there (seeding peInput), set a value, then write it back.
func editTextField(m *Model, field int, value string) {
	m.peField = field
	m.peSyncFromField()
	m.peInput.SetValue(value)
	m.peSyncToField()
}

// TestProjectEditForm_SavesAgentsAndPin pins the unified editor's persistence:
// editing an agent's env, toggling its enabled override, renaming the project,
// and setting the pin must persist to BOTH config.json (agents) and
// projects.json (project). A fresh registry load confirms the project bits hit
// disk. Non-vacuous: the default claude-custom env is ~/.claude-personal; the
// test changes it and asserts the new value.
func TestProjectEditForm_SavesAgentsAndPin(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	cfg := config.DefaultConfig() // seeds claude + claude-custom + others
	proj := project.NewProject("OldName", t.TempDir())
	reg := &project.ProjectRegistry{Projects: map[string]*project.Project{proj.ID: proj}}
	gs := project.NewGlobalTicketStore(nil)
	gs.AddProject(proj)

	cols := board.DefaultColumns()
	m := &Model{
		config:          cfg,
		projectRegistry: reg,
		globalStore:     gs,
		columns:         cols,
		columnTickets:   make([][]*board.Ticket, len(cols)),
		columnOffsets:   make([]int, len(cols)),
		width:           120,
		height:          40,
	}

	m.editProject(proj)

	idx := -1
	for i, r := range m.peAgents {
		if r.key == "claude-custom" {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatal("claude-custom not present in editor rows")
	}
	base := peAgentBaseField + idx*peFieldsPerAgent

	// Edit the custom agent's env (text field, via the real sync cycle).
	editTextField(m, base+peSubEnv, "CLAUDE_CONFIG_DIR=~/.claude-work")
	// Rename the project (field 0).
	editTextField(m, 0, "NewName")
	// Toggle the custom agent's enabled override to "off" (selector field).
	m.peField = base + peSubEnabled
	for m.peAgents[idx].enabled != "off" {
		m.peCycleSelector(1)
	}
	// Pin the project to the custom agent (selector field 1).
	m.peField = 1
	for m.peProjectAgent != "claude-custom" {
		m.peCycleSelector(1)
	}

	if _, cmd := m.saveProjectEditForm(); cmd != nil {
		// save returns nil cmd on success; a non-nil here would be unexpected
		_ = cmd
	}

	// config.json side: agent env + enabled override persisted in memory.
	cc := m.config.Agents["claude-custom"]
	if cc.Env["CLAUDE_CONFIG_DIR"] != "~/.claude-work" {
		t.Errorf("agent env not saved: got %v", cc.Env)
	}
	if cc.Enabled == nil || *cc.Enabled != false {
		t.Errorf("agent enabled override not saved: got %v", cc.Enabled)
	}

	// projects.json side: name + pin persisted to disk.
	reloaded, err := project.LoadRegistry()
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	got := reloaded.Projects[proj.ID]
	if got == nil {
		t.Fatal("project missing after reload")
	}
	if got.Name != "NewName" {
		t.Errorf("project name not persisted: got %q", got.Name)
	}
	if got.Settings.DefaultAgent != "claude-custom" {
		t.Errorf("project pin not persisted: got %q", got.Settings.DefaultAgent)
	}
}

// TestProjectEditForm_SavesInitPromptFile pins the new prompt-file path field:
// editing the agent's `prompt:` field must persist InitPromptFile into
// config.Agents while preserving the agent's existing (embedded) InitPrompt.
func TestProjectEditForm_SavesInitPromptFile(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	cfg := config.DefaultConfig()
	proj := project.NewProject("PromptProj", t.TempDir())
	reg := &project.ProjectRegistry{Projects: map[string]*project.Project{proj.ID: proj}}
	gs := project.NewGlobalTicketStore(nil)
	gs.AddProject(proj)

	cols := board.DefaultColumns()
	m := &Model{
		config:          cfg,
		projectRegistry: reg,
		globalStore:     gs,
		columns:         cols,
		columnTickets:   make([][]*board.Ticket, len(cols)),
		columnOffsets:   make([]int, len(cols)),
		width:           120,
		height:          40,
	}
	m.editProject(proj)

	idx := -1
	for i, r := range m.peAgents {
		if r.key == "claude" {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatal("claude not present in editor rows")
	}
	// The default claude agent ships the embedded template in init_prompt;
	// capture it to assert the file-link edit doesn't clobber it.
	wantInitPrompt := m.peAgents[idx].initPrompt
	if wantInitPrompt == "" {
		t.Fatal("expected claude agent to carry a non-empty embedded init_prompt")
	}

	base := peAgentBaseField + idx*peFieldsPerAgent
	editTextField(m, base+peSubInitPromptFile, "~/prompts/claude.md")

	m.saveProjectEditForm()

	got := m.config.Agents["claude"]
	if got.InitPromptFile != "~/prompts/claude.md" {
		t.Errorf("InitPromptFile not saved: got %q", got.InitPromptFile)
	}
	if got.InitPrompt != wantInitPrompt {
		t.Errorf("InitPrompt not preserved through save: got %q want %q", got.InitPrompt, wantInitPrompt)
	}
}

// TestProjectEditForm_SavesModel verifies that saveProjectEditForm persists
// proj.Settings.Model to disk and mirrors it onto the live store pointer.
// Also asserts that leading/trailing whitespace is trimmed.
func TestProjectEditForm_SavesModel(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	cfg := config.DefaultConfig()
	proj := project.NewProject("ModelProj", t.TempDir())
	reg := &project.ProjectRegistry{Projects: map[string]*project.Project{proj.ID: proj}}
	gs := project.NewGlobalTicketStore(nil)
	gs.AddProject(proj)

	cols := board.DefaultColumns()
	m := &Model{
		config:          cfg,
		projectRegistry: reg,
		globalStore:     gs,
		columns:         cols,
		columnTickets:   make([][]*board.Ticket, len(cols)),
		columnOffsets:   make([]int, len(cols)),
		width:           120,
		height:          40,
	}

	m.editProject(proj)

	// Case 1: plain model value.
	m.peModel = "opus"
	if _, cmd := m.saveProjectEditForm(); cmd != nil {
		_ = cmd
	}

	reloaded, err := project.LoadRegistry()
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	got := reloaded.Projects[proj.ID]
	if got == nil {
		t.Fatal("project missing after reload")
	}
	if got.Settings.Model != "opus" {
		t.Errorf("model not persisted: got %q, want %q", got.Settings.Model, "opus")
	}
	// Also verify the live store pointer was updated.
	if live := gs.GetProject(proj.ID); live != nil {
		if live.Settings.Model != "opus" {
			t.Errorf("live store model not updated: got %q, want %q", live.Settings.Model, "opus")
		}
	}

	// Case 2: whitespace is trimmed.
	m.editProject(proj)
	m.peModel = "  sonnet  "
	if _, cmd := m.saveProjectEditForm(); cmd != nil {
		_ = cmd
	}

	reloaded2, err := project.LoadRegistry()
	if err != nil {
		t.Fatalf("LoadRegistry (case 2): %v", err)
	}
	got2 := reloaded2.Projects[proj.ID]
	if got2 == nil {
		t.Fatal("project missing after reload (case 2)")
	}
	if got2.Settings.Model != "sonnet" {
		t.Errorf("model whitespace not trimmed: got %q, want %q", got2.Settings.Model, "sonnet")
	}
}

// TestProjectEditForm_FocusedLine pins the peFocusedLine return values so a
// future field insertion (peAgentBaseField bump) is caught immediately.
func TestProjectEditForm_FocusedLine(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents = map[string]config.AgentConfig{
		"claude": {Command: "claude", Label: "Claude"},
	}
	m := &Model{config: cfg}

	cases := []struct {
		field int
		want  int
	}{
		{0, 3},  // project name
		{1, 4},  // default agent
		{2, 5},  // model
		{3, 6},  // ignore-briefs (new field)
		{4, 10}, // agent 0 sub 0 (peAgentBaseField=4, base line=10)
		{5, 11}, // agent 0 sub 1
	}
	for _, tc := range cases {
		m.peField = tc.field
		got := m.peFocusedLine()
		if got != tc.want {
			t.Errorf("peFocusedLine(field=%d) = %d, want %d", tc.field, got, tc.want)
		}
	}
}

// TestProjectEditForm_SavesIgnoreBriefs verifies that saveProjectEditForm
// persists IgnoreTicketBriefs to disk and mirrors it onto the live store.
func TestProjectEditForm_SavesIgnoreBriefs(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	cfg := config.DefaultConfig()
	proj := project.NewProject("BriefsProj", t.TempDir())
	reg := &project.ProjectRegistry{Projects: map[string]*project.Project{proj.ID: proj}}
	gs := project.NewGlobalTicketStore(nil)
	gs.AddProject(proj)

	cols := board.DefaultColumns()
	m := &Model{
		config:          cfg,
		projectRegistry: reg,
		globalStore:     gs,
		columns:         cols,
		columnTickets:   make([][]*board.Ticket, len(cols)),
		columnOffsets:   make([]int, len(cols)),
		width:           120,
		height:          40,
	}

	m.editProject(proj)
	m.peBriefs = "ignore"
	if _, cmd := m.saveProjectEditForm(); cmd != nil {
		_ = cmd
	}

	reloaded, err := project.LoadRegistry()
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	got := reloaded.Projects[proj.ID]
	if got == nil {
		t.Fatal("project missing after reload")
	}
	if !got.Settings.IgnoreTicketBriefs {
		t.Error("IgnoreTicketBriefs not persisted: want true, got false")
	}
	if live := gs.GetProject(proj.ID); live != nil {
		if !live.Settings.IgnoreTicketBriefs {
			t.Error("live store IgnoreTicketBriefs not updated: want true, got false")
		}
	}

	// Flip back to "land" and verify it clears the flag.
	m.editProject(got)
	m.peBriefs = "land"
	if _, cmd := m.saveProjectEditForm(); cmd != nil {
		_ = cmd
	}
	reloaded2, _ := project.LoadRegistry()
	if got2 := reloaded2.Projects[proj.ID]; got2 != nil && got2.Settings.IgnoreTicketBriefs {
		t.Error("IgnoreTicketBriefs should be false after setting to 'land'")
	}
}

// TestEnabledAgentNames_FiltersByPath pins that the pin cycle only offers
// enabled agents: an explicit Enabled=&false hides one even though its command
// exists, and an Enabled=&true shows one whose command is absent.
func TestEnabledAgentNames_FiltersByPath(t *testing.T) {
	tr, fa := true, false
	m := &Model{config: &config.Config{Agents: map[string]config.AgentConfig{
		"sh-on":    {Command: "sh", Enabled: &tr},
		"sh-off":   {Command: "sh", Enabled: &fa},
		"ghost":    {Command: "definitely-not-a-real-binary-xyzzy"}, // auto → off
		"ghost-on": {Command: "definitely-not-a-real-binary-xyzzy", Enabled: &tr},
	}}}

	got := map[string]bool{}
	for _, n := range m.enabledAgentNames() {
		got[n] = true
	}
	if !got["sh-on"] || got["sh-off"] {
		t.Errorf("override not honored: %v", got)
	}
	if got["ghost"] {
		t.Errorf("auto-detect should hide a missing command: %v", got)
	}
	if !got["ghost-on"] {
		t.Errorf("force-on should show even a missing command: %v", got)
	}
}

package ui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/techdufus/openkanban/internal/config"
	"github.com/techdufus/openkanban/internal/project"
)

// ModeEditProject (the `e` key on a focused sidebar project) is a unified
// editor for BOTH project details (name + pinned agent → projects.json) and the
// SHARED agent registry (enable/disable, label, command, args, env, prompt-file
// → config.json). Editing two config files in one screen is intentional — it
// keeps the TUI simple. The per-project bits persist via registry.Update; the
// agent bits via config.Save.

// peAgentRow is the working copy of one agent while the editor is open. Edits
// stay here until Ctrl+S writes them back to m.config.Agents.
type peAgentRow struct {
	key            string
	label          string
	command        string
	args           string // space-joined
	env            string // "K=V, K=V" (keys sorted for stable display)
	enabled        string // "auto" | "on" | "off"
	statusFile     string // preserved verbatim
	initPrompt     string // preserved verbatim
	initPromptFile string // editable: path to a file whose contents become the init prompt
}

// Flat field layout for the editor cursor (m.peField):
//
//	0                                        = project name (text)
//	1                                        = project default agent (selector)
//	2                                        = project model override (text)
//	3                                        = ignore ticket briefs (selector: land/ignore)
//	peAgentBaseField + i*peFieldsPerAgent + sub = agent i's fields
//	    sub: 0 enabled(sel) 1 label 2 command 3 args 4 env 5 prompt-file
const peFieldsPerAgent = 6

// peAgentBaseField is the flat field index where agent rows begin.
// Inserting a new project-level field above bumps this; update in one place.
const peAgentBaseField = 4

const (
	peSubEnabled        = 0
	peSubLabel          = 1
	peSubCommand        = 2
	peSubArgs           = 3
	peSubEnv            = 4
	peSubInitPromptFile = 5
)

func (m *Model) peFieldCount() int { return peAgentBaseField + len(m.peAgents)*peFieldsPerAgent }

// peLocate maps a flat field index to (isAgent, agentIdx, sub).
func (m *Model) peLocate(field int) (isAgent bool, idx, sub int) {
	if field < peAgentBaseField {
		return false, 0, field
	}
	rel := field - peAgentBaseField
	return true, rel / peFieldsPerAgent, rel % peFieldsPerAgent
}

// peFieldIsText reports whether a field is a free-text input (vs a selector).
func (m *Model) peFieldIsText(field int) bool {
	if field == 0 {
		return true // name
	}
	if field == 1 {
		return false // default-agent selector
	}
	if field == 2 {
		return true // model override (text)
	}
	if field == 3 {
		return false // ignore-briefs selector
	}
	_, _, sub := m.peLocate(field)
	return sub != peSubEnabled
}

// editProject opens the editor for proj (mirrors editTicket's seeding).
func (m *Model) editProject(proj *project.Project) (tea.Model, tea.Cmd) {
	if proj == nil {
		m.notify("No project selected")
		return m, nil
	}
	m.mode = ModeEditProject
	m.editingProjectID = proj.ID
	m.peName = proj.Name
	m.peProjectAgent = proj.Settings.DefaultAgent
	m.peModel = proj.Settings.Model
	if proj.Settings.IgnoreTicketBriefs {
		m.peBriefs = "ignore"
	} else {
		m.peBriefs = "land"
	}
	m.peAgents = m.buildPeAgents()
	m.peField = 0
	m.peScrollOffset = 0

	ti := textinput.New()
	ti.CharLimit = 512
	ti.Width = 44
	m.peInput = ti
	m.peSyncFromField()
	return m, m.peInput.Cursor.BlinkCmd()
}

// buildPeAgents snapshots the agent registry into editable rows, ordered by
// AgentPriority (configured agents first, then any extras alphabetically).
func (m *Model) buildPeAgents() []peAgentRow {
	seen := map[string]bool{}
	var rows []peAgentRow
	add := func(key string) {
		cfg, ok := m.config.Agents[key]
		if !ok || seen[key] {
			return
		}
		seen[key] = true
		rows = append(rows, peAgentRow{
			key:            key,
			label:          cfg.Label,
			command:        cfg.Command,
			args:           strings.Join(cfg.Args, " "),
			env:            peJoinEnv(cfg.Env),
			enabled:        peEnabledStr(cfg.Enabled),
			statusFile:     cfg.StatusFile,
			initPrompt:     cfg.InitPrompt,
			initPromptFile: cfg.InitPromptFile,
		})
	}
	for _, key := range config.AgentPriority {
		add(key)
	}
	extras := make([]string, 0)
	for key := range m.config.Agents {
		if !seen[key] {
			extras = append(extras, key)
		}
	}
	sort.Strings(extras)
	for _, key := range extras {
		add(key)
	}
	return rows
}

// peSyncFromField seeds peInput from the working value of the current field
// (text fields) or blurs it (selectors).
func (m *Model) peSyncFromField() {
	if !m.peFieldIsText(m.peField) {
		m.peInput.Blur()
		return
	}
	m.peInput.SetValue(m.peTextValue(m.peField))
	m.peInput.CursorEnd()
	m.peInput.Focus()
}

// peSyncToField writes peInput's value back into the working copy for the
// current field (no-op for selectors).
func (m *Model) peSyncToField() {
	if !m.peFieldIsText(m.peField) {
		return
	}
	val := m.peInput.Value()
	if m.peField == 0 {
		m.peName = val
		return
	}
	if m.peField == 2 {
		m.peModel = val
		return
	}
	_, idx, sub := m.peLocate(m.peField)
	if idx < 0 || idx >= len(m.peAgents) {
		return
	}
	switch sub {
	case peSubLabel:
		m.peAgents[idx].label = val
	case peSubCommand:
		m.peAgents[idx].command = val
	case peSubArgs:
		m.peAgents[idx].args = val
	case peSubEnv:
		m.peAgents[idx].env = val
	case peSubInitPromptFile:
		m.peAgents[idx].initPromptFile = val
	}
}

// peTextValue returns the current working-copy string for a text field.
func (m *Model) peTextValue(field int) string {
	if field == 0 {
		return m.peName
	}
	if field == 2 {
		return m.peModel
	}
	_, idx, sub := m.peLocate(field)
	if idx < 0 || idx >= len(m.peAgents) {
		return ""
	}
	r := m.peAgents[idx]
	switch sub {
	case peSubLabel:
		return r.label
	case peSubCommand:
		return r.command
	case peSubArgs:
		return r.args
	case peSubEnv:
		return r.env
	case peSubInitPromptFile:
		return r.initPromptFile
	}
	return ""
}

// handleProjectEditForm dispatches keys while ModeEditProject is active.
func (m *Model) handleProjectEditForm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "ctrl+c":
		m.mode = ModeNormal
		m.peInput.Blur()
		m.editingProjectID = ""
		return m, nil
	case "ctrl+s":
		m.peSyncToField()
		return m.saveProjectEditForm()
	case "tab", "down":
		m.peSyncToField()
		m.peField = (m.peField + 1) % m.peFieldCount()
		m.peSyncFromField()
		return m, nil
	case "shift+tab", "up":
		m.peSyncToField()
		m.peField = (m.peField - 1 + m.peFieldCount()) % m.peFieldCount()
		m.peSyncFromField()
		return m, nil
	}

	// Model field (2): left/right cycle presets before forwarding to text input.
	if m.peField == 2 {
		switch msg.String() {
		case "left":
			m.peCycleModel(-1)
			return m, nil
		case "right":
			m.peCycleModel(1)
			return m, nil
		}
	}

	// Selector fields consume left/right to cycle their value.
	if !m.peFieldIsText(m.peField) {
		switch msg.String() {
		case "left", "h":
			m.peCycleSelector(-1)
		case "right", "l":
			m.peCycleSelector(1)
		}
		return m, nil
	}

	// Text field: forward everything else to the input.
	var cmd tea.Cmd
	m.peInput, cmd = m.peInput.Update(msg)
	return m, cmd
}

// peCycleModel cycles m.peModel through the preset model list by dir (+1/-1).
func (m *Model) peCycleModel(dir int) {
	presets := []string{"", "opus", "opusplan", "sonnet"}
	cur := 0
	for i, p := range presets {
		if p == m.peModel {
			cur = i
			break
		}
	}
	m.peModel = presets[((cur+dir)%len(presets)+len(presets))%len(presets)]
	m.peInput.SetValue(m.peModel)
}

// peCycleSelector advances the current selector field by dir (+1/-1).
func (m *Model) peCycleSelector(dir int) {
	if m.peField == 1 { // project default agent — cycle over all agent keys
		keys := make([]string, len(m.peAgents))
		for i, r := range m.peAgents {
			keys[i] = r.key
		}
		if len(keys) == 0 {
			return
		}
		cur := 0
		for i, k := range keys {
			if k == m.peProjectAgent {
				cur = i
				break
			}
		}
		m.peProjectAgent = keys[((cur+dir)%len(keys)+len(keys))%len(keys)]
		return
	}
	if m.peField == 3 { // ignore-briefs toggle
		if m.peBriefs == "land" {
			m.peBriefs = "ignore"
		} else {
			m.peBriefs = "land"
		}
		return
	}
	_, idx, sub := m.peLocate(m.peField)
	if sub == peSubEnabled && idx >= 0 && idx < len(m.peAgents) {
		order := []string{"auto", "on", "off"}
		cur := 0
		for i, s := range order {
			if s == m.peAgents[idx].enabled {
				cur = i
				break
			}
		}
		m.peAgents[idx].enabled = order[((cur+dir)%3+3)%3]
	}
}

// saveProjectEditForm persists project name + pin (projects.json) and the agent
// registry (config.json), then returns to the board.
func (m *Model) saveProjectEditForm() (tea.Model, tea.Cmd) {
	name := strings.TrimSpace(m.peName)
	if name == "" {
		m.notify("Project name cannot be empty")
		return m, nil
	}

	// Agents → config.Agents (preserve untouched fields; require a command).
	for _, r := range m.peAgents {
		cmd := strings.TrimSpace(r.command)
		if cmd == "" {
			m.notify("Agent '" + r.key + "': command cannot be empty")
			return m, nil
		}
		m.config.Agents[r.key] = config.AgentConfig{
			Label:          strings.TrimSpace(r.label),
			Command:        cmd,
			Args:           strings.Fields(r.args),
			Enabled:        peEnabledPtr(r.enabled),
			Env:            peParseEnv(r.env),
			StatusFile:     r.statusFile,
			InitPrompt:     r.initPrompt, // preserved verbatim (not an editable field)
			InitPromptFile: strings.TrimSpace(r.initPromptFile),
		}
	}
	if err := m.config.Save(""); err != nil {
		m.notify("Failed to save agents: " + err.Error())
		return m, nil
	}

	// Project name + pin → registry.
	if proj, err := m.projectRegistry.Get(m.editingProjectID); err == nil && proj != nil {
		proj.Name = name
		proj.Settings.DefaultAgent = m.peProjectAgent
		proj.Settings.Model = strings.TrimSpace(m.peModel)
		proj.Settings.IgnoreTicketBriefs = (m.peBriefs == "ignore")
		if err := m.projectRegistry.Update(proj); err != nil {
			m.notify("Failed to save project: " + err.Error())
			return m, nil
		}
		// Mirror into the live store pointer so the board reflects it now.
		if live := m.globalStore.GetProject(m.editingProjectID); live != nil {
			live.Name = name
			live.Settings.DefaultAgent = m.peProjectAgent
			live.Settings.Model = strings.TrimSpace(m.peModel)
			live.Settings.IgnoreTicketBriefs = (m.peBriefs == "ignore")
		}
	}

	m.mode = ModeNormal
	m.peInput.Blur()
	m.editingProjectID = ""
	m.refreshColumnTickets()
	m.notify("Saved project: " + name)
	return m, nil
}

// --- value (de)serialization helpers ---

func peJoinEnv(e map[string]string) string {
	if len(e) == 0 {
		return ""
	}
	keys := make([]string, 0, len(e))
	for k := range e {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+e[k])
	}
	return strings.Join(parts, ", ")
}

func peParseEnv(s string) map[string]string {
	out := map[string]string{}
	for _, kv := range strings.Split(s, ",") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		if i := strings.Index(kv, "="); i > 0 {
			out[strings.TrimSpace(kv[:i])] = strings.TrimSpace(kv[i+1:])
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func peEnabledStr(b *bool) string {
	if b == nil {
		return "auto"
	}
	if *b {
		return "on"
	}
	return "off"
}

func peEnabledPtr(s string) *bool {
	switch s {
	case "on":
		t := true
		return &t
	case "off":
		f := false
		return &f
	}
	return nil // "auto"
}

// renderProjectEditForm renders the unified editor. Field rows that are the
// active cursor get a ▸ marker; text fields show the live input.
func (m *Model) renderProjectEditForm() string {
	c := m.colors
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(c.primary)
	sectionStyle := lipgloss.NewStyle().Bold(true).Foreground(c.secondary)
	labelStyle := lipgloss.NewStyle().Foreground(c.subtext)
	activeLabel := lipgloss.NewStyle().Bold(true).Foreground(c.info)
	dimStyle := lipgloss.NewStyle().Foreground(c.muted)
	valStyle := lipgloss.NewStyle().Foreground(c.text)

	cursor := func(field int) string {
		if m.peField == field {
			return lipgloss.NewStyle().Foreground(c.info).Render("▸ ")
		}
		return "  "
	}
	lbl := func(field int, s string) string {
		if m.peField == field {
			return activeLabel.Render(s)
		}
		return labelStyle.Render(s)
	}
	// textOrValue renders the live input when focused, else the stored value.
	textOrValue := func(field int) string {
		if m.peField == field {
			return m.peInput.View()
		}
		v := m.peTextValue(field)
		if v == "" {
			return dimStyle.Render("(empty)")
		}
		return valStyle.Render(v)
	}

	var b []string
	b = append(b, titleStyle.Render("Edit Project"))
	b = append(b, "")
	b = append(b, sectionStyle.Render("Project"))
	b = append(b, cursor(0)+lbl(0, "Name:    ")+textOrValue(0))
	// default-agent selector
	agentDisp := m.peProjectAgent
	if agentDisp == "" {
		agentDisp = dimStyle.Render("(unpinned — will refuse to spawn)")
	} else {
		agentDisp = valStyle.Render(m.agentLabel(m.peProjectAgent))
	}
	b = append(b, cursor(1)+lbl(1, "Agent:   ")+agentDisp+dimStyle.Render("  ◀ ▶"))
	b = append(b, cursor(2)+lbl(2, "Model:   ")+textOrValue(2)+dimStyle.Render("  ←/→ presets · blank = claude default"))
	b = append(b, cursor(3)+lbl(3, "Briefs:  ")+valStyle.Render(m.peBriefs)+dimStyle.Render("  ◀ ▶ · ignore = keep tickets/ out of git"))
	b = append(b, "")
	b = append(b, sectionStyle.Render("Agents (shared across all projects)"))
	b = append(b, dimStyle.Render("  enabled: auto = show when on PATH"))

	for i, r := range m.peAgents {
		base := peAgentBaseField + i*peFieldsPerAgent
		// header line: enabled selector + key + label
		en := r.enabled
		enStyle := dimStyle
		if en == "on" {
			enStyle = lipgloss.NewStyle().Foreground(c.success)
		} else if en == "off" {
			enStyle = lipgloss.NewStyle().Foreground(c.err)
		}
		head := cursor(base+peSubEnabled) + enStyle.Render(fmt.Sprintf("[%s]", en))
		if m.peField == base+peSubEnabled {
			head += dimStyle.Render(" ◀ ▶")
		}
		head += "  " + valStyle.Render(r.key)
		b = append(b, head)
		b = append(b, cursor(base+peSubLabel)+lbl(base+peSubLabel, "  label:   ")+textOrValue(base+peSubLabel))
		b = append(b, cursor(base+peSubCommand)+lbl(base+peSubCommand, "  command: ")+textOrValue(base+peSubCommand))
		b = append(b, cursor(base+peSubArgs)+lbl(base+peSubArgs, "  args:    ")+textOrValue(base+peSubArgs))
		b = append(b, cursor(base+peSubEnv)+lbl(base+peSubEnv, "  env:     ")+textOrValue(base+peSubEnv))
		b = append(b, cursor(base+peSubInitPromptFile)+lbl(base+peSubInitPromptFile, "  prompt:  ")+textOrValue(base+peSubInitPromptFile)+dimStyle.Render("  file path → init prompt (~ / rel to config dir)"))
	}

	b = append(b, "")
	b = append(b, dimStyle.Render("Tab/↑↓ move · ←/→ toggle · Ctrl+S save · Esc cancel"))

	// Simple scroll: keep the focused field's row in view within the height.
	lines := b
	maxH := m.height - 4
	if maxH > 4 && len(lines) > maxH {
		// Estimate the focused line: project rows occupy fixed offsets.
		focusLine := m.peFocusedLine()
		if focusLine < m.peScrollOffset {
			m.peScrollOffset = focusLine
		} else if focusLine >= m.peScrollOffset+maxH {
			m.peScrollOffset = focusLine - maxH + 1
		}
		if m.peScrollOffset < 0 {
			m.peScrollOffset = 0
		}
		end := m.peScrollOffset + maxH
		if end > len(lines) {
			end = len(lines)
		}
		lines = lines[m.peScrollOffset:end]
	}

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(c.primary).
		Padding(1, 2).
		Render(strings.Join(lines, "\n"))
}

// peFocusedLine returns the rendered line index of the current field (header
// rows above the agents add a fixed offset).
func (m *Model) peFocusedLine() int {
	// Layout: 0=title, 1=blank, 2="Project", 3=name(0), 4=agent(1), 5=model(2),
	// 6=briefs(3), 7=blank, 8="Agents", 9=hint, then agents start at 10
	// (peFieldsPerAgent lines each — one rendered row per sub-field).
	if m.peField == 0 {
		return 3
	}
	if m.peField == 1 {
		return 4
	}
	if m.peField == 2 {
		return 5
	}
	if m.peField == 3 {
		return 6
	}
	_, idx, sub := m.peLocate(m.peField)
	return 10 + idx*peFieldsPerAgent + sub
}

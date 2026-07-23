package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	knownAgents := map[string]bool{"opencode": true, "claude": true, "gemini": true, "codex": true, "aider": true}
	if !knownAgents[cfg.Defaults.DefaultAgent] {
		t.Errorf("Defaults.DefaultAgent = %q; want one of opencode, claude, gemini, codex, aider", cfg.Defaults.DefaultAgent)
	}

	if cfg.Defaults.BranchPrefix != "task/" {
		t.Errorf("Defaults.BranchPrefix = %q; want %q", cfg.Defaults.BranchPrefix, "task/")
	}

	if cfg.Defaults.BranchNaming != "template" {
		t.Errorf("Defaults.BranchNaming = %q; want %q", cfg.Defaults.BranchNaming, "template")
	}

	if cfg.Defaults.BranchTemplate != "{prefix}{slug}" {
		t.Errorf("Defaults.BranchTemplate = %q; want %q", cfg.Defaults.BranchTemplate, "{prefix}{slug}")
	}

	if cfg.Defaults.SlugMaxLength != 40 {
		t.Errorf("Defaults.SlugMaxLength = %d; want %d", cfg.Defaults.SlugMaxLength, 40)
	}

	if !cfg.Defaults.AutoSpawnAgent {
		t.Error("Defaults.AutoSpawnAgent should be true")
	}

	if !cfg.Defaults.AutoCreateBranch {
		t.Error("Defaults.AutoCreateBranch should be true")
	}

	for _, agent := range []string{"claude", "opencode", "gemini", "codex", "aider"} {
		if _, ok := cfg.Agents[agent]; !ok {
			t.Errorf("expected agent %q to be defined", agent)
		}
	}

	claude := cfg.Agents["claude"]
	if claude.Command != "claude" {
		t.Errorf("claude.Command = %q; want %q", claude.Command, "claude")
	}

	opencode := cfg.Agents["opencode"]
	if opencode.Command != "opencode" {
		t.Errorf("opencode.Command = %q; want %q", opencode.Command, "opencode")
	}

	aider := cfg.Agents["aider"]
	if aider.Command != "aider" {
		t.Errorf("aider.Command = %q; want %q", aider.Command, "aider")
	}
	if len(aider.Args) != 1 || aider.Args[0] != "--yes" {
		t.Errorf("aider.Args = %v; want [--yes]", aider.Args)
	}

	gemini := cfg.Agents["gemini"]
	if gemini.Command != "gemini" {
		t.Errorf("gemini.Command = %q; want %q", gemini.Command, "gemini")
	}
	if len(gemini.Args) != 1 || gemini.Args[0] != "--yolo" {
		t.Errorf("gemini.Args = %v; want [--yolo]", gemini.Args)
	}

	codex := cfg.Agents["codex"]
	if codex.Command != "codex" {
		t.Errorf("codex.Command = %q; want %q", codex.Command, "codex")
	}
	if len(codex.Args) != 1 || codex.Args[0] != "--full-auto" {
		t.Errorf("codex.Args = %v; want [--full-auto]", codex.Args)
	}

	if cfg.UI.Theme != "catppuccin-mocha" {
		t.Errorf("UI.Theme = %q; want %q", cfg.UI.Theme, "catppuccin-mocha")
	}

	if !cfg.UI.ShowAgentStatus {
		t.Error("UI.ShowAgentStatus should be true")
	}

	if cfg.UI.RefreshInterval != 5 {
		t.Errorf("UI.RefreshInterval = %d; want %d", cfg.UI.RefreshInterval, 5)
	}

	if cfg.UI.ColumnWidth != 40 {
		t.Errorf("UI.ColumnWidth = %d; want %d", cfg.UI.ColumnWidth, 40)
	}

	if !cfg.Cleanup.DeleteWorktree {
		t.Error("Cleanup.DeleteWorktree should be true")
	}

	if cfg.Cleanup.DeleteBranch {
		t.Error("Cleanup.DeleteBranch should be false")
	}

	if cfg.Cleanup.ForceWorktreeRemoval {
		t.Error("Cleanup.ForceWorktreeRemoval should be false")
	}
}

// TestClaudeLeanPreset pins the token-optimized claude-lean preset shape.
// The authoritative spawn-wiring assertion lives in
// internal/ui/respawn_daemon_test.go (TestBuildSpawnReq_LeanPresetReachesSpawn);
// this only guards the preset definition + its lean InitPrompt invariants.
// See docs/TOKEN_OPTIMIZATION.md.
func TestClaudeLeanPreset(t *testing.T) {
	cfg := DefaultConfig()
	lean, ok := cfg.Agents["claude-lean"]
	if !ok {
		t.Fatal("claude-lean preset missing from default agents")
	}
	// Command must be "claude" so it inherits Claude-class spawn behavior
	// (plan mode, prompt-suggestion disable) via buildSpawnReq's basename switch.
	if lean.Command != "claude" {
		t.Errorf("claude-lean.Command = %q; want %q", lean.Command, "claude")
	}
	if lean.Env["CLAUDE_CONFIG_DIR"] == "" {
		t.Error("claude-lean.Env missing CLAUDE_CONFIG_DIR (the slimmed profile)")
	}
	if lean.Env["CLAUDE_CODE_DISABLE_AUTO_MEMORY"] != "1" {
		t.Errorf("claude-lean.Env[CLAUDE_CODE_DISABLE_AUTO_MEMORY] = %q; want \"1\"",
			lean.Env["CLAUDE_CODE_DISABLE_AUTO_MEMORY"])
	}
	hasStrictMCP := false
	for _, a := range lean.Args {
		if a == "--strict-mcp-config" {
			hasStrictMCP = true
		}
	}
	if !hasStrictMCP {
		t.Errorf("claude-lean.Args = %v; want to contain --strict-mcp-config", lean.Args)
	}
	if lean.InitPrompt != defaultLeanAgentPrompt {
		t.Error("claude-lean.InitPrompt should be the lean template (defaultLeanAgentPrompt)")
	}
	// Lean InitPrompt invariants: it must NOT mandate /prime (the whole point —
	// a lean session has no global CLAUDE.md/memory for /prime to load), but it
	// MUST preserve the close-out directive.
	if strings.Contains(defaultLeanAgentPrompt, "/prime") {
		t.Error("lean InitPrompt should not reference /prime")
	}
	if !strings.Contains(defaultLeanAgentPrompt, "finishing-an-openkanban-ticket") {
		t.Error("lean InitPrompt must keep the finishing-an-openkanban-ticket close-out directive")
	}
}

func TestConfigDir(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")

	dir, err := ConfigDir()
	if err != nil {
		t.Fatalf("ConfigDir() error: %v", err)
	}

	if dir == "" {
		t.Error("ConfigDir() returned empty string")
	}

	if filepath.Base(dir) != "openkanban" {
		t.Errorf("ConfigDir() = %q; want to end with 'openkanban'", dir)
	}

	if filepath.Base(filepath.Dir(dir)) != ".config" {
		t.Errorf("ConfigDir() = %q; want parent to be '.config'", dir)
	}
}

func TestConfigDir_EnvOverride(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", "/custom/test/path")
	t.Setenv("XDG_CONFIG_HOME", "/should/be/ignored")

	dir, err := ConfigDir()
	if err != nil {
		t.Fatalf("ConfigDir() error: %v", err)
	}

	if dir != "/custom/test/path" {
		t.Errorf("ConfigDir() = %q; want %q", dir, "/custom/test/path")
	}
}

func TestConfigDir_XDGFallback(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "/xdg/config")

	dir, err := ConfigDir()
	if err != nil {
		t.Fatalf("ConfigDir() error: %v", err)
	}

	expected := filepath.Join("/xdg/config", "openkanban")
	if dir != expected {
		t.Errorf("ConfigDir() = %q; want %q", dir, expected)
	}
}

func TestConfigPath(t *testing.T) {
	path, err := ConfigPath()
	if err != nil {
		t.Fatalf("ConfigPath() error: %v", err)
	}

	if filepath.Base(path) != "config.json" {
		t.Errorf("ConfigPath() = %q; want to end with 'config.json'", path)
	}
}

func TestLoad_NonExistentFile(t *testing.T) {
	cfg, err := Load("/nonexistent/path/config.json")
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	defaults := DefaultConfig()
	if cfg.Defaults.DefaultAgent != defaults.Defaults.DefaultAgent {
		t.Errorf("Load() should return defaults when file not found")
	}
}

func TestLoad_EmptyPath(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\") error: %v", err)
	}

	if cfg == nil {
		t.Error("Load(\"\") should not return nil config")
	}
}

func TestLoad_ValidFile(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	customConfig := map[string]interface{}{
		"defaults": map[string]interface{}{
			"default_agent":   "claude",
			"branch_prefix":   "feature/",
			"slug_max_length": 30,
		},
		"ui": map[string]interface{}{
			"theme": "dark",
		},
	}

	data, err := json.Marshal(customConfig)
	if err != nil {
		t.Fatalf("failed to marshal test config: %v", err)
	}

	if err := os.WriteFile(configPath, data, 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Defaults.DefaultAgent != "claude" {
		t.Errorf("Defaults.DefaultAgent = %q; want %q", cfg.Defaults.DefaultAgent, "claude")
	}

	if cfg.Defaults.BranchPrefix != "feature/" {
		t.Errorf("Defaults.BranchPrefix = %q; want %q", cfg.Defaults.BranchPrefix, "feature/")
	}

	if cfg.Defaults.SlugMaxLength != 30 {
		t.Errorf("Defaults.SlugMaxLength = %d; want %d", cfg.Defaults.SlugMaxLength, 30)
	}

	if cfg.UI.Theme != "dark" {
		t.Errorf("UI.Theme = %q; want %q", cfg.UI.Theme, "dark")
	}
}

// TestLoad_LegacyConfigInheritsDaemonDefault verifies the
// backward-compat contract: a config.json written before DaemonSettings
// existed (no "daemon" key) MUST still load with Autostart=true.
// Without this, every user upgrading from an older binary would
// silently lose TUI-spawned daemon autostart on first launch.
//
// The mechanism: Load() / LoadWithValidation() start from
// DefaultConfig() then json.Unmarshal user JSON over it. Go's
// json.Unmarshal only overwrites fields PRESENT in the JSON; absent
// keys keep the destination struct's existing value. This test
// pins that behavior so a future refactor can't regress it.
func TestLoad_LegacyConfigInheritsDaemonDefault(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "config.json")
	// Deliberately omit the "daemon" key — simulating a config
	// written by an older openkanban binary.
	legacy := `{"defaults":{"default_agent":"claude"},"ui":{"theme":"catppuccin-mocha"}}`
	if err := os.WriteFile(tmp, []byte(legacy), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	t.Run("Load", func(t *testing.T) {
		cfg, err := Load(tmp)
		if err != nil {
			t.Fatalf("Load() error: %v", err)
		}
		if !cfg.Daemon.Autostart {
			t.Errorf("legacy config missing 'daemon' key: Autostart got %v want true", cfg.Daemon.Autostart)
		}
	})

	t.Run("LoadWithValidation", func(t *testing.T) {
		cfg, _, err := LoadWithValidation(tmp)
		if err != nil {
			t.Fatalf("LoadWithValidation() error: %v", err)
		}
		if !cfg.Daemon.Autostart {
			t.Errorf("legacy config missing 'daemon' key (validation path): Autostart got %v want true", cfg.Daemon.Autostart)
		}
	})
}

func TestLoad_EmptyArgs(t *testing.T) {
	tests := []struct {
		name string
		args string
	}{
		{name: "empty array", args: `[]`},
		{name: "single empty string", args: `[""]`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmp := filepath.Join(t.TempDir(), "config.json")
			cfg := `{"agents":{"claude":{"command":"claude","init_prompt":"x","args":` + tt.args + `}}}`
			if err := os.WriteFile(tmp, []byte(cfg), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			c, err := Load(tmp)
			if err != nil {
				t.Fatalf("Load() error: %v", err)
			}
			if c == nil {
				t.Fatal("Load() returned nil config")
			}
			if _, ok := c.Agents["claude"]; !ok {
				t.Fatal("claude agent missing from loaded config")
			}
		})
	}
}

func TestLoad_MissingArgs(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "config.json")
	cfg := `{"agents":{"claude":{"command":"claude","init_prompt":"x"}}}`
	if err := os.WriteFile(tmp, []byte(cfg), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	c, err := Load(tmp)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got := c.Agents["claude"].Args; got != nil {
		t.Errorf("want nil Args, got %v", got)
	}
}

func TestLoad_InvalidJSON(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	if err := os.WriteFile(configPath, []byte("not valid json"), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	_, err := Load(configPath)
	if err == nil {
		t.Error("Load() should return error for invalid JSON")
	}
}

func TestSave(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	cfg := DefaultConfig()
	cfg.Defaults.DefaultAgent = "custom-agent"

	if err := cfg.Save(configPath); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		t.Error("Save() should create config file")
	}

	loaded, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if loaded.Defaults.DefaultAgent != "custom-agent" {
		t.Errorf("loaded.Defaults.DefaultAgent = %q; want %q", loaded.Defaults.DefaultAgent, "custom-agent")
	}
}

func TestSave_CreatesDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "nested", "dir", "config.json")

	cfg := DefaultConfig()

	if err := cfg.Save(configPath); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		t.Error("Save() should create nested directories")
	}
}

func TestGetEffectiveInitPrompt(t *testing.T) {
	t.Run("returns agent-specific prompt when set", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Agents["claude"] = AgentConfig{
			Command:    "claude",
			InitPrompt: "custom claude prompt",
		}

		prompt := cfg.GetEffectiveInitPrompt("claude")
		if prompt != "custom claude prompt" {
			t.Errorf("GetEffectiveInitPrompt(\"claude\") = %q; want %q", prompt, "custom claude prompt")
		}
	})

	t.Run("falls back to global default when agent has no prompt", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Agents["custom"] = AgentConfig{
			Command:    "custom",
			InitPrompt: "",
		}
		cfg.Defaults.InitPrompt = "global prompt"

		prompt := cfg.GetEffectiveInitPrompt("custom")
		if prompt != "global prompt" {
			t.Errorf("GetEffectiveInitPrompt(\"custom\") = %q; want %q", prompt, "global prompt")
		}
	})

	t.Run("falls back to hardcoded default when no prompts set", func(t *testing.T) {
		cfg := &Config{
			Agents:   map[string]AgentConfig{},
			Defaults: BoardSettings{},
		}

		prompt := cfg.GetEffectiveInitPrompt("unknown")
		if prompt == "" {
			t.Error("GetEffectiveInitPrompt should return non-empty default prompt")
		}
	})

	t.Run("returns default for unknown agent", func(t *testing.T) {
		cfg := DefaultConfig()

		prompt := cfg.GetEffectiveInitPrompt("unknown-agent")
		if prompt == "" {
			t.Error("GetEffectiveInitPrompt should return non-empty default for unknown agent")
		}
	})

	t.Run("agent init_prompt_file overrides inline init_prompt", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "prompt.md")
		if err := os.WriteFile(f, []byte("from linked file"), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg := DefaultConfig()
		cfg.Agents["claude"] = AgentConfig{
			Command:        "claude",
			InitPrompt:     "inline should lose",
			InitPromptFile: f,
		}
		if got := cfg.GetEffectiveInitPrompt("claude"); got != "from linked file" {
			t.Errorf("got %q; want linked-file contents", got)
		}
	})

	t.Run("defaults init_prompt_file used when agent has neither", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "global.md")
		if err := os.WriteFile(f, []byte("global file prompt"), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg := DefaultConfig()
		cfg.Agents["custom"] = AgentConfig{Command: "custom"} // no inline, no file
		cfg.Defaults.InitPromptFile = f
		if got := cfg.GetEffectiveInitPrompt("custom"); got != "global file prompt" {
			t.Errorf("got %q; want global-file contents", got)
		}
	})

	t.Run("missing file falls through to inline init_prompt", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Agents["claude"] = AgentConfig{
			Command:        "claude",
			InitPrompt:     "inline fallback",
			InitPromptFile: filepath.Join(t.TempDir(), "does-not-exist.md"),
		}
		if got := cfg.GetEffectiveInitPrompt("claude"); got != "inline fallback" {
			t.Errorf("got %q; want inline fallback on unreadable file", got)
		}
	})

	t.Run("blank file falls through to inline init_prompt", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "blank.md")
		if err := os.WriteFile(f, []byte("   \n\t\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg := DefaultConfig()
		cfg.Agents["claude"] = AgentConfig{
			Command:        "claude",
			InitPrompt:     "inline fallback",
			InitPromptFile: f,
		}
		if got := cfg.GetEffectiveInitPrompt("claude"); got != "inline fallback" {
			t.Errorf("got %q; want inline fallback on blank file", got)
		}
	})

	t.Run("relative init_prompt_file resolves against config dir", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("OPENKANBAN_CONFIG_DIR", dir)
		if err := os.WriteFile(filepath.Join(dir, "p.md"), []byte("rel file prompt"), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg := DefaultConfig()
		cfg.Agents["claude"] = AgentConfig{Command: "claude", InitPromptFile: "p.md"}
		if got := cfg.GetEffectiveInitPrompt("claude"); got != "rel file prompt" {
			t.Errorf("got %q; want relative-path file contents", got)
		}
	})

	t.Run("leading ~ in init_prompt_file expands to home", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		if err := os.WriteFile(filepath.Join(home, "myprompt.md"), []byte("home file prompt"), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg := DefaultConfig()
		cfg.Agents["claude"] = AgentConfig{Command: "claude", InitPromptFile: "~/myprompt.md"}
		if got := cfg.GetEffectiveInitPrompt("claude"); got != "home file prompt" {
			t.Errorf("got %q; want ~-expanded file contents", got)
		}
	})
}

func TestMergeAgentDefaults(t *testing.T) {
	cfg := &Config{
		Agents: map[string]AgentConfig{
			"claude": {
				Command: "custom-claude",
				Args:    []string{"--custom"},
			},
		},
	}

	cfg.mergeAgentDefaults()

	if cfg.Agents["claude"].StatusFile != ".claude/status.json" {
		t.Errorf("claude.StatusFile = %q; want %q", cfg.Agents["claude"].StatusFile, ".claude/status.json")
	}

	if cfg.Agents["claude"].Env == nil {
		t.Error("claude.Env should not be nil after merge")
	}

	if cfg.Agents["claude"].Command != "custom-claude" {
		t.Errorf("claude.Command = %q; want %q", cfg.Agents["claude"].Command, "custom-claude")
	}

	if cfg.Agents["claude"].InitPrompt != defaultAgentPrompt {
		t.Error("claude.InitPrompt should inherit defaultAgentPrompt when user config omits it")
	}
}

func TestConfigStructure(t *testing.T) {
	cfg := DefaultConfig()

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json.Marshal error: %v", err)
	}

	var unmarshaled Config
	if err := json.Unmarshal(data, &unmarshaled); err != nil {
		t.Fatalf("json.Unmarshal error: %v", err)
	}

	if unmarshaled.Defaults.DefaultAgent != cfg.Defaults.DefaultAgent {
		t.Errorf("round-trip failed for Defaults.DefaultAgent")
	}

	if len(unmarshaled.Agents) != len(cfg.Agents) {
		t.Errorf("round-trip failed for Agents count")
	}
}

func TestAgentConfigFields(t *testing.T) {
	cfg := DefaultConfig()

	for name, agent := range cfg.Agents {
		if agent.Command == "" {
			t.Errorf("agent %q has empty Command", name)
		}
		if agent.Env == nil {
			t.Errorf("agent %q has nil Env", name)
		}
	}
}

func TestUIConfigDefaults(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.UI.ColumnWidth <= 0 {
		t.Errorf("UI.ColumnWidth = %d; want positive value", cfg.UI.ColumnWidth)
	}

	if cfg.UI.TicketHeight <= 0 {
		t.Errorf("UI.TicketHeight = %d; want positive value", cfg.UI.TicketHeight)
	}

	if cfg.UI.RefreshInterval <= 0 {
		t.Errorf("UI.RefreshInterval = %d; want positive value", cfg.UI.RefreshInterval)
	}
}

func TestDetectAvailableAgent(t *testing.T) {
	t.Run("returns first available agent by priority", func(t *testing.T) {
		agents := map[string]AgentConfig{
			"opencode": {Command: "go"},
			"claude":   {Command: "go"},
			"aider":    {Command: "go"},
		}
		result := DetectAvailableAgent(agents)
		if result != "opencode" {
			t.Errorf("DetectAvailableAgent() = %q; want %q (first in priority)", result, "opencode")
		}
	})

	t.Run("skips unavailable agents", func(t *testing.T) {
		agents := map[string]AgentConfig{
			"opencode": {Command: "nonexistent-binary-12345"},
			"claude":   {Command: "go"},
			"aider":    {Command: "go"},
		}
		result := DetectAvailableAgent(agents)
		if result != "claude" {
			t.Errorf("DetectAvailableAgent() = %q; want %q (second in priority)", result, "claude")
		}
	})

	t.Run("falls back to first priority when none available", func(t *testing.T) {
		agents := map[string]AgentConfig{
			"opencode": {Command: "nonexistent-binary-12345"},
			"claude":   {Command: "nonexistent-binary-67890"},
			"aider":    {Command: "nonexistent-binary-abcde"},
		}
		result := DetectAvailableAgent(agents)
		if result != "opencode" {
			t.Errorf("DetectAvailableAgent() = %q; want %q (fallback)", result, "opencode")
		}
	})

	t.Run("handles missing agent configs", func(t *testing.T) {
		agents := map[string]AgentConfig{
			"claude": {Command: "go"},
		}
		result := DetectAvailableAgent(agents)
		if result != "claude" {
			t.Errorf("DetectAvailableAgent() = %q; want %q", result, "claude")
		}
	})

	t.Run("handles empty agent map", func(t *testing.T) {
		agents := map[string]AgentConfig{}
		result := DetectAvailableAgent(agents)
		if result != "opencode" {
			t.Errorf("DetectAvailableAgent() = %q; want %q (fallback)", result, "opencode")
		}
	})
}

func TestAgentPriority(t *testing.T) {
	if len(AgentPriority) == 0 {
		t.Error("AgentPriority should not be empty")
	}

	if AgentPriority[0] != "opencode" {
		t.Errorf("AgentPriority[0] = %q; want %q", AgentPriority[0], "opencode")
	}

	expected := []string{"opencode", "claude", "claude-custom", "gemini", "codex", "aider"}
	if len(AgentPriority) != len(expected) {
		t.Errorf("AgentPriority has %d items; want %d", len(AgentPriority), len(expected))
	}
	for i, name := range expected {
		if AgentPriority[i] != name {
			t.Errorf("AgentPriority[%d] = %q; want %q", i, AgentPriority[i], name)
		}
	}
}

// TestMergeAgentDefaults_AddsNewBuiltin verifies that a user config lacking
// "claude-custom" receives the built-in preset after mergeAgentDefaults runs.
// Fixture A: agents map contains only "claude"; "claude-custom" is absent.
func TestMergeAgentDefaults_AddsNewBuiltin(t *testing.T) {
	cfg := &Config{
		Agents: map[string]AgentConfig{
			"claude": {
				Command: "claude",
				Args:    []string{"--dangerously-skip-permissions"},
			},
		},
	}

	// Confirm the precondition: claude-custom is absent before the merge.
	if _, exists := cfg.Agents["claude-custom"]; exists {
		t.Fatal("precondition failed: claude-custom should be absent before merge")
	}

	cfg.mergeAgentDefaults()

	got, exists := cfg.Agents["claude-custom"]
	if !exists {
		t.Fatal("claude-custom was not added by mergeAgentDefaults")
	}
	if got.Command != "claude" {
		t.Errorf("claude-custom.Command = %q; want %q", got.Command, "claude")
	}
	if got.Env["CLAUDE_CONFIG_DIR"] == "" {
		t.Errorf("claude-custom.Env[CLAUDE_CONFIG_DIR] is empty; want non-empty")
	}
}

// TestMergeAgentDefaults_BackfillsLabel verifies that a user "claude" entry
// with an empty Label receives the default Label after mergeAgentDefaults runs.
// Fixture B: user has "claude" with no Label set.
func TestMergeAgentDefaults_BackfillsLabel(t *testing.T) {
	cfg := &Config{
		Agents: map[string]AgentConfig{
			"claude": {
				Command: "claude",
				// Label deliberately left empty to exercise backfill.
			},
		},
	}

	// Confirm the precondition: Label is empty before the merge.
	if cfg.Agents["claude"].Label != "" {
		t.Fatalf("precondition failed: claude.Label = %q; want empty before merge", cfg.Agents["claude"].Label)
	}

	cfg.mergeAgentDefaults()

	if got := cfg.Agents["claude"].Label; got != "Claude (Default)" {
		t.Errorf("claude.Label after merge = %q; want %q", got, "Claude (Default)")
	}
}

func TestLoadWithValidation_ValidFile(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	cfg := DefaultConfig()
	if err := cfg.Save(configPath); err != nil {
		t.Fatalf("failed to save test config: %v", err)
	}

	loaded, result, err := LoadWithValidation(configPath)
	if err != nil {
		t.Fatalf("LoadWithValidation() error: %v", err)
	}

	if loaded == nil {
		t.Error("LoadWithValidation() should return config")
	}

	if result == nil {
		t.Error("LoadWithValidation() should return validation result")
	}

	if result.HasErrors() {
		t.Errorf("valid config should not have errors:\n%s", result.FormatErrors())
	}
}

func TestLoadWithValidation_InvalidJSON(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	if err := os.WriteFile(configPath, []byte("not valid json"), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	_, result, err := LoadWithValidation(configPath)
	if err == nil {
		t.Error("LoadWithValidation() should return error for invalid JSON")
	}

	if result == nil {
		t.Error("LoadWithValidation() should return validation result for JSON errors")
	}

	if !result.HasErrors() {
		t.Error("validation result should have errors for invalid JSON")
	}
}

func TestLoadWithValidation_NonExistentFile(t *testing.T) {
	cfg, result, err := LoadWithValidation("/nonexistent/path/config.json")
	if err != nil {
		t.Fatalf("LoadWithValidation() error: %v", err)
	}

	if cfg == nil {
		t.Error("LoadWithValidation() should return default config when file not found")
	}

	if result == nil {
		t.Error("LoadWithValidation() should return validation result")
	}
}

// promptData is the test fixture for exercising defaultAgentPrompt.
// Its shape must match the production ContextData payload so the
// template renders against the same field set production sees.
type promptData struct {
	Title, Description, BranchName, BaseBranch, Status, BriefPath string
	HasBrief, IsExternalResume                                    bool
}

func TestDefaultAgentPrompt_FreshSpawn(t *testing.T) {
	data := promptData{
		Title:            "Test ticket",
		Description:      "do the thing",
		BranchName:       "task/test",
		BaseBranch:       "main",
		Status:           "in_progress",
		BriefPath:        "tickets/test.md",
		HasBrief:         true,
		IsExternalResume: false,
	}
	tmpl, err := template.New("p").Parse(defaultAgentPrompt)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		t.Fatalf("exec: %v", err)
	}
	got := buf.String()
	for _, want := range []string{
		"in_progress on spawn",
		"Work only inside this worktree",
		"tickets/test.md",
		"## Notes (from openkanban card)",
		"finishing-an-openkanban-ticket",
		"superpowers:finishing-a-development-branch",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("want substring %q in:\n%s", want, got)
		}
	}
}

// TestDefaultAgentPrompt_FreshSpawn_NonInProgress pins the status-
// conditional contract: when a ticket is re-spawned at a non-in_progress
// status (typically in_review), the prompt must NOT claim "moved to
// in_progress on spawn" — that claim was false on every re-spawn and
// produced "wait, didn't we just merge this?" UX. The prompt acknowledges
// the actual current status instead.
func TestDefaultAgentPrompt_FreshSpawn_NonInProgress(t *testing.T) {
	cases := []string{"in_review", "backlog", "done"}
	for _, status := range cases {
		t.Run(status, func(t *testing.T) {
			data := promptData{
				Title:      "Test ticket",
				BranchName: "task/test",
				BaseBranch: "main",
				Status:     status,
				HasBrief:   false,
			}
			tmpl, err := template.New("p").Parse(defaultAgentPrompt)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			var buf bytes.Buffer
			if err := tmpl.Execute(&buf, data); err != nil {
				t.Fatalf("exec: %v", err)
			}
			got := buf.String()

			if strings.Contains(got, "moved this ticket to in_progress") {
				t.Errorf("prompt falsely claims status was moved to in_progress for status=%q\nfull prompt:\n%s", status, got)
			}
			if !strings.Contains(got, "Ticket status is `"+status+"`") {
				t.Errorf("prompt does not acknowledge actual status=%q\nfull prompt:\n%s", status, got)
			}
			if !strings.Contains(got, "OpenKanban does not change ticket status on spawn") {
				t.Errorf("prompt missing clarifying line about status not being auto-changed\nfull prompt:\n%s", got)
			}
		})
	}
}

func TestDefaultAgentPrompt_ExternalResume(t *testing.T) {
	data := struct {
		Title, Description, BranchName, BaseBranch, BriefPath string
		HasBrief, IsExternalResume                            bool
	}{
		Title:            "Test ticket",
		BranchName:       "task/test",
		BaseBranch:       "main",
		HasBrief:         false,
		IsExternalResume: true,
	}
	tmpl, err := template.New("p").Parse(defaultAgentPrompt)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		t.Fatalf("exec: %v", err)
	}
	got := buf.String()
	for _, want := range []string{
		"has scoped this session",
		"scoped to this ticket's worktree",
		"Test ticket",
		"finishing-an-openkanban-ticket",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("want substring %q in:\n%s", want, got)
		}
	}
}

func TestLoadWithValidation_InvalidConfig(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	invalidConfig := map[string]interface{}{
		"defaults": map[string]interface{}{
			"branch_naming": "invalid-value",
		},
		"agents": map[string]interface{}{
			"bad": map[string]interface{}{
				"command": "",
			},
		},
	}

	data, err := json.Marshal(invalidConfig)
	if err != nil {
		t.Fatalf("failed to marshal test config: %v", err)
	}

	if err := os.WriteFile(configPath, data, 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	cfg, result, err := LoadWithValidation(configPath)
	if err != nil {
		t.Fatalf("LoadWithValidation() unexpected error: %v", err)
	}

	if cfg == nil {
		t.Error("LoadWithValidation() should return config even with validation errors")
	}

	if result == nil {
		t.Fatal("LoadWithValidation() should return validation result")
	}

	if !result.HasErrors() {
		t.Error("validation result should have errors for invalid config")
	}
}

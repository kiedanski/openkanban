package config

import (
	"testing"

	"github.com/techdufus/openkanban/internal/board"
)

func TestRoleForType(t *testing.T) {
	cases := []struct {
		in   board.TicketType
		want string
	}{
		{board.TypeResearch, "claude-research"},
		{board.TypeSpec, "claude-spec"},
		{board.TypeImplement, "claude"},
		{board.TypeReview, "claude-review"},
		{board.TypeFreeform, ""},
		{board.TicketType("bogus"), ""},
	}
	for _, c := range cases {
		if got := RoleForType(c.in); got != c.want {
			t.Errorf("RoleForType(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestRoleAgentsHaveDistinctInitPrompts is the acceptance guard: each pipeline
// role must spawn a claude-class agent whose InitPrompt DIFFERS from the other
// roles and from the default coding role. If two roles collapse to the same
// prompt, the type→role binding is cosmetic.
func TestRoleAgentsHaveDistinctInitPrompts(t *testing.T) {
	agents := defaultAgents()
	keys := []string{"claude", "claude-research", "claude-spec", "claude-review"}
	seen := map[string]string{}
	for _, k := range keys {
		a, ok := agents[k]
		if !ok {
			t.Fatalf("defaultAgents missing %q", k)
		}
		if a.Command != "claude" {
			t.Errorf("%s Command = %q, want claude", k, a.Command)
		}
		if a.InitPrompt == "" {
			t.Errorf("%s has empty InitPrompt", k)
		}
		if prev, dup := seen[a.InitPrompt]; dup {
			t.Errorf("%s InitPrompt is identical to %s; roles must differ", k, prev)
		}
		seen[a.InitPrompt] = k
	}
}

// TestRoleAgentsUseDefaultProfile pins that the role agents ship with an empty
// Env, so they launch the DEFAULT claude profile and differ from the plain
// "claude" only by InitPrompt (not by CLAUDE_CONFIG_DIR). This is what makes
// binding a type→role safe against the project-pin "wrong Claude" guard.
func TestRoleAgentsUseDefaultProfile(t *testing.T) {
	agents := defaultAgents()
	for _, k := range []string{"claude-research", "claude-spec", "claude-review"} {
		if len(agents[k].Env) != 0 {
			t.Errorf("%s Env = %v, want empty (default profile)", k, agents[k].Env)
		}
	}
}

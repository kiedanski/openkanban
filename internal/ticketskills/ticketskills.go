// Package ticketskills ships openkanban's ticket-graph agent skills —
// spin-off-a-ticket and fan-out-a-plan. Each SKILL.md is the version-controlled
// source of truth, embedded via //go:embed (mirroring internal/finishskill) and
// written into the user's global ~/.claude/skills/ on launch so the skills stay
// in sync with the binary and propagate on `openkanban update`.
package ticketskills

import (
	_ "embed"
	"os"
	"path/filepath"
)

//go:embed spin-off-a-ticket.SKILL.md
var spinOffMarkdown string

//go:embed fan-out-a-plan.SKILL.md
var fanOutMarkdown string

// skill pairs the install directory name (the slug agents invoke) with its
// embedded SKILL.md body.
type skill struct {
	name string
	body string
}

func all() []skill {
	return []skill{
		{name: "spin-off-a-ticket", body: spinOffMarkdown},
		{name: "fan-out-a-plan", body: fanOutMarkdown},
	}
}

// InstallPath is where a skill is written under the given home dir.
func InstallPath(home, name string) string {
	return filepath.Join(home, ".claude", "skills", name, "SKILL.md")
}

// EnsureInstalled writes each embedded SKILL.md to ~/.claude/skills/<name>/ when
// it is missing or differs from the embed, and returns the skill names it wrote.
// The repo embed is the source of truth (the skills are vendored into
// openkanban), so a differing global copy is overwritten — this is how skill
// edits propagate on update. Best-effort: callers should treat a non-nil error
// as non-fatal (the skills are conveniences, not hard dependencies).
func EnsureInstalled(home string) (wrote []string, err error) {
	if home == "" {
		return nil, nil
	}
	for _, s := range all() {
		dest := InstallPath(home, s.name)
		if existing, readErr := os.ReadFile(dest); readErr == nil && string(existing) == s.body {
			continue // already current
		}
		if mkErr := os.MkdirAll(filepath.Dir(dest), 0o755); mkErr != nil {
			return wrote, mkErr
		}
		// Atomic write: temp file in the same dir, then rename.
		tmp := dest + ".tmp"
		if wErr := os.WriteFile(tmp, []byte(s.body), 0o644); wErr != nil {
			return wrote, wErr
		}
		if rErr := os.Rename(tmp, dest); rErr != nil {
			os.Remove(tmp)
			return wrote, rErr
		}
		wrote = append(wrote, s.name)
	}
	return wrote, nil
}

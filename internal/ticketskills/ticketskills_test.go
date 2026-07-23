package ticketskills

import (
	"os"
	"strings"
	"testing"
)

func TestEnsureInstalledWritesBothSkills(t *testing.T) {
	home := t.TempDir()

	wrote, err := EnsureInstalled(home)
	if err != nil {
		t.Fatalf("EnsureInstalled: %v", err)
	}
	if len(wrote) != 2 {
		t.Fatalf("expected 2 skills written, got %d (%v)", len(wrote), wrote)
	}

	for _, name := range []string{"spin-off-a-ticket", "fan-out-a-plan"} {
		data, rerr := os.ReadFile(InstallPath(home, name))
		if rerr != nil {
			t.Fatalf("skill %q not written: %v", name, rerr)
		}
		if !strings.Contains(string(data), "name: "+name) {
			t.Errorf("skill %q missing 'name: %s' frontmatter", name, name)
		}
	}
}

func TestEnsureInstalledIsIdempotent(t *testing.T) {
	home := t.TempDir()
	if _, err := EnsureInstalled(home); err != nil {
		t.Fatalf("first EnsureInstalled: %v", err)
	}
	wrote, err := EnsureInstalled(home)
	if err != nil {
		t.Fatalf("second EnsureInstalled: %v", err)
	}
	if len(wrote) != 0 {
		t.Errorf("expected no rewrite when already current, got %v", wrote)
	}
}

func TestEnsureInstalledEmptyHomeNoOp(t *testing.T) {
	wrote, err := EnsureInstalled("")
	if err != nil || wrote != nil {
		t.Errorf("empty home should be a no-op, got wrote=%v err=%v", wrote, err)
	}
}

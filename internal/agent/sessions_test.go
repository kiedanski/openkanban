package agent

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/techdufus/openkanban/internal/config"
)

func TestSessionUUIDPattern(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"canonical lowercase", "7f3a9b2c-1d8e-4a5b-9c3d-2f1e0a8b9c4d", true},
		{"all zeros", "00000000-0000-0000-0000-000000000000", true},
		{"uppercase rejected", "7F3A9B2C-1D8E-4A5B-9C3D-2F1E0A8B9C4D", false},
		{"missing hyphens", "7f3a9b2c1d8e4a5b9c3d2f1e0a8b9c4d", false},
		{"too short", "7f3a9b2c-1d8e-4a5b-9c3d-2f1e0a8b9c4", false},
		{"too long", "7f3a9b2c-1d8e-4a5b-9c3d-2f1e0a8b9c4dd", false},
		{"non-hex chars", "7f3a9b2c-1d8e-4a5b-9c3d-2f1e0a8b9czz", false},
		{"empty", "", false},
		{"opencode-style ref", "ses_abc123", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SessionUUIDPattern.MatchString(tc.in)
			if got != tc.ok {
				t.Errorf("MatchString(%q) = %v, want %v", tc.in, got, tc.ok)
			}
		})
	}
}

// withFakeHome rewires $HOME to a temp dir so SessionPath's globbing
// hits files we control.
func withFakeHome(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	return tmp
}

func TestSessionPath_NotFound(t *testing.T) {
	withFakeHome(t)
	uuid := "deadbeef-1234-4321-abcd-0123456789ab"
	_, err := SessionPath(uuid)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("SessionPath(unknown) error = %v, want os.ErrNotExist", err)
	}
}

func TestSessionPath_RejectsNonUUID(t *testing.T) {
	withFakeHome(t)
	if _, err := SessionPath("not-a-uuid"); err == nil {
		t.Fatal("SessionPath(non-uuid) succeeded; want error")
	}
}

func TestSessionPath_Found(t *testing.T) {
	home := withFakeHome(t)
	uuid := "7f3a9b2c-1d8e-4a5b-9c3d-2f1e0a8b9c4d"
	projDir := filepath.Join(home, ".claude", "projects", "encoded-test-cwd")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	want := filepath.Join(projDir, uuid+".jsonl")
	if err := os.WriteFile(want, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := SessionPath(uuid)
	if err != nil {
		t.Fatalf("SessionPath: %v", err)
	}
	if got != want {
		t.Errorf("SessionPath = %q, want %q", got, want)
	}
}

func TestSessionActive_NotHeld(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof not on PATH; skipping")
	}
	home := withFakeHome(t)
	uuid := "11111111-2222-3333-4444-555555555555"
	projDir := filepath.Join(home, ".claude", "projects", "encoded-test-cwd")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := filepath.Join(projDir, uuid+".jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	holder, err := SessionActive(uuid)
	if err != nil {
		t.Fatalf("SessionActive: %v", err)
	}
	if holder.PID != 0 {
		t.Errorf("PID = %d, want 0 (nothing should hold the file)", holder.PID)
	}
	if holder.Path != path {
		t.Errorf("Path = %q, want %q", holder.Path, path)
	}
}

func TestSessionActive_Held(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof not on PATH; skipping")
	}
	if _, err := exec.LookPath("tail"); err != nil {
		t.Skip("tail not on PATH; skipping")
	}

	home := withFakeHome(t)
	uuid := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	projDir := filepath.Join(home, ".claude", "projects", "encoded-test-cwd")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := filepath.Join(projDir, uuid+".jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// `tail -f path` keeps an open file descriptor on the JSONL.
	cmd := exec.Command("tail", "-f", path)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start tail: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	// Give tail a moment to actually open the file.
	deadline := time.Now().Add(2 * time.Second)
	var holder SessionHolder
	for time.Now().Before(deadline) {
		h, err := SessionActive(uuid)
		if err != nil {
			t.Fatalf("SessionActive: %v", err)
		}
		if h.PID != 0 {
			holder = h
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if holder.PID == 0 {
		t.Fatal("expected SessionActive to report a holder PID; got 0")
	}
	if holder.PID != cmd.Process.Pid {
		// lsof may report a child or parent depending on platform; just
		// log if it differs, don't fail — the important thing is "non-zero".
		t.Logf("holder PID %d differs from tail PID %d (acceptable)",
			holder.PID, cmd.Process.Pid)
	}
}

// writeJSONL writes the given events as one JSON object per line into
// `<dir>/<name>` and returns the file path. Creates `dir` if needed.
func writeJSONL(t *testing.T, dir, name string, events []map[string]any) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	var sb strings.Builder
	for _, ev := range events {
		b, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		sb.Write(b)
		sb.WriteString("\n")
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// encodeWorktree mirrors the encoding claude-code does for its
// per-project session directory name. Delegates to the production
// encoder so setup-side bucket paths in these tests stay in lockstep
// with ProjectDirFor (the encoder's own assertions in TestProjectDirFor
// use literal expected strings, not this helper — see that test).
func encodeWorktree(p string) string {
	return EncodeClaudeBucket(p)
}

func TestIsClaudeSessionDead(t *testing.T) {
	t.Run("no_project_dir", func(t *testing.T) {
		withFakeHome(t)
		// Worktree path that has no corresponding claude project dir.
		dead, path, err := IsClaudeSessionDead("/tmp/never-touched-by-claude")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !dead {
			t.Errorf("dead = false, want true (no project dir)")
		}
		if path != "" {
			t.Errorf("sessionPath = %q, want \"\"", path)
		}
	})

	t.Run("empty_dir", func(t *testing.T) {
		home := withFakeHome(t)
		worktree := "/worktree/empty"
		dir := filepath.Join(home, ".claude", "projects", encodeWorktree(worktree))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		dead, path, err := IsClaudeSessionDead(worktree)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !dead {
			t.Errorf("dead = false, want true (empty dir)")
		}
		if path != "" {
			t.Errorf("sessionPath = %q, want \"\"", path)
		}
	})

	t.Run("non_jsonl_only", func(t *testing.T) {
		home := withFakeHome(t)
		worktree := "/worktree/non-jsonl"
		dir := filepath.Join(home, ".claude", "projects", encodeWorktree(worktree))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hi"), 0o644); err != nil {
			t.Fatal(err)
		}
		dead, path, err := IsClaudeSessionDead(worktree)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !dead {
			t.Errorf("dead = false, want true (no .jsonl present)")
		}
		if path != "" {
			t.Errorf("sessionPath = %q, want \"\"", path)
		}
	})

	t.Run("dead_local_commands_only", func(t *testing.T) {
		home := withFakeHome(t)
		worktree := "/worktree/commands-only"
		dir := filepath.Join(home, ".claude", "projects", encodeWorktree(worktree))
		events := []map[string]any{
			{"type": "custom-title", "title": "scratch"},
			{"type": "permission-mode", "mode": "acceptEdits"},
			{
				"type": "user",
				"message": map[string]any{
					"role":    "user",
					"content": "<command-name>/exit</command-name>",
				},
			},
			{
				"type": "assistant",
				"message": map[string]any{
					"role":    "assistant",
					"content": "No response requested.",
				},
			},
			{
				"type": "assistant",
				"message": map[string]any{
					"role": "assistant",
					"content": []map[string]any{
						{"type": "text", "text": "  No response requested.  "},
					},
				},
			},
			{
				"type": "assistant",
				"message": map[string]any{
					"role":    "assistant",
					"content": "",
				},
			},
		}
		want := writeJSONL(t, dir, "11111111-2222-3333-4444-555555555555.jsonl", events)

		dead, got, err := IsClaudeSessionDead(worktree)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !dead {
			t.Errorf("dead = false, want true (only auto-replies)")
		}
		if got != want {
			t.Errorf("sessionPath = %q, want %q", got, want)
		}
	})

	t.Run("alive_real_assistant_string", func(t *testing.T) {
		home := withFakeHome(t)
		worktree := "/worktree/alive-string"
		dir := filepath.Join(home, ".claude", "projects", encodeWorktree(worktree))
		events := []map[string]any{
			{
				"type": "user",
				"message": map[string]any{
					"role":    "user",
					"content": "do a thing",
				},
			},
			{
				"type": "assistant",
				"message": map[string]any{
					"role":    "assistant",
					"content": "Sure, I can help with that.",
				},
			},
		}
		want := writeJSONL(t, dir, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee.jsonl", events)

		dead, got, err := IsClaudeSessionDead(worktree)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if dead {
			t.Errorf("dead = true, want false (real assistant string)")
		}
		if got != want {
			t.Errorf("sessionPath = %q, want %q", got, want)
		}
	})

	t.Run("alive_real_assistant_array", func(t *testing.T) {
		home := withFakeHome(t)
		worktree := "/worktree/alive-array"
		dir := filepath.Join(home, ".claude", "projects", encodeWorktree(worktree))
		events := []map[string]any{
			{
				"type": "assistant",
				"message": map[string]any{
					"role": "assistant",
					"content": []map[string]any{
						{"type": "text", "text": "Working on it."},
						{"type": "tool_use", "name": "bash", "input": map[string]any{"cmd": "ls"}},
					},
				},
			},
		}
		want := writeJSONL(t, dir, "bbbbbbbb-cccc-dddd-eeee-ffffffffffff.jsonl", events)

		dead, got, err := IsClaudeSessionDead(worktree)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if dead {
			t.Errorf("dead = true, want false (text part present)")
		}
		if got != want {
			t.Errorf("sessionPath = %q, want %q", got, want)
		}
	})

	t.Run("malformed_lines_mixed", func(t *testing.T) {
		home := withFakeHome(t)
		worktree := "/worktree/mixed"
		dir := filepath.Join(home, ".claude", "projects", encodeWorktree(worktree))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		// Hand-craft the file so we can inject malformed lines around the
		// valid assistant event.
		good := map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"role":    "assistant",
				"content": "Real reply.",
			},
		}
		goodBytes, err := json.Marshal(good)
		if err != nil {
			t.Fatal(err)
		}
		body := strings.Join([]string{
			"this is not json",
			"{\"type\":\"assistant\",\"message\":{\"role\":\"assistant\",\"content\":",
			string(goodBytes),
			"{not closed",
		}, "\n") + "\n"
		path := filepath.Join(dir, "cccccccc-dddd-eeee-ffff-000000000000.jsonl")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}

		dead, got, err := IsClaudeSessionDead(worktree)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if dead {
			t.Errorf("dead = true, want false (good line should be found)")
		}
		if got != path {
			t.Errorf("sessionPath = %q, want %q", got, path)
		}
	})

	t.Run("picks_most_recent_jsonl", func(t *testing.T) {
		home := withFakeHome(t)
		worktree := "/worktree/multi"
		dir := filepath.Join(home, ".claude", "projects", encodeWorktree(worktree))

		// Older file: has a real assistant reply (would be "alive").
		olderEvents := []map[string]any{
			{
				"type": "assistant",
				"message": map[string]any{
					"role":    "assistant",
					"content": "Real reply from older session.",
				},
			},
		}
		olderPath := writeJSONL(t, dir, "11111111-1111-1111-1111-111111111111.jsonl", olderEvents)

		// Newer file: only auto-replies (would be "dead").
		newerEvents := []map[string]any{
			{
				"type": "assistant",
				"message": map[string]any{
					"role":    "assistant",
					"content": "No response requested.",
				},
			},
		}
		newerPath := writeJSONL(t, dir, "22222222-2222-2222-2222-222222222222.jsonl", newerEvents)

		// Force the mtimes so newer is unambiguously newer.
		oldTime := time.Now().Add(-2 * time.Hour)
		newTime := time.Now()
		if err := os.Chtimes(olderPath, oldTime, oldTime); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(newerPath, newTime, newTime); err != nil {
			t.Fatal(err)
		}

		dead, got, err := IsClaudeSessionDead(worktree)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !dead {
			t.Errorf("dead = false, want true (newer file has only auto-replies)")
		}
		if got != newerPath {
			t.Errorf("sessionPath = %q, want newer %q", got, newerPath)
		}
	})
}

func TestFindClaudeSession(t *testing.T) {
	t.Run("no_project_dir", func(t *testing.T) {
		withFakeHome(t)
		if got := FindClaudeSession("/tmp/never-touched"); got != "" {
			t.Errorf("FindClaudeSession = %q, want \"\" (no project dir)", got)
		}
	})

	t.Run("empty_string_worktree", func(t *testing.T) {
		withFakeHome(t)
		if got := FindClaudeSession(""); got != "" {
			t.Errorf("FindClaudeSession(\"\") = %q, want \"\"", got)
		}
	})

	t.Run("no_jsonl_files", func(t *testing.T) {
		home := withFakeHome(t)
		worktree := "/worktree/empty"
		dir := filepath.Join(home, ".claude", "projects", encodeWorktree(worktree))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if got := FindClaudeSession(worktree); got != "" {
			t.Errorf("FindClaudeSession = %q, want \"\" (empty dir)", got)
		}
	})

	t.Run("dead_session_returns_empty", func(t *testing.T) {
		home := withFakeHome(t)
		worktree := "/worktree/dead"
		dir := filepath.Join(home, ".claude", "projects", encodeWorktree(worktree))
		events := []map[string]any{
			{
				"type": "assistant",
				"message": map[string]any{
					"role":    "assistant",
					"content": "No response requested.",
				},
			},
		}
		writeJSONL(t, dir, "11111111-2222-3333-4444-555555555555.jsonl", events)
		if got := FindClaudeSession(worktree); got != "" {
			t.Errorf("FindClaudeSession = %q, want \"\" (only auto-replies)", got)
		}
	})

	t.Run("live_session_returns_uuid", func(t *testing.T) {
		home := withFakeHome(t)
		worktree := "/worktree/live"
		dir := filepath.Join(home, ".claude", "projects", encodeWorktree(worktree))
		uuid := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
		events := []map[string]any{
			{
				"type": "assistant",
				"message": map[string]any{
					"role":    "assistant",
					"content": "Real reply.",
				},
			},
		}
		writeJSONL(t, dir, uuid+".jsonl", events)
		if got := FindClaudeSession(worktree); got != uuid {
			t.Errorf("FindClaudeSession = %q, want %q", got, uuid)
		}
	})

	t.Run("non_uuid_basename_returns_empty", func(t *testing.T) {
		home := withFakeHome(t)
		worktree := "/worktree/garbage"
		dir := filepath.Join(home, ".claude", "projects", encodeWorktree(worktree))
		events := []map[string]any{
			{
				"type": "assistant",
				"message": map[string]any{
					"role":    "assistant",
					"content": "Real reply.",
				},
			},
		}
		writeJSONL(t, dir, "not-a-uuid.jsonl", events)
		if got := FindClaudeSession(worktree); got != "" {
			t.Errorf("FindClaudeSession = %q, want \"\" (non-uuid basename)", got)
		}
	})

	t.Run("picks_freshest_by_mtime", func(t *testing.T) {
		home := withFakeHome(t)
		worktree := "/worktree/multi-fresh"
		dir := filepath.Join(home, ".claude", "projects", encodeWorktree(worktree))

		liveEvents := []map[string]any{
			{
				"type": "assistant",
				"message": map[string]any{
					"role":    "assistant",
					"content": "Real reply.",
				},
			},
		}
		olderUUID := "11111111-1111-1111-1111-111111111111"
		newerUUID := "22222222-2222-2222-2222-222222222222"
		olderPath := writeJSONL(t, dir, olderUUID+".jsonl", liveEvents)
		newerPath := writeJSONL(t, dir, newerUUID+".jsonl", liveEvents)

		oldTime := time.Now().Add(-2 * time.Hour)
		newTime := time.Now()
		if err := os.Chtimes(olderPath, oldTime, oldTime); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(newerPath, newTime, newTime); err != nil {
			t.Fatal(err)
		}

		if got := FindClaudeSession(worktree); got != newerUUID {
			t.Errorf("FindClaudeSession = %q, want newer %q", got, newerUUID)
		}
	})

	t.Run("newer_dead_takes_precedence_over_older_live", func(t *testing.T) {
		// Matches IsClaudeSessionDead's "picks_most_recent_jsonl"
		// behaviour: we look at the freshest file only, and judge its
		// aliveness. An older live session does NOT rescue a newer
		// dead one — that prevents us from back-filling a stale UUID
		// when the user just did ForceFresh and the new session is
		// still warming up.
		home := withFakeHome(t)
		worktree := "/worktree/newer-dead"
		dir := filepath.Join(home, ".claude", "projects", encodeWorktree(worktree))

		liveEvents := []map[string]any{
			{
				"type": "assistant",
				"message": map[string]any{
					"role":    "assistant",
					"content": "Old session said something real.",
				},
			},
		}
		deadEvents := []map[string]any{
			{
				"type": "assistant",
				"message": map[string]any{
					"role":    "assistant",
					"content": "No response requested.",
				},
			},
		}
		olderPath := writeJSONL(t, dir, "11111111-1111-1111-1111-111111111111.jsonl", liveEvents)
		newerPath := writeJSONL(t, dir, "22222222-2222-2222-2222-222222222222.jsonl", deadEvents)

		oldTime := time.Now().Add(-2 * time.Hour)
		newTime := time.Now()
		if err := os.Chtimes(olderPath, oldTime, oldTime); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(newerPath, newTime, newTime); err != nil {
			t.Fatal(err)
		}

		if got := FindClaudeSession(worktree); got != "" {
			t.Errorf("FindClaudeSession = %q, want \"\" (newer file is dead)", got)
		}
	})
}

func TestDeleteClaudeSession(t *testing.T) {
	t.Run("empty_path", func(t *testing.T) {
		if err := DeleteClaudeSession(""); err != nil {
			t.Errorf("DeleteClaudeSession(\"\") err = %v, want nil", err)
		}
	})

	t.Run("removes_existing_file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "session.jsonl")
		if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := DeleteClaudeSession(path); err != nil {
			t.Fatalf("DeleteClaudeSession: %v", err)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("file still present after delete: %v", err)
		}
	})

	t.Run("nonexistent_path_returns_error", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "missing.jsonl")
		err := DeleteClaudeSession(path)
		if err == nil {
			t.Fatal("DeleteClaudeSession(missing) err = nil, want error")
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("err = %v, want one wrapping os.ErrNotExist", err)
		}
	})

	t.Run("uuid_path_purges_history", func(t *testing.T) {
		home := withFakeHome(t)
		uuid := "abcdef12-3456-4789-abcd-0123456789ab"
		// Place the transcript at the path DeleteClaudeSession is given.
		projDir := filepath.Join(home, ".claude", "projects", "enc")
		if err := os.MkdirAll(projDir, 0o755); err != nil {
			t.Fatal(err)
		}
		transcript := filepath.Join(projDir, uuid+".jsonl")
		if err := os.WriteFile(transcript, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		// Seed a history.jsonl with: a priming entry for our uuid, an
		// unrelated entry for the same uuid (user-typed), and an entry
		// for a different uuid.
		hist := filepath.Join(home, ".claude", "history.jsonl")
		entries := []map[string]any{
			{"sessionId": uuid, "display": ClaudePrimingPrefixes[0] + " more", "project": "/p"},
			{"sessionId": uuid, "display": "some user prompt", "project": "/p"},
			{"sessionId": "deadbeef-1234-4321-abcd-0123456789ab", "display": ClaudePrimingPrefixes[0] + " other", "project": "/p"},
		}
		writeHistory(t, hist, entries)

		if err := DeleteClaudeSession(transcript); err != nil {
			t.Fatalf("DeleteClaudeSession: %v", err)
		}
		// Transcript gone.
		if _, err := os.Stat(transcript); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("transcript still present: %v", err)
		}
		// History: only the priming entry for OUR uuid should be gone.
		got := readHistory(t, hist)
		if len(got) != 2 {
			t.Fatalf("history len = %d, want 2; got=%v", len(got), got)
		}
		if got[0]["display"] != "some user prompt" {
			t.Errorf("entry[0].display = %v, want user prompt", got[0]["display"])
		}
		if got[1]["sessionId"] != "deadbeef-1234-4321-abcd-0123456789ab" {
			t.Errorf("entry[1].sessionId = %v, want the other uuid", got[1]["sessionId"])
		}
	})

	t.Run("non_uuid_basename_skips_purge", func(t *testing.T) {
		home := withFakeHome(t)
		dir := t.TempDir()
		path := filepath.Join(dir, "not-a-uuid.jsonl")
		if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		hist := filepath.Join(home, ".claude", "history.jsonl")
		seeded := []map[string]any{
			{"sessionId": "abcdef12-3456-4789-abcd-0123456789ab", "display": ClaudePrimingPrefixes[0] + " x", "project": "/p"},
		}
		writeHistory(t, hist, seeded)

		if err := DeleteClaudeSession(path); err != nil {
			t.Fatalf("DeleteClaudeSession: %v", err)
		}
		// History untouched.
		got := readHistory(t, hist)
		if len(got) != 1 {
			t.Fatalf("history len = %d, want 1 (untouched)", len(got))
		}
	})
}

// writeHistory writes the given entries as JSONL at path, creating
// parent dirs as needed.
func writeHistory(t *testing.T, path string, entries []map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	var sb strings.Builder
	for _, e := range entries {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal entry: %v", err)
		}
		sb.Write(b)
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		t.Fatalf("write history: %v", err)
	}
}

// readHistory parses a JSONL file back into a slice of maps.
func readHistory(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open history: %v", err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("unmarshal line %q: %v", sc.Text(), err)
		}
		out = append(out, m)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan history: %v", err)
	}
	return out
}

func TestPurgeClaudePrimingHistory(t *testing.T) {
	const uuid = "abcdef12-3456-4789-abcd-0123456789ab"
	const otherUUID = "deadbeef-1234-4321-abcd-0123456789ab"
	prim := ClaudePrimingPrefixes[0] + " continued"

	t.Run("file_missing", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "history.jsonl")
		if err := PurgeClaudePrimingHistory(path, uuid, ClaudePrimingPrefixes...); err != nil {
			t.Errorf("err = %v, want nil", err)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("missing file was created: %v", err)
		}
	})

	t.Run("empty_uuid_refuses_wildcard", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "history.jsonl")
		entries := []map[string]any{{"sessionId": uuid, "display": prim}}
		writeHistory(t, path, entries)
		if err := PurgeClaudePrimingHistory(path, "", ClaudePrimingPrefixes...); err != nil {
			t.Errorf("err = %v, want nil", err)
		}
		if len(readHistory(t, path)) != 1 {
			t.Errorf("file was modified despite empty uuid")
		}
	})

	t.Run("no_prefixes_refuses_wildcard", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "history.jsonl")
		entries := []map[string]any{{"sessionId": uuid, "display": prim}}
		writeHistory(t, path, entries)
		if err := PurgeClaudePrimingHistory(path, uuid); err != nil {
			t.Errorf("err = %v, want nil", err)
		}
		if len(readHistory(t, path)) != 1 {
			t.Errorf("file was modified despite no prefixes")
		}
	})

	t.Run("single_match_removed", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "history.jsonl")
		entries := []map[string]any{{"sessionId": uuid, "display": prim}}
		writeHistory(t, path, entries)
		if err := PurgeClaudePrimingHistory(path, uuid, ClaudePrimingPrefixes...); err != nil {
			t.Fatalf("err = %v", err)
		}
		got := readHistory(t, path)
		if len(got) != 0 {
			t.Errorf("history len = %d, want 0", len(got))
		}
	})

	t.Run("uuid_match_prefix_mismatch_preserved", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "history.jsonl")
		entries := []map[string]any{{"sessionId": uuid, "display": "my own prompt"}}
		writeHistory(t, path, entries)
		if err := PurgeClaudePrimingHistory(path, uuid, ClaudePrimingPrefixes...); err != nil {
			t.Fatalf("err = %v", err)
		}
		got := readHistory(t, path)
		if len(got) != 1 || got[0]["display"] != "my own prompt" {
			t.Errorf("history = %v, want 1 entry preserved", got)
		}
	})

	t.Run("prefix_match_uuid_mismatch_preserved", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "history.jsonl")
		entries := []map[string]any{{"sessionId": otherUUID, "display": prim}}
		writeHistory(t, path, entries)
		if err := PurgeClaudePrimingHistory(path, uuid, ClaudePrimingPrefixes...); err != nil {
			t.Fatalf("err = %v", err)
		}
		got := readHistory(t, path)
		if len(got) != 1 || got[0]["sessionId"] != otherUUID {
			t.Errorf("history = %v, want other-uuid entry preserved", got)
		}
	})

	t.Run("mixed_multiple_matches", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "history.jsonl")
		// 5 entries: A:priming, A:user, B:priming, A:priming-resume, A:user-2
		// Expect: keep A:user, B:priming, A:user-2 (3 entries, order preserved).
		entries := []map[string]any{
			{"sessionId": uuid, "display": ClaudePrimingPrefixes[0] + " #1"},
			{"sessionId": uuid, "display": "user prompt 1"},
			{"sessionId": otherUUID, "display": ClaudePrimingPrefixes[0] + " B"},
			{"sessionId": uuid, "display": ClaudePrimingPrefixes[1] + "ticket\""},
			{"sessionId": uuid, "display": "user prompt 2"},
		}
		writeHistory(t, path, entries)
		if err := PurgeClaudePrimingHistory(path, uuid, ClaudePrimingPrefixes...); err != nil {
			t.Fatalf("err = %v", err)
		}
		got := readHistory(t, path)
		if len(got) != 3 {
			t.Fatalf("history len = %d, want 3; got=%v", len(got), got)
		}
		if got[0]["display"] != "user prompt 1" ||
			got[1]["sessionId"] != otherUUID ||
			got[2]["display"] != "user prompt 2" {
			t.Errorf("unexpected surviving entries: %v", got)
		}
	})

	t.Run("malformed_line_preserved", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "history.jsonl")
		// Mix: a valid match, a malformed garbage line, a valid non-match.
		var sb strings.Builder
		sb.WriteString(`{"sessionId":"` + uuid + `","display":"` + prim + `"}` + "\n")
		sb.WriteString("not-valid-json-at-all\n")
		sb.WriteString(`{"sessionId":"` + otherUUID + `","display":"keep me"}` + "\n")
		if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := PurgeClaudePrimingHistory(path, uuid, ClaudePrimingPrefixes...); err != nil {
			t.Fatalf("err = %v", err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
		if len(lines) != 2 {
			t.Fatalf("lines = %d, want 2; raw=%q", len(lines), raw)
		}
		if lines[0] != "not-valid-json-at-all" {
			t.Errorf("malformed line not preserved: got %q", lines[0])
		}
	})

	t.Run("oversized_line", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "history.jsonl")
		// 256KB display payload, no match.
		big := strings.Repeat("x", 256*1024)
		entries := []map[string]any{{"sessionId": otherUUID, "display": big}}
		writeHistory(t, path, entries)
		if err := PurgeClaudePrimingHistory(path, uuid, ClaudePrimingPrefixes...); err != nil {
			t.Fatalf("err = %v on oversized line", err)
		}
		got := readHistory(t, path)
		if len(got) != 1 {
			t.Errorf("history len = %d, want 1 (preserved)", len(got))
		}
	})

	t.Run("preserves_file_mode", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "history.jsonl")
		entries := []map[string]any{{"sessionId": uuid, "display": prim}}
		writeHistory(t, path, entries)
		// writeHistory sets 0o600; confirm purge preserves that.
		if err := PurgeClaudePrimingHistory(path, uuid, ClaudePrimingPrefixes...); err != nil {
			t.Fatalf("err = %v", err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("perm = %#o, want 0o600", perm)
		}
	})
}

// TestClaudePrimingPrefixes_MatchTemplate guards against drift between
// the ClaudePrimingPrefixes constants and the shipped template body. If
// agent_prompt.tmpl's leading sentences are edited, this test fails
// until the constants are updated in lockstep.
func TestClaudePrimingPrefixes_MatchTemplate(t *testing.T) {
	tmpl := config.DefaultAgentPrompt()
	if tmpl == "" {
		t.Fatal("DefaultAgentPrompt returned empty string")
	}

	t.Run("fresh_spawn_starts_with_prefix_0", func(t *testing.T) {
		data := ContextData{
			Title:        "fixture",
			BranchName:   "task/fixture",
			BaseBranch:   "main",
			Status:       "in_progress",
			WorktreePath: "/tmp/fixture",
		}
		got := BuildContextPrompt(tmpl, data)
		if !strings.HasPrefix(got, ClaudePrimingPrefixes[0]) {
			t.Errorf("fresh-spawn rendering does not start with prefix\n  prefix=%q\n  got=%q", ClaudePrimingPrefixes[0], got[:min(len(got), 200)])
		}
	})

	t.Run("external_resume_starts_with_prefix_1", func(t *testing.T) {
		data := ContextData{
			Title:            "fixture",
			BranchName:       "task/fixture",
			BaseBranch:       "main",
			Status:           "in_progress",
			WorktreePath:     "/tmp/fixture",
			IsExternalResume: true,
		}
		got := BuildContextPrompt(tmpl, data)
		if !strings.HasPrefix(got, ClaudePrimingPrefixes[1]) {
			t.Errorf("external-resume rendering does not start with prefix\n  prefix=%q\n  got=%q", ClaudePrimingPrefixes[1], got[:min(len(got), 200)])
		}
	})
}

// TestShippedTemplate_NoUnportablePriming guards the fix for the recurring
// "Unknown skill: /prime" failure: the shipped default template must not mandate
// a personal priming skill (/prime) nor leak a maintainer-specific memory path.
// Both branches (fresh spawn + external resume) are rendered and scanned. If a
// future edit reintroduces either, this fails. (text/template strips {{/* */}}
// comments, so the header-comment's mention of /prime never reaches output.)
func TestShippedTemplate_NoUnportablePriming(t *testing.T) {
	tmpl := config.DefaultAgentPrompt()
	base := ContextData{
		Title:        "fixture",
		BranchName:   "task/fixture",
		BaseBranch:   "main",
		Status:       "in_progress",
		WorktreePath: "/tmp/fixture",
		HasBrief:     true,
		BriefPath:    "tickets/fixture.md",
	}
	for _, resume := range []bool{false, true} {
		data := base
		data.IsExternalResume = resume
		got := BuildContextPrompt(tmpl, data)
		if strings.Contains(got, "/prime") {
			t.Errorf("rendered template (resume=%v) still mandates /prime:\n%s", resume, got)
		}
		if strings.Contains(got, "-Users-cmeid") {
			t.Errorf("rendered template (resume=%v) leaks a maintainer-specific memory path:\n%s", resume, got)
		}
	}
}

func TestProjectDirFor(t *testing.T) {
	home := withFakeHome(t)
	projects := filepath.Join(home, ".claude", "projects")
	cases := []struct {
		name    string
		workdir string
		want    string
	}{
		{"nested path", "/Users/cmeid/manifold/dev", filepath.Join(projects, "-Users-cmeid-manifold-dev")},
		{"worktree path", "/Users/cmeid/manifold/dev/openkanban", filepath.Join(projects, "-Users-cmeid-manifold-dev-openkanban")},
		// A literal `-` in the path encodes identically to a `/`, so this
		// collides with the "nested path" case above. That lossiness is
		// exactly why production reads the start cwd from disk rather than
		// decoding a bucket name back to a path.
		{"literal dash collides", "/Users/cmeid/manifold-dev", filepath.Join(projects, "-Users-cmeid-manifold-dev")},
		// Underscores encode to `-` — the exact regression that broke
		// resume when the Claude CLI changed its encoding (sessions filed
		// under the old `_`-preserving bucket became unreachable). `want`
		// is a hardcoded literal, NOT computed via any encoder helper, so
		// this asserts the rule rather than tautologically restating it.
		{"underscore path", "/Users/cmeid/manifold/dev/sa_csm_program", filepath.Join(projects, "-Users-cmeid-manifold-dev-sa-csm-program")},
		{"dot in path", "/Users/cmeid/repo.v2", filepath.Join(projects, "-Users-cmeid-repo-v2")},
		{"space in path", "/Users/cmeid/my project", filepath.Join(projects, "-Users-cmeid-my-project")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ProjectDirFor(tc.workdir)
			if err != nil {
				t.Fatalf("ProjectDirFor: %v", err)
			}
			if got != tc.want {
				t.Errorf("ProjectDirFor(%q) = %q, want %q", tc.workdir, got, tc.want)
			}
		})
	}
}

func TestResumeResolvable(t *testing.T) {
	const uuid = "7f3a9b2c-1d8e-4a5b-9c3d-2f1e0a8b9c4d"
	worktree := "/Users/cmeid/manifold/dev/sa_csm_program"

	t.Run("present_in_launch_bucket", func(t *testing.T) {
		home := withFakeHome(t)
		// Literal NEW (dash) bucket = what the live CLI and ProjectDirFor use.
		bucket := filepath.Join(home, ".claude", "projects", "-Users-cmeid-manifold-dev-sa-csm-program")
		writeFile(t, filepath.Join(bucket, uuid+".jsonl"), "{}\n")
		if !ResumeResolvable(uuid, worktree) {
			t.Fatal("ResumeResolvable = false, want true (transcript is in the launch bucket)")
		}
	})

	t.Run("missing_entirely", func(t *testing.T) {
		withFakeHome(t)
		if ResumeResolvable(uuid, worktree) {
			t.Fatal("ResumeResolvable = true, want false (no transcript anywhere — lost session)")
		}
	})

	t.Run("only_in_old_encoding_bucket", func(t *testing.T) {
		home := withFakeHome(t)
		// Literal OLD (underscore) bucket: the live CLI won't look here, so
		// resume is NOT resolvable until NormalizeSessionBucket relocates it.
		oldBucket := filepath.Join(home, ".claude", "projects", "-Users-cmeid-manifold-dev-sa_csm_program")
		writeFile(t, filepath.Join(oldBucket, uuid+".jsonl"), "{}\n")
		if ResumeResolvable(uuid, worktree) {
			t.Fatal("ResumeResolvable = true, want false (transcript only in the stale underscore bucket)")
		}
	})

	t.Run("not_a_uuid", func(t *testing.T) {
		withFakeHome(t)
		if ResumeResolvable("nope", worktree) {
			t.Fatal("ResumeResolvable = true, want false (non-UUID ref)")
		}
	})

	t.Run("empty_worktree", func(t *testing.T) {
		withFakeHome(t)
		if ResumeResolvable(uuid, "") {
			t.Fatal("ResumeResolvable = true, want false (no launch cwd)")
		}
	})
}

func mustExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected %s to exist: %v", path, err)
	}
}

func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected %s to be gone, stat err = %v", path, err)
	}
}

func TestNormalizeSessionBucket(t *testing.T) {
	const uuid = "7f3a9b2c-1d8e-4a5b-9c3d-2f1e0a8b9c4d"

	// placeSession writes <uuid>.jsonl plus a sibling <uuid>/ artifact dir
	// (subagents/ + tool-results/) into the bucket for `cwd`, mirroring
	// Claude Code's on-disk session layout. Returns the bucket path.
	placeSession := func(t *testing.T, home, cwd string) string {
		t.Helper()
		bucket := filepath.Join(home, ".claude", "projects", encodeWorktree(cwd))
		writeFile(t, filepath.Join(bucket, uuid+".jsonl"), "{}\n")
		writeFile(t, filepath.Join(bucket, uuid, "subagents", "agent-x.jsonl"), "{}\n")
		writeFile(t, filepath.Join(bucket, uuid, "tool-results", "r.txt"), "result")
		return bucket
	}

	t.Run("not_a_uuid", func(t *testing.T) {
		withFakeHome(t)
		if moved, err := NormalizeSessionBucket("nope", "/work/tree"); moved || err != nil {
			t.Fatalf("got (moved=%v, err=%v), want (false, nil)", moved, err)
		}
	})

	t.Run("empty_worktree", func(t *testing.T) {
		withFakeHome(t)
		if moved, err := NormalizeSessionBucket(uuid, ""); moved || err != nil {
			t.Fatalf("got (moved=%v, err=%v), want (false, nil)", moved, err)
		}
	})

	t.Run("missing_session", func(t *testing.T) {
		withFakeHome(t)
		if moved, err := NormalizeSessionBucket(uuid, "/work/tree"); moved || err != nil {
			t.Fatalf("got (moved=%v, err=%v), want (false, nil)", moved, err)
		}
	})

	t.Run("already_canonical", func(t *testing.T) {
		home := withFakeHome(t)
		cwd := "/work/tree"
		bucket := placeSession(t, home, cwd)
		if moved, err := NormalizeSessionBucket(uuid, cwd); moved || err != nil {
			t.Fatalf("got (moved=%v, err=%v), want (false, nil)", moved, err)
		}
		mustExist(t, filepath.Join(bucket, uuid+".jsonl"))
	})

	t.Run("relocates_transcript_and_sidecar", func(t *testing.T) {
		if _, err := exec.LookPath("lsof"); err != nil {
			t.Skip("lsof not on PATH; skipping")
		}
		home := withFakeHome(t)
		bornCwd := "/Users/cmeid/manifold/dev"
		worktree := "/Users/cmeid/manifold/dev/openkanban"
		fromBucket := placeSession(t, home, bornCwd)

		moved, err := NormalizeSessionBucket(uuid, worktree)
		if err != nil {
			t.Fatalf("NormalizeSessionBucket: %v", err)
		}
		if !moved {
			t.Fatal("moved = false, want true")
		}
		toBucket := filepath.Join(home, ".claude", "projects", encodeWorktree(worktree))
		mustExist(t, filepath.Join(toBucket, uuid+".jsonl"))
		mustExist(t, filepath.Join(toBucket, uuid, "subagents", "agent-x.jsonl"))
		if b, err := os.ReadFile(filepath.Join(toBucket, uuid, "tool-results", "r.txt")); err != nil || string(b) != "result" {
			t.Errorf("tool-result content = %q (err %v), want %q", string(b), err, "result")
		}
		mustNotExist(t, filepath.Join(fromBucket, uuid+".jsonl"))
		mustNotExist(t, filepath.Join(fromBucket, uuid))
	})

	t.Run("relocates_across_cli_encoding_change", func(t *testing.T) {
		if _, err := exec.LookPath("lsof"); err != nil {
			t.Skip("lsof not on PATH; skipping")
		}
		home := withFakeHome(t)
		// Reproduces the live resume regression. The transcript was filed by
		// an older Claude CLI under the OLD bucket encoding (underscores
		// preserved); the current CLI — and the fixed ProjectDirFor — read the
		// NEW bucket (underscores → '-'). NormalizeSessionBucket must relocate
		// across that rename so `claude --resume` finds it.
		//
		// Both bucket names are HARDCODED LITERALS, deliberately decoupled from
		// EncodeClaudeBucket/ProjectDirFor. If this test seeded via the encoder
		// helper, reverting the encoder fix would move the seed in lockstep with
		// the canonical target, the relocation would short-circuit as
		// "already canonical", and the test would pass vacuously. With literals,
		// reverting ProjectDirFor to the old '/'-only encoding makes canonicalDir
		// equal the underscore seed bucket → no-op → this test FAILS (red).
		worktree := "/Users/cmeid/manifold/dev/sa_csm_program"
		projects := filepath.Join(home, ".claude", "projects")
		oldBucket := filepath.Join(projects, "-Users-cmeid-manifold-dev-sa_csm_program")
		newBucket := filepath.Join(projects, "-Users-cmeid-manifold-dev-sa-csm-program")

		writeFile(t, filepath.Join(oldBucket, uuid+".jsonl"), "{}\n")
		writeFile(t, filepath.Join(oldBucket, uuid, "subagents", "agent-x.jsonl"), "{}\n")
		writeFile(t, filepath.Join(oldBucket, uuid, "tool-results", "r.txt"), "result")

		moved, err := NormalizeSessionBucket(uuid, worktree)
		if err != nil {
			t.Fatalf("NormalizeSessionBucket: %v", err)
		}
		if !moved {
			t.Fatal("moved = false, want true (transcript must relocate from the old underscore bucket to the new dash bucket)")
		}
		mustExist(t, filepath.Join(newBucket, uuid+".jsonl"))
		mustExist(t, filepath.Join(newBucket, uuid, "subagents", "agent-x.jsonl"))
		if b, err := os.ReadFile(filepath.Join(newBucket, uuid, "tool-results", "r.txt")); err != nil || string(b) != "result" {
			t.Errorf("tool-result content = %q (err %v), want %q", string(b), err, "result")
		}
		mustNotExist(t, filepath.Join(oldBucket, uuid+".jsonl"))
		mustNotExist(t, filepath.Join(oldBucket, uuid))
	})

	t.Run("live_session_skipped", func(t *testing.T) {
		if _, err := exec.LookPath("lsof"); err != nil {
			t.Skip("lsof not on PATH; skipping")
		}
		if _, err := exec.LookPath("tail"); err != nil {
			t.Skip("tail not on PATH; skipping")
		}
		home := withFakeHome(t)
		fromBucket := placeSession(t, home, "/born/elsewhere")
		jsonlPath := filepath.Join(fromBucket, uuid+".jsonl")

		cmd := exec.Command("tail", "-f", jsonlPath)
		if err := cmd.Start(); err != nil {
			t.Fatalf("start tail: %v", err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if h, _ := SessionActive(uuid); h.PID != 0 {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}

		moved, err := NormalizeSessionBucket(uuid, "/work/tree")
		if err != nil {
			t.Fatalf("NormalizeSessionBucket: %v", err)
		}
		if moved {
			t.Fatal("moved = true, want false (a live session must be skipped)")
		}
		mustExist(t, jsonlPath)
	})

	t.Run("collision_refused", func(t *testing.T) {
		if _, err := exec.LookPath("lsof"); err != nil {
			t.Skip("lsof not on PATH; skipping")
		}
		home := withFakeHome(t)
		// "/born/elsewhere" → "-born-elsewhere" sorts before
		// "/work/tree" → "-work-tree", so SessionPath's sorted glob
		// returns the foreign transcript first and we reach the
		// collision check rather than the already-canonical short-circuit.
		fromBucket := placeSession(t, home, "/born/elsewhere")
		worktree := "/work/tree"
		toBucket := filepath.Join(home, ".claude", "projects", encodeWorktree(worktree))
		writeFile(t, filepath.Join(toBucket, uuid+".jsonl"), "{}\n")

		moved, err := NormalizeSessionBucket(uuid, worktree)
		if err == nil {
			t.Fatal("expected a collision error, got nil")
		}
		if moved {
			t.Fatal("moved = true on collision, want false")
		}
		mustExist(t, filepath.Join(fromBucket, uuid+".jsonl"))
	})
}

// TestDefaultPrompt_WrapUpContract pins the standardized close-out contract
// in the shipped template's wrap-up section. The fresh-spawn branch must NOT
// carry the retired "you must not push/merge/PR by default" prohibition, and
// MUST tell the agent to land via the skill's single permission prompt. The
// prefix-drift guard above only checks the leading sentence, so without this
// test the wrap-up prose could regress to the old contradiction unnoticed.
func TestDefaultPrompt_WrapUpContract(t *testing.T) {
	tmpl := config.DefaultAgentPrompt()
	if tmpl == "" {
		t.Fatal("DefaultAgentPrompt returned empty string")
	}
	// Fresh-spawn branch (IsExternalResume:false) — the wrap-up prose under
	// test lives in this branch. A resume fixture would false-pass the
	// absence checks because it never contained the prohibition.
	got := BuildContextPrompt(tmpl, ContextData{
		Title:        "fixture",
		BranchName:   "task/fixture",
		BaseBranch:   "main",
		Status:       "in_progress",
		WorktreePath: "/tmp/fixture",
	})

	// Retired prohibitions must be absent.
	for _, banned := range []string{
		"you must not:",
		"open a PR by default",
		"pushing is per-repo and the user owns the call",
	} {
		if strings.Contains(got, banned) {
			t.Errorf("wrap-up section still contains retired prohibition %q — it now lands work via the skill", banned)
		}
	}

	// New contract must be present. Anchor on phrases unique to the
	// wrap-up section so deleting that paragraph actually fails the test
	// ("commit → PR → merge" and "permission prompt" appear nowhere else
	// in the rendered fresh-spawn prompt).
	for _, want := range []string{
		"commit → PR → merge",
		"permission prompt",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("wrap-up section missing new-contract language %q", want)
		}
	}
	// The skill is still the named close-out entry point.
	if !strings.Contains(got, "finishing-an-openkanban-ticket") {
		t.Error("wrap-up section no longer names the finishing-an-openkanban-ticket skill")
	}
}

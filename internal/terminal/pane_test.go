package terminal

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	xvt "github.com/charmbracelet/x/vt"
)

func TestDetectMouseModeChanges(t *testing.T) {
	tests := []struct {
		name           string
		data           []byte
		initialEnabled bool
		wantEnabled    bool
	}{
		{
			name:           "X10 mouse tracking enable",
			data:           []byte("\x1b[?1000h"),
			initialEnabled: false,
			wantEnabled:    true,
		},
		{
			name:           "Button-event tracking enable",
			data:           []byte("\x1b[?1002h"),
			initialEnabled: false,
			wantEnabled:    true,
		},
		{
			name:           "Any-event tracking enable",
			data:           []byte("\x1b[?1003h"),
			initialEnabled: false,
			wantEnabled:    true,
		},
		{
			name:           "SGR extended mode enable",
			data:           []byte("\x1b[?1006h"),
			initialEnabled: false,
			wantEnabled:    true,
		},
		{
			name:           "X10 mouse tracking disable",
			data:           []byte("\x1b[?1000l"),
			initialEnabled: true,
			wantEnabled:    false,
		},
		{
			name:           "Button-event tracking disable",
			data:           []byte("\x1b[?1002l"),
			initialEnabled: true,
			wantEnabled:    false,
		},
		{
			name:           "Any-event tracking disable",
			data:           []byte("\x1b[?1003l"),
			initialEnabled: true,
			wantEnabled:    false,
		},
		{
			name:           "SGR extended mode disable",
			data:           []byte("\x1b[?1006l"),
			initialEnabled: true,
			wantEnabled:    false,
		},
		{
			name:           "Sequence embedded in other data",
			data:           []byte("some text\x1b[?1000hmore text"),
			initialEnabled: false,
			wantEnabled:    true,
		},
		{
			name:           "No mouse sequence - state unchanged",
			data:           []byte("regular terminal output"),
			initialEnabled: false,
			wantEnabled:    false,
		},
		{
			name:           "No mouse sequence - enabled stays enabled",
			data:           []byte("regular terminal output"),
			initialEnabled: true,
			wantEnabled:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &Pane{mouseEnabled: tt.initialEnabled}
			p.detectMouseModeChanges(tt.data)
			if p.mouseEnabled != tt.wantEnabled {
				t.Errorf("mouseEnabled = %v, want %v", p.mouseEnabled, tt.wantEnabled)
			}
		})
	}
}

func TestDetectAltScreenChanges(t *testing.T) {
	tests := []struct {
		name         string
		data         []byte
		initialState bool
		expectedState bool
	}{
		{
			name:         "Enable alt screen 1049h",
			data:         []byte("\x1b[?1049h"),
			initialState: false,
			expectedState: true,
		},
		{
			name:         "Enable alt screen 47h",
			data:         []byte("\x1b[?47h"),
			initialState: false,
			expectedState: true,
		},
		{
			name:         "Disable alt screen 1049l",
			data:         []byte("\x1b[?1049l"),
			initialState: true,
			expectedState: false,
		},
		{
			name:         "Disable alt screen 47l",
			data:         []byte("\x1b[?47l"),
			initialState: true,
			expectedState: false,
		},
		{
			name:         "Sequence embedded in other data",
			data:         []byte("Hello\x1b[?1049hWorld"),
			initialState: false,
			expectedState: true,
		},
		{
			name:         "No alt screen sequence - state unchanged",
			data:         []byte("Hello World"),
			initialState: false,
			expectedState: false,
		},
		{
			name:         "No alt screen sequence - enabled stays enabled",
			data:         []byte("Hello World"),
			initialState: true,
			expectedState: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pane := New("test", 80, 24, 1000)
			pane.altScreenActive = tc.initialState
			pane.detectAltScreenChanges(tc.data)
			if pane.altScreenActive != tc.expectedState {
				t.Errorf("expected altScreenActive=%v, got %v", tc.expectedState, pane.altScreenActive)
			}
		})
	}
}

func TestParseOscTitlePayload(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "OSC 0 prefix", in: "0;hello-title", want: "hello-title"},
		{name: "OSC 2 prefix", in: "2;window-title", want: "window-title"},
		{name: "OSC 1 prefix", in: "1;icon name", want: "icon name"},
		{name: "Multi-digit prefix", in: "10;color-payload", want: "color-payload"},
		{name: "No prefix", in: "bare title", want: "bare title"},
		{name: "Empty payload after prefix", in: "2;", want: ""},
		{name: "Title contains semicolons", in: "2;a;b;c", want: "a;b;c"},
		{name: "Empty input", in: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseOscTitlePayload([]byte(tt.in))
			if got != tt.want {
				t.Errorf("parseOscTitlePayload(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestPaneOscTitleCapture wires the title handlers onto a fresh emulator
// and writes a raw OSC 2 escape sequence through it. Exercises the full
// path from emulator parse → registered handler → atomic Title()
// without spinning up a PTY (which would require a forked subprocess).
func TestPaneOscTitleCapture(t *testing.T) {
	p := New("test", 80, 24, 1000)
	p.vt = xvt.NewSafeEmulator(80, 24)
	p.registerTitleHandlersUnlocked()

	if got := p.Title(); got != "" {
		t.Fatalf("Title before any OSC = %q, want empty", got)
	}

	// OSC 2 (set window title), terminated with BEL.
	p.vt.Write([]byte("\x1b]2;hello-title\x07"))
	if got := p.Title(); got != "hello-title" {
		t.Errorf("after OSC 2, Title = %q, want %q", got, "hello-title")
	}

	// OSC 0 (set window title + icon name), terminated with ST.
	p.vt.Write([]byte("\x1b]0;next-title\x1b\\"))
	if got := p.Title(); got != "next-title" {
		t.Errorf("after OSC 0, Title = %q, want %q", got, "next-title")
	}
}

// TestPaneLastActivity_AdvancesOnHandleOutput pins the daemon-side
// half of the activity-override feature: any non-empty handleOutput
// call must advance LastActivity, and the timestamp must be readable
// without taking p.mu (it's the daemon broadcaster's lock-free path).
// An empty Pane reports the zero time.
func TestPaneLastActivity_AdvancesOnHandleOutput(t *testing.T) {
	p := New("test", 80, 24, 1000)
	p.vt = xvt.NewSafeEmulator(80, 24)

	if !p.LastActivity().IsZero() {
		t.Fatalf("LastActivity before any output = %v, want zero", p.LastActivity())
	}

	before := time.Now()
	p.handleOutput([]byte("hello"))
	first := p.LastActivity()
	if first.IsZero() {
		t.Fatalf("LastActivity after handleOutput is still zero")
	}
	if first.Before(before) {
		t.Errorf("LastActivity %v is before the call site time %v", first, before)
	}

	// Second write must advance the timestamp strictly forward. Sleep
	// past the nanosecond clock's resolution to avoid a flaky equal.
	time.Sleep(time.Millisecond)
	p.handleOutput([]byte("world"))
	second := p.LastActivity()
	if !second.After(first) {
		t.Errorf("second LastActivity %v did not advance past first %v", second, first)
	}
}

// TestPaneLastActivity_EmptyDataDoesNotStamp guards against a class of
// false-positive activity: an internal caller invoking handleOutput
// with no bytes (e.g. an empty pty.Read or a guard call) must not
// advance the timestamp, or idle sessions would look "active" to the
// override.
func TestPaneLastActivity_EmptyDataDoesNotStamp(t *testing.T) {
	p := New("test", 80, 24, 1000)
	p.vt = xvt.NewSafeEmulator(80, 24)

	p.handleOutput([]byte("seed"))
	seeded := p.LastActivity()
	if seeded.IsZero() {
		t.Fatalf("seed write didn't stamp activity")
	}

	time.Sleep(time.Millisecond)
	p.handleOutput(nil)
	p.handleOutput([]byte{})
	after := p.LastActivity()
	if !after.Equal(seeded) {
		t.Errorf("empty handleOutput moved LastActivity from %v to %v", seeded, after)
	}
}


func TestTranslateKey(t *testing.T) {
	tests := []struct {
		name string
		msg  tea.KeyMsg
		want []byte
	}{
		{
			name: "Tab",
			msg:  tea.KeyMsg{Type: tea.KeyTab},
			want: []byte("\t"),
		},
		{
			name: "Shift+Tab emits CSI Z",
			msg:  tea.KeyMsg{Type: tea.KeyShiftTab},
			want: []byte("\x1b[Z"),
		},
		{
			name: "Enter",
			msg:  tea.KeyMsg{Type: tea.KeyEnter},
			want: []byte("\r"),
		},
		{
			// Terminals configured for shift+enter (iTerm2/Ghostty/etc.)
			// emit ESC+CR; bubbletea v1 reports that as Alt+Enter since
			// it has no Shift field. Pass it through so the inner agent
			// (e.g. Claude Code) sees a real meta-Enter, not bare CR.
			name: "Alt+Enter (shift+enter from terminal) emits ESC+CR",
			msg:  tea.KeyMsg{Type: tea.KeyEnter, Alt: true},
			want: []byte{27, '\r'},
		},
		{
			name: "Up arrow",
			msg:  tea.KeyMsg{Type: tea.KeyUp},
			want: []byte("\x1b[A"),
		},
	}

	p := &Pane{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := p.translateKey(tt.msg)
			if !bytes.Equal(got, tt.want) {
				t.Errorf("translateKey(%v) = %q, want %q", tt.msg, got, tt.want)
			}
		})
	}
}

// TestTranslateKey_DECCKM exercises the application-cursor-keys branch
// of translateKey. When DECCKM is set (cursorAppMode true), arrow keys
// must encode as SS3 (ESC O A/B/C/D); when reset, they use CSI. This is
// what charm/x/vt's SendKey does internally and what iTerm2 emits —
// without it, Claude Code's input handler can't tell openkanban's
// arrows from generic cursor-up-in-text, which scrolls/mutates the
// chat view instead of navigating prompt history.
func TestTranslateKey_DECCKM(t *testing.T) {
	tests := []struct {
		name string
		msg  tea.KeyMsg
		on   []byte
		off  []byte
	}{
		{name: "Up", msg: tea.KeyMsg{Type: tea.KeyUp}, on: []byte("\x1bOA"), off: []byte("\x1b[A")},
		{name: "Down", msg: tea.KeyMsg{Type: tea.KeyDown}, on: []byte("\x1bOB"), off: []byte("\x1b[B")},
		{name: "Right", msg: tea.KeyMsg{Type: tea.KeyRight}, on: []byte("\x1bOC"), off: []byte("\x1b[C")},
		{name: "Left", msg: tea.KeyMsg{Type: tea.KeyLeft}, on: []byte("\x1bOD"), off: []byte("\x1b[D")},
	}

	for _, tt := range tests {
		t.Run(tt.name+"/off", func(t *testing.T) {
			p := &Pane{}
			got := p.translateKey(tt.msg)
			if !bytes.Equal(got, tt.off) {
				t.Errorf("DECCKM off: translateKey(%v) = %q, want %q", tt.msg, got, tt.off)
			}
		})
		t.Run(tt.name+"/on", func(t *testing.T) {
			p := &Pane{}
			p.cursorAppMode.Store(true)
			got := p.translateKey(tt.msg)
			if !bytes.Equal(got, tt.on) {
				t.Errorf("DECCKM on: translateKey(%v) = %q, want %q", tt.msg, got, tt.on)
			}
		})
	}
}

// TestPaneCursorAppModeCallback wires the EnableMode/DisableMode
// callbacks against a real emulator and feeds the DECCKM enable/reset
// byte sequences. Pins the integration that translateKey's branch
// relies on: the callback fires synchronously inside vt.Write, the
// atomic flips, and a subsequent key translation sees the new value.
// No PTY needed.
func TestPaneCursorAppModeCallback(t *testing.T) {
	p := New("test", 80, 24, 1000)
	p.vt = xvt.NewSafeEmulator(80, 24)
	p.vt.SetCallbacks(xvt.Callbacks{
		EnableMode: func(mode ansi.Mode) {
			if mode == ansi.ModeCursorKeys {
				p.cursorAppMode.Store(true)
			}
		},
		DisableMode: func(mode ansi.Mode) {
			if mode == ansi.ModeCursorKeys {
				p.cursorAppMode.Store(false)
			}
		},
	})

	if p.cursorAppMode.Load() {
		t.Fatalf("cursorAppMode before any DECCKM byte = true, want false")
	}
	if got := p.translateKey(tea.KeyMsg{Type: tea.KeyUp}); !bytes.Equal(got, []byte("\x1b[A")) {
		t.Fatalf("baseline Up encoding = %q, want %q", got, "\x1b[A")
	}

	p.vt.Write([]byte("\x1b[?1h"))
	if !p.cursorAppMode.Load() {
		t.Fatalf("after ESC[?1h, cursorAppMode = false, want true")
	}
	if got := p.translateKey(tea.KeyMsg{Type: tea.KeyUp}); !bytes.Equal(got, []byte("\x1bOA")) {
		t.Errorf("DECCKM-on Up encoding = %q, want %q", got, "\x1bOA")
	}

	p.vt.Write([]byte("\x1b[?1l"))
	if p.cursorAppMode.Load() {
		t.Fatalf("after ESC[?1l, cursorAppMode = true, want false")
	}
	if got := p.translateKey(tea.KeyMsg{Type: tea.KeyUp}); !bytes.Equal(got, []byte("\x1b[A")) {
		t.Errorf("post-reset Up encoding = %q, want %q", got, "\x1b[A")
	}
}

func TestBuildCleanEnv(t *testing.T) {
	tests := []struct {
		name         string
		sessionName  string
		ticketID     string
		wantSession  string // "" means must be absent
		wantTicketID string // "" means must be absent
	}{
		{
			name:         "both set",
			sessionName:  "task/foo",
			ticketID:     "abc-123",
			wantSession:  "OPENKANBAN_SESSION=task/foo",
			wantTicketID: "OPENKANBAN_TICKET_ID=abc-123",
		},
		{
			name:        "session only",
			sessionName: "task/foo",
			wantSession: "OPENKANBAN_SESSION=task/foo",
		},
		{
			name:         "ticket only",
			ticketID:     "abc-123",
			wantTicketID: "OPENKANBAN_TICKET_ID=abc-123",
		},
		{
			name: "neither",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := buildCleanEnv(tt.sessionName, tt.ticketID)

			contains := func(target string) bool {
				for _, e := range env {
					if e == target {
						return true
					}
				}
				return false
			}
			anyWithPrefix := func(prefix string) bool {
				for _, e := range env {
					if strings.HasPrefix(e, prefix) {
						return true
					}
				}
				return false
			}

			if tt.wantSession != "" && !contains(tt.wantSession) {
				t.Errorf("missing %q in env", tt.wantSession)
			}
			if tt.wantSession == "" && anyWithPrefix("OPENKANBAN_SESSION=") {
				t.Errorf("OPENKANBAN_SESSION must be absent when sessionName empty; got env=%v", env)
			}
			if tt.wantTicketID != "" && !contains(tt.wantTicketID) {
				t.Errorf("missing %q in env", tt.wantTicketID)
			}
			if tt.wantTicketID == "" && anyWithPrefix("OPENKANBAN_TICKET_ID=") {
				t.Errorf("OPENKANBAN_TICKET_ID must be absent when ticketID empty; got env=%v", env)
			}
			if !contains("TERM=xterm-256color") {
				t.Errorf("expected TERM=xterm-256color in env")
			}
		})
	}
}

// TestBuildCleanEnv_StripsInheritedOpenkanban guards the env-leak fix
// in T2 of the integration plan: any OPENKANBAN_* in the inherited
// process env MUST be stripped before the freshly-spawned values are
// appended, so a nested openkanban pane (or a daemon-spawned session
// whose daemon process itself has those vars set) doesn't accidentally
// inherit the outer pane's identity.
func TestBuildCleanEnv_StripsInheritedOpenkanban(t *testing.T) {
	t.Setenv("OPENKANBAN_SESSION", "leaky")
	t.Setenv("OPENKANBAN_TICKET_ID", "stale-id")
	t.Setenv("OPENKANBAN_PTY_DEBUG_LOG", "/tmp/whatever")

	env := buildCleanEnv("fresh-session", "T-1")

	contains := func(target string) bool {
		for _, e := range env {
			if e == target {
				return true
			}
		}
		return false
	}
	anyWithPrefix := func(prefix string) bool {
		for _, e := range env {
			if strings.HasPrefix(e, prefix) {
				return true
			}
		}
		return false
	}

	if !contains("OPENKANBAN_SESSION=fresh-session") {
		t.Errorf("expected OPENKANBAN_SESSION=fresh-session; env=%v", env)
	}
	if !contains("OPENKANBAN_TICKET_ID=T-1") {
		t.Errorf("expected OPENKANBAN_TICKET_ID=T-1; env=%v", env)
	}
	// Specifically: the inherited leaky values must NOT have made it
	// through. Each var must appear exactly once, with the fresh value.
	for _, e := range env {
		if e == "OPENKANBAN_SESSION=leaky" {
			t.Errorf("inherited OPENKANBAN_SESSION=leaky leaked through; env=%v", env)
		}
		if e == "OPENKANBAN_TICKET_ID=stale-id" {
			t.Errorf("inherited OPENKANBAN_TICKET_ID=stale-id leaked through; env=%v", env)
		}
	}
	// And no other OPENKANBAN_* (e.g. OPENKANBAN_PTY_DEBUG_LOG) should
	// have survived the strip. OPENKANBAN_BIN is exempt: it is synthesized
	// fresh by buildCleanEnv (like SESSION/TICKET_ID), not inherited.
	for _, e := range env {
		if strings.HasPrefix(e, "OPENKANBAN_") &&
			!strings.HasPrefix(e, "OPENKANBAN_SESSION=") &&
			!strings.HasPrefix(e, "OPENKANBAN_TICKET_ID=") &&
			!strings.HasPrefix(e, "OPENKANBAN_BIN=") {
			t.Errorf("unexpected inherited OPENKANBAN_* survived strip: %q", e)
		}
	}
	if anyWithPrefix("OPENKANBAN_PTY_DEBUG_LOG") {
		t.Errorf("OPENKANBAN_PTY_DEBUG_LOG should have been stripped; env=%v", env)
	}
}

// TestBuildCleanEnv_InjectsSelfBinOntoPath verifies buildCleanEnv front-loads
// the running openkanban binary's directory onto the spawned agent's PATH and
// exports OPENKANBAN_BIN. This is the fix for the "unknown command \"ticket\""
// failure: a spawned agent's non-interactive shell must resolve `openkanban`
// to the same build that owns the board (which has `ticket new`), not to a
// stale binary earlier on the user's PATH.
func TestBuildCleanEnv_InjectsSelfBinOntoPath(t *testing.T) {
	const fakeExe = "/opt/okb-selftest/bin/openkanban"
	orig := selfExecutable
	selfExecutable = func() (string, error) { return fakeExe, nil }
	t.Cleanup(func() { selfExecutable = orig })

	t.Setenv("PATH", "/usr/bin:/bin")
	env := buildCleanEnv("task/foo", "abc-123")

	get := func(key string) (string, bool) {
		for _, e := range env {
			if strings.HasPrefix(e, key+"=") {
				return strings.TrimPrefix(e, key+"="), true
			}
		}
		return "", false
	}

	wantDir := filepath.Dir(fakeExe) // "/opt/okb-selftest/bin"
	gotPath, ok := get("PATH")
	if !ok {
		t.Fatalf("PATH missing from env; env=%v", env)
	}
	wantPath := wantDir + string(os.PathListSeparator) + "/usr/bin:/bin"
	if gotPath != wantPath {
		t.Errorf("PATH = %q, want %q", gotPath, wantPath)
	}

	// Exactly one PATH entry — a duplicate makes resolution order ambiguous
	// across libc implementations.
	pathCount := 0
	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			pathCount++
		}
	}
	if pathCount != 1 {
		t.Errorf("expected exactly one PATH entry, got %d; env=%v", pathCount, env)
	}

	if gotBin, ok := get("OPENKANBAN_BIN"); !ok || gotBin != fakeExe {
		t.Errorf("OPENKANBAN_BIN = %q (present=%v), want %q", gotBin, ok, fakeExe)
	}
}

// TestBuildCleanEnv_SelfBinUnresolvableLeavesPath verifies that when the
// running binary can't be resolved, buildCleanEnv leaves PATH untouched and
// omits OPENKANBAN_BIN rather than corrupting the agent's PATH.
func TestBuildCleanEnv_SelfBinUnresolvableLeavesPath(t *testing.T) {
	orig := selfExecutable
	selfExecutable = func() (string, error) { return "", fmt.Errorf("boom") }
	t.Cleanup(func() { selfExecutable = orig })

	t.Setenv("PATH", "/usr/bin:/bin")
	env := buildCleanEnv("task/foo", "abc-123")

	for _, e := range env {
		if strings.HasPrefix(e, "OPENKANBAN_BIN=") {
			t.Errorf("OPENKANBAN_BIN must be absent when self-resolution fails; got %q", e)
		}
		if strings.HasPrefix(e, "PATH=") && e != "PATH=/usr/bin:/bin" {
			t.Errorf("PATH must be untouched when self-resolution fails; got %q", e)
		}
	}
}

// TestBuildCleanEnv_SkipsInjectWhenSelfAlreadyOnPath verifies buildCleanEnv
// does NOT touch PATH when a bare `openkanban` already resolves to this build
// — the single-install case must stay a no-op (no reorder, no duplicate),
// while OPENKANBAN_BIN is still exported.
func TestBuildCleanEnv_SkipsInjectWhenSelfAlreadyOnPath(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "openkanban")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	resolvedExe := exe
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		resolvedExe = r
	}

	orig := selfExecutable
	selfExecutable = func() (string, error) { return exe, nil }
	t.Cleanup(func() { selfExecutable = orig })

	// Put our temp dir on PATH so `openkanban` already resolves to us.
	inheritedPath := dir + string(os.PathListSeparator) + "/usr/bin"
	t.Setenv("PATH", inheritedPath)
	env := buildCleanEnv("task/foo", "abc-123")

	var gotPath string
	pathCount := 0
	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			gotPath = strings.TrimPrefix(e, "PATH=")
			pathCount++
		}
	}
	if pathCount != 1 {
		t.Fatalf("expected exactly one PATH entry, got %d; env=%v", pathCount, env)
	}
	if gotPath != inheritedPath {
		t.Errorf("PATH = %q, want it left unchanged %q", gotPath, inheritedPath)
	}
	// OPENKANBAN_BIN is still exported (points at the resolved self).
	found := false
	for _, e := range env {
		if e == "OPENKANBAN_BIN="+resolvedExe {
			found = true
		}
	}
	if !found {
		t.Errorf("OPENKANBAN_BIN=%q not found; env=%v", resolvedExe, env)
	}
}

// TestBuildCleanEnv_AddsPathWhenInheritedEnvHasNone covers the fallback branch:
// when the inherited env carries no PATH at all, buildCleanEnv still adds
// PATH=<self bin dir> so the injected openkanban stays findable.
func TestBuildCleanEnv_AddsPathWhenInheritedEnvHasNone(t *testing.T) {
	const fakeExe = "/opt/okb-selftest/bin/openkanban"
	orig := selfExecutable
	selfExecutable = func() (string, error) { return fakeExe, nil }
	t.Cleanup(func() { selfExecutable = orig })

	// Remove PATH from the process env for the duration of this test. (t.Setenv
	// can only set, not unset, so do it directly with a guarded restore.)
	savedPath, hadPath := os.LookupEnv("PATH")
	os.Unsetenv("PATH")
	t.Cleanup(func() {
		if hadPath {
			os.Setenv("PATH", savedPath)
		}
	})

	env := buildCleanEnv("task/foo", "abc-123")

	wantDir := filepath.Dir(fakeExe)
	var gotPath string
	pathCount := 0
	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			gotPath = strings.TrimPrefix(e, "PATH=")
			pathCount++
		}
	}
	if pathCount != 1 {
		t.Fatalf("expected exactly one PATH entry, got %d; env=%v", pathCount, env)
	}
	if gotPath != wantDir {
		t.Errorf("PATH = %q, want bare self-bin dir %q", gotPath, wantDir)
	}
}

package cmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/daemon"
	"github.com/techdufus/openkanban/internal/daemonclient"
	"github.com/techdufus/openkanban/internal/project"
)

// --- Scaffolding -----------------------------------------------------------

// daemonTestEnv pins per-test daemon socket / pid / log paths, plus a
// fake $HOME so agent.SessionPath looks for the JSONL inside the test
// sandbox. Use /tmp (not t.TempDir / $TMPDIR) for AF_UNIX path length:
// macOS caps socket paths at 104 bytes.
func daemonTestEnv(t *testing.T) (sock string, home string) {
	t.Helper()

	dir, err := os.MkdirTemp("/tmp", "okbt-")
	if err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	sock = filepath.Join(dir, "d.sock")
	pid := filepath.Join(dir, "d.pid")
	log := filepath.Join(dir, "d.log")
	t.Setenv("OPENKANBAN_DAEMON_SOCK", sock)
	t.Setenv("OPENKANBAN_DAEMON_PID", pid)
	t.Setenv("OPENKANBAN_DAEMON_LOG", log)
	// Prevent the autostart path from ever firing during tests.
	// /usr/bin/true is POSIX-portable; /bin/true does not exist on
	// macOS.
	t.Setenv("OPENKANBAN_DAEMON_BINARY", "/usr/bin/true")

	home = filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	t.Setenv("HOME", home)

	return sock, home
}

// startDaemonServer spins up an in-process daemon.Server bound to the
// test's socket. Returns the server (so tests can poke at it) and a
// cancel func that stops it. The cancel is also registered via
// t.Cleanup so callers don't have to remember it.
func startDaemonServer(t *testing.T, sock string) (*daemon.Server, func()) {
	t.Helper()

	pid := os.Getenv("OPENKANBAN_DAEMON_PID")
	if pid == "" {
		t.Fatalf("OPENKANBAN_DAEMON_PID not set (was daemonTestEnv called?)")
	}

	srv, err := daemon.NewServer(sock, pid)
	if err != nil {
		t.Fatalf("daemon.NewServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ctx)
	}()

	stop := func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(3 * time.Second):
			t.Logf("daemon server did not exit within 3s")
		}
	}
	t.Cleanup(stop)
	return srv, stop
}

// writeFakeSessionFile creates a Claude-style session JSONL under the
// fake $HOME so agent.SessionPath resolves to a real file. Returns the
// absolute path written.
func writeFakeSessionFile(t *testing.T, home, uuid string) string {
	t.Helper()
	projDir := filepath.Join(home, ".claude", "projects", "encoded-test-cwd")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir projDir: %v", err)
	}
	path := filepath.Join(projDir, uuid+".jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}
	return path
}

// spawnDaemonSessionForUUID asks the daemon to spawn a /bin/cat session
// tagged with AgentSessionUUID=uuid. /bin/cat keeps the PTY open until
// killed, which is exactly what we want for "daemon owns this UUID"
// tests.
//
// The client is held open via t.Cleanup until the test ends — closing
// it earlier would trip the daemon's last-client-disconnect shutdown
// path, which is wired up to kill all live sessions defensively.
// applySessionFlags then dials a SECOND client for its Owns probe, so
// the daemon goes from 1 client to 2 to 1, never to 0.
func spawnDaemonSessionForUUID(t *testing.T, sock, uuid string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, err := daemonclient.New(ctx)
	if err != nil {
		t.Fatalf("daemonclient.New: %v", err)
	}
	t.Cleanup(func() { c.Close() })

	resp, err := c.Spawn(ctx, daemon.SpawnReq{
		TicketID:         "T-DAEMON-OWNS",
		SessionName:      "test-session",
		Command:          "/bin/cat",
		Cols:             80,
		Rows:             24,
		Scrollback:       1000,
		AgentSessionUUID: uuid,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	return resp.SessionID
}

// holdDaemonOpen establishes a long-lived client connection so the
// daemon doesn't shut down between RPCs. Use in tests that talk to the
// daemon via short-lived clients (probeDaemonOwnership / killDaemonSession)
// and need the daemon to survive across them.
func holdDaemonOpen(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := daemonclient.New(ctx)
	if err != nil {
		t.Fatalf("holdDaemonOpen: %v", err)
	}
	t.Cleanup(func() { c.Close() })
}

// startForeignSessionHolder runs `tail -f <jsonl>` so lsof sees a
// foreign PID holding the file. Used to assert the lsof fallback path.
func startForeignSessionHolder(t *testing.T, jsonlPath string) *exec.Cmd {
	t.Helper()
	if _, err := exec.LookPath("tail"); err != nil {
		t.Skip("tail not on PATH; skipping")
	}
	cmd := exec.Command("tail", "-f", jsonlPath)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start tail: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	// Give lsof a moment to notice the fd.
	time.Sleep(100 * time.Millisecond)
	return cmd
}

// resetTicketNewFlags clears the package-level --session/--migrate/etc.
// vars between sub-tests. They're set indirectly via tests calling
// applySessionFlags after stamping the globals.
func resetTicketNewFlags() {
	ticketNewSession = ""
	ticketNewMigrate = false
	ticketNewForce = false
	ticketNewCreatedBy = ""
	ticketNewWorktree = false
	ticketNewJSON = false
	ticketNewBlockedBy = ""
	ticketNewWorktreeFrom = ""
}

// requireLsof skips when lsof isn't installed. The daemon-owns matrix
// doesn't NEED lsof at all (the daemon answers Owns first and short-
// circuits), but the daemon-down / daemon-doesn't-own paths do.
func requireLsof(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof not on PATH; skipping")
	}
}

// --- Daemon-owns matrix ----------------------------------------------------

func TestTicketNew_Migrate_DaemonOwns_WithoutForce_Refuses(t *testing.T) {
	sock, home := daemonTestEnv(t)
	startDaemonServer(t, sock)

	uuid := "11111111-1111-4111-8111-111111111111"
	writeFakeSessionFile(t, home, uuid)
	spawnDaemonSessionForUUID(t, sock, uuid)

	t.Cleanup(resetTicketNewFlags)
	resetTicketNewFlags()
	ticketNewSession = uuid
	ticketNewMigrate = true
	ticketNewForce = false

	ticket := board.NewTicket("t", "proj-x")
	err := applySessionFlags(ticket, project.NewGlobalTicketStore(nil))
	if err == nil {
		t.Fatalf("applySessionFlags: want error, got nil")
	}
	if !strings.Contains(err.Error(), "openkanbankd") {
		t.Errorf("error %q should mention openkanbankd (daemon-owned refusal)", err.Error())
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("error %q should mention --force", err.Error())
	}
	if ticket.AgentSessionID != "" {
		t.Errorf("ticket fields stamped on failure: AgentSessionID=%q", ticket.AgentSessionID)
	}
	// SessionOwned removed in task/enforce-one-to-one-session — every
	// spawn is migrate-on-resume so the flag has no readers.
}

func TestTicketNew_Migrate_DaemonOwns_WithForce_KillsViaDaemon(t *testing.T) {
	sock, home := daemonTestEnv(t)
	startDaemonServer(t, sock)

	uuid := "22222222-2222-4222-8222-222222222222"
	writeFakeSessionFile(t, home, uuid)
	daemonSessionID := spawnDaemonSessionForUUID(t, sock, uuid)

	t.Cleanup(resetTicketNewFlags)
	resetTicketNewFlags()
	ticketNewSession = uuid
	ticketNewMigrate = true
	ticketNewForce = true

	ticket := board.NewTicket("t", "proj-x")
	if err := applySessionFlags(ticket, project.NewGlobalTicketStore(nil)); err != nil {
		t.Fatalf("applySessionFlags: %v", err)
	}
	if ticket.AgentSessionID != uuid {
		t.Errorf("AgentSessionID = %q, want %q", ticket.AgentSessionID, uuid)
	}
	// SessionOwned removed — see board/board.go comment.

	// The daemon session should be gone now. Use the List RPC.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := daemonclient.New(ctx)
	if err != nil {
		t.Fatalf("daemonclient.New: %v", err)
	}
	defer c.Close()

	list, err := c.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, s := range list.Sessions {
		if s.SessionID == daemonSessionID {
			t.Errorf("session %s should have been killed via daemon", daemonSessionID)
		}
	}
	// Sanity: the daemon itself is still alive (we should be able to
	// keep talking to it).
	if got := c.ClientCount(); got < 1 {
		t.Errorf("daemon reports ClientCount=%d after migrate; want >=1", got)
	}
}

// --- Daemon-up, doesn't own ------------------------------------------------

func TestTicketNew_Migrate_DaemonUpDoesntOwn_WithoutForce_UsesLsofPath(t *testing.T) {
	requireLsof(t)

	sock, home := daemonTestEnv(t)
	startDaemonServer(t, sock)

	uuid := "33333333-3333-4333-8333-333333333333"
	jsonlPath := writeFakeSessionFile(t, home, uuid)
	// Daemon is up but knows nothing about this UUID. lsof MUST see
	// our foreign holder to make the refusal meaningful.
	startForeignSessionHolder(t, jsonlPath)

	t.Cleanup(resetTicketNewFlags)
	resetTicketNewFlags()
	ticketNewSession = uuid
	ticketNewMigrate = true
	ticketNewForce = false

	ticket := board.NewTicket("t", "proj-x")
	err := applySessionFlags(ticket, project.NewGlobalTicketStore(nil))
	if err == nil {
		t.Fatalf("applySessionFlags: want error, got nil")
	}
	// The lsof refusal message mentions "pid N", NOT "openkanbankd".
	if strings.Contains(err.Error(), "openkanbankd") {
		t.Errorf("error %q should be the lsof path (mentions pid), "+
			"not the daemon-owned path", err.Error())
	}
	if !strings.Contains(err.Error(), "pid ") {
		t.Errorf("error %q should mention pid", err.Error())
	}
}

func TestTicketNew_Migrate_DaemonUpDoesntOwn_WithForce_UsesLsofForceExit(t *testing.T) {
	requireLsof(t)

	sock, home := daemonTestEnv(t)
	startDaemonServer(t, sock)

	uuid := "44444444-4444-4444-8444-444444444444"
	jsonlPath := writeFakeSessionFile(t, home, uuid)
	holder := startForeignSessionHolder(t, jsonlPath)

	t.Cleanup(resetTicketNewFlags)
	resetTicketNewFlags()
	ticketNewSession = uuid
	ticketNewMigrate = true
	ticketNewForce = true

	ticket := board.NewTicket("t", "proj-x")
	if err := applySessionFlags(ticket, project.NewGlobalTicketStore(nil)); err != nil {
		t.Fatalf("applySessionFlags: %v", err)
	}

	// holder process must be dead now. Wait briefly for the kernel.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := holder.Process.Signal(os.Signal(nil)); err != nil {
			// process gone — perfect
			return
		}
		// On some platforms Signal(nil) returns nil even for dead procs;
		// fall back to waiting with WNOHANG via Wait.
		state, werr := holder.Process.Wait()
		if werr == nil && state.Exited() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Best-effort: the kill might not have observable effect through
	// Signal(nil) on every platform. Don't fail the test on that alone
	// — applySessionFlags returned nil, which is the load-bearing
	// assertion.
}

// --- Daemon-down ----------------------------------------------------------

func TestTicketNew_Migrate_DaemonDown_WithoutForce_UsesLsofPath(t *testing.T) {
	requireLsof(t)

	_, home := daemonTestEnv(t)
	// NOT calling startDaemonServer — the socket file does not exist.

	uuid := "55555555-5555-4555-8555-555555555555"
	jsonlPath := writeFakeSessionFile(t, home, uuid)
	startForeignSessionHolder(t, jsonlPath)

	t.Cleanup(resetTicketNewFlags)
	resetTicketNewFlags()
	ticketNewSession = uuid
	ticketNewMigrate = true
	ticketNewForce = false

	ticket := board.NewTicket("t", "proj-x")
	err := applySessionFlags(ticket, project.NewGlobalTicketStore(nil))
	if err == nil {
		t.Fatalf("applySessionFlags: want error, got nil")
	}
	if strings.Contains(err.Error(), "openkanbankd") {
		t.Errorf("error %q should be the lsof path, not daemon-owned", err.Error())
	}
	if !strings.Contains(err.Error(), "pid ") {
		t.Errorf("error %q should mention pid", err.Error())
	}
}

func TestTicketNew_Migrate_DaemonDown_WithForce_UsesLsofForceExit(t *testing.T) {
	requireLsof(t)

	_, home := daemonTestEnv(t)
	// Daemon NOT started.

	uuid := "66666666-6666-4666-8666-666666666666"
	jsonlPath := writeFakeSessionFile(t, home, uuid)
	startForeignSessionHolder(t, jsonlPath)

	t.Cleanup(resetTicketNewFlags)
	resetTicketNewFlags()
	ticketNewSession = uuid
	ticketNewMigrate = true
	ticketNewForce = true

	ticket := board.NewTicket("t", "proj-x")
	if err := applySessionFlags(ticket, project.NewGlobalTicketStore(nil)); err != nil {
		t.Fatalf("applySessionFlags: %v", err)
	}
	if ticket.AgentSessionID != uuid {
		t.Errorf("AgentSessionID = %q, want %q", ticket.AgentSessionID, uuid)
	}
	// SessionOwned removed — see board/board.go comment.
}

// Sanity test for the no-holder, daemon-down case.
func TestTicketNew_Migrate_DaemonDown_NoHolder_Succeeds(t *testing.T) {
	requireLsof(t)

	_, home := daemonTestEnv(t)

	uuid := "77777777-7777-4777-8777-777777777777"
	writeFakeSessionFile(t, home, uuid)
	// No holder.

	t.Cleanup(resetTicketNewFlags)
	resetTicketNewFlags()
	ticketNewSession = uuid
	ticketNewMigrate = true
	ticketNewForce = false

	ticket := board.NewTicket("t", "proj-x")
	if err := applySessionFlags(ticket, project.NewGlobalTicketStore(nil)); err != nil {
		t.Fatalf("applySessionFlags: %v", err)
	}
	if ticket.AgentSessionID != uuid {
		t.Errorf("AgentSessionID = %q, want %q", ticket.AgentSessionID, uuid)
	}
}

// --- Daemon-aware delete helpers -------------------------------------------
//
// Full `ticket delete` end-to-end coverage would require setting up a
// project registry, an on-disk ticket store, and a $XDG_CONFIG_HOME
// — too much for one PR. Instead we drive probeDaemonOwnership +
// killDaemonSession directly, which is the load-bearing logic the
// delete command would call into.

func TestTicketDelete_DaemonOwns_CallsKill(t *testing.T) {
	sock, home := daemonTestEnv(t)
	startDaemonServer(t, sock)

	uuid := "88888888-8888-4888-8888-888888888888"
	writeFakeSessionFile(t, home, uuid)
	daemonSessionID := spawnDaemonSessionForUUID(t, sock, uuid)

	resp, up, owns, err := probeDaemonOwnership(uuid)
	if err != nil {
		t.Fatalf("probeDaemonOwnership: %v", err)
	}
	if !up || !owns {
		t.Fatalf("probeDaemonOwnership: up=%v owns=%v want both true", up, owns)
	}
	if resp.SessionID != daemonSessionID {
		t.Errorf("SessionID = %q, want %q", resp.SessionID, daemonSessionID)
	}

	if err := killDaemonSession(resp.SessionID, 3*time.Second); err != nil {
		t.Fatalf("killDaemonSession: %v", err)
	}

	// Subsequent Owns should now be Owned=false.
	_, _, owns, err = probeDaemonOwnership(uuid)
	if err != nil {
		t.Fatalf("probeDaemonOwnership post-kill: %v", err)
	}
	if owns {
		t.Errorf("daemon still reports owns=true after Kill")
	}
}

func TestTicketDelete_DaemonUp_NotOwns_NoOp(t *testing.T) {
	sock, _ := daemonTestEnv(t)
	startDaemonServer(t, sock)
	// Keep at least one client connected for the duration of the test
	// so the daemon doesn't shut itself down between our probe and the
	// test's assertions.
	holdDaemonOpen(t)

	uuid := "99999999-9999-4999-8999-999999999999"
	// Daemon up, but never spawned anything tagged with uuid.

	resp, up, owns, err := probeDaemonOwnership(uuid)
	if err != nil {
		t.Fatalf("probeDaemonOwnership: %v", err)
	}
	if !up {
		t.Errorf("daemonUp = false, want true")
	}
	if owns {
		t.Errorf("daemonOwns = true, want false")
	}
	if resp.SessionID != "" {
		t.Errorf("SessionID = %q, want empty", resp.SessionID)
	}
}

func TestTicketDelete_DaemonDown_NoOp(t *testing.T) {
	daemonTestEnv(t)
	// NOT starting the daemon.

	uuid := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"

	_, up, owns, err := probeDaemonOwnership(uuid)
	if err != nil {
		t.Fatalf("probeDaemonOwnership: %v", err)
	}
	if up {
		t.Errorf("daemonUp = true, want false (daemon is not running)")
	}
	if owns {
		t.Errorf("daemonOwns = true, want false")
	}
}

// --- B2: end-to-end ticket delete drives the TicketDone fallback ---------
//
// These exercise the cobra RunE on ticketDeleteCmd against a real
// daemon.Server plus an on-disk project registry + ticket store. The
// load-bearing assertion is the new TicketID-keyed TicketDone layer:
// it must sweep daemon sessions for tickets whose AgentSessionID
// hasn't been back-filled yet (the freshly-spawned race window).

// setupTmpConfigDirCmd is the cmd-package equivalent of
// project.setupTmpConfigDir. Points OPENKANBAN_CONFIG_DIR at a
// per-test tmpdir and seeds the tickets/ subdir.
func setupTmpConfigDirCmd(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("OPENKANBAN_CONFIG_DIR", dir)
	if err := os.MkdirAll(filepath.Join(dir, "tickets"), 0o755); err != nil {
		t.Fatalf("mkdir tickets: %v", err)
	}
	return dir
}

// seedProjectWithTicket registers a project (via the on-disk
// projects.json) and writes a single ticket .md inside its per-project
// directory. Returns the project and ticket so callers can drive the
// CLI flags against them.
func seedProjectWithTicket(t *testing.T, projectName string, agentSessionID string) (*project.Project, *board.Ticket) {
	t.Helper()

	repoDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repoDir: %v", err)
	}

	registry, err := project.LoadRegistry()
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	proj := project.NewProject(projectName, repoDir)
	if err := registry.Add(proj); err != nil {
		t.Fatalf("registry.Add: %v", err)
	}

	store := project.NewTicketStore(proj.ID, repoDir)
	ticket := board.NewTicket("delete-me", proj.ID)
	ticket.AgentSessionID = agentSessionID
	store.Add(ticket)
	if err := store.SaveTicket(ticket); err != nil {
		t.Fatalf("SaveTicket: %v", err)
	}
	return proj, ticket
}

// runTicketDelete invokes the ticketDeleteCmd RunE with the given flags
// set, then restores the previous values. Returns the RunE error.
func runTicketDelete(t *testing.T, projectArg, ticketID string) error {
	t.Helper()
	prevProj, prevID := ticketDeleteProject, ticketDeleteID
	t.Cleanup(func() {
		ticketDeleteProject = prevProj
		ticketDeleteID = prevID
	})
	ticketDeleteProject = projectArg
	ticketDeleteID = ticketID
	return ticketDeleteCmd.RunE(ticketDeleteCmd, nil)
}

// daemonHasSessionForTicket asks the daemon how many live sessions
// match the given TicketID. Used by tests to confirm the second-layer
// TicketDone RPC actually cleaned up.
func daemonHasSessionForTicket(t *testing.T, ticketID string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := daemonclient.New(ctx)
	if err != nil {
		t.Fatalf("daemonclient.New: %v", err)
	}
	defer c.Close()
	list, err := c.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, s := range list.Sessions {
		if s.TicketID == ticketID {
			return true
		}
	}
	return false
}

// TestTicketDelete_KillsDaemonSessionWithoutBackfilledUUID is the load-
// bearing test for B2: a freshly-spawned daemon session whose owning
// ticket has AgentSessionID="" (UUID not yet back-filled) must still
// be terminated by `openkanban ticket delete`. Pre-B2, the delete
// handler only fired the UUID-keyed Owns/Kill RPC when
// t.AgentSessionID != "", so this exact race-window scenario leaked
// daemon sessions.
func TestTicketDelete_KillsDaemonSessionWithoutBackfilledUUID(t *testing.T) {
	sock, _ := daemonTestEnv(t)
	setupTmpConfigDirCmd(t)
	startDaemonServer(t, sock)

	// AgentSessionID intentionally empty: this is the back-fill race
	// window the new TicketDone layer is here to close.
	proj, ticket := seedProjectWithTicket(t, "proj-b2", "")

	// Spawn a daemon session keyed only by the TicketID. Use a fake
	// (but well-formed) UUID for AgentSessionUUID so the Spawn req
	// passes validation, but the *ticket's* AgentSessionID stays
	// empty — that's the bug scenario.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	holder, err := daemonclient.New(ctx)
	if err != nil {
		t.Fatalf("daemonclient.New: %v", err)
	}
	t.Cleanup(func() { holder.Close() })

	_, err = holder.Spawn(ctx, daemon.SpawnReq{
		TicketID:         string(ticket.ID),
		SessionName:      "test-session",
		Command:          "/bin/cat",
		Cols:             80,
		Rows:             24,
		Scrollback:       1000,
		AgentSessionUUID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	if !daemonHasSessionForTicket(t, string(ticket.ID)) {
		t.Fatalf("pre-delete: expected daemon to own a session for %s", ticket.ID)
	}

	if err := runTicketDelete(t, proj.Name, string(ticket.ID)); err != nil {
		t.Fatalf("ticket delete RunE: %v", err)
	}

	// Allow the daemon's kill goroutine a brief moment to drop the
	// session from its bookkeeping. List below short-circuits well
	// before this deadline once the session is gone.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !daemonHasSessionForTicket(t, string(ticket.ID)) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if daemonHasSessionForTicket(t, string(ticket.ID)) {
		t.Errorf("post-delete: daemon still owns a session for %s; "+
			"TicketDone fallback failed to fire", ticket.ID)
	}
}

// TestTicketDelete_BothLayersFireCleanly covers the case where the
// UUID-keyed Owns/Kill path AND the TicketID-keyed TicketDone path are
// both eligible (AgentSessionID IS set, AND a live daemon session
// matches). The first layer kills the session; the second layer is a
// no-op on miss, and that must not error.
func TestTicketDelete_BothLayersFireCleanly(t *testing.T) {
	sock, home := daemonTestEnv(t)
	setupTmpConfigDirCmd(t)
	startDaemonServer(t, sock)

	uuid := "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	writeFakeSessionFile(t, home, uuid)

	proj, ticket := seedProjectWithTicket(t, "proj-b2-both", uuid)

	// Spawn the daemon session tagged with BOTH the matching TicketID
	// and the matching UUID — the first delete layer should find it
	// via Owns/Kill.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	holder, err := daemonclient.New(ctx)
	if err != nil {
		t.Fatalf("daemonclient.New: %v", err)
	}
	t.Cleanup(func() { holder.Close() })

	_, err = holder.Spawn(ctx, daemon.SpawnReq{
		TicketID:         string(ticket.ID),
		SessionName:      "test-session",
		Command:          "/bin/cat",
		Cols:             80,
		Rows:             24,
		Scrollback:       1000,
		AgentSessionUUID: uuid,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	if err := runTicketDelete(t, proj.Name, string(ticket.ID)); err != nil {
		t.Fatalf("ticket delete RunE: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !daemonHasSessionForTicket(t, string(ticket.ID)) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if daemonHasSessionForTicket(t, string(ticket.ID)) {
		t.Errorf("post-delete: daemon still owns a session for %s", ticket.ID)
	}
}

// TestTicketDelete_DaemonDown_StillDeletesTicket asserts the
// no-autostart, daemon-down contract on the new TicketDone layer: when
// openkanbankd isn't running, `ticket delete` must complete cleanly
// (the .md unlink is authoritative; a missing daemon means there are
// no sessions to clean up anyway).
func TestTicketDelete_DaemonDown_StillDeletesTicket(t *testing.T) {
	daemonTestEnv(t)
	setupTmpConfigDirCmd(t)
	// NOT starting the daemon.

	proj, ticket := seedProjectWithTicket(t, "proj-b2-down", "")

	if err := runTicketDelete(t, proj.Name, string(ticket.ID)); err != nil {
		t.Fatalf("ticket delete RunE with daemon down: %v", err)
	}

	// .md should be gone from the project directory.
	store, err := project.LoadTicketStore(proj)
	if err != nil {
		t.Fatalf("LoadTicketStore: %v", err)
	}
	if store.Count() != 0 {
		t.Errorf("ticket store count = %d, want 0 (delete should have removed the .md)", store.Count())
	}
}

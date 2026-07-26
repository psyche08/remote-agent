package provider

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCodexDesktopSocketCandidatesPreferModernRouter(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)

	candidates := codexDesktopSocketCandidates()
	want := filepath.Join(codexHome, "ipc", "ipc.sock")
	if len(candidates) == 0 || candidates[0] != want {
		t.Fatalf("first socket candidate=%q, want modern router %q; all=%#v", firstNonEmptySlice(candidates), want, candidates)
	}
}

func TestDialCodexDesktopSocketFallsBackToListeningCandidate(t *testing.T) {
	// Keep the Unix socket path below macOS's short sun_path limit.
	dir, err := os.MkdirTemp("/tmp", "rc-codex-ipc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	stale := filepath.Join(dir, "stale.sock")
	active := filepath.Join(dir, "active.sock")
	if err := os.WriteFile(stale, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", active)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chmod(active, 0o600); err != nil {
		t.Fatal(err)
	}

	// Exercise the shared selector directly: a stale/non-socket candidate must
	// not mask a later live router.
	conn, err := dialCodexDesktopSocketCandidates([]string{stale, active}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
}

func TestDialCodexDesktopSocketRejectsPublicSocket(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "rc-codex-ipc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "public.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chmod(socket, 0o666); err != nil {
		t.Fatal(err)
	}

	if _, err := dialCodexDesktopSocketCandidates([]string{socket}, 50*time.Millisecond); err == nil {
		t.Fatal("public Desktop IPC socket was accepted")
	}
}

func TestDialCodexDesktopSocketReportsMissingRouter(t *testing.T) {
	_, err := dialCodexDesktopSocketCandidates([]string{filepath.Join(t.TempDir(), "missing.sock")}, 50*time.Millisecond)
	if err == nil || err.Error() != "Codex Desktop IPC socket not found" {
		t.Fatalf("missing socket error=%v", err)
	}
}

func TestLiveCodexDesktopSocketHandshake(t *testing.T) {
	if os.Getenv("RC_TEST_CODEX_DESKTOP_IPC") != "1" {
		t.Skip("set RC_TEST_CODEX_DESKTOP_IPC=1 for a local Desktop router check")
	}
	client := NewCodexDesktopIPCClient("", 3*time.Second, "local")
	conn, err := client.connect(3 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.initialize(conn, 3*time.Second); err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	_ = conn.Close()

	target := os.Getenv("RC_TEST_CODEX_DESKTOP_THREAD")
	if target == "" {
		return
	}
	rows := client.SnapshotLiveThreads(3 * time.Second)
	for _, row := range rows {
		t.Logf("thread=%s owner=%t status=%s",
			stringAny(row["transcript_id"]), stringAny(row["desktop_owner_client_id"]) != "", stringAny(row["status"]))
		if stringAny(row["transcript_id"]) == target && stringAny(row["desktop_owner_client_id"]) != "" {
			return
		}
	}
	t.Fatalf("Desktop router published %d live rows but no active owner for target thread", len(rows))
}

func firstNonEmptySlice(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

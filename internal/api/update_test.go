package api

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAttachAutoUpdateWorkerLogDoesNotInheritSupervisorPipe(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auto-update-worker.log")
	if err := os.WriteFile(path, []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", `echo stdout-line; echo stderr-line >&2`)
	logFile, err := attachAutoUpdateWorkerLog(cmd, dir)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Stdout != logFile || cmd.Stderr != logFile {
		t.Fatal("update worker must write only to its detached log file")
	}
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if err := logFile.Close(); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"stdout-line", "stderr-line"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("worker log missing %q: %q", want, body)
		}
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("worker log mode=%#o, want 0600", info.Mode().Perm())
	}
}

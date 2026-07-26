package autoupdate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseUpdateRemovesRetiredClaudeWrapperArtifacts(t *testing.T) {
	root := t.TempDir()
	stage := filepath.Join(root, "stage")
	binDir := filepath.Join(root, "bin")
	etcDir := filepath.Join(root, "etc")
	stateDir := filepath.Join(root, "state")
	legacyStateDir := filepath.Join(root, "legacy-state")
	for _, dir := range []string{stage, binDir, filepath.Join(etcDir, "services.d"), filepath.Join(stateDir, "data"), filepath.Join(legacyStateDir, "data")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	artifacts := []string{
		filepath.Join(stateDir, "data", "claude-wrapper-watch.enabled"),
		filepath.Join(legacyStateDir, "data", "claude-wrapper-watch.enabled"),
		filepath.Join(etcDir, "services.d", "remote-coding-claude-wrapper-watch.yaml"),
		filepath.Join(etcDir, "services.d", "remote-agent-claude-wrapper-watch.yaml"),
		filepath.Join(etcDir, "services.d", "remote-coding-watchdog.yaml"),
		filepath.Join(etcDir, "services.d", "remote-agent-watchdog.yaml"),
		filepath.Join(binDir, "remote-coding-watchdog"),
		filepath.Join(binDir, "remote-agent-watchdog"),
		filepath.Join(binDir, "claude-wrapper"),
		filepath.Join(binDir, "install-claude-wrapper"),
		filepath.Join(binDir, "watch-claude-wrapper"),
	}
	for _, path := range artifacts {
		if err := os.WriteFile(path, []byte("legacy\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	updateBody, err := os.ReadFile(filepath.Join("..", "..", "deploy", "update.sh"))
	if err != nil {
		t.Fatal(err)
	}
	update := filepath.Join(stage, "update.sh")
	writeUpdateExecutable(t, update, strings.ReplaceAll(string(updateBody), "__REMOTE_AGENT_TEAM_ID__", "TESTTEAM"))
	stagedBinary := filepath.Join(stage, "remote-agent-darwin-arm64")
	writeUpdateExecutable(t, stagedBinary, "#!/bin/sh\n[ \"$1\" = version ] && echo '{\"commit\":\"test\"}'\n")
	codesignLog := filepath.Join(root, "codesign.log")
	mockCodesign := filepath.Join(root, "codesign")
	writeUpdateExecutable(t, mockCodesign, "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$CODESIGN_LOG\"\n[ \"$1\" = -d ] && echo 'TeamIdentifier=TESTTEAM' >&2\nexit 0\n")
	target := filepath.Join(root, "remote-agent")
	cmd := exec.Command("bash", update, stagedBinary, target, "device-a")
	cmd.Env = append(os.Environ(),
		"RA_SUPERVISOR="+filepath.Join(root, "missing-supervisor"),
		"RA_STATE_DIR="+stateDir,
		"RA_LEGACY_STATE_DIR="+legacyStateDir,
		"RA_BIN_DIR="+binDir,
		"RA_ETC_DIR="+etcDir,
		"RA_PLATFORM=Darwin",
		"RA_CODESIGN="+mockCodesign,
		"CODESIGN_LOG="+codesignLog,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("update failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("updated binary missing: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), `{"commit":"test"}`) {
		t.Fatalf("staged binary did not report its version: %s", out)
	}
	signArgs, err := os.ReadFile(codesignLog)
	if err != nil {
		t.Fatal(err)
	}
	gotSignArgs := string(signArgs)
	if !strings.Contains(gotSignArgs, "--verify --strict --verbose=2 "+stagedBinary) || strings.Contains(gotSignArgs, "--force --sign -") {
		t.Fatalf("codesign args = %q, want verification without ad-hoc re-signing", gotSignArgs)
	}
	for _, path := range artifacts {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("legacy wrapper artifact still active: %s err=%v\n%s", path, err, out)
		}
	}
}

func TestPublishedUpdateScriptAcceptsEmbeddedTeamID(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "deploy", "update.sh"))
	if err != nil {
		t.Fatal(err)
	}
	published := strings.ReplaceAll(string(body), "__REMOTE_AGENT_TEAM_ID__", "TESTTEAM")
	if strings.Contains(published, `EXPECTED_TEAM_ID:-__REMOTE_AGENT_TEAM_ID__`) {
		t.Fatal("published script retained team placeholder")
	}
	if strings.Contains(published, `""|TESTTEAM)`) {
		t.Fatal("placeholder replacement corrupted the missing-team guard")
	}
}

func TestReleaseUpdateRestartsOnlyCurrentServices(t *testing.T) {
	result, err := runUpdateWithSupervisor(t, "current", "")
	if err != nil {
		t.Fatalf("update failed: %v\n%s", err, result.output)
	}
	for _, want := range []string{
		"restart remote-agent",
		"restart remote-agent-log-upload",
	} {
		if !supervisorCalled(result.supervisorLog, want) {
			t.Fatalf("supervisor log missing %q:\n%s", want, result.supervisorLog)
		}
	}
	for _, unwanted := range []string{
		"reload-config",
		"restart remote-coding",
		"restart remote-coding-log-upload",
		"start remote-agent",
		"start remote-coding",
	} {
		if supervisorCalled(result.supervisorLog, unwanted) {
			t.Fatalf("supervisor log unexpectedly contains %q:\n%s", unwanted, result.supervisorLog)
		}
	}
}

func TestReleaseUpdateFallsBackToLegacyServices(t *testing.T) {
	result, err := runUpdateWithSupervisor(t, "legacy", "")
	if err != nil {
		t.Fatalf("update failed: %v\n%s", err, result.output)
	}
	for _, want := range []string{
		"restart remote-coding",
		"restart remote-coding-log-upload",
	} {
		if !supervisorCalled(result.supervisorLog, want) {
			t.Fatalf("supervisor log missing %q:\n%s", want, result.supervisorLog)
		}
	}
	for _, unwanted := range []string{
		"reload-config",
		"restart remote-agent",
		"restart remote-agent-log-upload",
		"start remote-agent",
		"start remote-coding",
	} {
		if supervisorCalled(result.supervisorLog, unwanted) {
			t.Fatalf("supervisor log unexpectedly contains %q:\n%s", unwanted, result.supervisorLog)
		}
	}
	if !strings.Contains(result.output, "supervisor restarted legacy remote-coding") {
		t.Fatalf("update did not report the legacy fallback:\n%s", result.output)
	}
}

func TestReleaseUpdateFailsWhenConfiguredCurrentServiceCannotRestart(t *testing.T) {
	result, err := runUpdateWithSupervisor(t, "current", "remote-agent")
	if err == nil {
		t.Fatalf("update succeeded although configured service did not restart:\n%s", result.output)
	}
	if !strings.Contains(result.output, "failed to restart configured remote-agent service") {
		t.Fatalf("update did not report current-service restart failure:\n%s", result.output)
	}
	if supervisorCalled(result.supervisorLog, "restart remote-coding") {
		t.Fatalf("current-service failure fell through to legacy identity:\n%s", result.supervisorLog)
	}
	for _, unwanted := range []string{"reload-config", "start remote-agent", "start remote-coding"} {
		if supervisorCalled(result.supervisorLog, unwanted) {
			t.Fatalf("failed update used %q:\n%s", unwanted, result.supervisorLog)
		}
	}
}

func TestReleaseUpdateFailsWithoutConfiguredAgentService(t *testing.T) {
	result, err := runUpdateWithSupervisor(t, "none", "")
	if err == nil {
		t.Fatalf("update succeeded without a configured service:\n%s", result.output)
	}
	if !strings.Contains(result.output, "no configured remote-agent or legacy remote-coding service drop-in") {
		t.Fatalf("update did not report missing service identity:\n%s", result.output)
	}
	for _, unwanted := range []string{
		"reload-config",
		"restart remote-agent",
		"restart remote-coding",
		"start remote-agent",
		"start remote-coding",
	} {
		if supervisorCalled(result.supervisorLog, unwanted) {
			t.Fatalf("update called %q without a configured service:\n%s", unwanted, result.supervisorLog)
		}
	}
}

type updateSupervisorResult struct {
	output        string
	supervisorLog string
}

func runUpdateWithSupervisor(t *testing.T, serviceIdentity string, failRestart string) (updateSupervisorResult, error) {
	t.Helper()
	root := t.TempDir()
	stage := filepath.Join(root, "stage")
	if err := os.MkdirAll(stage, 0o755); err != nil {
		t.Fatal(err)
	}
	updateBody, err := os.ReadFile(filepath.Join("..", "..", "deploy", "update.sh"))
	if err != nil {
		t.Fatal(err)
	}
	update := filepath.Join(stage, "update.sh")
	writeUpdateExecutable(t, update, strings.ReplaceAll(string(updateBody), "__REMOTE_AGENT_TEAM_ID__", "TESTTEAM"))
	stagedBinary := filepath.Join(stage, "remote-agent-darwin-arm64")
	writeUpdateExecutable(t, stagedBinary, "#!/bin/sh\n[ \"$1\" = version ] && echo '{\"commit\":\"test\"}'\n")
	supervisorLog := filepath.Join(root, "supervisor.log")
	supervisor := filepath.Join(root, "private-services")
	writeUpdateExecutable(t, supervisor, `#!/bin/sh
printf '%s\n' "$*" >>"$SUPERVISOR_LOG"
if [ "$1 $2" = "restart $FAIL_RESTART" ]; then
  exit 1
fi
exit 0
`)
	etcDir := filepath.Join(root, "etc")
	dropinDir := filepath.Join(etcDir, "services.d")
	if err := os.MkdirAll(dropinDir, 0o755); err != nil {
		t.Fatal(err)
	}
	switch serviceIdentity {
	case "current":
		if err := os.WriteFile(filepath.Join(dropinDir, "remote-agent.yaml"), []byte("services: {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	case "legacy":
		if err := os.WriteFile(filepath.Join(dropinDir, "remote-coding.yaml"), []byte("services: {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	case "none":
	default:
		t.Fatalf("unknown service identity %q", serviceIdentity)
	}
	cmd := exec.Command("bash", update, stagedBinary, filepath.Join(root, "remote-agent"), "device-a")
	cmd.Env = append(os.Environ(),
		"RA_SUPERVISOR="+supervisor,
		"RA_STATE_DIR="+filepath.Join(root, "state"),
		"RA_LEGACY_STATE_DIR="+filepath.Join(root, "legacy-state"),
		"RA_BIN_DIR="+filepath.Join(root, "bin"),
		"RA_ETC_DIR="+etcDir,
		"RA_PLATFORM=Linux",
		"SUPERVISOR_LOG="+supervisorLog,
		"FAIL_RESTART="+failRestart,
	)
	out, err := cmd.CombinedOutput()
	logBody, readErr := os.ReadFile(supervisorLog)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	return updateSupervisorResult{
		output:        string(out),
		supervisorLog: string(logBody),
	}, err
}

func supervisorCalled(logText string, call string) bool {
	for _, line := range strings.Split(strings.TrimSpace(logText), "\n") {
		if line == call {
			return true
		}
	}
	return false
}

func writeUpdateExecutable(t *testing.T, path string, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

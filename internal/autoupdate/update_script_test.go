package autoupdate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublishedUpdateScriptAcceptsEmbeddedTeamID(t *testing.T) {
	body := readUpdateScript(t)
	published := strings.ReplaceAll(body, "__AGENTHALO_TEAM_ID__", "TESTTEAM")
	if strings.Contains(published, `EXPECTED_TEAM_ID:-__AGENTHALO_TEAM_ID__`) {
		t.Fatal("published script retained team placeholder")
	}
	if strings.Contains(published, `""|TESTTEAM)`) {
		t.Fatal("placeholder replacement corrupted the missing-team guard")
	}
}

func TestReleaseUpdateVerifiesSignedAgentHaloBinary(t *testing.T) {
	root := t.TempDir()
	stage := filepath.Join(root, "stage")
	if err := os.MkdirAll(stage, 0o755); err != nil {
		t.Fatal(err)
	}
	update := filepath.Join(stage, "update.sh")
	writeUpdateExecutable(t, update, strings.ReplaceAll(readUpdateScript(t), "__AGENTHALO_TEAM_ID__", "TESTTEAM"))
	stagedBinary := filepath.Join(stage, "agenthalo-darwin-arm64")
	writeUpdateExecutable(t, stagedBinary, "#!/bin/sh\nif [ \"$1\" = version ]; then echo '{\"commit\":\"test\"}'; fi\nexit 0\n")
	codesignLog := filepath.Join(root, "codesign.log")
	mockCodesign := filepath.Join(root, "codesign")
	writeUpdateExecutable(t, mockCodesign, "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$CODESIGN_LOG\"\n[ \"$1\" = -d ] && { echo 'Identifier=dev.linsheng.agenthalo' >&2; echo 'TeamIdentifier=TESTTEAM' >&2; echo 'CodeDirectory v=20500 flags=0x10000(runtime)' >&2; }\nexit 0\n")
	target := filepath.Join(root, "agenthalo")
	cmd := exec.Command("bash", update, stagedBinary, target, "device-a")
	cmd.Env = append(os.Environ(),
		"AGENTHALO_SUPERVISOR="+filepath.Join(root, "missing-supervisor"),
		"AGENTHALO_STATE_DIR="+filepath.Join(root, "state"),
		"AGENTHALO_ETC_DIR="+filepath.Join(root, "etc"),
		"AGENTHALO_PLATFORM=Darwin",
		"AGENTHALO_CODESIGN="+mockCodesign,
		"CODESIGN_LOG="+codesignLog,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("update failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("updated binary missing: %v\n%s", err, out)
	}
	signArgs, err := os.ReadFile(codesignLog)
	if err != nil {
		t.Fatal(err)
	}
	got := string(signArgs)
	if !strings.Contains(got, "--verify --strict --verbose=2 "+stagedBinary) || strings.Contains(got, "--force --sign -") {
		t.Fatalf("codesign args=%q, want verification without ad-hoc signing", got)
	}
}

func TestReleaseUpdateRejectsWrongIdentifierTeamAndUnsignedBinary(t *testing.T) {
	for _, tc := range []struct {
		name       string
		identifier string
		team       string
		verifyOK   bool
		want       string
	}{
		{name: "wrong identifier", identifier: "com.example.impostor", team: "TESTTEAM", verifyOK: true, want: "signing identifier mismatch"},
		{name: "wrong team", identifier: "dev.linsheng.agenthalo", team: "WRONGTEAM", verifyOK: true, want: "Developer ID team mismatch"},
		{name: "unsigned", identifier: "dev.linsheng.agenthalo", team: "TESTTEAM", verifyOK: false, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			update := filepath.Join(root, "update.sh")
			writeUpdateExecutable(t, update, strings.ReplaceAll(readUpdateScript(t), "__AGENTHALO_TEAM_ID__", "TESTTEAM"))
			staged := filepath.Join(root, "agenthalo-darwin-arm64")
			writeUpdateExecutable(t, staged, "#!/bin/sh\nexit 0\n")
			codesign := filepath.Join(root, "codesign")
			verifyExit := "0"
			if !tc.verifyOK {
				verifyExit = "1"
			}
			writeUpdateExecutable(t, codesign, "#!/bin/sh\nif [ \"$1\" = --verify ]; then exit "+verifyExit+"; fi\nif [ \"$1\" = -d ]; then\n  echo 'Identifier="+tc.identifier+"' >&2\n  echo 'TeamIdentifier="+tc.team+"' >&2\n  echo 'CodeDirectory v=20500 flags=0x10000(runtime)' >&2\nfi\nexit 0\n")
			target := filepath.Join(root, "installed-agenthalo")
			cmd := exec.Command("bash", update, staged, target, "device-a")
			cmd.Env = append(os.Environ(),
				"AGENTHALO_PLATFORM=Darwin",
				"AGENTHALO_CODESIGN="+codesign,
				"AGENTHALO_STATE_DIR="+filepath.Join(root, "state"),
				"AGENTHALO_ETC_DIR="+filepath.Join(root, "etc"),
			)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("update accepted invalid signature metadata: %s", out)
			}
			if tc.want != "" && !strings.Contains(string(out), tc.want) {
				t.Fatalf("output=%s, want %q", out, tc.want)
			}
			if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
				t.Fatalf("invalid binary reached target: stat err=%v", statErr)
			}
		})
	}
}

func TestReleaseUpdateRestartsOnlyAgentHaloServices(t *testing.T) {
	result, err := runUpdateWithSupervisor(t, true, "")
	if err != nil {
		t.Fatalf("update failed: %v\n%s", err, result.output)
	}
	for _, want := range []string{"restart agenthalo", "restart agenthalo-log-upload"} {
		if !supervisorCalled(result.supervisorLog, want) {
			t.Fatalf("supervisor log missing %q:\n%s", want, result.supervisorLog)
		}
	}
	for _, forbidden := range []string{"remote-agent", "remote-coding", "reload-config"} {
		if strings.Contains(result.supervisorLog, forbidden) {
			t.Fatalf("fresh updater called old identity %q:\n%s", forbidden, result.supervisorLog)
		}
	}
}

func TestReleaseUpdateRefreshesClaudeDesktopObserverHooks(t *testing.T) {
	body := readUpdateScript(t)
	for _, required := range []string{
		`CLAUDE_SETTINGS="${AGENTHALO_CLAUDE_SETTINGS:-$HOME/.claude/settings.json}"`,
		`TURNSTATE_DIR="${AGENTHALO_TURNSTATE_DIR:-$HOME/.claude/agenthalo-turnstate}"`,
		`INTERACTION_DIR="${AGENTHALO_INTERACTION_DIR:-$HOME/.claude/agenthalo-interactions}"`,
		`install -d -m 0700 "$TURNSTATE_DIR" "$INTERACTION_DIR"`,
		`"$TARGET" hook install-turnstate --settings "$CLAUDE_SETTINGS" --binary "$TARGET"`,
		`--turnstate-dir "$TURNSTATE_DIR" --interaction-dir "$INTERACTION_DIR"`,
	} {
		if !strings.Contains(body, required) {
			t.Fatalf("update script does not refresh Claude observer hooks: missing %q", required)
		}
	}
}

func TestReleaseUpdateFailsWhenAgentHaloCannotRestart(t *testing.T) {
	result, err := runUpdateWithSupervisor(t, true, "agenthalo")
	if err == nil {
		t.Fatalf("update succeeded although AgentHalo did not restart:\n%s", result.output)
	}
	if !strings.Contains(result.output, "failed to restart configured AgentHalo service") {
		t.Fatalf("update did not report restart failure:\n%s", result.output)
	}
}

func TestReleaseUpdateFailsWithoutAgentHaloService(t *testing.T) {
	result, err := runUpdateWithSupervisor(t, false, "")
	if err == nil {
		t.Fatalf("update succeeded without an AgentHalo service:\n%s", result.output)
	}
	if !strings.Contains(result.output, "no configured AgentHalo service drop-in") {
		t.Fatalf("update did not report missing service:\n%s", result.output)
	}
	if strings.TrimSpace(result.supervisorLog) != "" {
		t.Fatalf("updater called supervisor without a service:\n%s", result.supervisorLog)
	}
}

type updateSupervisorResult struct {
	output        string
	supervisorLog string
}

func runUpdateWithSupervisor(t *testing.T, configured bool, failRestart string) (updateSupervisorResult, error) {
	t.Helper()
	root := t.TempDir()
	stage := filepath.Join(root, "stage")
	if err := os.MkdirAll(stage, 0o755); err != nil {
		t.Fatal(err)
	}
	update := filepath.Join(stage, "update.sh")
	writeUpdateExecutable(t, update, strings.ReplaceAll(readUpdateScript(t), "__AGENTHALO_TEAM_ID__", "TESTTEAM"))
	stagedBinary := filepath.Join(stage, "agenthalo-linux-arm64")
	writeUpdateExecutable(t, stagedBinary, "#!/bin/sh\nif [ \"$1\" = version ]; then echo '{\"commit\":\"test\"}'; fi\nexit 0\n")
	supervisorLog := filepath.Join(root, "supervisor.log")
	supervisor := filepath.Join(root, "private-services")
	writeUpdateExecutable(t, supervisor, `#!/bin/sh
printf '%s\n' "$*" >>"$SUPERVISOR_LOG"
if [ "$1 $2" = "restart $FAIL_RESTART" ]; then exit 1; fi
exit 0
`)
	etcDir := filepath.Join(root, "etc")
	dropinDir := filepath.Join(etcDir, "services.d")
	if err := os.MkdirAll(dropinDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if configured {
		if err := os.WriteFile(filepath.Join(dropinDir, "agenthalo.yaml"), []byte("services: {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("bash", update, stagedBinary, filepath.Join(root, "agenthalo"), "device-a")
	cmd.Env = append(os.Environ(),
		"AGENTHALO_SUPERVISOR="+supervisor,
		"AGENTHALO_STATE_DIR="+filepath.Join(root, "state"),
		"AGENTHALO_ETC_DIR="+etcDir,
		"AGENTHALO_PLATFORM=Linux",
		"SUPERVISOR_LOG="+supervisorLog,
		"FAIL_RESTART="+failRestart,
	)
	out, err := cmd.CombinedOutput()
	logBody, readErr := os.ReadFile(supervisorLog)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	return updateSupervisorResult{output: string(out), supervisorLog: string(logBody)}, err
}

func readUpdateScript(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "deploy", "update.sh"))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
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

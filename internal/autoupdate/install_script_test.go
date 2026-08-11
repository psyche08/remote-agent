package autoupdate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallerUsesFreshAgentHaloRuntimeIdentity(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "deploy", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(body)
	for _, required := range []string{
		`-o bin/agenthalo ./cmd/agenthalo`,
		`STATE_DIR="${AGENTHALO_STATE_DIR:-/opt/private-tunnel/state/agenthalo}"`,
		`LIBEXEC_DIR="${AGENTHALO_LIBEXEC_DIR:-/opt/private-tunnel/libexec/agenthalo}"`,
		`RUNTIME_BIN="$LIBEXEC_DIR/agenthalo"`,
		`sudo -n install -d -o "$(id -un)" -g staff -m 0755 "$dir"`,
		`install -m 0755 "$REPO_AGENTHALO/bin/agenthalo" "$RUNTIME_BIN.new"`,
		`echo "  agenthalo:"`,
		`UPDATE_RELAY_URL="${AGENTHALO_UPDATE_RELAY_URL:-}"`,
		`UPDATE_CERT_DIR="${AGENTHALO_UPDATE_CERT_DIR:-}"`,
		`--update-relay-url) UPDATE_RELAY_URL="$2"; shift 2 ;;`,
		`--update-cert-dir) UPDATE_CERT_DIR="$2"; shift 2 ;;`,
		`AGENTHALO_UPDATE_RELAY_URL: `,
		`AGENTHALO_UPDATE_CERT_DIR: `,
		`echo "  agenthalo-log-upload:"`,
		`DROPIN="$ETC_DIR/services.d/agenthalo.yaml"`,
		`"$SUPERVISOR" start agenthalo`,
		`$HOME/.claude/agenthalo-turnstate`,
		`$HOME/.claude/agenthalo-interactions`,
		`install -d -m 0700 "$AGENTHALO_TURNSTATE_DIR" "$AGENTHALO_INTERACTION_DIR"`,
		`--interaction-dir "$AGENTHALO_INTERACTION_DIR"`,
		`SOURCE_CFG="${AGENTHALO_CONFIG_SOURCE:-}"`,
		`"signing_key_path" in locked_use`,
		`refusing non-AgentHalo config`,
		`EXPECTED_IDENTIFIER="dev.linsheng.agenthalo"`,
		`AGENTHALO_EXPECTED_TEAM_ID is required for an AgentHalo macOS install`,
		`AGENTHALO_SIGN_IDENTITY must be a non-ad-hoc signing identity`,
		`"$CODESIGN" --force --identifier "$EXPECTED_IDENTIFIER" --options runtime --timestamp`,
		`verify_agenthalo_signature "$REPO_AGENTHALO/bin/agenthalo"`,
		`[ "$identifier" = "$EXPECTED_IDENTIFIER" ]`,
		`[ "$team" = "$EXPECTED_TEAM_ID" ]`,
		`flags=.*(runtime)`,
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("installer missing AgentHalo invariant %q", required)
		}
	}
	for _, forbidden := range []string{
		`services.d/remote-agent`, `services.d/remote-coding`, `/state/remote-agent`,
		`/state/remote-coding`, `/libexec/remote-agent`, `echo "  remote-agent:`,
		`echo "  remote-coding:`, `RA_`, `RC_`,
		`SOURCE_CFG="${AGENTHALO_CONFIG_SOURCE:-$REPO_AGENTHALO/config.json}"`,
		`--force --sign -`,
	} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("fresh installer still contains old runtime identity %q", forbidden)
		}
	}
	cfgAt := strings.Index(script, `CFG="${AGENTHALO_CONFIG:-$ETC_DIR/agenthalo/config.json}"`)
	desktopAt := strings.Index(script, `DESKTOP_HELPER="$("$RUNTIME_BIN" desktop install 2>/dev/null)"`)
	if cfgAt < 0 || desktopAt < 0 || cfgAt >= desktopAt {
		t.Fatalf("installer must define and prepare CFG before installing its LaunchAgent (cfg=%d desktop=%d)", cfgAt, desktopAt)
	}
	if got := strings.Count(script, `"$RUNTIME_BIN" desktop install`); got != 1 {
		t.Fatalf("installer invokes desktop install %d times, want exactly once", got)
	}
	for _, safeReload := range []string{
		`DESKTOP_TARGET="gui/$(id -u)/dev.linsheng.agenthalo.desktop"`,
		`launchctl print "$DESKTOP_TARGET"`,
		`deferring its safe restart to AgentHalo startup`,
	} {
		if !strings.Contains(script, safeReload) {
			t.Fatalf("installer can replace a loaded desktop helper without the startup safety barrier: missing %q", safeReload)
		}
	}
	if strings.Contains(script, `could not register the desktop LaunchAgent; run mac/launchagent/install.sh by hand`) {
		t.Fatal("fresh installer still converts a failed desktop startup into success")
	}
	if !strings.Contains(script, `AgentHalo desktop LaunchAgent failed its startup readiness check`) {
		t.Fatal("fresh installer does not propagate a failed desktop startup")
	}
}

func TestPublisherUsesOnlyAgentHaloSigningIdentifier(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "deploy", "publish-release.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(body)
	if !strings.Contains(script, `codesign --force --identifier dev.linsheng.agenthalo --options runtime`) {
		t.Fatal("publisher does not pin the AgentHalo main signing identifier")
	}
	if strings.Contains(script, "AGENTHALO_AGENT_SIGNING_IDENTIFIER") ||
		strings.Contains(script, "codesign --force --identifier com.psyche08.remote-agent") {
		t.Fatal("publisher still exposes a legacy or configurable main signing identity")
	}
	if !strings.Contains(script, `cp "$DESKTOP_ASSET" "$OUT/notary-payload/agenthalo-desktop"`) {
		t.Fatal("publisher does not submit the independently executed desktop helper for notarization")
	}
	if !strings.Contains(script, `status --porcelain --untracked-files=normal`) {
		t.Fatal("publisher dirty-tree gate does not include untracked release inputs")
	}
}

func TestStandaloneLaunchAgentInstallerRefusesLiveReplacement(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "mac", "launchagent", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(body)
	guardAt := strings.LastIndex(script, `if launchctl print "$TARGET/$LABEL" >/dev/null 2>&1; then`)
	writeAt := strings.Index(script, `cat > "$PLIST" <<PLISTEOF`)
	if guardAt < 0 || writeAt < 0 || guardAt >= writeAt {
		t.Fatalf("loaded-job safety guard must run before rewriting the LaunchAgent plist (guard=%d write=%d)", guardAt, writeAt)
	}
	if !strings.Contains(script, "prepare_restart") {
		t.Fatal("live-replacement refusal does not direct operators to the atomic restart path")
	}
	if !strings.Contains(script, `LABEL="dev.linsheng.agenthalo.desktop"`) {
		t.Fatal("standalone LaunchAgent installer does not use the AgentHalo label")
	}
	for _, required := range []string{
		`SUPPORT_DIR="$HOME/Library/Application Support/AgentHalo"`,
		`HELPER="$SUPPORT_DIR/bin/agenthalo-desktop"`,
		`SOCKET="$SUPPORT_DIR/desktop.sock"`,
		`LOG="$HOME/Library/Logs/AgentHalo/agenthalo-desktop.log"`,
		`rm -f "$SOCKET"`,
		`[ -S "$SOCKET" ]`,
		`did not create its socket after launchd bootstrap`,
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("standalone LaunchAgent installer missing fresh AgentHalo runtime path %q", required)
		}
	}
	if strings.Contains(script, `com.psyche08.remote-agent-desktop`) ||
		strings.Contains(script, `LEGACY_LABEL`) ||
		strings.Contains(script, `Application Support/remote-agent`) ||
		strings.Contains(script, `remote-agent-desktop`) {
		t.Fatal("standalone LaunchAgent installer retains an old product label or runtime path")
	}
	// Even explicit uninstall must first pass the signed prepare_restart path;
	// blindly booting out either label can remove a fail-closed shield.
	if strings.Contains(script, `launchctl bootout "$TARGET/`) {
		t.Fatal("standalone LaunchAgent script still has an unsafe bootout path")
	}
}

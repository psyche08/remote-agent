package autoupdate

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func readAuthorizationPluginFile(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "mac", "authorization-plugin", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(body)
}

func TestAuthorizationPluginInstallerPinsAgentHaloSigningIdentity(t *testing.T) {
	script := readAuthorizationPluginFile(t, "install.sh")
	for _, required := range []string{
		`BUNDLE_NAME="AgentHaloLockedUse.bundle"`,
		`PLUGIN_IDENTIFIER="dev.linsheng.agenthalo.locked-use.plugin"`,
		`MECHANISM="AgentHaloLockedUse:invoke,privileged"`,
		`RULE_NAME="dev.linsheng.agenthalo.locked-use"`,
		`STATE_DIR="/Library/Application Support/AgentHalo/locked-use"`,
		`BUILD_DIR="${AGENTHALO_PLUGIN_BUILD_DIR:-$HERE/build}"`,
		`DEVICE_ID="${AGENTHALO_DEVICE_ID:-}"`,
		`AGENT_USER="${AGENTHALO_AGENT_USER:-${SUDO_USER:-}}"`,
		`EXPECTED_TEAM_ID="${AGENTHALO_EXPECTED_TEAM_ID:-${AGENTHALO_SIGN_TEAM_ID:-}}"`,
		`[ "${AGENTHALO_LOCKED_USE_ACK:-}" != "1" ]`,
		`[ "$ACTUAL_IDENTIFIER" != "$PLUGIN_IDENTIFIER" ]`,
		`[ -z "$EXPECTED_TEAM_ID" ]`,
		`[ -z "$ACTUAL_TEAM_ID" ] || [ "$ACTUAL_TEAM_ID" != "$EXPECTED_TEAM_ID" ]`,
		`[ "$SIGNATURE_KIND" = "adhoc" ]`,
		`codesign --verify --strict "$PLUGIN_DIR/$BUNDLE_NAME"`,
		`[ "$INSTALLED_IDENTIFIER" != "$PLUGIN_IDENTIFIER" ]`,
		`[ "$INSTALLED_TEAM_ID" != "$EXPECTED_TEAM_ID" ]`,
		`<key>shared</key><false/>`,
		`<key>timeout</key><integer>0</integer>`,
		`chmod 0600 "$STATE_DIR/grant.json"`,
		`chmod +a "user:$AGENT_USER allow write" "$STATE_DIR/grant.json"`,
		`for proof in receipt.pending receipt receipt.complete; do`,
		`not migrate or remove another product's authorization records`,
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("authorization plug-in installer missing AgentHalo gate %q", required)
		}
	}
}

func TestAuthorizationPluginInstallerKeepsExactlyOneAgentHaloBranch(t *testing.T) {
	script := readAuthorizationPluginFile(t, "install.sh")
	for _, required := range []string{
		`retained = [value for value in rules if value != rule_name]`,
		`retained = [value for value in mechanisms if value != mechanism]`,
		`rules.count(rule_name) == 1`,
		`rules.index(rule_name) < rules.index("use-login-window-ui")`,
		`mechanisms.count(mechanism) == 1`,
		`echo "==> verified: exactly one AgentHalo branch is present in $RIGHT"`,
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("authorization plug-in installer missing exact-branch invariant %q", required)
		}
	}
}

func TestAuthorizationPluginReadbackAcceptsOnlyCanonicalZeroTimeout(t *testing.T) {
	files := map[string]string{
		"install.sh": readAuthorizationPluginFile(t, "install.sh"),
	}
	preflight, err := os.ReadFile(filepath.Join("..", "..", "mac", "preflight.sh"))
	if err != nil {
		t.Fatalf("read preflight.sh: %v", err)
	}
	files["preflight.sh"] = string(preflight)

	for name, script := range files {
		for _, required := range []string{
			`timeout = rule.get("timeout")`,
			`timeout_ok = timeout is None or (type(timeout) is int and timeout == 0)`,
			`rule.get("shared") is False`,
			`type(rule.get("tries")) is int`,
			`rule.get("tries") == 1`,
		} {
			if !strings.Contains(script, required) {
				t.Fatalf("%s missing canonical timeout/single-use check %q", name, required)
			}
		}
		if strings.Contains(script, `int(rule.get("timeout", -1)) == 0`) {
			t.Fatalf("%s rejects authd's canonical omission of timeout=0", name)
		}
	}
}

func TestAuthorizationPluginBuildUsesAgentHaloDeploymentIdentity(t *testing.T) {
	checks := map[string][]string{
		"Info.plist": {
			`<string>AgentHaloLockedUse</string>`,
			`<string>dev.linsheng.agenthalo.locked-use.plugin</string>`,
		},
		"build.sh": {
			`BUNDLE_NAME="AgentHaloLockedUse.bundle"`,
			`BUILD_DIR="${AGENTHALO_PLUGIN_BUILD_DIR:-$HERE/build}"`,
			`SIGNING_IDENTIFIER="dev.linsheng.agenthalo.locked-use.plugin"`,
			`IDENTITY="${AGENTHALO_PLUGIN_SIGN_IDENTITY:-}"`,
			`-o "$BUNDLE/Contents/MacOS/AgentHaloLockedUse"`,
			`"$HERE/AgentHaloLockedUse.m"`,
		},
		"interop_check.m": {
			`#include "AgentHaloLockedUse.m"`,
			`AgentHaloClaimsMatchAuthorizationUser`,
			`AgentHaloVerifySignature`,
		},
		"AgentHaloLockedUse.m": {
			`AuthorizationPluginCreate`,
			`#define AGENTHALO_LOCKED_USE_DIR`,
			`AgentHaloGrantAllowsUnlock`,
		},
	}
	for name, requiredValues := range checks {
		text := readAuthorizationPluginFile(t, name)
		for _, required := range requiredValues {
			if !strings.Contains(text, required) {
				t.Fatalf("%s missing AgentHalo identity %q", name, required)
			}
		}
	}
}

func TestAuthorizationPluginDoesNotRetainRemoteAgentAliases(t *testing.T) {
	legacyPrefix := regexp.MustCompile(`\bRA(?:_|[A-Z])[A-Za-z0-9_]*`)
	for _, name := range []string{
		"AgentHaloLockedUse.m",
		"build.sh",
		"install.sh",
		"interop_check.m",
		"uninstall.sh",
	} {
		text := readAuthorizationPluginFile(t, name)
		if match := legacyPrefix.FindString(text); match != "" {
			t.Fatalf("%s retains legacy project prefix %q", name, match)
		}
	}
}

func TestAuthorizationPluginUninstallerSurgicallyEditsCurrentRight(t *testing.T) {
	script := readAuthorizationPluginFile(t, "uninstall.sh")
	if strings.Contains(script, `security authorizationdb write "$RIGHT" < "$BACKUP"`) {
		t.Fatal("uninstaller restores a stale full authorization-right backup")
	}
	for _, required := range []string{
		`security authorizationdb read "$RIGHT" > "$TMP"`,
		`RULE_NAME="dev.linsheng.agenthalo.locked-use"`,
		`MECHANISM="AgentHaloLockedUse:invoke,privileged"`,
		`right["rule"] = [value for value in rules if value != rule_name]`,
		`retained = [value for value in mechanisms if value != mechanism]`,
		`security authorizationdb write "$RIGHT" < "$TMP.new"`,
		`refusing destructive cleanup`,
		`remove_rule_definition "$RULE_NAME"`,
		`rm -rf "${PLUGIN_DIR:?}/$BUNDLE_NAME"`,
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("authorization plug-in uninstaller missing surgical-removal invariant %q", required)
		}
	}
	verified := strings.Index(script, `echo "==> unregistered from the current $RIGHT"`)
	removeRule := strings.Index(script, `remove_rule_definition "$RULE_NAME"`)
	removeBundle := strings.Index(script, `rm -rf "${PLUGIN_DIR:?}/$BUNDLE_NAME"`)
	removeState := strings.Index(script, `rm -rf "$STATE_DIR"`)
	if verified < 0 || removeRule <= verified || removeBundle <= removeRule || removeState <= removeBundle {
		t.Fatal("uninstaller must verify the live right and remove its rule definition before bundle/state cleanup")
	}
}

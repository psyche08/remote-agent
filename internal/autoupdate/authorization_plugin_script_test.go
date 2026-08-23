package autoupdate

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
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
		`right["rule"] = [rule_name] + retained`,
		`retained = [value for value in mechanisms if value != mechanism]`,
		`rules.count(rule_name) == 1`,
		`rules[0] == rule_name`,
		`rules == expected`,
		`mechanisms.count(mechanism) == 1`,
		`mechanisms[0] == mechanism`,
		`mechanisms == expected`,
		`echo "==> verified: exactly one index-zero AgentHalo branch is present in $RIGHT"`,
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("authorization plug-in installer missing exact-branch invariant %q", required)
		}
	}
}

const authorizationRuleName = "dev.linsheng.agenthalo.locked-use"
const authorizationMechanism = "AgentHaloLockedUse:invoke,privileged"

func installerPythonBlock(t *testing.T, script, invocation string) string {
	t.Helper()
	start := strings.Index(script, invocation)
	if start < 0 {
		t.Fatalf("installer Python invocation not found: %s", invocation)
	}
	start += len(invocation)
	end := strings.Index(script[start:], "\nPYEOF")
	if end < 0 {
		t.Fatalf("installer Python block has no terminator: %s", invocation)
	}
	return script[start : start+end]
}

func writeAuthorizationRightFixture(t *testing.T, path, class, key string, values []string) {
	t.Helper()
	fixture := map[string]any{"class": class, key: values}
	if class == "rule" {
		fixture["k-of-n"] = 1
	}
	writeAuthorizationFixture(t, path, fixture)
}

func writeAuthorizationFixture(t *testing.T, path string, fixture map[string]any) {
	t.Helper()
	body, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	python := `import json, plistlib, sys
fixture = json.loads(sys.argv[2])
with open(sys.argv[1], "wb") as f:
    plistlib.dump(fixture, f)
`
	command := exec.Command("python3", "-", path, string(body))
	command.Stdin = strings.NewReader(python)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("write authorization fixture: %v: %s", err, output)
	}
}

func authorizationFixtureHasExactIntegerKOfN(t *testing.T, path string, want int) bool {
	t.Helper()
	python := `import plistlib, sys
with open(sys.argv[1], "rb") as f:
    value = plistlib.load(f).get("k-of-n")
raise SystemExit(0 if type(value) is int and value == int(sys.argv[2]) else 1)
`
	command := exec.Command("python3", "-", path, strconv.Itoa(want))
	command.Stdin = strings.NewReader(python)
	return command.Run() == nil
}

func readAuthorizationList(t *testing.T, path, key string) []string {
	t.Helper()
	python := `import json, plistlib, sys
with open(sys.argv[1], "rb") as f:
    print(json.dumps(plistlib.load(f)[sys.argv[2]]))
`
	command := exec.Command("python3", "-", path, key)
	command.Stdin = strings.NewReader(python)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("read transformed authorization fixture: %v: %s", err, output)
	}
	var values []string
	if err := json.Unmarshal(output, &values); err != nil {
		t.Fatalf("decode transformed authorization fixture: %v: %s", err, output)
	}
	return values
}

func runAuthorizationTransformer(
	t *testing.T, python, source, destination string,
) error {
	t.Helper()
	command := exec.Command(
		"python3", "-", source, destination,
		authorizationRuleName, authorizationMechanism)
	command.Stdin = strings.NewReader(python)
	if output, err := command.CombinedOutput(); err != nil {
		return &scriptExecutionError{err: err, output: string(output)}
	}
	return nil
}

type scriptExecutionError struct {
	err    error
	output string
}

func (e *scriptExecutionError) Error() string { return e.err.Error() + ": " + e.output }

func TestAuthorizationPluginInstallerMakesAgentHaloFirstWithoutDeletingOtherRules(t *testing.T) {
	script := readAuthorizationPluginFile(t, "install.sh")
	python := installerPythonBlock(
		t, script,
		`if ! /usr/bin/python3 - "$TMP" "$TMP.new" "$RULE_NAME" "$MECHANISM" <<'PYEOF'`)
	directory := t.TempDir()
	source := filepath.Join(directory, "right.plist")
	first := filepath.Join(directory, "first.plist")
	second := filepath.Join(directory, "second.plist")
	original := []string{
		"com.openai.codex.locked-use",
		authorizationRuleName,
		"com.example.third-party",
		authorizationRuleName,
		"use-login-window-ui",
		"authenticate-session-owner-or-admin",
	}
	want := []string{
		authorizationRuleName,
		"com.openai.codex.locked-use",
		"com.example.third-party",
		"use-login-window-ui",
		"authenticate-session-owner-or-admin",
	}
	writeAuthorizationRightFixture(t, source, "rule", "rule", original)
	if err := runAuthorizationTransformer(t, python, source, first); err != nil {
		t.Fatal(err)
	}
	if got := readAuthorizationList(t, first, "rule"); !reflect.DeepEqual(got, want) {
		t.Fatalf("rule transform deleted or reordered another branch:\n got %#v\nwant %#v", got, want)
	}

	// Installation is intentionally repeatable: a second run must produce the
	// same list, not stack another AgentHalo rule or move another plug-in.
	if err := runAuthorizationTransformer(t, python, first, second); err != nil {
		t.Fatal(err)
	}
	if got := readAuthorizationList(t, second, "rule"); !reflect.DeepEqual(got, want) {
		t.Fatalf("rule transform is not idempotent:\n got %#v\nwant %#v", got, want)
	}
}

func TestAuthorizationPluginInstallerNormalizesOnlySinglePasswordFallback(t *testing.T) {
	script := readAuthorizationPluginFile(t, "install.sh")
	python := installerPythonBlock(
		t, script,
		`if ! /usr/bin/python3 - "$TMP" "$TMP.new" "$RULE_NAME" "$MECHANISM" <<'PYEOF'`)
	directory := t.TempDir()
	source := filepath.Join(directory, "right.plist")
	first := filepath.Join(directory, "first.plist")
	second := filepath.Join(directory, "second.plist")
	writeAuthorizationFixture(t, source, map[string]any{
		"class": "rule",
		"rule":  []string{"use-login-window-ui"},
	})

	if err := runAuthorizationTransformer(t, python, source, first); err != nil {
		t.Fatalf("single password fallback was not safely normalized: %v", err)
	}
	want := []string{authorizationRuleName, "use-login-window-ui"}
	if got := readAuthorizationList(t, first, "rule"); !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized rule list = %#v, want %#v", got, want)
	}
	if !authorizationFixtureHasExactIntegerKOfN(t, first, 1) {
		t.Fatal("normalization did not write exact integer k-of-n=1")
	}

	if err := runAuthorizationTransformer(t, python, first, second); err != nil {
		t.Fatalf("normalized shape is not idempotent: %v", err)
	}
	firstBody, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	secondBody, err := os.ReadFile(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstBody) != string(secondBody) {
		t.Fatal("second transform changed the already-normalized authorization right")
	}
}

func TestAuthorizationPluginInstallerRejectsUnsafeKOfNShapes(t *testing.T) {
	script := readAuthorizationPluginFile(t, "install.sh")
	python := installerPythonBlock(
		t, script,
		`if ! /usr/bin/python3 - "$TMP" "$TMP.new" "$RULE_NAME" "$MECHANISM" <<'PYEOF'`)
	directory := t.TempDir()

	tests := []struct {
		name    string
		fixture map[string]any
	}{
		{name: "missing with multiple rules", fixture: map[string]any{
			"class": "rule",
			"rule":  []string{"com.example.third-party", "use-login-window-ui"},
		}},
		{name: "string one", fixture: map[string]any{
			"class": "rule", "k-of-n": "1", "rule": []string{"use-login-window-ui"},
		}},
		{name: "boolean true", fixture: map[string]any{
			"class": "rule", "k-of-n": true, "rule": []string{"use-login-window-ui"},
		}},
		{name: "integer two", fixture: map[string]any{
			"class": "rule", "k-of-n": 2, "rule": []string{"use-login-window-ui"},
		}},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := filepath.Join(directory, "unsafe-"+strconv.Itoa(index)+".plist")
			destination := source + ".new"
			writeAuthorizationFixture(t, source, test.fixture)
			if err := runAuthorizationTransformer(t, python, source, destination); err == nil {
				t.Fatal("unsafe k-of-n shape was accepted")
			}
		})
	}
}

func TestAuthorizationPluginInstallerKeepsMechanismFirstAndPreservesFallbacks(t *testing.T) {
	script := readAuthorizationPluginFile(t, "install.sh")
	python := installerPythonBlock(
		t, script,
		`if ! /usr/bin/python3 - "$TMP" "$TMP.new" "$RULE_NAME" "$MECHANISM" <<'PYEOF'`)
	directory := t.TempDir()
	source := filepath.Join(directory, "right.plist")
	destination := filepath.Join(directory, "new.plist")
	original := []string{
		"builtin:authenticate,privileged",
		authorizationMechanism,
		"OtherPlugin:invoke,privileged",
		authorizationMechanism,
	}
	want := []string{
		authorizationMechanism,
		"builtin:authenticate,privileged",
		"OtherPlugin:invoke,privileged",
	}
	writeAuthorizationRightFixture(t, source, "evaluate-mechanisms", "mechanisms", original)
	if err := runAuthorizationTransformer(t, python, source, destination); err != nil {
		t.Fatal(err)
	}
	if got := readAuthorizationList(t, destination, "mechanisms"); !reflect.DeepEqual(got, want) {
		t.Fatalf("mechanism transform deleted or reordered a fallback:\n got %#v\nwant %#v", got, want)
	}
}

func runAuthorizationReadback(
	t *testing.T, python, current, original string,
) error {
	t.Helper()
	command := exec.Command(
		"python3", "-", current, original,
		authorizationRuleName, authorizationMechanism)
	command.Stdin = strings.NewReader(python)
	if output, err := command.CombinedOutput(); err != nil {
		return &scriptExecutionError{err: err, output: string(output)}
	}
	return nil
}

func TestAuthorizationPluginLiveReadbackRequiresFirstUniqueRuleAndPreservedFallbacks(t *testing.T) {
	script := readAuthorizationPluginFile(t, "install.sh")
	python := installerPythonBlock(
		t, script,
		`if ! /usr/bin/python3 - "$TMP.current" "$TMP" "$RULE_NAME" "$MECHANISM" <<'PYEOF'`)
	directory := t.TempDir()
	originalPath := filepath.Join(directory, "original.plist")
	currentPath := filepath.Join(directory, "current.plist")
	original := []string{
		"com.openai.codex.locked-use",
		authorizationRuleName,
		"com.example.third-party",
		"use-login-window-ui",
	}
	valid := []string{
		authorizationRuleName,
		"com.openai.codex.locked-use",
		"com.example.third-party",
		"use-login-window-ui",
	}
	writeAuthorizationRightFixture(t, originalPath, "rule", "rule", original)

	tests := []struct {
		name   string
		values []string
		valid  bool
	}{
		{name: "exact live order", values: valid, valid: true},
		{name: "AgentHalo follows another plug-in", values: []string{
			"com.openai.codex.locked-use", authorizationRuleName,
			"com.example.third-party", "use-login-window-ui",
		}},
		{name: "duplicate AgentHalo", values: append(
			append([]string{}, valid...), authorizationRuleName)},
		{name: "password fallback missing", values: []string{
			authorizationRuleName, "com.openai.codex.locked-use", "com.example.third-party",
		}},
		{name: "another plug-in deleted", values: []string{
			authorizationRuleName, "com.example.third-party", "use-login-window-ui",
		}},
		{name: "another plug-in reordered", values: []string{
			authorizationRuleName, "com.example.third-party",
			"com.openai.codex.locked-use", "use-login-window-ui",
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			writeAuthorizationRightFixture(t, currentPath, "rule", "rule", test.values)
			err := runAuthorizationReadback(t, python, currentPath, originalPath)
			if test.valid && err != nil {
				t.Fatalf("valid live readback rejected: %v", err)
			}
			if !test.valid && err == nil {
				t.Fatal("unsafe live readback accepted")
			}
		})
	}
}

func TestAuthorizationPluginLiveReadbackRequiresNormalizedKOfN(t *testing.T) {
	script := readAuthorizationPluginFile(t, "install.sh")
	python := installerPythonBlock(
		t, script,
		`if ! /usr/bin/python3 - "$TMP.current" "$TMP" "$RULE_NAME" "$MECHANISM" <<'PYEOF'`)
	directory := t.TempDir()
	originalPath := filepath.Join(directory, "original.plist")
	currentPath := filepath.Join(directory, "current.plist")
	writeAuthorizationFixture(t, originalPath, map[string]any{
		"class": "rule",
		"rule":  []string{"use-login-window-ui"},
	})
	valid := map[string]any{
		"class":  "rule",
		"k-of-n": 1,
		"rule":   []string{authorizationRuleName, "use-login-window-ui"},
	}
	writeAuthorizationFixture(t, currentPath, valid)
	if err := runAuthorizationReadback(t, python, currentPath, originalPath); err != nil {
		t.Fatalf("normalized live readback rejected: %v", err)
	}

	unsafe := []struct {
		name    string
		fixture map[string]any
	}{
		{name: "missing", fixture: map[string]any{
			"class": "rule", "rule": []string{authorizationRuleName, "use-login-window-ui"},
		}},
		{name: "string one", fixture: map[string]any{
			"class": "rule", "k-of-n": "1", "rule": []string{authorizationRuleName, "use-login-window-ui"},
		}},
		{name: "integer two", fixture: map[string]any{
			"class": "rule", "k-of-n": 2, "rule": []string{authorizationRuleName, "use-login-window-ui"},
		}},
	}
	for _, test := range unsafe {
		t.Run(test.name, func(t *testing.T) {
			writeAuthorizationFixture(t, currentPath, test.fixture)
			if err := runAuthorizationReadback(t, python, currentPath, originalPath); err == nil {
				t.Fatal("readback accepted an unsafe live k-of-n")
			}
		})
	}

	writeAuthorizationFixture(t, originalPath, map[string]any{
		"class": "rule",
		"rule":  []string{"com.example.third-party", "use-login-window-ui"},
	})
	writeAuthorizationFixture(t, currentPath, map[string]any{
		"class":  "rule",
		"k-of-n": 1,
		"rule": []string{
			authorizationRuleName, "com.example.third-party", "use-login-window-ui",
		},
	})
	if err := runAuthorizationReadback(t, python, currentPath, originalPath); err == nil {
		t.Fatal("readback accepted an ambiguously missing original k-of-n with multiple rules")
	}
}

func TestAuthorizationPreflightAcceptsOnlyCanonicalOrSafelyNormalizableRule(t *testing.T) {
	preflight, err := os.ReadFile(filepath.Join("..", "..", "mac", "preflight.sh"))
	if err != nil {
		t.Fatalf("read preflight.sh: %v", err)
	}
	python := installerPythonBlock(
		t, string(preflight),
		`RIGHT_SHAPE="$(/usr/bin/python3 - "$RIGHT_PLIST" <<'PYEOF'`)
	directory := t.TempDir()

	tests := []struct {
		name       string
		fixture    map[string]any
		wantOutput string
	}{
		{name: "canonical multi rule", fixture: map[string]any{
			"class": "rule", "k-of-n": 1,
			"rule": []string{"com.example.third-party", "use-login-window-ui"},
		}, wantOutput: "canonical-1-of-n\n"},
		{name: "normalizable single fallback", fixture: map[string]any{
			"class": "rule", "rule": []string{"use-login-window-ui"},
		}, wantOutput: "normalizable-single-password-fallback\n"},
		{name: "missing with multiple rules", fixture: map[string]any{
			"class": "rule", "rule": []string{"com.example.third-party", "use-login-window-ui"},
		}},
		{name: "string one", fixture: map[string]any{
			"class": "rule", "k-of-n": "1", "rule": []string{"use-login-window-ui"},
		}},
		{name: "boolean true", fixture: map[string]any{
			"class": "rule", "k-of-n": true, "rule": []string{"use-login-window-ui"},
		}},
		{name: "integer two", fixture: map[string]any{
			"class": "rule", "k-of-n": 2, "rule": []string{"use-login-window-ui"},
		}},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(directory, "preflight-"+strconv.Itoa(index)+".plist")
			writeAuthorizationFixture(t, path, test.fixture)
			command := exec.Command("python3", "-", path)
			command.Stdin = strings.NewReader(python)
			output, err := command.CombinedOutput()
			if test.wantOutput == "" {
				if err == nil {
					t.Fatalf("unsafe preflight shape accepted: %s", output)
				}
				return
			}
			if err != nil {
				t.Fatalf("safe preflight shape rejected: %v: %s", err, output)
			}
			if string(output) != test.wantOutput {
				t.Fatalf("preflight result = %q, want %q", output, test.wantOutput)
			}
		})
	}
}

func TestAuthorizationPluginLiveReadbackRequiresFirstUniqueMechanism(t *testing.T) {
	script := readAuthorizationPluginFile(t, "install.sh")
	python := installerPythonBlock(
		t, script,
		`if ! /usr/bin/python3 - "$TMP.current" "$TMP" "$RULE_NAME" "$MECHANISM" <<'PYEOF'`)
	directory := t.TempDir()
	originalPath := filepath.Join(directory, "original.plist")
	currentPath := filepath.Join(directory, "current.plist")
	original := []string{
		"builtin:authenticate,privileged",
		authorizationMechanism,
		"OtherPlugin:invoke,privileged",
	}
	valid := []string{
		authorizationMechanism,
		"builtin:authenticate,privileged",
		"OtherPlugin:invoke,privileged",
	}
	writeAuthorizationRightFixture(
		t, originalPath, "evaluate-mechanisms", "mechanisms", original)
	writeAuthorizationRightFixture(
		t, currentPath, "evaluate-mechanisms", "mechanisms", valid)
	if err := runAuthorizationReadback(t, python, currentPath, originalPath); err != nil {
		t.Fatalf("valid mechanism live readback rejected: %v", err)
	}

	writeAuthorizationRightFixture(t, currentPath, "evaluate-mechanisms", "mechanisms", []string{
		"builtin:authenticate,privileged", authorizationMechanism,
		"OtherPlugin:invoke,privileged",
	})
	if err := runAuthorizationReadback(t, python, currentPath, originalPath); err == nil {
		t.Fatal("live readback accepted AgentHalo mechanism after a fallback")
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

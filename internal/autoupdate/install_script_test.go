package autoupdate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallerMigratesRuntimeIdentity(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "deploy", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(body)
	for _, required := range []string{
		`-o bin/remote-agent ./cmd/remote-agent`,
		`STATE_DIR="${RA_STATE_DIR:-/opt/private-tunnel/state/remote-agent}"`,
		`LEGACY_STATE_DIR="${RA_LEGACY_STATE_DIR:-/opt/private-tunnel/state/remote-coding}"`,
		`mv "$LEGACY_STATE_DIR" "$STATE_DIR"`,
		`ln -s "$STATE_DIR" "$LEGACY_STATE_DIR"`,
		`echo "  remote-agent:"`,
		`echo "  remote-agent-log-upload:"`,
		`LEGACY_DROPIN="$ETC_DIR/services.d/remote-coding.yaml"`,
		`rm -f "$LEGACY_DROPIN"`,
		`"$SUPERVISOR" restart remote-agent`,
		`"~/.claude/remote-coding-turnstate"`,
		`claude["turnstate_dir"] = "~/.claude/remote-agent-turnstate"`,
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("installer missing migration invariant %q", required)
		}
	}
	if strings.Contains(script, `echo "  remote-coding:"`) || strings.Contains(script, `-o bin/remote-coding`) {
		t.Fatal("installer still registers or builds the legacy runtime identity")
	}
}

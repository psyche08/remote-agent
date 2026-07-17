package relaycert

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverPairPrefersDeviceUser(t *testing.T) {
	dir := t.TempDir()
	writePair(t, dir, "user-alpha")
	writePair(t, dir, "user-testuser")
	writePair(t, dir, "agent-testuser-device-a")

	cert, key, err := DiscoverPair(dir, "", "device-a")
	if err != nil {
		t.Fatal(err)
	}
	if cert != filepath.Join(dir, "user-testuser.crt") || key != filepath.Join(dir, "user-testuser.key") {
		t.Fatalf("cert=%s key=%s", cert, key)
	}
}

func TestDiscoverPairFallsBackToDeviceAgent(t *testing.T) {
	dir := t.TempDir()
	writePair(t, dir, "agent-testuser-device-b")

	cert, key, err := DiscoverPair(dir, "", "device-b")
	if err != nil {
		t.Fatal(err)
	}
	if cert != filepath.Join(dir, "agent-testuser-device-b.crt") || key != filepath.Join(dir, "agent-testuser-device-b.key") {
		t.Fatalf("cert=%s key=%s", cert, key)
	}
}

func TestDiscoverPairSupportsLegacyUserAgentName(t *testing.T) {
	dir := t.TempDir()
	writePair(t, dir, "testuser-agent")

	cert, key, err := DiscoverPair(dir, "testuser", "device-b")
	if err != nil {
		t.Fatal(err)
	}
	if cert != filepath.Join(dir, "testuser-agent.crt") || key != filepath.Join(dir, "testuser-agent.key") {
		t.Fatalf("cert=%s key=%s", cert, key)
	}
}

// Some legacy deployments only have testuser-agent.crt/key and the auto-update
// process does not know the user id, so discovery must support that fallback.
func TestDiscoverPairFallsBackToLegacyAgentNameWithoutUser(t *testing.T) {
	dir := t.TempDir()
	writePair(t, dir, "testuser-agent")

	cert, key, err := DiscoverPair(dir, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if cert != filepath.Join(dir, "testuser-agent.crt") || key != filepath.Join(dir, "testuser-agent.key") {
		t.Fatalf("cert=%s key=%s", cert, key)
	}
}

func TestDiscoverPairErrorsWhenNothingMatches(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := DiscoverPair(dir, "", ""); err == nil {
		t.Fatal("expected error for empty dir")
	}
}

func writePair(t *testing.T, dir string, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name+".crt"), []byte("cert"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".key"), []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
}

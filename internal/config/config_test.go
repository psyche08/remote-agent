package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigKeepsUnknownProviderFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	err := os.WriteFile(path, []byte(`{
	  "device_id": "device-a",
	  "providers": {
	    "codex": {"app_name": "Codex", "command": "codex", "cwd": "~/Developer", "approval_policy": "never"}
	  }
	}`), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DeviceID != "device-a" || cfg.Host != "127.0.0.1" || cfg.Port != 8765 {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
	if got := cfg.Providers["codex"].Extra["approval_policy"]; got != "never" {
		t.Fatalf("unknown provider field not preserved: %#v", got)
	}
}

func TestResolvePathPrefersExplicit(t *testing.T) {
	dir := t.TempDir()
	example := filepath.Join(dir, "config.example.json")
	explicit := filepath.Join(dir, "custom.json")
	for _, p := range []string{example, explicit} {
		if err := os.WriteFile(p, []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ResolvePath(explicit, dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != explicit {
		t.Fatalf("got %s want %s", got, explicit)
	}
}

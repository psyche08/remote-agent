package config

import (
	"encoding/json"
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
	    "codex": {"type": "codex", "app_name": "Codex", "command": "codex", "args": ["app-server"], "cwd": "~/Developer", "approval_policy": "never"}
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
	pc := cfg.Providers["codex"]
	if pc.Type != "codex" || len(pc.Args) != 1 || pc.Args[0] != "app-server" {
		t.Fatalf("typed provider fields not decoded: %#v", pc)
	}
	if _, ok := pc.Extra["type"]; ok {
		t.Fatalf("typed provider field leaked into Extra: %#v", pc.Extra)
	}
}

func TestExampleEnablesFreshInstallRuntimeDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ComputerUse.Enabled {
		t.Fatal("fresh-install example disables computer use")
	}
	if !cfg.ComputerUse.LockedUse.Enabled {
		t.Fatal("fresh-install example disables Locked Use")
	}
	if cfg.ComputerUse.DebugHTTPActions {
		t.Fatal("fresh-install example enables HTTP debug actions")
	}
	claude := cfg.Providers["claude"].Extra
	if got := claude["primary_route"]; got != "desktop_computer_use" {
		t.Fatalf("fresh-install Claude primary_route=%#v", got)
	}
	if got := claude["fallback_route"]; got != "stream_json_cli" {
		t.Fatalf("fresh-install Claude fallback_route=%#v", got)
	}
	if got := claude["desktop_bundle_id"]; got != "com.anthropic.claudefordesktop" {
		t.Fatalf("fresh-install Claude desktop_bundle_id=%#v", got)
	}
	if got := claude["desktop_team_id"]; got != "Q6L2SF6YDW" {
		t.Fatalf("fresh-install Claude desktop_team_id=%#v", got)
	}
	got, ok := cfg.Providers["codex"].Extra["shared_daemon_autostart"].(bool)
	if !ok || !got {
		t.Fatalf("fresh-install Codex autostart=%#v, want true", cfg.Providers["codex"].Extra["shared_daemon_autostart"])
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

// Computer use and Locked Use are both opt-in. A config that never mentions
// them must leave the device with no computer-use surface at all.
func TestComputerUseDefaultsOff(t *testing.T) {
	var cfg Config
	ApplyDefaults(&cfg)
	if cfg.ComputerUse.Enabled {
		t.Error("computer use defaulted to enabled")
	}
	if cfg.ComputerUse.DebugHTTPActions {
		t.Error("computer-use HTTP debug actions defaulted to enabled")
	}
	if cfg.ComputerUse.LockedUse.Enabled {
		t.Error("locked use defaulted to enabled")
	}
	// The shield is required unless a deployment explicitly opts out, so a
	// zero-valued config cannot open an unshielded window.
	if !cfg.ComputerUse.LockedUse.ShieldRequired() {
		t.Error("display shield is not required by default")
	}
}

func TestComputerUseDebugHTTPActionsRequiresExplicitConfig(t *testing.T) {
	cfg := Config{ComputerUse: ComputerUseConfig{
		Enabled: true, DebugHTTPActions: true,
		LockedUse: LockedUseConfig{Enabled: true},
	}}
	ApplyDefaults(&cfg)
	if !cfg.ComputerUse.DebugHTTPActions {
		t.Fatal("explicit debug_http_actions was not preserved")
	}

	var decoded Config
	if err := json.Unmarshal([]byte(`{
		"computer_use":{"enabled":true,"locked_use":{"enabled":true}}
	}`), &decoded); err != nil {
		t.Fatal(err)
	}
	ApplyDefaults(&decoded)
	if decoded.ComputerUse.DebugHTTPActions {
		t.Fatal("omitted debug_http_actions normalized to true")
	}
}

// Locked Use extends computer use and must not be reachable on its own.
func TestLockedUseCannotOutliveComputerUse(t *testing.T) {
	cfg := Config{ComputerUse: ComputerUseConfig{
		Enabled:   false,
		LockedUse: LockedUseConfig{Enabled: true},
	}}
	ApplyDefaults(&cfg)
	if cfg.ComputerUse.LockedUse.Enabled {
		t.Error("locked use stayed enabled while computer use was disabled")
	}
}

func TestLockedUseTimingsAreClamped(t *testing.T) {
	cfg := Config{ComputerUse: ComputerUseConfig{
		Enabled: true,
		LockedUse: LockedUseConfig{
			Enabled:            true,
			GrantTTLSeconds:    9999,
			WindowTTLSeconds:   9999999,
			InputRelockGraceMs: 9999999,
		},
	}}
	ApplyDefaults(&cfg)
	lu := cfg.ComputerUse.LockedUse
	if lu.GrantTTLSeconds != MaxGrantTTLSeconds {
		t.Errorf("grant ttl = %d, want %d", lu.GrantTTLSeconds, MaxGrantTTLSeconds)
	}
	if lu.WindowTTLSeconds != 900 {
		t.Errorf("window ttl = %d, want 900", lu.WindowTTLSeconds)
	}
	if lu.InputRelockGraceMs != 5000 {
		t.Errorf("input grace = %d, want 5000", lu.InputRelockGraceMs)
	}

	// A negative or tiny value must be raised to the floor, never left to
	// disable a check by being effectively zero.
	tiny := Config{ComputerUse: ComputerUseConfig{
		Enabled: true,
		LockedUse: LockedUseConfig{
			Enabled: true, GrantTTLSeconds: -5, WindowTTLSeconds: 1, InputRelockGraceMs: 1,
		},
	}}
	ApplyDefaults(&tiny)
	lu = tiny.ComputerUse.LockedUse
	if lu.GrantTTLSeconds < 2 || lu.WindowTTLSeconds < 15 || lu.InputRelockGraceMs < 100 {
		t.Errorf("clamps did not raise sub-floor values: %+v", lu)
	}
}

func TestLockedUseForcesShieldWhenConfigRequestsOptOut(t *testing.T) {
	off := false
	cfg := Config{ComputerUse: ComputerUseConfig{
		Enabled:   true,
		LockedUse: LockedUseConfig{Enabled: true, RequireDisplayShield: &off},
	}}
	ApplyDefaults(&cfg)
	if !cfg.ComputerUse.LockedUse.ShieldRequired() {
		t.Error("Locked Use retained an unsafe require_display_shield=false opt-out")
	}

	// Ordinary computer use does not own an unlock window or InputGuard, so a
	// disabled Locked Use block retains the caller's value unchanged.
	ordinary := Config{ComputerUse: ComputerUseConfig{
		Enabled:   true,
		LockedUse: LockedUseConfig{Enabled: false, RequireDisplayShield: &off},
	}}
	ApplyDefaults(&ordinary)
	if ordinary.ComputerUse.LockedUse.ShieldRequired() {
		t.Error("Locked Use-disabled config did not retain require_display_shield=false")
	}
}

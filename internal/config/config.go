package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type Config struct {
	DeviceID        string                    `json:"device_id"`
	Devices         []string                  `json:"devices"`
	Host            string                    `json:"host"`
	Port            int                       `json:"port"`
	UDS             string                    `json:"uds"`
	StateDir        string                    `json:"state_dir"`
	ProjectRoots    []string                  `json:"project_roots"`
	PushProxy       string                    `json:"push_proxy"`
	PushVAPID       map[string]string         `json:"push_vapid"`
	DefaultProvider string                    `json:"default_provider"`
	Providers       map[string]ProviderConfig `json:"providers"`
	ComputerUse     ComputerUseConfig         `json:"computer_use"`
}

// ComputerUseConfig gates the device-scoped computer-use control surface (the
// agent operating the desktop directly) and its Locked Use extension. Both are
// opt-in and disabled by default: an unset block means the feature is off and
// every endpoint reports unavailable.
type ComputerUseConfig struct {
	// Enabled turns on the computer-use action surface (screenshot/click/type/
	// key/move/scroll). When false, /computer_use/action returns not_enabled.
	Enabled bool `json:"enabled"`
	// LockedUse participates in the macOS unlock flow so an authorized turn can
	// drive the desktop after the screen locks. It requires Enabled and the
	// separately installed Apple Authorization Plug-in.
	LockedUse LockedUseConfig `json:"locked_use"`
}

// LockedUseConfig configures Locked Use. Every field fails closed: an unset or
// invalid value collapses to the most restrictive safe default rather than a
// wider unlock window.
type LockedUseConfig struct {
	// Enabled opts this device into Locked Use. Off by default. Turning it on
	// still requires the provisioned Authorization Plug-in; without a verifiable
	// key pair the controller stays disarmed.
	Enabled bool `json:"enabled"`
	// GrantDir is where the controller writes signed unlock grants for the
	// Authorization Plug-in to read. Defaults to <state_dir>/data/locked-use.
	GrantDir string `json:"grant_dir"`
	// SigningKeyPath is the Ed25519 private key the controller signs grants
	// with. Defaults to <state_dir>/data/locked-use/signing.key (0600). The
	// plugin is provisioned with only the matching public key.
	SigningKeyPath string `json:"signing_key_path"`
	// GrantTTLSeconds bounds how long a single signed grant is valid. A grant
	// is minted just before an unlock attempt and consumed by it, so this is
	// deliberately tiny: a grant that lingers on disk is ambient authorization
	// any local process could ride. The Authorization Plug-in independently
	// enforces its own hard ceiling and ignores a longer self-declared expiry.
	// Clamped to [2, 15]; default 10.
	GrantTTLSeconds int `json:"grant_ttl_seconds"`
	// WindowTTLSeconds is the hard ceiling on a single per-turn unlock window
	// regardless of turn activity. Clamped to [15, 900]; default 300.
	WindowTTLSeconds int `json:"window_ttl_seconds"`
	// InputRelockGraceMs is how long the machine must already have been idle of
	// local input before a window may open. It does NOT set the monitor's poll
	// cadence: that is a fixed fast interval so this knob cannot be widened
	// into a window where a present human types unnoticed.
	// Clamped to [100, 5000]; default 250.
	InputRelockGraceMs int `json:"input_relock_grace_ms"`
	// RequireDisplayShield, when true (the default), refuses to open a window
	// unless the privacy shield covering the screen engages first.
	RequireDisplayShield *bool `json:"require_display_shield"`
}

type ProviderConfig struct {
	Type    string         `json:"type"`
	AppName string         `json:"app_name"`
	Command string         `json:"command"`
	Args    []string       `json:"args"`
	Cwd     string         `json:"cwd"`
	Extra   map[string]any `json:"-"`
}

func (p *ProviderConfig) UnmarshalJSON(b []byte) error {
	type alias ProviderConfig
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	delete(raw, "type")
	delete(raw, "app_name")
	delete(raw, "command")
	delete(raw, "args")
	delete(raw, "cwd")
	*p = ProviderConfig(a)
	p.Extra = raw
	return nil
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	ApplyDefaults(&cfg)
	return &cfg, nil
}

func ApplyDefaults(cfg *Config) {
	if cfg.DeviceID == "" {
		cfg.DeviceID = envOr("DEVICE_ID", "mac-unknown")
	}
	if len(cfg.Devices) == 0 {
		cfg.Devices = []string{cfg.DeviceID}
	}
	if cfg.Host == "" {
		cfg.Host = "127.0.0.1"
	}
	if cfg.Port == 0 {
		cfg.Port = 8765
	}
	if cfg.DefaultProvider == "" {
		cfg.DefaultProvider = "claude"
	}
	if cfg.Providers == nil {
		cfg.Providers = map[string]ProviderConfig{}
	}
	applyComputerUseDefaults(&cfg.ComputerUse)
}

const (
	DefaultGrantTTLSeconds    = 10
	DefaultWindowTTLSeconds   = 300
	DefaultInputRelockGraceMs = 250
	// MaxGrantTTLSeconds mirrors the ceiling the Authorization Plug-in enforces
	// independently. Keeping both sides in step means a config change can only
	// ever shorten a grant's life, never extend it past what the plugin accepts.
	MaxGrantTTLSeconds = 15
)

func applyComputerUseDefaults(cu *ComputerUseConfig) {
	lu := &cu.LockedUse
	lu.GrantTTLSeconds = clampInt(lu.GrantTTLSeconds, 2, MaxGrantTTLSeconds, DefaultGrantTTLSeconds)
	lu.WindowTTLSeconds = clampInt(lu.WindowTTLSeconds, 15, 900, DefaultWindowTTLSeconds)
	lu.InputRelockGraceMs = clampInt(lu.InputRelockGraceMs, 100, 5000, DefaultInputRelockGraceMs)
	if lu.RequireDisplayShield == nil {
		shield := true
		lu.RequireDisplayShield = &shield
	}
	// Locked Use extends computer use; it can never be the only thing enabled.
	if !cu.Enabled {
		lu.Enabled = false
	}
	// A grant TTL at or above the window ceiling would let one minted grant
	// outlive the window it was issued for.
	if lu.GrantTTLSeconds > lu.WindowTTLSeconds {
		lu.GrantTTLSeconds = lu.WindowTTLSeconds
	}
}

// clampInt keeps an out-of-range or unset value from widening a security
// window. Zero means "unset" and takes the default; anything else is clamped.
func clampInt(v, min, max, fallback int) int {
	if v == 0 {
		return fallback
	}
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// ShieldRequired reports whether a window may open without the display shield.
// It reads as fail-closed for a zero-valued struct built outside Load.
func (l LockedUseConfig) ShieldRequired() bool {
	return l.RequireDisplayShield == nil || *l.RequireDisplayShield
}

func ResolvePath(explicit string, baseDir string) (string, error) {
	candidates := []string{}
	if explicit != "" {
		candidates = append(candidates, explicit)
	}
	if baseDir != "" {
		candidates = append(candidates,
			filepath.Join(baseDir, "config.json"),
			filepath.Join(baseDir, "config.example.json"),
		)
	}
	for _, p := range candidates {
		if p == "" {
			continue
		}
		p = expandUser(p)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", errors.New("no config file found")
}

func ResolveStateDir(cfg *Config, baseDir string) string {
	if cfg.StateDir != "" {
		return expandUser(cfg.StateDir)
	}
	return baseDir
}

func expandUser(p string) string {
	if len(p) >= 2 && p[:2] == "~/" {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}

func envOr(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}

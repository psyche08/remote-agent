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
}

type ProviderConfig struct {
	AppName string         `json:"app_name"`
	Command string         `json:"command"`
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
	delete(raw, "app_name")
	delete(raw, "command")
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

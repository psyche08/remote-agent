package main

import (
	"testing"

	"github.com/psyche08/remote-agent/internal/config"
)

func TestApplyListenerOverridesUsesExplicitUDSForInternalHealth(t *testing.T) {
	cfg := &config.Config{Host: "127.0.0.1", Port: 8765, UDS: "/configured.sock"}
	if err := applyListenerOverrides(cfg, "127.0.0.1:0", "/tmp/explicit.sock"); err != nil {
		t.Fatal(err)
	}
	if cfg.UDS != "/tmp/explicit.sock" || cfg.Host != "127.0.0.1" || cfg.Port != 8765 {
		t.Fatalf("UDS override was not made authoritative: %#v", cfg)
	}
}

func TestApplyListenerOverridesUpdatesDevelopmentTCP(t *testing.T) {
	cfg := &config.Config{Host: "127.0.0.1", Port: 8765, UDS: "/configured.sock"}
	if err := applyListenerOverrides(cfg, "localhost:18765", ""); err != nil {
		t.Fatal(err)
	}
	if cfg.UDS != "" || cfg.Host != "localhost" || cfg.Port != 18765 {
		t.Fatalf("TCP override was not reflected in runtime config: %#v", cfg)
	}
}

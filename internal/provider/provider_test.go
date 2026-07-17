package provider

import (
	"testing"

	"github.com/psyche08/remote-agent/internal/config"
)

func TestBuildRegistryUsesOneClaudeStreamJSONProvider(t *testing.T) {
	reg := BuildRegistry(&config.Config{Providers: map[string]config.ProviderConfig{
		"claude": {AppName: "Claude Code CLI", Command: "/custom/claude", Cwd: "/work"},
	}})
	cli, ok := reg["claude"].(*Claude)
	if !ok || cli.cliStream == nil {
		t.Fatalf("claude provider is not stream-json: %#v", reg["claude"])
	}
	if _, exists := reg["claude_cli"]; exists {
		t.Fatalf("claude_cli must be an API alias, not a second provider: %#v", reg.IDs())
	}
	if cli.Status().AppName != "Claude" || cli.command != "/custom/claude" || cli.cwd != "/work" {
		t.Fatalf("legacy Claude config was not migrated: %#v", cli.Status())
	}
	ids := reg.IDs()
	if len(ids) != 2 || ids[0] != "codex" || ids[1] != "claude" {
		t.Fatalf("unexpected provider order: %#v", ids)
	}
}

func TestBuildRegistryPrefersExplicitClaudeCLIConfig(t *testing.T) {
	reg := BuildRegistry(&config.Config{Providers: map[string]config.ProviderConfig{
		"claude":     {Command: "/old/desktop-cli", Cwd: "/old"},
		"claude_cli": {Command: "/standalone/claude", Cwd: "/work"},
	}})
	c := reg["claude"].(*Claude)
	if c.command != "/standalone/claude" || c.cwd != "/work" {
		t.Fatalf("explicit CLI config not selected: %#v", c.Status())
	}
}

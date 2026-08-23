package provider

import (
	"encoding/json"
	"testing"

	"github.com/psyche08/remote-agent/internal/config"
)

type countingStatusProvider struct {
	*PTYProvider
	statusCalls int
}

func (p *countingStatusProvider) Status() Status {
	p.statusCalls++
	return p.PTYProvider.Status()
}

func TestProviderProfilesDescribeCanonicalOwnersAndRoutes(t *testing.T) {
	registry := BuildRegistry(&config.Config{Providers: map[string]config.ProviderConfig{}})
	t.Cleanup(registry.Shutdown)

	codex := Profile(registry["codex"])
	if codex.ProviderID != "codex" || codex.Family != ProviderFamilyCodex ||
		codex.AdapterKind != AdapterKindCodexAppServer ||
		codex.RuntimeNamespace != RuntimeNamespaceCodex || codex.Surface != ProviderSurfaceCodex {
		t.Fatalf("unexpected Codex profile: %#v", codex)
	}
	if len(codex.Aliases) != 1 || codex.Aliases[0] != (ProviderAlias{
		ProviderID: "chatgpt", Surface: ProviderSurfaceChatGPT,
	}) {
		t.Fatalf("ChatGPT must be one Codex surface alias: %#v", codex.Aliases)
	}
	if len(codex.Routes) != 1 || codex.Routes[0].Role != ProviderRoutePrimary {
		t.Fatalf("unexpected Codex routes: %#v", codex.Routes)
	}

	claude := Profile(registry["claude"])
	if claude.ProviderID != "claude" || claude.Family != ProviderFamilyClaude ||
		claude.AdapterKind != AdapterKindDesktopTranscript ||
		claude.RuntimeNamespace != RuntimeNamespaceClaude ||
		claude.Surface != ProviderSurfaceClaudeDesktop {
		t.Fatalf("unexpected Claude profile: %#v", claude)
	}
	if len(claude.Routes) != 2 || claude.Routes[0] != (ProviderRoute{
		AdapterKind: AdapterKindDesktopTranscript,
		Surface:     ProviderSurfaceClaudeDesktop,
		Role:        ProviderRoutePrimary,
	}) || claude.Routes[1] != (ProviderRoute{
		AdapterKind: AdapterKindStreamJSONCLI,
		Surface:     ProviderSurfaceClaudeCLI,
		Role:        ProviderRouteFallback,
	}) {
		t.Fatalf("unexpected Claude primary/fallback routes: %#v", claude.Routes)
	}
}

func TestChatGPTAliasResolvesToCodexWithoutSecondOwner(t *testing.T) {
	registry := BuildRegistry(&config.Config{Providers: map[string]config.ProviderConfig{
		"chatgpt": {
			Type: "pty", AppName: "must not register", Command: "/bin/sh", Cwd: t.TempDir(),
		},
	}})
	t.Cleanup(registry.Shutdown)

	if _, exists := registry["chatgpt"]; exists {
		t.Fatalf("ChatGPT alias registered a second owner: %#v", registry.IDs())
	}
	p, canonical, ok := registry.Resolve("chatgpt")
	if !ok || canonical != "codex" || p != registry["codex"] {
		t.Fatalf("ChatGPT alias did not resolve to Codex: canonical=%q ok=%v p=%T", canonical, ok, p)
	}
	if CanonicalProviderID("claude_desktop") != "claude" || CanonicalProviderID("claude_cli") != "claude" {
		t.Fatal("Claude surfaces must retain one canonical owner")
	}
	if got := registry.IDs(); len(got) != 2 || got[0] != "codex" || got[1] != "claude" {
		t.Fatalf("aliases changed canonical provider list: %#v", got)
	}
}

func TestProviderMetadataRetainsLegacyFieldsAndTypedProfile(t *testing.T) {
	registry := BuildRegistry(&config.Config{Providers: map[string]config.ProviderConfig{}})
	t.Cleanup(registry.Shutdown)

	rows := registry.Metadata()
	if len(rows) != 2 || rows[0].ProviderID != "codex" || rows[1].ProviderID != "claude" {
		t.Fatalf("unexpected metadata order: %#v", rows)
	}
	row := rows[0]
	if row.Status.ProviderID != "codex" || row.Capabilities == nil || len(row.Actions) == 0 {
		t.Fatalf("legacy provider fields missing: %#v", row)
	}
	if row.Family != ProviderFamilyCodex || row.AdapterKind != AdapterKindCodexAppServer {
		t.Fatalf("typed profile missing: %#v", row.ProviderProfile)
	}

	b, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(b, &payload); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"provider_id", "family", "adapter_kind", "runtime_namespace", "surface",
		"aliases", "routes", "status", "capabilities", "actions", "model_select",
	} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("metadata JSON missing %q: %s", key, b)
		}
	}
}

func TestProviderMetadataReusesOneStatusSnapshotForActions(t *testing.T) {
	p := &countingStatusProvider{PTYProvider: NewPTYProvider("counted", config.ProviderConfig{
		Command: "/bin/sh", Cwd: t.TempDir(),
	})}
	metadata := Metadata(p)
	if p.statusCalls != 1 {
		t.Fatalf("metadata probed provider status %d times, want one", p.statusCalls)
	}
	if metadata.Status.ProviderID != "counted" || len(metadata.Actions) == 0 {
		t.Fatalf("unexpected metadata: %#v", metadata)
	}
}

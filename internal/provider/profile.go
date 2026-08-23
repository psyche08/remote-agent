package provider

// ProviderFamily identifies the native session protocol family. Multiple UI
// surfaces may belong to one family and must not be treated as independent
// session owners.
type ProviderFamily string

const (
	ProviderFamilyCodex    ProviderFamily = "codex"
	ProviderFamilyClaude   ProviderFamily = "claude"
	ProviderFamilyDeepSeek ProviderFamily = "deepseek"
	ProviderFamilyCatPaw   ProviderFamily = "catpaw"
	ProviderFamilyGeneric  ProviderFamily = "generic"
)

// AdapterKind identifies how AgentHalo talks to the native runtime. It is a
// closed vocabulary so API and PWA consumers do not need to infer transport
// semantics from Status.Backend or a provider id.
type AdapterKind string

const (
	AdapterKindCodexAppServer    AdapterKind = "codex_app_server"
	AdapterKindDesktopTranscript AdapterKind = "desktop_transcript"
	AdapterKindStreamJSONCLI     AdapterKind = "stream_json_cli"
	AdapterKindGenericPTY        AdapterKind = "generic_pty"
	AdapterKindUnknown           AdapterKind = "unknown"
)

// RuntimeNamespace is the durable ownership namespace for native sessions.
// Aliased surfaces share a namespace and therefore can never create a second
// native owner for the same logical session.
type RuntimeNamespace string

const (
	RuntimeNamespaceCodex    RuntimeNamespace = "codex"
	RuntimeNamespaceClaude   RuntimeNamespace = "claude"
	RuntimeNamespaceDeepSeek RuntimeNamespace = "deepseek"
	RuntimeNamespaceCatPaw   RuntimeNamespace = "catpaw"
)

// ProviderSurface is the product UI through which the user sees a provider.
// It is deliberately distinct from ProviderFamily and RuntimeNamespace.
type ProviderSurface string

const (
	ProviderSurfaceCodex         ProviderSurface = "codex"
	ProviderSurfaceChatGPT       ProviderSurface = "chatgpt"
	ProviderSurfaceClaudeDesktop ProviderSurface = "claude_desktop"
	ProviderSurfaceClaudeCLI     ProviderSurface = "claude_cli"
	ProviderSurfaceDeepSeek      ProviderSurface = "deepseek"
	ProviderSurfaceCatPaw        ProviderSurface = "catpaw"
	ProviderSurfaceTerminal      ProviderSurface = "terminal"
)

// ProviderRouteRole defines whether a route is eligible before any side
// effect (primary) or only under the provider's fail-closed fallback policy.
type ProviderRouteRole string

const (
	ProviderRoutePrimary  ProviderRouteRole = "primary"
	ProviderRouteFallback ProviderRouteRole = "fallback"
)

// ProviderRoute describes one route owned by a canonical provider. A route is
// metadata only; it grants no authority and does not relax per-session route
// commitment rules.
type ProviderRoute struct {
	AdapterKind AdapterKind       `json:"adapter_kind"`
	Surface     ProviderSurface   `json:"surface"`
	Role        ProviderRouteRole `json:"role"`
}

// ProviderAlias exposes another product surface without registering another
// Provider instance. ProviderID is accepted at API boundaries and resolves to
// the canonical ProviderProfile.ProviderID.
type ProviderAlias struct {
	ProviderID string          `json:"provider_id"`
	Surface    ProviderSurface `json:"surface"`
}

// ProviderProfile is stable provider identity and routing metadata. The first
// four fields describe the canonical primary route; Routes may additionally
// describe a tightly constrained fallback owned by that same provider.
type ProviderProfile struct {
	ProviderID       string           `json:"provider_id"`
	Family           ProviderFamily   `json:"family"`
	AdapterKind      AdapterKind      `json:"adapter_kind"`
	RuntimeNamespace RuntimeNamespace `json:"runtime_namespace"`
	Surface          ProviderSurface  `json:"surface"`
	Aliases          []ProviderAlias  `json:"aliases"`
	Routes           []ProviderRoute  `json:"routes"`
}

// ProviderProfiler lets a provider declare its native identity without
// expanding the existing Provider delivery interface.
type ProviderProfiler interface {
	ProviderProfile() ProviderProfile
}

// ProviderMetadata is the typed /providers row. Its legacy status,
// capabilities, actions, and model_select fields are retained so clients can
// adopt ProviderProfile fields incrementally.
type ProviderMetadata struct {
	ProviderProfile
	Status       Status             `json:"status"`
	Capabilities map[string]bool    `json:"capabilities"`
	Actions      []ActionCapability `json:"actions"`
	ModelSelect  ModelSelect        `json:"model_select"`
}

// CanonicalProviderID resolves a product surface to its single provider
// owner. In particular ChatGPT and Codex share one app-server owner.
func CanonicalProviderID(id string) string {
	switch id {
	case "chatgpt":
		return "codex"
	case "claude_cli", "claude_desktop":
		return "claude"
	default:
		return id
	}
}

// Profile returns normalized metadata even for older providers that have not
// implemented ProviderProfiler yet.
func Profile(p Provider) ProviderProfile {
	if p == nil {
		return ProviderProfile{
			Family:      ProviderFamilyGeneric,
			AdapterKind: AdapterKindUnknown,
			Aliases:     []ProviderAlias{},
			Routes:      []ProviderRoute{},
		}
	}
	if profiler, ok := p.(ProviderProfiler); ok {
		return normalizeProviderProfile(p.ID(), profiler.ProviderProfile())
	}
	return normalizeProviderProfile(p.ID(), ProviderProfile{})
}

func normalizeProviderProfile(id string, profile ProviderProfile) ProviderProfile {
	if profile.ProviderID == "" {
		profile.ProviderID = CanonicalProviderID(id)
	}
	if profile.Family == "" {
		profile.Family = ProviderFamilyGeneric
	}
	if profile.AdapterKind == "" {
		profile.AdapterKind = AdapterKindUnknown
	}
	if profile.RuntimeNamespace == "" {
		profile.RuntimeNamespace = RuntimeNamespace(profile.ProviderID)
	}
	if profile.Surface == "" {
		profile.Surface = ProviderSurface(profile.ProviderID)
	}
	if profile.Aliases == nil {
		profile.Aliases = []ProviderAlias{}
	}
	if profile.Routes == nil {
		profile.Routes = []ProviderRoute{{
			AdapterKind: profile.AdapterKind,
			Surface:     profile.Surface,
			Role:        ProviderRoutePrimary,
		}}
	}
	return profile
}

func (c *Codex) ProviderProfile() ProviderProfile {
	return ProviderProfile{
		ProviderID:       "codex",
		Family:           ProviderFamilyCodex,
		AdapterKind:      AdapterKindCodexAppServer,
		RuntimeNamespace: RuntimeNamespaceCodex,
		Surface:          ProviderSurfaceCodex,
		Aliases: []ProviderAlias{
			{ProviderID: "chatgpt", Surface: ProviderSurfaceChatGPT},
		},
		Routes: []ProviderRoute{
			{AdapterKind: AdapterKindCodexAppServer, Surface: ProviderSurfaceCodex, Role: ProviderRoutePrimary},
		},
	}
}

func (c *Claude) ProviderProfile() ProviderProfile {
	return ProviderProfile{
		ProviderID:       "claude",
		Family:           ProviderFamilyClaude,
		AdapterKind:      AdapterKindDesktopTranscript,
		RuntimeNamespace: RuntimeNamespaceClaude,
		Surface:          ProviderSurfaceClaudeDesktop,
		Aliases: []ProviderAlias{
			{ProviderID: "claude_desktop", Surface: ProviderSurfaceClaudeDesktop},
			{ProviderID: "claude_cli", Surface: ProviderSurfaceClaudeCLI},
		},
		Routes: []ProviderRoute{
			{AdapterKind: AdapterKindDesktopTranscript, Surface: ProviderSurfaceClaudeDesktop, Role: ProviderRoutePrimary},
			{AdapterKind: AdapterKindStreamJSONCLI, Surface: ProviderSurfaceClaudeCLI, Role: ProviderRouteFallback},
		},
	}
}

func (p *PTYProvider) ProviderProfile() ProviderProfile {
	return ProviderProfile{
		ProviderID:       p.ID(),
		Family:           ProviderFamilyGeneric,
		AdapterKind:      AdapterKindGenericPTY,
		RuntimeNamespace: RuntimeNamespace(p.ID()),
		Surface:          ProviderSurfaceTerminal,
	}
}

// Metadata creates a provider-neutral typed response without changing the
// Provider interface or calling mutable provider operations.
func Metadata(p Provider) ProviderMetadata {
	profile := Profile(p)
	status := p.Status()
	return ProviderMetadata{
		ProviderProfile: profile,
		Status:          status,
		Capabilities:    status.Capabilities,
		Actions:         ActionsForStatus(p, status),
		ModelSelect:     p.ModelSelect(),
	}
}

// Resolve returns the one canonical provider for an id or aliased surface.
func (r Registry) Resolve(id string) (Provider, string, bool) {
	canonical := CanonicalProviderID(id)
	p, ok := r[canonical]
	return p, canonical, ok
}

// Metadata returns providers in the same stable order as IDs.
func (r Registry) Metadata() []ProviderMetadata {
	rows := make([]ProviderMetadata, 0, len(r))
	for _, id := range r.IDs() {
		rows = append(rows, Metadata(r[id]))
	}
	return rows
}

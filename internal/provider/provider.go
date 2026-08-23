package provider

import (
	"context"
	"errors"
	"sort"

	"github.com/psyche08/remote-agent/internal/config"
)

var ErrComputerUseAutomationCleanup = errors.New("computer-use automation cleanup was not confirmed")

// ComputerUseToolRequest is a model-originated computer-use call after the
// provider has bound it to an authoritative runtime turn. ProviderID,
// SessionID, ThreadID, and TurnID are derived from the provider protocol; a
// tool argument can never override them.
type ComputerUseToolRequest struct {
	ProviderID string
	SessionID  string
	ThreadID   string
	TurnID     string
	CallID     string
	Tool       string

	App      string
	BundleID string
	Path     []int
	Value    *string

	X      *int
	Y      *int
	Button string
	Count  int
	Text   string
	Keys   []string
	DeltaX int
	DeltaY int
}

// ComputerUseToolResult is provider-neutral model content. ImageURL, when
// present, is a validated data:image URL rather than a helper filesystem path.
type ComputerUseToolResult struct {
	Text     string
	ImageURL string
}

type ComputerUseToolHandler func(context.Context, ComputerUseToolRequest) (ComputerUseToolResult, error)

// ComputerUseToolHost is implemented by providers that can expose computer
// use as a first-class model tool. The handler is installed by the API server
// and executes in-process; it must never be routed back through public HTTP.
type ComputerUseToolHost interface {
	SetComputerUseToolHandler(ComputerUseToolHandler)
}

// ComputerUseAutomationCallback is one bounded, trusted provider UI
// transaction. The server injects a tool handler whose provider, logical
// session, and short-lived operation turn are already fixed outside every tool
// request. The callback may perform multiple inspect/mutate steps, but must not
// retain the handler after it returns.
type ComputerUseAutomationCallback func(context.Context, ComputerUseToolHandler) error

// ComputerUseAutomationHandler runs a trusted provider's UI transaction for
// one stored logical session. This is deliberately separate from
// ComputerUseToolHost: model-originated tools must continue proving their
// native generation/thread/turn envelope, while this seam is authority granted
// by the server only for the lifetime of the callback.
type ComputerUseAutomationHandler func(context.Context, string, ComputerUseAutomationCallback) error

// ComputerUseAutomationHost is implemented by providers whose trusted adapter
// drives their own UI. The handler is installed in-process by the API server;
// it must never be exposed through HTTP or accept a provider identity from the
// provider callback.
type ComputerUseAutomationHost interface {
	SetComputerUseAutomationHandler(ComputerUseAutomationHandler)
}

// ComputerUseReadiness is a read-only snapshot from the signed desktop
// helper. It is status metadata only: providers must still pass every fresh
// helper gate inside the actual operation, and must never treat this snapshot
// as authority to unlock or mutate an application.
type ComputerUseReadiness struct {
	Enabled                bool
	Available              bool
	LockedUseEnabled       bool
	LockedUseArmed         bool
	LockedUseActive        bool
	LockedUseSuppressed    bool
	LockedUseQuarantined   bool
	RequiresManualRecovery bool
	Stopping               bool
	Detail                 string
}

type ComputerUseReadinessHandler func(context.Context) ComputerUseReadiness

// ComputerUseReadinessHost lets the API server inject the helper's
// authoritative runtime status without coupling a provider to the helper
// transport. The handler grants no Computer Use authority.
type ComputerUseReadinessHost interface {
	SetComputerUseReadinessHandler(ComputerUseReadinessHandler)
}

// ClaudeControlRouteCommitHandler is the synchronous durability barrier before
// Claude Desktop or CLI performs its first external side effect. The server
// fixes provider identity, accepts only an exact stored logical session, and
// persists the selected route as committed before returning success.
type ClaudeControlRouteCommitHandler func(context.Context, string, string) error

// ClaudeControlRouteCommitHost is intentionally separate from computer-use
// tools: route ownership is durable session metadata, not a model capability
// and not authority to mutate the desktop.
type ClaudeControlRouteCommitHost interface {
	SetClaudeControlRouteCommitHandler(ClaudeControlRouteCommitHandler)
}

// ClaudeControlStartOptionsHost restores the complete create-time contract for
// a logical Claude session. It is intentionally separate from route binding so
// restarts cannot drop mode/model/effort while retaining only cwd.
type ClaudeControlStartOptionsHost interface {
	BindClaudeControlStartOptions(string, StartOptions)
}

type SendResult struct {
	OK              bool    `json:"ok"`
	State           string  `json:"state"`
	Message         string  `json:"message"`
	Error           *string `json:"error"`
	NativeTaskID    string  `json:"native_task_id,omitempty"`
	NativeSessionID string  `json:"native_session_id,omitempty"`
}

// Attachment is an uploaded file that has already been validated and stored
// by the HTTP boundary. Providers receive the private local path only; the PWA
// sees the opaque ID and display name.
type Attachment struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Path      string `json:"-"`
	MediaType string `json:"media_type"`
	Size      int64  `json:"size"`
}

type AttachmentSender interface {
	SendPromptWithAttachments(sessionID string, prompt string, attachments []Attachment) SendResult
}

// PromptOperationSender receives the API task id as a stable, restart-safe
// operation identity. Providers that can deliver through more than one native
// route use it to durably reject a duplicate prompt after an uncertain return.
type PromptOperationSender interface {
	SendPromptOperation(sessionID string, prompt string, operationID string) SendResult
}

type PromptOperationAttachmentSender interface {
	SendPromptOperationWithAttachments(
		sessionID string, prompt string, attachments []Attachment, operationID string,
	) SendResult
}

// QuestionAnswer preserves option identity without flattening a multi-select
// into a comma-delimited string (option labels are allowed to contain commas).
// Other is separate so a provider can address the native free-text control
// rather than guessing whether an unmatched label was an option or free text.
type QuestionAnswer struct {
	Selected []string `json:"selected,omitempty"`
	Other    string   `json:"other,omitempty"`
}

// StructuredQuestionAnswerer is optional. The API prefers it when the PWA
// submits answer_items and falls back to the legacy string map only for a
// provider that has not implemented exact multi-select semantics.
type StructuredQuestionAnswerer interface {
	AnswerQuestionStructured(sessionID string, requestID string, answers map[string]QuestionAnswer) map[string]any
}

type SessionAsset struct {
	MediaType string
	Data      []byte
}

// SessionAssetReader resolves only assets already referenced by the selected
// transcript. This keeps transcript images available without exposing paths or
// embedding large base64 strings in /session_preview.
type SessionAssetReader interface {
	ReadSessionAsset(sessionID string, assetID string) (SessionAsset, bool, error)
}

type Status struct {
	ProviderID   string          `json:"provider_id"`
	AppName      string          `json:"app_name"`
	IsRunning    bool            `json:"is_running"`
	IsFrontmost  bool            `json:"is_frontmost"`
	Installed    bool            `json:"installed"`
	State        string          `json:"state"`
	LastError    *string         `json:"last_error"`
	Capabilities map[string]bool `json:"capabilities"`
	Backend      string          `json:"backend"`
	Command      string          `json:"command,omitempty"`
	Cwd          string          `json:"cwd,omitempty"`
	Account      map[string]any  `json:"account,omitempty"`
}

// InstallChecker is implemented by providers that can tell whether their
// underlying app/CLI actually exists on this device. Providers without it
// are always treated as installed.
type InstallChecker interface {
	Installed() bool
}

type ModelSelect struct {
	Models        []ModelOption `json:"models"`
	Efforts       []string      `json:"efforts"`
	CurrentModel  *string       `json:"current_model"`
	CurrentEffort *string       `json:"current_effort"`
	Mode          string        `json:"mode"`
	Modes         []ModeOption  `json:"modes"`
	Note          string        `json:"note,omitempty"`
}

type ModelOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type ModeOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type Provider interface {
	ID() string
	Status() Status
	ModelSelect() ModelSelect
	ListNativeSessions() []map[string]any
	SessionMessages(sessionID string) ([]map[string]any, error)
	SessionModel(sessionID string) map[string]any
	ReferencedFiles(sessionID string) map[string]bool
	OpenOrCreateSession(sessionID string, opts StartOptions) (string, error)
	CloseSession(sessionID string) map[string]any
	SendPrompt(sessionID string, prompt string) SendResult
	LatestOutput(sessionID string) map[string]any
	DetectState(sessionID string) string
	RelayApproval(sessionID string, decision string) map[string]any
	SendKeys(sessionID string, keys []string) map[string]any
	Interrupt(sessionID string) map[string]any
	SetSessionModel(sessionID string, model string, effort string) map[string]any
}

type StartOptions struct {
	Cwd    string
	Model  string
	Effort string
	Mode   string
}

type RewindUserMessageOptions struct {
	SessionID string
	ThreadID  string
	TurnID    string
	Prompt    string
	Cwd       string
}

type RewindUserMessageResult struct {
	SessionID    string
	ThreadID     string
	TurnID       string
	State        string
	Message      string
	NativeTaskID string
}

type UserMessageRewinder interface {
	RewindUserMessage(opts RewindUserMessageOptions) (RewindUserMessageResult, error)
}

type Registry map[string]Provider

func BuildRegistry(cfg *config.Config) Registry {
	reg := Registry{}
	for id, pc := range cfg.Providers {
		// Product surfaces and compatibility ids are aliases of one canonical
		// owner. Never let config register a second mutable provider for them.
		if id == "claude" || CanonicalProviderID(id) != id {
			continue
		}
		if id == "codex" {
			reg[id] = NewCodex(id, pc)
			continue
		}
		if id == "catpaw" {
			reg[id] = NewCatPaw(id, pc)
			continue
		}
		if pc.Type == "pty" {
			if pc.AppName == "" {
				pc.AppName = id
			}
			reg[id] = NewPTYProvider(id, pc)
		}
	}
	// Claude Desktop and standalone CLI transcripts share the same Claude
	// session id. Expose one provider so discovery, stored records, streaming,
	// approvals, questions, and interrupts cannot split across owners. Desktop
	// Computer Use is primary; structured stream-json CLI is only the explicit
	// fresh-session, pre-mutation fallback.
	pc, ok := cfg.Providers["claude"]
	if !ok {
		pc, ok = cfg.Providers["claude_cli"]
	}
	if !ok {
		pc = config.ProviderConfig{Command: "claude", Cwd: "~/Developer"}
	}
	if pc.AppName == "" || pc.AppName == "Claude Desktop" || pc.AppName == "Claude CLI" || pc.AppName == "Claude CLI (tmux)" || pc.AppName == "Claude Code CLI" {
		pc.AppName = "Claude"
	}
	reg["claude"] = NewClaude("claude", pc)
	if _, ok := reg["codex"]; !ok {
		reg["codex"] = NewCodex("codex", config.ProviderConfig{AppName: "Codex", Command: "codex", Cwd: "~/Developer"})
	}
	return reg
}

func (r Registry) IDs() []string {
	ids := make([]string, 0, len(r))
	for id := range r {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		order := map[string]int{"codex": 0, "claude": 1, "deepseek": 2, "catpaw": 3}
		oi, okI := order[ids[i]]
		oj, okJ := order[ids[j]]
		if okI && okJ && oi != oj {
			return oi < oj
		}
		if okI != okJ {
			return okI
		}
		return ids[i] < ids[j]
	})
	return ids
}

// Shutdown closes every provider-owned background process and connection.
// Providers use an optional lifecycle interface so the public delivery
// contract does not force implementations to allocate long-lived resources.
func (r Registry) Shutdown() {
	for _, id := range r.IDs() {
		switch p := r[id].(type) {
		case interface{ Shutdown() }:
			p.Shutdown()
		case interface{ StopCLIStream() }:
			p.StopCLIStream()
		}
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func stringIn(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

package provider

import (
	"sort"

	"github.com/psyche08/remote-agent/internal/config"
)

type SendResult struct {
	OK           bool    `json:"ok"`
	State        string  `json:"state"`
	Message      string  `json:"message"`
	Error        *string `json:"error"`
	NativeTaskID string  `json:"native_task_id,omitempty"`
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
		if id == "claude" || id == "claude_cli" || id == "claude_desktop" {
			continue
		}
		if id == "codex" {
			reg[id] = NewCodex(id, pc)
		}
	}
	// Claude Desktop and standalone CLI transcripts share the same Claude
	// session id. Expose one provider so discovery, stored records, streaming,
	// approvals, questions, and interrupts cannot split across owners. The
	// Desktop database contributes metadata only; control is stream-json CLI.
	pc, ok := cfg.Providers["claude_cli"]
	if !ok {
		pc, ok = cfg.Providers["claude"]
	}
	if !ok {
		pc = config.ProviderConfig{Command: "claude", Cwd: "~/Developer"}
	}
	if pc.AppName == "" || pc.AppName == "Claude Desktop" || pc.AppName == "Claude CLI" || pc.AppName == "Claude CLI (tmux)" || pc.AppName == "Claude Code CLI" {
		pc.AppName = "Claude"
	}
	reg["claude"] = NewClaudeCLI("claude", pc)
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
		order := map[string]int{"codex": 0, "claude": 1}
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

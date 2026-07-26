package provider

// ActionID is a stable machine-readable operation name. The legacy boolean
// capabilities remain available for old consoles; new clients can use this
// typed list to render only operations whose scope and risk they understand.
type ActionID string

const (
	ActionSendPrompt ActionID = "prompt.send"
	ActionCreate     ActionID = "session.create"
	ActionResume     ActionID = "session.resume"
	ActionClose      ActionID = "session.close"
	ActionInterrupt  ActionID = "turn.interrupt"
	ActionSteer      ActionID = "turn.steer"
	ActionApproval   ActionID = "approval.respond"
	ActionQuestion   ActionID = "question.respond"
	ActionRawKeys    ActionID = "terminal.keys"
	ActionSetModel   ActionID = "session.model.set"
	ActionUpload     ActionID = "attachment.upload"
	ActionRewind     ActionID = "message.rewind"
)

type ActionScope string

const (
	ActionScopeProvider ActionScope = "provider"
	ActionScopeSession  ActionScope = "session"
	ActionScopeRequest  ActionScope = "request"
)

type ActionRisk string

const (
	ActionRiskSafe         ActionRisk = "safe"
	ActionRiskInterruptive ActionRisk = "interruptive"
	ActionRiskDestructive  ActionRisk = "destructive"
)

type ActionCapability struct {
	ID        ActionID    `json:"id"`
	Endpoint  string      `json:"endpoint"`
	Scope     ActionScope `json:"scope"`
	Risk      ActionRisk  `json:"risk"`
	Supported bool        `json:"supported"`
}

// Actions exposes a closed, typed action vocabulary. Support is derived from
// each provider's runtime capabilities and optional structured interfaces so
// an unstructured PTY fallback cannot accidentally inherit approval, steer,
// attachment, or model controls.
func Actions(p Provider) []ActionCapability {
	status := p.Status()
	capability := func(name string) bool { return status.Capabilities[name] }
	model := p.ModelSelect()
	_, supportsAttachments := p.(AttachmentSender)
	_, supportsRewind := p.(UserMessageRewinder)
	_, supportsResume := p.(interface {
		OpenResumeSession(sessionID string, resumeID string, cwd string, fork bool) (string, error)
	})
	_, supportsQuestion := p.(interface {
		AnswerQuestion(sessionID string, requestID string, answers map[string]string) map[string]any
	})
	return []ActionCapability{
		{ID: ActionSendPrompt, Endpoint: "/send_prompt", Scope: ActionScopeSession, Risk: ActionRiskSafe, Supported: true},
		{ID: ActionCreate, Endpoint: "/sessions", Scope: ActionScopeProvider, Risk: ActionRiskSafe, Supported: capability("create_session")},
		{ID: ActionResume, Endpoint: "/resume_native_session", Scope: ActionScopeSession, Risk: ActionRiskSafe, Supported: supportsResume},
		{ID: ActionClose, Endpoint: "/close_session", Scope: ActionScopeSession, Risk: ActionRiskDestructive, Supported: true},
		{ID: ActionInterrupt, Endpoint: "/interrupt", Scope: ActionScopeSession, Risk: ActionRiskInterruptive, Supported: capability("interrupt")},
		{ID: ActionSteer, Endpoint: "/steer", Scope: ActionScopeSession, Risk: ActionRiskSafe, Supported: capability("steer")},
		{ID: ActionApproval, Endpoint: "/approval", Scope: ActionScopeRequest, Risk: ActionRiskSafe, Supported: capability("approval")},
		{ID: ActionQuestion, Endpoint: "/question_answer", Scope: ActionScopeRequest, Risk: ActionRiskSafe, Supported: supportsQuestion},
		{ID: ActionRawKeys, Endpoint: "/keys", Scope: ActionScopeSession, Risk: ActionRiskInterruptive, Supported: capability("raw_keys")},
		{ID: ActionSetModel, Endpoint: "/set_model", Scope: ActionScopeSession, Risk: ActionRiskSafe, Supported: len(model.Models) > 0 || len(model.Efforts) > 0},
		{ID: ActionUpload, Endpoint: "/upload", Scope: ActionScopeSession, Risk: ActionRiskSafe, Supported: supportsAttachments},
		{ID: ActionRewind, Endpoint: "/rewind_user_message", Scope: ActionScopeSession, Risk: ActionRiskDestructive, Supported: supportsRewind},
	}
}

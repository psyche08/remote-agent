package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const codexComputerUseNamespace = "computer_use"

type codexComputerUseTurnKey struct {
	Generation uint64
	ThreadID   string
	TurnID     string
}

func (c *Codex) SetComputerUseToolHandler(handler ComputerUseToolHandler) {
	c.computerUseMu.Lock()
	defer c.computerUseMu.Unlock()
	c.computerUseToolHandler = handler
	if handler == nil {
		// Threads created without an installed in-process broker must remain
		// fail-closed even if a later connection sends a forged tool request.
		clear(c.computerUseThreads)
		clear(c.computerUseInspected)
	}
}

func (c *Codex) computerUseDynamicTools() any {
	c.computerUseMu.Lock()
	enabled := c.computerUseToolHandler != nil
	c.computerUseMu.Unlock()
	if !enabled {
		return nil
	}

	appTarget := map[string]any{
		"app": map[string]any{
			"type": "string", "description": "Application name, for example TextEdit.",
		},
		"bundle_id": map[string]any{
			"type": "string", "description": "Application bundle identifier. Prefer this when known.",
		},
	}
	pathTarget := cloneComputerUseProperties(appTarget)
	pathTarget["path"] = map[string]any{
		"type": "array", "items": map[string]any{"type": "integer", "minimum": 0},
		"minItems": 1, "maxItems": 40,
		"description": "Accessibility element index path returned by get_app_state.",
	}

	return []map[string]any{{
		"type":        "namespace",
		"name":        codexComputerUseNamespace,
		"description": "Operate the Mac UI for this exact active model turn. Call get_app_state before every other computer_use tool in a turn.",
		"tools": []map[string]any{
			codexComputerUseFunction(
				"get_app_state",
				"Establish the turn's safe control mode (a single Locked Use window when configured, otherwise a verified ordinary-unlocked session), then return a fresh screenshot and the target application's Accessibility tree. This must be the first computer_use call in every assistant turn. Supply app or bundle_id.",
				appTarget,
				nil,
			),
			codexComputerUseFunction(
				"press",
				"Press one actionable Accessibility element from the latest get_app_state tree.",
				pathTarget,
				[]string{"path"},
			),
			codexComputerUseFunction(
				"set_value",
				"Set the value of one editable Accessibility element from the latest get_app_state tree.",
				withComputerUseProperty(pathTarget, "value", map[string]any{"type": "string"}),
				[]string{"path", "value"},
			),
			codexComputerUseFunction(
				"click",
				"Click screenshot coordinates. Accessibility press is preferred when an element path is available.",
				map[string]any{
					"x":      map[string]any{"type": "integer", "minimum": 0, "maximum": 32767},
					"y":      map[string]any{"type": "integer", "minimum": 0, "maximum": 32767},
					"button": map[string]any{"type": "string", "enum": []string{"left", "right", "middle"}},
					"count":  map[string]any{"type": "integer", "minimum": 1, "maximum": 3},
				},
				[]string{"x", "y"},
			),
			codexComputerUseFunction(
				"type_text",
				"Type text through the desktop input channel.",
				map[string]any{"text": map[string]any{"type": "string", "minLength": 1, "maxLength": 4096}},
				[]string{"text"},
			),
			codexComputerUseFunction(
				"press_key",
				"Press a key or key chord, such as [\"cmd\", \"s\"].",
				map[string]any{"keys": map[string]any{
					"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1, "maxItems": 5,
				}},
				[]string{"keys"},
			),
			codexComputerUseFunction(
				"scroll",
				"Scroll at screenshot coordinates by a non-zero delta_x or delta_y.",
				map[string]any{
					"x":       map[string]any{"type": "integer", "minimum": 0, "maximum": 32767},
					"y":       map[string]any{"type": "integer", "minimum": 0, "maximum": 32767},
					"delta_x": map[string]any{"type": "integer", "minimum": -4096, "maximum": 4096},
					"delta_y": map[string]any{"type": "integer", "minimum": -4096, "maximum": 4096},
				},
				[]string{"x", "y"},
			),
		},
	}}
}

func codexComputerUseFunction(name, description string, properties map[string]any, required []string) map[string]any {
	schema := map[string]any{
		"type":                 "object",
		"properties":           cloneComputerUseProperties(properties),
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return map[string]any{
		"type": "function", "name": name, "description": description,
		"inputSchema": schema,
	}
}

func cloneComputerUseProperties(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func withComputerUseProperty(in map[string]any, name string, value any) map[string]any {
	out := cloneComputerUseProperties(in)
	out[name] = value
	return out
}

func codexTurnStartOptions(threadStart map[string]any) map[string]any {
	out := make(map[string]any, len(threadStart))
	for key, value := range threadStart {
		if key != "dynamicTools" {
			out[key] = value
		}
	}
	return out
}

func (c *Codex) enableComputerUseThread(generation uint64, threadID string) {
	if generation == 0 || threadID == "" || !c.isCurrentClientGeneration(generation) {
		return
	}
	c.computerUseMu.Lock()
	defer c.computerUseMu.Unlock()
	if c.computerUseToolHandler != nil {
		c.computerUseThreads[threadID] = generation
	}
}

// forgetComputerUseTurn is called while runtime state is being retired. It
// removes the read-before-mutate capability so a reused or stale turn id cannot
// carry inspection authority into another model turn.
func (c *Codex) forgetComputerUseTurn(threadID, turnID string) {
	c.computerUseMu.Lock()
	defer c.computerUseMu.Unlock()
	for key := range c.computerUseInspected {
		if turnID != "" {
			if key.TurnID == turnID && (threadID == "" || key.ThreadID == threadID) {
				delete(c.computerUseInspected, key)
			}
			continue
		}
		if threadID != "" && key.ThreadID == threadID {
			delete(c.computerUseInspected, key)
		}
	}
}

func (c *Codex) clearComputerUseGeneration(generation uint64) {
	c.computerUseMu.Lock()
	defer c.computerUseMu.Unlock()
	for threadID, ownerGeneration := range c.computerUseThreads {
		if ownerGeneration == generation {
			delete(c.computerUseThreads, threadID)
		}
	}
	for key := range c.computerUseInspected {
		if key.Generation == generation {
			delete(c.computerUseInspected, key)
		}
	}
}

func (c *Codex) publishComputerUseTerminal(threadID, message string) {
	if c.streamPublisher == nil || threadID == "" {
		return
	}
	frame := map[string]any{"type": "error", "error": message}
	targets := []string{}
	if sessionID := c.sessionForThread(threadID); sessionID != "" {
		targets = append(targets, sessionID)
	}
	if !stringIn(targets, threadID) {
		targets = append(targets, threadID)
	}
	for _, target := range targets {
		c.streamPublisher(target, frame)
	}
}

func (c *Codex) answerComputerUseDynamicTool(
	client codexAppClient,
	generation uint64,
	requestID any,
	params map[string]any,
) error {
	request, key, handler, err := c.authorizeComputerUseDynamicTool(generation, params)
	if err != nil {
		return respondCodexComputerUseFailure(client, requestID, err)
	}

	result, err := handler(context.Background(), request)
	if err != nil {
		return respondCodexComputerUseFailure(client, requestID, err)
	}
	textEmpty := strings.TrimSpace(result.Text) == ""
	if request.Tool == "get_app_state" && textEmpty {
		return respondCodexComputerUseFailure(client, requestID, errors.New("get_app_state returned no accessibility state"))
	}
	if textEmpty {
		result.Text = `{"ok":true}`
	}
	if result.ImageURL != "" && !strings.HasPrefix(result.ImageURL, "data:image/png;base64,") {
		return respondCodexComputerUseFailure(client, requestID, errors.New("computer-use broker returned an invalid image"))
	}
	if request.Tool == "get_app_state" {
		if result.ImageURL == "" {
			return respondCodexComputerUseFailure(client, requestID, errors.New("get_app_state returned no screenshot"))
		}
		if !c.markComputerUseInspected(key, codexComputerUseTarget(request)) {
			return respondCodexComputerUseFailure(client, requestID, errors.New("computer-use turn ended before inspection completed"))
		}
	} else if result.ImageURL != "" {
		return respondCodexComputerUseFailure(client, requestID, errors.New("computer-use mutation returned unexpected image content"))
	}

	items := []map[string]any{{"type": "inputText", "text": result.Text}}
	if result.ImageURL != "" {
		items = append(items, map[string]any{"type": "inputImage", "imageUrl": result.ImageURL})
	}
	err = client.Respond(requestID, map[string]any{"success": true, "contentItems": items})
	if err != nil && request.Tool == "get_app_state" {
		c.clearComputerUseInspected(key)
	}
	return err
}

func (c *Codex) authorizeComputerUseDynamicTool(
	generation uint64,
	params map[string]any,
) (ComputerUseToolRequest, codexComputerUseTurnKey, ComputerUseToolHandler, error) {
	threadID := strings.TrimSpace(stringAny(params["threadId"]))
	turnID := strings.TrimSpace(stringAny(params["turnId"]))
	callID := strings.TrimSpace(stringAny(params["callId"]))
	tool := strings.TrimSpace(stringAny(params["tool"]))
	key := codexComputerUseTurnKey{Generation: generation, ThreadID: threadID, TurnID: turnID}
	if generation == 0 || threadID == "" || turnID == "" || callID == "" {
		return ComputerUseToolRequest{}, key, nil, errors.New("computer-use request is missing authoritative turn identity")
	}
	if !codexComputerUseMutation(tool) && tool != "get_app_state" {
		return ComputerUseToolRequest{}, key, nil, fmt.Errorf("unsupported computer-use tool: %s", tool)
	}

	now := time.Now()
	c.runtimeMu.Lock()
	activeAt, active := c.activeThreads[threadID]
	_, interrupting := c.interruptingThreads[threadID]
	validRuntime := c.turnThreads[turnID] == threadID && active &&
		!codexActiveExpired(now, activeAt) && c.appServerThreads[threadID] &&
		!interrupting
	c.computerUseMu.Lock()
	handler := c.computerUseToolHandler
	advertised := c.computerUseThreads[threadID] == generation
	inspectedTarget := c.computerUseInspected[key]
	c.computerUseMu.Unlock()
	c.runtimeMu.Unlock()
	if !validRuntime || !advertised || handler == nil || !c.isCurrentClientGeneration(generation) {
		return ComputerUseToolRequest{}, key, nil, errors.New("computer-use tool is unavailable for this active turn")
	}
	if codexComputerUseMutation(tool) && inspectedTarget == "" {
		return ComputerUseToolRequest{}, key, nil, errors.New("get_app_state must succeed before mutating the UI in this turn")
	}

	args := mapAny(params["arguments"])
	if args == nil {
		return ComputerUseToolRequest{}, key, nil, errors.New("computer-use arguments must be an object")
	}
	request, err := parseCodexComputerUseRequest(tool, args)
	if err != nil {
		return ComputerUseToolRequest{}, key, nil, err
	}
	if target := codexComputerUseTarget(request); codexComputerUseAXMutation(tool) && target != inspectedTarget {
		return ComputerUseToolRequest{}, key, nil, errors.New(
			"get_app_state must inspect the same application before an Accessibility mutation")
	}
	// A screenshot/AX tree is a single-use observation capability. Consuming it
	// before dispatch also serializes concurrent mutations: at most one can act
	// on a given snapshot, and even a broker error leaves the UI state uncertain
	// enough that the model must inspect again.
	if codexComputerUseMutation(tool) && !c.consumeComputerUseInspection(key, inspectedTarget) {
		return ComputerUseToolRequest{}, key, nil, errors.New(
			"get_app_state must succeed again before the next UI mutation")
	}
	request.ProviderID = c.id
	request.ThreadID = threadID
	request.SessionID = firstNonEmpty(c.sessionForThread(threadID), threadID)
	request.TurnID = turnID
	request.CallID = callID
	return request, key, handler, nil
}

func codexComputerUseMutation(tool string) bool {
	switch tool {
	case "press", "set_value", "click", "type_text", "press_key", "scroll":
		return true
	default:
		return false
	}
}

func codexComputerUseAXMutation(tool string) bool {
	return tool == "press" || tool == "set_value"
}

// AX index paths only have meaning inside the application tree that produced
// them. Bind inspection authority to the target, preferring bundle id exactly
// as the Swift resolver does, so a model cannot inspect one app and use the
// resulting turn-wide boolean as authority over another app.
func codexComputerUseTarget(request ComputerUseToolRequest) string {
	if bundleID := strings.ToLower(strings.TrimSpace(request.BundleID)); bundleID != "" {
		return "bundle:" + bundleID
	}
	if app := strings.ToLower(strings.TrimSpace(request.App)); app != "" {
		return "app:" + app
	}
	return ""
}

func (c *Codex) markComputerUseInspected(key codexComputerUseTurnKey, target string) bool {
	if target == "" {
		return false
	}
	if !c.isCurrentClientGeneration(key.Generation) {
		return false
	}
	c.runtimeMu.Lock()
	defer c.runtimeMu.Unlock()
	activeAt, active := c.activeThreads[key.ThreadID]
	_, interrupting := c.interruptingThreads[key.ThreadID]
	if !active || codexActiveExpired(time.Now(), activeAt) ||
		c.turnThreads[key.TurnID] != key.ThreadID || interrupting {
		return false
	}
	c.computerUseMu.Lock()
	defer c.computerUseMu.Unlock()
	if c.computerUseToolHandler == nil || c.computerUseThreads[key.ThreadID] != key.Generation {
		return false
	}
	c.computerUseInspected[key] = target
	return true
}

func (c *Codex) clearComputerUseInspected(key codexComputerUseTurnKey) {
	c.computerUseMu.Lock()
	delete(c.computerUseInspected, key)
	c.computerUseMu.Unlock()
}

func (c *Codex) consumeComputerUseInspection(key codexComputerUseTurnKey, target string) bool {
	c.computerUseMu.Lock()
	defer c.computerUseMu.Unlock()
	if target == "" || c.computerUseInspected[key] != target {
		return false
	}
	delete(c.computerUseInspected, key)
	return true
}

func respondCodexComputerUseFailure(client codexAppClient, requestID any, err error) error {
	message := "computer-use request refused"
	if err != nil && strings.TrimSpace(err.Error()) != "" {
		message = err.Error()
	}
	return client.Respond(requestID, map[string]any{
		"success":      false,
		"contentItems": []map[string]any{{"type": "inputText", "text": message}},
	})
}

func parseCodexComputerUseRequest(tool string, args map[string]any) (ComputerUseToolRequest, error) {
	request := ComputerUseToolRequest{Tool: tool}
	var err error
	request.App, err = optionalComputerUseString(args, "app", true)
	if err != nil {
		return ComputerUseToolRequest{}, err
	}
	request.BundleID, err = optionalComputerUseString(args, "bundle_id", true)
	if err != nil {
		return ComputerUseToolRequest{}, err
	}

	switch tool {
	case "get_app_state":
		if request.App == "" && request.BundleID == "" {
			return ComputerUseToolRequest{}, errors.New("get_app_state requires app or bundle_id")
		}
	case "press", "set_value":
		if request.App == "" && request.BundleID == "" {
			return ComputerUseToolRequest{}, fmt.Errorf("%s requires app or bundle_id", tool)
		}
		path, err := requiredComputerUseIntList(args, "path")
		if err != nil {
			return ComputerUseToolRequest{}, err
		}
		if len(path) == 0 || len(path) > 40 {
			return ComputerUseToolRequest{}, errors.New("path must contain 1..40 indices")
		}
		for _, index := range path {
			if index < 0 {
				return ComputerUseToolRequest{}, errors.New("path contains a negative index")
			}
		}
		request.Path = path
		if tool == "set_value" {
			value, err := requiredComputerUseString(args, "value", false)
			if err != nil {
				return ComputerUseToolRequest{}, err
			}
			request.Value = &value
		}
	case "click":
		x, err := requiredComputerUseInt(args, "x")
		if err != nil {
			return ComputerUseToolRequest{}, err
		}
		y, err := requiredComputerUseInt(args, "y")
		if err != nil {
			return ComputerUseToolRequest{}, err
		}
		if err := validateComputerUseScreenshotPoint(x, y); err != nil {
			return ComputerUseToolRequest{}, err
		}
		request.X, request.Y = &x, &y
		request.Button, err = optionalComputerUseString(args, "button", true)
		if err != nil {
			return ComputerUseToolRequest{}, err
		}
		if request.Button != "" && request.Button != "left" && request.Button != "right" && request.Button != "middle" {
			return ComputerUseToolRequest{}, errors.New("button must be left, right, or middle")
		}
		if _, ok := args["count"]; ok {
			request.Count, err = requiredComputerUseInt(args, "count")
			if err != nil || request.Count < 1 || request.Count > 3 {
				return ComputerUseToolRequest{}, errors.New("count must be an integer from 1 to 3")
			}
		}
	case "type_text":
		text, err := requiredComputerUseString(args, "text", false)
		if err != nil || text == "" {
			return ComputerUseToolRequest{}, errors.New("text must be a non-empty string")
		}
		request.Text = text
	case "press_key":
		keys, err := requiredComputerUseStringList(args, "keys")
		if err != nil || len(keys) == 0 || len(keys) > 5 {
			return ComputerUseToolRequest{}, errors.New("keys must contain 1..5 strings")
		}
		request.Keys = keys
	case "scroll":
		x, err := requiredComputerUseInt(args, "x")
		if err != nil {
			return ComputerUseToolRequest{}, err
		}
		y, err := requiredComputerUseInt(args, "y")
		if err != nil {
			return ComputerUseToolRequest{}, err
		}
		if err := validateComputerUseScreenshotPoint(x, y); err != nil {
			return ComputerUseToolRequest{}, err
		}
		request.X, request.Y = &x, &y
		if _, ok := args["delta_x"]; ok {
			request.DeltaX, err = requiredComputerUseInt(args, "delta_x")
			if err != nil {
				return ComputerUseToolRequest{}, err
			}
		}
		if _, ok := args["delta_y"]; ok {
			request.DeltaY, err = requiredComputerUseInt(args, "delta_y")
			if err != nil {
				return ComputerUseToolRequest{}, err
			}
		}
		if request.DeltaX == 0 && request.DeltaY == 0 {
			return ComputerUseToolRequest{}, errors.New("scroll requires a non-zero delta_x or delta_y")
		}
		if request.DeltaX < -4096 || request.DeltaX > 4096 || request.DeltaY < -4096 || request.DeltaY > 4096 {
			return ComputerUseToolRequest{}, errors.New("scroll delta must be within +/-4096")
		}
	default:
		return ComputerUseToolRequest{}, fmt.Errorf("unsupported computer-use tool: %s", tool)
	}
	return request, nil
}

func validateComputerUseScreenshotPoint(x, y int) error {
	if x < 0 || x > 32767 || y < 0 || y > 32767 {
		return errors.New("screenshot coordinates must be within 0..32767")
	}
	return nil
}

func optionalComputerUseString(args map[string]any, key string, trim bool) (string, error) {
	value, ok := args[key]
	if !ok || value == nil {
		return "", nil
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", key)
	}
	if trim {
		text = strings.TrimSpace(text)
	}
	return text, nil
}

func requiredComputerUseString(args map[string]any, key string, trim bool) (string, error) {
	text, err := optionalComputerUseString(args, key, trim)
	if err != nil {
		return "", err
	}
	if _, ok := args[key]; !ok {
		return "", fmt.Errorf("%s is required", key)
	}
	return text, nil
}

func requiredComputerUseInt(args map[string]any, key string) (int, error) {
	value, ok := args[key]
	if !ok {
		return 0, fmt.Errorf("%s is required", key)
	}
	number, ok := numberToInt64(value)
	if !ok || int64(int(number)) != number {
		return 0, fmt.Errorf("%s must be an integer", key)
	}
	return int(number), nil
}

func requiredComputerUseIntList(args map[string]any, key string) ([]int, error) {
	raw, ok := args[key]
	if !ok {
		return nil, fmt.Errorf("%s is required", key)
	}
	values := listAny(raw)
	if values == nil {
		return nil, fmt.Errorf("%s must be an integer array", key)
	}
	out := make([]int, 0, len(values))
	for _, value := range values {
		number, ok := numberToInt64(value)
		if !ok || int64(int(number)) != number {
			return nil, fmt.Errorf("%s must be an integer array", key)
		}
		out = append(out, int(number))
	}
	return out, nil
}

func requiredComputerUseStringList(args map[string]any, key string) ([]string, error) {
	raw, ok := args[key]
	if !ok {
		return nil, fmt.Errorf("%s is required", key)
	}
	values := listAny(raw)
	if values == nil {
		return nil, fmt.Errorf("%s must be a string array", key)
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		text, ok := value.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return nil, fmt.Errorf("%s must be a string array", key)
		}
		out = append(out, text)
	}
	return out, nil
}

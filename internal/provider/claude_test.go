package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/psyche08/remote-agent/internal/config"
)

func TestClaudeCLIUsesAutoModeByDefault(t *testing.T) {
	c := NewClaudeCLI("claude", config.ProviderConfig{Command: "/bin/echo"})
	if c.permissionMode != "auto" || c.preferDesktop {
		t.Fatalf("unexpected CLI defaults: permission=%q preferDesktop=%v", c.permissionMode, c.preferDesktop)
	}
	if got := strings.Join(c.permissionArgs("manual"), " "); got != "--permission-mode default" {
		t.Fatalf("legacy manual mode is not mapped to current CLI default: %q", got)
	}
}

func TestClaudeCLIStreamJSONLifecycleAndApproval(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args.txt")
	t.Setenv("CLAUDE_STREAM_ARGS_FILE", argsPath)
	c := NewClaudeCLI("claude", config.ProviderConfig{Command: writeClaudeStreamFixture(t, dir), Cwd: dir})
	t.Cleanup(c.StopCLIStream)
	frameSeen := make(chan struct{}, 1)
	c.SetStreamPublisher(func(target string, frame map[string]any) {
		if target == "logical-1" || target == "019ef769-7611-70e0-839a-283dc0e5f256" {
			select {
			case frameSeen <- struct{}{}:
			default:
			}
		}
	})
	transcriptID := "019ef769-7611-70e0-839a-283dc0e5f256"
	c.BindTranscript("logical-1", transcriptID)
	got, err := c.OpenOrCreateSession("logical-1", StartOptions{Cwd: dir, Mode: "edit", Model: "opus", Effort: "high"})
	if err != nil || got != transcriptID {
		t.Fatalf("open stream session: id=%q err=%v", got, err)
	}
	joined := string(waitForFile(t, argsPath))
	for _, want := range []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--permission-prompt-tool", "stdio", "--session-id", transcriptID, "--permission-mode", "acceptEdits", "--model", "opus", "--effort", "high"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("stream CLI args missing %q: %s", want, joined)
		}
	}
	res := c.SendPrompt("logical-1", "do not persist this prompt")
	if !res.OK || res.NativeTaskID != transcriptID {
		t.Fatalf("send stream prompt: %#v", res)
	}
	waitForClaudeState(t, c, "logical-1", "waiting_approval")
	approval := c.ApprovalRequest("logical-1")
	if approval["request_id"] != "request-1" || approval["source"] != "claude_cli_stream" {
		t.Fatalf("stream approval not scoped: %#v", approval)
	}
	second := c.SendPrompt("logical-1", "second owner")
	if second.OK || second.State != "running" {
		t.Fatalf("stream accepted a concurrent turn: %#v", second)
	}
	allowed := c.RelayApprovalRequest("logical-1", "request-1", "allow")
	if !boolAny(allowed["ok"]) {
		t.Fatalf("stream approval failed: %#v", allowed)
	}
	waitForClaudeState(t, c, "logical-1", "idle")
	output := c.LatestOutput("logical-1")
	if output["source"] != "claude_cli_stream" || !strings.Contains(stringAny(output["text"]), "hello from stream") {
		t.Fatalf("stream output missing: %#v", output)
	}
	select {
	case <-frameSeen:
	default:
		t.Fatal("stream publisher received no frames")
	}
	closed := c.CloseSession("logical-1")
	if !boolAny(closed["ok"]) || !boolAny(closed["killed"]) {
		t.Fatalf("close stream session: %#v", closed)
	}
}

func TestClaudeCLIStreamJSONResumeAndInterrupt(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args.txt")
	t.Setenv("CLAUDE_STREAM_ARGS_FILE", argsPath)
	c := NewClaudeCLI("claude", config.ProviderConfig{
		Command: writeClaudeStreamFixture(t, dir), Cwd: dir,
		Extra: map[string]any{"turnstate_dir": filepath.Join(dir, "turnstate")},
	})
	t.Cleanup(c.StopCLIStream)
	transcriptID := "019ef769-7611-70e0-839a-283dc0e5f256"
	got, err := c.OpenResumeSession("logical-resume", transcriptID, dir, false)
	if err != nil || got != transcriptID {
		t.Fatalf("resume stream session: id=%q err=%v", got, err)
	}
	args := waitForFile(t, argsPath)
	if !strings.Contains(string(args), "--resume\n"+transcriptID) || strings.Contains(string(args), "--session-id") {
		t.Fatalf("bad resume args: %s", args)
	}
	res := c.Interrupt("logical-resume")
	if !boolAny(res["ok"]) || !strings.Contains(stringAny(res["detail"]), "acknowledged") {
		t.Fatalf("stream interrupt failed: %#v", res)
	}
	waitForClaudeState(t, c, "logical-resume", "idle")
}

func TestClaudeCLIStreamJSONDenialAndQuestionResponses(t *testing.T) {
	dir := t.TempDir()
	c := NewClaudeCLI("claude", config.ProviderConfig{Command: writeClaudeStreamFixture(t, dir), Cwd: dir})
	t.Cleanup(c.StopCLIStream)
	transcriptID := "019ef769-7611-70e0-839a-283dc0e5f256"
	if _, err := c.OpenOrCreateSession(transcriptID, StartOptions{}); err != nil {
		t.Fatal(err)
	}
	c.onCLIStreamEvent(claudeRuntimeSession{SessionID: transcriptID}, json.RawMessage(`{
		"type":"control_request","request_id":"deny-1","request":{
			"subtype":"can_use_tool","tool_name":"Write","tool_use_id":"tool-deny",
			"input":{"file_path":"/tmp/example"}
		}}`))
	denied := c.RelayApprovalRequest(transcriptID, "deny-1", "deny")
	if !boolAny(denied["ok"]) || c.pendingControl(transcriptID, "deny-1") != nil {
		t.Fatalf("stream denial failed: %#v", denied)
	}
	waitForClaudeState(t, c, transcriptID, "idle")
	c.onCLIStreamEvent(claudeRuntimeSession{SessionID: transcriptID}, json.RawMessage(`{
		"type":"control_request","request_id":"question-1","request":{
			"subtype":"can_use_tool","tool_name":"AskUserQuestion","tool_use_id":"tool-question",
			"input":{"questions":[
				{"header":"Mode","question":"Which mode?","multiSelect":false,"options":[{"label":"Safe"},{"label":"Fast"}]},
				{"header":"Checks","question":"Which checks?","multiSelect":true,"options":[{"label":"Tests"},{"label":"Lint"}]}
			]}
		}}`))
	question := c.PendingQuestion(transcriptID)
	if question == nil || question["source"] != "claude_cli_stream" || question["request_id"] != "question-1" {
		t.Fatalf("stream question missing: %#v", question)
	}
	answered := c.AnswerQuestion(transcriptID, "question-1", map[string]string{
		"Which mode?": "Safe", "Which checks?": "Tests, Other: security",
	})
	if !boolAny(answered["ok"]) || c.pendingControl(transcriptID, "question-1") != nil {
		t.Fatalf("stream question response failed: %#v", answered)
	}
}

func TestClaudeCLIStreamPublishesNormalizedFramesToBoundSession(t *testing.T) {
	c := NewClaudeCLI("claude", config.ProviderConfig{})
	c.BindTranscript("logical-1", "session-1")
	frames := map[string][]map[string]any{}
	c.SetStreamPublisher(func(target string, frame map[string]any) {
		encoded, _ := json.Marshal(frame)
		var snapshot map[string]any
		_ = json.Unmarshal(encoded, &snapshot)
		frames[target] = append(frames[target], snapshot)
	})
	session := claudeRuntimeSession{SessionID: "session-1", Cwd: "/repo"}
	for _, raw := range []string{
		`{"type":"stream_event","event":{"type":"message_start"}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"hello"},{"type":"tool_use","id":"tool-1","name":"Read","input":{"file_path":"main.go"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tool-1","content":"file data"}]}}`,
		`{"type":"result","result":"done"}`,
	} {
		c.onCLIStreamEvent(session, json.RawMessage(raw))
	}
	for _, target := range []string{"session-1", "logical-1"} {
		got := frames[target]
		if len(got) != 5 {
			t.Fatalf("target %s frames=%#v", target, got)
		}
		if got[0]["type"] != "turn" || got[0]["status"] != "started" || got[1]["type"] != "delta" || got[1]["text"] != "hello" {
			t.Fatalf("target %s missing lifecycle/delta: %#v", target, got)
		}
		tool := mapAny(got[2]["item"])
		if got[2]["type"] != "item" || tool["kind"] != "tool" || tool["name"] != "Read" {
			t.Fatalf("target %s missing tool item: %#v", target, got[2])
		}
		if files := listAny(tool["files"]); len(files) != 1 || stringAny(files[0]) != "/repo/main.go" {
			t.Fatalf("target %s tool files=%#v", target, tool)
		}
		if update := mapAny(got[3]["item"]); got[3]["type"] != "item_update" || update["result"] != "file data" {
			t.Fatalf("target %s missing tool result: %#v", target, got[3])
		}
		if got[4]["type"] != "turn" || got[4]["status"] != "completed" {
			t.Fatalf("target %s missing completion: %#v", target, got[4])
		}
	}
}

func TestClaudeCLIStreamJSONRestartsCompletedProcessWithResume(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args.txt")
	t.Setenv("CLAUDE_STREAM_ARGS_FILE", argsPath)
	t.Setenv("CLAUDE_STREAM_ARGS_APPEND", "1")
	t.Setenv("CLAUDE_STREAM_EXIT_AFTER_RESULT", "1")
	c := NewClaudeCLI("claude", config.ProviderConfig{Command: writeClaudeStreamFixture(t, dir), Cwd: dir})
	t.Cleanup(c.StopCLIStream)
	transcriptID := "019ef769-7611-70e0-839a-283dc0e5f256"
	c.BindTranscript("logical-restart", transcriptID)
	if _, err := c.OpenOrCreateSession("logical-restart", StartOptions{}); err != nil {
		t.Fatal(err)
	}
	if res := c.SendPrompt("logical-restart", "first turn"); !res.OK {
		t.Fatalf("first prompt failed: %#v", res)
	}
	waitForClaudeState(t, c, "logical-restart", "waiting_approval")
	if res := c.RelayApprovalRequest("logical-restart", "request-1", "allow"); !boolAny(res["ok"]) {
		t.Fatalf("approval failed: %#v", res)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && c.cliStream.HasSession("logical-restart") {
		time.Sleep(10 * time.Millisecond)
	}
	if c.cliStream.HasSession("logical-restart") {
		t.Fatal("fixture process did not exit")
	}
	if res := c.SendPrompt("logical-restart", "second turn"); !res.OK {
		t.Fatalf("second prompt failed: %#v", res)
	}
	args := string(waitForFile(t, argsPath))
	deadline = time.Now().Add(3 * time.Second)
	for !strings.Contains(args, "--resume\n"+transcriptID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		args = string(waitForFile(t, argsPath))
	}
	if !strings.Contains(args, "--resume\n"+transcriptID) {
		t.Fatalf("completed process was not resumed: %s", args)
	}
}

func writeClaudeStreamFixture(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-claude-stream")
	script := `#!/bin/sh
if [ -n "$CLAUDE_STREAM_ARGS_FILE" ]; then
  if [ "$CLAUDE_STREAM_ARGS_APPEND" = "1" ]; then
    printf '%s\n' launch "$@" >> "$CLAUDE_STREAM_ARGS_FILE"
  else
    printf '%s\n' "$@" > "$CLAUDE_STREAM_ARGS_FILE"
  fi
fi
sid=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "--session-id" ] || [ "$prev" = "--resume" ]; then sid="$arg"; fi
  prev="$arg"
done
printf '{"type":"system","subtype":"init","session_id":"%s"}\n' "$sid"
while IFS= read -r line; do
  case "$line" in
    *'"subtype":"initialize"'*)
      rid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
      printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$rid"
      ;;
    *'"subtype":"interrupt"'*)
      rid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
      printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$rid"
      printf '{"type":"result","session_id":"%s","result":"interrupted"}\n' "$sid"
      ;;
    *'"type":"control_response"'*)
      printf '{"type":"result","session_id":"%s","result":"done"}\n' "$sid"
      if [ "$CLAUDE_STREAM_EXIT_AFTER_RESULT" = "1" ]; then exit 0; fi
      ;;
    *)
      printf '{"type":"stream_event","session_id":"%s","event":{"type":"message_start"}}\n' "$sid"
      printf '{"type":"stream_event","session_id":"%s","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello from stream"}}}\n' "$sid"
      printf '{"type":"control_request","session_id":"%s","request_id":"request-1","request":{"subtype":"can_use_tool","tool_name":"Bash","tool_use_id":"tool-1","input":{"command":"true"},"permission_suggestions":[]}}\n' "$sid"
      ;;
  esac
done
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func waitForClaudeState(t *testing.T, c *Claude, sessionID string, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := c.DetectState(sessionID); got == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Claude state=%q want %q", c.DetectState(sessionID), want)
}

func waitForFile(t *testing.T, path string) []byte {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			return data
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
	return nil
}

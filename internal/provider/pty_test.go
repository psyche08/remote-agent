package provider

import (
	"strings"
	"testing"
	"time"

	"github.com/psyche08/remote-agent/internal/config"
)

func TestBuildRegistryAddsCapabilityLimitedPTYProvider(t *testing.T) {
	registry := BuildRegistry(&config.Config{Providers: map[string]config.ProviderConfig{
		"terminal-agent": {
			Type: "pty", AppName: "Terminal Agent", Command: "/bin/sh", Cwd: t.TempDir(),
			Extra: map[string]any{"allow_raw_keys": true},
		},
	}})
	t.Cleanup(registry.Shutdown)
	generic, ok := registry["terminal-agent"].(*PTYProvider)
	if !ok {
		t.Fatalf("PTY provider was not registered: %#v", registry["terminal-agent"])
	}
	status := generic.Status()
	if status.Backend != "generic_pty" || !status.Capabilities["pty"] || !status.Capabilities["raw_keys"] {
		t.Fatalf("unexpected PTY status: %#v", status)
	}
	if status.Capabilities["approval"] || status.Capabilities["steer"] {
		t.Fatalf("PTY provider inherited structured controls: %#v", status.Capabilities)
	}
	actions := Actions(generic)
	support := map[ActionID]bool{}
	for _, action := range actions {
		support[action.ID] = action.Supported
	}
	if !support[ActionRawKeys] || support[ActionApproval] || support[ActionSteer] {
		t.Fatalf("typed actions do not match PTY capabilities: %#v", actions)
	}
}

func TestPTYProviderStreamsSanitizedOutputAndCompletesAtReadyPattern(t *testing.T) {
	p := NewPTYProvider("terminal-agent", config.ProviderConfig{
		AppName: "Terminal Agent",
		Command: "/bin/sh",
		Args:    []string{"-c", `while IFS= read -r line; do printf '\033[31mreply:%s\033[0m\n' "$line"; done`},
		Cwd:     t.TempDir(),
		Extra: map[string]any{
			"ready_pattern":   `reply:(hello|world)`,
			"idle_timeout_ms": float64(500),
		},
	})
	t.Cleanup(p.Shutdown)
	frames := make(chan map[string]any, 16)
	p.SetStreamPublisher(func(target string, frame map[string]any) {
		if target == "session-a" {
			frames <- frame
		}
	})
	if native, err := p.OpenOrCreateSession("session-a", StartOptions{}); err != nil || native != "session-a" {
		t.Fatalf("open PTY session: native=%q err=%v", native, err)
	}
	result := p.SendPrompt("session-a", "hello")
	if !result.OK || result.State != "running" || result.NativeTaskID == "" {
		t.Fatalf("send result: %#v", result)
	}

	deadline := time.After(3 * time.Second)
	var streamed strings.Builder
	completed := false
	for !completed {
		select {
		case frame := <-frames:
			switch frame["type"] {
			case "delta":
				streamed.WriteString(stringAny(frame["text"]))
			case "turn":
				completed = frame["status"] == "completed"
			}
		case <-deadline:
			t.Fatalf("timed out waiting for completion; output=%q state=%s", streamed.String(), p.DetectState("session-a"))
		}
	}
	if text := streamed.String(); !strings.Contains(text, "reply:hello") || strings.ContainsRune(text, '\x1b') {
		t.Fatalf("stream was not sanitized: %q", text)
	}
	if state := p.DetectState("session-a"); state != "idle" {
		t.Fatalf("state=%q want idle", state)
	}
	second := p.SendPrompt("session-a", "world")
	if !second.OK {
		t.Fatalf("second send failed: %#v", second)
	}
	streamed.Reset()
	completed = false
	deadline = time.After(3 * time.Second)
	for !completed {
		select {
		case frame := <-frames:
			switch frame["type"] {
			case "delta":
				streamed.WriteString(stringAny(frame["text"]))
			case "turn":
				completed = frame["status"] == "completed"
			}
		case <-deadline:
			t.Fatalf("timed out waiting for second completion; output=%q", streamed.String())
		}
	}
	if !strings.Contains(streamed.String(), "reply:world") {
		t.Fatalf("second turn completed from stale ready output: %q", streamed.String())
	}
	messages, err := p.SessionMessages("session-a")
	if err != nil || len(messages) < 2 {
		t.Fatalf("messages=%#v err=%v", messages, err)
	}
	last := messages[len(messages)-1]
	if last["role"] != "assistant" || !strings.Contains(stringAny(last["text"]), "reply:world") {
		t.Fatalf("assistant history missing PTY output: %#v", messages)
	}
}

func TestPTYProviderRawKeysAreExplicitlyOptIn(t *testing.T) {
	p := NewPTYProvider("terminal-agent", config.ProviderConfig{Command: "/bin/sh", Cwd: t.TempDir()})
	if result := p.SendKeys("missing", []string{"ENTER"}); result["ok"] != false ||
		!strings.Contains(stringAny(result["detail"]), "disabled") {
		t.Fatalf("raw keys should fail before session lookup: %#v", result)
	}
	if _, err := encodePTYKeys([]string{"ENTER", "1", "ESC"}); err != nil {
		t.Fatalf("known PTY keys rejected: %v", err)
	}
	if _, err := encodePTYKeys([]string{"arbitrary text"}); err == nil {
		t.Fatal("multi-character raw key payload should be rejected")
	}
}

func TestPTYProviderRejectsOverlappingTurns(t *testing.T) {
	p := NewPTYProvider("terminal-agent", config.ProviderConfig{
		Command: "/bin/sh",
		Args:    []string{"-c", `while IFS= read -r line; do sleep 1; printf 'reply:%s\n' "$line"; done`},
		Cwd:     t.TempDir(),
		Extra:   map[string]any{"idle_timeout_ms": float64(1500)},
	})
	t.Cleanup(p.Shutdown)
	if _, err := p.OpenOrCreateSession("session-a", StartOptions{}); err != nil {
		t.Fatal(err)
	}
	if first := p.SendPrompt("session-a", "first"); !first.OK {
		t.Fatalf("first send failed: %#v", first)
	}
	second := p.SendPrompt("session-a", "second")
	if second.OK || second.State != "running" || second.Error == nil ||
		!strings.Contains(*second.Error, "already has a running turn") {
		t.Fatalf("overlapping send was not rejected: %#v", second)
	}
}

func TestPTYProviderPublishesRealTurnIDOnUnexpectedExit(t *testing.T) {
	p := NewPTYProvider("terminal-agent", config.ProviderConfig{
		Command: "/bin/sh", Args: []string{"-c", `IFS= read -r line; exit 7`}, Cwd: t.TempDir(),
	})
	t.Cleanup(p.Shutdown)
	frames := make(chan map[string]any, 8)
	p.SetStreamPublisher(func(target string, frame map[string]any) {
		if target == "session-a" {
			frames <- frame
		}
	})
	if _, err := p.OpenOrCreateSession("session-a", StartOptions{}); err != nil {
		t.Fatal(err)
	}
	if result := p.SendPrompt("session-a", "exit now"); !result.OK {
		t.Fatalf("send failed: %#v", result)
	}

	deadline := time.After(3 * time.Second)
	for {
		select {
		case frame := <-frames:
			if frame["type"] == "turn" && frame["status"] == "error" {
				if frame["turn_id"] != "pty-1" {
					t.Fatalf("unexpected-exit turn_id=%v, want pty-1", frame["turn_id"])
				}
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for unexpected-exit terminal frame")
		}
	}
}

func TestPTYProviderClosesExitedTerminalAndBoundsRetainedSessions(t *testing.T) {
	p := NewPTYProvider("terminal-agent", config.ProviderConfig{
		Command: "/bin/sh", Args: []string{"-c", "exit 0"}, Cwd: t.TempDir(),
		Extra: map[string]any{"max_sessions": float64(1)},
	})
	t.Cleanup(p.Shutdown)
	if _, err := p.OpenOrCreateSession("session-a", StartOptions{}); err != nil {
		t.Fatal(err)
	}
	first := p.session("session-a")
	select {
	case <-first.done:
	case <-time.After(2 * time.Second):
		t.Fatal("first PTY process did not exit")
	}
	first.mu.Lock()
	terminal := first.terminal
	first.mu.Unlock()
	if terminal != nil {
		t.Fatal("master PTY fd was retained after process exit")
	}
	if _, err := p.OpenOrCreateSession("session-b", StartOptions{}); err != nil {
		t.Fatalf("exited session was not pruned to make bounded room: %v", err)
	}
	if p.session("session-a") != nil {
		t.Fatal("oldest exited PTY session was retained past max_sessions")
	}
}

func TestTerminalSanitizerHandlesSplitUTF8AndControlSequences(t *testing.T) {
	var sanitizer terminalSanitizer
	first := sanitizer.Write([]byte{0x1b, '[', '3', '1', 'm', 0xe4, 0xbd})
	second := sanitizer.Write([]byte{0xa0, 0x1b, ']', '0', ';', 's', 'e', 'c', 'r', 'e', 't', 0x07, '\r', '\n'})
	if first != "" || second != "你\n" {
		t.Fatalf("sanitized chunks = %q + %q", first, second)
	}
}

func TestRegistryShutdownStopsPTYProcesses(t *testing.T) {
	registry := BuildRegistry(&config.Config{Providers: map[string]config.ProviderConfig{
		"terminal-agent": {
			Type: "pty", Command: "/bin/sh", Args: []string{"-c", "while :; do sleep 1; done"}, Cwd: t.TempDir(),
			Extra: map[string]any{"kill_grace_ms": float64(50)},
		},
	}})
	p := registry["terminal-agent"].(*PTYProvider)
	if _, err := p.OpenOrCreateSession("session-a", StartOptions{}); err != nil {
		t.Fatal(err)
	}
	session := p.session("session-a")
	registry.Shutdown()
	select {
	case <-session.done:
	case <-time.After(2 * time.Second):
		t.Fatal("PTY process was not stopped by registry shutdown")
	}
}

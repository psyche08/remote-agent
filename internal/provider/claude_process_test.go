package provider

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/psyche08/remote-agent/internal/config"
)

type fakeClaudeDesktopProcessManager struct {
	aliases []string
	stopped bool
	err     error
}

func (f *fakeClaudeDesktopProcessManager) StopSession(aliases []string, _ time.Duration) (bool, error) {
	f.aliases = append([]string{}, aliases...)
	return f.stopped, f.err
}

func writeClaudeDesktopSessionMeta(t *testing.T, base, nativeID, cliID string) {
	t.Helper()
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"sessionId":"` + nativeID + `","cliSessionId":"` + cliID + `","title":"handoff"}`)
	if err := os.WriteFile(filepath.Join(base, "local_"+nativeID+".json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeDesktopProcessOwnershipSignalsOnlyMatchingFamily(t *testing.T) {
	const transcriptID = "019ef769-7611-70e0-839a-283dc0e5f256"
	processes := []claudeProcess{
		{PID: 10, PPID: 1, Command: "/Users/testuser/Library/Application Support/Claude/claude-code/2.1/claude.app/Contents/MacOS/claude"},
		{PID: 11, PPID: 10, Command: "/Users/testuser/Library/Application Support/Claude/claude-code/2.1/claude.app/Contents/MacOS/claude --resume " + transcriptID},
		{PID: 12, PPID: 1, Command: "/Users/testuser/Library/Application Support/Claude/claude-code/2.1/claude.app/Contents/MacOS/claude --resume unrelated"},
		{PID: 13, PPID: 1, Command: "/Users/testuser/.local/bin/claude --resume " + transcriptID},
	}
	alive := map[int]bool{10: true, 11: true, 12: true, 13: true}
	signaled := []int{}
	m := &systemClaudeDesktopProcessManager{
		listProcesses: func() ([]claudeProcess, error) { return processes, nil },
		openFiles:     func(int) ([]string, error) { return nil, nil },
		signal: func(pid int, _ syscall.Signal) error {
			signaled = append(signaled, pid)
			alive[pid] = false
			return nil
		},
		alive: func(pid int) bool { return alive[pid] },
		sleep: func(time.Duration) {},
	}
	stopped, err := m.StopSession([]string{transcriptID}, time.Second)
	if err != nil || !stopped {
		t.Fatalf("StopSession stopped=%v err=%v", stopped, err)
	}
	sort.Ints(signaled)
	if !reflect.DeepEqual(signaled, []int{10, 11}) {
		t.Fatalf("signaled PIDs = %v, want only matching Desktop family", signaled)
	}
}

func TestClaudeDesktopProcessOwnershipUsesOpenTranscript(t *testing.T) {
	const transcriptID = "019ef769-7611-70e0-839a-283dc0e5f256"
	aliases := map[string]bool{transcriptID: true}
	if !filesHaveSessionTranscript([]string{"/Users/testuser/.claude/projects/-repo/" + transcriptID + ".jsonl"}, aliases) {
		t.Fatal("exact transcript file should prove process ownership")
	}
	if filesHaveSessionTranscript([]string{"/Users/testuser/.claude/projects/-repo/unrelated.jsonl"}, aliases) {
		t.Fatal("unrelated transcript must not prove process ownership")
	}
	if isClaudeDesktopInternalProcess("/Users/testuser/.local/bin/claude --resume " + transcriptID) {
		t.Fatal("standalone managed CLI must not be classified as Desktop internal CLI")
	}
}

func TestClaudeCLIResumeStopsDesktopOwnerBeforeIgnoringRunningTurnstate(t *testing.T) {
	dir := t.TempDir()
	desktopDir := filepath.Join(dir, "desktop")
	turnDir := filepath.Join(dir, "turnstate")
	argsPath := filepath.Join(dir, "args.txt")
	const transcriptID = "019ef769-7611-70e0-839a-283dc0e5f256"
	const nativeID = "local-native-session"
	writeClaudeDesktopSessionMeta(t, desktopDir, nativeID, transcriptID)
	if err := os.MkdirAll(turnDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(turnDir, transcriptID+".json"), []byte(`{"state":"running","ts":9999999999}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_STREAM_ARGS_FILE", argsPath)
	c := NewClaudeCLI("claude", config.ProviderConfig{
		Command: writeClaudeStreamFixture(t, dir), Cwd: dir,
		Extra: map[string]any{"claude_code_sessions_dir": desktopDir, "turnstate_dir": turnDir},
	})
	t.Cleanup(c.StopCLIStream)
	fake := &fakeClaudeDesktopProcessManager{stopped: true}
	c.desktopProcesses = fake
	if !c.WaitResumable(transcriptID) {
		t.Fatal("Desktop-origin session must reach the guarded resume handoff")
	}
	got, err := c.OpenResumeSession("logical", transcriptID, dir, false)
	if err != nil || got != transcriptID {
		t.Fatalf("resume after Desktop handoff: id=%q err=%v", got, err)
	}
	wantAliases := []string{transcriptID, nativeID}
	sort.Strings(wantAliases)
	if !reflect.DeepEqual(fake.aliases, wantAliases) {
		t.Fatalf("handoff aliases=%v want=%v", fake.aliases, wantAliases)
	}
	if args := string(waitForFile(t, argsPath)); !stringsContainsAll(args, "--resume\n"+transcriptID) {
		t.Fatalf("resume args missing after handoff: %s", args)
	}
}

func TestClaudeCLIResumeRefusesWhenDesktopOwnerCannotStop(t *testing.T) {
	dir := t.TempDir()
	desktopDir := filepath.Join(dir, "desktop")
	argsPath := filepath.Join(dir, "args.txt")
	const transcriptID = "019ef769-7611-70e0-839a-283dc0e5f256"
	writeClaudeDesktopSessionMeta(t, desktopDir, "native", transcriptID)
	t.Setenv("CLAUDE_STREAM_ARGS_FILE", argsPath)
	c := NewClaudeCLI("claude", config.ProviderConfig{
		Command: writeClaudeStreamFixture(t, dir), Cwd: dir,
		Extra: map[string]any{"claude_code_sessions_dir": desktopDir},
	})
	t.Cleanup(c.StopCLIStream)
	c.desktopProcesses = &fakeClaudeDesktopProcessManager{err: errors.New("still alive")}
	if _, err := c.OpenResumeSession("logical", transcriptID, dir, false); err == nil {
		t.Fatal("resume must fail when Desktop owner exit cannot be confirmed")
	}
	if _, err := os.Stat(argsPath); !os.IsNotExist(err) {
		t.Fatalf("managed CLI started despite failed handoff: stat err=%v", err)
	}
}

func TestClaudeCLILazySendStopsDesktopOwnerBeforeResume(t *testing.T) {
	dir := t.TempDir()
	desktopDir := filepath.Join(dir, "desktop")
	turnDir := filepath.Join(dir, "turnstate")
	const transcriptID = "019ef769-7611-70e0-839a-283dc0e5f256"
	writeClaudeDesktopSessionMeta(t, desktopDir, "native", transcriptID)
	if err := os.MkdirAll(turnDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(turnDir, transcriptID+".json"), []byte(`{"state":"running","ts":9999999999}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c := NewClaudeCLI("claude", config.ProviderConfig{
		Command: writeClaudeStreamFixture(t, dir), Cwd: dir,
		Extra: map[string]any{"claude_code_sessions_dir": desktopDir, "turnstate_dir": turnDir},
	})
	t.Cleanup(c.StopCLIStream)
	fake := &fakeClaudeDesktopProcessManager{stopped: true}
	c.desktopProcesses = fake
	c.sessions["logical"] = transcriptID
	c.BindTranscript("logical", transcriptID)
	result := c.SendPrompt("logical", "fixture prompt")
	if !result.OK || result.NativeTaskID != transcriptID {
		t.Fatalf("lazy send after Desktop handoff: %#v", result)
	}
	if len(fake.aliases) == 0 {
		t.Fatal("lazy send bypassed Desktop process handoff")
	}
}

func stringsContainsAll(value string, wants ...string) bool {
	for _, want := range wants {
		if !strings.Contains(value, want) {
			return false
		}
	}
	return true
}

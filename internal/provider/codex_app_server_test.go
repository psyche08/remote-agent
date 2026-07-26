package provider

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/psyche08/remote-agent/internal/config"
)

func codexHelperMode() string {
	for i, arg := range os.Args {
		if arg == "--codex-app-server-helper" && i+1 < len(os.Args) {
			return os.Args[i+1]
		}
	}
	return ""
}

func TestCodexAppServerHelperProcess(t *testing.T) {
	mode := codexHelperMode()
	if mode == "" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var request map[string]any
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			os.Exit(2)
		}
		switch stringAny(request["method"]) {
		case "initialize":
			_ = encoder.Encode(map[string]any{
				"id": request["id"],
				"result": map[string]any{
					"codexHome": "/tmp/codex-home", "platformFamily": "unix",
					"platformOs": "test", "userAgent": "codex-cli/0.0.0-test",
				},
			})
		case "thread/list":
			if mode == "eof" {
				os.Exit(0)
			}
		}
	}
	os.Exit(0)
}

func codexHelperCommand(mode string) []string {
	return []string{
		os.Args[0], "-test.run=^TestCodexAppServerHelperProcess$",
		"--", "--codex-app-server-helper", mode,
	}
}

func TestCodexAppServerExitImmediatelyFailsPendingRPC(t *testing.T) {
	client := NewCodexAppServerClient(codexHelperCommand("eof"), t.TempDir(), nil, nil)
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Initialize("remote-coding-test"); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	_, err := client.ThreadList(5*time.Second, nil)
	if err == nil {
		t.Fatal("thread/list unexpectedly succeeded after child exit")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("pending RPC waited for its timeout after EOF: %s", elapsed)
	}
	if !strings.Contains(err.Error(), "codex app-server exited") {
		t.Fatalf("unexpected exit error: %v", err)
	}
	select {
	case <-client.Done():
	case <-time.After(time.Second):
		t.Fatal("client lifecycle did not close after child exit")
	}
}

type exitAwareFakeCodexClient struct {
	*fakeCodexClient
	done     chan struct{}
	exitMu   sync.Mutex
	exitErr  error
	onExit   func(error)
	exitOnce sync.Once
}

type initializeNotificationFakeCodexClient struct {
	*fakeCodexClient
	onNotification func(string, map[string]any)
}

func (f *initializeNotificationFakeCodexClient) Initialize(string) error {
	f.onNotification("thread/status/changed", map[string]any{
		"threadId": fakeCodexThreadID,
		"status":   map[string]any{"type": "active"},
	})
	return nil
}

func newExitAwareFakeCodexClient() *exitAwareFakeCodexClient {
	return &exitAwareFakeCodexClient{fakeCodexClient: newFakeCodexClient(), done: make(chan struct{})}
}

func (f *exitAwareFakeCodexClient) Done() <-chan struct{} { return f.done }

func (f *exitAwareFakeCodexClient) ExitError() error {
	f.exitMu.Lock()
	defer f.exitMu.Unlock()
	return f.exitErr
}

func (f *exitAwareFakeCodexClient) SetExitHandler(handler func(error)) {
	f.exitMu.Lock()
	f.onExit = handler
	err := f.exitErr
	exited := err != nil
	f.exitMu.Unlock()
	if exited && handler != nil {
		handler(err)
	}
}

func (f *exitAwareFakeCodexClient) exit(err error) {
	f.exitOnce.Do(func() {
		f.exitMu.Lock()
		f.exitErr = err
		handler := f.onExit
		close(f.done)
		f.exitMu.Unlock()
		if handler != nil {
			handler(err)
		}
	})
}

func (f *exitAwareFakeCodexClient) exitHandler() func(error) {
	f.exitMu.Lock()
	defer f.exitMu.Unlock()
	return f.onExit
}

func TestCodexRebuildsExitedClientAndIgnoresOldGenerationExit(t *testing.T) {
	c := NewCodex("codex", config.ProviderConfig{
		Command: "/unused/codex", Cwd: t.TempDir(),
		Extra: map[string]any{"prefer_desktop_codex": false},
	})
	c.desktopFactory = func() codexDesktopClient { return &fakeDesktopClient{} }
	created := []*exitAwareFakeCodexClient{}
	c.clientFactory = func(func(string, map[string]any), func(any, string, map[string]any)) codexAppClient {
		client := newExitAwareFakeCodexClient()
		created = append(created, client)
		return client
	}

	first, err := c.ensureClient()
	if err != nil {
		t.Fatal(err)
	}
	created[0].exit(errors.New("first generation exited"))
	second, err := c.ensureClient()
	if err != nil {
		t.Fatal(err)
	}
	if first == second || len(created) != 2 {
		t.Fatalf("dead client was reused: first=%p second=%p created=%d", first, second, len(created))
	}
	if generation := c.clientGeneration; generation != 2 {
		t.Fatalf("client generation=%d want=2", generation)
	}

	// A delayed duplicate callback from generation 1 must not clear generation
	// 2 or its pending state.
	if handler := created[0].exitHandler(); handler != nil {
		handler(errors.New("late old-generation callback"))
	}
	current, generation := c.currentClientRoute()
	if current != second || generation != 2 {
		t.Fatalf("old exit cleared the live generation: client=%p generation=%d", current, generation)
	}
}

func TestCodexInitializeNotificationDoesNotDeadlockClientInstall(t *testing.T) {
	c := NewCodex("codex", config.ProviderConfig{
		Command: "/unused/codex", Cwd: t.TempDir(),
		Extra: map[string]any{"prefer_desktop_codex": false},
	})
	c.clientFactory = func(onNotification func(string, map[string]any), _ func(any, string, map[string]any)) codexAppClient {
		return &initializeNotificationFakeCodexClient{
			fakeCodexClient: newFakeCodexClient(),
			onNotification:  onNotification,
		}
	}

	done := make(chan error, 1)
	go func() {
		_, err := c.ensureClient()
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("initialize notification deadlocked client installation")
	}
	if !c.threadActive(fakeCodexThreadID) {
		t.Fatal("initialize notification was not handled by the installed generation")
	}
}

func TestCodexReplacementWaitsForExitedGenerationCleanup(t *testing.T) {
	c := NewCodex("codex", config.ProviderConfig{
		Command: "/unused/codex", Cwd: t.TempDir(),
		Extra: map[string]any{"prefer_desktop_codex": false},
	})
	factoryCalled := make(chan struct{}, 2)
	c.clientFactory = func(func(string, map[string]any), func(any, string, map[string]any)) codexAppClient {
		factoryCalled <- struct{}{}
		return newExitAwareFakeCodexClient()
	}
	firstRaw, err := c.ensureClient()
	if err != nil {
		t.Fatal(err)
	}
	<-factoryCalled
	first := firstRaw.(*exitAwareFakeCodexClient)
	c.BindTranscript("sess-a", approvalThreadA)
	c.addAppServerApproval(1, float64(91), "item/commandExecution/requestApproval",
		commandApprovalParams(approvalThreadA, nil))

	publishEntered := make(chan struct{})
	releasePublish := make(chan struct{})
	var publishOnce sync.Once
	c.SetStreamPublisher(func(string, map[string]any) {
		publishOnce.Do(func() { close(publishEntered) })
		<-releasePublish
	})
	exitDone := make(chan struct{})
	go func() {
		first.exit(errors.New("generation one exited"))
		close(exitDone)
	}()
	select {
	case <-publishEntered:
	case <-time.After(time.Second):
		t.Fatal("exit cleanup did not reach approval publication")
	}

	reconnectDone := make(chan error, 1)
	go func() {
		_, reconnectErr := c.ensureClient()
		reconnectDone <- reconnectErr
	}()
	select {
	case <-factoryCalled:
		t.Fatal("replacement generation started before exited generation cleanup finished")
	case <-time.After(100 * time.Millisecond):
	}

	close(releasePublish)
	select {
	case <-exitDone:
	case <-time.After(time.Second):
		t.Fatal("exit cleanup did not finish")
	}
	select {
	case <-factoryCalled:
	case <-time.After(time.Second):
		t.Fatal("replacement generation was not started after cleanup")
	}
	select {
	case reconnectErr := <-reconnectDone:
		if reconnectErr != nil {
			t.Fatal(reconnectErr)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement generation did not finish initialization")
	}
}

package provider

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
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

func codexInitializeExperimentalAPIEnabled(request map[string]any) bool {
	capabilities := mapAny(mapAny(request["params"])["capabilities"])
	enabled, ok := capabilities["experimentalApi"].(bool)
	return ok && enabled
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
			if mode == "require-experimental-api" && !codexInitializeExperimentalAPIEnabled(request) {
				_ = encoder.Encode(map[string]any{
					"id": request["id"],
					"error": map[string]any{
						"code":    -32602,
						"message": "initialize.params.capabilities.experimentalApi must be true",
					},
				})
				continue
			}
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

func TestCodexAppServerStdioInitializeAdvertisesExperimentalAPI(t *testing.T) {
	client := NewCodexAppServerClient(codexHelperCommand("require-experimental-api"), t.TempDir(), nil, nil)
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Initialize("agenthalo-stdio-test"); err != nil {
		t.Fatal(err)
	}
}

func TestCodexAppServerExitImmediatelyFailsPendingRPC(t *testing.T) {
	client := NewCodexAppServerClient(codexHelperCommand("eof"), t.TempDir(), nil, nil)
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Initialize("agenthalo-test"); err != nil {
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

type fakeCodexAppServerJSONTransport struct {
	writes    chan []byte
	reads     chan []byte
	closed    chan struct{}
	closeOnce sync.Once
}

type writeFailingCodexAppServerTransport struct {
	closed    chan struct{}
	closeOnce sync.Once
}

func newWriteFailingCodexAppServerTransport() *writeFailingCodexAppServerTransport {
	return &writeFailingCodexAppServerTransport{closed: make(chan struct{})}
}

func (t *writeFailingCodexAppServerTransport) WriteJSON([]byte) error {
	return io.ErrClosedPipe
}

func (t *writeFailingCodexAppServerTransport) ReadJSON() ([]byte, error) {
	<-t.closed
	return nil, io.EOF
}

func (t *writeFailingCodexAppServerTransport) Close() error {
	t.closeOnce.Do(func() { close(t.closed) })
	return nil
}

func newFakeCodexAppServerJSONTransport() *fakeCodexAppServerJSONTransport {
	return &fakeCodexAppServerJSONTransport{
		writes: make(chan []byte, 16),
		reads:  make(chan []byte, 16),
		closed: make(chan struct{}),
	}
}

func TestCodexSharedAppServerWriteFailureRetiresBlockedReader(t *testing.T) {
	transport := newWriteFailingCodexAppServerTransport()
	client := NewCodexSharedAppServerClient(
		CodexSharedDaemonStatus{SocketPath: "/private/tmp/codex-shared-write-failure.sock"},
		t.TempDir(),
		nil,
		nil,
	)
	client.dialWebSocket = func(CodexSharedDaemonStatus, time.Duration) (codexAppServerJSONTransport, error) {
		return transport, nil
	}
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}

	_, err := client.ThreadList(time.Second, nil)
	if err == nil || !strings.Contains(err.Error(), "write to codex app-server failed") {
		t.Fatalf("write error=%v", err)
	}
	select {
	case <-client.Done():
	case <-time.After(time.Second):
		t.Fatal("write failure left client lifecycle open")
	}
	select {
	case <-transport.closed:
	default:
		t.Fatal("write failure did not close transport and unblock reader")
	}
	if exitErr := client.ExitError(); exitErr == nil ||
		!strings.Contains(exitErr.Error(), "write to codex app-server failed") {
		t.Fatalf("exit error=%v", exitErr)
	}
}

func (t *fakeCodexAppServerJSONTransport) WriteJSON(payload []byte) error {
	select {
	case <-t.closed:
		return io.ErrClosedPipe
	case t.writes <- append([]byte(nil), payload...):
		return nil
	}
}

func (t *fakeCodexAppServerJSONTransport) ReadJSON() ([]byte, error) {
	select {
	case <-t.closed:
		return nil, io.EOF
	case payload := <-t.reads:
		return append([]byte(nil), payload...), nil
	}
}

func (t *fakeCodexAppServerJSONTransport) Close() error {
	t.closeOnce.Do(func() { close(t.closed) })
	return nil
}

func (t *fakeCodexAppServerJSONTransport) send(tst *testing.T, obj map[string]any) {
	tst.Helper()
	payload, err := json.Marshal(obj)
	if err != nil {
		tst.Fatal(err)
	}
	select {
	case t.reads <- payload:
	case <-time.After(time.Second):
		tst.Fatal("timed out delivering fake app-server message")
	}
}

func (t *fakeCodexAppServerJSONTransport) nextWrite(tst *testing.T) map[string]any {
	tst.Helper()
	select {
	case payload := <-t.writes:
		if strings.HasSuffix(string(payload), "\n") {
			tst.Fatalf("WebSocket payload unexpectedly used stdio newline framing: %q", payload)
		}
		var msg map[string]any
		if err := json.Unmarshal(payload, &msg); err != nil {
			tst.Fatalf("invalid client JSON: %v", err)
		}
		return msg
	case <-time.After(time.Second):
		tst.Fatal("timed out waiting for app-server write")
		return nil
	}
}

func newFakeSharedCodexClient(t *testing.T, transport *fakeCodexAppServerJSONTransport, onNotification func(string, map[string]any), onServerRequest func(any, string, map[string]any)) *CodexAppServerClient {
	t.Helper()
	const socketPath = "/private/tmp/codex-shared-test.sock"
	status := CodexSharedDaemonStatus{SocketPath: socketPath}
	client := NewCodexSharedAppServerClient(status, t.TempDir(), onNotification, onServerRequest)
	client.dialWebSocket = func(got CodexSharedDaemonStatus, timeout time.Duration) (codexAppServerJSONTransport, error) {
		if got.SocketPath != socketPath {
			t.Fatalf("dial path=%q want=%q", got.SocketPath, socketPath)
		}
		if timeout != codexAppServerWebSocketDialTimeout {
			t.Fatalf("dial timeout=%s want=%s", timeout, codexAppServerWebSocketDialTimeout)
		}
		return transport, nil
	}
	return client
}

func initializeFakeSharedCodexClient(t *testing.T, client *CodexAppServerClient, transport *fakeCodexAppServerJSONTransport) {
	t.Helper()
	result := make(chan error, 1)
	go func() { result <- client.Initialize("agenthalo-shared-test") }()

	initialize := transport.nextWrite(t)
	if method := stringAny(initialize["method"]); method != "initialize" {
		t.Fatalf("first method=%q want=initialize", method)
	}
	if !codexInitializeExperimentalAPIEnabled(initialize) {
		t.Fatalf("initialize.params.capabilities.experimentalApi=%#v want=true", mapAny(initialize["params"])["capabilities"])
	}
	transport.send(t, map[string]any{"id": initialize["id"], "result": map[string]any{
		"codexHome": "/tmp/codex-home", "platformFamily": "unix",
	}})
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("initialize did not complete")
	}
	initialized := transport.nextWrite(t)
	if method := stringAny(initialized["method"]); method != "initialized" {
		t.Fatalf("second method=%q want=initialized", method)
	}
}

func TestCodexSharedAppServerClientUsesWebSocketJSONFrames(t *testing.T) {
	transport := newFakeCodexAppServerJSONTransport()
	notifications := make(chan map[string]any, 1)
	serverRequests := make(chan map[string]any, 1)
	client := newFakeSharedCodexClient(t, transport,
		func(method string, params map[string]any) {
			notifications <- map[string]any{"method": method, "params": params}
		},
		func(id any, method string, params map[string]any) {
			serverRequests <- map[string]any{"id": id, "method": method, "params": params}
		},
	)
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	initializeFakeSharedCodexClient(t, client, transport)

	resumeResult := make(chan error, 1)
	go func() {
		_, err := client.ThreadResume("thread-shared", map[string]any{"excludeTurns": true}, time.Second)
		resumeResult <- err
	}()
	resume := transport.nextWrite(t)
	if method := stringAny(resume["method"]); method != "thread/resume" {
		t.Fatalf("method=%q want=thread/resume", method)
	}
	if threadID := stringAny(mapAny(resume["params"])["threadId"]); threadID != "thread-shared" {
		t.Fatalf("threadId=%q want=thread-shared", threadID)
	}
	transport.send(t, map[string]any{"id": resume["id"], "result": map[string]any{
		"thread": map[string]any{"id": "thread-shared"},
	}})
	if err := <-resumeResult; err != nil {
		t.Fatal(err)
	}

	transport.send(t, map[string]any{
		"method": "thread/status/changed",
		"params": map[string]any{
			"threadId": "thread-shared",
			"turnId":   "turn-shared",
			"status":   map[string]any{"type": "active"},
		},
	})
	select {
	case notification := <-notifications:
		if method := stringAny(notification["method"]); method != "thread/status/changed" {
			t.Fatalf("notification method=%q", method)
		}
	case <-time.After(time.Second):
		t.Fatal("notification callback was not invoked")
	}
	if !client.IsActive("thread-shared") {
		t.Fatal("shared transport notification did not update thread status")
	}
	if turnID, ok := client.ThreadTurn("thread-shared"); !ok || turnID != "turn-shared" {
		t.Fatalf("turn=(%q,%v) want=(turn-shared,true)", turnID, ok)
	}

	transport.send(t, map[string]any{
		"id":     71,
		"method": "item/commandExecution/requestApproval",
		"params": map[string]any{"threadId": "thread-shared"},
	})
	var request map[string]any
	select {
	case request = <-serverRequests:
	case <-time.After(time.Second):
		t.Fatal("server request callback was not invoked")
	}
	if stringAny(request["method"]) != "item/commandExecution/requestApproval" {
		t.Fatalf("unexpected server request: %#v", request)
	}
	if err := client.Respond(request["id"], map[string]any{"decision": "accept"}); err != nil {
		t.Fatal(err)
	}
	response := transport.nextWrite(t)
	if id, ok := numericID(response["id"]); !ok || id != 71 {
		t.Fatalf("response id=%v want=71", response["id"])
	}
	if decision := stringAny(mapAny(response["result"])["decision"]); decision != "accept" {
		t.Fatalf("decision=%q want=accept", decision)
	}
}

func TestCodexSharedAppServerExitFailsPendingRPC(t *testing.T) {
	transport := newFakeCodexAppServerJSONTransport()
	client := newFakeSharedCodexClient(t, transport, nil, nil)
	exits := make(chan error, 2)
	client.SetExitHandler(func(err error) { exits <- err })
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	initializeFakeSharedCodexClient(t, client, transport)

	result := make(chan error, 1)
	go func() {
		_, err := client.ThreadList(5*time.Second, nil)
		result <- err
	}()
	request := transport.nextWrite(t)
	if method := stringAny(request["method"]); method != "thread/list" {
		t.Fatalf("method=%q want=thread/list", method)
	}
	started := time.Now()
	if err := transport.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "codex app-server exited") {
			t.Fatalf("error=%v want app-server exit", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pending shared RPC was not failed on WebSocket close")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("pending shared RPC failed too slowly: %s", elapsed)
	}
	select {
	case <-client.Done():
	case <-time.After(time.Second):
		t.Fatal("shared client lifecycle did not close")
	}
	select {
	case err := <-exits:
		if err == nil || !strings.Contains(err.Error(), "codex app-server exited") {
			t.Fatalf("exit callback error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("shared client exit callback was not invoked")
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exits:
		t.Fatalf("shared client exit callback invoked more than once: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestCodexSharedAppServerRejectsEmptySocketPath(t *testing.T) {
	client := NewCodexSharedAppServerClient(CodexSharedDaemonStatus{}, t.TempDir(), nil, nil)
	if err := client.Start(); err == nil || !strings.Contains(err.Error(), "socket path is empty") {
		t.Fatalf("Start error=%v want empty socket path failure", err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-client.Done():
	case <-time.After(time.Second):
		t.Fatal("unstarted shared client did not close its lifecycle")
	}
}

func TestCompactMapDropsEmptyProtocolOverrides(t *testing.T) {
	got := compactMap(map[string]any{
		"model":           "",
		"reasoningEffort": "  ",
		"cwd":             "/tmp/project",
		"enabled":         false,
		"count":           0,
	})
	if _, ok := got["model"]; ok {
		t.Fatalf("empty model override was retained: %#v", got)
	}
	if _, ok := got["reasoningEffort"]; ok {
		t.Fatalf("empty effort override was retained: %#v", got)
	}
	if got["cwd"] != "/tmp/project" || got["enabled"] != false || got["count"] != 0 {
		t.Fatalf("non-empty values were changed: %#v", got)
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

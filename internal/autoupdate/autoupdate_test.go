package autoupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testPlatform = "darwin-arm64"
	oldCommit    = "aaaa1111"
	newCommit    = "bbbb2222"
)

var (
	testBinary   = []byte("fake binary bytes")
	testScript   = []byte("#!/bin/sh\nexit 0\n")
	testWatchdog = []byte("#!/bin/sh\necho watchdog\n")
)

func testManifest(t *testing.T, commit string) []byte {
	t.Helper()
	m := Manifest{
		Commit:  commit,
		BuiltAt: "2026-07-03T12:00:00+08:00",
		Binaries: map[string]Artifact{
			testPlatform: {Path: "remote-agent-" + testPlatform, SHA256: sha(testBinary)},
		},
		UpdateScript: Artifact{Path: "update.sh", SHA256: sha(testScript)},
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func fakeFetch(t *testing.T, manifest []byte) func(context.Context, Options, string) ([]byte, error) {
	t.Helper()
	files := map[string][]byte{
		"manifest.json":                manifest,
		"remote-agent-" + testPlatform: testBinary,
		"update.sh":                    testScript,
		"watchdog.sh":                  testWatchdog,
	}
	return func(_ context.Context, _ Options, relPath string) ([]byte, error) {
		b, ok := files[relPath]
		if !ok {
			return nil, fmt.Errorf("not found: %s", relPath)
		}
		return b, nil
	}
}

// scriptRunner 记录 update.sh 的调用;调用后把 installed 置 true,
// 供 HealthCheck 模拟"脚本重启服务后版本变化"。
type scriptRunner struct {
	installed bool
	dir       string
	name      string
	args      []string
	fail      bool
}

func (r *scriptRunner) Run(_ context.Context, dir string, name string, args ...string) (string, error) {
	r.dir, r.name, r.args = dir, name, args
	if r.fail {
		return "boom", fmt.Errorf("update script failed")
	}
	r.installed = true
	return "installed", nil
}

func baseOptions(t *testing.T, r Runner, fetch func(context.Context, Options, string) ([]byte, error)) Options {
	t.Helper()
	dir := t.TempDir()
	return Options{
		DeviceID:      "device-b",
		TargetPath:    filepath.Join(dir, "bin", "remote-agent"),
		StagingDir:    filepath.Join(dir, "staging"),
		StatePath:     filepath.Join(dir, "state.json"),
		Platform:      testPlatform,
		Runner:        r,
		Fetch:         fetch,
		LogWriter:     os.Stderr,
		HealthTimeout: 100 * time.Millisecond,
		Now:           func() time.Time { return time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC) },
	}
}

func TestApplyRefusesWithinCooldown(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	statePath := filepath.Join(dir, "state.json")
	if err := SaveState(statePath, State{
		LastSuccessAt: now.Add(-10 * time.Minute).Format(time.RFC3339Nano),
		LastFrom:      newCommit,
		LastTo:        newCommit,
	}); err != nil {
		t.Fatal(err)
	}
	opts := baseOptions(t, failRunner{t: t}, fakeFetch(t, testManifest(t, newCommit)))
	opts.StatePath = statePath
	opts.Now = func() time.Time { return now }
	opts.HealthCheck = func(context.Context, Options) (string, error) { return oldCommit, nil }
	res, err := Apply(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "refused" || res.Reason != "cooldown" {
		t.Fatalf("res=%#v", res)
	}
	st, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if st.LastFrom != oldCommit || st.LastTo != newCommit {
		t.Fatalf("cooldown lost observed release pair: %#v", st)
	}
}

func TestApplyUpdatesNewManifestWithinCooldown(t *testing.T) {
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	r := &scriptRunner{}
	opts := baseOptions(t, r, fakeFetch(t, testManifest(t, newCommit)))
	if err := SaveState(opts.StatePath, State{
		LastSuccessAt: now.Add(-10 * time.Minute).Format(time.RFC3339Nano),
		LastFrom:      oldCommit,
		LastTo:        oldCommit,
	}); err != nil {
		t.Fatal(err)
	}
	opts.Now = func() time.Time { return now }
	opts.HealthCheck = func(context.Context, Options) (string, error) {
		if r.installed {
			return newCommit, nil
		}
		return oldCommit, nil
	}
	res, err := Apply(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "updated" || res.From != oldCommit || res.To != newCommit {
		t.Fatalf("new manifest was delayed by cooldown: %#v", res)
	}
}

func TestApplyNoUpdateWhenVersionMatchesManifest(t *testing.T) {
	r := &scriptRunner{}
	opts := baseOptions(t, r, fakeFetch(t, testManifest(t, newCommit)))
	if err := SaveState(opts.StatePath, State{LastUnchangedTarget: newCommit}); err != nil {
		t.Fatal(err)
	}
	opts.HealthCheck = func(context.Context, Options) (string, error) { return newCommit, nil }
	res, err := Apply(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "no_update" || res.From != newCommit || res.To != newCommit {
		t.Fatalf("res=%#v", res)
	}
	if r.name != "" {
		t.Fatal("update script should not run when version matches")
	}
	st, err := LoadState(opts.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if st.LastUnchangedTarget != "" {
		t.Fatalf("LastUnchangedTarget=%q, want cleared", st.LastUnchangedTarget)
	}
	if !strings.HasSuffix(st.LastAttemptAt, "+08:00") {
		t.Fatalf("LastAttemptAt=%q, want 东八区 (+08:00)", st.LastAttemptAt)
	}
}

func TestApplyRefusesSameUnchangedTarget(t *testing.T) {
	r := &scriptRunner{}
	opts := baseOptions(t, r, fakeFetch(t, testManifest(t, newCommit)))
	if err := SaveState(opts.StatePath, State{LastUnchangedTarget: newCommit}); err != nil {
		t.Fatal(err)
	}
	opts.HealthCheck = func(context.Context, Options) (string, error) { return oldCommit, nil }
	res, err := Apply(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "refused" || res.Reason != "unchanged_version" {
		t.Fatalf("res=%#v", res)
	}
	if r.name != "" {
		t.Fatal("update script should not run for unchanged-target refusal")
	}
}

func TestApplyUpdatesWhenManifestDiffers(t *testing.T) {
	r := &scriptRunner{}
	opts := baseOptions(t, r, fakeFetch(t, testManifest(t, newCommit)))
	opts.HealthCheck = func(_ context.Context, _ Options) (string, error) {
		if r.installed {
			return newCommit, nil
		}
		return oldCommit, nil
	}
	res, err := Apply(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "updated" || res.From != oldCommit || res.To != newCommit {
		t.Fatalf("res=%#v", res)
	}
	wantScript := filepath.Join(opts.StagingDir, "update.sh")
	wantBinary := filepath.Join(opts.StagingDir, "remote-agent-"+testPlatform)
	if r.name != wantScript {
		t.Fatalf("ran %q, want %q", r.name, wantScript)
	}
	if len(r.args) != 3 || r.args[0] != wantBinary || r.args[1] != opts.TargetPath || r.args[2] != "device-b" {
		t.Fatalf("args=%v", r.args)
	}
	if b, err := os.ReadFile(wantBinary); err != nil || string(b) != string(testBinary) {
		t.Fatalf("staged binary content mismatch: %v", err)
	}
	st, err := LoadState(opts.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if st.LastSuccessAt == "" || !strings.HasSuffix(st.LastSuccessAt, "+08:00") {
		t.Fatalf("LastSuccessAt=%q, want 东八区 (+08:00)", st.LastSuccessAt)
	}
	if st.LastUnchangedTarget != "" {
		t.Fatalf("LastUnchangedTarget=%q", st.LastUnchangedTarget)
	}
}

func TestApplyStagesExtraAssets(t *testing.T) {
	m := Manifest{
		Commit:  newCommit,
		BuiltAt: "2026-07-03T12:00:00+08:00",
		Binaries: map[string]Artifact{
			testPlatform: {Path: "remote-agent-" + testPlatform, SHA256: sha(testBinary)},
		},
		UpdateScript: Artifact{Path: "update.sh", SHA256: sha(testScript)},
		Assets:       []Artifact{{Path: "watchdog.sh", SHA256: sha(testWatchdog)}},
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	r := &scriptRunner{}
	opts := baseOptions(t, r, fakeFetch(t, b))
	opts.HealthCheck = func(_ context.Context, _ Options) (string, error) {
		if r.installed {
			return newCommit, nil
		}
		return oldCommit, nil
	}
	if _, err := Apply(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(opts.StagingDir, "watchdog.sh")
	if b, err := os.ReadFile(want); err != nil || string(b) != string(testWatchdog) {
		t.Fatalf("staged asset mismatch: path=%s err=%v content=%q", want, err, string(b))
	}
}

func TestApplyRecordsUnchangedTargetWhenHealthDoesNotAdvance(t *testing.T) {
	r := &scriptRunner{}
	opts := baseOptions(t, r, fakeFetch(t, testManifest(t, newCommit)))
	opts.HealthCheck = func(context.Context, Options) (string, error) { return oldCommit, nil }
	res, err := Apply(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "refused" || res.Reason != "unchanged_version" {
		t.Fatalf("res=%#v", res)
	}
	st, err := LoadState(opts.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if st.LastUnchangedTarget != newCommit {
		t.Fatalf("LastUnchangedTarget=%q, want %q", st.LastUnchangedTarget, newCommit)
	}
}

func TestApplyErrorsOnShaMismatch(t *testing.T) {
	m := Manifest{
		Commit:  newCommit,
		BuiltAt: "2026-07-03T12:00:00+08:00",
		Binaries: map[string]Artifact{
			testPlatform: {Path: "remote-agent-" + testPlatform, SHA256: strings.Repeat("0", 64)},
		},
		UpdateScript: Artifact{Path: "update.sh", SHA256: sha(testScript)},
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	r := &scriptRunner{}
	opts := baseOptions(t, r, fakeFetch(t, b))
	opts.HealthCheck = func(context.Context, Options) (string, error) { return oldCommit, nil }
	res, applyErr := Apply(context.Background(), opts)
	if applyErr == nil || res.Status != "error" {
		t.Fatalf("res=%#v err=%v", res, applyErr)
	}
	if !strings.Contains(applyErr.Error(), "sha256") {
		t.Fatalf("err=%v, want sha256 mismatch", applyErr)
	}
	if r.name != "" {
		t.Fatal("update script must not run on sha mismatch")
	}
}

func TestApplyErrorsWhenPlatformMissing(t *testing.T) {
	m := Manifest{
		Commit:       newCommit,
		BuiltAt:      "2026-07-03T12:00:00+08:00",
		Binaries:     map[string]Artifact{"linux-amd64": {Path: "x", SHA256: sha(testBinary)}},
		UpdateScript: Artifact{Path: "update.sh", SHA256: sha(testScript)},
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	opts := baseOptions(t, &scriptRunner{}, fakeFetch(t, b))
	opts.HealthCheck = func(context.Context, Options) (string, error) { return oldCommit, nil }
	res, applyErr := Apply(context.Background(), opts)
	if applyErr == nil || res.Status != "error" || !strings.Contains(applyErr.Error(), testPlatform) {
		t.Fatalf("res=%#v err=%v", res, applyErr)
	}
}

func TestApplyErrorsWithoutTargetPath(t *testing.T) {
	opts := baseOptions(t, &scriptRunner{}, fakeFetch(t, testManifest(t, newCommit)))
	opts.TargetPath = ""
	opts.HealthCheck = func(context.Context, Options) (string, error) { return oldCommit, nil }
	res, applyErr := Apply(context.Background(), opts)
	if applyErr == nil || res.Status != "error" {
		t.Fatalf("res=%#v err=%v", res, applyErr)
	}
}

type failRunner struct{ t *testing.T }

func (r failRunner) Run(context.Context, string, string, ...string) (string, error) {
	r.t.Fatal("runner should not be called")
	return "", nil
}

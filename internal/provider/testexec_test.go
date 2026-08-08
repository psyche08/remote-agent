package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"
)

// macOS evaluates a newly written executable the first time it is exec'd
// (code-signing and malware checks). On this platform that first exec costs
// hundreds of milliseconds and its tail runs past a second, while every later
// exec of the same file costs a few milliseconds. Tests that write a fake CLI
// and then invoke it through a bounded timeout must therefore pay that cost up
// front; otherwise they race the platform instead of exercising the code under
// test, and the resulting failure looks like a timeout in the code rather than
// a slow first exec.
const (
	testExecutableWarmBudget  = 60 * time.Second
	testExecutableWarmAttempt = 15 * time.Second
)

func TestMain(m *testing.M) {
	code := m.Run()
	cleanupCodexSharedDaemonTestBinary()
	cleanupClaudeStreamFixture()
	os.Exit(code)
}

// writeWarmTestExecutable writes script to path and blocks until it has
// actually completed an exec, so callers can invoke it under a tight timeout.
// warmEnv pins environment variables for the warm-up exec only, so warming a
// fake that reacts to the environment cannot disturb the calling test's state.
func writeWarmTestExecutable(t *testing.T, path, script string, mode os.FileMode, warmEnv ...string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(script), mode); err != nil {
		t.Fatal(err)
	}
	if err := warmTestExecutable(path, warmEnv...); err != nil {
		t.Fatal(err)
	}
	return path
}

// warmTestExecutable blocks until path has run to completion at least once.
// Readiness is a finished exec, not an elapsed duration: a non-zero exit still
// proves the kernel finished evaluating the file and handed it to the loader.
func warmTestExecutable(path string, warmEnv ...string) error {
	deadline := time.Now().Add(testExecutableWarmBudget)
	for attempt := 1; ; attempt++ {
		err := runTestExecutableWarmup(path, warmEnv)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("test executable %s never became runnable after %d attempts: %w", path, attempt, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func runTestExecutableWarmup(path string, warmEnv []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), testExecutableWarmAttempt)
	defer cancel()
	// The argument is deliberately one no fake CLI handles, so warming cannot
	// consume a scripted response or be mistaken for a real invocation.
	cmd := exec.CommandContext(ctx, path, "--rc-test-warmup")
	// A nil stdin is /dev/null, so a fake that reads stdin sees EOF and exits
	// instead of blocking the warm-up.
	cmd.Stdin = nil
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if len(warmEnv) > 0 {
		cmd.Env = append(os.Environ(), warmEnv...)
	}
	err := cmd.Run()
	if ctx.Err() != nil {
		return fmt.Errorf("warm-up exec of %s timed out: %w", path, ctx.Err())
	}
	var exited *exec.ExitError
	if err != nil && !errors.As(err, &exited) {
		return err
	}
	return nil
}

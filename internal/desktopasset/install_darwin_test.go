//go:build darwin

package desktopasset

import (
	"errors"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func stubLaunchctl(t *testing.T, run func(args ...string) ([]byte, error)) {
	t.Helper()
	previous := launchctlRun
	launchctlRun = run
	t.Cleanup(func() { launchctlRun = previous })
}

func stubMaterializeCurrent(t *testing.T, run func(path string) (bool, error)) {
	t.Helper()
	previous := materializeCurrent
	materializeCurrent = run
	t.Cleanup(func() { materializeCurrent = previous })
}

func stubPrepareHelperRestart(t *testing.T, run func(socketPath string) error) {
	t.Helper()
	previous := prepareHelperRestart
	prepareHelperRestart = run
	t.Cleanup(func() { prepareHelperRestart = previous })
}

func TestEnsureCurrentRestartsLoadedJobWhenBytesAreUnchanged(t *testing.T) {
	stubMaterializeCurrent(t, func(path string) (bool, error) {
		if path != "/tmp/current-helper" {
			t.Fatalf("materialize path = %q", path)
		}
		return false, nil
	})
	var calls [][]string
	prepared := ""
	stubPrepareHelperRestart(t, func(socketPath string) error {
		prepared = socketPath
		return nil
	})
	stubLaunchctl(t, func(args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		return nil, nil
	})

	replaced, err := EnsureCurrent("/tmp/current-helper", "/tmp/current-helper.sock")
	if err != nil || replaced {
		t.Fatalf("EnsureCurrent: replaced=%v err=%v, want unchanged success", replaced, err)
	}
	want := [][]string{
		{"print", "gui/" + strconv.Itoa(os.Getuid()) + "/" + LaunchAgentLabel},
		{"kickstart", "-k", "gui/" + strconv.Itoa(os.Getuid()) + "/" + LaunchAgentLabel},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("launchctl calls = %#v, want config-reload restart %#v", calls, want)
	}
	if prepared != "/tmp/current-helper.sock" {
		t.Fatalf("restart preflight socket=%q", prepared)
	}
}

func TestEnsureCurrentDoesNotFallbackOnPreferredInspectionFailure(t *testing.T) {
	stubMaterializeCurrent(t, func(string) (bool, error) { return false, nil })
	wantErr := errors.New("exit status 1")
	var calls [][]string
	stubPrepareHelperRestart(t, func(string) error {
		t.Fatal("inspection failure must not restart the job")
		return nil
	})
	stubLaunchctl(t, func(args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		return []byte("Operation not permitted"), wantErr
	})

	_, err := EnsureCurrent("/tmp/current-helper", "/tmp/current-helper.sock")
	if !errors.Is(err, wantErr) {
		t.Fatalf("EnsureCurrent err=%v, want wrapped %v", err, wantErr)
	}
	want := [][]string{{"print", "gui/" + strconv.Itoa(os.Getuid()) + "/" + LaunchAgentLabel}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("launchctl calls=%#v, want preferred inspection only %#v", calls, want)
	}
}

func TestRestartLaunchAgentAllowsAnUnloadedJob(t *testing.T) {
	var calls [][]string
	stubPrepareHelperRestart(t, func(string) error {
		t.Fatal("an unloaded job must not require a helper restart preflight")
		return nil
	})
	stubLaunchctl(t, func(args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		return []byte(`Could not find service "dev.linsheng.agenthalo.desktop"`),
			errors.New("exit status 113")
	})

	if err := restartLaunchAgentIfLoaded(
		"gui/501/dev.linsheng.agenthalo.desktop", "/tmp/helper.sock",
	); err != nil {
		t.Fatalf("unloaded job: %v", err)
	}
	want := [][]string{{"print", "gui/501/dev.linsheng.agenthalo.desktop"}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("launchctl calls = %#v, want %#v", calls, want)
	}
}

func TestRestartLaunchAgentPropagatesInspectionFailure(t *testing.T) {
	wantErr := errors.New("exit status 1")
	stubPrepareHelperRestart(t, func(string) error {
		t.Fatal("inspection failure must not reach restart preflight")
		return nil
	})
	stubLaunchctl(t, func(args ...string) ([]byte, error) {
		return []byte("Operation not permitted"), wantErr
	})

	err := restartLaunchAgentIfLoaded(
		"gui/501/dev.linsheng.agenthalo.desktop", "/tmp/helper.sock",
	)
	if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "Operation not permitted") {
		t.Fatalf("inspection err=%v, want wrapped launchctl output and %v", err, wantErr)
	}
}

func TestRestartLaunchAgentPropagatesKickstartFailure(t *testing.T) {
	wantErr := errors.New("exit status 1")
	var calls [][]string
	stubPrepareHelperRestart(t, func(string) error { return nil })
	stubLaunchctl(t, func(args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		if args[0] == "print" {
			return nil, nil
		}
		return []byte("Operation not permitted"), wantErr
	})

	err := restartLaunchAgentIfLoaded(
		"gui/501/dev.linsheng.agenthalo.desktop", "/tmp/helper.sock",
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("restart err = %v, want wrapped %v", err, wantErr)
	}
	if !strings.Contains(err.Error(), "Operation not permitted") {
		t.Fatalf("restart error lost launchctl output: %v", err)
	}
	want := [][]string{
		{"print", "gui/501/dev.linsheng.agenthalo.desktop"},
		{"kickstart", "-k", "gui/501/dev.linsheng.agenthalo.desktop"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("launchctl calls = %#v, want %#v", calls, want)
	}
}

func TestRestartLaunchAgentPropagatesJobDisappearingAfterPreflight(t *testing.T) {
	wantErr := errors.New("exit status 113")
	stubPrepareHelperRestart(t, func(string) error { return nil })
	stubLaunchctl(t, func(args ...string) ([]byte, error) {
		if args[0] == "print" {
			return nil, nil
		}
		return []byte(`Could not find service "dev.linsheng.agenthalo.desktop"`), wantErr
	})

	err := restartLaunchAgentIfLoaded(
		"gui/501/dev.linsheng.agenthalo.desktop", "/tmp/helper.sock",
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("post-preflight disappearance err=%v, want wrapped failure", err)
	}
}

func TestRestartLaunchAgentRunsKickstartForALoadedJob(t *testing.T) {
	var calls [][]string
	stubPrepareHelperRestart(t, func(string) error { return nil })
	stubLaunchctl(t, func(args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		return nil, nil
	})

	if err := restartLaunchAgentIfLoaded(
		"gui/501/dev.linsheng.agenthalo.desktop", "/tmp/helper.sock",
	); err != nil {
		t.Fatalf("loaded job: %v", err)
	}
	want := [][]string{
		{"print", "gui/501/dev.linsheng.agenthalo.desktop"},
		{"kickstart", "-k", "gui/501/dev.linsheng.agenthalo.desktop"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("launchctl calls = %#v, want %#v", calls, want)
	}
}

func TestRestartLaunchAgentDoesNotKickstartWhenSafetyPreflightFails(t *testing.T) {
	wantErr := errors.New("relock could not be confirmed")
	stubPrepareHelperRestart(t, func(socketPath string) error {
		if socketPath != "/tmp/helper.sock" {
			t.Fatalf("preflight socket=%q", socketPath)
		}
		return wantErr
	})
	var calls [][]string
	stubLaunchctl(t, func(args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		return nil, nil
	})

	err := restartLaunchAgentIfLoaded(
		"gui/501/dev.linsheng.agenthalo.desktop", "/tmp/helper.sock",
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("restart err=%v, want wrapped preflight failure", err)
	}
	want := [][]string{{"print", "gui/501/dev.linsheng.agenthalo.desktop"}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("unsafe restart calls=%#v, want inspection only", calls)
	}
}

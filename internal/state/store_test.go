package state

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestStoreReadsMissingFilesAsEmptyArrays(t *testing.T) {
	st := New(t.TempDir())
	sessions, err := st.Sessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Fatalf("got %d sessions", len(sessions))
	}
}

func TestUpdateSessionFieldsIsAtomicAndAbortsOnError(t *testing.T) {
	st := New(t.TempDir())
	if err := st.UpsertSession(Record{"session_id": "s1", "provider_id": "claude"}); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for key, value := range map[string]any{"route": "desktop", "state": "running"} {
		key, value := key, value
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, found, err := st.UpdateSessionFields("s1", func(rec Record) error {
				rec[key] = value
				return nil
			}); err != nil || !found {
				t.Errorf("update %s found=%v err=%v", key, found, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	rec, found, err := st.FindSession("s1")
	if err != nil || !found || rec["route"] != "desktop" || rec["state"] != "running" {
		t.Fatalf("atomic record=%#v found=%v err=%v", rec, found, err)
	}
	want := errors.New("reject")
	if _, found, err := st.UpdateSessionFields("s1", func(rec Record) error {
		rec["route"] = "cli"
		return want
	}); !found || !errors.Is(err, want) {
		t.Fatalf("rejected update found=%v err=%v", found, err)
	}
	rec, _, _ = st.FindSession("s1")
	if rec["route"] != "desktop" {
		t.Fatalf("rejected update was persisted: %#v", rec)
	}
}

func TestStoreRoundTripsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	st := New(dir)
	records := []Record{{"session_id": "s1", "provider_id": "codex", "custom": map[string]any{"x": float64(1)}}}
	if err := st.SaveSessions(records); err != nil {
		t.Fatal(err)
	}
	got, err := st.Sessions()
	if err != nil {
		t.Fatal(err)
	}
	if got[0]["session_id"] != "s1" {
		t.Fatalf("bad record: %#v", got[0])
	}
	if _, err := os.Stat(filepath.Join(dir, "sessions.json")); err != nil {
		t.Fatal(err)
	}
}

func TestAppendTaskOncePublishesOneDurableIdentity(t *testing.T) {
	st := New(t.TempDir())
	first := Record{"task_id": "prompt-operation-1", "request_digest": "digest-a"}
	got, created, err := st.AppendTaskOnce(first)
	if err != nil || !created || got["request_digest"] != "digest-a" {
		t.Fatalf("first got=%#v created=%v err=%v", got, created, err)
	}
	existing, created, err := st.AppendTaskOnce(Record{
		"task_id": "prompt-operation-1", "request_digest": "digest-b",
	})
	if err != nil || created || existing["request_digest"] != "digest-a" {
		t.Fatalf("retry got=%#v created=%v err=%v", existing, created, err)
	}
	records, err := st.Tasks()
	if err != nil || len(records) != 1 {
		t.Fatalf("tasks=%#v err=%v", records, err)
	}
}

func TestCommitTaskSessionOutcomeRecoversEveryCrashStage(t *testing.T) {
	for _, stage := range []string{"after_prepare", "after_session", "after_task"} {
		t.Run(stage, func(t *testing.T) {
			dir := t.TempDir()
			st := New(dir)
			if err := st.SaveSessions([]Record{{
				"session_id": "logical-1", "provider_id": "claude",
				"claude_control_route":     "desktop_computer_use",
				"claude_control_committed": false,
			}}); err != nil {
				t.Fatal(err)
			}
			if err := st.SaveTasks([]Record{{
				"task_id": "prompt-operation-1", "session_id": "logical-1",
				"provider_id": "claude", "status": "sent",
			}}); err != nil {
				t.Fatal(err)
			}
			injected := errors.New("simulated outcome crash")
			taskSessionOutcomeFault = func(current string) error {
				if current == stage {
					return injected
				}
				return nil
			}
			t.Cleanup(func() { taskSessionOutcomeFault = func(string) error { return nil } })
			_, _, found, err := st.CommitTaskSessionOutcome(
				"prompt-operation-1", "logical-1",
				func(task Record, session Record) error {
					task["status"] = "running"
					task["native_task_id"] = "native-turn-1"
					task["native_session_id"] = "native-session-1"
					task["claude_control_route"] = "desktop_computer_use"
					task["claude_control_committed"] = true
					session["state"] = "running"
					session["native_session_id"] = "native-session-1"
					session["transcript_id"] = "native-session-1"
					session["claude_control_committed"] = true
					return nil
				},
			)
			if !found || !errors.Is(err, injected) {
				t.Fatalf("fault found=%v err=%v", found, err)
			}
			journalInfo, err := os.Stat(filepath.Join(dir, taskSessionOutcomeJournal))
			if err != nil || journalInfo.Mode().Perm() != 0o600 {
				t.Fatalf("prepared journal mode=%v err=%v", journalInfo, err)
			}

			// A fresh process must replay the prepared final state before exposing
			// either compatibility file. The replay itself never invokes the crash
			// seam, just as a real restart would not resume the interrupted stack.
			restarted := New(dir)
			tasks, err := restarted.Tasks()
			if err != nil || len(tasks) != 1 || tasks[0]["status"] != "running" ||
				tasks[0]["native_session_id"] != "native-session-1" {
				t.Fatalf("recovered tasks=%#v err=%v", tasks, err)
			}
			sessions, err := restarted.Sessions()
			if err != nil || len(sessions) != 1 || sessions[0]["state"] != "running" ||
				sessions[0]["native_session_id"] != "native-session-1" ||
				sessions[0]["transcript_id"] != "native-session-1" ||
				sessions[0]["claude_control_committed"] != true {
				t.Fatalf("recovered sessions=%#v err=%v", sessions, err)
			}
			if _, err := os.Stat(filepath.Join(dir, taskSessionOutcomeJournal)); !os.IsNotExist(err) {
				t.Fatalf("outcome journal was not retired: %v", err)
			}
			taskSessionOutcomeFault = func(string) error { return nil }
		})
	}
}

func TestAtomicArrayWriteFailureCleansTempAndPreservesPreviousFile(t *testing.T) {
	for _, failureStage := range []string{"after_write", "after_file_sync"} {
		t.Run(failureStage, func(t *testing.T) {
			dir := t.TempDir()
			st := New(dir)
			if err := st.SaveSessions([]Record{{"session_id": "before"}}); err != nil {
				t.Fatal(err)
			}
			injected := errors.New("simulated file durability failure")
			atomicWriteFault = func(stage string, path string) error {
				if stage == failureStage && filepath.Base(path) == "sessions.json" {
					return injected
				}
				return nil
			}
			t.Cleanup(func() { atomicWriteFault = func(string, string) error { return nil } })
			if err := st.SaveSessions([]Record{{"session_id": "after"}}); !errors.Is(err, injected) {
				t.Fatalf("atomic write err=%v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "sessions.json.tmp")); !os.IsNotExist(err) {
				t.Fatalf("failed atomic write left temp file: %v", err)
			}
			atomicWriteFault = func(string, string) error { return nil }
			records, err := st.Sessions()
			if err != nil || len(records) != 1 || records[0]["session_id"] != "before" {
				t.Fatalf("failed atomic write replaced previous file: records=%#v err=%v", records, err)
			}
			info, err := os.Stat(filepath.Join(dir, "sessions.json"))
			if err != nil || info.Mode().Perm() != 0o644 {
				t.Fatalf("sessions file mode=%v err=%v", info, err)
			}
		})
	}
}

func TestCommitTaskSessionOutcomeAbortsBeforeJournalOnValidationError(t *testing.T) {
	dir := t.TempDir()
	st := New(dir)
	if err := st.SaveSessions([]Record{{"session_id": "logical-1", "state": "idle"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveTasks([]Record{{
		"task_id": "prompt-operation-1", "session_id": "logical-1", "status": "sent",
	}}); err != nil {
		t.Fatal(err)
	}
	want := errors.New("reject outcome")
	if _, _, found, err := st.CommitTaskSessionOutcome(
		"prompt-operation-1", "logical-1", func(task Record, session Record) error {
			task["status"] = "running"
			session["state"] = "running"
			return want
		},
	); !found || !errors.Is(err, want) {
		t.Fatalf("rejected outcome found=%v err=%v", found, err)
	}
	tasks, _ := st.Tasks()
	sessions, _ := st.Sessions()
	if tasks[0]["status"] != "sent" || sessions[0]["state"] != "idle" {
		t.Fatalf("rejected outcome leaked: tasks=%#v sessions=%#v", tasks, sessions)
	}
	if _, err := os.Stat(filepath.Join(dir, taskSessionOutcomeJournal)); !os.IsNotExist(err) {
		t.Fatalf("rejected outcome wrote a journal: %v", err)
	}
}

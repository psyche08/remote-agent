package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Record map[string]any

type Store struct {
	dataDir string
	mu      sync.Mutex
}

const taskSessionOutcomeJournal = "task-session-outcome.json"

type taskSessionOutcomeTransaction struct {
	Version  int      `json:"version"`
	Tasks    []Record `json:"tasks"`
	Sessions []Record `json:"sessions"`
}

// taskSessionOutcomeFault is a deterministic crash seam for state tests. A
// returned error deliberately leaves the prepared journal in place so a new
// Store instance must replay it before exposing either tasks or sessions.
var taskSessionOutcomeFault = func(string) error { return nil }

// atomicWriteFault is a deterministic durability seam for state tests.
var atomicWriteFault = func(string, string) error { return nil }

func New(dataDir string) *Store {
	return &Store{dataDir: dataDir}
}

func (s *Store) DataDir() string {
	return s.dataDir
}

func (s *Store) Sessions() ([]Record, error) {
	return s.readArray("sessions.json")
}

func (s *Store) Tasks() ([]Record, error) {
	return s.readArray("tasks.json")
}

func (s *Store) SaveSessions(records []Record) error {
	return s.writeArray("sessions.json", records)
}

func (s *Store) SaveTasks(records []Record) error {
	return s.writeArray("tasks.json", records)
}

func (s *Store) FindSession(sessionID string) (Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records, err := s.readArrayUnlocked("sessions.json")
	if err != nil {
		return nil, false, err
	}
	for _, r := range records {
		if stringField(r, "session_id") == sessionID {
			return r, true, nil
		}
	}
	return nil, false, nil
}

func (s *Store) UpsertSession(record Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	records, err := s.readArrayUnlocked("sessions.json")
	if err != nil {
		return err
	}
	sessionID := stringField(record, "session_id")
	replaced := false
	for i := range records {
		if stringField(records[i], "session_id") == sessionID {
			records[i] = record
			replaced = true
			break
		}
	}
	if !replaced {
		records = append(records, record)
	}
	return s.writeArrayUnlocked("sessions.json", records)
}

func (s *Store) RemoveSession(sessionID string) (Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records, err := s.readArrayUnlocked("sessions.json")
	if err != nil {
		return nil, false, err
	}
	out := make([]Record, 0, len(records))
	var removed Record
	for _, r := range records {
		if stringField(r, "session_id") == sessionID {
			removed = r
			continue
		}
		out = append(out, r)
	}
	if removed == nil {
		return nil, false, nil
	}
	return removed, true, s.writeArrayUnlocked("sessions.json", out)
}

// UpdateSessionFields performs a conditional read-modify-write while holding
// the store mutex. The callback may inspect and mutate the latest record; an
// error aborts without writing. This is the CAS boundary for security state
// that must not lose concurrent task/session updates.
func (s *Store) UpdateSessionFields(
	sessionID string, update func(Record) error,
) (Record, bool, error) {
	if update == nil {
		return nil, false, fmt.Errorf("session update callback is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	records, err := s.readArrayUnlocked("sessions.json")
	if err != nil {
		return nil, false, err
	}
	for i := range records {
		if stringField(records[i], "session_id") != sessionID {
			continue
		}
		if err := update(records[i]); err != nil {
			return records[i], true, err
		}
		if err := s.writeArrayUnlocked("sessions.json", records); err != nil {
			return nil, true, err
		}
		return records[i], true, nil
	}
	return nil, false, nil
}

func (s *Store) AppendTask(record Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	records, err := s.readArrayUnlocked("tasks.json")
	if err != nil {
		return err
	}
	records = append(records, record)
	return s.writeArrayUnlocked("tasks.json", records)
}

// AppendTaskOnce atomically publishes a task identity. If the same task id is
// already durable, the existing record is returned without appending a second
// copy. Callers must compare their own request digest before treating an
// existing record as an idempotent retry.
func (s *Store) AppendTaskOnce(record Record) (Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records, err := s.readArrayUnlocked("tasks.json")
	if err != nil {
		return nil, false, err
	}
	taskID := stringField(record, "task_id")
	if taskID == "" {
		return nil, false, fmt.Errorf("task_id is required")
	}
	for _, existing := range records {
		if stringField(existing, "task_id") == taskID {
			return existing, false, nil
		}
	}
	records = append(records, record)
	if err := s.writeArrayUnlocked("tasks.json", records); err != nil {
		return nil, false, err
	}
	return record, true, nil
}

func (s *Store) UpdateTask(taskID string, fields Record) (Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records, err := s.readArrayUnlocked("tasks.json")
	if err != nil {
		return nil, false, err
	}
	for i := range records {
		if stringField(records[i], "task_id") != taskID {
			continue
		}
		for k, v := range fields {
			records[i][k] = v
		}
		records[i]["updated_at"] = nowISO()
		return records[i], true, s.writeArrayUnlocked("tasks.json", records)
	}
	return nil, false, nil
}

// CommitTaskSessionOutcome publishes one provider delivery outcome together
// with the logical-session identity it established. tasks.json and
// sessions.json are separate compatibility files, so a durable write-ahead
// journal is the commit point: after it exists, every Store read replays the
// same final arrays before returning. This removes the crash window where a
// task could say "running" while the native transcript/Claude owner was still
// absent from its session record.
func (s *Store) CommitTaskSessionOutcome(
	taskID string,
	sessionID string,
	update func(task Record, session Record) error,
) (Record, Record, bool, error) {
	if update == nil {
		return nil, nil, false, fmt.Errorf("task/session outcome callback is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.recoverTaskSessionOutcomeUnlocked(); err != nil {
		return nil, nil, false, err
	}
	tasks, err := s.readArrayFileUnlocked("tasks.json")
	if err != nil {
		return nil, nil, false, err
	}
	sessions, err := s.readArrayFileUnlocked("sessions.json")
	if err != nil {
		return nil, nil, false, err
	}
	taskIndex, sessionIndex := -1, -1
	for i := range tasks {
		if stringField(tasks[i], "task_id") == taskID {
			taskIndex = i
			break
		}
	}
	for i := range sessions {
		if stringField(sessions[i], "session_id") == sessionID {
			sessionIndex = i
			break
		}
	}
	if taskIndex < 0 || sessionIndex < 0 {
		return nil, nil, false, nil
	}
	if stringField(tasks[taskIndex], "session_id") != sessionID {
		return nil, nil, true, fmt.Errorf("task/session outcome identity mismatch")
	}
	if err := update(tasks[taskIndex], sessions[sessionIndex]); err != nil {
		return tasks[taskIndex], sessions[sessionIndex], true, err
	}
	timestamp := nowISO()
	tasks[taskIndex]["updated_at"] = timestamp
	sessions[sessionIndex]["updated_at"] = timestamp
	transaction := taskSessionOutcomeTransaction{
		Version: 1, Tasks: tasks, Sessions: sessions,
	}
	if err := s.writeJSONAtomicUnlocked(taskSessionOutcomeJournal, transaction, 0o600); err != nil {
		return nil, nil, true, err
	}
	if err := taskSessionOutcomeFault("after_prepare"); err != nil {
		return nil, nil, true, err
	}
	// Publish the session identity first. Even readers which bypass Store never
	// observe a successful task before its native/owner binding; Store readers
	// additionally replay the journal and see both or neither.
	if err := s.writeArrayFileUnlocked("sessions.json", sessions); err != nil {
		return nil, nil, true, err
	}
	if err := taskSessionOutcomeFault("after_session"); err != nil {
		return nil, nil, true, err
	}
	if err := s.writeArrayFileUnlocked("tasks.json", tasks); err != nil {
		return nil, nil, true, err
	}
	if err := taskSessionOutcomeFault("after_task"); err != nil {
		return nil, nil, true, err
	}
	if err := s.clearTaskSessionOutcomeJournalUnlocked(); err != nil {
		return nil, nil, true, err
	}
	return tasks[taskIndex], sessions[sessionIndex], true, nil
}

func (s *Store) readArray(name string) ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readArrayUnlocked(name)
}

func (s *Store) readArrayUnlocked(name string) ([]Record, error) {
	if err := s.recoverTaskSessionOutcomeUnlocked(); err != nil {
		return nil, err
	}
	return s.readArrayFileUnlocked(name)
}

func (s *Store) readArrayFileUnlocked(name string) ([]Record, error) {
	p := filepath.Join(s.dataDir, name)
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return []Record{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(trimSpace(b)) == 0 {
		return []Record{}, nil
	}
	var records []Record
	if err := json.Unmarshal(b, &records); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	return records, nil
}

func stringField(r Record, key string) string {
	if v, ok := r[key].(string); ok {
		return v
	}
	return ""
}

func nowISO() string {
	return timeNow().UTC().Format("2006-01-02T15:04:05.999999999Z07:00")
}

var timeNow = func() time.Time { return time.Now() }

func (s *Store) writeArray(name string, records []Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeArrayUnlocked(name, records)
}

func (s *Store) writeArrayUnlocked(name string, records []Record) error {
	if err := s.recoverTaskSessionOutcomeUnlocked(); err != nil {
		return err
	}
	return s.writeArrayFileUnlocked(name, records)
}

func (s *Store) writeArrayFileUnlocked(name string, records []Record) error {
	return s.writeJSONAtomicUnlocked(name, records, 0o644)
}

func (s *Store) writeJSONAtomicUnlocked(name string, value any, mode os.FileMode) error {
	if err := os.MkdirAll(s.dataDir, 0o755); err != nil {
		return err
	}
	p := filepath.Join(s.dataDir, name)
	tmp := p + ".tmp"
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	cleanup := func(primary error, closeFile bool) error {
		if closeFile {
			primary = errors.Join(primary, f.Close())
		}
		if removeErr := os.Remove(tmp); removeErr != nil && !os.IsNotExist(removeErr) {
			primary = errors.Join(primary, fmt.Errorf("remove atomic temp %s: %w", tmp, removeErr))
		}
		return primary
	}
	if err := f.Chmod(mode); err != nil {
		return cleanup(err, true)
	}
	if written, err := f.Write(b); err != nil {
		return cleanup(err, true)
	} else if written != len(b) {
		return cleanup(io.ErrShortWrite, true)
	}
	if err := atomicWriteFault("after_write", p); err != nil {
		return cleanup(err, true)
	}
	if err := f.Sync(); err != nil {
		return cleanup(err, true)
	}
	if err := atomicWriteFault("after_file_sync", p); err != nil {
		return cleanup(err, true)
	}
	if err := f.Close(); err != nil {
		return cleanup(err, false)
	}
	if err := os.Rename(tmp, p); err != nil {
		return cleanup(err, false)
	}
	if err := atomicWriteFault("after_rename", p); err != nil {
		return err
	}
	return syncDirectory(s.dataDir)
}

func (s *Store) recoverTaskSessionOutcomeUnlocked() error {
	path := filepath.Join(s.dataDir, taskSessionOutcomeJournal)
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var transaction taskSessionOutcomeTransaction
	if err := json.Unmarshal(b, &transaction); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if transaction.Version != 1 || transaction.Tasks == nil || transaction.Sessions == nil {
		return fmt.Errorf("invalid task/session outcome journal: %s", path)
	}
	if err := s.writeArrayFileUnlocked("sessions.json", transaction.Sessions); err != nil {
		return err
	}
	if err := s.writeArrayFileUnlocked("tasks.json", transaction.Tasks); err != nil {
		return err
	}
	return s.clearTaskSessionOutcomeJournalUnlocked()
}

func (s *Store) clearTaskSessionOutcomeJournalUnlocked() error {
	path := filepath.Join(s.dataDir, taskSessionOutcomeJournal)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return syncDirectory(s.dataDir)
}

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return errors.Join(err, f.Close())
	}
	return f.Close()
}

func trimSpace(b []byte) []byte {
	i, j := 0, len(b)
	for i < j && (b[i] == ' ' || b[i] == '\n' || b[i] == '\r' || b[i] == '\t') {
		i++
	}
	for j > i && (b[j-1] == ' ' || b[j-1] == '\n' || b[j-1] == '\r' || b[j-1] == '\t') {
		j--
	}
	return b[i:j]
}

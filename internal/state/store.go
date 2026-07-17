package state

import (
	"encoding/json"
	"fmt"
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
	records, err := s.Sessions()
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
	records, err := s.Sessions()
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
	return s.SaveSessions(records)
}

func (s *Store) RemoveSession(sessionID string) (Record, bool, error) {
	records, err := s.Sessions()
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
	return removed, true, s.SaveSessions(out)
}

func (s *Store) AppendTask(record Record) error {
	records, err := s.Tasks()
	if err != nil {
		return err
	}
	records = append(records, record)
	return s.SaveTasks(records)
}

func (s *Store) UpdateTask(taskID string, fields Record) (Record, bool, error) {
	records, err := s.Tasks()
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
		return records[i], true, s.SaveTasks(records)
	}
	return nil, false, nil
}

func (s *Store) readArray(name string) ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	if err := os.MkdirAll(s.dataDir, 0o755); err != nil {
		return err
	}
	p := filepath.Join(s.dataDir, name)
	tmp := p + ".tmp"
	b, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
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

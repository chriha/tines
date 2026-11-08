// Package state persists run history (separate from config so the config
// stays a clean, portable description of intent).
package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

type TargetState struct {
	LastRun     time.Time `json:"last_run,omitempty"`
	LastOK      time.Time `json:"last_ok,omitempty"`
	LastStatus  string    `json:"last_status,omitempty"` // ok | failed | cancelled
	LastMessage string    `json:"last_message,omitempty"`
	LastVerify  time.Time `json:"last_verify,omitempty"`
	VerifyOK    bool      `json:"verify_ok,omitempty"`
	CheckedAt   time.Time `json:"checked_at,omitempty"`
	Behind      int       `json:"behind"` // files a sync would touch; -1 unknown
	BehindBytes int64     `json:"behind_bytes,omitempty"`
}

type StreamState struct {
	LastSourceFiles int64                   `json:"last_source_files,omitempty"`
	Targets         map[string]*TargetState `json:"targets"`
}

type Store struct {
	mu   sync.Mutex
	path string
	Data struct {
		Streams map[string]*StreamState `json:"streams"`
	}
}

func Dir() string {
	if d := os.Getenv("TINES_STATE_DIR"); d != "" {
		return d
	}
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "tines")
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".local", "state", "tines")
}

func Open() (*Store, error) {
	s := &Store{path: filepath.Join(Dir(), "state.json")}
	s.Data.Streams = map[string]*StreamState{}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.Data); err != nil {
		return s, nil // corrupt state is not fatal; it is only a cache
	}
	if s.Data.Streams == nil {
		s.Data.Streams = map[string]*StreamState{}
	}
	return s, nil
}

// Update mutates the target state under lock and saves.
func (s *Store) Update(streamID, targetID string, fn func(*StreamState, *TargetState)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.stream(streamID)
	ts := ss.Targets[targetID]
	if ts == nil {
		ts = &TargetState{Behind: -1}
		ss.Targets[targetID] = ts
	}
	fn(ss, ts)
	_ = s.save()
}

func (s *Store) stream(id string) *StreamState {
	ss := s.Data.Streams[id]
	if ss == nil {
		ss = &StreamState{Targets: map[string]*TargetState{}}
		s.Data.Streams[id] = ss
	}
	if ss.Targets == nil {
		ss.Targets = map[string]*TargetState{}
	}
	return ss
}

// Get returns a copy of the target state (Behind=-1 when unknown).
func (s *Store) Get(streamID, targetID string) (TargetState, StreamState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.stream(streamID)
	if ts := ss.Targets[targetID]; ts != nil {
		return *ts, StreamState{LastSourceFiles: ss.LastSourceFiles}
	}
	return TargetState{Behind: -1}, StreamState{LastSourceFiles: ss.LastSourceFiles}
}

func (s *Store) save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s.Data, "", "  ")
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// ---- per-stream run lock ---------------------------------------------

type Lock struct{ f *os.File }

var ErrLocked = errors.New("stream is already running (in this or another tines process)")

func LockStream(id string) (*Lock, error) {
	dir := filepath.Join(Dir(), "locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, id+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, ErrLocked
	}
	return &Lock{f}, nil
}

func (l *Lock) Unlock() {
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	l.f.Close()
}

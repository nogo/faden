// Package jsonl is a faden.Store that keeps one append-only file per session
// in a directory. Reach for it when a host has no database. Every write is
// synced before it returns, a torn last line is cut at open, and a lock file
// lets one process own the directory.
package jsonl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/nogo/faden"
)

const lockName = "LOCK"

// record is one line of a session file.
type record struct {
	Type    string          `json:"type"` // "session", "turn", "event" or "cursor"
	Session *faden.Session  `json:"session,omitempty"`
	Turn    *faden.Turn     `json:"turn,omitempty"`
	Event   *faden.Event    `json:"event,omitempty"`
	Backend string          `json:"backend,omitempty"`
	Cursor  json.RawMessage `json:"cursor,omitempty"`
}

type session struct {
	meta   faden.Session
	file   *os.File
	size   int64
	turns  []string // request ids, oldest first
	events []faden.Event
}

// Store implements faden.Store.
type Store struct {
	dir  string
	lock *os.File

	mu       sync.Mutex // guards everything below
	sessions map[string]*session
	turns    map[string]faden.Turn // by request id
}

// Open locks dir (creating it if needed) and loads every session in it. It
// returns faden.ErrLocked if another owner holds the lock.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("jsonl: open: %w", err)
	}
	lock, err := lockFile(filepath.Join(dir, lockName))
	if err != nil {
		return nil, err
	}
	s := &Store{dir: dir, lock: lock, sessions: map[string]*session{}, turns: map[string]faden.Turn{}}
	names, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		lock.Close()
		return nil, fmt.Errorf("jsonl: open: %w", err)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := s.load(name); err != nil {
			s.Close()
			return nil, err
		}
	}
	return s, nil
}

// load replays one session file and cuts a torn last line.
func (s *Store) load(name string) error {
	f, err := os.OpenFile(name, os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("jsonl: load %s: %w", name, err)
	}
	data, err := os.ReadFile(name)
	if err != nil {
		f.Close()
		return fmt.Errorf("jsonl: load %s: %w", name, err)
	}
	good := int64(bytes.LastIndexByte(data, '\n') + 1)
	if good < int64(len(data)) {
		if err := f.Truncate(good); err != nil {
			f.Close()
			return fmt.Errorf("jsonl: cut torn line of %s: %w", name, err)
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return fmt.Errorf("jsonl: cut torn line of %s: %w", name, err)
		}
	}
	sess := &session{file: f, size: good}
	for n, line := range bytes.Split(data[:good], []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var rec record
		if err := json.Unmarshal(line, &rec); err != nil {
			f.Close()
			return fmt.Errorf("jsonl: %s line %d: %w", name, n+1, err)
		}
		if err := s.apply(sess, rec); err != nil {
			f.Close()
			return fmt.Errorf("jsonl: %s line %d: %w", name, n+1, err)
		}
	}
	if sess.meta.ID == "" {
		f.Close()
		return fmt.Errorf("jsonl: %s has no session record", name)
	}
	s.sessions[sess.meta.ID] = sess
	return nil
}

func (s *Store) apply(sess *session, rec record) error {
	switch rec.Type {
	case "session":
		if rec.Session == nil {
			return errors.New("session record without session")
		}
		cursors := sess.meta.Cursors
		sess.meta = *rec.Session
		sess.meta.Cursors = cursors
	case "turn":
		if rec.Turn == nil {
			return errors.New("turn record without turn")
		}
		if _, known := s.turns[rec.Turn.RequestID]; !known {
			sess.turns = append(sess.turns, rec.Turn.RequestID)
		}
		s.turns[rec.Turn.RequestID] = *rec.Turn
	case "event":
		if rec.Event == nil {
			return errors.New("event record without event")
		}
		sess.events = append(sess.events, *rec.Event)
	case "cursor":
		if sess.meta.Cursors == nil {
			sess.meta.Cursors = map[string]json.RawMessage{}
		}
		sess.meta.Cursors[rec.Backend] = rec.Cursor
	default:
		return fmt.Errorf("unknown record type %q", rec.Type)
	}
	return nil
}

// write appends records to the session file in one write and syncs it, then
// applies them to memory. On a failed write the file is cut back.
func (s *Store) write(sess *session, recs ...record) error {
	var buf []byte
	for _, rec := range recs {
		line, err := json.Marshal(rec)
		if err != nil {
			return fmt.Errorf("jsonl: encode: %w", err)
		}
		buf = append(append(buf, line...), '\n')
	}
	if _, err := sess.file.Write(buf); err != nil {
		sess.file.Truncate(sess.size)
		return fmt.Errorf("jsonl: append: %w", err)
	}
	if err := sess.file.Sync(); err != nil {
		return fmt.Errorf("jsonl: sync: %w", err)
	}
	sess.size += int64(len(buf))
	for _, rec := range recs {
		if err := s.apply(sess, rec); err != nil {
			return fmt.Errorf("jsonl: apply: %w", err)
		}
	}
	return nil
}

func (s *Store) create(id string) (*session, error) {
	name := filepath.Join(s.dir, url.PathEscape(id)+".jsonl")
	f, err := os.OpenFile(name, os.O_RDWR|os.O_APPEND|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, fmt.Errorf("jsonl: create session %s: %w", id, err)
	}
	if d, err := os.Open(s.dir); err == nil {
		d.Sync()
		d.Close()
	}
	return &session{file: f}, nil
}

// SubmitTurn implements faden.Store.
func (s *Store) SubmitTurn(_ context.Context, t faden.Turn) (faden.Turn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if known, ok := s.turns[t.RequestID]; ok {
		return known, nil
	}
	if t.RequestID == "" || t.Session == "" {
		return faden.Turn{}, errors.New("jsonl: submit: request id and session are required")
	}
	t.Status, t.Attempts, t.Error = faden.Pending, 0, ""
	t.Created = time.Now().UTC()
	sess, ok := s.sessions[t.Session]
	meta := faden.Session{ID: t.Session, Backend: t.Backend, Created: t.Created}
	if !ok {
		var err error
		if sess, err = s.create(t.Session); err != nil {
			return faden.Turn{}, err
		}
	} else if sess.meta.Backend != t.Backend {
		meta.Created = sess.meta.Created
	}
	recs := []record{{Type: "turn", Turn: &t}}
	if !ok || sess.meta.Backend != t.Backend {
		recs = append([]record{{Type: "session", Session: &meta}}, recs...)
	}
	if err := s.write(sess, recs...); err != nil {
		if !ok {
			sess.file.Close()
		}
		return faden.Turn{}, err
	}
	s.sessions[t.Session] = sess
	return t, nil
}

// SaveTurn implements faden.Store.
func (s *Store) SaveTurn(_ context.Context, t faden.Turn) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	known, ok := s.turns[t.RequestID]
	if !ok {
		return fmt.Errorf("jsonl: save turn %s: %w", t.RequestID, faden.ErrNotFound)
	}
	known.Status, known.Attempts, known.Error = t.Status, t.Attempts, t.Error
	return s.write(s.sessions[known.Session], record{Type: "turn", Turn: &known})
}

// Turn implements faden.Store.
func (s *Store) Turn(_ context.Context, requestID string) (faden.Turn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.turns[requestID]
	if !ok {
		return faden.Turn{}, fmt.Errorf("jsonl: turn %s: %w", requestID, faden.ErrNotFound)
	}
	return t, nil
}

// Unfinished implements faden.Store.
func (s *Store) Unfinished(_ context.Context) ([]faden.Turn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []faden.Turn
	for _, sess := range s.sessions {
		for _, id := range sess.turns {
			if t := s.turns[id]; !t.Status.Final() {
				out = append(out, t)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Created.Equal(out[j].Created) {
			return out[i].Created.Before(out[j].Created)
		}
		return out[i].RequestID < out[j].RequestID
	})
	return out, nil
}

// Session implements faden.Store.
func (s *Store) Session(_ context.Context, id string) (faden.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return faden.Session{}, fmt.Errorf("jsonl: session %s: %w", id, faden.ErrNotFound)
	}
	out := sess.meta
	out.Cursors = make(map[string]json.RawMessage, len(sess.meta.Cursors))
	for k, v := range sess.meta.Cursors {
		out.Cursors[k] = v
	}
	return out, nil
}

// AppendEvent implements faden.Store.
func (s *Store) AppendEvent(_ context.Context, e faden.Event) (faden.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[e.Session]
	if !ok {
		return faden.Event{}, fmt.Errorf("jsonl: append event: session %s: %w", e.Session, faden.ErrNotFound)
	}
	e.Seq = int64(len(sess.events)) + 1
	if err := s.write(sess, record{Type: "event", Event: &e}); err != nil {
		return faden.Event{}, err
	}
	return e, nil
}

// Events implements faden.Store.
func (s *Store) Events(_ context.Context, id string, after int64) ([]faden.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return nil, fmt.Errorf("jsonl: events: session %s: %w", id, faden.ErrNotFound)
	}
	if after < 0 {
		after = 0
	}
	if after >= int64(len(sess.events)) {
		return nil, nil
	}
	return append([]faden.Event(nil), sess.events[after:]...), nil
}

// SetCursor implements faden.Store.
func (s *Store) SetCursor(_ context.Context, id, backend string, cursor json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return fmt.Errorf("jsonl: set cursor: session %s: %w", id, faden.ErrNotFound)
	}
	return s.write(sess, record{Type: "cursor", Backend: backend, Cursor: cursor})
}

// Close closes the files and releases the lock.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var errs []error
	for _, sess := range s.sessions {
		errs = append(errs, sess.file.Close())
	}
	s.sessions = map[string]*session{}
	errs = append(errs, s.lock.Close())
	return errors.Join(errs...)
}

var _ faden.Store = (*Store)(nil)

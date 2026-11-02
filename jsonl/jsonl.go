// Package jsonl is a faden.Store that keeps one append-only file per session
// in a directory. Reach for it when a host has no database. Every write is
// synced before it returns, a torn last line is cut at open, and a lock file
// lets one process own the directory.
//
// Sessions, turns and cursors stay in memory; events stay in the file. The
// store keeps one file offset per event and reads events when asked, so its
// memory does not grow with what tools and models wrote.
package jsonl

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
	events []int64  // file offset of each event line; event n is at events[n-1]
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
	slices.Sort(names)
	for _, name := range names {
		if err := s.load(name); err != nil {
			s.Close()
			return nil, err
		}
	}
	return s, nil
}

// load replays one session file and cuts a torn last line. Event bodies are
// checked but not kept.
func (s *Store) load(name string) error {
	f, err := os.OpenFile(name, os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("jsonl: load %s: %w", name, err)
	}
	sess := &session{file: f}
	n := 0
	good, err := lines(f, 0, func(off int64, line []byte) error {
		n++
		var rec struct {
			record
			Event *struct{} `json:"event"` // validates the body without keeping it
		}
		if err := json.Unmarshal(line, &rec); err != nil {
			return err
		}
		if rec.Type == "event" && rec.Event == nil {
			return errors.New("event record without event")
		}
		return s.apply(sess, rec.record, off)
	})
	if err != nil {
		f.Close()
		return fmt.Errorf("jsonl: %s line %d: %w", name, n, err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("jsonl: load %s: %w", name, err)
	}
	if good < info.Size() {
		if err := f.Truncate(good); err != nil {
			f.Close()
			return fmt.Errorf("jsonl: cut torn line of %s: %w", name, err)
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return fmt.Errorf("jsonl: cut torn line of %s: %w", name, err)
		}
	}
	if good == 0 {
		if err := f.Close(); err != nil {
			return fmt.Errorf("jsonl: close interrupted session %s: %w", name, err)
		}
		if err := os.Remove(name); err != nil {
			return fmt.Errorf("jsonl: remove interrupted session %s: %w", name, err)
		}
		return nil
	}
	if sess.meta.ID == "" {
		f.Close()
		return fmt.Errorf("jsonl: %s has no session record", name)
	}
	sess.size = good
	s.sessions[sess.meta.ID] = sess
	return nil
}

// lines calls fn with each complete line of r, without its newline, and the
// line's offset; r starts at offset base. It returns the offset after the
// last complete line. A last line without a newline is torn and not passed.
func lines(r io.Reader, base int64, fn func(off int64, line []byte) error) (int64, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	off := base
	for {
		line, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			// Only a line longer than the buffer is copied.
			long := slices.Clone(line)
			for errors.Is(err, bufio.ErrBufferFull) {
				line, err = br.ReadSlice('\n')
				long = append(long, line...)
			}
			line = long
		}
		if errors.Is(err, io.EOF) {
			return off, nil
		}
		if err != nil {
			return off, err
		}
		if len(line) > 1 {
			if err := fn(off, line[:len(line)-1]); err != nil {
				return off, err
			}
		}
		off += int64(len(line))
	}
}

// apply adds a record that starts at file offset off to memory.
func (s *Store) apply(sess *session, rec record, off int64) error {
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
		sess.events = append(sess.events, off)
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
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf) // one line per record
	starts := make([]int64, len(recs))
	for i, rec := range recs {
		starts[i] = sess.size + int64(buf.Len())
		if err := enc.Encode(rec); err != nil {
			return fmt.Errorf("jsonl: encode: %w", err)
		}
	}
	if _, err := sess.file.Write(buf.Bytes()); err != nil {
		sess.file.Truncate(sess.size)
		return fmt.Errorf("jsonl: append: %w", err)
	}
	if err := sess.file.Sync(); err != nil {
		return fmt.Errorf("jsonl: sync: %w", err)
	}
	sess.size += int64(buf.Len())
	for i, rec := range recs {
		if err := s.apply(sess, rec, starts[i]); err != nil {
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
	slices.SortFunc(out, func(a, b faden.Turn) int {
		if c := a.Created.Compare(b.Created); c != 0 {
			return c
		}
		return strings.Compare(a.RequestID, b.RequestID)
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
	out.Cursors = maps.Clone(sess.meta.Cursors)
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
	after = max(after, 0)
	if after >= int64(len(sess.events)) {
		return nil, nil
	}
	offsets := sess.events[after:]
	out := make([]faden.Event, 0, len(offsets))
	start := offsets[0]
	_, err := lines(io.NewSectionReader(sess.file, start, sess.size-start), start, func(off int64, line []byte) error {
		if len(out) == len(offsets) || off != offsets[len(out)] {
			return nil // a turn, session or cursor record
		}
		var rec struct {
			Event faden.Event `json:"event"`
		}
		if err := json.Unmarshal(line, &rec); err != nil {
			return err
		}
		out = append(out, rec.Event)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("jsonl: events: session %s: %w", id, err)
	}
	if len(out) != len(offsets) {
		return nil, fmt.Errorf("jsonl: events: session %s: file ends before event %d", id, int(after)+len(out)+1)
	}
	return out, nil
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

// Package jsonl is a faden.Store that keeps one append-only file per session
// in a directory. Reach for it when a host has no database. Every write is
// synced before it returns.
//
// Several processes may open one directory. A process owns a session while
// it holds an flock on the session's file (Claim); the kernel drops the lock
// when the process dies, so another process takes over at once. A process
// reads the sessions it does not own from disk, so it sees what their
// owners wrote. A short flock on the directory's LOCK file orders what
// several processes must agree on: a new session, a submitted turn (a
// request id is unique across processes) and the cut of a torn last line.
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
	"iter"
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
	path   string
	file   *os.File
	size   int64    // end of the last complete line read; bytes beyond it are a torn line or not read yet
	owned  bool     // this store holds the flock on file
	turns  []string // request ids, oldest first
	events []int64  // file offset of each event line; event n is at events[n-1]
}

// Store implements faden.Store.
type Store struct {
	dir  string
	lock *os.File // the directory's LOCK file, locked while dirDepth > 0

	mu       sync.Mutex // guards everything below
	dirDepth int
	sessions map[string]*session
	turns    map[string]faden.Turn // by request id
}

// Open opens dir, creating it if needed, and loads the sessions in it. It
// removes session files that a crash left without a first record.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("jsonl: open: %w", err)
	}
	lock, err := os.OpenFile(filepath.Join(dir, lockName), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("jsonl: open: %w", err)
	}
	s := &Store{dir: dir, lock: lock, sessions: map[string]*session{}, turns: map[string]faden.Turn{}}
	s.mu.Lock()
	err = s.lockDir()
	if err == nil {
		err = s.refresh(true)
		s.unlockDir()
	}
	s.mu.Unlock()
	if err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// lockDir takes the directory lock, waiting for other processes. It nests.
// The caller holds s.mu.
func (s *Store) lockDir() error {
	if s.dirDepth == 0 {
		if err := lockFile(s.lock, true); err != nil {
			return err
		}
	}
	s.dirDepth++
	return nil
}

func (s *Store) unlockDir() {
	s.dirDepth--
	if s.dirDepth == 0 {
		unlockFile(s.lock)
	}
}

func (s *Store) path(id string) string {
	return filepath.Join(s.dir, url.PathEscape(id)+".jsonl")
}

// refresh brings memory up to date with the directory: it reads what other
// processes appended to known sessions and loads sessions it has not seen.
// With cleanup it removes session files without a first record; the caller
// then holds the directory lock, so no creator is half way. The caller holds
// s.mu.
func (s *Store) refresh(cleanup bool) error {
	names, err := filepath.Glob(filepath.Join(s.dir, "*.jsonl"))
	if err != nil {
		return fmt.Errorf("jsonl: refresh: %w", err)
	}
	slices.Sort(names)
	known := make(map[string]*session, len(s.sessions))
	for _, sess := range s.sessions {
		known[sess.path] = sess
	}
	for _, name := range names {
		if sess, ok := known[name]; ok {
			if err := s.catchUp(sess); err != nil {
				return err
			}
			continue
		}
		if _, err := s.load(name, cleanup); err != nil {
			return err
		}
	}
	return nil
}

// load opens one session file and reads it. It returns nil for a file that
// does not exist or has no complete first record: that is a session whose
// creation is not finished or was interrupted.
func (s *Store) load(name string, cleanup bool) (*session, error) {
	f, err := os.OpenFile(name, os.O_RDWR|os.O_APPEND, 0o644)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("jsonl: load %s: %w", name, err)
	}
	sess := &session{path: name, file: f}
	if err := s.catchUp(sess); err != nil {
		f.Close()
		return nil, err
	}
	if sess.size == 0 {
		if err := f.Close(); err != nil {
			return nil, fmt.Errorf("jsonl: close interrupted session %s: %w", name, err)
		}
		if cleanup {
			if err := os.Remove(name); err != nil {
				return nil, fmt.Errorf("jsonl: remove interrupted session %s: %w", name, err)
			}
		}
		return nil, nil
	}
	if sess.meta.ID == "" {
		f.Close()
		return nil, fmt.Errorf("jsonl: %s has no session record", name)
	}
	s.sessions[sess.meta.ID] = sess
	return sess, nil
}

// catchUp applies the complete lines that were appended to the session file
// since the last read, by this store or another process. A torn last line is
// not applied. Event bodies are checked but not kept.
func (s *Store) catchUp(sess *session) error {
	info, err := sess.file.Stat()
	if err != nil {
		return fmt.Errorf("jsonl: stat %s: %w", sess.path, err)
	}
	if info.Size() == sess.size {
		return nil
	}
	good, err := lines(io.NewSectionReader(sess.file, sess.size, info.Size()-sess.size), sess.size, func(off int64, line []byte) error {
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
	sess.size = good
	if err != nil {
		return fmt.Errorf("jsonl: %s at offset %d: %w", sess.path, good, err)
	}
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

// write appends records to the end of the session file in one write and
// syncs it, then reads the file again, so memory holds what the disk holds,
// records of other processes included. On a failed write the file is cut back
// when nobody else wrote meanwhile.
func (s *Store) write(sess *session, recs ...record) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf) // one line per record
	for _, rec := range recs {
		if err := enc.Encode(rec); err != nil {
			return fmt.Errorf("jsonl: encode: %w", err)
		}
	}
	before, err := sess.file.Stat()
	if err != nil {
		return fmt.Errorf("jsonl: append: %w", err)
	}
	if n, err := sess.file.Write(buf.Bytes()); err != nil {
		if after, serr := sess.file.Stat(); serr == nil && after.Size() == before.Size()+int64(n) {
			sess.file.Truncate(before.Size())
		}
		return fmt.Errorf("jsonl: append: %w", err)
	}
	if err := sess.file.Sync(); err != nil {
		return fmt.Errorf("jsonl: sync: %w", err)
	}
	if err := s.catchUp(sess); err != nil {
		return fmt.Errorf("jsonl: apply: %w", err)
	}
	return nil
}

// claim takes the session's flock without waiting and cuts a torn last line
// a dead writer left. The caller holds s.mu.
func (s *Store) claim(sess *session) error {
	if sess.owned {
		return nil
	}
	if err := lockFile(sess.file, false); err != nil {
		return err
	}
	// Under the directory lock no submitter is half way through an append,
	// so bytes past the last complete line belong to a dead writer.
	err := s.lockDir()
	if err == nil {
		err = s.heal(sess)
		s.unlockDir()
	}
	if err != nil {
		unlockFile(sess.file)
		return err
	}
	sess.owned = true
	return nil
}

func (s *Store) heal(sess *session) error {
	if err := s.catchUp(sess); err != nil {
		return err
	}
	info, err := sess.file.Stat()
	if err != nil {
		return fmt.Errorf("jsonl: stat %s: %w", sess.path, err)
	}
	if info.Size() > sess.size {
		if err := sess.file.Truncate(sess.size); err != nil {
			return fmt.Errorf("jsonl: cut torn line of %s: %w", sess.path, err)
		}
		if err := sess.file.Sync(); err != nil {
			return fmt.Errorf("jsonl: cut torn line of %s: %w", sess.path, err)
		}
	}
	return nil
}

func (s *Store) release(sess *session) {
	if sess.owned {
		unlockFile(sess.file)
		sess.owned = false
	}
}

// asOwner runs fn while this store owns the session. A store without a claim
// takes one for the single write, so a session owned elsewhere fails with
// faden.ErrLocked and an unowned one stays consistent.
func (s *Store) asOwner(sess *session, fn func() error) error {
	if !sess.owned {
		if err := s.claim(sess); err != nil {
			return err
		}
		defer s.release(sess)
	}
	if err := s.catchUp(sess); err != nil {
		return err
	}
	return fn()
}

// lookup returns a session, up to date with its file.
func (s *Store) lookup(id string) (*session, error) {
	sess, ok := s.sessions[id]
	if ok {
		return sess, s.catchUp(sess)
	}
	sess, err := s.load(s.path(id), false)
	if err != nil {
		return nil, err
	}
	if sess == nil || sess.meta.ID != id {
		return nil, fmt.Errorf("session %s: %w", id, faden.ErrNotFound)
	}
	return sess, nil
}

// create starts the file of a new session. The caller holds the directory
// lock and has refreshed, so a file that is there is an interrupted creation.
func (s *Store) create(id string) (*session, error) {
	name := s.path(id)
	f, err := os.OpenFile(name, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("jsonl: create session %s: %w", id, err)
	}
	if err := f.Truncate(0); err != nil {
		f.Close()
		return nil, fmt.Errorf("jsonl: create session %s: %w", id, err)
	}
	if d, err := os.Open(s.dir); err == nil {
		d.Sync()
		d.Close()
	}
	return &session{path: name, file: f}, nil
}

// SubmitTurn implements faden.Store. It holds the directory lock, so the same
// request id from two processes is stored once.
func (s *Store) SubmitTurn(_ context.Context, t faden.Turn) (faden.Turn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.lockDir(); err != nil {
		return faden.Turn{}, err
	}
	defer s.unlockDir()
	if err := s.refresh(false); err != nil {
		return faden.Turn{}, err
	}
	if known, ok := s.turns[t.RequestID]; ok {
		return copyTurn(known), nil
	}
	if t.RequestID == "" || t.Session == "" {
		return faden.Turn{}, errors.New("jsonl: submit: request id and session are required")
	}
	t.Files = slices.Clone(t.Files) // the caller keeps its own slice
	t.Progress = faden.Progress{Status: faden.Pending}
	t.Created = time.Now().UTC()
	sess, ok := s.sessions[t.Session]
	recs := []record{{Type: "turn", Turn: &t}}
	if !ok {
		var err error
		if sess, err = s.create(t.Session); err != nil {
			return faden.Turn{}, err
		}
		meta := faden.Session{ID: t.Session, Created: t.Created}
		recs = append([]record{{Type: "session", Session: &meta}}, recs...)
		if err := s.write(sess, recs...); err != nil {
			sess.file.Close()
			return faden.Turn{}, err
		}
		s.sessions[t.Session] = sess
		return copyTurn(t), nil
	}
	// A turn for a session another process owns is appended without its
	// lock: the owner reads it with its next catch-up. Otherwise this store
	// takes the claim for the append, to cut a torn last line first.
	had := sess.owned
	err := s.claim(sess)
	switch {
	case errors.Is(err, faden.ErrLocked):
		err = s.write(sess, recs...)
	case err == nil:
		err = s.write(sess, recs...)
		if !had {
			s.release(sess)
		}
	}
	if err != nil {
		return faden.Turn{}, err
	}
	return copyTurn(t), nil
}

// turn returns a stored turn, reading the file of its session first.
func (s *Store) turn(requestID string) (faden.Turn, error) {
	t, ok := s.turns[requestID]
	if ok {
		if sess, known := s.sessions[t.Session]; known {
			if err := s.catchUp(sess); err != nil {
				return faden.Turn{}, err
			}
		}
	} else if err := s.refresh(false); err != nil {
		return faden.Turn{}, err
	}
	t, ok = s.turns[requestID]
	if !ok {
		return faden.Turn{}, fmt.Errorf("jsonl: turn %s: %w", requestID, faden.ErrNotFound)
	}
	return t, nil
}

// SaveTurn implements faden.Store.
func (s *Store) SaveTurn(_ context.Context, t faden.Turn) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.turn(t.RequestID); err != nil {
		return fmt.Errorf("jsonl: save turn %s: %w", t.RequestID, err)
	}
	sess := s.sessions[s.turns[t.RequestID].Session]
	return s.asOwner(sess, func() error {
		known := s.turns[t.RequestID]
		known.Progress = t.Progress
		known.Usage = slices.Clone(t.Usage) // the caller keeps its own slice
		return s.write(sess, record{Type: "turn", Turn: &known})
	})
}

// copyTurn returns t with its own slices, so a caller cannot change the stored turn.
func copyTurn(t faden.Turn) faden.Turn {
	t.Usage = slices.Clone(t.Usage)
	t.Files = slices.Clone(t.Files)
	return t
}

// Turn implements faden.Store.
func (s *Store) Turn(_ context.Context, requestID string) (faden.Turn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.turn(requestID)
	return copyTurn(t), err
}

// Unfinished implements faden.Store. It reads the directory, so it sees the
// turns other processes submitted and finished.
func (s *Store) Unfinished(_ context.Context) ([]faden.Turn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refresh(false); err != nil {
		return nil, err
	}
	var out []faden.Turn
	for _, sess := range s.sessions {
		for _, id := range sess.turns {
			if t := s.turns[id]; !t.Status.Final() {
				out = append(out, copyTurn(t))
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
	sess, err := s.lookup(id)
	if err != nil {
		return faden.Session{}, fmt.Errorf("jsonl: %w", err)
	}
	out := sess.meta
	out.Cursors = maps.Clone(sess.meta.Cursors)
	return out, nil
}

// AppendEvent implements faden.Store.
func (s *Store) AppendEvent(_ context.Context, e faden.Event) (faden.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, err := s.lookup(e.Session)
	if err != nil {
		return faden.Event{}, fmt.Errorf("jsonl: append event: %w", err)
	}
	err = s.asOwner(sess, func() error {
		e.Seq = int64(len(sess.events)) + 1
		return s.write(sess, record{Type: "event", Event: &e})
	})
	if err != nil {
		return faden.Event{}, err
	}
	return e, nil
}

// Events implements faden.Store. It takes the lock only to copy the
// session's offsets: lines below the file size it saw never change, so it
// reads them while appends go on.
func (s *Store) Events(ctx context.Context, id string, after int64) iter.Seq2[faden.Event, error] {
	return func(yield func(faden.Event, error) bool) {
		s.mu.Lock()
		sess, err := s.lookup(id)
		var offsets []int64
		var file *os.File
		var size int64
		if err == nil {
			after = max(after, 0)
			if after < int64(len(sess.events)) {
				offsets = sess.events[after:len(sess.events):len(sess.events)]
			}
			file, size = sess.file, sess.size
		}
		s.mu.Unlock()
		if err != nil {
			yield(faden.Event{}, fmt.Errorf("jsonl: events: %w", err))
			return
		}
		if len(offsets) == 0 {
			return
		}
		errStop := errors.New("stop")
		n := 0
		start := offsets[0]
		_, err = lines(io.NewSectionReader(file, start, size-start), start, func(off int64, line []byte) error {
			if n == len(offsets) || off != offsets[n] {
				return nil // a turn, session or cursor record
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			var rec struct {
				Event faden.Event `json:"event"`
			}
			if err := json.Unmarshal(line, &rec); err != nil {
				return err
			}
			n++
			if !yield(rec.Event, nil) {
				return errStop
			}
			return nil
		})
		switch {
		case errors.Is(err, errStop):
		case err != nil:
			yield(faden.Event{}, fmt.Errorf("jsonl: events: session %s: %w", id, err))
		case n != len(offsets):
			yield(faden.Event{}, fmt.Errorf("jsonl: events: session %s: file ends before event %d", id, int(after)+n+1))
		}
	}
}

// SetCursor implements faden.Store.
func (s *Store) SetCursor(_ context.Context, id, backend string, cursor json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, err := s.lookup(id)
	if err != nil {
		return fmt.Errorf("jsonl: set cursor: %w", err)
	}
	return s.asOwner(sess, func() error {
		return s.write(sess, record{Type: "cursor", Backend: backend, Cursor: cursor})
	})
}

// Claim implements faden.Store. The claim is an flock on the session's file.
func (s *Store) Claim(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, err := s.lookup(id)
	if err != nil {
		return fmt.Errorf("jsonl: claim: %w", err)
	}
	return s.claim(sess)
}

// Release implements faden.Store.
func (s *Store) Release(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[id]; ok {
		s.release(sess)
	}
	return nil
}

// Close closes the files, which releases every claim.
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

// NextEvent implements faden.Store.
func (s *Store) NextEvent(ctx context.Context, id string, after int64) (*faden.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, err := s.lookup(id)
	if err != nil {
		return nil, fmt.Errorf("jsonl: next event: %w", err)
	}
	after = max(after, 0)
	if after >= int64(len(sess.events)) {
		return nil, nil
	}
	start := sess.events[after]
	end := sess.size
	if after+1 < int64(len(sess.events)) {
		end = sess.events[after+1]
	}
	line, err := bufio.NewReader(io.NewSectionReader(sess.file, start, end-start)).ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("jsonl: next event: %w", err)
	}
	var rec struct {
		Event faden.Event `json:"event"`
	}
	if err := json.Unmarshal(line, &rec); err != nil {
		return nil, fmt.Errorf("jsonl: next event: %w", err)
	}
	return &rec.Event, nil
}

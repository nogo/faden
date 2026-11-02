// Package sqlite stores faden sessions in a host's SQLite database. Use Open
// with the host's driver and WithTx to submit alongside the host's own writes.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/nogo/faden"
)

// Store implements faden.Store without owning the database or transaction.
// The host configures SQLite's durability settings and keeps one store owner.
type Store struct {
	db     *sql.DB
	tx     *sql.Tx
	prefix string
	mu     *sync.Mutex // serializes writes through this store and its transaction bindings
}

var identifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Open creates missing tables in db. An empty prefix selects faden_. Prefixes
// must contain only ASCII letters, digits and underscores, starting with a
// letter or underscore. Open neither installs a driver nor closes db.
func Open(db *sql.DB, prefix string) (*Store, error) {
	if db == nil {
		return nil, errors.New("sqlite: open: database is required")
	}
	if prefix == "" {
		prefix = "faden_"
	}
	if !identifier.MatchString(prefix) {
		return nil, fmt.Errorf("sqlite: open: invalid table prefix %q", prefix)
	}
	s := &Store{db: db, prefix: prefix, mu: &sync.Mutex{}}
	err := s.write(context.Background(), func(s *Store) error {
		for _, schema := range []string{
			`sessions (id TEXT PRIMARY KEY, data BLOB NOT NULL)`,
			`turns (request_id TEXT PRIMARY KEY, created TEXT NOT NULL, status TEXT NOT NULL, data BLOB NOT NULL)`,
			`events (session TEXT NOT NULL, seq INTEGER NOT NULL, data BLOB NOT NULL, PRIMARY KEY (session, seq))`,
		} {
			if _, err := s.tx.ExecContext(context.Background(), `CREATE TABLE IF NOT EXISTS `+s.prefix+schema); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("sqlite: open: %w", err)
	}
	return s, nil
}

// WithTx returns a store whose operations use tx, which must belong to the
// database passed to Open. The host owns tx and must commit before starting a
// runner on the unbound store. Writes are durable only after that commit.
func (s *Store) WithTx(tx *sql.Tx) *Store {
	return &Store{db: s.db, tx: tx, prefix: s.prefix, mu: s.mu}
}

// Close leaves the host's database and transaction open.
func (s *Store) Close() error { return nil }

func (s *Store) write(ctx context.Context, fn func(*Store) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tx != nil {
		return fn(s)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(s.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) row(ctx context.Context, query string, args ...any) *sql.Row {
	if s.tx != nil {
		return s.tx.QueryRowContext(ctx, query, args...)
	}
	return s.db.QueryRowContext(ctx, query, args...)
}

func (s *Store) rows(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if s.tx != nil {
		return s.tx.QueryContext(ctx, query, args...)
	}
	return s.db.QueryContext(ctx, query, args...)
}

func decode(row *sql.Row, out any) error {
	var data []byte
	if err := row.Scan(&data); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return faden.ErrNotFound
		}
		return err
	}
	return json.Unmarshal(data, out)
}

func (s *Store) put(ctx context.Context, query string, value any, args ...any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = s.tx.ExecContext(ctx, query, append(args, data)...)
	return err
}

// SubmitTurn implements faden.Store.
func (s *Store) SubmitTurn(ctx context.Context, t faden.Turn) (faden.Turn, error) {
	var out faden.Turn
	err := s.write(ctx, func(s *Store) error {
		known, err := s.Turn(ctx, t.RequestID)
		if err == nil {
			out = known
			return nil
		}
		if !errors.Is(err, faden.ErrNotFound) {
			return err
		}
		if t.RequestID == "" || t.Session == "" {
			return errors.New("request id and session are required")
		}
		t.Status, t.Attempts, t.Error = faden.Pending, 0, ""
		t.Created = time.Now().UTC()
		// Encode the turn before creating the session, including for host transactions.
		data, err := json.Marshal(t)
		if err != nil {
			return err
		}
		sess := faden.Session{ID: t.Session, Created: t.Created}
		if err := s.put(ctx, `INSERT INTO `+s.prefix+`sessions (id,data) VALUES (?,?) ON CONFLICT(id) DO NOTHING`, sess, sess.ID); err != nil {
			return err
		}
		_, err = s.tx.ExecContext(ctx, `INSERT INTO `+s.prefix+`turns (request_id,created,status,data) VALUES (?,?,?,?)`, t.RequestID, t.Created.Format("2006-01-02T15:04:05.000000000Z"), t.Status, data)
		out = t
		return err
	})
	if err != nil {
		return faden.Turn{}, fmt.Errorf("sqlite: submit %s: %w", t.RequestID, err)
	}
	return out, nil
}

// Turn implements faden.Store.
func (s *Store) Turn(ctx context.Context, id string) (faden.Turn, error) {
	var t faden.Turn
	if err := decode(s.row(ctx, `SELECT data FROM `+s.prefix+`turns WHERE request_id=?`, id), &t); err != nil {
		return t, fmt.Errorf("sqlite: turn %s: %w", id, err)
	}
	return t, nil
}

// SaveTurn implements faden.Store.
func (s *Store) SaveTurn(ctx context.Context, t faden.Turn) error {
	err := s.write(ctx, func(s *Store) error {
		known, err := s.Turn(ctx, t.RequestID)
		if err != nil {
			return err
		}
		known.Status, known.Attempts, known.Error = t.Status, t.Attempts, t.Error
		return s.put(ctx, `UPDATE `+s.prefix+`turns SET status=?2,data=?3 WHERE request_id=?1`, known, t.RequestID, t.Status)
	})
	if err != nil {
		return fmt.Errorf("sqlite: save turn %s: %w", t.RequestID, err)
	}
	return nil
}

// Session implements faden.Store.
func (s *Store) Session(ctx context.Context, id string) (faden.Session, error) {
	var sess faden.Session
	if err := decode(s.row(ctx, `SELECT data FROM `+s.prefix+`sessions WHERE id=?`, id), &sess); err != nil {
		return sess, fmt.Errorf("sqlite: session %s: %w", id, err)
	}
	return sess, nil
}

func readRows[T any](rows *sql.Rows) ([]T, error) {
	defer rows.Close()
	var out []T
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var item T
		if err := json.Unmarshal(data, &item); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// Unfinished implements faden.Store.
func (s *Store) Unfinished(ctx context.Context) ([]faden.Turn, error) {
	rows, err := s.rows(ctx, `SELECT data FROM `+s.prefix+`turns WHERE status IN (?,?) ORDER BY created,request_id`, faden.Pending, faden.Running)
	if err != nil {
		return nil, fmt.Errorf("sqlite: unfinished: %w", err)
	}
	out, err := readRows[faden.Turn](rows)
	if err != nil {
		return nil, fmt.Errorf("sqlite: unfinished: %w", err)
	}
	return out, nil
}

// AppendEvent implements faden.Store.
func (s *Store) AppendEvent(ctx context.Context, e faden.Event) (faden.Event, error) {
	err := s.write(ctx, func(s *Store) error {
		if _, err := s.Session(ctx, e.Session); err != nil {
			return err
		}
		if err := s.row(ctx, `SELECT COALESCE(MAX(seq),0)+1 FROM `+s.prefix+`events WHERE session=?`, e.Session).Scan(&e.Seq); err != nil {
			return err
		}
		return s.put(ctx, `INSERT INTO `+s.prefix+`events (session,seq,data) VALUES (?,?,?)`, e, e.Session, e.Seq)
	})
	if err != nil {
		return faden.Event{}, fmt.Errorf("sqlite: append event: session %s: %w", e.Session, err)
	}
	return e, nil
}

// Events implements faden.Store.
func (s *Store) Events(ctx context.Context, id string, after int64) ([]faden.Event, error) {
	if _, err := s.Session(ctx, id); err != nil {
		return nil, err
	}
	rows, err := s.rows(ctx, `SELECT data FROM `+s.prefix+`events WHERE session=? AND seq>? ORDER BY seq`, id, after)
	if err != nil {
		return nil, fmt.Errorf("sqlite: events: session %s: %w", id, err)
	}
	out, err := readRows[faden.Event](rows)
	if err != nil {
		return nil, fmt.Errorf("sqlite: events: session %s: %w", id, err)
	}
	return out, nil
}

// SetCursor implements faden.Store.
func (s *Store) SetCursor(ctx context.Context, id, backend string, cursor json.RawMessage) error {
	err := s.write(ctx, func(s *Store) error {
		sess, err := s.Session(ctx, id)
		if err != nil {
			return err
		}
		if sess.Cursors == nil {
			sess.Cursors = map[string]json.RawMessage{}
		}
		sess.Cursors[backend] = cursor
		return s.put(ctx, `UPDATE `+s.prefix+`sessions SET data=?2 WHERE id=?1`, sess, id)
	})
	if err != nil {
		return fmt.Errorf("sqlite: set cursor: session %s: %w", id, err)
	}
	return nil
}

var _ faden.Store = (*Store)(nil)

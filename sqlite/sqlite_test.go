package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/nogo/faden"
	"github.com/nogo/faden/sqlite"
	"github.com/nogo/faden/storetest"
	_ "modernc.org/sqlite"
)

func database(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func open(t *testing.T, db *sql.DB, prefix string) *sqlite.Store {
	t.Helper()
	s, err := sqlite.Open(db, prefix)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// closingStore closes the host connection too, so reopen reads a new connection.
type closingStore struct {
	faden.Store
	db *sql.DB
}

func (s closingStore) Close() error { return errors.Join(s.Store.Close(), s.db.Close()) }

func TestStore(t *testing.T) {
	storetest.Run(t, func(t *testing.T) storetest.Storage {
		path := filepath.Join(t.TempDir(), "sessions.db")
		return storetest.Storage{Open: func() faden.Store {
			db := database(t, path)
			return closingStore{open(t, db, ""), db}
		}}
	})
}

func TestPrefixesAndHostOwnership(t *testing.T) {
	db := database(t, filepath.Join(t.TempDir(), "host.db"))
	a, b := open(t, db, ""), open(t, db, "other_")
	ctx := context.Background()
	turn := faden.Turn{Session: "s", RequestID: "r", Backend: "fake"}
	if _, err := a.SubmitTurn(ctx, turn); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Turn(ctx, "r"); !errors.Is(err, faden.ErrNotFound) {
		t.Fatalf("other store turn: %v", err)
	}
	for _, table := range []string{"faden_sessions", "faden_turns", "faden_events", "other_sessions", "other_turns", "other_events"} {
		var name string
		if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("Close closed host database: %v", err)
	}
	if _, err := sqlite.Open(db, "bad; DROP TABLE faden_turns;"); err == nil {
		t.Fatal("accepted invalid prefix")
	}
	if _, err := sqlite.Open(nil, ""); err == nil {
		t.Fatal("accepted nil database")
	}
	// Opening existing tables preserves their contents.
	if _, err := open(t, db, "").Turn(ctx, "r"); err != nil {
		t.Fatal(err)
	}
}

type backend struct{ ran chan string }

func (b backend) ContinuesWithoutPrompt() bool { return true }

func (b backend) Stop(context.Context, faden.Turn) error { return nil }
func (b backend) Run(_ context.Context, t faden.Turn, _ *faden.Attempt) error {
	b.ran <- t.RequestID
	return nil
}

func TestHostTransactionRollbackAndCommit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	db := database(t, filepath.Join(t.TempDir(), "host.db"))
	s := open(t, db, "")
	if _, err := db.Exec(`CREATE TABLE questions (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"rolled-back", "committed"} {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { tx.Rollback() })
		if _, err := tx.ExecContext(ctx, `INSERT INTO questions VALUES (?)`, id); err != nil {
			t.Fatal(err)
		}
		bound := s.WithTx(tx)
		if _, err := bound.SubmitTurn(ctx, faden.Turn{Session: id, RequestID: id, Backend: "fake"}); err != nil {
			t.Fatal(err)
		}
		if _, err := bound.Turn(ctx, id); err != nil {
			t.Fatal(err)
		}
		if err := bound.Close(); err != nil {
			t.Fatal(err)
		}
		if id == "rolled-back" {
			err = tx.Rollback()
		} else {
			err = tx.Commit()
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Turn(ctx, "rolled-back"); !errors.Is(err, faden.ErrNotFound) {
		t.Fatalf("rolled back turn: %v", err)
	}
	if _, err := s.Session(ctx, "rolled-back"); !errors.Is(err, faden.ErrNotFound) {
		t.Fatalf("rolled back session: %v", err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM questions WHERE id='rolled-back'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rolled back question count=%d: %v", count, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM questions WHERE id='committed'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("committed question count=%d: %v", count, err)
	}
	ran := make(chan string, 2)
	r, err := faden.Open(s, map[string]faden.Backend{"fake": backend{ran}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	done, err := r.Wait(ctx, "committed")
	if err != nil || done.Status != faden.Done {
		t.Fatalf("committed turn=%+v: %v", done, err)
	}
	select {
	case id := <-ran:
		if id != "committed" {
			t.Fatalf("ran %q", id)
		}
	default:
		t.Fatal("committed turn did not run")
	}
	select {
	case id := <-ran:
		t.Fatalf("unexpected run %q", id)
	default:
	}
}

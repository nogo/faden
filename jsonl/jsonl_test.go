package jsonl_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nogo/faden"
	"github.com/nogo/faden/jsonl"
	"github.com/nogo/faden/storetest"
)

func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) storetest.Storage {
		dir := t.TempDir()
		return storetest.Storage{
			Shared: true,
			InterruptCreate: func(session string, torn bool) {
				var data []byte
				if torn {
					data = []byte(`{"type":"session","session":`)
				}
				if err := os.WriteFile(filepath.Join(dir, session+".jsonl"), data, 0o644); err != nil {
					t.Fatal(err)
				}
			},
			Open: func() faden.Store {
				s, err := jsonl.Open(dir)
				if err != nil {
					t.Fatal(err)
				}
				return s
			},
			Tear: func() {
				files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
				if len(files) != 1 {
					t.Fatalf("session files = %v, want 1", files)
				}
				f, err := os.OpenFile(files[0], os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				if _, err := f.WriteString(`{"type":"event","event":{"session":"s1","seq":2,"ki`); err != nil {
					t.Fatal(err)
				}
			},
		}
	})
}

func TestOneFilePerSession(t *testing.T) {
	dir := t.TempDir()
	s, _ := jsonl.Open(dir)
	defer s.Close()
	for _, c := range []struct{ id, session string }{{"r1", "s1"}, {"r2", "s1"}, {"r3", "s2"}} {
		if _, err := s.SubmitTurn(t.Context(), faden.Turn{RequestID: c.id, Session: c.session, Backend: "a"}); err != nil {
			t.Fatal(err)
		}
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if len(files) != 2 {
		t.Fatalf("files = %v, want 2", files)
	}
}

func TestCorruptMiddleLineFailsOpen(t *testing.T) {
	dir := t.TempDir()
	s, _ := jsonl.Open(dir)
	s.SubmitTurn(t.Context(), faden.Turn{RequestID: "r1", Session: "s1", Backend: "a"})
	s.Close()
	file := filepath.Join(dir, "s1.jsonl")
	data, _ := os.ReadFile(file)
	os.WriteFile(file, append([]byte("garbage\n"), data...), 0o644)
	if s, err := jsonl.Open(dir); err == nil {
		s.Close()
		t.Fatal("Open accepted a corrupt middle line")
	}
}

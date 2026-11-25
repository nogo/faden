package jsonl_test

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/nogo/faden"
	"github.com/nogo/faden/jsonl"
)

// An append reads its own line back to stay in step with other processes.
// That read must cost about the line, not a fixed 64 KB buffer.
func TestAppendAllocatesAboutItsLine(t *testing.T) {
	ctx := context.Background()
	s, err := jsonl.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.SubmitTurn(ctx, faden.Turn{RequestID: "r", Session: "s", Backend: "b"}); err != nil {
		t.Fatal(err)
	}
	e := faden.Event{Session: "s", RequestID: "r", Kind: faden.AssistantMessage, Text: "short"}
	res := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			if _, err := s.AppendEvent(ctx, e); err != nil {
				b.Fatal(err)
			}
		}
	})
	if got := res.AllocedBytesPerOp(); got > 16<<10 {
		t.Fatalf("one short append allocates %d bytes", got)
	}
	t.Logf("short append: %d B/op", res.AllocedBytesPerOp())
}

func heapInUse() int64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.HeapAlloc)
}

func finishedTurns(t *testing.T, s *jsonl.Store, sessions, turns int, input string) {
	t.Helper()
	ctx := t.Context()
	for i := range turns {
		id, session := fmt.Sprint("r", i), fmt.Sprint("s", i%sessions)
		if _, err := s.SubmitTurn(ctx, faden.Turn{RequestID: id, Session: session, Input: input, Backend: "b"}); err != nil {
			t.Fatal(err)
		}
		if err := s.SaveTurn(ctx, faden.Turn{RequestID: id, Progress: faden.Progress{Status: faden.Done}}); err != nil {
			t.Fatal(err)
		}
	}
}

// A finished turn stays in the file. The store keeps where it is, not what
// the user wrote.
func TestFinishedTurnsStayOnDisk(t *testing.T) {
	dir := t.TempDir()
	s, err := jsonl.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	const turns = 2000
	finishedTurns(t, s, 50, turns, strings.Repeat("y", 4<<10)) // 8 MB of input
	s.Close()
	before := heapInUse()
	s, err = jsonl.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	held := heapInUse() - before
	if held > 1<<20 {
		t.Fatalf("open holds %d KB for %d finished turns", held>>10, turns)
	}
	got, err := s.Turn(t.Context(), "r7")
	if err != nil || got.Input != strings.Repeat("y", 4<<10) || got.Status != faden.Done {
		t.Fatalf("turn read back = %.40q %s, %v", got.Input, got.Status, err)
	}
	t.Logf("open holds %d KB for %d finished turns of 4 KB", held>>10, turns)
}

func openFiles(t *testing.T) int {
	t.Helper()
	fds, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Skip("no /dev/fd:", err)
	}
	return len(fds)
}

// A session this store does not own holds no open file, so a store that
// served many sessions holds no more files than a new one.
func TestOnlyOwnedSessionsKeepAFileOpen(t *testing.T) {
	s, err := jsonl.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	before := openFiles(t)
	finishedTurns(t, s, 200, 200, "x")
	if got := openFiles(t); got > before {
		t.Fatalf("open files grew from %d to %d after 200 sessions", before, got)
	}
	if err := s.Claim(t.Context(), "s1"); err != nil {
		t.Fatal(err)
	}
	if got := openFiles(t); got != before+1 {
		t.Fatalf("open files = %d with one claim, want %d", got, before+1)
	}
	if err := s.Release(t.Context(), "s1"); err != nil {
		t.Fatal(err)
	}
	if got := openFiles(t); got != before {
		t.Fatalf("open files = %d after release, want %d", got, before)
	}
}

// Package continuationtest runs killed hosts for backend continuation tests.
package continuationtest

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/nogo/faden"
	"github.com/nogo/faden/jsonl"
)

// Factory makes the backend under test.
type Factory func() faden.Backend

type signalingStore struct {
	faden.Store
	ready bool
	file  string
}

func (s *signalingStore) AppendEvent(ctx context.Context, e faden.Event) (faden.Event, error) {
	stored, err := s.Store.AppendEvent(ctx, e)
	if s.file != "" {
		if _, statErr := os.Stat(s.file); statErr != nil {
			return stored, err
		}
	}
	if err == nil && e.Kind == faden.ToolResult && !s.ready {
		s.ready = true
		os.Stdout.WriteString("ready\n")
	}
	return stored, err
}

// Host submits a turn and signals after its first tool result is durable.
func Host(t *testing.T, dir, name, input string, factory Factory) {
	t.Helper()
	store, err := jsonl.Open(filepath.Join(dir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := &signalingStore{Store: store}
	if file := os.Getenv("FADEN_KILL_AFTER_FILE"); file != "" {
		s.file = filepath.Join(dir, file)
	}
	r, err := faden.Open(s, map[string]faden.Backend{name: factory()})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Submit(t.Context(), faden.Submission{Session: "s", RequestID: "r", Backend: name, Input: input, Context: os.Getenv("FADEN_TEST_CONTEXT")}); err != nil {
		t.Fatal(err)
	}
	if turn, err := r.Wait(t.Context(), "r"); err != nil || turn.Status != faden.Done {
		t.Fatalf("turn: %+v, error: %v", turn, err)
	}
}

// KillHost kills a real host after its store has synced a tool result.
func KillHost(t *testing.T, dir, env string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHostHelper$")
	cmd.Env = append(os.Environ(), env+"="+dir)
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	ready := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(out).ReadString('\n'); ready <- line }()
	select {
	case line := <-ready:
		if line != "ready\n" {
			t.Fatalf("host output: %q", line)
		}
	case <-time.After(2 * time.Minute):
		t.Fatal("waiting for stored tool result")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
}

// Continue reopens the host's store and waits for the recovered turn.
func Continue(t *testing.T, dir, name string, factory Factory) []faden.Event {
	t.Helper()
	store, err := jsonl.Open(filepath.Join(dir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	prior, err := faden.CollectEvents(store.Events(t.Context(), "s", 0))
	if err != nil {
		t.Fatal(err)
	}
	var after int64
	if len(prior) > 0 {
		after = prior[len(prior)-1].Seq
	}
	r, err := faden.Open(store, map[string]faden.Backend{name: factory()})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	turn, err := r.Wait(ctx, "r")
	if err != nil || turn.Status != faden.Done || turn.Attempts != 2 {
		t.Fatalf("turn: %+v, error: %v", turn, err)
	}
	events, err := faden.CollectEvents(store.Events(ctx, "s", after))
	if err != nil {
		t.Fatal(err)
	}
	return events
}

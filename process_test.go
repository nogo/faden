package faden_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nogo/faden"
	"github.com/nogo/faden/jsonl"
	"github.com/nogo/faden/sqlite"
	_ "modernc.org/sqlite"
)

// These tests run two real processes on one store: the test binary, and a
// child it starts again with FADEN_PROCESS_ROLE set. They talk through files
// in a shared directory, not through the store under test.

const (
	processExpiry = time.Second
	processPoll   = 20 * time.Millisecond
)

var processStores = []string{"jsonl", "sqlite"}

// openProcessStore opens the store of one process. A sqlite claim runs out
// one second after its last renewal.
func openProcessStore(t *testing.T, kind, dir string) faden.Store {
	t.Helper()
	switch kind {
	case "jsonl":
		s, err := jsonl.Open(filepath.Join(dir, "store"))
		if err != nil {
			t.Fatal(err)
		}
		return s
	case "sqlite":
		db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "store.db")+"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)")
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		t.Cleanup(func() { db.Close() })
		s, err := sqlite.Open(db, "")
		if err != nil {
			t.Fatal(err)
		}
		s.SetClaimTiming(processExpiry, 200*time.Millisecond)
		return s
	}
	t.Fatalf("unknown store %q", kind)
	return nil
}

// processBackend records every run and stop in files of dir. The turns "ra"
// and "rb" end only when both have started, so they must run at once; "block"
// runs until its process dies, but only on its first attempt.
type processBackend struct{ dir string }

func (b processBackend) note(file, line string) {
	f, err := os.OpenFile(filepath.Join(b.dir, file), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err == nil {
		fmt.Fprintln(f, line)
		f.Close()
	}
}

func (processBackend) ContinuesWithoutPrompt() bool { return true }

func (b processBackend) Stop(_ context.Context, t faden.Turn) error {
	b.note("stops", t.RequestID)
	return nil
}

func (b processBackend) Run(ctx context.Context, t faden.Turn, _ *faden.Attempt) error {
	b.note("runs", fmt.Sprintf("%s attempt=%d", t.RequestID, t.Attempts))
	switch {
	case t.RequestID == "ra" || t.RequestID == "rb":
		b.note("started-"+t.RequestID, "")
		for !fileExists(b.dir, "started-ra") || !fileExists(b.dir, "started-rb") {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Millisecond):
			}
		}
	case t.RequestID == "block" && t.Attempts == 1:
		b.note("started-block", "")
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func fileExists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

func lineCount(dir, file, prefix string) int {
	data, _ := os.ReadFile(filepath.Join(dir, file))
	n := 0
	for line := range strings.SplitSeq(string(data), "\n") {
		if line != "" && strings.HasPrefix(line, prefix) {
			n++
		}
	}
	return n
}

func waitForFile(t *testing.T, dir, name string) {
	t.Helper()
	pollLocal(t, func() bool { return fileExists(dir, name) })
}

// TestProcessHost is the child process of the tests below. It does nothing
// in a normal run.
func TestProcessHost(t *testing.T) {
	role, dir, kind := os.Getenv("FADEN_PROCESS_ROLE"), os.Getenv("FADEN_PROCESS_DIR"), os.Getenv("FADEN_PROCESS_STORE")
	if role == "" {
		return
	}
	store := openProcessStore(t, kind, dir)
	defer store.Close()
	if role == "serve" {
		waitForFile(t, dir, "go") // open together with the parent
	}
	runner, err := faden.Open(store, map[string]faden.Backend{"fake": processBackend{dir}}, faden.Options{Poll: processPoll})
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	submit := func(session, id string) {
		if _, err := runner.Submit(t.Context(), sub(session, id, "invented")); err != nil {
			t.Fatal(err)
		}
	}
	switch role {
	case "session-b":
		submit("b", "rb")
		wait(t, runner, "rb")
	case "crash":
		submit("s", "block")
		wait(t, runner, "block") // never returns: the parent kills this process
	case "serve":
		wait(t, runner, "block")
	case "duplicates":
		for i := range 20 {
			waitForFile(t, dir, fmt.Sprintf("go-%d", i))
			submit("s", fmt.Sprintf("same-%d", i))
		}
		for i := range 20 {
			wait(t, runner, fmt.Sprintf("same-%d", i))
		}
	}
}

func startProcess(t *testing.T, kind, dir, role string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessHost$")
	cmd.Env = append(os.Environ(), "FADEN_PROCESS_ROLE="+role, "FADEN_PROCESS_DIR="+dir, "FADEN_PROCESS_STORE="+kind)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	return cmd
}

// finish waits for a child that should end by itself.
func finish(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("child process: %v", err)
	}
}

func openProcessRunner(t *testing.T, kind, dir string) *faden.Runner {
	t.Helper()
	store := openProcessStore(t, kind, dir)
	t.Cleanup(func() { store.Close() })
	runner, err := faden.Open(store, map[string]faden.Backend{"fake": processBackend{dir}}, faden.Options{Poll: processPoll})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runner.Close() })
	return runner
}

func TestTwoProcessesServeDifferentSessionsAtOnce(t *testing.T) {
	for _, kind := range processStores {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			runner := openProcessRunner(t, kind, dir)
			child := startProcess(t, kind, dir, "session-b")
			if _, err := runner.Submit(t.Context(), sub("a", "ra", "invented")); err != nil {
				t.Fatal(err)
			}
			// Each turn ends only after the other has started, so both ran at once.
			if got := wait(t, runner, "ra"); got.Status != faden.Done {
				t.Fatalf("ra = %+v", got)
			}
			// rb may run in the child; Wait then polls the store.
			if got := wait(t, runner, "rb"); got.Status != faden.Done {
				t.Fatalf("rb = %+v", got)
			}
			finish(t, child)
		})
	}
}

// runningOwner starts a process that runs a turn, and returns once the turn
// runs. The process stays the owner until kill.
func runningOwner(t *testing.T, kind, dir string) *exec.Cmd {
	t.Helper()
	child := startProcess(t, kind, dir, "crash")
	waitForFile(t, dir, "started-block")
	return child
}

func kill(t *testing.T, child *exec.Cmd) {
	t.Helper()
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	child.Wait()
}

func checkContinuedOnce(t *testing.T, runner *faden.Runner, dir string) {
	t.Helper()
	turn := wait(t, runner, "block")
	if turn.Status != faden.Done || turn.Attempts != 2 {
		t.Fatalf("turn = %+v, want done at attempt 2", turn)
	}
	if n := lineCount(dir, "runs", "block attempt=1"); n != 1 {
		t.Fatalf("attempt 1 ran %d times", n)
	}
	if n := lineCount(dir, "runs", "block attempt=2"); n != 1 {
		t.Fatalf("attempt 2 ran %d times, want once", n)
	}
	if n := lineCount(dir, "runs", "block attempt=3"); n != 0 {
		t.Fatalf("attempt 3 ran %d times, want never", n)
	}
	if n := lineCount(dir, "stops", "block"); n != 1 {
		t.Fatalf("leftover of block stopped %d times, want once", n)
	}
}

func TestKilledOwnersTurnContinuesInTheOtherProcess(t *testing.T) {
	for _, kind := range processStores {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			owner := runningOwner(t, kind, dir)
			runner := openProcessRunner(t, kind, dir) // finds the session owned, adopts it when the owner is gone
			kill(t, owner)
			killed := time.Now()
			checkContinuedOnce(t, runner, dir)
			if kind == "sqlite" && time.Since(killed) < processExpiry/2 {
				t.Fatalf("continued %v after the kill, before the claim could run out", time.Since(killed))
			}
		})
	}
}

func TestTwoOwnersOfOneInterruptedTurnRunItOnce(t *testing.T) {
	for _, kind := range processStores {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			kill(t, runningOwner(t, kind, dir))
			// Both processes open the store with the turn running.
			child := startProcess(t, kind, dir, "serve")
			os.WriteFile(filepath.Join(dir, "go"), nil, 0o644)
			runner := openProcessRunner(t, kind, dir)
			checkContinuedOnce(t, runner, dir)
			finish(t, child)
			checkContinuedOnce(t, runner, dir)
		})
	}
}

func TestSameRequestIDInTwoProcessesRunsOnce(t *testing.T) {
	for _, kind := range processStores {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			runner := openProcessRunner(t, kind, dir)
			child := startProcess(t, kind, dir, "duplicates")
			for i := range 20 {
				os.WriteFile(filepath.Join(dir, fmt.Sprintf("go-%d", i)), nil, 0o644)
				if _, err := runner.Submit(t.Context(), sub("s", fmt.Sprintf("same-%d", i), "invented")); err != nil {
					t.Fatal(err)
				}
			}
			for i := range 20 {
				id := fmt.Sprintf("same-%d", i)
				// A turn the child ran is waited for through the store.
				if got := wait(t, runner, id); got.Status != faden.Done || got.Attempts != 1 {
					t.Fatalf("%s = %+v", id, got)
				}
			}
			finish(t, child)
			for i := range 20 {
				if n := lineCount(dir, "runs", fmt.Sprintf("same-%d ", i)); n != 1 {
					t.Fatalf("same-%d ran %d times, want once", i, n)
				}
			}
		})
	}
}

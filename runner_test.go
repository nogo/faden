package faden_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nogo/faden"
	"github.com/nogo/faden/handover"
	"github.com/nogo/faden/jsonl"
	"github.com/nogo/faden/memstore"
)

// fake is a backend with a scripted Run.
type fake struct {
	promptless bool
	run        func(ctx context.Context, t faden.Turn, a *faden.Attempt) error

	mu      sync.Mutex // guards stopped
	stopped []string   // request ids Stop was called for
}

func (f *fake) ContinuesWithoutPrompt() bool { return f.promptless }

func (f *fake) Stop(_ context.Context, t faden.Turn) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, t.RequestID)
	return nil
}

func (f *fake) Run(ctx context.Context, t faden.Turn, a *faden.Attempt) error {
	return f.run(ctx, t, a)
}

func openStore(t *testing.T, dir string) *jsonl.Store {
	t.Helper()
	s, err := jsonl.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func sub(session, id, input string) faden.Submission {
	return faden.Submission{Session: session, RequestID: id, Input: input, Backend: "fake"}
}

func wait(t *testing.T, r *faden.Runner, id string) faden.Turn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	got, err := r.Wait(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestSubmitRunsTurnAndStoresCursorAndEvents(t *testing.T) {
	store := openStore(t, t.TempDir())
	b := &fake{run: func(ctx context.Context, turn faden.Turn, a *faden.Attempt) error {
		a.Emit(ctx, faden.Event{Kind: faden.AssistantMessage, Text: "re: " + turn.Input})
		return a.SaveCursor(ctx, json.RawMessage(`{"id":"c1"}`))
	}}
	r, err := faden.Open(store, map[string]faden.Backend{"fake": b})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	submitted, err := r.Submit(t.Context(), sub("s1", "r1", "hello"))
	if err != nil || submitted.Status != faden.Pending {
		t.Fatalf("Submit = %+v, %v", submitted, err)
	}
	got := wait(t, r, "r1")
	if got.Status != faden.Done || got.Attempts != 1 {
		t.Fatalf("turn = %+v", got)
	}
	events, _ := faden.CollectEvents(store.Events(t.Context(), "s1", 0))
	sess, _ := store.Session(t.Context(), "s1")
	if len(events) != 2 || events[0].Kind != faden.UserMessage || events[0].Text != "hello" || events[1].Text != "re: hello" || events[1].RequestID != "r1" || string(sess.Cursors["fake"]) != `{"id":"c1"}` {
		t.Fatalf("events = %+v, session = %+v", events, sess)
	}
}

func TestKnownRequestIDRunsNothing(t *testing.T) {
	store := openStore(t, t.TempDir())
	var mu sync.Mutex
	runs := 0
	b := &fake{run: func(context.Context, faden.Turn, *faden.Attempt) error {
		mu.Lock()
		runs++
		mu.Unlock()
		return nil
	}}
	r, _ := faden.Open(store, map[string]faden.Backend{"fake": b})
	defer r.Close()
	r.Submit(t.Context(), sub("s1", "r1", "first"))
	wait(t, r, "r1")
	again, err := r.Submit(t.Context(), sub("s1", "r1", "second"))
	if err != nil {
		t.Fatal(err)
	}
	if again.Input != "first" || again.Status != faden.Done {
		t.Fatalf("resubmitted turn = %+v", again)
	}
	r.Close()
	mu.Lock()
	defer mu.Unlock()
	if runs != 1 {
		t.Fatalf("backend ran %d times, want 1", runs)
	}
}

func TestSessionRunsOneTurnAtATimeOldestFirst(t *testing.T) {
	store := openStore(t, t.TempDir())
	release := make(chan struct{})
	var mu sync.Mutex
	var order []string
	active, overlap := 0, false
	b := &fake{run: func(_ context.Context, turn faden.Turn, _ *faden.Attempt) error {
		mu.Lock()
		active++
		overlap = overlap || active > 1
		order = append(order, turn.RequestID)
		mu.Unlock()
		if turn.RequestID == "r1" {
			<-release
		}
		mu.Lock()
		active--
		mu.Unlock()
		return nil
	}}
	r, _ := faden.Open(store, map[string]faden.Backend{"fake": b})
	defer r.Close()
	for _, id := range []string{"r1", "r2", "r3"} {
		r.Submit(t.Context(), sub("s1", id, id))
	}
	if got, _ := store.Turn(t.Context(), "r3"); got.Status != faden.Pending {
		t.Fatalf("r3 = %v while r1 runs, want pending", got.Status)
	}
	close(release)
	wait(t, r, "r3")
	mu.Lock()
	defer mu.Unlock()
	if overlap || strings.Join(order, ",") != "r1,r2,r3" {
		t.Fatalf("order = %v, overlap = %v", order, overlap)
	}
}

func TestBackendErrorFailsTurn(t *testing.T) {
	store := openStore(t, t.TempDir())
	b := &fake{run: func(context.Context, faden.Turn, *faden.Attempt) error {
		return context.DeadlineExceeded
	}}
	r, _ := faden.Open(store, map[string]faden.Backend{"fake": b})
	defer r.Close()
	r.Submit(t.Context(), sub("s1", "r1", "x"))
	if got := wait(t, r, "r1"); got.Status != faden.Failed || got.Error == "" {
		t.Fatalf("turn = %+v", got)
	}
}

// seedRunning leaves a running turn with the given attempts in a closed store.
func seedRunning(t *testing.T, dir string, attempts int) {
	t.Helper()
	s := openStore(t, dir)
	if _, err := s.SubmitTurn(t.Context(), faden.Turn{RequestID: "r1", Session: "s1", Input: "original", Backend: "fake"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveTurn(t.Context(), faden.Turn{RequestID: "r1", Status: faden.Running, Attempts: attempts}); err != nil {
		t.Fatal(err)
	}
	s.Close()
}

func TestOpenRaisesAttemptsBeforeBackendRunsAndPromptDependsOnBackend(t *testing.T) {
	for name, c := range map[string]struct {
		promptless bool
		wantInput  string
	}{
		"prompt": {false, "Continue where you left off."},
		"none":   {true, ""},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			seedRunning(t, dir, 1)
			store := openStore(t, dir)
			var stored faden.Turn
			var sent string
			b := &fake{promptless: c.promptless, run: func(ctx context.Context, turn faden.Turn, _ *faden.Attempt) error {
				stored, _ = store.Turn(ctx, "r1")
				sent = turn.Input
				return nil
			}}
			r, err := faden.Open(store, map[string]faden.Backend{"fake": b})
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			got := wait(t, r, "r1")
			if got.Status != faden.Done || got.Attempts != 2 || stored.Attempts != 2 || sent != c.wantInput {
				t.Fatalf("turn = %+v, stored while running = %+v, sent %q", got, stored, sent)
			}
		})
	}
}

func TestTurnFailsAfterThreeAttemptsWithoutCallingBackend(t *testing.T) {
	dir := t.TempDir()
	seedRunning(t, dir, 3)
	store := openStore(t, dir)
	b := &fake{run: func(context.Context, faden.Turn, *faden.Attempt) error {
		t.Error("backend called for a turn out of attempts")
		return nil
	}}
	r, err := faden.Open(store, map[string]faden.Backend{"fake": b})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got := wait(t, r, "r1")
	if got.Status != faden.Failed || got.Attempts != 3 || !strings.Contains(got.Error, "send a new turn") {
		t.Fatalf("turn = %+v", got)
	}
	// Given up, the turn never continues, so its leftover is stopped now.
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.stopped) != 1 || b.stopped[0] != "r1" {
		t.Fatalf("stopped = %v, want [r1]", b.stopped)
	}
}

// The crash test re-runs this test binary as a child that runs a turn and
// blocks; the parent kills it with SIGKILL and reopens the store.
const crashDirEnv = "FADEN_CRASH_CHILD_DIR"

func TestCrashChildProcess(t *testing.T) {
	dir := os.Getenv(crashDirEnv)
	if dir == "" {
		t.Skip("helper for TestKilledSteeredTurnFinishesAfterRestart")
	}
	store := openStore(t, dir)
	started := make(chan struct{})
	b := &steeringFake{fake: &fake{run: func(ctx context.Context, _ faden.Turn, a *faden.Attempt) error {
		a.Emit(ctx, faden.Event{Kind: faden.AssistantMessage, Text: "one"})
		a.Emit(ctx, faden.Event{Kind: faden.AssistantMessage, Text: "two"})
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}, steer: func(context.Context, faden.Turn, string) error { return nil }}
	r, err := faden.Open(store, map[string]faden.Backend{"fake": b})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Submit(t.Context(), sub("s1", "r1", "work")); err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err := r.Steer(t.Context(), "r1", "use blue"); err != nil {
		t.Fatal(err)
	}
	os.Stdout.WriteString("ready\n")
	select {} // killed by the parent
}

func TestKilledSteeredTurnFinishesAfterRestart(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashChildProcess$")
	cmd.Env = append(os.Environ(), crashDirEnv+"="+dir)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || line != "ready\n" {
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatalf("child output %q, err %v", line, err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()

	store := openStore(t, dir)
	var sent string
	b := &fake{run: func(_ context.Context, turn faden.Turn, _ *faden.Attempt) error {
		sent = turn.Input
		return nil
	}}
	r, err := faden.Open(store, map[string]faden.Backend{"fake": b})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got := wait(t, r, "r1")
	if got.Status != faden.Done || got.Attempts != 2 || sent != "Continue where you left off." {
		t.Fatalf("turn = %+v, sent %q", got, sent)
	}
	events, _ := faden.CollectEvents(store.Events(t.Context(), "s1", 0))
	// The input is stored once; the continuation prompt is not stored as a user message.
	if len(events) != 4 || events[0].Kind != faden.UserMessage || events[0].Text != "work" || events[1].Text != "one" || events[2].Text != "two" || events[3].Kind != faden.UserMessage || events[3].Text != "use blue" {
		t.Fatalf("events = %+v", events)
	}
	unfinished, _ := store.Unfinished(t.Context())
	if len(unfinished) != 0 {
		t.Fatalf("unfinished = %+v", unfinished)
	}
	if again, _ := r.Submit(t.Context(), sub("s1", "r1", "work")); again.Status != faden.Done {
		t.Fatalf("resubmit = %+v", again)
	}
	if n := strings.Count(readSession(t, dir), `"status":"pending"`); n != 1 {
		t.Fatalf("turn was stored %d times as pending, want 1", n)
	}
}

func readSession(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(dir + "/s1.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestContinuationCarriesTheKilledTurnsStoredEvents(t *testing.T) {
	for name, promptless := range map[string]bool{"prompt": false, "none": true} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			seedRunning(t, dir, 1)
			s := openStore(t, dir)
			for _, e := range []faden.Event{
				{Kind: faden.UserMessage, Text: "original"},
				{Kind: faden.ToolCall, Tool: "bash", Action: "echo first > first.txt"},
				{Kind: faden.ToolResult, Tool: "bash", Output: "wrote first.txt"},
			} {
				e.Session, e.RequestID = "s1", "r1"
				if _, err := s.AppendEvent(t.Context(), e); err != nil {
					t.Fatal(err)
				}
			}
			s.Close()

			store := openStore(t, dir)
			var turn faden.Turn
			var catchUp []faden.Event
			b := &fake{promptless: promptless, run: func(_ context.Context, got faden.Turn, a *faden.Attempt) error {
				turn = got
				catchUp, _ = a.CatchUp()
				return nil
			}}
			r, err := faden.Open(store, map[string]faden.Backend{"fake": b})
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			wait(t, r, "r1")
			var block string
			for _, e := range catchUp {
				block += e.Action + "|" + e.Output + "\n"
			}
			if promptless {
				if len(catchUp) != 0 {
					t.Fatalf("promptless backend got catch-up %+v", catchUp)
				}
				return
			}
			if turn.Input != "Continue where you left off." || !strings.Contains(block, "echo first > first.txt") || !strings.Contains(block, "wrote first.txt") {
				t.Fatalf("input %q, catch-up %q", turn.Input, block)
			}
		})
	}
}

func TestPrepareRunsBeforeEveryAttemptAndReachesTheBackend(t *testing.T) {
	dir := t.TempDir()
	seedRunning(t, dir, 1)
	store := openStore(t, dir)
	var prepared []int
	var got faden.Setup
	b := &fake{run: func(_ context.Context, _ faden.Turn, a *faden.Attempt) error {
		got = a.Setup()
		return nil
	}}
	want := faden.Setup{Dir: "/ws", Args: []string{"--effort", "high"}, Env: []string{"TOKEN=invented"}, System: "rules"}
	r, err := faden.Open(store, map[string]faden.Backend{"fake": b}, faden.Options{Prepare: func(_ context.Context, turn faden.Turn) (faden.Setup, error) {
		prepared = append(prepared, turn.Attempts)
		return want, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	wait(t, r, "r1") // a continuation: Open raised the attempt to 2
	r.Submit(t.Context(), sub("s2", "r2", "fresh"))
	wait(t, r, "r2")
	if len(prepared) != 2 || prepared[0] != 2 || prepared[1] != 1 {
		t.Fatalf("Prepare saw attempts %v, want [2 1]", prepared)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("attempt setup = %+v, want %+v", got, want)
	}
}

func TestPrepareErrorFailsTurnWithoutCallingBackend(t *testing.T) {
	store := openStore(t, t.TempDir())
	called := false
	b := &fake{run: func(context.Context, faden.Turn, *faden.Attempt) error {
		called = true
		return nil
	}}
	r, _ := faden.Open(store, map[string]faden.Backend{"fake": b}, faden.Options{Prepare: func(context.Context, faden.Turn) (faden.Setup, error) {
		return faden.Setup{}, errors.New("no workspace")
	}})
	defer r.Close()
	r.Submit(t.Context(), sub("s1", "r1", "x"))
	got := wait(t, r, "r1")
	if got.Status != faden.Failed || !strings.Contains(got.Error, "no workspace") || called {
		t.Fatalf("turn = %+v, backend called %v", got, called)
	}
}

func TestSetupIsNotStored(t *testing.T) {
	dir := t.TempDir()
	store := openStore(t, dir)
	b := &fake{run: func(ctx context.Context, _ faden.Turn, a *faden.Attempt) error {
		return a.Emit(ctx, faden.Event{Kind: faden.AssistantMessage, Text: "ok"})
	}}
	r, _ := faden.Open(store, map[string]faden.Backend{"fake": b}, faden.Options{Prepare: func(context.Context, faden.Turn) (faden.Setup, error) {
		return faden.Setup{Env: []string{"KEY=invented-secret"}, System: "invented-rules"}, nil
	}})
	defer r.Close()
	r.Submit(t.Context(), sub("s1", "r1", "x"))
	wait(t, r, "r1")
	files, _ := filepath.Glob(filepath.Join(dir, "*"))
	for _, f := range files {
		data, _ := os.ReadFile(f)
		if bytes.Contains(data, []byte("invented-secret")) || bytes.Contains(data, []byte("invented-rules")) {
			t.Fatalf("%s holds the setup", f)
		}
	}
}

func TestUsageSumsAcrossAttemptsAndFailure(t *testing.T) {
	store := openStore(t, t.TempDir())
	first := make(chan struct{})
	b := &fake{run: func(ctx context.Context, turn faden.Turn, a *faden.Attempt) error {
		a.AddUsage(faden.ModelUsage{Model: "invented", InputTokens: 10, OutputTokens: 2, CacheReadTokens: 3, CacheWriteTokens: 1, CostUSD: 0.125})
		a.AddUsage(faden.ModelUsage{Model: "other", InputTokens: 1})
		if turn.Attempts == 1 {
			close(first)
			<-ctx.Done()
			return ctx.Err()
		}
		a.AddUsage(faden.ModelUsage{Model: "invented", InputTokens: 5})
		return errors.New("invented failure")
	}}
	r, err := faden.Open(store, map[string]faden.Backend{"fake": b})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Submit(t.Context(), sub("s1", "usage", "hello")); err != nil {
		t.Fatal(err)
	}
	<-first
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r, err = faden.Open(store, map[string]faden.Backend{"fake": b})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got := wait(t, r, "usage")
	want := []faden.ModelUsage{{Model: "invented", InputTokens: 25, OutputTokens: 4, CacheReadTokens: 6, CacheWriteTokens: 2, CostUSD: 0.25}, {Model: "other", InputTokens: 2}}
	if got.Status != faden.Failed || got.Attempts != 2 || !reflect.DeepEqual(got.Usage, want) {
		t.Fatalf("turn=%+v want usage=%+v", got, want)
	}
}

func TestFailureKindIsStoredWithTheTurn(t *testing.T) {
	resets := time.Unix(1800000000, 0).UTC()
	cases := map[string]struct {
		err  error
		want faden.Turn
	}{
		"quota":       {fmt.Errorf("wrapped: %w", &faden.QuotaError{ResetsAt: resets, LimitType: "five_hour"}), faden.Turn{Failure: faden.FailureQuota, RetryAt: resets, LimitType: "five_hour"}},
		"unavailable": {&faden.UnavailableError{Reason: "not logged in"}, faden.Turn{Failure: faden.FailureUnavailable}},
		"agent":       {errors.New("boom"), faden.Turn{}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			store := openStore(t, t.TempDir())
			b := &fake{run: func(context.Context, faden.Turn, *faden.Attempt) error { return c.err }}
			r, _ := faden.Open(store, map[string]faden.Backend{"fake": b})
			defer r.Close()
			r.Submit(t.Context(), sub("s1", "r1", "x"))
			got := wait(t, r, "r1")
			if got.Status != faden.Failed || got.Failure != c.want.Failure || !got.RetryAt.Equal(c.want.RetryAt) || got.LimitType != c.want.LimitType {
				t.Fatalf("turn = %+v", got)
			}
			stored, _ := store.Turn(t.Context(), "r1")
			if stored.Failure != got.Failure || !stored.RetryAt.Equal(got.RetryAt) {
				t.Fatalf("stored = %+v", stored)
			}
		})
	}
}

func TestCancelPendingAndRunning(t *testing.T) {
	dir := t.TempDir()
	store := openStore(t, dir)
	started := make(chan struct{})
	b := &fake{run: func(ctx context.Context, turn faden.Turn, a *faden.Attempt) error {
		if turn.RequestID == "running" {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		}
		if turn.RequestID == "pending" {
			t.Error("cancelled pending turn ran")
		}
		return nil
	}}
	r, err := faden.Open(store, map[string]faden.Backend{"fake": b})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	r.Submit(t.Context(), sub("s", "running", "one"))
	<-started
	r.Submit(t.Context(), sub("s", "pending", "two"))
	r.Submit(t.Context(), sub("s", "next", "three"))
	r.Submit(t.Context(), sub("other", "other", "four"))
	for _, id := range []string{"pending", "running"} {
		for range 2 {
			got, err := r.Cancel(t.Context(), id)
			if err != nil || got.Status != faden.Cancelled {
				t.Fatalf("Cancel(%s) = %+v, %v", id, got, err)
			}
		}
		if got := wait(t, r, id); got.Status != faden.Cancelled {
			t.Fatal(got)
		}
	}
	for _, id := range []string{"next", "other"} {
		if got := wait(t, r, id); got.Status != faden.Done {
			t.Fatal(got)
		}
		if got, err := r.Cancel(t.Context(), id); err != nil || got.Status != faden.Done {
			t.Fatalf("final cancel = %+v, %v", got, err)
		}
	}
	r.Close()
	b.mu.Lock()
	stopped := append([]string(nil), b.stopped...)
	b.mu.Unlock()
	if !reflect.DeepEqual(stopped, []string{"running"}) {
		t.Fatalf("Stop = %v", stopped)
	}
	events, err := faden.CollectEvents(store.Events(t.Context(), "s", 0))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, e := range events {
		if e.Kind == faden.TurnEnded && e.Text == "cancelled" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("cancel events = %d, want 1: only the running turn started", count)
	}
	thread, err := handover.ReadThread(t.Context(), store, "s", json.RawMessage(`{"from":1,"to":100}`))
	if err != nil || strings.Count(thread, `"text":"cancelled"`) != 1 {
		t.Fatalf("read_thread = %s, %v", thread, err)
	}
	var selected strings.Builder
	for _, item := range handover.Select(events, "continue", 16000) {
		selected.WriteString(item.Text)
	}
	if strings.Count(selected.String(), "turn_ended  cancelled") != 1 {
		t.Fatalf("handover = %s", selected.String())
	}
	store.Close()
	store = openStore(t, dir)
	r, err = faden.Open(store, map[string]faden.Backend{"fake": b})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got := wait(t, r, "running"); got.Status != faden.Cancelled || got.Attempts != 1 {
		t.Fatal(got)
	}
}

func TestCancelFailedTurnAndUnknownRequest(t *testing.T) {
	store := openStore(t, t.TempDir())
	b := &fake{run: func(context.Context, faden.Turn, *faden.Attempt) error { return errors.New("invented failure") }}
	r, err := faden.Open(store, map[string]faden.Backend{"fake": b})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Submit(t.Context(), sub("s", "failed", "fail")); err != nil {
		t.Fatal(err)
	}
	want := wait(t, r, "failed")
	got, err := r.Cancel(t.Context(), "failed")
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("cancel = %+v, %v; want %+v", got, err, want)
	}
	if _, err := r.Cancel(t.Context(), "unknown"); !errors.Is(err, faden.ErrNotFound) {
		t.Fatalf("unknown cancel: %v", err)
	}
}

func TestCancelPendingDoesNotAdvanceBackendHistory(t *testing.T) {
	store := openStore(t, t.TempDir())
	started, release := make(chan struct{}), make(chan struct{})
	first := &fake{run: func(ctx context.Context, _ faden.Turn, _ *faden.Attempt) error {
		close(started)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	var catchUp []faden.Event
	second := &fake{run: func(_ context.Context, turn faden.Turn, a *faden.Attempt) error {
		if turn.RequestID != "next" {
			t.Error("cancelled backend ran")
		}
		catchUp, _ = a.CatchUp()
		return nil
	}}
	r, err := faden.Open(store, map[string]faden.Backend{"fake": first, "second": second})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Submit(t.Context(), sub("s", "first", "original history")); err != nil {
		t.Fatal(err)
	}
	<-started
	pending := sub("s", "pending", "withdrawn")
	pending.Backend = "second"
	if _, err := r.Submit(t.Context(), pending); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Cancel(t.Context(), "pending"); err != nil {
		t.Fatal(err)
	}
	close(release)
	wait(t, r, "first")
	next := sub("s", "next", "continue")
	next.Backend = "second"
	if _, err := r.Submit(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	wait(t, r, "next")
	var history, pendingEvent bool
	for _, e := range catchUp {
		history = history || e.Kind == faden.UserMessage && e.Text == "original history"
		pendingEvent = pendingEvent || e.RequestID == "pending"
	}
	if !history || pendingEvent {
		t.Fatalf("handover lost history or shows the never-started turn: %+v", catchUp)
	}
}

func TestDrainLetsRunningTurnFinishAndKeepsPendingPending(t *testing.T) {
	dir := t.TempDir()
	store := openStore(t, dir)
	started, release := make(chan struct{}), make(chan struct{})
	b := &fake{run: func(ctx context.Context, turn faden.Turn, a *faden.Attempt) error {
		if turn.RequestID != "running" {
			t.Errorf("turn %s started during drain", turn.RequestID)
			return nil
		}
		close(started)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	r, err := faden.Open(store, map[string]faden.Backend{"fake": b})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	r.Submit(t.Context(), sub("s", "running", "one"))
	<-started
	r.Submit(t.Context(), sub("s", "pending", "two"))
	drained := make(chan error, 1)
	go func() { drained <- r.Drain(t.Context()) }()
	select {
	case err := <-drained:
		t.Fatalf("Drain returned %v before the turn ended", err)
	case <-time.After(50 * time.Millisecond):
	}
	late, err := r.Submit(t.Context(), sub("s", "late", "three"))
	if err != nil || late.Status != faden.Pending {
		t.Fatalf("Submit during drain = %+v, %v", late, err)
	}
	close(release)
	if err := <-drained; err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]faden.Status{"running": faden.Done, "pending": faden.Pending, "late": faden.Pending} {
		if got, err := store.Turn(t.Context(), id); err != nil || got.Status != want {
			t.Fatalf("%s = %+v, %v; want %s", id, got, err, want)
		}
	}
}

func TestDrainPastDeadlineLeavesTurnRunningForNextOpen(t *testing.T) {
	dir := t.TempDir()
	store := openStore(t, dir)
	started := make(chan struct{})
	b := &fake{run: func(ctx context.Context, turn faden.Turn, a *faden.Attempt) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}
	r, err := faden.Open(store, map[string]faden.Backend{"fake": b})
	if err != nil {
		t.Fatal(err)
	}
	r.Submit(t.Context(), sub("s", "slow", "one"))
	<-started
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := r.Drain(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Drain = %v, want deadline exceeded", err)
	}
	got, err := store.Turn(t.Context(), "slow")
	if err != nil || got.Status != faden.Running || got.Attempts != 1 {
		t.Fatalf("after drain: %+v, %v", got, err)
	}
	b2 := &fake{run: func(context.Context, faden.Turn, *faden.Attempt) error { return nil }}
	r2, err := faden.Open(store, map[string]faden.Backend{"fake": b2})
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	if got := wait(t, r2, "slow"); got.Status != faden.Done || got.Attempts != 2 {
		t.Fatalf("after reopen: %+v", got)
	}
}

func TestRunnerRunsTurnsOnMemstore(t *testing.T) {
	store := memstore.New()
	b := &fake{run: func(ctx context.Context, turn faden.Turn, a *faden.Attempt) error {
		a.Emit(ctx, faden.Event{Kind: faden.AssistantMessage, Text: "re: " + turn.Input})
		return a.SaveCursor(ctx, json.RawMessage(`{"id":"c1"}`))
	}}
	r, err := faden.Open(store, map[string]faden.Backend{"fake": b})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	r.Submit(t.Context(), sub("s1", "r1", "first"))
	r.Submit(t.Context(), sub("s1", "r2", "second"))
	if got := wait(t, r, "r2"); got.Status != faden.Done {
		t.Fatalf("turn = %+v", got)
	}
	var texts []string
	events, _ := faden.CollectEvents(store.Events(t.Context(), "s1", 0))
	for _, e := range events {
		texts = append(texts, e.Text)
	}
	sess, _ := store.Session(t.Context(), "s1")
	if want := []string{"first", "re: first", "second", "re: second"}; !slices.Equal(texts, want) || string(sess.Cursors["fake"]) != `{"id":"c1"}` {
		t.Fatalf("events = %v, session = %+v", texts, sess)
	}
}

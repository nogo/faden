package claude

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nogo/faden"
	"github.com/nogo/faden/jsonl"
)

// steerRunner runs program as the claude command in dir and submits turn "r".
func steerRunner(t *testing.T, dir, program string) (*faden.Runner, *jsonl.Store) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(program), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	store := openStore(t, filepath.Join(dir, "store"))
	b, err := New(Config{Runtime: faden.Local{Dir: dir, PIDDir: dir}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := faden.Open(store, map[string]faden.Backend{"claude": b})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	if _, err := r.Submit(t.Context(), faden.Submission{Session: "s", RequestID: "r", Input: "work", Backend: "claude"}); err != nil {
		t.Fatal(err)
	}
	return r, store
}

// The real CLI answers a message that arrives during an answer without tool
// calls after that answer, with a second result.
func TestSteerWaitsForLastResult(t *testing.T) {
	dir := t.TempDir()
	program := `#!/bin/sh
IFS= read -r first
printf '%s\n' '{"type":"assistant","message":{"id":"first","content":[{"type":"text","text":"ready"}]}}'
IFS= read -r second
printf '%s\n' "$second" > second
printf '%s\n' '{"type":"result","result":"first answer"}'
printf '%s\n' '{"type":"assistant","message":{"id":"second","content":[{"type":"text","text":"waiting"}]}}'
IFS= read -r gate < gate
printf '%s\n' '{"type":"result","result":"last answer"}'
cat > /dev/null
`
	if err := syscall.Mkfifo(filepath.Join(dir, "gate"), 0600); err != nil {
		t.Fatal(err)
	}
	r, store := steerRunner(t, dir, program)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	watch, err := r.Watch(ctx, "s", 0)
	if err != nil {
		t.Fatal(err)
	}
	for item := range watch {
		if item.Event == nil {
			continue
		}
		switch item.Event.Text {
		case "ready":
			if _, err := r.Steer(ctx, "r", "use blue"); err != nil {
				t.Fatal(err)
			}
		case "waiting":
			turn, err := store.Turn(ctx, "r")
			if err != nil || turn.Status != faden.Running {
				t.Fatalf("after first result: %+v, %v", turn, err)
			}
			data, err := os.ReadFile(filepath.Join(dir, "second"))
			if err != nil || !strings.Contains(string(data), "use blue") {
				t.Fatalf("stdin = %s, %v", data, err)
			}
			if _, err := r.Steer(ctx, "r", "too late"); !errors.Is(err, faden.ErrNotRunning) {
				t.Fatalf("Steer after the first result = %v, want ErrNotRunning", err)
			}
			gate, err := os.OpenFile(filepath.Join(dir, "gate"), os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			_, err = gate.WriteString("go\n")
			gate.Close()
			if err != nil {
				t.Fatal(err)
			}
		case "last answer":
			turn, err := r.Wait(ctx, "r")
			if err != nil || turn.Status != faden.Done {
				t.Fatalf("Wait = %+v, %v", turn, err)
			}
			return
		case "first answer":
			t.Fatal("intermediate result ended turn")
		}
	}
	t.Fatal("stream ended before last answer")
}

// The real CLI takes in a message that arrives during a tool call at the next
// step, and answers both messages with one result. It then waits for more
// input until stdin closes.
func TestSteerTakenInAtToolCallEndsTurn(t *testing.T) {
	dir := t.TempDir()
	program := `#!/bin/sh
IFS= read -r first
printf '%s\n' '{"type":"assistant","message":{"id":"a1","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"sleep 6"}}]}}'
IFS= read -r second
printf '%s\n' "$second" > second
printf '%s\n' '{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"slept"}]}}'
printf '%s\n' '{"type":"assistant","message":{"id":"a2","content":[{"type":"text","text":"blue it is"}]}}'
printf '%s\n' '{"type":"result","result":"blue it is"}'
cat > /dev/null
`
	r, store := steerRunner(t, dir, program)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	watch, err := r.Watch(ctx, "s", 0)
	if err != nil {
		t.Fatal(err)
	}
	for item := range watch {
		if item.Event == nil {
			continue
		}
		switch item.Event.Kind {
		case faden.ToolCall:
			if _, err := r.Steer(ctx, "r", "use blue"); err != nil {
				t.Fatal(err)
			}
		case faden.TurnEnded:
			turn, err := r.Wait(ctx, "r")
			if err != nil || turn.Status != faden.Done || item.Event.Text != "blue it is" {
				t.Fatalf("Wait = %+v, %v; ended with %q", turn, err, item.Event.Text)
			}
			data, err := os.ReadFile(filepath.Join(dir, "second"))
			if err != nil || !strings.Contains(string(data), "use blue") {
				t.Fatalf("stdin = %s, %v", data, err)
			}
			var ended int
			for e, err := range store.Events(ctx, "s", 0) {
				if err != nil {
					t.Fatal(err)
				}
				if e.Kind == faden.TurnEnded {
					ended++
				}
			}
			if ended != 1 {
				t.Fatalf("%d TurnEnded events, want 1", ended)
			}
			return
		}
	}
	t.Fatal("the turn did not end: stdin stayed open after the only result")
}

// TestLiveSteer steers a real Claude Code turn during its first tool call.
// Claude takes the message in at the next step, so one result answers both.
func TestLiveSteer(t *testing.T) {
	if os.Getenv("FADEN_LIVE") != "1" {
		t.Skip("set FADEN_LIVE=1 to run Claude Code")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	store := openStore(t, filepath.Join(dir, "store"))
	r, err := faden.Open(store, map[string]faden.Backend{"claude": runnerBackend(t, dir)()})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	input := "Run the Bash command `sleep 5` three times, one call at a time. Then answer in one short sentence."
	if _, err := r.Submit(ctx, faden.Submission{Session: "s", RequestID: "r", Input: input, Backend: "claude", Model: "haiku"}); err != nil {
		t.Fatal(err)
	}
	watch, err := r.Watch(ctx, "s", 0)
	if err != nil {
		t.Fatal(err)
	}
	var calls, ended int
	var answer string
	for item := range watch {
		if item.Event == nil {
			continue
		}
		switch item.Event.Kind {
		case faden.ToolCall:
			calls++
			if calls == 1 {
				if _, err := r.Steer(ctx, "r", "Stop running commands. Reply only with the word BLUE."); err != nil {
					t.Fatal(err)
				}
			}
		case faden.TurnEnded:
			ended++
			answer = item.Event.Text
		}
		if ended == 1 {
			break
		}
	}
	turn, err := r.Wait(ctx, "r")
	if err != nil || turn.Status != faden.Done {
		t.Fatalf("Wait = %+v, %v", turn, err)
	}
	t.Logf("tool calls %d, answer %q, usage %+v", calls, answer, turn.Usage)
}

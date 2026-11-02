package claude

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nogo/faden"
	"github.com/nogo/faden/internal/continuationtest"
	"github.com/nogo/faden/jsonl"
)

// newAttempt submits turn to store and returns a run of it with the cursor
// the store holds, as the runner would without catch-up.
func newAttempt(t *testing.T, store faden.Store, turn faden.Turn) *faden.Attempt {
	t.Helper()
	turn.Backend = "claude"
	if _, err := store.SubmitTurn(t.Context(), turn); err != nil {
		t.Fatal(err)
	}
	a, err := faden.NewAttempt(t.Context(), store, turn)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func openStore(t *testing.T, dir string) *jsonl.Store {
	t.Helper()
	store, err := jsonl.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func claudeCursor(t *testing.T, store faden.Store) cursor {
	t.Helper()
	sess, err := store.Session(t.Context(), "s")
	if err != nil {
		t.Fatal(err)
	}
	var c cursor
	json.Unmarshal(sess.Cursors["claude"], &c)
	return c
}

// These invented lines exercise the wire format. They are not a real-run recording.
const stream = `{"type":"system","subtype":"init","session_id":"ignored"}
{"type":"assistant","message":{"id":"msg_invented","role":"assistant","content":[{"type":"text","text":"I will inspect the file."},{"type":"tool_use","id":"tool_invented","name":"Bash","input":{"command":"cat greeting.txt"}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool_invented","content":[{"type":"text","text":"hello"}]}]}}
{"type":"assistant","message":{"id":"msg_answer","role":"assistant","content":[{"type":"text","text":"The file says hello."}]}}
{"type":"result","subtype":"success","is_error":false,"result":"The file says hello."}
`

func TestTurns(t *testing.T) {
	dir := t.TempDir()
	program := "#!/bin/sh\nprintf '%s\\n' \"$@\" > args\ncat > input\ncat fixture\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(program), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fixture"), []byte(stream), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	store := openStore(t, filepath.Join(dir, "store"))
	b, err := New(Config{Runtime: faden.Local{PIDDir: dir, Dir: dir, Env: append(os.Environ(), "INVENTED_TOKEN=environment-only")}, Args: []string{"--permission-mode", "acceptEdits"}})
	if err != nil {
		t.Fatal(err)
	}
	first := faden.Turn{Session: "s", RequestID: "r1", Input: "Read greeting.txt"}
	if err := b.Run(t.Context(), first, newAttempt(t, store, first)); err != nil {
		t.Fatal(err)
	}
	c := claudeCursor(t, store)
	if len(c.SessionID) != 36 || c.SessionID[14] != '4' || c.LastAssistantID != "msg_answer" {
		t.Fatalf("cursor: %+v", c)
	}
	events, err := store.Events(t.Context(), "s", 0)
	if err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	if !strings.Contains(string(args), "--session-id\n"+c.SessionID) || strings.Contains(string(args), "environment-only") {
		t.Fatalf("args: %s", args)
	}
	// The runner stores the user message; the backend stores only what Claude says and does.
	want := []faden.Kind{faden.AssistantMessage, faden.ToolCall, faden.ToolResult, faden.AssistantMessage, faden.TurnEnded}
	var kinds []faden.Kind
	for _, e := range events {
		kinds = append(kinds, e.Kind)
		// The CLI keeps its own session; faden stores normalized fields only.
		if e.Native != nil {
			t.Fatalf("native payload stored: %q", e.Native)
		}
	}
	if !reflect.DeepEqual(kinds, want) || events[1].Action != "cat greeting.txt" || events[2].Output != "hello" {
		t.Fatalf("events: %+v", events)
	}
	second := faden.Turn{Session: "s", RequestID: "r2", Input: "Continue where you left off."}
	if err := b.Run(t.Context(), second, newAttempt(t, store, second)); err != nil {
		t.Fatal(err)
	}
	args, _ = os.ReadFile(filepath.Join(dir, "args"))
	input, _ := os.ReadFile(filepath.Join(dir, "input"))
	if !strings.Contains(string(args), "--resume\n"+c.SessionID) || !strings.Contains(string(input), "Continue where you left off.") {
		t.Fatalf("resume: %s %s", args, input)
	}
}

type inspectRuntime struct {
	store  faden.Store
	output string
	code   int
}

func (r inspectRuntime) Stop(context.Context, string) error { return nil }
func (r inspectRuntime) Run(ctx context.Context, command faden.Command) (faden.Result, error) {
	w := command.Stdout
	if sess, err := r.store.Session(ctx, "s"); err != nil || len(sess.Cursors["claude"]) == 0 {
		return faden.Result{}, errors.New("cursor missing before launch")
	}
	_, err := io.WriteString(w, r.output)
	return faden.Result{ExitCode: r.code}, err
}

func TestCheckpointAndStreamFailures(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		code         int
	}{
		{"success", stream, 0}, {"missing result", strings.Split(stream, `{"type":"result"`)[0], 0}, {"malformed", "{\n", 0}, {"exit", stream, 2}, {"error result", `{"type":"result","is_error":true}` + "\n", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openStore(t, t.TempDir())
			b, _ := New(Config{Runtime: inspectRuntime{s, tc.output, tc.code}})
			turn := faden.Turn{Session: "s", RequestID: "r"}
			err := b.Run(t.Context(), turn, newAttempt(t, s, turn))
			if (err == nil) != (tc.name == "success") {
				t.Fatalf("error: %v", err)
			}
		})
	}
}

func waitFile(t *testing.T, path string) []byte {
	t.Helper()
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			return data
		}
		select {
		case <-timer.C:
			t.Fatalf("waiting for %s: %v", path, err)
		case <-tick.C:
		}
	}
}

func runnerBackend(t *testing.T, dir string) continuationtest.Factory {
	t.Helper()
	return func() faden.Backend {
		b, err := New(Config{Runtime: faden.Local{PIDDir: dir, Dir: dir}, Args: []string{"--permission-mode", "bypassPermissions"}})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
}

func TestHostHelper(t *testing.T) {
	dir := os.Getenv("FADEN_CLAUDE_HOST")
	if dir == "" {
		return
	}
	input := "Write first.txt containing FIRST. Then run bash sleep 120. Then write second.txt and third.txt containing DONE, one after another. If interrupted and resumed, before using tools say FIRST_FILE_KNOWN if this conversation shows that first.txt was written, or FIRST_FILE_UNKNOWN otherwise."
	continuationtest.Host(t, dir, "claude", input, runnerBackend(t, dir))
}

func TestLiveResumeAfterKill(t *testing.T) {
	t.Setenv("FADEN_KILL_AFTER_FILE", "first.txt")
	if os.Getenv("FADEN_LIVE") != "1" {
		t.Skip("set FADEN_LIVE=1 to run Claude Code")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	continuationtest.KillHost(t, dir, "FADEN_CLAUDE_HOST")
	events := continuationtest.Continue(t, dir, "claude", runnerBackend(t, dir))
	var answer strings.Builder
	for _, e := range events {
		if e.Kind == faden.AssistantMessage {
			answer.WriteString(e.Text)
		}
	}
	waitFile(t, filepath.Join(dir, "second.txt"))
	waitFile(t, filepath.Join(dir, "third.txt"))
	knowledge := "indeterminate"
	if strings.Contains(answer.String(), "FIRST_FILE_KNOWN") {
		knowledge = "known"
	} else if strings.Contains(answer.String(), "FIRST_FILE_UNKNOWN") {
		knowledge = "unknown"
	}
	t.Logf("Claude knowledge of first file: %s", knowledge)
	t.Logf("Claude resume after a kill; first file before kill: FIRST; resumed assistant: %s", answer.String())
}

func TestRunnerContinuesStoredTurn(t *testing.T) {
	dir := t.TempDir()
	program := "#!/bin/sh\nprintf '%s\\n' \"$@\" > args\ncat > input\ncat fixture\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(program), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fixture"), []byte(stream), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	store, err := jsonl.Open(filepath.Join(dir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	turn, err := store.SubmitTurn(t.Context(), faden.Turn{Session: "s", RequestID: "r", Backend: "claude", Input: "original"})
	if err != nil {
		t.Fatal(err)
	}
	turn.Status, turn.Attempts = faden.Running, 1
	if err := store.SaveTurn(t.Context(), turn); err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"session_id":"12345678-1234-4123-8123-123456789abc"}`)
	if err := store.SetCursor(t.Context(), "s", "claude", raw); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(t.Context(), faden.Event{Session: "s", RequestID: "r", Kind: faden.AssistantMessage, Text: strings.Repeat("x", 200)}); err != nil {
		t.Fatal(err)
	}
	store.Close()
	store, err = jsonl.Open(filepath.Join(dir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	b, _ := New(Config{Runtime: faden.Local{PIDDir: dir, Dir: dir}})
	runner, err := faden.Open(store, map[string]faden.Backend{"claude": b}, faden.Options{Budget: 64})
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	finished, err := runner.Wait(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	if finished.Status != faden.Done || finished.Attempts != 2 {
		t.Fatalf("turn: %+v", finished)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	input, _ := os.ReadFile(filepath.Join(dir, "input"))
	if strings.Contains(string(input), strings.Repeat("x", 200)) || !strings.Contains(string(input), "Omitted events") {
		t.Fatalf("host budget ignored: %s", input)
	}
	if !strings.Contains(string(args), "--resume\n12345678-1234-4123-8123-123456789abc") || !strings.Contains(string(input), "Continue where you left off.") {
		t.Fatalf("args %s input %s", args, input)
	}
	session, err := store.Session(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	var c cursor
	json.Unmarshal(session.Cursors["claude"], &c)
	if c.LastAssistantID != "msg_answer" {
		t.Fatalf("cursor: %+v", c)
	}
}

type threadGrant struct{ revoked bool }

func (g *threadGrant) Grant(string) (string, error) { return "invented-token", nil }
func (g *threadGrant) Revoke(string)                { g.revoked = true }
func (g *threadGrant) Config() []byte {
	return []byte(`{"mcpServers":{"faden":{"type":"http","url":"http://127.0.0.1:1/mcp","headers":{"Authorization":"Bearer ${FADEN_MCP_TOKEN}"}}}}`)
}

type hostRuntime struct{ faden.Local }

func TestThreadsTokenOnlyInEnvironment(t *testing.T) {
	dir := t.TempDir()
	program := "#!/bin/sh\nprintf '%s\\n' \"$@\" > args\ncat > input\nwhile [ $# -gt 0 ]; do [ \"$1\" = --mcp-config ] && cp \"$2\" config; shift; done\nprintf '%s' \"$FADEN_MCP_TOKEN\" > token\ncat fixture\n"
	os.WriteFile(filepath.Join(dir, "claude"), []byte(program), 0700)
	os.WriteFile(filepath.Join(dir, "fixture"), []byte(stream), 0600)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	grant := &threadGrant{}
	b, err := New(Config{Runtime: hostRuntime{faden.Local{PIDDir: dir, Dir: dir}}, Threads: grant})
	if err != nil {
		t.Fatal(err)
	}
	turn := faden.Turn{Session: "s", RequestID: "r", Input: "hi"}
	if err := b.Run(t.Context(), turn, newAttempt(t, openStore(t, filepath.Join(dir, "store")), turn)); err != nil {
		t.Fatal(err)
	}
	token, _ := os.ReadFile(filepath.Join(dir, "token"))
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	config, _ := os.ReadFile(filepath.Join(dir, "config"))
	if string(token) != "invented-token" || strings.Contains(string(args), "invented-token") || strings.Contains(string(config), "invented-token") {
		t.Fatalf("token %q args %q config %q", token, args, config)
	}
	if !strings.Contains(string(args), "--mcp-config") || !strings.Contains(string(config), "${FADEN_MCP_TOKEN}") || !grant.revoked {
		t.Fatalf("args %q config %q revoked %v", args, config, grant.revoked)
	}
}

func TestKilledTurnCarriesStoredToolEvents(t *testing.T) {
	dir := t.TempDir()
	program := "#!/bin/sh\ncat > input\ncat fixture\nexec sleep 120\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(program), 0700); err != nil {
		t.Fatal(err)
	}
	partial, _, _ := strings.Cut(stream, `{"type":"assistant","message":{"id":"msg_answer"`)
	if err := os.WriteFile(filepath.Join(dir, "fixture"), []byte(partial), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	continuationtest.KillHost(t, dir, "FADEN_CLAUDE_HOST")
	if err := os.WriteFile(filepath.Join(dir, "fixture"), []byte(stream), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\ncat > input\ncat fixture\n"), 0700); err != nil {
		t.Fatal(err)
	}
	continuationtest.Continue(t, dir, "claude", runnerBackend(t, dir))
	data, err := os.ReadFile(filepath.Join(dir, "input"))
	if err != nil {
		t.Fatal(err)
	}
	var input struct{ Message struct{ Content string } }
	if err := json.Unmarshal(data, &input); err != nil {
		t.Fatal(err)
	}
	prompt := input.Message.Content
	end := strings.Index(prompt, "Continue where you left off.")
	for _, text := range []string{"cat greeting.txt", "hello"} {
		at := strings.Index(prompt, text)
		if at < 0 || end < 0 || at >= end {
			t.Fatalf("missing %q before continuation: %s", text, prompt)
		}
	}
}

func TestFailedToolResultIsMarked(t *testing.T) {
	var got []faden.Event
	emit := func(e faden.Event) error { got = append(got, e); return nil }
	line := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"no such file","is_error":true},{"type":"tool_result","tool_use_id":"t2","content":"ok"}]}}`
	if _, err := parseLine([]byte(line), &cursor{}, emit); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[0].Failed || got[0].Output != "no such file" || got[1].Failed {
		t.Fatalf("events: %+v", got)
	}
}

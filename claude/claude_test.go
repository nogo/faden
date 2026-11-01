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
	"syscall"
	"testing"
	"time"

	"github.com/nogo/faden"
	"github.com/nogo/faden/jsonl"
)

type checkpoint struct {
	data json.RawMessage
	path string
}

func (s *checkpoint) SetCursor(_ context.Context, _, _ string, data json.RawMessage) error {
	s.data = append(s.data[:0], data...)
	if s.path != "" {
		return os.WriteFile(s.path, data, 0600)
	}
	return nil
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
	store := &checkpoint{path: filepath.Join(dir, "cursor")}
	b, err := New(Config{Runtime: faden.Local{PIDDir: dir, Dir: dir, Env: append(os.Environ(), "INVENTED_TOKEN=environment-only")}, Store: store, Args: []string{"--permission-mode", "acceptEdits"}})
	if err != nil {
		t.Fatal(err)
	}
	var events []faden.Event
	emit := func(e faden.Event) error { events = append(events, e); return nil }
	raw, err := b.Run(t.Context(), faden.Turn{Session: "s", Input: "Read greeting.txt"}, nil, emit)
	if err != nil {
		t.Fatal(err)
	}
	var c cursor
	json.Unmarshal(raw, &c)
	if len(c.SessionID) != 36 || c.SessionID[14] != '4' || c.LastAssistantID != "msg_answer" {
		t.Fatalf("cursor: %s", raw)
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
		if !json.Valid(e.Native) {
			t.Fatalf("native: %q", e.Native)
		}
	}
	if !reflect.DeepEqual(kinds, want) || events[1].Action != "cat greeting.txt" || events[2].Output != "hello" {
		t.Fatalf("events: %+v", events)
	}
	_, err = b.Run(t.Context(), faden.Turn{Session: "s", Input: "Continue where you left off."}, raw, emit)
	if err != nil {
		t.Fatal(err)
	}
	args, _ = os.ReadFile(filepath.Join(dir, "args"))
	input, _ := os.ReadFile(filepath.Join(dir, "input"))
	if !strings.Contains(string(args), "--resume\n"+c.SessionID) || !strings.Contains(string(input), "Continue where you left off.") {
		t.Fatalf("resume: %s %s", args, input)
	}
}

type inspectRuntime struct {
	store  *checkpoint
	output string
	code   int
}

func (r inspectRuntime) RunStream(_ context.Context, argv []string, _ io.Reader, w io.Writer) (faden.Result, error) {
	if len(r.store.data) == 0 {
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
			s := &checkpoint{}
			b, _ := New(Config{Store: s, Runtime: inspectRuntime{s, tc.output, tc.code}})
			_, err := b.Run(t.Context(), faden.Turn{Session: "s"}, nil, func(faden.Event) error { return nil })
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

func TestHostHelper(t *testing.T) {
	dir := os.Getenv("FADEN_CLAUDE_HOST")
	if dir == "" {
		return
	}
	s := &checkpoint{path: filepath.Join(dir, "cursor")}
	b, _ := New(Config{Store: s, Runtime: faden.Local{PIDDir: dir, Dir: dir}, Args: []string{"--permission-mode", "bypassPermissions"}})
	input := "Write first.txt containing FIRST. Then run bash sleep 120. Then write second.txt and third.txt containing DONE, one after another. If interrupted and resumed, before using tools say FIRST_FILE_KNOWN if this conversation shows that first.txt was written, or FIRST_FILE_UNKNOWN otherwise."
	_, err := b.Run(t.Context(), faden.Turn{Session: "s", Input: input}, nil, func(faden.Event) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
}

func startHost(t *testing.T, dir string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHostHelper$")
	cmd.Env = append(os.Environ(), "FADEN_CLAUDE_HOST="+dir)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	return cmd
}

func TestLiveResumeAfterKill(t *testing.T) {
	if os.Getenv("FADEN_LIVE") != "1" {
		t.Skip("set FADEN_LIVE=1 to run Claude Code")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cmd := startHost(t, dir)
	waitFile(t, filepath.Join(dir, "first.txt"))
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	raw := waitFile(t, filepath.Join(dir, "cursor"))
	s := &checkpoint{path: filepath.Join(dir, "cursor")}
	b, _ := New(Config{Store: s, Runtime: faden.Local{PIDDir: dir, Dir: dir}, Args: []string{"--permission-mode", "bypassPermissions"}})
	var answer strings.Builder
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	_, err := b.Run(ctx, faden.Turn{Session: "s", Input: "Continue where you left off."}, raw, func(e faden.Event) error {
		if e.Kind == faden.AssistantMessage {
			answer.WriteString(e.Text)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
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
	store.Close()
	store, err = jsonl.Open(filepath.Join(dir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	b, _ := New(Config{Store: store, Runtime: faden.Local{PIDDir: dir, Dir: dir}})
	runner, err := faden.Open(store, map[string]faden.Backend{"claude": b})
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

func TestThreadsTokenOnlyInEnvironment(t *testing.T) {
	dir := t.TempDir()
	program := "#!/bin/sh\nprintf '%s\\n' \"$@\" > args\ncat > input\nwhile [ $# -gt 0 ]; do [ \"$1\" = --mcp-config ] && cp \"$2\" config; shift; done\nprintf '%s' \"$FADEN_MCP_TOKEN\" > token\ncat fixture\n"
	os.WriteFile(filepath.Join(dir, "claude"), []byte(program), 0700)
	os.WriteFile(filepath.Join(dir, "fixture"), []byte(stream), 0600)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	grant := &threadGrant{}
	b, err := New(Config{Runtime: faden.Local{PIDDir: dir, Dir: dir}, Store: &checkpoint{}, Threads: grant})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Run(t.Context(), faden.Turn{Session: "s", Input: "hi"}, nil, func(faden.Event) error { return nil }); err != nil {
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

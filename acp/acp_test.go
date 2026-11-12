package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nogo/faden"
	"github.com/nogo/faden/internal/continuationtest"
	"github.com/nogo/faden/jsonl"
)

// These invented lines exercise the wire format. They are not a real-run recording.
const turnLines = `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess_invented","update":{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"Plan: read."}}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess_invented","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"I will "}}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess_invented","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"read it."}}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess_invented","update":{"sessionUpdate":"tool_call","toolCallId":"call_1","title":"Read greeting.txt","kind":"read","status":"pending","rawInput":{"path":"greeting.txt"}}}}
{"jsonrpc":"2.0","id":"perm-7","method":"session/request_permission","params":{"sessionId":"sess_invented","toolCall":{"toolCallId":"call_1"},"options":[{"optionId":"yes","name":"Allow","kind":"allow_once"},{"optionId":"no","name":"Reject","kind":"reject_once"}]}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess_invented","update":{"sessionUpdate":"tool_call_update","toolCallId":"call_1","status":"in_progress"}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess_invented","update":{"sessionUpdate":"tool_call_update","toolCallId":"call_1","status":"completed","content":[{"type":"content","content":{"type":"text","text":"hello"}}]}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess_invented","update":{"sessionUpdate":"tool_call","toolCallId":"call_2","title":"Edit a.txt","kind":"edit","status":"in_progress"}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess_invented","update":{"sessionUpdate":"tool_call_update","toolCallId":"call_2","status":"failed","content":[{"type":"diff","path":"/work/a.txt","oldText":"a","newText":"b"}]}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess_invented","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"It says hello."}}}}
{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}
`

// replayLines is what session/load sends before its answer: the history the
// store already holds.
const replayLines = `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess_invented","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"old question"}}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess_invented","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"old answer"}}}}
`

const loadCaps = `{"protocolVersion":1,"agentCapabilities":{"loadSession":true}}`

// A fake ACP agent that logs its arguments, HOST_VAR and every line it
// receives, answers initialize with "caps", and replays "fixture" after
// session/prompt.
const fakeAgent = `#!/bin/sh
printf '%s\n' "$@" > args
printf '%s' "$HOST_VAR" > var
while IFS= read -r line; do
  printf '%s\n' "$line" >> got
  case $line in
    *'"method":"initialize"'*) printf '{"jsonrpc":"2.0","id":1,"result":%s}\n' "$(cat caps)" ;;
    *'"method":"session/new"'*) echo '{"jsonrpc":"2.0","id":2,"result":{"sessionId":"sess_invented"}}' ;;
    *'"method":"session/load"'*) cat replay; echo '{"jsonrpc":"2.0","id":2,"result":{}}' ;;
    *'"method":"session/resume"'*) echo '{"jsonrpc":"2.0","id":2,"result":{}}' ;;
    *'"method":"session/prompt"'*) cat fixture ;;
  esac
done
`

func setup(t *testing.T, caps, fixture string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{"caps": caps, "fixture": fixture, "replay": replayLines} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "agent"), []byte(fakeAgent), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return dir
}

// checkpoint is a jsonl store that records what the fake agent had received
// when the cursor was saved.
type checkpoint struct {
	*jsonl.Store
	dir     string
	gotThen string
}

func (s *checkpoint) SetCursor(ctx context.Context, session, backend string, data json.RawMessage) error {
	got, _ := os.ReadFile(filepath.Join(s.dir, "got"))
	s.gotThen = string(got)
	return s.Store.SetCursor(ctx, session, backend, data)
}

func newBackend(t *testing.T, dir string, cfg Config) (*Backend, *checkpoint) {
	t.Helper()
	store, err := jsonl.Open(filepath.Join(dir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	cfg.Runtime = faden.Local{PIDDir: dir, Dir: dir}
	if cfg.Command == nil {
		cfg.Command = []string{"agent", "--acp"}
	}
	b, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return b, &checkpoint{Store: store, dir: dir}
}

// run submits turn to store and runs it once, as the runner would without catch-up.
func run(t *testing.T, b *Backend, store faden.Store, turn faden.Turn) error {
	t.Helper()
	turn.Backend = "acp"
	if _, err := store.SubmitTurn(t.Context(), turn); err != nil {
		t.Fatal(err)
	}
	a, err := faden.NewAttempt(t.Context(), store, turn)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	return b.Run(ctx, turn, a)
}

func gotMethods(t *testing.T, dir string) []string {
	t.Helper()
	data, _ := os.ReadFile(filepath.Join(dir, "got"))
	var methods []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		var m struct {
			Method string `json:"method"`
		}
		json.Unmarshal([]byte(line), &m)
		methods = append(methods, m.Method)
	}
	return methods
}

// sentLine returns the last line the agent received for method.
func sentLine(t *testing.T, dir, method string) string {
	t.Helper()
	data, _ := os.ReadFile(filepath.Join(dir, "got"))
	var last string
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.Contains(line, `"method":"`+method+`"`) {
			last = line
		}
	}
	return last
}

func events(t *testing.T, store faden.Store) []faden.Event {
	t.Helper()
	evs, err := faden.CollectEvents(store.Events(t.Context(), "s", 0))
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func kinds(evs []faden.Event) []faden.Kind {
	var out []faden.Kind
	for _, e := range evs {
		out = append(out, e.Kind)
	}
	return out
}

func TestTurnThenLoadSkipsReplay(t *testing.T) {
	dir := setup(t, loadCaps, turnLines)
	b, store := newBackend(t, dir, Config{})
	if err := run(t, b, store, faden.Turn{Session: "s", RequestID: "r1", Input: "Read greeting.txt"}); err != nil {
		t.Fatal(err)
	}
	sess, err := store.Session(t.Context(), "s")
	if err != nil || string(sess.Cursors["acp"]) != `{"session_id":"sess_invented"}` {
		t.Fatalf("cursor: %s, %v", sess.Cursors["acp"], err)
	}
	if strings.Contains(store.gotThen, "session/prompt") || !strings.Contains(store.gotThen, "session/new") {
		t.Fatalf("cursor saved after session/prompt was sent; the agent had received:\n%s", store.gotThen)
	}
	evs := events(t, store)
	want := []faden.Kind{faden.AssistantMessage, faden.ToolCall, faden.ToolResult, faden.ToolCall, faden.ToolResult, faden.AssistantMessage, faden.TurnEnded}
	if !reflect.DeepEqual(kinds(evs), want) {
		t.Fatalf("kinds: %v", kinds(evs))
	}
	if evs[0].Text != "I will read it." || evs[1].Tool != "read" || evs[1].Action != "Read greeting.txt" || string(evs[1].Input) != `{"path":"greeting.txt"}` {
		t.Fatalf("first events: %+v", evs[:2])
	}
	if evs[2].Output != "hello" || evs[2].Failed || evs[4].Output != "changed /work/a.txt" || !evs[4].Failed || evs[6].Text != "end_turn" {
		t.Fatalf("results: %+v", evs)
	}
	for _, e := range evs {
		if e.Native != nil {
			t.Fatalf("native payload stored: %q", e.Native)
		}
	}
	got, _ := os.ReadFile(filepath.Join(dir, "got"))
	// Without Setup.Dir the agent gets the host's working directory.
	cwd, _ := os.Getwd()
	for _, need := range []string{`"id":"perm-7","jsonrpc":"2.0","result":{"outcome":{"optionId":"no","outcome":"selected"}}`, `"cwd":"` + cwd + `"`, `"text":"Read greeting.txt"`, `"protocolVersion":1`} {
		if !strings.Contains(string(got), need) {
			t.Fatalf("missing %s in:\n%s", need, got)
		}
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	if string(args) != "--acp\n" {
		t.Fatalf("args: %q", args)
	}

	os.Remove(filepath.Join(dir, "got"))
	if err := run(t, b, store, faden.Turn{Session: "s", RequestID: "r2", Input: "again"}); err != nil {
		t.Fatal(err)
	}
	if methods := gotMethods(t, dir); !reflect.DeepEqual(methods, []string{"initialize", "session/load", "session/prompt", ""}) {
		t.Fatalf("methods: %v", methods)
	}
	if line := sentLine(t, dir, "session/load"); !strings.Contains(line, `"sessionId":"sess_invented"`) {
		t.Fatalf("load: %s", line)
	}
	for _, e := range events(t, store) {
		if strings.Contains(e.Text, "old answer") {
			t.Fatalf("replayed history stored: %+v", e)
		}
	}
	if got := len(events(t, store)); got != 2*len(want) {
		t.Fatalf("events after second turn: %d, want %d", got, 2*len(want))
	}
}

func TestResumeIsPreferredOverLoad(t *testing.T) {
	dir := setup(t, `{"protocolVersion":1,"agentCapabilities":{"loadSession":true,"sessionCapabilities":{"resume":{}}}}`, turnLines)
	b, store := newBackend(t, dir, Config{})
	for _, id := range []string{"r1", "r2"} {
		if err := run(t, b, store, faden.Turn{Session: "s", RequestID: id, Input: "hi"}); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := os.ReadFile(filepath.Join(dir, "got"))
	if strings.Contains(string(got), "session/load") || !strings.Contains(sentLine(t, dir, "session/resume"), `"sessionId":"sess_invented"`) {
		t.Fatalf("lines:\n%s", got)
	}
}

func TestAgentWithoutLoadStartsNewSessionAndSaysSo(t *testing.T) {
	dir := setup(t, `{"protocolVersion":1}`, turnLines)
	b, store := newBackend(t, dir, Config{})
	for _, id := range []string{"r1", "r2"} {
		if err := run(t, b, store, faden.Turn{Session: "s", RequestID: id, Input: "hi"}); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(strings.Join(gotMethods(t, dir), " "), "session/new"); n != 2 {
		t.Fatalf("session/new sent %d times", n)
	}
	var lost int
	for _, e := range events(t, store) {
		if e.Kind == faden.ErrorEvent && strings.Contains(e.Text, "cannot resume session sess_invented") {
			lost++
		}
	}
	if lost != 1 {
		t.Fatalf("lost-context events: %d", lost)
	}
}

func TestApproveSelectsAllowOnce(t *testing.T) {
	dir := setup(t, loadCaps, turnLines)
	b, store := newBackend(t, dir, Config{Approve: true})
	if err := run(t, b, store, faden.Turn{Session: "s", RequestID: "r", Input: "hi"}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "got"))
	if !strings.Contains(string(got), `"outcome":{"optionId":"yes","outcome":"selected"}`) {
		t.Fatalf("lines:\n%s", got)
	}
}

func TestSetupReachesAgentAndSystemGoesToNewSessionOnly(t *testing.T) {
	dir := setup(t, loadCaps, turnLines)
	b, store := newBackend(t, dir, Config{})
	runner, err := faden.Open(store, map[string]faden.Backend{"acp": b}, faden.Options{Prepare: func(context.Context, faden.Turn) (faden.Setup, error) {
		return faden.Setup{Dir: "/invented/ws", Args: []string{"--extra"}, Env: []string{"HOST_VAR=invented"}, System: "invented rules"}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	var prompts []string
	for _, id := range []string{"r1", "r2"} {
		if _, err := runner.Submit(t.Context(), faden.Submission{Session: "s", RequestID: id, Input: "hi " + id, Backend: "acp"}); err != nil {
			t.Fatal(err)
		}
		if got, err := runner.Wait(t.Context(), id); err != nil || got.Status != faden.Done {
			t.Fatalf("turn %s = %+v, %v", id, got, err)
		}
		prompts = append(prompts, sentLine(t, dir, "session/prompt"))
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	env, _ := os.ReadFile(filepath.Join(dir, "var"))
	if string(args) != "--acp\n--extra\n" || string(env) != "invented" {
		t.Fatalf("args %q env %q", args, env)
	}
	if !strings.Contains(sentLine(t, dir, "session/load"), `"cwd":"/invented/ws"`) {
		t.Fatalf("load: %s", sentLine(t, dir, "session/load"))
	}
	if !strings.Contains(prompts[0], `"text":"invented rules\n\nhi r1"`) || strings.Contains(prompts[1], "invented rules") {
		t.Fatalf("prompts:\n%s\n%s", prompts[0], prompts[1])
	}
}

func TestFailures(t *testing.T) {
	cases := map[string]struct {
		caps, fixture string
		check         func(error) bool
	}{
		"refusal": {loadCaps, `{"jsonrpc":"2.0","id":3,"result":{"stopReason":"refusal"}}` + "\n", func(err error) bool {
			return strings.Contains(err.Error(), "prompt ended: refusal")
		}},
		"auth required": {loadCaps, `{"jsonrpc":"2.0","id":3,"error":{"code":-32000,"message":"Authentication required"}}` + "\n", func(err error) bool {
			_, ok := errors.AsType[*faden.UnavailableError](err)
			return ok
		}},
		"other version": {`{"protocolVersion":2}`, "", func(err error) bool {
			return strings.Contains(err.Error(), "ACP version 2")
		}},
		"no end":    {loadCaps, `{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"x"}}}}` + "\n", nil},
		"malformed": {loadCaps, "{\n", nil},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir := setup(t, c.caps, c.fixture)
			if name == "no end" {
				// The agent exits after its last line, without a prompt answer.
				script := strings.Replace(fakeAgent, `*'"method":"session/prompt"'*) cat fixture ;;`, `*'"method":"session/prompt"'*) cat fixture; exit 3 ;;`, 1)
				os.WriteFile(filepath.Join(dir, "agent"), []byte(script), 0700)
			}
			b, store := newBackend(t, dir, Config{})
			err := run(t, b, store, faden.Turn{Session: "s", RequestID: "r", Input: "x"})
			if err == nil {
				t.Fatal("want an error")
			}
			if c.check != nil && !c.check(err) {
				t.Fatalf("error: %v", err)
			}
		})
	}
}

// cancelRuntime answers the handshake in process and records whether
// session/cancel reaches the agent before it is stopped.
type cancelRuntime struct {
	ready     chan struct{}
	cancelled chan struct{}
}

func (r *cancelRuntime) Stop(context.Context, string) error { return nil }
func (r *cancelRuntime) Run(ctx context.Context, cmd faden.Command) (faden.Result, error) {
	scan := bufio.NewScanner(cmd.Stdin)
	for scan.Scan() {
		var m message
		if err := json.Unmarshal(scan.Bytes(), &m); err != nil {
			return faden.Result{}, err
		}
		switch m.Method {
		case "initialize":
			fmt.Fprintln(cmd.Stdout, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}`)
		case "session/new":
			fmt.Fprintln(cmd.Stdout, `{"jsonrpc":"2.0","id":2,"result":{"sessionId":"sess-cancel"}}`)
		case "session/prompt":
			close(r.ready)
		case "session/cancel":
			if !strings.Contains(string(m.Params), `"sessionId":"sess-cancel"`) {
				return faden.Result{}, fmt.Errorf("bad cancel: %s", m.Params)
			}
			close(r.cancelled)
			<-ctx.Done()
			return faden.Result{}, ctx.Err()
		}
	}
	return faden.Result{}, scan.Err()
}

func TestCancelSendsSessionCancelBeforeRuntimeEnds(t *testing.T) {
	runtime := &cancelRuntime{ready: make(chan struct{}), cancelled: make(chan struct{})}
	b, err := New(Config{Runtime: runtime, Command: []string{"agent"}})
	if err != nil {
		t.Fatal(err)
	}
	store, err := jsonl.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	turn := faden.Turn{Session: "s", RequestID: "cancel", Input: "wait", Backend: "acp"}
	if _, err := store.SubmitTurn(t.Context(), turn); err != nil {
		t.Fatal(err)
	}
	a, err := faden.NewAttempt(t.Context(), store, turn)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx, turn, a) }()
	select {
	case <-runtime.ready:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancel returned nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not stop runtime")
	}
	select {
	case <-runtime.cancelled:
	default:
		t.Fatal("runtime ended without session/cancel")
	}
}

func TestNewNeedsRuntimeAndCommand(t *testing.T) {
	if _, err := New(Config{Command: []string{"agent"}}); err == nil {
		t.Fatal("no runtime accepted")
	}
	if _, err := New(Config{Runtime: faden.Local{}}); err == nil {
		t.Fatal("no command accepted")
	}
}

func runnerBackend(t *testing.T, dir string) continuationtest.Factory {
	t.Helper()
	return func() faden.Backend {
		b, err := New(Config{Runtime: faden.Local{PIDDir: dir, Dir: dir}, Command: []string{"agent", "--acp"}})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
}

func TestHostHelper(t *testing.T) {
	dir := os.Getenv("FADEN_ACP_HOST")
	if dir == "" {
		return
	}
	continuationtest.Host(t, dir, "acp", "Read greeting.txt", runnerBackend(t, dir))
}

func TestKilledTurnLoadsSessionAndCarriesStoredToolEvents(t *testing.T) {
	t.Setenv("FADEN_TEST_CONTEXT", "Vault rules for this turn.")
	partial, _, _ := strings.Cut(turnLines, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess_invented","update":{"sessionUpdate":"tool_call","toolCallId":"call_2"`)
	dir := setup(t, loadCaps, partial)
	continuationtest.KillHost(t, dir, "FADEN_ACP_HOST")
	if err := os.WriteFile(filepath.Join(dir, "fixture"), []byte(turnLines), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "got")); err != nil {
		t.Fatal(err)
	}
	evs := continuationtest.Continue(t, dir, "acp", runnerBackend(t, dir))
	if methods := gotMethods(t, dir); !reflect.DeepEqual(methods[:3], []string{"initialize", "session/load", "session/prompt"}) {
		t.Fatalf("methods: %v", methods)
	}
	var m struct {
		Params struct{ Prompt []struct{ Text string } }
	}
	if err := json.Unmarshal([]byte(sentLine(t, dir, "session/prompt")), &m); err != nil || len(m.Params.Prompt) == 0 {
		t.Fatalf("prompt line: %v", err)
	}
	prompt := m.Params.Prompt[0].Text
	end := strings.Index(prompt, "Continue where you left off.")
	for _, text := range []string{"Read greeting.txt", "hello", "Vault rules for this turn."} {
		if at := strings.Index(prompt, text); at < 0 || end < 0 || at >= end {
			t.Fatalf("missing %q before continuation: %s", text, prompt)
		}
	}
	if evs[len(evs)-1].Kind != faden.TurnEnded {
		t.Fatalf("continued events: %v", kinds(evs))
	}
}

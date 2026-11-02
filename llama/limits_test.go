package llama_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/nogo/faden"
	"github.com/nogo/faden/llama"
)

const toolCallAnswer = `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_%d","type":"function","function":{"name":"big","arguments":"{}"}}]}}]}`

// loopServer answers every request with a call to the "big" tool, or with
// finalAnswer once it was called after calls requests. It keeps raw bodies.
type loopServer struct {
	mu     sync.Mutex
	bodies []string
	stopAt int // answer finally from this request on; 0 means never
}

func (s *loopServer) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	s.mu.Lock()
	s.bodies = append(s.bodies, string(body))
	n := len(s.bodies)
	stop := s.stopAt > 0 && n >= s.stopAt
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if stop {
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"done"}}]}`)
		return
	}
	io.WriteString(w, strings.Replace(toolCallAnswer, "%d", string(rune('a'+n)), 1))
}

func (s *loopServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bodies)
}

// bigTool returns 100 KB of three-byte runes, so a byte cut can split one.
func bigTool() llama.Tool {
	return llama.Tool{
		Name: "big",
		Run:  func(context.Context, json.RawMessage) (string, error) { return strings.Repeat("€", 100<<10/3), nil },
	}
}

func resultOutputs(t *testing.T, store faden.Store) []string {
	t.Helper()
	events, err := store.Events(t.Context(), "s1", 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range events {
		if e.Kind == faden.ToolResult {
			out = append(out, e.Output)
		}
	}
	return out
}

func TestToolOutputIsCutStoredAndSentCut(t *testing.T) {
	ls := &loopServer{stopAt: 2}
	srv := httptest.NewServer(ls)
	t.Cleanup(srv.Close)
	store := openStore(t, t.TempDir())
	b, _ := llama.New(llama.Config{URL: srv.URL + "/v1", Tools: []llama.Tool{bigTool()}, MaxOutput: 1000})
	if got := run(t, store, b, "r1", "go"); got.Status != faden.Done {
		t.Fatalf("turn = %+v", got)
	}
	outs := resultOutputs(t, store)
	if len(outs) != 1 {
		t.Fatalf("results = %d", len(outs))
	}
	out := outs[0]
	if !utf8.ValidString(out) {
		t.Fatal("stored result is not valid UTF-8")
	}
	// 1000 is not a multiple of 3: the cut falls back to 999 bytes.
	if !strings.HasPrefix(out, strings.Repeat("€", 333)+"\n[output cut") || len(out) > 1100 {
		t.Fatalf("stored result = %.60q... (%d bytes)", out, len(out))
	}
	var req request
	json.Unmarshal([]byte(ls.bodies[1]), &req)
	last := req.Messages[len(req.Messages)-1]
	if last.Role != "tool" || last.Content != out {
		t.Fatalf("model saw %.60q, store has %.60q", last.Content, out)
	}
}

func TestToolOutputDefaultLimitIs12000Bytes(t *testing.T) {
	ls := &loopServer{stopAt: 2}
	srv := httptest.NewServer(ls)
	t.Cleanup(srv.Close)
	store := openStore(t, t.TempDir())
	b, _ := llama.New(llama.Config{URL: srv.URL + "/v1", Tools: []llama.Tool{bigTool()}})
	run(t, store, b, "r1", "go")
	out := resultOutputs(t, store)[0]
	body, _, _ := strings.Cut(out, "\n[output cut")
	if len(body) != 12000 || !utf8.ValidString(out) {
		t.Fatalf("kept %d bytes before the marker, valid %v", len(body), utf8.ValidString(out))
	}
}

func TestShortToolOutputIsNotChanged(t *testing.T) {
	_, srv := newReplay(t, "add_call.json", "final.json")
	store := openStore(t, t.TempDir())
	var runs int
	b, _ := llama.New(llama.Config{URL: srv.URL + "/v1", Tools: []llama.Tool{addTool(&runs)}, MaxOutput: 1})
	run(t, store, b, "r1", "2+3")
	if outs := resultOutputs(t, store); len(outs) != 1 || outs[0] != "5" {
		t.Fatalf("results = %q", outs)
	}
}

func TestBashAndReadThreadOutputAreCut(t *testing.T) {
	_, srv := newReplay(t, "bash_call.json", "final.json")
	dir := t.TempDir()
	store := openStore(t, t.TempDir())
	b, _ := llama.New(llama.Config{URL: srv.URL + "/v1", Runtime: faden.Local{PIDDir: dir, Dir: dir}, MaxOutput: 5})
	run(t, store, b, "r1", "go")
	outs := resultOutputs(t, store)
	if len(outs) != 1 || !strings.Contains(outs[0], "[output cut at 5 bytes]") {
		t.Fatalf("bash result = %q", outs)
	}
}

func TestMaxTokensIsSentOnlyWhenSet(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tokens int
		want   string
	}{{"set", 321, `"max_tokens":321`}, {"unset", 0, ""}} {
		t.Run(tc.name, func(t *testing.T) {
			ls := &loopServer{stopAt: 1}
			srv := httptest.NewServer(ls)
			t.Cleanup(srv.Close)
			b, _ := llama.New(llama.Config{URL: srv.URL + "/v1", MaxTokens: tc.tokens})
			run(t, openStore(t, t.TempDir()), b, "r1", "go")
			has := strings.Contains(ls.bodies[0], "max_tokens")
			if tc.want == "" && has || tc.want != "" && !strings.Contains(ls.bodies[0], tc.want) {
				t.Fatalf("body = %s", ls.bodies[0])
			}
		})
	}
}

func TestMaxStepsBoundsModelCalls(t *testing.T) {
	for _, tc := range []struct {
		name         string
		steps, calls int
	}{{"configured", 3, 3}, {"default", 0, 40}} {
		t.Run(tc.name, func(t *testing.T) {
			ls := &loopServer{}
			srv := httptest.NewServer(ls)
			t.Cleanup(srv.Close)
			b, _ := llama.New(llama.Config{URL: srv.URL + "/v1", Tools: []llama.Tool{bigTool()}, MaxSteps: tc.steps, MaxOutput: 10})
			got := run(t, openStore(t, t.TempDir()), b, "r1", "go")
			if got.Status != faden.Failed || !strings.Contains(got.Error, "no final answer") {
				t.Fatalf("turn = %+v", got)
			}
			if ls.count() != tc.calls {
				t.Fatalf("model calls = %d, want %d", ls.count(), tc.calls)
			}
		})
	}
}

// TestStoredBytesOfLargeToolOutput logs the session file size for one 100 KB
// tool output, for docs/decisions.md.
func TestStoredBytesOfLargeToolOutput(t *testing.T) {
	ls := &loopServer{stopAt: 2}
	srv := httptest.NewServer(ls)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	store := openStore(t, dir)
	b, _ := llama.New(llama.Config{URL: srv.URL + "/v1", Tools: []llama.Tool{bigTool()}})
	run(t, store, b, "r1", "go")
	info, err := os.Stat(dir + "/s1.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("session file: %d bytes", info.Size())
	if info.Size() > 20<<10 {
		t.Fatalf("session file holds %d bytes for one 100 KB tool output", info.Size())
	}
}

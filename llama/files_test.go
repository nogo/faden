package llama_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nogo/faden"
	"github.com/nogo/faden/container"
	"github.com/nogo/faden/llama"
)

type fileCall struct {
	name string
	args map[string]any
}

// callServer answers each request with the next call, then with "done".
func callServer(t *testing.T, calls ...fileCall) *httptest.Server {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		io.Copy(io.Discard, req.Body)
		i := int(n.Add(1)) - 1
		w.Header().Set("Content-Type", "application/json")
		if i >= len(calls) {
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"done"}}]}`)
			return
		}
		args, _ := json.Marshal(calls[i].args)
		msg := map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{
			"id": fmt.Sprintf("call_%d", i), "type": "function",
			"function": map[string]any{"name": calls[i].name, "arguments": string(args)},
		}}}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": msg}}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// runFileTools runs one turn with the calls and returns the stored results.
func runFileTools(t *testing.T, rt faden.Runtime, dir string, calls ...fileCall) []faden.Event {
	t.Helper()
	srv := callServer(t, calls...)
	store := openStore(t, t.TempDir())
	b, err := llama.New(llama.Config{URL: srv.URL + "/v1", Runtime: rt, FileTools: true, MaxOutput: 200})
	if err != nil {
		t.Fatal(err)
	}
	r, err := faden.Open(store, map[string]faden.Backend{"llama": b}, faden.Options{Prepare: func(context.Context, faden.Turn) (faden.Setup, error) { return faden.Setup{Dir: dir}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Submit(t.Context(), faden.Submission{Session: "s1", RequestID: "r1", Input: "go", Backend: "llama", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if got, err := r.Wait(t.Context(), "r1"); err != nil || got.Status != faden.Done {
		t.Fatalf("turn = %+v, %v", got, err)
	}
	events, _ := store.Events(t.Context(), "s1", 0)
	var results []faden.Event
	for _, e := range events {
		if e.Kind == faden.ToolResult {
			results = append(results, e)
		}
	}
	if len(results) != len(calls) {
		t.Fatalf("%d results for %d calls", len(results), len(calls))
	}
	return results
}

func localRuntime(t *testing.T, dir string) faden.Runtime {
	return faden.Local{PIDDir: t.TempDir(), Dir: dir}
}

func TestFileToolsWriteReadEdit(t *testing.T) {
	dir := t.TempDir()
	res := runFileTools(t, localRuntime(t, dir), dir,
		fileCall{"write", map[string]any{"path": "sub/a.txt", "content": "one\ntwo\nthree\nfour\n"}},
		fileCall{"read", map[string]any{"path": "sub/a.txt", "offset": 2, "limit": 2}},
		fileCall{"edit", map[string]any{"path": "sub/a.txt", "old": "three", "new": "3"}},
		fileCall{"read", map[string]any{"path": "sub/a.txt"}},
	)
	for _, e := range res {
		if e.Failed {
			t.Fatalf("failed: %q", e.Output)
		}
	}
	if res[1].Output != "two\nthree\n" {
		t.Fatalf("partial read = %q", res[1].Output)
	}
	if res[3].Output != "one\ntwo\n3\nfour\n" {
		t.Fatalf("read after edit = %q", res[3].Output)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "sub/a.txt")); string(got) != "one\ntwo\n3\nfour\n" {
		t.Fatalf("file = %q", got)
	}
}

func TestEditFailsOnZeroOrSeveralMatches(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f"), []byte("x x y"), 0o644)
	res := runFileTools(t, localRuntime(t, dir), dir,
		fileCall{"edit", map[string]any{"path": "f", "old": "x", "new": "z"}},
		fileCall{"edit", map[string]any{"path": "f", "old": "q", "new": "z"}},
	)
	if !res[0].Failed || !strings.Contains(res[0].Output, "2 times") {
		t.Fatalf("several = %+v", res[0])
	}
	if !res[1].Failed || !strings.Contains(res[1].Output, "not found") {
		t.Fatalf("zero = %+v", res[1])
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "f")); string(got) != "x x y" {
		t.Fatalf("file changed to %q", got)
	}
}

func TestFilePathsOutsideDirAreRejected(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "work")
	outside := filepath.Join(root, "outside")
	os.Mkdir(dir, 0o755)
	os.Mkdir(outside, 0o755)
	os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0o644)
	os.Symlink(outside, filepath.Join(dir, "link"))
	os.Symlink(filepath.Join(outside, "secret"), filepath.Join(dir, "filelink"))
	paths := []string{"../outside/secret", "sub/../../outside/secret", filepath.Join(outside, "secret"), "link/secret", "filelink", "link/new"}
	var calls []fileCall
	for _, p := range paths {
		calls = append(calls, fileCall{"read", map[string]any{"path": p}})
	}
	calls = append(calls, fileCall{"write", map[string]any{"path": "link/new", "content": "x"}},
		fileCall{"edit", map[string]any{"path": "filelink", "old": "secret", "new": "x"}})
	for i, e := range runFileTools(t, localRuntime(t, dir), dir, calls...) {
		if !e.Failed || strings.Contains(e.Output, "secret") && !strings.Contains(e.Output, "outside the working") {
			t.Fatalf("call %d: %+v", i, e)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "new")); err == nil {
		t.Fatal("write escaped through the symlink")
	}
	if got, _ := os.ReadFile(filepath.Join(outside, "secret")); string(got) != "secret" {
		t.Fatalf("edit escaped: %q", got)
	}
}

func TestReadRejectsBinaryAndCutsOutput(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "img.png"), []byte("\x89PNG\r\n\x1a\n\x00\x00\x00"), 0o644)
	os.WriteFile(filepath.Join(dir, "big"), []byte(strings.Repeat("a", 5000)), 0o644)
	res := runFileTools(t, localRuntime(t, dir), dir,
		fileCall{"read", map[string]any{"path": "img.png"}},
		fileCall{"read", map[string]any{"path": "big"}},
		fileCall{"read", map[string]any{"path": "missing"}},
	)
	if !res[0].Failed || !strings.Contains(res[0].Output, "binary") {
		t.Fatalf("binary = %+v", res[0])
	}
	if res[1].Failed || !strings.Contains(res[1].Output, "[output cut at 200 bytes]") || len(res[1].Output) > 260 {
		t.Fatalf("big = %d bytes, failed %v", len(res[1].Output), res[1].Failed)
	}
	if !res[2].Failed {
		t.Fatal("missing file did not fail")
	}
}

// fakePodman runs the command after "exec" locally, so container.Runtime
// is exercised without an engine.
const fakePodman = `#!/bin/sh
shift
wd=
while :; do
  case "$1" in
    --interactive) shift;;
    --workdir) wd=$2; shift 2;;
    --env) shift 2;;
    *) break;;
  esac
done
shift # container name
[ -n "$wd" ] && cd "$wd"
exec "$@"
`

func TestFileToolsThroughContainer(t *testing.T) {
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "podman"), []byte(fakePodman), 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	dir := t.TempDir()
	rt := container.Runtime{Engine: "podman", Container: "box"}
	res := runFileTools(t, rt, dir,
		fileCall{"write", map[string]any{"path": "a.txt", "content": "hello world"}},
		fileCall{"edit", map[string]any{"path": "a.txt", "old": "world", "new": "there"}},
		fileCall{"read", map[string]any{"path": "a.txt"}},
		fileCall{"read", map[string]any{"path": "../x"}},
	)
	if res[2].Output != "hello there" || res[2].Failed {
		t.Fatalf("read = %+v", res[2])
	}
	if !res[3].Failed || !strings.Contains(res[3].Output, "outside") {
		t.Fatalf("escape = %+v", res[3])
	}
}

func TestFileToolNameCollisionIsAnErrorAtNew(t *testing.T) {
	rt := faden.Local{}
	for _, name := range []string{"read", "write", "edit"} {
		tool := llama.Tool{Name: name, Run: func(_ context.Context, _ json.RawMessage) (string, error) { return "", nil }}
		if _, err := llama.New(llama.Config{Runtime: rt, FileTools: true, Tools: []llama.Tool{tool}}); err == nil {
			t.Fatalf("%s: no error", name)
		}
		if _, err := llama.New(llama.Config{Runtime: rt, Tools: []llama.Tool{tool}}); err != nil {
			t.Fatalf("%s without FileTools: %v", name, err)
		}
	}
	if _, err := llama.New(llama.Config{FileTools: true}); err == nil {
		t.Fatal("FileTools without Runtime accepted")
	}
}

// Live runs one turn against a real llama-server that edits a file.
func TestLiveEditsFile(t *testing.T) {
	if os.Getenv("FADEN_LIVE") != "1" {
		t.Skip("set FADEN_LIVE=1, FADEN_LLAMA_URL and FADEN_LLAMA_MODEL")
	}
	url, model := os.Getenv("FADEN_LLAMA_URL"), os.Getenv("FADEN_LLAMA_MODEL")
	if url == "" || model == "" {
		t.Fatal("FADEN_LLAMA_URL and FADEN_LLAMA_MODEL are required")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "greeting.txt")
	if err := os.WriteFile(path, []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := llama.New(llama.Config{URL: url, Model: model, Runtime: localRuntime(t, dir), FileTools: true})
	if err != nil {
		t.Fatal(err)
	}
	r, err := faden.Open(openStore(t, t.TempDir()), map[string]faden.Backend{"llama": b},
		faden.Options{Prepare: func(context.Context, faden.Turn) (faden.Setup, error) { return faden.Setup{Dir: dir}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Submit(t.Context(), faden.Submission{Session: "s1", RequestID: "r1", Input: "Use the edit tool to change the word world to there in greeting.txt, then say done.", Backend: "llama"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	got, err := r.Wait(ctx, "r1")
	if err != nil || got.Status != faden.Done {
		t.Fatalf("turn = %+v, %v", got, err)
	}
	if text, _ := os.ReadFile(path); !strings.Contains(string(text), "hello there") {
		t.Fatalf("file = %q", text)
	}
}

package llama_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nogo/faden"
	"github.com/nogo/faden/llama"
)

// TestEffortIsSentInEveryRequest checks the request bodies of a turn with a
// tool call, so the effort reaches the answer after the tool too.
func TestEffortIsSentInEveryRequest(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		effort string
		want   any // nil: no reasoning_effort key
	}{
		{"high", "high"},
		{"none", "none"},
		{"", nil},
	} {
		t.Run("effort="+tc.effort, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			var bodies []map[string]any
			answers := []string{readTestdata(t, "add_call.json"), readTestdata(t, "final.json")}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				var body map[string]any
				json.Unmarshal(raw, &body)
				mu.Lock()
				defer mu.Unlock()
				if len(bodies) == len(answers) {
					http.Error(w, "unexpected request", http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, answers[len(bodies)])
				bodies = append(bodies, body)
			}))
			defer srv.Close()
			var runs int
			b, err := llama.New(llama.Config{URL: srv.URL + "/v1", Tools: []llama.Tool{addTool(&runs)}})
			if err != nil {
				t.Fatal(err)
			}
			got := runWithEffort(t, b, tc.effort)
			if got.Status != faden.Done {
				t.Fatalf("turn = %+v", got)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(bodies) != 2 {
				t.Fatalf("sent %d requests, want 2", len(bodies))
			}
			for i, body := range bodies {
				v, ok := body["reasoning_effort"]
				if tc.want == nil && ok {
					t.Fatalf("request %d has reasoning_effort %v, want none", i, v)
				}
				if tc.want != nil && v != tc.want {
					t.Fatalf("request %d reasoning_effort = %v, want %v", i, v, tc.want)
				}
			}
		})
	}
}

func readTestdata(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func runWithEffort(t *testing.T, b *llama.Backend, effort string) faden.Turn {
	t.Helper()
	r, err := faden.Open(openStore(t, t.TempDir()), map[string]faden.Backend{"llama": b})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Submit(t.Context(), faden.Submission{Session: "s1", RequestID: "r1", Input: "what is 2+3?", Backend: "llama", Model: "test-model", Effort: effort}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	got, err := r.Wait(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

package codex

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/nogo/faden"
	"github.com/nogo/faden/jsonl"
)

func TestTokenUsage(t *testing.T) {
	total, last, err := tokenUsage(json.RawMessage(`{"threadId":"invented","turnId":"turn","tokenUsage":{"total":{"inputTokens":120,"cachedInputTokens":40,"outputTokens":30,"reasoningOutputTokens":10},"last":{"inputTokens":20,"cachedInputTokens":5,"outputTokens":7,"reasoningOutputTokens":3}}}`))
	if err != nil || total != (faden.ModelUsage{InputTokens: 120, CacheReadTokens: 40, OutputTokens: 30}) || last != (faden.ModelUsage{InputTokens: 20, CacheReadTokens: 5, OutputTokens: 7}) {
		t.Fatalf("total=%+v last=%+v err=%v", total, last, err)
	}
	base := subtractUsage(total, last)
	total, last, err = tokenUsage(json.RawMessage(`{"tokenUsage":{"total":{"inputTokens":150,"cachedInputTokens":50,"outputTokens":42},"last":{"inputTokens":30,"cachedInputTokens":10,"outputTokens":12}}}`))
	if err != nil || subtractUsage(total, base) != (faden.ModelUsage{InputTokens: 50, CacheReadTokens: 15, OutputTokens: 19}) {
		t.Fatalf("usage=%+v err=%v", subtractUsage(total, base), err)
	}
}
func TestLiveUsage(t *testing.T) {
	if os.Getenv("FADEN_LIVE") != "1" {
		t.Skip("set FADEN_LIVE=1")
	}
	if _, err := exec.LookPath("codex"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	store, err := jsonl.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	backend, err := New(Config{Runtime: faden.Local{PIDDir: dir, Dir: dir}})
	if err != nil {
		t.Fatal(err)
	}
	runner, err := faden.Open(store, map[string]faden.Backend{"codex": backend})
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	_, err = runner.Submit(ctx, faden.Submission{Session: "usage", RequestID: "usage", Backend: "codex", Input: "Reply with OK."})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := runner.Wait(ctx, "usage")
	if err != nil {
		t.Fatal(err)
	}
	if turn.Status != faden.Done || len(turn.Usage) == 0 {
		t.Fatalf("turn=%+v", turn)
	}
	for _, u := range turn.Usage {
		if u.Model == "" || u.InputTokens <= 0 || u.OutputTokens <= 0 {
			t.Fatalf("usage=%+v", u)
		}
	}
	t.Logf("usage=%+v", turn.Usage)
}

func TestTurnUsageReplacesSnapshots(t *testing.T) {
	fixture := `{"method":"thread/tokenUsage/updated","params":{"tokenUsage":{"total":{"inputTokens":120,"cachedInputTokens":40,"outputTokens":30},"last":{"inputTokens":20,"cachedInputTokens":5,"outputTokens":7}}}}
{"method":"thread/tokenUsage/updated","params":{"tokenUsage":{"total":{"inputTokens":150,"cachedInputTokens":50,"outputTokens":42},"last":{"inputTokens":30,"cachedInputTokens":10,"outputTokens":12}}}}
{"method":"thread/tokenUsage/updated","params":{"tokenUsage":{"total":{"inputTokens":150,"cachedInputTokens":50,"outputTokens":42},"last":{"inputTokens":30,"cachedInputTokens":10,"outputTokens":12}}}}
{"method":"turn/completed","params":{"turn":{"status":"completed"}}}
`
	dir := setup(t, fixture)
	b, store := newBackend(t, dir)
	runner, err := faden.Open(store, map[string]faden.Backend{"codex": b})
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	_, err = runner.Submit(t.Context(), faden.Submission{Session: "s", RequestID: "usage", Backend: "codex", Model: "invented", Input: "Hello"})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := runner.Wait(t.Context(), "usage")
	if err != nil {
		t.Fatal(err)
	}
	if turn.Status != faden.Done || len(turn.Usage) != 1 || turn.Usage[0] != (faden.ModelUsage{Model: "invented", InputTokens: 50, CacheReadTokens: 15, OutputTokens: 19}) {
		t.Fatalf("turn=%+v", turn)
	}
}

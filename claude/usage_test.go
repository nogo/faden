package claude

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/nogo/faden"
	"github.com/nogo/faden/jsonl"
)

func TestResultUsage(t *testing.T) {
	for _, failed := range []bool{false, true} {
		line := `{"type":"result","modelUsage":{"invented-a":{"inputTokens":10,"outputTokens":5,"cacheReadInputTokens":20,"cacheCreationInputTokens":30,"costUSD":0.25},"invented-b":{"inputTokens":2,"outputTokens":1,"costUSD":0.125}},"total_cost_usd":0.375`
		if failed {
			line += `,"is_error":true`
		}
		line += `}`
		var got []faden.ModelUsage
		end, err := parseLine([]byte(line), &cursor{}, func(faden.Event) error { return nil }, func(u faden.ModelUsage) { got = append(got, u) }, "alias")
		if !end || (err != nil) != failed || len(got) != 2 || got[0] != (faden.ModelUsage{Model: "invented-a", InputTokens: 60, OutputTokens: 5, CacheReadTokens: 20, CacheWriteTokens: 30, CostUSD: 0.25}) || got[1].CostUSD != 0.125 {
			t.Fatalf("usage=%+v end=%v err=%v", got, end, err)
		}
	}
}

func TestResultTotalCostFallback(t *testing.T) {
	var got faden.ModelUsage
	_, err := parseLine([]byte(`{"type":"result","total_cost_usd":0.25}`), &cursor{}, func(faden.Event) error { return nil }, func(u faden.ModelUsage) { got = u }, "invented")
	if err != nil || got != (faden.ModelUsage{Model: "invented", CostUSD: 0.25}) {
		t.Fatalf("usage=%+v err=%v", got, err)
	}
}

func TestLiveUsage(t *testing.T) {
	if os.Getenv("FADEN_LIVE") != "1" {
		t.Skip("set FADEN_LIVE=1")
	}
	if _, err := exec.LookPath("claude"); err != nil {
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
	runner, err := faden.Open(store, map[string]faden.Backend{"claude": backend})
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	_, err = runner.Submit(ctx, faden.Submission{Session: "usage", RequestID: "usage", Backend: "claude", Input: "Reply with OK."})
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
		if u.Model == "" || u.InputTokens <= 0 || u.OutputTokens <= 0 || u.CostUSD <= 0 {
			t.Fatalf("usage=%+v", u)
		}
	}
	t.Logf("usage=%+v", turn.Usage)
}

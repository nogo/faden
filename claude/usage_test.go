package claude

import (
	"context"
	"errors"
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
		end, err := (&output{model: "alias", emit: func(faden.Event) error { return nil }, addUsage: func(u faden.ModelUsage) { got = append(got, u) }}).line([]byte(line))
		if !end || (err != nil) != failed || len(got) != 2 || got[0] != (faden.ModelUsage{Model: "invented-a", InputTokens: 60, OutputTokens: 5, CacheReadTokens: 20, CacheWriteTokens: 30, CostUSD: 0.25}) || got[1].CostUSD != 0.125 {
			t.Fatalf("usage=%+v end=%v err=%v", got, end, err)
		}
	}
}

func TestResultTotalCostFallback(t *testing.T) {
	var got faden.ModelUsage
	_, err := (&output{model: "invented", emit: func(faden.Event) error { return nil }, addUsage: func(u faden.ModelUsage) { got = u }}).line([]byte(`{"type":"result","total_cost_usd":0.25}`))
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

func parseAll(t *testing.T, lines ...string) error {
	t.Helper()
	o := &output{emit: func(faden.Event) error { return nil }, addUsage: func(faden.ModelUsage) {}}
	var err error
	for _, line := range lines {
		if _, err = o.line([]byte(line)); err != nil {
			break
		}
	}
	return err
}

func TestFailureKinds(t *testing.T) {
	var quota *faden.QuotaError
	err := parseAll(t, `{"type":"result","is_error":true,"error":"rate_limit","result":"limit","quotaLimits":{"status":"rejected","resetsAt":1800000000,"rateLimitType":"five_hour"}}`)
	if !errors.As(err, &quota) || !quota.ResetsAt.Equal(time.Unix(1800000000, 0)) || quota.LimitType != "five_hour" {
		t.Fatalf("rejected limits: %v", err)
	}
	// stream-json puts the error on the assistant line before the result.
	err = parseAll(t,
		`{"type":"assistant","error":"rate_limit","message":{"id":"m1","content":[{"type":"text","text":"limit"}]}}`,
		`{"type":"result","is_error":true,"result":"limit"}`)
	if !errors.As(err, &quota) || !quota.ResetsAt.IsZero() {
		t.Fatalf("rate_limit without reset: %v", err)
	}
	err = parseAll(t, `{"type":"result","is_error":true,"quotaLimits":{"status":"allowed_warning"},"result":"x"}`)
	if errors.As(err, &quota) {
		t.Fatalf("a warning is not a quota stop: %v", err)
	}
	var unavailable *faden.UnavailableError
	err = parseAll(t,
		`{"type":"assistant","error":"authentication_failed","message":{"id":"m1","content":[{"type":"text","text":"Not logged in · Please run /login"}]}}`,
		`{"type":"result","is_error":true,"result":"Not logged in · Please run /login"}`)
	if !errors.As(err, &unavailable) {
		t.Fatalf("not logged in: %v", err)
	}
	err = parseAll(t, `{"type":"result","is_error":true,"result":"Not logged in · Please run /login"}`)
	if !errors.As(err, &unavailable) {
		t.Fatalf("not logged in, result only: %v", err)
	}
	err = parseAll(t, `{"type":"result","is_error":true,"result":"boom"}`)
	if errors.As(err, &quota) || errors.As(err, &unavailable) || err == nil {
		t.Fatalf("plain failure: %v", err)
	}
}

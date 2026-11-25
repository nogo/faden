package jsonl_test

import (
	"context"
	"testing"

	"github.com/nogo/faden"
	"github.com/nogo/faden/jsonl"
)

// An append reads its own line back to stay in step with other processes.
// That read must cost about the line, not a fixed 64 KB buffer.
func TestAppendAllocatesAboutItsLine(t *testing.T) {
	ctx := context.Background()
	s, err := jsonl.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.SubmitTurn(ctx, faden.Turn{RequestID: "r", Session: "s", Backend: "b"}); err != nil {
		t.Fatal(err)
	}
	e := faden.Event{Session: "s", RequestID: "r", Kind: faden.AssistantMessage, Text: "short"}
	res := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			if _, err := s.AppendEvent(ctx, e); err != nil {
				b.Fatal(err)
			}
		}
	})
	if got := res.AllocedBytesPerOp(); got > 16<<10 {
		t.Fatalf("one short append allocates %d bytes", got)
	}
	t.Logf("short append: %d B/op", res.AllocedBytesPerOp())
}

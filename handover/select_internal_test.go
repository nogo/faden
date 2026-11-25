package handover

import (
	"errors"
	"iter"
	"testing"

	"github.com/nogo/faden"
)

func TestSelectReportsAFailedRead(t *testing.T) {
	broken := errors.New("disk gone")
	reads := 0
	events := func(yield func(faden.Event, error) bool) {
		reads++
		if !yield(faden.Event{Seq: 1, Kind: faden.UserMessage, Text: "hi"}, nil) {
			return
		}
		if reads == 2 {
			yield(faden.Event{}, broken)
		}
	}
	if _, err := selectFrom(events, "", 0); !errors.Is(err, broken) {
		t.Fatalf("err = %v, want the read error", err)
	}
}

func TestSelectFailsWhenTheEventsChangeBetweenReads(t *testing.T) {
	n := 1
	var events iter.Seq2[faden.Event, error] = func(yield func(faden.Event, error) bool) {
		for i := range n {
			if !yield(faden.Event{Seq: int64(i + 1), Kind: faden.AssistantMessage, Text: "a"}, nil) {
				return
			}
		}
		n++
	}
	if _, err := selectFrom(events, "", 0); err == nil {
		t.Fatal("a second read with another event was accepted")
	}
}

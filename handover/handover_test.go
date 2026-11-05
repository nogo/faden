package handover_test

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/nogo/faden"
	"github.com/nogo/faden/handover"
)

func TestSelection(t *testing.T) {
	events := []faden.Event{
		{Seq: 1, Kind: faden.UserMessage, Text: "first"},
		{Seq: 2, Kind: faden.AssistantMessage, Text: strings.Repeat("a", 500)},
		{Seq: 3, Kind: faden.ToolResult, Output: strings.Repeat("界", 300)},
		{Seq: 4, Kind: faden.AssistantMessage, Text: "newest"},
	}
	items := handover.Select(events, "request", 800)
	if len(items) != 4 || items[0].Text != "first" || !strings.Contains(items[1].Text, "2-2") || !strings.Contains(items[1].Text, "read_thread") || items[3].Text != "newest" {
		t.Fatalf("%+v", items)
	}
	if !utf8.ValidString(items[2].Text) || len([]rune(items[2].Text)) != 240 || !strings.HasSuffix(items[2].Text, "...") {
		t.Fatalf("%+v", items[2])
	}
	items = handover.Select(events, strings.Repeat("x", 17000), 0)
	if len(items) != 2 || items[0].Text != "first" || !strings.Contains(items[1].Text, "2-4") {
		t.Fatalf("%+v", items)
	}
	items = handover.Select(events[:2], "", 0)
	if items[1].Text != events[1].Text {
		t.Fatal("assistant text shortened")
	}
}

func TestLongToolOutputIsCutWithoutCopyingIt(t *testing.T) {
	events := []faden.Event{{Seq: 1, Kind: faden.ToolResult, Tool: "bash", Output: strings.Repeat("界", 100_000)}}
	items := handover.Select(events, "", 0)
	got := items[0].Text
	if !utf8.ValidString(got) || utf8.RuneCountInString(got) != 240 || !strings.HasPrefix(got, "tool_result bash    界") || !strings.HasSuffix(got, "界...") {
		t.Fatalf("item = %q", got)
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	handover.Select(events, "", 0)
	runtime.ReadMemStats(&after)
	if n := after.TotalAlloc - before.TotalAlloc; n > 16<<10 {
		t.Fatalf("Select of one 300 KB output allocated %d bytes", n)
	}
}

func TestFailedToolResultSaysSo(t *testing.T) {
	items := handover.Select([]faden.Event{{Seq: 1, Kind: faden.ToolResult, Tool: "bash", Output: "exit 3", Failed: true}}, "", 0)
	if len(items) != 1 || !strings.HasPrefix(items[0].Text, "tool_result failed bash") {
		t.Fatalf("%+v", items)
	}
}

// countingEvents serves n invented events and counts how many were read.
type countingEvents struct {
	n    int64
	read int
}

func (c *countingEvents) NextEvent(_ context.Context, _ string, after int64) (*faden.Event, error) {
	if after >= c.n {
		return nil, nil
	}
	c.read++
	return &faden.Event{Seq: after + 1, Kind: faden.ToolResult, Output: "out"}, nil
}

func TestReadThreadReadsOnlyWhatItAnswers(t *testing.T) {
	events := &countingEvents{n: 10000}
	got, err := handover.ReadThread(context.Background(), events, "s", json.RawMessage(`{"from":1,"to":1000000}`))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(got, "\n")
	if len(lines) != 101 || lines[100] != "[cut: ask again from 101]" {
		t.Fatalf("got %d lines, last %q", len(lines), lines[len(lines)-1])
	}
	if events.read != 101 {
		t.Fatalf("read %d events of 10000, want 101", events.read)
	}
}

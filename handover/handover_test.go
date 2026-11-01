package handover_test

import (
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

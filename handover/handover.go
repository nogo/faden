// Package handover selects thread items for a backend that has not seen them.
package handover

import (
	"fmt"
	"strings"

	"github.com/nogo/faden"
)

// DefaultBudget is the catch-up budget in bytes.
const DefaultBudget = 16000

// Item is an attributed message or a reference to omitted events.
type Item struct {
	Role string
	Text string
}

// Select keeps the first user message, then the newest items that fit.
// The request consumes budget but is sent separately and is never shortened.
func Select(events []faden.Event, request string, budget int) []Item {
	if budget <= 0 {
		budget = DefaultBudget
	}
	remaining := budget - len(request)
	chosen := make([]bool, len(events))
	texts := make([]string, len(events))
	first := -1
	for i, e := range events {
		texts[i] = text(e)
		if first < 0 && e.Kind == faden.UserMessage {
			first = i
		}
	}
	if first >= 0 {
		chosen[first] = true
		remaining -= len(texts[first])
	}
	for i := len(events) - 1; i >= 0; i-- {
		if i == first {
			continue
		}
		if len(texts[i]) > remaining {
			break
		}
		chosen[i] = true
		remaining -= len(texts[i])
	}
	var items []Item
	for i := 0; i < len(events); {
		if !chosen[i] {
			start := events[i].Seq
			end := start
			for i < len(events) && !chosen[i] {
				end = events[i].Seq
				i++
			}
			items = append(items, Item{"user", fmt.Sprintf("Omitted events %d-%d; use read_thread to read them.", start, end)})
			continue
		}
		role := "user"
		if events[i].Kind == faden.AssistantMessage {
			role = "assistant"
		}
		items = append(items, Item{role, texts[i]})
		i++
	}
	return items
}

func text(e faden.Event) string {
	if e.Kind == faden.UserMessage || e.Kind == faden.AssistantMessage {
		return e.Text
	}
	s := strings.TrimSpace(string(e.Kind) + " " + e.Tool + " " + e.Text + " " + e.Action + " " + string(e.Input) + " " + e.Output)
	r := []rune(s)
	if len(r) > 240 {
		s = string(r[:237]) + "..."
	}
	return s
}

// Block renders one attributed text block for a CLI request.
func Block(items []Item) string {
	var b strings.Builder
	for _, item := range items {
		fmt.Fprintf(&b, "[%s]\n%s\n", item.Role, item.Text)
	}
	return b.String()
}

// Package handover selects thread items for a backend that has not seen them,
// and serves read_thread, the tool that fetches the items a selection left out.
package handover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/nogo/faden"
)

// DefaultBudget is the catch-up budget in bytes.
const DefaultBudget = 16000

// maxItem is the length in runes of an item that is not a user or assistant message.
const maxItem = 240

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
	// Only chosen events get a text: rendering every event of a long thread
	// would copy every tool output.
	chosen := make([]bool, len(events))
	texts := make([]string, len(events))
	first := slices.IndexFunc(events, func(e faden.Event) bool { return e.Kind == faden.UserMessage })
	if first >= 0 {
		chosen[first], texts[first] = true, text(events[first])
		remaining -= len(texts[first])
	}
	for i := len(events) - 1; i >= 0; i-- {
		if i == first {
			continue
		}
		s := text(events[i])
		if len(s) > remaining {
			break
		}
		chosen[i], texts[i] = true, s
		remaining -= len(s)
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
			items = append(items, Item{"user", fmt.Sprintf("Omitted events %d-%d; use %s to read them.", start, end, ThreadTool)})
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

// text is an event's catch-up text. Items other than user and assistant
// messages are shortened to maxItem runes; only the bytes that can survive
// the cut are copied, because a tool output can be megabytes.
func text(e faden.Event) string {
	if e.Kind == faden.UserMessage || e.Kind == faden.AssistantMessage {
		return e.Text
	}
	const limit = (maxItem + 1) * utf8.UTFMax
	var b strings.Builder
	cut := false
	kind := string(e.Kind)
	if e.Failed {
		kind += " failed"
	}
	for i, part := range [...]string{kind, e.Tool, e.Text, e.Action, string(e.Input), e.Output} {
		if i > 0 {
			b.WriteByte(' ')
		}
		if room := limit - b.Len(); len(part) > room {
			for room > 0 && !utf8.RuneStart(part[room]) {
				room--
			}
			b.WriteString(part[:room])
			cut = true
			break
		}
		b.WriteString(part)
	}
	s := strings.TrimSpace(b.String())
	if cut || utf8.RuneCountInString(s) > maxItem {
		s = clip(s, maxItem-3) + "..."
	}
	return s
}

// clip returns the first n runes of s.
func clip(s string, n int) string {
	for i := range s {
		if n == 0 {
			return s[:i]
		}
		n--
	}
	return s
}

// Prompt is what a CLI backend sends for t: its input, preceded by one
// attributed text block of a's catch-up when it has one.
func Prompt(t faden.Turn, a *faden.Attempt) string {
	catchUp, budget := a.CatchUp()
	if len(catchUp) == 0 {
		return t.Input
	}
	var b strings.Builder
	for _, item := range Select(catchUp, t.Input, budget) {
		fmt.Fprintf(&b, "[%s]\n%s\n", item.Role, item.Text)
	}
	b.WriteString("\n")
	b.WriteString(t.Input)
	return b.String()
}

// ThreadTool is the name of the read-only tool that returns a session's
// events by sequence range. ThreadToolDescription and ThreadToolSchema
// describe it to a model.
const (
	ThreadTool            = "read_thread"
	ThreadToolDescription = "Read events of this conversation by sequence number, from first to last inclusive."
	ThreadToolSchema      = `{"type":"object","properties":{"from":{"type":"integer"},"to":{"type":"integer"}},"required":["from","to"]}`
)

// maxThreadItems bounds one ReadThread answer.
const maxThreadItems = 100

// Events reads a session's events after a sequence number. A faden.Store
// implements it.
type Events interface {
	Events(ctx context.Context, session string, after int64) ([]faden.Event, error)
}

// ReadThread runs ThreadTool for session own. args holds from, to and an
// optional session, which must be own. The answer has one JSON event per
// line, without native payloads, and at most 100 events; a last line says
// where to continue.
func ReadThread(ctx context.Context, events Events, own string, args json.RawMessage) (string, error) {
	var a struct {
		Session string `json:"session"`
		From    int64  `json:"from"`
		To      int64  `json:"to"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("bad arguments: %w", err)
	}
	if a.Session != "" && a.Session != own {
		return "", errors.New("only this conversation can be read")
	}
	a.From = max(a.From, 1)
	list, err := events.Events(ctx, own, a.From-1)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	enc := json.NewEncoder(&b)
	for n, e := range list {
		if e.Seq > a.To {
			break
		}
		if n == maxThreadItems {
			fmt.Fprintf(&b, "[cut: ask again from %d]\n", e.Seq)
			break
		}
		e.Native, e.Session, e.RequestID = nil, "", ""
		if err := enc.Encode(e); err != nil {
			return "", err
		}
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

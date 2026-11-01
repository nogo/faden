// Package llama is faden's own agent loop against an OpenAI-compatible
// /v1/chat/completions endpoint such as llama-server. Reach for it to run a
// local model with tools whose every step survives a crash.
//
// Each model answer is stored as an event. A tool call is stored before it
// runs and its result after. The messages sent to the model are rebuilt from
// the session's events, so the loop continues without a prompt. A stored call
// without a result runs again only if its tool is Safe; otherwise its result
// says it was interrupted.
package llama

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/nogo/faden"
)

// maxSteps bounds the model calls of one Run.
const maxSteps = 40

// Store is what the backend reads of a session: its events.
type Store interface {
	Events(ctx context.Context, session string, after int64) ([]faden.Event, error)
}

// Config wires a Backend.
type Config struct {
	// URL is the server's base address, such as http://localhost:8080.
	URL string
	// Model is used when a turn names none.
	Model string
	// Store holds the events the messages are rebuilt from.
	Store Store
	// Runtime runs the bash tool. Nil leaves bash out.
	Runtime faden.Runtime
	// Tools are the host's Go functions.
	Tools []Tool
	// Client is the HTTP client; nil means http.DefaultClient.
	Client *http.Client
}

// Backend implements faden.Backend.
type Backend struct {
	cfg Config
}

// New returns a backend. It fails if a host tool reuses a built-in name.
func New(cfg Config) (*Backend, error) {
	if cfg.Client == nil {
		cfg.Client = http.DefaultClient
	}
	for _, t := range cfg.Tools {
		if t.Name == "" || t.Run == nil {
			return nil, errors.New("llama: tool needs a name and a Run function")
		}
		if t.Name == bashName || t.Name == readThreadName {
			return nil, fmt.Errorf("llama: tool name %q is built in", t.Name)
		}
	}
	return &Backend{cfg: cfg}, nil
}

// ContinuesWithoutPrompt reports true: faden owns the transcript.
func (b *Backend) ContinuesWithoutPrompt() bool { return true }

// message is one chat message in the OpenAI format.
type message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// transcript is the session as chat messages, plus which calls were stored.
type transcript struct {
	messages []message
	called   map[string]bool // call ids with a stored tool_call event
	answered map[string]bool // call ids with a stored tool_result event
}

func rebuild(events []faden.Event) (*transcript, error) {
	tr := &transcript{called: map[string]bool{}, answered: map[string]bool{}}
	for _, e := range events {
		switch e.Kind {
		case faden.UserMessage:
			tr.messages = append(tr.messages, message{Role: "user", Content: e.Text})
		case faden.AssistantMessage:
			m := message{Role: "assistant", Content: e.Text}
			if len(e.Native) > 0 {
				if err := json.Unmarshal(e.Native, &m); err != nil {
					return nil, fmt.Errorf("llama: event %d: %w", e.Seq, err)
				}
			}
			tr.messages = append(tr.messages, m)
		case faden.ToolCall:
			tr.called[callID(e)] = true
		case faden.ToolResult:
			id := callID(e)
			tr.answered[id] = true
			tr.messages = append(tr.messages, message{Role: "tool", ToolCallID: id, Content: e.Output})
		}
	}
	return tr, nil
}

func callID(e faden.Event) string {
	var n struct {
		ID string `json:"id"`
	}
	json.Unmarshal(e.Native, &n)
	return n.ID
}

// pending returns the calls of the last assistant message that have no result.
func (tr *transcript) pending() []toolCall {
	for i := len(tr.messages) - 1; i >= 0; i-- {
		if tr.messages[i].Role == "tool" {
			continue
		}
		var out []toolCall
		for _, c := range tr.messages[i].ToolCalls {
			if !tr.answered[c.ID] {
				out = append(out, c)
			}
		}
		return out
	}
	return nil
}

// needsModel reports whether the model must answer next: the transcript does
// not end with a final assistant message.
func (tr *transcript) needsModel() bool {
	for i := len(tr.messages) - 1; i >= 0; i-- {
		m := tr.messages[i]
		if m.Role == "tool" {
			continue
		}
		return !(m.Role == "assistant" && len(m.ToolCalls) == 0)
	}
	return false
}

// Run implements faden.Backend. It returns no cursor: the events are the
// transcript.
func (b *Backend) Run(ctx context.Context, t faden.Turn, _ json.RawMessage, emit func(faden.Event) error) (json.RawMessage, error) {
	events, err := b.cfg.Store.Events(ctx, t.Session, 0)
	if err != nil {
		return nil, fmt.Errorf("llama: read session %s: %w", t.Session, err)
	}
	if t.Input != "" {
		e := faden.Event{Kind: faden.UserMessage, Text: t.Input}
		if err := emit(e); err != nil {
			return nil, fmt.Errorf("llama: store input: %w", err)
		}
		events = append(events, e)
	} else if !hasInput(events, t.RequestID) {
		return nil, errors.New("llama: the turn's input was never stored; send a new turn")
	}
	tr, err := rebuild(events)
	if err != nil {
		return nil, err
	}
	tools := b.toolset(t.Session)
	model := t.Model
	if model == "" {
		model = b.cfg.Model
	}
	for step := 0; ; {
		if calls := tr.pending(); len(calls) > 0 {
			for _, c := range calls {
				if err := b.execute(ctx, tools, tr, c, emit); err != nil {
					return nil, err
				}
			}
			continue
		}
		if !tr.needsModel() {
			return nil, nil
		}
		if step++; step > maxSteps {
			return nil, fmt.Errorf("llama: no final answer after %d model calls", maxSteps)
		}
		answer, err := b.complete(ctx, model, tools, tr.messages)
		if err != nil {
			return nil, err
		}
		for i := range answer.ToolCalls {
			c := &answer.ToolCalls[i]
			if c.ID == "" {
				c.ID = fmt.Sprintf("call_%d_%d", len(tr.messages), i)
			}
			c.Type = "function"
		}
		native, err := json.Marshal(answer)
		if err != nil {
			return nil, fmt.Errorf("llama: encode answer: %w", err)
		}
		if err := emit(faden.Event{Kind: faden.AssistantMessage, Text: answer.Content, Native: native}); err != nil {
			return nil, fmt.Errorf("llama: store answer: %w", err)
		}
		tr.messages = append(tr.messages, answer)
	}
}

func hasInput(events []faden.Event, requestID string) bool {
	for _, e := range events {
		if e.Kind == faden.UserMessage && e.RequestID == requestID {
			return true
		}
	}
	return false
}

// execute stores a call, runs it unless it was interrupted, and stores the result.
func (b *Backend) execute(ctx context.Context, tools map[string]Tool, tr *transcript, c toolCall, emit func(faden.Event) error) error {
	tool, known := tools[c.Function.Name]
	idJSON, _ := json.Marshal(map[string]string{"id": c.ID})
	args := json.RawMessage(c.Function.Arguments)
	if !json.Valid(args) {
		args, _ = json.Marshal(c.Function.Arguments)
	}
	interrupted := tr.called[c.ID] && !(known && tool.Safe)
	if !tr.called[c.ID] {
		call := faden.Event{Kind: faden.ToolCall, Tool: c.Function.Name, Input: args, Native: idJSON}
		if err := emit(call); err != nil {
			return fmt.Errorf("llama: store call %s: %w", c.ID, err)
		}
		tr.called[c.ID] = true
	}
	var out string
	switch {
	case interrupted:
		out = "interrupted: the host stopped while this call ran, and it was not run again"
	case !known:
		out = fmt.Sprintf("error: unknown tool %q", c.Function.Name)
	default:
		var err error
		out, err = tool.Run(ctx, args)
		if ctx.Err() != nil {
			return ctx.Err() // no result is stored: the next Open sees the call as interrupted
		}
		if err != nil {
			out = "error: " + err.Error()
		}
	}
	if err := emit(faden.Event{Kind: faden.ToolResult, Tool: c.Function.Name, Output: out, Native: idJSON}); err != nil {
		return fmt.Errorf("llama: store result %s: %w", c.ID, err)
	}
	tr.answered[c.ID] = true
	tr.messages = append(tr.messages, message{Role: "tool", ToolCallID: c.ID, Content: out})
	return nil
}

// complete asks the server for the next assistant message.
func (b *Backend) complete(ctx context.Context, model string, tools map[string]Tool, msgs []message) (message, error) {
	type function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	}
	type toolDef struct {
		Type     string   `json:"type"`
		Function function `json:"function"`
	}
	var defs []toolDef
	for _, t := range sortedTools(tools) {
		defs = append(defs, toolDef{"function", function{t.Name, t.Description, t.schema()}})
	}
	body, err := json.Marshal(struct {
		Model    string    `json:"model"`
		Messages []message `json:"messages"`
		Tools    []toolDef `json:"tools,omitempty"`
	}{model, msgs, defs})
	if err != nil {
		return message{}, fmt.Errorf("llama: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(b.cfg.URL, "/")+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return message{}, fmt.Errorf("llama: request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.cfg.Client.Do(req)
	if err != nil {
		return message{}, fmt.Errorf("llama: call model: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return message{}, fmt.Errorf("llama: read answer: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return message{}, fmt.Errorf("llama: model answered %s: %.200s", resp.Status, raw)
	}
	var parsed struct {
		Choices []struct {
			Message message `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || len(parsed.Choices) == 0 {
		return message{}, fmt.Errorf("llama: unreadable answer: %.200s", raw)
	}
	m := parsed.Choices[0].Message
	m.Role = "assistant"
	return m, nil
}

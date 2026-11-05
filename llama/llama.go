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
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nogo/faden"
	"github.com/nogo/faden/handover"
)

const (
	defaultMaxOutput = 12000
	defaultMaxSteps  = 40
)

// Config wires a Backend.
type Config struct {
	// URL includes the API version, such as http://localhost:8080/v1.
	URL string
	// Key is an optional bearer token supplied by the host. It is never stored.
	Key string
	// Model is used when a turn names none.
	Model string
	// Runtime runs the bash tool. Nil leaves bash out.
	Runtime faden.Runtime
	// FileTools adds read, write and edit, which run through Runtime in the
	// attempt's Dir. It needs Runtime. A host tool may not reuse their names.
	FileTools bool
	// Tools are the host's Go functions.
	Tools []Tool
	// Client is the HTTP client; nil means http.DefaultClient.
	Client *http.Client
	// MaxOutput is the most bytes of one tool result the model sees and the
	// store keeps. Zero means 12,000. A longer result is cut at a rune boundary
	// and ends with a marker.
	MaxOutput int
	// MaxTokens is sent as max_tokens when not zero.
	MaxTokens int
	// MaxSteps bounds the model calls of one Run. Zero means 40.
	MaxSteps int
}

// Backend implements faden.Backend.
type Backend struct {
	cfg Config
	// wait pauses before a model call is retried. Tests replace it.
	wait func(ctx context.Context, d time.Duration) error
}

// New returns a backend. It fails if a host tool reuses a built-in name.
func New(cfg Config) (*Backend, error) {
	if cfg.Client == nil {
		cfg.Client = http.DefaultClient
	}
	if cfg.MaxOutput <= 0 {
		cfg.MaxOutput = defaultMaxOutput
	}
	if cfg.MaxSteps <= 0 {
		cfg.MaxSteps = defaultMaxSteps
	}
	if cfg.FileTools && cfg.Runtime == nil {
		return nil, errors.New("llama: FileTools needs a Runtime")
	}
	for _, t := range cfg.Tools {
		if t.Name == "" || t.Run == nil {
			return nil, errors.New("llama: tool needs a name and a Run function")
		}
		if t.Name == bashName || t.Name == handover.ThreadTool {
			return nil, fmt.Errorf("llama: tool name %q is built in", t.Name)
		}
		if cfg.FileTools && slices.Contains(fileToolNames, t.Name) {
			return nil, fmt.Errorf("llama: tool name %q is a file tool", t.Name)
		}
	}
	return &Backend{cfg: cfg, wait: sleep}, nil
}

// ContinuesWithoutPrompt reports true: faden owns the transcript.
func (b *Backend) ContinuesWithoutPrompt() bool { return true }

// Stop stops the bash command an earlier attempt of t left running.
func (b *Backend) Stop(ctx context.Context, t faden.Turn) error {
	if b.cfg.Runtime == nil {
		return nil
	}
	return b.cfg.Runtime.Stop(ctx, t.RequestID)
}

// message is one chat message in the OpenAI format.
type message struct {
	Role       string     `json:"role"`
	Reasoning  string     `json:"reasoning_content,omitempty"`
	Content    string     `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// completion carries response metadata without putting it in the transcript.
type completion struct {
	message message
	usage   faden.ModelUsage
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// transcript is the session as chat messages, plus which calls of the last
// assistant message were stored. Models reuse ids such as call_0 across
// messages, so the maps cover one assistant message only.
type transcript struct {
	messages []message
	called   map[string]bool // call ids with a stored tool_call event
	answered map[string]bool // call ids with a stored tool_result event
}

// startMessage forgets the call state of the previous assistant message.
func (tr *transcript) startMessage() {
	tr.called = map[string]bool{}
	tr.answered = map[string]bool{}
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
			tr.startMessage()
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

// rebuildFor replays native llama messages and portable context at each
// handover. name is this backend's key in the runner.
func rebuildFor(name string, events []faden.Event) (*transcript, error) {
	var native []faden.Event
	// Before its first handover, the session belongs to that handover's From.
	active := true
	for _, e := range events {
		if e.Kind == faden.Handover {
			boundary, err := faden.HandoverOf(e)
			if err != nil {
				return nil, fmt.Errorf("llama: %w", err)
			}
			active = boundary.From == name
			break
		}
	}
	for i, e := range events {
		if e.Kind == faden.Handover {
			boundary, err := faden.HandoverOf(e)
			if err != nil {
				return nil, fmt.Errorf("llama: %w", err)
			}
			active = boundary.To == name
			if !active {
				continue
			}
			var unseen []faden.Event
			for _, prior := range events[:i] {
				if prior.Seq > boundary.After {
					unseen = append(unseen, prior)
				}
			}
			request := ""
			for _, next := range events[i+1:] {
				if next.RequestID == e.RequestID && next.Kind == faden.UserMessage {
					request = next.Text
					break
				}
			}
			for _, item := range handover.Select(unseen, request, boundary.Budget) {
				kind := faden.UserMessage
				if item.Role == "assistant" {
					kind = faden.AssistantMessage
				}
				native = append(native, faden.Event{Kind: kind, Text: item.Text})
			}
			continue
		}
		if active {
			native = append(native, e)
		}
	}
	return rebuild(native)
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
	for _, m := range slices.Backward(tr.messages) {
		if m.Role == "tool" {
			continue
		}
		var out []toolCall
		for _, c := range m.ToolCalls {
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
	for _, m := range slices.Backward(tr.messages) {
		if m.Role == "tool" {
			continue
		}
		return !(m.Role == "assistant" && len(m.ToolCalls) == 0)
	}
	return false
}

// Run implements faden.Backend. It saves no cursor: the events are the
// transcript.
func (b *Backend) Run(ctx context.Context, t faden.Turn, a *faden.Attempt) error {
	events, err := faden.CollectEvents(a.Events(ctx, 0))
	if err != nil {
		return fmt.Errorf("llama: read session %s: %w", t.Session, err)
	}
	tr, err := rebuildFor(t.Backend, events)
	if err != nil {
		return err
	}
	if t.Context != "" {
		for i := len(tr.messages) - 1; i >= 0; i-- {
			if tr.messages[i].Role == "user" {
				tr.messages[i].Content = t.Context + "\n\n" + tr.messages[i].Content
				break
			}
		}
	}
	emit := func(e faden.Event) error { return a.Emit(ctx, e) }
	tools := b.toolset(a, t.Session, t.RequestID)
	model := t.Model
	if model == "" {
		model = b.cfg.Model
	}
	for step := 0; ; {
		if calls := tr.pending(); len(calls) > 0 {
			for _, c := range calls {
				if err := b.execute(ctx, tools, tr, c, emit); err != nil {
					return err
				}
			}
			continue
		}
		if !tr.needsModel() {
			return nil
		}
		if step++; step > b.cfg.MaxSteps {
			return fmt.Errorf("llama: no final answer after %d model calls", b.cfg.MaxSteps)
		}
		response, err := b.complete(ctx, model, tools, system(a.Setup().System, tr.messages))
		if err != nil {
			return err
		}
		if response.usage.Model == "" {
			response.usage.Model = model
		}
		a.AddUsage(response.usage)
		answer := response.message
		for i := range answer.ToolCalls {
			c := &answer.ToolCalls[i]
			if c.ID == "" {
				c.ID = fmt.Sprintf("call_%d_%d", len(tr.messages), i)
			}
			c.Type = "function"
		}
		native, err := json.Marshal(answer)
		if err != nil {
			return fmt.Errorf("llama: encode answer: %w", err)
		}
		if err := emit(faden.Event{Kind: faden.AssistantMessage, Text: answer.Content, Native: native}); err != nil {
			return fmt.Errorf("llama: store answer: %w", err)
		}
		tr.startMessage()
		tr.messages = append(tr.messages, answer)
	}
}

// system puts the host's system prompt first. The prompt is not part of the
// transcript, so it is never stored as an event.
func system(prompt string, msgs []message) []message {
	if prompt == "" {
		return msgs
	}
	return append([]message{{Role: "system", Content: prompt}}, msgs...)
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
	failed := true
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
		failed = err != nil
		if failed {
			out = "error: " + err.Error()
		}
		out = cutOutput(out, b.cfg.MaxOutput)
	}
	if err := emit(faden.Event{Kind: faden.ToolResult, Tool: c.Function.Name, Output: out, Failed: failed, Native: idJSON}); err != nil {
		return fmt.Errorf("llama: store result %s: %w", c.ID, err)
	}
	tr.answered[c.ID] = true
	tr.messages = append(tr.messages, message{Role: "tool", ToolCallID: c.ID, Content: out})
	return nil
}

// complete asks the server for the next assistant message.
func (b *Backend) complete(ctx context.Context, model string, tools map[string]Tool, msgs []message) (answer completion, callErr error) {
	defer func() {
		if callErr != nil && b.cfg.Key != "" && strings.Contains(callErr.Error(), b.cfg.Key) {
			callErr = errors.New(strings.ReplaceAll(callErr.Error(), b.cfg.Key, "[redacted]"))
		}
	}()
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
		Model     string    `json:"model"`
		Messages  []message `json:"messages"`
		Tools     []toolDef `json:"tools,omitempty"`
		MaxTokens int       `json:"max_tokens,omitempty"`
	}{model, msgs, defs, b.cfg.MaxTokens})
	if err != nil {
		return completion{}, fmt.Errorf("llama: encode request: %w", err)
	}
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return completion{}, err
		}
		m, retry, delay, err := b.call(ctx, body)
		if err == nil || !retry || attempt == 3 {
			return m, finalError(err, delay)
		}
		if err := b.wait(ctx, max(delay, time.Second<<attempt)); err != nil {
			return completion{}, err
		}
	}
}

// sleep waits d, or until ctx ends.
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// call makes one request. Failed requests never reach the event store.
func (b *Backend) call(ctx context.Context, body []byte) (completion, bool, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(b.cfg.URL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return completion{}, false, 0, fmt.Errorf("llama: request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if b.cfg.Key != "" {
		req.Header.Set("Authorization", "Bearer "+b.cfg.Key)
	}
	resp, err := b.cfg.Client.Do(req)
	if err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) {
			return completion{}, true, 0, fmt.Errorf("llama: call model: %w", &faden.UnavailableError{Reason: "connection refused"})
		}
		return completion{}, true, 0, fmt.Errorf("llama: call model: %w", err)
	}
	defer resp.Body.Close()
	retry := resp.StatusCode == 429 || resp.StatusCode >= 500 && resp.StatusCode <= 599
	delay := retryAfter(resp.Header.Get("Retry-After"), time.Now())
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return completion{}, retry || resp.StatusCode == http.StatusOK, delay, fmt.Errorf("llama: read answer: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return completion{}, retry, delay, &httpError{status: resp.StatusCode}
	}
	var parsed struct {
		Usage struct {
			Prompt     int64 `json:"prompt_tokens"`
			Completion int64 `json:"completion_tokens"`
		} `json:"usage"`
		Model   string `json:"model"`
		Choices []struct {
			Message message `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || len(parsed.Choices) == 0 {
		return completion{}, false, 0, errors.New("llama: unreadable answer")
	}
	m := parsed.Choices[0].Message
	u := faden.ModelUsage{Model: parsed.Model, InputTokens: parsed.Usage.Prompt, OutputTokens: parsed.Usage.Completion}
	m.Role = "assistant"
	return completion{message: m, usage: u}, false, 0, nil
}

// httpError is a model answer that was not 200. Its text omits the body, which can echo credentials.
type httpError struct{ status int }

func (e *httpError) Error() string { return fmt.Sprintf("llama: model answered %d", e.status) }

// finalError names a failure that survived the retries. A 429 is a quota stop,
// with the reset taken from Retry-After when the endpoint sent it.
func finalError(err error, delay time.Duration) error {
	var h *httpError
	if errors.As(err, &h) && h.status == http.StatusTooManyRequests {
		q := &faden.QuotaError{}
		if delay > 0 {
			q.ResetsAt = time.Now().Add(delay).UTC()
		}
		return fmt.Errorf("%w: %w", err, q)
	}
	return err
}

func retryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 {
		// Cap before converting to a duration to avoid overflow.
		return time.Duration(min(seconds, int64((1<<63-1)/time.Second))) * time.Second
	}
	if date, err := http.ParseTime(value); err == nil {
		return max(0, date.Sub(now))
	}
	return 0
}

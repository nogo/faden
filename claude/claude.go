// Package claude runs durable Claude Code turns through a host-supplied runtime.
// The host supplies permissions, environment, and persistent Claude session files.
// Events carry normalized fields only: Claude keeps its own session, and a
// killed turn continues from these fields.
package claude

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nogo/faden"
	"github.com/nogo/faden/handover"
	"github.com/nogo/faden/internal/attach"
	"github.com/nogo/faden/mcp"
)

// Runtime runs keyed commands and stops leftovers before continuation.
type Runtime = faden.Runtime

// Threads grants a session access to read_thread. *mcp.Server implements it.
type Threads interface {
	Grant(session string) (token string, err error)
	Revoke(session string)
	Config() []byte
}

// maxStderr bounds the stderr a run keeps in Result.Output.
const maxStderr = 64 << 10

// Config wires the backend to the host's runtime.
type Config struct {
	Runtime Runtime
	// Args contains host-supplied permission flags, never credentials.
	Args []string
	// Threads, if set, lets Claude read the session through read_thread. It
	// passes its token through Command.Env. The host must make the config
	// file and thread server reachable from the command.
	Threads Threads
}

// Backend implements faden.Backend for Claude Code.
type Backend struct {
	cfg    Config
	mu     sync.Mutex // guards inputs
	inputs map[string]*input
}

// New validates the host's collaborators.
func New(cfg Config) (*Backend, error) {
	if cfg.Runtime == nil {
		return nil, errors.New("claude: runtime is required")
	}
	cfg.Args = append([]string(nil), cfg.Args...)
	return &Backend{cfg: cfg, inputs: make(map[string]*input)}, nil
}

// ContinuesWithoutPrompt reports false: Claude needs a continuation message.
func (*Backend) ContinuesWithoutPrompt() bool { return false }

// Stop stops the Claude process an earlier attempt of t left running.
func (b *Backend) Stop(ctx context.Context, t faden.Turn) error {
	return b.cfg.Runtime.Stop(ctx, t.RequestID)
}

type cursor struct {
	SessionID       string `json:"session_id"`
	LastAssistantID string `json:"last_assistant_id,omitempty"`
}

// output reads what Claude prints during one run. It holds the cursor the
// run saves, and what the run learns that is never stored: the last result,
// the error Claude named on a message and the quota limits it reported.
type output struct {
	cursor   cursor
	model    string // the turn's model, for a cost without a model breakdown
	emit     func(faden.Event) error
	addUsage func(faden.ModelUsage)
	// closeInput, if set, ends Claude's stdin at the first result.
	closeInput func()
	last       *result // nil until a result line arrives
	apiError   string
	limits     *quotaLimits
}

// result is one result line. Claude prints one per answer. A steered message
// that Claude takes in at a tool call shares the current answer, so results
// do not count messages. Each result repeats the process's usage totals so
// far (measured with 2.1.318), so only the last one counts.
type result struct {
	ModelUsage map[string]struct {
		Input  int64   `json:"inputTokens"`
		Output int64   `json:"outputTokens"`
		Read   int64   `json:"cacheReadInputTokens"`
		Write  int64   `json:"cacheCreationInputTokens"`
		Cost   float64 `json:"costUSD"`
	} `json:"modelUsage"`
	TotalCost float64 `json:"total_cost_usd"`
	IsError   bool    `json:"is_error"`
	Result    string  `json:"result"`
}

type quotaLimits struct {
	Status        string  `json:"status"`
	ResetsAt      float64 `json:"resetsAt"`
	RateLimitType string  `json:"rateLimitType"`
}

// Run saves the session UUID before launching Claude, then stores completed
// items and saves the cursor after assistant messages.
func (b *Backend) Run(ctx context.Context, t faden.Turn, a *faden.Attempt) error {
	st := &output{model: t.Model, emit: func(e faden.Event) error { return a.Emit(ctx, e) }, addUsage: a.AddUsage}
	c := &st.cursor
	if raw := a.Cursor(); len(raw) > 0 {
		if err := json.Unmarshal(raw, c); err != nil {
			return fmt.Errorf("claude: cursor: %w", err)
		}
	}
	resume := c.SessionID != ""
	if !resume {
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return fmt.Errorf("claude: UUID: %w", err)
		}
		id[6] = (id[6] & 15) | 64
		id[8] = (id[8] & 63) | 128
		c.SessionID = fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:])
	}
	save := func() error {
		data, _ := json.Marshal(c)
		return a.SaveCursor(ctx, data)
	}
	if err := save(); err != nil {
		return fmt.Errorf("claude: checkpoint: %w", err)
	}
	argv := []string{"claude", "--print", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--include-partial-messages"}
	if resume {
		argv = append(argv, "--resume", c.SessionID)
	} else {
		argv = append(argv, "--session-id", c.SessionID)
	}
	if t.Model != "" {
		argv = append(argv, "--model", t.Model)
	}
	argv = append(argv, b.cfg.Args...)
	setup := a.Setup()
	runtime := b.cfg.Runtime
	var env []string
	if b.cfg.Threads != nil {
		token, err := b.cfg.Threads.Grant(t.Session)
		if err != nil {
			return err
		}
		defer b.cfg.Threads.Revoke(t.Session)
		file, err := os.CreateTemp("", "faden-mcp-*.json")
		if err != nil {
			return fmt.Errorf("claude: MCP config: %w", err)
		}
		defer os.Remove(file.Name())
		_, err = file.Write(b.cfg.Threads.Config())
		if cerr := file.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("claude: MCP config: %w", err)
		}
		argv = append(argv, "--mcp-config", file.Name())
		env = []string{mcp.TokenEnv + "=" + token}
	}
	// Claude ignores a new system prompt on a resumed session.
	if setup.System != "" && !resume {
		argv = append(argv, "--append-system-prompt", setup.System)
	}
	argv = append(argv, setup.Args...)
	env = append(env, setup.Env...)
	files, err := attach.Open(setup.Dir, t.Files)
	if err != nil {
		return fmt.Errorf("claude: %w", err)
	}
	defer files.Close()
	prompt := attach.WithLines(handover.Prompt(t, a), files.Others)
	first, stop := userMessage(prompt, files.Images)
	defer stop()
	stdin, send, err := os.Pipe()
	if err != nil {
		return err
	}
	defer stdin.Close()
	defer send.Close()
	in := &input{writer: send}
	in.writeMu.Lock()
	b.mu.Lock()
	b.inputs[t.RequestID] = in
	b.mu.Unlock()
	defer func() { b.mu.Lock(); delete(b.inputs, t.RequestID); b.mu.Unlock() }()
	st.closeInput = in.close
	sent := make(chan struct{})
	var sendErr error
	go func() { _, sendErr = io.Copy(send, first); in.writeMu.Unlock(); close(sent) }()
	defer func() { send.Close(); <-sent }()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	reader, writer := io.Pipe()
	type outcome struct {
		result faden.Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := runtime.Run(runCtx, faden.Command{Key: t.RequestID, Dir: setup.Dir, Argv: argv, Env: env, Stdin: stdin, Stdout: writer, MaxOutput: maxStderr})
		writer.CloseWithError(err)
		done <- outcome{res, err}
	}()
	scan := bufio.NewScanner(reader)
	scan.Buffer(make([]byte, 4096), 16*1024*1024)
	var parseErr error
	for scan.Scan() {
		if err := emitFragment(scan.Bytes(), a); err != nil {
			parseErr = err
			break
		}
		err := st.line(scan.Bytes())
		if err == nil && c.LastAssistantID != "" {
			err = save()
		}
		if err != nil {
			parseErr = err
			break
		}
	}
	if parseErr == nil {
		parseErr = scan.Err()
	}
	cancel()
	reader.Close()
	out := <-done
	in.close()
	<-sent
	if parseErr != nil {
		return fmt.Errorf("claude: stream: %w", parseErr)
	}
	// Claude has exited, so it has answered every message it took in.
	if st.last != nil {
		st.usage()
		// A cancelled turn already ends with the runner's "cancelled" event.
		if ctx.Err() == nil {
			if err := st.end(); err != nil {
				return fmt.Errorf("claude: stream: %w", err)
			}
		}
	}
	if sendErr != nil {
		return fmt.Errorf("claude: stream: %w", sendErr)
	}
	if out.err != nil {
		return fmt.Errorf("claude: run: %w", out.err)
	}
	if out.result.ExitCode != 0 {
		return fmt.Errorf("claude: exit code %d", out.result.ExitCode)
	}
	if st.last == nil {
		return errors.New("claude: stream ended without result")
	}
	return nil
}

type block struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Name    string          `json:"name"`
	Input   json.RawMessage `json:"input"`
	Content json.RawMessage `json:"content"`
	IsError bool            `json:"is_error"`
}

// line reads one line of Claude's output. A result line is kept for end and
// closes Claude's input.
func (s *output) line(line []byte) error {
	var msg struct {
		result
		Error       string       `json:"error"`
		QuotaLimits *quotaLimits `json:"quotaLimits"`
		Type        string       `json:"type"`
		Message     struct {
			ID      string  `json:"id"`
			Content []block `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &msg); err != nil {
		return err
	}
	if msg.Error != "" {
		s.apiError = msg.Error
	}
	if msg.QuotaLimits != nil {
		s.limits = msg.QuotaLimits
	}
	if msg.Type == "result" {
		last := msg.result
		s.last = &last
		if s.closeInput != nil {
			s.closeInput()
		}
		return nil
	}
	if msg.Type != "assistant" && msg.Type != "user" {
		return nil
	}
	if msg.Type == "assistant" {
		s.cursor.LastAssistantID = msg.Message.ID
	}
	for _, v := range msg.Message.Content {
		var e faden.Event
		switch v.Type {
		case "text":
			if msg.Type != "assistant" {
				continue
			}
			e.Kind, e.Text = faden.AssistantMessage, v.Text
		case "tool_use":
			e.Kind, e.Tool, e.Input = faden.ToolCall, v.Name, v.Input
			var action struct {
				Command  string `json:"command"`
				FilePath string `json:"file_path"`
			}
			json.Unmarshal(v.Input, &action)
			e.Action = action.Command
			if e.Action == "" {
				e.Action = action.FilePath
			}
			if e.Action == "" {
				e.Action = v.Name
			}
		case "tool_result":
			e.Kind, e.Failed = faden.ToolResult, v.IsError
			if json.Unmarshal(v.Content, &e.Output) != nil {
				var blocks []block
				if err := json.Unmarshal(v.Content, &blocks); err != nil {
					return err
				}
				var text []string
				for _, b := range blocks {
					if b.Type == "text" {
						text = append(text, b.Text)
					}
				}
				e.Output = strings.Join(text, "\n")
			}
		default:
			continue
		}
		if err := s.emit(e); err != nil {
			return err
		}
	}
	return nil
}

// usage adds the last result's usage, which covers the whole process.
func (s *output) usage() {
	models := make([]string, 0, len(s.last.ModelUsage))
	for model := range s.last.ModelUsage {
		models = append(models, model)
	}
	sort.Strings(models)
	for _, model := range models {
		u := s.last.ModelUsage[model]
		s.addUsage(faden.ModelUsage{Model: model, InputTokens: u.Input + u.Read + u.Write, OutputTokens: u.Output, CacheReadTokens: u.Read, CacheWriteTokens: u.Write, CostUSD: u.Cost})
	}
	if len(models) == 0 && s.last.TotalCost != 0 {
		s.addUsage(faden.ModelUsage{Model: s.model, CostUSD: s.last.TotalCost})
	}
}

// end stores the last result as the turn's end, and names the failure if
// Claude reported one.
func (s *output) end() error {
	if err := s.emit(faden.Event{Kind: faden.TurnEnded, Text: s.last.Result}); err != nil {
		return err
	}
	if s.last.IsError {
		return s.failure(s.last.Result)
	}
	return nil
}

// failure names why an unsuccessful turn failed. Claude reports a quota stop
// as error "rate_limit" or as rejected quota limits, and a missing login as
// error "authentication_failed" or a "Not logged in" result.
func (s *output) failure(result string) error {
	if s.apiError == "rate_limit" || s.limits != nil && s.limits.Status == "rejected" {
		q := &faden.QuotaError{}
		if s.limits != nil {
			q.LimitType = s.limits.RateLimitType
			if s.limits.ResetsAt > 0 {
				q.ResetsAt = time.Unix(int64(s.limits.ResetsAt), 0).UTC()
			}
		}
		return q
	}
	if s.apiError == "authentication_failed" || strings.HasPrefix(result, "Not logged in") {
		return &faden.UnavailableError{Reason: result}
	}
	return errors.New("Claude reported an unsuccessful turn")
}

// emitFragment handles partial messages without passing them to storage.
func emitFragment(line []byte, a *faden.Attempt) error {
	var m struct {
		Type  string `json:"type"`
		Event struct {
			Type  string `json:"type"`
			Delta struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				Thinking string `json:"thinking"`
			} `json:"delta"`
		} `json:"event"`
	}
	if err := json.Unmarshal(line, &m); err != nil {
		return err
	}
	if m.Type != "stream_event" || m.Event.Type != "content_block_delta" {
		return nil
	}
	switch m.Event.Delta.Type {
	case "text_delta":
		a.EmitFragment(faden.Fragment{Kind: faden.FragmentText, Text: m.Event.Delta.Text})
	case "thinking_delta":
		a.EmitFragment(faden.Fragment{Kind: faden.FragmentThinking, Text: m.Event.Delta.Thinking})
	}
	return nil
}

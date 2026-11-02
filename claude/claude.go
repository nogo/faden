// Package claude runs durable Claude Code turns through a host-supplied runtime.
// The host supplies permissions, environment, and persistent Claude session files.
// Events carry normalized fields only: Claude keeps its own session, and a
// killed turn continues from these fields.
package claude

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/nogo/faden"
	"github.com/nogo/faden/handover"
	"github.com/nogo/faden/mcp"
)

// Runtime runs keyed commands and stops leftovers before continuation.
type Runtime = faden.Runtime

// Store checkpoints the cursor before the process starts and after assistant messages.
type Store interface {
	SetCursor(context.Context, string, string, json.RawMessage) error
}

// Threads grants a session access to read_thread. *mcp.Server implements it.
type Threads interface {
	Grant(session string) (token string, err error)
	Revoke(session string)
	Config() []byte
}

// Config wires the backend to the host's runtime and store.
type Config struct {
	Runtime Runtime
	Store   Store
	// Name is the backend's key in the runner. Empty means "claude".
	Name string
	// Args contains host-supplied permission flags, never credentials.
	Args []string
	// Threads, if set, lets Claude read the session through read_thread. It
	// passes its token through Command.Env. The host must make the config
	// file and thread server reachable from the command.
	Threads Threads
}

// Backend implements faden.Backend for Claude Code.
type Backend struct{ cfg Config }

// New validates the host's collaborators.
func New(cfg Config) (*Backend, error) {
	if cfg.Runtime == nil || cfg.Store == nil {
		return nil, errors.New("claude: runtime and store are required")
	}
	if cfg.Name == "" {
		cfg.Name = "claude"
	}
	cfg.Args = append([]string(nil), cfg.Args...)
	return &Backend{cfg: cfg}, nil
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

// Run saves the session UUID before launching Claude, then stores completed items.
func (b *Backend) Run(ctx context.Context, t faden.Turn, raw json.RawMessage, emit func(faden.Event) error) (json.RawMessage, error) {
	var c cursor
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, fmt.Errorf("claude: cursor: %w", err)
		}
	}
	resume := c.SessionID != ""
	if !resume {
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return nil, fmt.Errorf("claude: UUID: %w", err)
		}
		id[6] = (id[6] & 15) | 64
		id[8] = (id[8] & 63) | 128
		c.SessionID = fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:])
	}
	save := func() error {
		data, _ := json.Marshal(c)
		return b.cfg.Store.SetCursor(ctx, t.Session, b.cfg.Name, data)
	}
	if err := save(); err != nil {
		return nil, fmt.Errorf("claude: checkpoint: %w", err)
	}
	argv := []string{"claude", "--print", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose"}
	if resume {
		argv = append(argv, "--resume", c.SessionID)
	} else {
		argv = append(argv, "--session-id", c.SessionID)
	}
	if t.Model != "" {
		argv = append(argv, "--model", t.Model)
	}
	argv = append(argv, b.cfg.Args...)
	runtime := b.cfg.Runtime
	var env []string
	if b.cfg.Threads != nil {
		token, err := b.cfg.Threads.Grant(t.Session)
		if err != nil {
			return nil, err
		}
		defer b.cfg.Threads.Revoke(t.Session)
		file, err := os.CreateTemp("", "faden-mcp-*.json")
		if err != nil {
			return nil, fmt.Errorf("claude: MCP config: %w", err)
		}
		defer os.Remove(file.Name())
		_, err = file.Write(b.cfg.Threads.Config())
		if cerr := file.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return nil, fmt.Errorf("claude: MCP config: %w", err)
		}
		argv = append(argv, "--mcp-config", file.Name())
		env = []string{mcp.TokenEnv + "=" + token}
	}
	input, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]string{"role": "user", "content": handover.Prompt(t)}})
	input = append(input, '\n')
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	reader, writer := io.Pipe()
	type outcome struct {
		result faden.Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := runtime.Run(runCtx, faden.Command{Key: t.RequestID, Argv: argv, Env: env, Stdin: bytes.NewReader(input), Stdout: writer})
		writer.CloseWithError(err)
		done <- outcome{res, err}
	}()
	scan := bufio.NewScanner(reader)
	scan.Buffer(make([]byte, 4096), 16*1024*1024)
	ended := false
	var parseErr error
	for scan.Scan() {
		end, err := parseLine(scan.Bytes(), &c, emit)
		if err == nil && c.LastAssistantID != "" {
			err = save()
		}
		if err != nil {
			parseErr = err
			break
		}
		ended = ended || end
	}
	if parseErr == nil {
		parseErr = scan.Err()
	}
	cancel()
	reader.Close()
	out := <-done
	data, _ := json.Marshal(c)
	if parseErr != nil {
		return data, fmt.Errorf("claude: stream: %w", parseErr)
	}
	if out.err != nil {
		return data, fmt.Errorf("claude: run: %w", out.err)
	}
	if out.result.ExitCode != 0 {
		return data, fmt.Errorf("claude: exit code %d", out.result.ExitCode)
	}
	if !ended {
		return data, errors.New("claude: stream ended without result")
	}
	return data, nil
}

type block struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Name    string          `json:"name"`
	Input   json.RawMessage `json:"input"`
	Content json.RawMessage `json:"content"`
	IsError bool            `json:"is_error"`
}

func parseLine(line []byte, c *cursor, emit func(faden.Event) error) (bool, error) {
	var msg struct {
		Type    string `json:"type"`
		IsError bool   `json:"is_error"`
		Result  string `json:"result"`
		Message struct {
			ID      string  `json:"id"`
			Content []block `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &msg); err != nil {
		return false, err
	}
	if msg.Type == "result" {
		if err := emit(faden.Event{Kind: faden.TurnEnded, Text: msg.Result}); err != nil {
			return false, err
		}
		if msg.IsError {
			return true, errors.New("Claude reported an unsuccessful turn")
		}
		return true, nil
	}
	if msg.Type != "assistant" && msg.Type != "user" {
		return false, nil
	}
	if msg.Type == "assistant" {
		c.LastAssistantID = msg.Message.ID
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
					return false, err
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
		if err := emit(e); err != nil {
			return false, err
		}
	}
	return false, nil
}

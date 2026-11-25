// Package acp runs durable turns on any agent that speaks the Agent Client
// Protocol (ACP, version 1) over stdio: Gemini CLI, the Claude and Codex ACP
// adapters, Kiro, Grok and others. Reach for it when faden has no native
// backend for an agent; the native backends know more of their CLI.
//
// The host supplies the command that starts the agent in ACP mode, its
// environment and its session files. The cursor is the ACP session id. A
// later turn resumes it with session/resume or session/load, whichever the
// agent offers. An agent that offers neither gets a new session, and the
// thread records that the agent lost its context.
//
// Events carry normalized fields only. The agent streams text in chunks;
// the backend joins them into one assistant message, ended by a tool call or
// the end of the prompt. ACP requires the session's working directory: it
// is Setup.Dir, or the host's own when Setup.Dir is empty, whatever the
// runtime's directory. Model and effort are not sent: ACP v1 has no
// standard field for them, so the host puts them in the command.
package acp

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nogo/faden"
	"github.com/nogo/faden/handover"
	"github.com/nogo/faden/internal/attach"
)

// Runtime runs keyed commands and stops leftovers before continuation.
type Runtime = faden.Runtime

// maxStderr bounds the stderr a run keeps in Result.Output.
const maxStderr = 64 << 10

// Config wires the backend to the host's runtime and agent.
type Config struct {
	Runtime Runtime
	// Command starts the agent in ACP mode, such as
	// {"gemini", "--experimental-acp"}. It never holds credentials.
	Command []string
	// Approve answers the agent's permission requests with its "allow once"
	// option. When false, every request gets its "reject once" option, as
	// Codex approvals are declined. The host sets it when its runtime is the
	// sandbox.
	Approve bool
}

// Backend implements faden.Backend for an ACP agent.
type Backend struct {
	cfg    Config
	mu     sync.Mutex // guards active
	active map[string]func()
}

// New validates the host's collaborators.
func New(cfg Config) (*Backend, error) {
	if cfg.Runtime == nil {
		return nil, errors.New("acp: runtime is required")
	}
	if len(cfg.Command) == 0 {
		return nil, errors.New("acp: command is required")
	}
	cfg.Command = append([]string(nil), cfg.Command...)
	return &Backend{cfg: cfg, active: make(map[string]func())}, nil
}

// ContinuesWithoutPrompt reports false: ACP has no way to continue a prompt
// that was cut, so the runner sends the killed turn's stored events.
func (*Backend) ContinuesWithoutPrompt() bool { return false }

// Stop cancels an active prompt, then stops the agent or any leftover.
func (b *Backend) Stop(ctx context.Context, t faden.Turn) error {
	b.mu.Lock()
	stop := b.active[t.RequestID]
	b.mu.Unlock()
	if stop != nil {
		stop()
	}
	return b.cfg.Runtime.Stop(ctx, t.RequestID)
}

type cursor struct {
	SessionID string `json:"session_id"`
}

// Request ids by step. The conversation is strictly sequential.
const (
	idInitialize = 1
	idSession    = 2
	idPrompt     = 3
)

// codeAuthRequired is ACP's error code for an agent that needs a login.
const codeAuthRequired = -32000

// graceAfterTurn is how long the agent gets to exit after its stdin closes.
const graceAfterTurn = 5 * time.Second

type message struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Run starts the agent, opens or resumes its session, saves the session id,
// then sends the prompt and stores completed items until the prompt ends.
func (b *Backend) Run(ctx context.Context, t faden.Turn, a *faden.Attempt) error {
	x := &exchange{ctx: ctx, a: a, t: t, approve: b.cfg.Approve, tools: map[string]*toolCall{}}
	if raw := a.Cursor(); len(raw) > 0 {
		if err := json.Unmarshal(raw, &x.cursor); err != nil {
			return fmt.Errorf("acp: cursor: %w", err)
		}
	}
	setup := a.Setup()
	cwd, err := filepath.Abs(cmp.Or(setup.Dir, "."))
	if err != nil {
		return fmt.Errorf("acp: working directory: %w", err)
	}
	x.cwd = cwd
	prompt, err := promptText(ctx, t, a, setup.Dir)
	if err != nil {
		return err
	}
	x.prompt = prompt
	argv := append(append([]string(nil), b.cfg.Command...), setup.Args...)

	// The agent outlives ctx: on cancellation it first gets session/cancel.
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	// An *os.File lets the child read stdin directly, so no copying goroutine
	// can keep Wait blocked after the child exits.
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("acp: stdin: %w", err)
	}
	stdoutR, stdoutW := io.Pipe()
	defer stdinW.Close()
	defer stdoutR.Close()
	type outcome struct {
		result faden.Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := b.cfg.Runtime.Run(runCtx, faden.Command{Key: t.RequestID, Argv: argv, Env: setup.Env, Stdin: stdinR, Stdout: stdoutW, MaxOutput: maxStderr})
		stdinR.Close()
		stdoutW.CloseWithError(err)
		done <- outcome{res, err}
	}()

	x.conn = &conn{stdin: stdinW, cancel: cancel}
	defer b.track(ctx, runCtx, t.RequestID, x.conn)()
	finish := func(ended bool, runErr error) error {
		stdinW.Close()
		// Nothing reads stdout from here on. Closed, it fails the runtime's
		// copy of what the agent still writes, instead of blocking it forever.
		stdoutR.Close()
		var out outcome
		if ended && runErr == nil {
			select {
			case out = <-done:
			case <-time.After(graceAfterTurn):
				cancel()
				out = <-done
			}
			// The agent exited, or was stopped after its prompt ended; either way the turn is complete.
			out.err = nil
		} else {
			cancel()
			out = <-done
		}
		if runErr != nil {
			return fmt.Errorf("acp: %w", runErr)
		}
		if out.err != nil {
			return fmt.Errorf("acp: run: %w", out.err)
		}
		if !ended {
			return fmt.Errorf("acp: agent exited before the prompt ended (exit code %d): %s", out.result.ExitCode, strings.TrimSpace(string(out.result.Output)))
		}
		return nil
	}

	initialize := map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{"fs": map[string]bool{"readTextFile": false, "writeTextFile": false}, "terminal": false},
		"clientInfo":         map[string]string{"name": "faden", "version": "1"},
	}
	if err := x.conn.request(idInitialize, "initialize", initialize); err != nil {
		return finish(false, err)
	}
	scan := bufio.NewScanner(stdoutR)
	scan.Buffer(make([]byte, 4096), 16*1024*1024)
	ended := false
	var runErr error
	for runErr == nil && !ended && scan.Scan() {
		var m message
		if err := json.Unmarshal(scan.Bytes(), &m); err != nil {
			runErr = err
			break
		}
		ended, runErr = x.handle(m)
	}
	if runErr == nil && !ended {
		runErr = scan.Err()
		if runErr != nil {
			runErr = fmt.Errorf("stream: %w", runErr)
		}
	}
	return finish(ended, runErr)
}

// promptText is the text of session/prompt: the catch-up, context and input,
// then a path line for each file. Images go as path lines too, so an agent
// without image support still sees them.
func promptText(ctx context.Context, t faden.Turn, a *faden.Attempt, dir string) (string, error) {
	files, err := attach.Open(dir, t.Files)
	if err != nil {
		return "", fmt.Errorf("acp: %w", err)
	}
	defer files.Close()
	paths := append([]string(nil), files.Others...)
	for _, im := range files.Images {
		paths = append(paths, im.Path)
	}
	prompt, err := handover.Prompt(ctx, t, a)
	if err != nil {
		return "", fmt.Errorf("acp: %w", err)
	}
	return attach.WithLines(prompt, paths), nil
}

// track lets Stop cancel c while the attempt of requestID runs, and cancels
// c when ctx ends before runCtx does. Call the returned func when the
// attempt ends.
func (b *Backend) track(ctx, runCtx context.Context, requestID string, c *conn) (untrack func()) {
	b.mu.Lock()
	b.active[requestID] = c.interrupt
	b.mu.Unlock()
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		select {
		case <-ctx.Done():
			c.interrupt()
		case <-runCtx.Done():
		}
	}()
	return func() {
		c.cancel()
		<-watched
		b.mu.Lock()
		delete(b.active, requestID)
		b.mu.Unlock()
	}
}

// conn is one attempt's JSON-RPC conversation with an agent. It owns the
// write side of the pipe and the session id, under one lock, so a cancel
// sent from another goroutine never interleaves with a request.
type conn struct {
	mu        sync.Mutex // guards writes to stdin and sessionID
	stdin     *os.File
	sessionID string
	cancel    context.CancelFunc // stops the agent
	once      sync.Once
}

func (c *conn) send(v map[string]any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = c.stdin.Write(append(data, '\n'))
	return err
}

func (c *conn) request(id int, method string, params any) error {
	return c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
}

func (c *conn) setSession(id string) {
	c.mu.Lock()
	c.sessionID = id
	c.mu.Unlock()
}

// interrupt sends session/cancel for the session, if it is known, and then
// stops the agent. Only the first call does anything.
func (c *conn) interrupt() {
	c.once.Do(func() {
		// Release a blocked prompt write before waiting for the lock.
		c.stdin.SetWriteDeadline(time.Now().Add(time.Second))
		c.mu.Lock()
		if c.sessionID != "" {
			data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]string{"sessionId": c.sessionID}})
			c.stdin.SetWriteDeadline(time.Now().Add(time.Second))
			c.stdin.Write(append(data, '\n'))
		}
		c.mu.Unlock()
		c.cancel()
	})
}

// toolCall is what the agent has said so far about one tool call.
type toolCall struct {
	title    string
	kind     string
	input    json.RawMessage
	output   string
	reported bool // the ToolCall event is stored
}

// exchange is the reply loop of one attempt: what it sends, what it stores,
// and what it learns that is never stored.
type exchange struct {
	ctx     context.Context
	a       *faden.Attempt
	t       faden.Turn
	cwd     string
	prompt  string
	approve bool
	cursor  cursor
	conn    *conn
	// fresh is true when this attempt opened a new session, so the host's
	// system text goes before the prompt.
	fresh bool
	// prompting is true once session/prompt is sent. Updates before it are
	// the replay of session/load, which the store already holds.
	prompting bool
	text      strings.Builder      // the assistant message being streamed
	tools     map[string]*toolCall // by toolCallId, until the call ends
}

// handle answers one message from the agent. It reports true once the
// prompt ended.
func (x *exchange) handle(m message) (bool, error) {
	switch {
	case m.Method != "" && len(m.ID) > 0:
		return false, x.conn.send(x.reply(m))
	case m.Method == "session/update":
		if !x.prompting {
			return false, nil
		}
		return false, x.update(m.Params)
	case m.Method != "":
		return false, nil
	case m.Error != nil:
		err := fmt.Errorf("request %s failed: %s", m.ID, m.Error.Message)
		if m.Error.Code == codeAuthRequired {
			return false, fmt.Errorf("%w: %w", err, &faden.UnavailableError{Reason: "agent requires authentication"})
		}
		return false, err
	}
	return x.answer(m)
}

func (x *exchange) emit(e faden.Event) error { return x.a.Emit(x.ctx, e) }

// answer advances the handshake: initialize, then session/new, /resume or
// /load, then session/prompt once the session id is stored, then the end.
func (x *exchange) answer(m message) (bool, error) {
	switch string(m.ID) {
	case fmt.Sprint(idInitialize):
		var res struct {
			ProtocolVersion   int `json:"protocolVersion"`
			AgentCapabilities struct {
				LoadSession         bool `json:"loadSession"`
				SessionCapabilities struct {
					Resume json.RawMessage `json:"resume"`
				} `json:"sessionCapabilities"`
			} `json:"agentCapabilities"`
		}
		if err := json.Unmarshal(m.Result, &res); err != nil {
			return false, fmt.Errorf("initialize: %w", err)
		}
		if res.ProtocolVersion != 1 {
			return false, fmt.Errorf("agent speaks ACP version %d, faden speaks 1", res.ProtocolVersion)
		}
		params := map[string]any{"cwd": x.cwd, "mcpServers": []any{}}
		caps := res.AgentCapabilities
		canResume := len(caps.SessionCapabilities.Resume) > 0 && string(caps.SessionCapabilities.Resume) != "null"
		switch {
		case x.cursor.SessionID != "" && canResume:
			params["sessionId"] = x.cursor.SessionID
			x.conn.setSession(x.cursor.SessionID)
			return false, x.conn.request(idSession, "session/resume", params)
		case x.cursor.SessionID != "" && caps.LoadSession:
			params["sessionId"] = x.cursor.SessionID
			x.conn.setSession(x.cursor.SessionID)
			return false, x.conn.request(idSession, "session/load", params)
		case x.cursor.SessionID != "":
			lost := "The agent cannot resume session " + x.cursor.SessionID + "; it starts a new session without the earlier thread."
			if err := x.emit(faden.Event{Kind: faden.ErrorEvent, Text: lost}); err != nil {
				return false, err
			}
		}
		x.fresh = true
		return false, x.conn.request(idSession, "session/new", params)
	case fmt.Sprint(idSession):
		if x.fresh {
			var res struct {
				SessionID string `json:"sessionId"`
			}
			if err := json.Unmarshal(m.Result, &res); err != nil || res.SessionID == "" {
				return false, errors.New("session/new answer has no session id")
			}
			x.cursor.SessionID = res.SessionID
			x.conn.setSession(res.SessionID)
			data, _ := json.Marshal(x.cursor)
			if err := x.a.SaveCursor(x.ctx, data); err != nil {
				return false, fmt.Errorf("checkpoint: %w", err)
			}
		}
		text := x.prompt
		if system := x.a.Setup().System; x.fresh && system != "" {
			text = system + "\n\n" + text
		}
		x.prompting = true
		params := map[string]any{"sessionId": x.cursor.SessionID, "prompt": []map[string]string{{"type": "text", "text": text}}}
		return false, x.conn.request(idPrompt, "session/prompt", params)
	case fmt.Sprint(idPrompt):
		var res struct {
			StopReason string `json:"stopReason"`
		}
		if err := json.Unmarshal(m.Result, &res); err != nil {
			return false, fmt.Errorf("session/prompt: %w", err)
		}
		if err := x.flushText(); err != nil {
			return false, err
		}
		if err := x.emit(faden.Event{Kind: faden.TurnEnded, Text: res.StopReason}); err != nil {
			return false, err
		}
		if res.StopReason != "end_turn" {
			return true, errors.New("prompt ended: " + res.StopReason)
		}
		return true, nil
	}
	return false, nil
}

// reply answers a request from the agent. A permission request gets the
// option the host's Approve picks; anything else is not supported, because
// faden offers the agent no file system and no terminal.
func (x *exchange) reply(m message) map[string]any {
	if m.Method != "session/request_permission" {
		return map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32601, "message": "faden does not support " + m.Method}}
	}
	var p struct {
		Options []struct {
			OptionID string `json:"optionId"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	json.Unmarshal(m.Params, &p)
	want := "reject_once"
	if x.approve {
		want = "allow_once"
	}
	outcome := map[string]string{"outcome": "cancelled"}
	for _, o := range p.Options {
		if o.Kind == want {
			outcome = map[string]string{"outcome": "selected", "optionId": o.OptionID}
			break
		}
	}
	return map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"outcome": outcome}}
}

// content is one ACP content block, or one entry of a tool call's content.
type content struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Path    string `json:"path"`
	Content *struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

// update stores what one session/update completes. Text chunks wait in
// x.text until a tool call or the end of the prompt.
func (x *exchange) update(raw json.RawMessage) error {
	var p struct {
		Update struct {
			Kind       string          `json:"sessionUpdate"`
			Content    json.RawMessage `json:"content"`
			ToolCallID string          `json:"toolCallId"`
			Title      string          `json:"title"`
			ToolKind   string          `json:"kind"`
			Status     string          `json:"status"`
			RawInput   json.RawMessage `json:"rawInput"`
			RawOutput  json.RawMessage `json:"rawOutput"`
		} `json:"update"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	u := p.Update
	switch u.Kind {
	case "agent_message_chunk", "agent_thought_chunk":
		var c content
		if json.Unmarshal(u.Content, &c) != nil || c.Type != "text" {
			return nil
		}
		if u.Kind == "agent_thought_chunk" {
			x.a.EmitFragment(faden.Fragment{Kind: faden.FragmentThinking, Text: c.Text})
			return nil
		}
		x.text.WriteString(c.Text)
		x.a.EmitFragment(faden.Fragment{Kind: faden.FragmentText, Text: c.Text})
		return nil
	case "tool_call", "tool_call_update":
		if err := x.flushText(); err != nil {
			return err
		}
		tc := x.tools[u.ToolCallID]
		if tc == nil {
			tc = &toolCall{}
			x.tools[u.ToolCallID] = tc
		}
		tc.title = cmp.Or(u.Title, tc.title)
		tc.kind = cmp.Or(u.ToolKind, tc.kind)
		if len(u.RawInput) > 0 {
			tc.input = u.RawInput
		}
		// An update's content replaces the earlier content.
		if len(u.Content) > 0 {
			tc.output = toolOutput(u.Content)
		}
		if tc.output == "" && len(u.RawOutput) > 0 && string(u.RawOutput) != "null" {
			tc.output = string(u.RawOutput)
		}
		return x.toolStatus(u.ToolCallID, tc, u.Status)
	}
	return nil
}

// toolStatus stores the call once the agent runs it, and the result once
// it ends. A pending call may still be rejected, so it waits.
func (x *exchange) toolStatus(id string, tc *toolCall, status string) error {
	if status == "" || status == "pending" {
		return nil
	}
	tool := cmp.Or(tc.kind, "other")
	if !tc.reported {
		tc.reported = true
		if err := x.emit(faden.Event{Kind: faden.ToolCall, Tool: tool, Action: tc.title, Input: tc.input}); err != nil {
			return err
		}
	}
	if status != "completed" && status != "failed" {
		return nil
	}
	delete(x.tools, id)
	return x.emit(faden.Event{Kind: faden.ToolResult, Tool: tool, Output: tc.output, Failed: status == "failed"})
}

// toolOutput is the text of a tool call's content: its text blocks, and a
// line for each file it changed.
func toolOutput(raw json.RawMessage) string {
	var blocks []content
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var b strings.Builder
	for _, c := range blocks {
		var part string
		switch {
		case c.Type == "content" && c.Content != nil && c.Content.Type == "text":
			part = c.Content.Text
		case c.Type == "diff":
			part = "changed " + c.Path
		default:
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(part)
	}
	return b.String()
}

// flushText stores the streamed text as one assistant message.
func (x *exchange) flushText() error {
	if x.text.Len() == 0 {
		return nil
	}
	text := x.text.String()
	x.text.Reset()
	return x.emit(faden.Event{Kind: faden.AssistantMessage, Text: text})
}

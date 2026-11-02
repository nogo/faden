// Package codex runs durable Codex turns through a host-supplied runtime.
// It starts `codex app-server` and speaks its JSON-RPC over stdio.
// The host supplies sandbox and approval flags, environment, and the
// persistent Codex session files. Events carry normalized fields only: Codex
// keeps its own session, and a killed turn continues from these fields.
package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nogo/faden"
	"github.com/nogo/faden/handover"
	"github.com/nogo/faden/internal/attach"
)

// Runtime runs keyed commands and stops leftovers before continuation.
type Runtime = faden.Runtime

// Config wires the backend to the host's runtime.
type Config struct {
	Runtime Runtime
	// Args are host-supplied flags for `codex app-server`, such as
	// `-c sandbox_mode="workspace-write"` and `-c approval_policy="never"`.
	// They never hold credentials.
	Args []string
}

// Backend implements faden.Backend for Codex.
type Backend struct {
	cfg    Config
	mu     sync.Mutex // guards active
	active map[string]func()
}

// New validates the host's collaborators.
func New(cfg Config) (*Backend, error) {
	if cfg.Runtime == nil {
		return nil, errors.New("codex: runtime is required")
	}
	cfg.Args = append([]string(nil), cfg.Args...)
	return &Backend{cfg: cfg, active: make(map[string]func())}, nil
}

// ContinuesWithoutPrompt reports false so the runner supplies stored events
// that Codex's own transcript can lose when a turn is killed.
func (*Backend) ContinuesWithoutPrompt() bool { return false }

// Stop interrupts an active turn, then stops its app-server or any leftover.
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
	ThreadID string `json:"thread_id"`
}

// limitState holds the primary rate limit from the last notification that
// carried one. Both fields are zero until then.
type limitState struct {
	resetsAt  time.Time
	limitType string
}

// note reads a rate-limit snapshot from m. The wire name is not verified for
// app-server, so it accepts the snake_case of codex exec --json and camelCase.
// Reset times are Unix seconds.
func (l *limitState) note(m message) {
	var p struct {
		Snake struct {
			Primary *struct {
				ResetsAt int64 `json:"resets_at"`
			} `json:"primary"`
		} `json:"rate_limits"`
		Camel struct {
			Primary *struct {
				ResetsAt int64 `json:"resetsAt"`
			} `json:"primary"`
		} `json:"rateLimits"`
	}
	if json.Unmarshal(m.Params, &p) != nil {
		return
	}
	var at int64
	switch {
	case p.Snake.Primary != nil:
		at = p.Snake.Primary.ResetsAt
	case p.Camel.Primary != nil:
		at = p.Camel.Primary.ResetsAt
	}
	if at > 0 {
		l.resetsAt, l.limitType = time.Unix(at, 0).UTC(), "primary"
	}
}

// Request ids by step. The conversation is strictly sequential.
const (
	idInitialize = 1
	idThread     = 2
	idTurn       = 3
)

// graceAfterTurn is how long Codex gets to exit after its stdin closes.
const graceAfterTurn = 5 * time.Second

type message struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Run starts the app-server, opens or resumes the thread, saves the thread id,
// then starts the turn and stores completed items until the turn ends.
func (b *Backend) Run(ctx context.Context, t faden.Turn, a *faden.Attempt) error {
	var c cursor
	if raw := a.Cursor(); len(raw) > 0 {
		if err := json.Unmarshal(raw, &c); err != nil {
			return fmt.Errorf("codex: cursor: %w", err)
		}
	}
	setup := a.Setup()
	items := []map[string]string{}
	// A continuation without a prompt sends nothing: the thread already holds the files.
	if t.Input != "" || t.Context != "" {
		files, err := attach.Open(setup.Dir, t.Files)
		if err != nil {
			return fmt.Errorf("codex: %w", err)
		}
		for _, im := range files.Images {
			items = append(items, map[string]string{"type": "localImage", "path": im.Abs})
		}
		files.Close()
		text := attach.WithLines(handover.Prompt(t, a), files.Others)
		items = append([]map[string]string{{"type": "text", "text": text}}, items...)
	}
	argv := append([]string{"codex", "app-server"}, b.cfg.Args...)
	argv = append(argv, setup.Args...)

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	// An *os.File lets the child read stdin directly, so no copying goroutine
	// can keep Wait blocked after the child exits.
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("codex: stdin: %w", err)
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
		res, err := b.cfg.Runtime.Run(runCtx, faden.Command{Key: t.RequestID, Argv: argv, Env: setup.Env, Stdin: stdinR, Stdout: stdoutW})
		stdinR.Close()
		stdoutW.CloseWithError(err)
		done <- outcome{res, err}
	}()

	var wire sync.Mutex // guards writes and the active protocol turn id
	var turnID, threadID string
	send := func(v map[string]any) error {
		wire.Lock()
		defer wire.Unlock()
		data, _ := json.Marshal(v)
		_, err := stdinW.Write(append(data, '\n'))
		return err
	}
	request := func(id int, method string, params any) error {
		return send(map[string]any{"id": id, "method": method, "params": params})
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			// Release a blocked prompt write before waiting for the wire lock.
			stdinW.SetWriteDeadline(time.Now().Add(time.Second))
			wire.Lock()
			if turnID != "" {
				data, _ := json.Marshal(map[string]any{"id": 4, "method": "turn/interrupt", "params": map[string]string{"threadId": threadID, "turnId": turnID}})
				stdinW.SetWriteDeadline(time.Now().Add(time.Second))
				stdinW.Write(append(data, '\n'))
			}
			wire.Unlock()
			cancel()
		})
	}
	b.mu.Lock()
	b.active[t.RequestID] = stop
	b.mu.Unlock()
	watchDone := make(chan struct{})
	defer func() {
		cancel()
		<-watchDone
		b.mu.Lock()
		delete(b.active, t.RequestID)
		b.mu.Unlock()
	}()
	go func() {
		defer close(watchDone)
		select {
		case <-ctx.Done():
			stop()
		case <-runCtx.Done():
		}
	}()
	finish := func(ended bool, runErr error) error {
		stdinW.Close()
		var out outcome
		if ended && runErr == nil {
			select {
			case out = <-done:
			case <-time.After(graceAfterTurn):
				cancel()
				out = <-done
			}
			// Codex exited, or was stopped after its turn ended; either way the turn is complete.
			out.err, out.result.ExitCode = nil, 0
		} else {
			cancel()
			out = <-done
		}
		if runErr != nil {
			return fmt.Errorf("codex: %w", runErr)
		}
		if out.err != nil {
			return fmt.Errorf("codex: run: %w", out.err)
		}
		if !ended {
			return errors.New("codex: stream ended without turn/completed")
		}
		return nil
	}

	if err := request(idInitialize, "initialize", map[string]any{"clientInfo": map[string]string{"name": "faden", "version": "1"}}); err != nil {
		return finish(false, err)
	}
	scan := bufio.NewScanner(stdoutR)
	scan.Buffer(make([]byte, 4096), 16*1024*1024)
	emit := func(e faden.Event) error { return a.Emit(ctx, e) }
	ended := false
	var usage faden.ModelUsage
	var base *faden.ModelUsage
	model := t.Model
	defer func() {
		if usage != (faden.ModelUsage{}) {
			usage.Model = model
			a.AddUsage(usage)
		}
	}()
	limits := &limitState{}
	var runErr error
	for runErr == nil && !ended && scan.Scan() {
		var m message
		if err := json.Unmarshal(scan.Bytes(), &m); err != nil {
			runErr = err
			break
		}
		if string(m.ID) == fmt.Sprint(idThread) {
			var p struct {
				Thread struct {
					ID string `json:"id"`
				} `json:"thread"`
			}
			if json.Unmarshal(m.Result, &p) == nil {
				wire.Lock()
				threadID = p.Thread.ID
				wire.Unlock()
			}
		}
		if m.Method == "turn/started" || string(m.ID) == fmt.Sprint(idTurn) {
			var p struct {
				Turn struct {
					ID string `json:"id"`
				} `json:"turn"`
			}
			raw := m.Params
			if m.Method == "" {
				raw = m.Result
			}
			if json.Unmarshal(raw, &p) == nil && p.Turn.ID != "" {
				wire.Lock()
				turnID = p.Turn.ID
				wire.Unlock()
			}
		}
		switch {
		case m.Method != "" && len(m.ID) > 0:
			runErr = send(declineOrReject(m))
		case m.Method != "":
			if m.Method == "item/agentMessage/delta" {
				var p struct {
					Delta string `json:"delta"`
				}
				runErr = json.Unmarshal(m.Params, &p)
				if runErr == nil {
					a.EmitFragment(faden.Fragment{Kind: faden.FragmentText, Text: p.Delta})
				}
			} else if m.Method == "thread/tokenUsage/updated" {
				var total, last faden.ModelUsage
				total, last, runErr = tokenUsage(m.Params)
				if runErr == nil {
					if base == nil {
						v := subtractUsage(total, last)
						base = &v
					}
					usage = subtractUsage(total, *base)
				}
			} else {
				limits.note(m)
				ended, runErr = notification(m, emit, limits)
			}
		case m.Error != nil:
			runErr = fmt.Errorf("request %s failed: %s", m.ID, m.Error.Message)
		default:
			if string(m.ID) == fmt.Sprint(idThread) {
				var result struct {
					Model string `json:"model"`
				}
				if json.Unmarshal(m.Result, &result) == nil && result.Model != "" {
					model = result.Model
				}
			}
			runErr = answer(ctx, a, t, &c, m, items, request, send)
		}
	}
	if runErr == nil && !ended {
		runErr = scan.Err()
		if runErr != nil {
			runErr = fmt.Errorf("stream: %w", runErr)
		}
	}
	return finish(ended, runErr)
}

// Subtract the thread total before this attempt to exclude earlier turns.
func subtractUsage(total, base faden.ModelUsage) faden.ModelUsage {
	return faden.ModelUsage{InputTokens: total.InputTokens - base.InputTokens, OutputTokens: total.OutputTokens - base.OutputTokens, CacheReadTokens: total.CacheReadTokens - base.CacheReadTokens}
}

type usageBreakdown struct {
	Input  int64 `json:"inputTokens"`
	Cached int64 `json:"cachedInputTokens"`
	Output int64 `json:"outputTokens"`
}

func (u usageBreakdown) usage() faden.ModelUsage {
	return faden.ModelUsage{InputTokens: u.Input, CacheReadTokens: u.Cached, OutputTokens: u.Output}
}

func tokenUsage(raw json.RawMessage) (faden.ModelUsage, faden.ModelUsage, error) {
	var p struct {
		TokenUsage struct {
			Last  usageBreakdown `json:"last"`
			Total usageBreakdown `json:"total"`
		} `json:"tokenUsage"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return faden.ModelUsage{}, faden.ModelUsage{}, err
	}
	return p.TokenUsage.Total.usage(), p.TokenUsage.Last.usage(), nil
}

// answer advances the handshake: initialize, then thread/start or
// thread/resume, then turn/start once the thread id is stored.
func answer(ctx context.Context, a *faden.Attempt, t faden.Turn, c *cursor, m message, items []map[string]string, request func(int, string, any) error, send func(map[string]any) error) error {
	switch string(m.ID) {
	case fmt.Sprint(idInitialize):
		if err := send(map[string]any{"method": "initialized"}); err != nil {
			return err
		}
		params := map[string]any{}
		if t.Model != "" {
			params["model"] = t.Model
		}
		setup := a.Setup()
		if setup.Dir != "" {
			params["cwd"] = setup.Dir
		}
		if setup.System != "" {
			params["developerInstructions"] = setup.System
		}
		if c.ThreadID != "" {
			params["threadId"] = c.ThreadID
			return request(idThread, "thread/resume", params)
		}
		return request(idThread, "thread/start", params)
	case fmt.Sprint(idThread):
		var res struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
		}
		if err := json.Unmarshal(m.Result, &res); err != nil || res.Thread.ID == "" {
			return errors.New("thread answer has no thread id")
		}
		c.ThreadID = res.Thread.ID
		data, _ := json.Marshal(c)
		if err := a.SaveCursor(ctx, data); err != nil {
			return fmt.Errorf("checkpoint: %w", err)
		}
		return request(idTurn, "turn/start", map[string]any{"threadId": c.ThreadID, "input": items})
	}
	return nil
}

// declineOrReject answers a request from Codex. Approvals are declined;
// anything else is not supported.
func declineOrReject(m message) map[string]any {
	switch m.Method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		return map[string]any{"id": m.ID, "result": map[string]string{"decision": "decline"}}
	case "execCommandApproval", "applyPatchApproval":
		return map[string]any{"id": m.ID, "result": map[string]string{"decision": "denied"}}
	}
	return map[string]any{"id": m.ID, "error": map[string]any{"code": -32601, "message": "faden does not support " + m.Method}}
}

type item struct {
	Type             string `json:"type"`
	Text             string `json:"text"`
	Command          string `json:"command"`
	CWD              string `json:"cwd"`
	AggregatedOutput string `json:"aggregatedOutput"`
	Status           string `json:"status"`
	ExitCode         *int   `json:"exitCode"`
	Changes          []struct {
		Path string `json:"path"`
	} `json:"changes"`
	Server    string          `json:"server"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
	Result    json.RawMessage `json:"result"`
	Error     json.RawMessage `json:"error"`
}

// notification stores a completed item or the turn's end. Other
// notifications, such as streaming deltas, are dropped.
func notification(m message, emit func(faden.Event) error, limits *limitState) (bool, error) {
	switch m.Method {
	case "item/completed":
		var p struct {
			Item item `json:"item"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil {
			return false, err
		}
		return false, emitItem(p.Item, emit)
	case "turn/completed":
		var p struct {
			Turn struct {
				Status string `json:"status"`
				Error  *struct {
					Message        string          `json:"message"`
					CodexErrorInfo json.RawMessage `json:"codexErrorInfo"`
				} `json:"error"`
			} `json:"turn"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil {
			return false, err
		}
		if err := emit(faden.Event{Kind: faden.TurnEnded, Text: p.Turn.Status}); err != nil {
			return false, err
		}
		if p.Turn.Status != "completed" {
			msg := "turn " + p.Turn.Status
			if p.Turn.Error != nil {
				msg += ": " + p.Turn.Error.Message
				if string(p.Turn.Error.CodexErrorInfo) == `"usageLimitExceeded"` {
					return true, fmt.Errorf("codex: %s: %w", msg, &faden.QuotaError{ResetsAt: limits.resetsAt, LimitType: limits.limitType})
				}
			}
			return true, errors.New(msg)
		}
		return true, nil
	}
	return false, nil
}

func emitItem(it item, emit func(faden.Event) error) error {
	var call, result faden.Event
	switch it.Type {
	case "agentMessage":
		return emit(faden.Event{Kind: faden.AssistantMessage, Text: it.Text})
	case "commandExecution":
		input, _ := json.Marshal(map[string]string{"command": it.Command, "cwd": it.CWD})
		call = faden.Event{Tool: "commandExecution", Action: it.Command, Input: input}
		result.Output = it.AggregatedOutput
	case "fileChange":
		var paths []string
		for _, ch := range it.Changes {
			paths = append(paths, ch.Path)
		}
		input, _ := json.Marshal(map[string]any{"paths": paths})
		call = faden.Event{Tool: "fileChange", Action: strings.Join(paths, ", "), Input: input}
		result.Output = it.Status
	case "mcpToolCall":
		call = faden.Event{Tool: it.Server + "/" + it.Tool, Action: it.Server + "/" + it.Tool, Input: it.Arguments}
		result.Output = string(it.Result)
		if len(it.Error) > 0 && string(it.Error) != "null" {
			result.Output, result.Failed = string(it.Error), true
		}
	default:
		return nil
	}
	call.Kind = faden.ToolCall
	result.Kind, result.Tool = faden.ToolResult, call.Tool
	result.Failed = result.Failed || it.Status == "failed" || it.Status == "declined" ||
		it.ExitCode != nil && *it.ExitCode != 0
	if err := emit(call); err != nil {
		return err
	}
	return emit(result)
}

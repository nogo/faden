// Package codex runs durable Codex turns through a host-supplied runtime.
// It starts `codex app-server` and speaks its JSON-RPC over stdio.
// The host supplies sandbox and approval flags, environment, and the
// persistent Codex session files. Events carry normalized fields only: Codex
// keeps its own session, and a killed turn continues from these fields.
package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nogo/faden"
	"github.com/nogo/faden/handover"
	"github.com/nogo/faden/internal/agentproc"
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
	cfg   Config
	procs agentproc.Running
}

// New validates the host's collaborators.
func New(cfg Config) (*Backend, error) {
	if cfg.Runtime == nil {
		return nil, errors.New("codex: runtime is required")
	}
	cfg.Args = append([]string(nil), cfg.Args...)
	return &Backend{cfg: cfg}, nil
}

// ContinuesWithoutPrompt reports false so the runner supplies stored events
// that Codex's own transcript can lose when a turn is killed.
func (*Backend) ContinuesWithoutPrompt() bool { return false }

// Stop interrupts an active turn, then stops its app-server or any leftover.
func (b *Backend) Stop(ctx context.Context, t faden.Turn) error {
	b.procs.Interrupt(t.RequestID)
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
	idInterrupt  = 4
)

type message struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code int `json:"code"`
	} `json:"error"`
}

// Run starts the app-server, opens or resumes the thread, saves the thread id,
// then starts the turn and stores completed items until the turn ends.
func (b *Backend) Run(ctx context.Context, t faden.Turn, a *faden.Attempt) error {
	x := &exchange{ctx: ctx, a: a, t: t, usage: attemptUsage{model: t.Model}}
	if raw := a.Cursor(); len(raw) > 0 {
		if err := json.Unmarshal(raw, &x.cursor); err != nil {
			return fmt.Errorf("codex: cursor: %w", err)
		}
	}
	setup := a.Setup()
	items, err := turnInput(ctx, t, a, setup.Dir)
	if err != nil {
		return err
	}
	x.items = items
	argv := append([]string{"codex", "app-server"}, b.cfg.Args...)
	argv = append(argv, setup.Args...)

	// The app-server outlives ctx: on cancellation it first gets turn/interrupt.
	c := &conn{}
	proc, err := b.procs.Start(ctx, b.cfg.Runtime, faden.Command{Key: t.RequestID, Argv: argv, Env: setup.Env}, c.interruptMessage)
	if err != nil {
		return fmt.Errorf("codex: %w", err)
	}
	c.Proc, x.conn = proc, c
	ended := false
	runErr := c.request(idInitialize, "initialize", map[string]any{"clientInfo": map[string]string{"name": "faden", "version": "1"}})
	if runErr == nil {
		defer x.usage.report(a)
		ended, runErr = proc.Read(func(line []byte) (bool, error) {
			var m message
			if err := json.Unmarshal(line, &m); err != nil {
				return false, err
			}
			return x.handle(m)
		})
	}
	// Codex exits, or is stopped, after its turn ended; either way the turn
	// is complete.
	_, procErr := proc.Close(ended && runErr == nil)
	switch {
	case runErr != nil:
		return fmt.Errorf("codex: %w", runErr)
	case procErr != nil:
		return fmt.Errorf("codex: run: %w", procErr)
	case !ended:
		return errors.New("codex: stream ended without turn/completed")
	}
	return nil
}

// turnInput is the input of turn/start: the prompt with a line for each file
// that is not an image, then the images as local files. A continuation
// without a prompt sends nothing: the thread already holds the files.
func turnInput(ctx context.Context, t faden.Turn, a *faden.Attempt, dir string) ([]map[string]string, error) {
	items := []map[string]string{}
	if t.Input == "" && t.Context == "" {
		return items, nil
	}
	files, err := attach.Open(dir, t.Files)
	if err != nil {
		return nil, fmt.Errorf("codex: %w", err)
	}
	defer files.Close()
	prompt, err := handover.Prompt(ctx, t, a)
	if err != nil {
		return nil, fmt.Errorf("codex: %w", err)
	}
	items = append(items, map[string]string{"type": "text", "text": attach.WithLines(prompt, files.Others)})
	for _, im := range files.Images {
		items = append(items, map[string]string{"type": "localImage", "path": im.Abs})
	}
	return items, nil
}

// conn is one attempt's conversation with an app-server. It keeps the ids
// the reply loop learns, so an interrupt names the current turn.
type conn struct {
	*agentproc.Proc
	mu       sync.Mutex // guards threadID and turnID
	threadID string
	turnID   string
}

func (c *conn) request(id int, method string, params any) error {
	return c.Send(map[string]any{"id": id, "method": method, "params": params})
}

// learn keeps the thread and turn ids that m carries, so interrupt can name them.
func (c *conn) learn(m message) {
	if string(m.ID) == fmt.Sprint(idThread) {
		var p struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
		}
		if json.Unmarshal(m.Result, &p) == nil {
			c.mu.Lock()
			c.threadID = p.Thread.ID
			c.mu.Unlock()
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
			c.mu.Lock()
			c.turnID = p.Turn.ID
			c.mu.Unlock()
		}
	}
}

// interruptMessage is turn/interrupt for the active turn, or nil before
// Codex named one.
func (c *conn) interruptMessage() any {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.turnID == "" {
		return nil
	}
	return map[string]any{"id": idInterrupt, "method": "turn/interrupt", "params": map[string]string{"threadId": c.threadID, "turnId": c.turnID}}
}

// exchange is the reply loop of one attempt: what it sends, what it stores,
// and what it learns that is never stored.
type exchange struct {
	ctx    context.Context
	a      *faden.Attempt
	t      faden.Turn
	cursor cursor
	items  []map[string]string // the input of turn/start
	conn   *conn
	usage  attemptUsage
	limits limitState
}

// handle answers one message from Codex. It reports true once the turn ended.
func (x *exchange) handle(m message) (bool, error) {
	x.conn.learn(m)
	switch {
	case m.Method != "" && len(m.ID) > 0:
		return false, x.conn.Send(declineOrReject(m))
	case m.Method == "item/agentMessage/delta":
		var p struct {
			Delta string `json:"delta"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil {
			return false, err
		}
		x.a.EmitFragment(faden.Fragment{Kind: faden.FragmentText, Text: p.Delta})
		return false, nil
	case m.Method == "thread/tokenUsage/updated":
		return false, x.usage.update(m.Params)
	case m.Method != "":
		x.limits.note(m)
		return notification(m, x.emit, &x.limits)
	case m.Error != nil:
		// The message is Codex's own text and can echo what it was sent; the
		// code is the protocol's.
		return false, fmt.Errorf("request %s failed (code %d)", m.ID, m.Error.Code)
	}
	return false, x.answer(m)
}

func (x *exchange) emit(e faden.Event) error { return x.a.Emit(x.ctx, e) }

// answer advances the handshake: initialize, then thread/start or
// thread/resume, then turn/start once the thread id is stored.
func (x *exchange) answer(m message) error {
	switch string(m.ID) {
	case fmt.Sprint(idInitialize):
		if err := x.conn.Send(map[string]any{"method": "initialized"}); err != nil {
			return err
		}
		params := map[string]any{}
		if x.t.Model != "" {
			params["model"] = x.t.Model
		}
		setup := x.a.Setup()
		if setup.Dir != "" {
			params["cwd"] = setup.Dir
		}
		if setup.System != "" {
			params["developerInstructions"] = setup.System
		}
		if x.cursor.ThreadID != "" {
			params["threadId"] = x.cursor.ThreadID
			return x.conn.request(idThread, "thread/resume", params)
		}
		return x.conn.request(idThread, "thread/start", params)
	case fmt.Sprint(idThread):
		var res struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
			Model string `json:"model"`
		}
		err := json.Unmarshal(m.Result, &res)
		if err == nil && res.Model != "" {
			x.usage.model = res.Model
		}
		if err != nil || res.Thread.ID == "" {
			return errors.New("thread answer has no thread id")
		}
		x.cursor.ThreadID = res.Thread.ID
		data, _ := json.Marshal(x.cursor)
		if err := x.a.SaveCursor(x.ctx, data); err != nil {
			return fmt.Errorf("checkpoint: %w", err)
		}
		params := map[string]any{"threadId": x.cursor.ThreadID, "input": x.items}
		if x.t.Effort != "" {
			params["effort"] = x.t.Effort
		}
		return x.conn.request(idTurn, "turn/start", params)
	}
	return nil
}

// attemptUsage turns Codex's thread totals into this attempt's use. Codex
// reports totals for the whole thread, so the total before the attempt's
// first call is subtracted: earlier turns and repeated snapshots do not count.
type attemptUsage struct {
	base  *faden.ModelUsage
	used  faden.ModelUsage
	model string // the model Codex named for the thread, else the turn's
}

func (u *attemptUsage) update(raw json.RawMessage) error {
	total, last, err := tokenUsage(raw)
	if err != nil {
		return err
	}
	if u.base == nil {
		v := subtractUsage(total, last)
		u.base = &v
	}
	u.used = subtractUsage(total, *u.base)
	return nil
}

// report adds the attempt's use to a, if Codex reported any.
func (u *attemptUsage) report(a *faden.Attempt) {
	if u.used != (faden.ModelUsage{}) {
		used := u.used
		used.Model = u.model
		a.AddUsage(used)
	}
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
			// The error's message is Codex's own text and stays out; its kind,
			// when it is a plain name, is the protocol's.
			msg := "turn " + p.Turn.Status
			if p.Turn.Error != nil {
				var kind string
				if json.Unmarshal(p.Turn.Error.CodexErrorInfo, &kind) == nil && kind != "" {
					msg += " (" + kind + ")"
				}
				if kind == "usageLimitExceeded" {
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

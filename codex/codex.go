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
	"time"

	"github.com/nogo/faden"
	"github.com/nogo/faden/handover"
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
type Backend struct{ cfg Config }

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

// Stop stops the app-server an earlier attempt of t left running.
func (b *Backend) Stop(ctx context.Context, t faden.Turn) error {
	return b.cfg.Runtime.Stop(ctx, t.RequestID)
}

type cursor struct {
	ThreadID string `json:"thread_id"`
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
	items := []map[string]string{}
	if t.Input != "" {
		items = append(items, map[string]string{"type": "text", "text": handover.Prompt(t, a)})
	}
	setup := a.Setup()
	argv := append([]string{"codex", "app-server"}, b.cfg.Args...)
	argv = append(argv, setup.Args...)

	runCtx, cancel := context.WithCancel(ctx)
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

	send := func(v map[string]any) error {
		data, _ := json.Marshal(v)
		_, err := stdinW.Write(append(data, '\n'))
		return err
	}
	request := func(id int, method string, params any) error {
		return send(map[string]any{"id": id, "method": method, "params": params})
	}
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
	var runErr error
	for runErr == nil && !ended && scan.Scan() {
		var m message
		if err := json.Unmarshal(scan.Bytes(), &m); err != nil {
			runErr = err
			break
		}
		switch {
		case m.Method != "" && len(m.ID) > 0:
			runErr = send(declineOrReject(m))
		case m.Method != "":
			ended, runErr = notification(m, emit)
		case m.Error != nil:
			runErr = fmt.Errorf("request %s failed: %s", m.ID, m.Error.Message)
		default:
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
func notification(m message, emit func(faden.Event) error) (bool, error) {
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
					Message string `json:"message"`
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

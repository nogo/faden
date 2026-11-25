// Package agentproc runs an agent CLI for one attempt and exchanges JSON
// lines with it over stdin and stdout. Use it for an agent that speaks a
// JSON-RPC protocol on stdio, as Codex's app-server and ACP agents do; the
// protocol stays in the backend.
package agentproc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/nogo/faden"
)

// Grace is how long an agent may take to exit after its turn ended, before
// it is stopped.
const Grace = 5 * time.Second

const (
	maxStderr = 64 << 10 // the stderr a run keeps in Result.Output
	maxLine   = 16 << 20 // the longest line read from the agent
)

// Running holds the processes of the attempts that run, by command key, so a
// backend's Stop can interrupt one. The zero value is ready.
type Running struct {
	mu    sync.Mutex
	procs map[string]*Proc
}

// Interrupt interrupts the process running under key, if there is one.
func (r *Running) Interrupt(key string) {
	r.mu.Lock()
	p := r.procs[key]
	r.mu.Unlock()
	if p != nil {
		p.Interrupt()
	}
}

// Start runs cmd through rt, with pipes for its stdin and stdout, and keeps
// it under cmd.Key until Close. The process outlives ctx: when ctx ends
// first, the process is interrupted. interrupt returns the message to send
// before the process is stopped, or nil for none; it runs under the write
// lock, so it must not call Send.
func (r *Running) Start(ctx context.Context, rt faden.Runtime, cmd faden.Command, interrupt func() any) (*Proc, error) {
	// An *os.File lets the child read stdin directly, so no copying goroutine
	// can keep Wait blocked after the child exits.
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("stdin: %w", err)
	}
	stdoutR, stdoutW := io.Pipe()
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	p := &Proc{
		stdin:     stdinW,
		stdout:    stdoutR,
		cancel:    cancel,
		interrupt: interrupt,
		done:      make(chan outcome, 1),
		watched:   make(chan struct{}),
	}
	p.forget = r.add(cmd.Key, p)
	cmd.Stdin, cmd.Stdout, cmd.MaxOutput = stdinR, stdoutW, maxStderr
	go func() {
		res, err := rt.Run(runCtx, cmd)
		stdinR.Close()
		stdoutW.CloseWithError(err)
		p.done <- outcome{res, err}
	}()
	go func() {
		defer close(p.watched)
		select {
		case <-ctx.Done():
			p.Interrupt()
		case <-runCtx.Done():
		}
	}()
	return p, nil
}

func (r *Running) add(key string, p *Proc) (forget func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.procs == nil {
		r.procs = map[string]*Proc{}
	}
	r.procs[key] = p
	return func() {
		r.mu.Lock()
		delete(r.procs, key)
		r.mu.Unlock()
	}
}

// Proc is the agent process of one attempt. Its writes are serialized, so
// an interrupt sent from another goroutine never interleaves with a message.
type Proc struct {
	mu        sync.Mutex // guards writes to stdin
	stdin     *os.File
	stdout    *io.PipeReader
	cancel    context.CancelFunc // stops the process
	interrupt func() any
	once      sync.Once
	done      chan outcome
	watched   chan struct{} // closed when the goroutine that watches ctx ends
	forget    func()
}

type outcome struct {
	result faden.Result
	err    error
}

// Send writes v as one JSON line.
func (p *Proc) Send(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err = p.stdin.Write(append(data, '\n'))
	return err
}

// Read calls handle with each line the agent writes, until handle reports
// the end of the turn or fails, or the output ends. A line is valid only
// during the call. Read reports whether the turn ended.
func (p *Proc) Read(handle func(line []byte) (ended bool, err error)) (bool, error) {
	scan := bufio.NewScanner(p.stdout)
	scan.Buffer(make([]byte, 4096), maxLine)
	for scan.Scan() {
		if ended, err := handle(scan.Bytes()); ended || err != nil {
			return ended, err
		}
	}
	if err := scan.Err(); err != nil {
		return false, fmt.Errorf("stream: %w", err)
	}
	return false, nil
}

// Interrupt sends the interrupt message, if there is one, and then stops the
// process. Only the first call does anything.
func (p *Proc) Interrupt() {
	p.once.Do(func() {
		// Release a blocked write before waiting for the lock.
		p.stdin.SetWriteDeadline(time.Now().Add(time.Second))
		p.mu.Lock()
		if msg := p.interruptMessage(); msg != nil {
			if data, err := json.Marshal(msg); err == nil {
				p.stdin.SetWriteDeadline(time.Now().Add(time.Second))
				p.stdin.Write(append(data, '\n'))
			}
		}
		p.mu.Unlock()
		p.cancel()
	})
}

func (p *Proc) interruptMessage() any {
	if p.interrupt == nil {
		return nil
	}
	return p.interrupt()
}

// Close ends the attempt and waits for the process. After a turn that ended,
// the agent gets Grace to exit before it is stopped, and how it exits is no
// error. Otherwise it is stopped at once, and Close returns what the runtime
// returned.
func (p *Proc) Close(ended bool) (faden.Result, error) {
	p.stdin.Close()
	// Nothing reads stdout from here on. Closed, it fails the runtime's copy
	// of what the agent still writes, instead of blocking it forever.
	p.stdout.Close()
	var out outcome
	if ended {
		select {
		case out = <-p.done:
		case <-time.After(Grace):
			p.cancel()
			out = <-p.done
		}
		out = outcome{}
	} else {
		p.cancel()
		out = <-p.done
	}
	p.cancel()
	<-p.watched
	p.forget()
	return out.result, out.err
}

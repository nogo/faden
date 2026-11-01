package faden

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"syscall"
	"time"
)

// Result is what a finished command left behind. Output holds standard
// output and standard error in the order they were written.
type Result struct {
	Output   []byte
	ExitCode int
}

// Runtime starts a command where the host wants it to run and waits for it.
// A command that exits with a non-zero code is not an error: the code is in
// the Result. An error means the command did not start or was stopped.
//
// The host creates and owns whatever the runtime runs in. A container
// runtime must keep the CLI's own session files across turns.
type Runtime interface {
	Run(ctx context.Context, argv []string) (Result, error)
}

// Local runs commands as processes of the host, in Dir (the host's working
// directory if empty), with the host's environment. A stopped command is
// killed together with the processes it started.
type Local struct {
	Dir string
	// Env replaces the inherited environment when non-nil.
	Env []string
}

// Run implements Runtime.
func (l Local) Run(ctx context.Context, argv []string) (Result, error) {
	var out bytes.Buffer
	res, err := l.run(ctx, argv, nil, &out, &out)
	res.Output = out.Bytes()
	return res, err
}

// RunStream runs a command with stdin and streamed stdout. Stderr is kept in Result.
func (l Local) RunStream(ctx context.Context, argv []string, input io.Reader, output io.Writer) (Result, error) {
	var stderr bytes.Buffer
	res, err := l.run(ctx, argv, input, output, &stderr)
	res.Output = stderr.Bytes()
	return res, err
}

func (l Local) run(ctx context.Context, argv []string, input io.Reader, output, stderr io.Writer) (Result, error) {
	if len(argv) == 0 {
		return Result{}, errors.New("faden: local run: empty command")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = l.Dir
	cmd.Env = l.Env
	cmd.Stdin = input
	cmd.Stdout, cmd.Stderr = output, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return Result{}, fmt.Errorf("faden: local run %s: %w", argv[0], ctx.Err())
	case errors.As(err, &exit):
		return Result{ExitCode: exit.ExitCode()}, nil
	case err != nil:
		return Result{}, fmt.Errorf("faden: local run %s: %w", argv[0], err)
	}
	return Result{}, nil
}

package faden

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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
	// PIDDir is an existing, durable directory owned by one sequential command runner.
	// Reuse it after a restart; never share it with another active Local.
	// The host must sync its creation and keep it across restarts.
	PIDDir string
	Dir    string
	// Env replaces the inherited environment when non-nil.
	Env []string
}

// WithEnv returns a copy of l that also sets the given NAME=value entries.
func (l Local) WithEnv(extra ...string) Local {
	env := l.Env
	if env == nil {
		env = os.Environ()
	}
	l.Env = append(append([]string(nil), env...), extra...)
	return l
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
	path, err := l.pidFile()
	if err != nil {
		return Result{}, err
	}
	if err := stopLocalLeftover(path); err != nil {
		return Result{}, fmt.Errorf("faden: local stop leftover: %w", err)
	}
	program, err := exec.LookPath(argv[0])
	if err != nil {
		return Result{}, fmt.Errorf("faden: local run: %w", err)
	}
	gate, release, err := os.Pipe()
	if err != nil {
		return Result{}, fmt.Errorf("faden: local gate: %w", err)
	}
	defer gate.Close()
	defer release.Close()
	args := append([]string{"-c", `read -r ready <&3 && exec 3<&- && exec "$@"`, "sh", program}, argv[1:]...)
	cmd := exec.CommandContext(ctx, "/bin/sh", args...)
	cmd.ExtraFiles = []*os.File{gate}
	cmd.Dir = l.Dir
	cmd.Env = l.Env
	cmd.Stdin = input
	cmd.Stdout, cmd.Stderr = output, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("faden: local start: %w", err)
	}
	gate.Close()
	if err := recordLocalGroup(path, cmd.Process.Pid); err != nil {
		stopLocalGroup(cmd.Process.Pid)
		cmd.Wait()
		return Result{}, fmt.Errorf("faden: local record process group: %w", err)
	}
	if _, err := io.WriteString(release, "run\n"); err != nil {
		stopLocalGroup(cmd.Process.Pid)
		cmd.Wait()
		return Result{}, fmt.Errorf("faden: local release command: %w", err)
	}
	release.Close()
	err = cmd.Wait()
	if stopErr := stopLocalGroup(cmd.Process.Pid); stopErr != nil {
		return Result{}, fmt.Errorf("faden: local stop group: %w", stopErr)
	}
	if removeErr := os.Remove(path); removeErr != nil {
		return Result{}, fmt.Errorf("faden: local remove pid: %w", removeErr)
	}
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

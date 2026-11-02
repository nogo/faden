package faden

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Result is what a finished command left behind. Output buffers stderr and,
// when Command.Stdout is nil, stdout.
type Result struct {
	Output   []byte
	ExitCode int
}

// Command describes one command belonging to a turn. Env adds to the runtime environment.
// Nil Stdout buffers output in Result.Output; otherwise only stderr is buffered.
type Command struct {
	// Dir is the working directory of the command; empty uses the runtime's own.
	Dir    string
	Key    string
	Argv   []string
	Env    []string
	Stdin  io.Reader
	Stdout io.Writer
}

// Runtime starts a command where the host wants it to run and waits for it.
// A command that exits with a non-zero code is not an error: the code is in
// the Result. An error means the command did not start or was stopped.
//
// The host creates and owns whatever the runtime runs in. A container
// runtime must keep the CLI's own session files across turns.
//
// Each key has one command owner at a time, including Stop during recovery.
type Runtime interface {
	Run(context.Context, Command) (Result, error)
	Stop(context.Context, string) error
}

// Local runs commands as processes of the host, in Dir (the host's working
// directory if empty), with the host's environment. A stopped command is
// killed together with the processes it started.
type Local struct {
	// PIDDir is an existing, durable directory for keyed command records.
	// Reuse it after a restart. Each key has one command owner at a time.
	// The host must sync its creation and keep it across restarts.
	PIDDir string
	Dir    string
	// Env replaces the inherited environment when non-nil.
	Env []string
}

// Run implements Runtime.
func (l Local) Run(ctx context.Context, command Command) (Result, error) {
	var out bytes.Buffer
	output := command.Stdout
	if output == nil {
		output = &out
	}
	res, err := l.run(ctx, command, output, &out)
	res.Output = out.Bytes()
	return res, err
}

// Stop stops the leftover belonging to key.
func (l Local) Stop(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := l.pidFile(key)
	if err != nil {
		return err
	}
	if err := stopLocalLeftover(path); err != nil {
		return fmt.Errorf("faden: local stop key %q: %w", key, err)
	}
	return nil
}

func (l Local) run(ctx context.Context, command Command, output, stderr io.Writer) (Result, error) {
	argv := command.Argv
	if len(argv) == 0 {
		return Result{}, errors.New("faden: local run: empty command")
	}
	path, err := l.pidFile(command.Key)
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
	cmd.Dir = cmp.Or(command.Dir, l.Dir)
	cmd.Env = l.Env
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = append(append([]string(nil), cmd.Env...), command.Env...)
	cmd.Stdin = command.Stdin
	cmd.Stdout, cmd.Stderr = output, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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
		os.Remove(path)
		return Result{}, fmt.Errorf("faden: local release command: %w", err)
	}
	release.Close()
	err = cmd.Wait()
	if stopErr := stopLocalGroup(cmd.Process.Pid); stopErr != nil {
		return Result{}, fmt.Errorf("faden: local stop group: %w", stopErr)
	}
	if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return Result{}, fmt.Errorf("faden: local remove pid: %w", removeErr)
	}
	exit, exited := errors.AsType[*exec.ExitError](err)
	switch {
	case ctx.Err() != nil:
		return Result{}, fmt.Errorf("faden: local run %s: %w", argv[0], ctx.Err())
	case exited:
		return Result{ExitCode: exit.ExitCode()}, nil
	case err != nil:
		return Result{}, fmt.Errorf("faden: local run %s: %w", argv[0], err)
	}
	return Result{}, nil
}

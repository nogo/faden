// Package container runs a CLI inside a container that the host already runs,
// with `podman exec` or `docker exec`. Reach for it when an unattended agent
// must work in a sandbox and still survive a host restart.
//
// The host owns the container: it starts, builds and removes it. This package
// never does.
//
// Claude's own session files, in ~/.claude, must outlive a turn. Put them on a
// volume, or keep the container alive as long as the session. Otherwise
// `claude --resume` finds nothing after a restart.
//
// The start command writes its process id to /tmp/faden/<turn>.pid inside the
// container. Before every start, the runtime kills the process in that file and
// removes it. A turn has one owner at a time, so a pid file found at start
// belongs to an earlier owner that died with the host.
//
// A variable in Env reaches the CLI by name (`--env NAME`). The container
// engine reads the value from its own environment, so the value never appears
// in the process list.
package container

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/nogo/faden"
)

const pidDir = "/tmp/faden"

// Runtime runs commands in one running container for one turn.
type Runtime struct {
	// Engine is "podman" or "docker". Empty means podman if it is on PATH, else docker.
	Engine string
	// Container is the name or id of the running container.
	Container string
	// Turn names the pid file. Use letters, digits, '.', '_' and '-' only.
	Turn string
	// Dir is the working directory inside the container. Empty keeps the image default.
	Dir string
	// Env holds NAME=value entries. Only the names go to the engine's arguments.
	Env []string
}

// Run implements faden.Runtime. Output holds standard output and standard error together.
func (r Runtime) Run(ctx context.Context, argv []string) (faden.Result, error) {
	var out bytes.Buffer
	res, err := r.start(ctx, argv, nil, &out, &out)
	res.Output = out.Bytes()
	return res, err
}

// RunStream runs a command with stdin and streamed stdout. Stderr is kept in Result.
func (r Runtime) RunStream(ctx context.Context, argv []string, input io.Reader, output io.Writer) (faden.Result, error) {
	var stderr bytes.Buffer
	res, err := r.start(ctx, argv, input, output, &stderr)
	res.Output = stderr.Bytes()
	return res, err
}

func (r Runtime) start(ctx context.Context, argv []string, input io.Reader, output, stderr io.Writer) (faden.Result, error) {
	if len(argv) == 0 {
		return faden.Result{}, errors.New("container: empty command")
	}
	if r.Container == "" || !validTurn(r.Turn) {
		return faden.Result{}, fmt.Errorf("container: need a container and a turn of letters, digits, '.', '_' or '-' (turn %q)", r.Turn)
	}
	engine, err := r.engine()
	if err != nil {
		return faden.Result{}, err
	}
	pidFile := pidDir + "/" + r.Turn + ".pid"
	if err := r.killLeftover(ctx, engine, pidFile); err != nil {
		return faden.Result{}, err
	}

	args := []string{"exec", "--interactive"}
	if r.Dir != "" {
		args = append(args, "--workdir", r.Dir)
	}
	for _, kv := range r.Env {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || name == "" {
			return faden.Result{}, fmt.Errorf("container: env entry %q is not NAME=value", name)
		}
		args = append(args, "--env", name)
	}
	args = append(args, r.Container, "sh", "-c", `mkdir -p /tmp/faden && echo $$ > "$1" && shift && exec "$@"`, "sh", pidFile)
	args = append(args, argv...)

	cmd := exec.CommandContext(ctx, engine, args...)
	cmd.Env = append(os.Environ(), r.Env...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = input, output, stderr
	err = cmd.Run()
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		// Stopping the engine client does not stop the process inside the container.
		r.killLeftover(context.WithoutCancel(ctx), engine, pidFile)
		return faden.Result{}, fmt.Errorf("container: run %s: %w", argv[0], ctx.Err())
	case errors.As(err, &exit):
		return faden.Result{ExitCode: exit.ExitCode()}, nil
	case err != nil:
		return faden.Result{}, fmt.Errorf("container: run %s: %w", argv[0], err)
	}
	return faden.Result{}, nil
}

// killLeftover kills the process named in the pid file, if any, and removes the file.
func (r Runtime) killLeftover(ctx context.Context, engine, pidFile string) error {
	script := `[ -f "$1" ] && kill -9 "$(cat "$1")" 2>/dev/null; rm -f "$1"; exit 0`
	out, err := exec.CommandContext(ctx, engine, "exec", r.Container, "sh", "-c", script, "sh", pidFile).CombinedOutput()
	if err != nil {
		return fmt.Errorf("container: stop earlier process in %s: %w: %s", r.Container, err, bytes.TrimSpace(out))
	}
	return nil
}

func (r Runtime) engine() (string, error) {
	switch r.Engine {
	case "podman", "docker":
		return r.Engine, nil
	case "":
		for _, name := range []string{"podman", "docker"} {
			if _, err := exec.LookPath(name); err == nil {
				return name, nil
			}
		}
		return "", errors.New("container: neither podman nor docker is on PATH")
	}
	return "", fmt.Errorf("container: engine %q is not podman or docker", r.Engine)
}

func validTurn(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

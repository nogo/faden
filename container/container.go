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
// Each command records its group leader and Linux start time in a file
// named by the hash of its turn key. Run and Stop recover only that key.
// The container must provide Linux /proc, setsid, awk, and sync with file operands.
//
// A variable in Env reaches the CLI by name (`--env NAME`). The container
// engine reads the value from its own environment, so the value never appears
// in the process list.
package container

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/nogo/faden"
)

const pidDir = "/tmp/faden"

// Runtime runs keyed commands in one running container.
type Runtime struct {
	// Engine is "podman" or "docker". Empty means podman if it is on PATH, else docker.
	Engine string
	// Container is the name or id of the running container.
	Container string
	// Dir is the working directory inside the container. Empty keeps the image default.
	Dir string
	// Env holds NAME=value entries. Only the names go to the engine's arguments.
	Env []string
}

// Run implements faden.Runtime.
func (r Runtime) Run(ctx context.Context, command faden.Command) (faden.Result, error) {
	var out bytes.Buffer
	output := command.Stdout
	if output == nil {
		output = &out
	}
	res, err := r.start(ctx, command, output, &out)
	res.Output = out.Bytes()
	return res, err
}

func (r Runtime) start(ctx context.Context, command faden.Command, output, stderr io.Writer) (faden.Result, error) {
	argv := command.Argv
	if len(argv) == 0 {
		return faden.Result{}, errors.New("container: empty command")
	}
	if r.Container == "" {
		return faden.Result{}, errors.New("container: need a container")
	}
	engine, err := r.engine()
	if err != nil {
		return faden.Result{}, err
	}
	pidFile := recordPath(command.Key)
	if err := r.killLeftover(ctx, engine, pidFile); err != nil {
		return faden.Result{}, err
	}

	args := []string{"exec", "--interactive"}
	if r.Dir != "" {
		args = append(args, "--workdir", r.Dir)
	}
	for _, kv := range append(append([]string(nil), r.Env...), command.Env...) {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || name == "" {
			return faden.Result{}, fmt.Errorf("container: env entry %q is not NAME=value", name)
		}
		args = append(args, "--env", name)
	}
	args = append(args, r.Container, "setsid", "sh", "-c", startScript, "sh", pidFile)
	args = append(args, argv...)

	cmd := exec.CommandContext(ctx, engine, args...)
	cmd.Env = append(append(os.Environ(), r.Env...), command.Env...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = command.Stdin, output, stderr
	err = cmd.Run()
	if stopErr := r.killLeftover(context.WithoutCancel(ctx), engine, pidFile); stopErr != nil {
		return faden.Result{}, stopErr
	}
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
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
	script := stopScript
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

// recordPath hashes arbitrary request IDs into safe file names.
func recordPath(key string) string {
	return fmt.Sprintf("%s/%x.pid", pidDir, sha256.Sum256([]byte(key)))
}

// Stop stops only the group whose identity still matches this key's record.
func (r Runtime) Stop(ctx context.Context, key string) error {
	if r.Container == "" {
		return errors.New("container: need a container")
	}
	engine, err := r.engine()
	if err != nil {
		return err
	}
	return r.killLeftover(ctx, engine, recordPath(key))
}

// The shell remains the group leader while its command runs.
const startScript = `mkdir -p /tmp/faden || exit 1
file=$1; shift
stat=$(cat /proc/$$/stat) || exit 1
stat=${stat##*) }
start=$(printf '%s\n' "$stat" | awk '{print $20}')
tmp="$file.$$"
(umask 077; printf '%s %s\n' "$$" "$start" > "$tmp") &&
sync "$tmp" && mv "$tmp" "$file" && sync /tmp/faden || exit 1
exec 3<&0
"$@" <&3 &
exec 3<&-
child=$!
wait "$child"
status=$?
rm -f "$file"
exit "$status"`

const stopScript = `if [ -f "$1" ]; then
read -r pid start extra < "$1"
case "$pid" in ''|*[!0-9]*) ;; *)
 if [ "$pid" -gt 1 ] && [ -n "$start" ] && [ -z "$extra" ] && [ -r "/proc/$pid/stat" ]; then
  stat=$(cat "/proc/$pid/stat")
  stat=${stat##*) }
  actual=$(printf '%s\n' "$stat" | awk '{print $20}')
  if [ "$actual" = "$start" ]; then kill -9 "-$pid" 2>/dev/null; fi
 fi;;
esac
rm -f "$1"
fi`

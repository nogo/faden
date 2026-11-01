package container_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nogo/faden"
	"github.com/nogo/faden/container"
)

// fakeEngine puts a program named podman on PATH. It appends its arguments to
// a log, one invocation per line with fields joined by a space, and exits with
// the code in $FAKE_EXIT for the start command.
func fakeEngine(t *testing.T) (logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "calls.log")
	script := fakeScript
	if err := os.WriteFile(filepath.Join(dir, "podman"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_LOG", logPath)
	return logPath
}

func calls(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func TestRunKillsEarlierProcessBeforeStarting(t *testing.T) {
	log := fakeEngine(t)
	r := container.Runtime{Container: "box", Dir: "/work"}
	if _, err := r.Run(t.Context(), faden.Command{Key: "test", Argv: []string{"claude", "--resume", "abc"}}); err != nil {
		t.Fatal(err)
	}
	got := calls(t, log)
	if len(got) != 3 {
		t.Fatalf("calls = %q, want kill then start", got)
	}
	if !strings.Contains(got[0], "kill -9") || !strings.HasSuffix(got[0], "/tmp/faden/9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08.pid") {
		t.Errorf("first call = %q, want the kill of /tmp/faden/9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08.pid", got[0])
	}
	start := got[1]
	for _, want := range []string{"exec --interactive --workdir /work box setsid sh -c", "sync \"$tmp\" && mv \"$tmp\" \"$file\"", "sh /tmp/faden/9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08.pid claude --resume abc"} {
		if !strings.Contains(start, want) {
			t.Errorf("start call %q lacks %q", start, want)
		}
	}
}

func TestEnvValueNeverReachesArguments(t *testing.T) {
	log := fakeEngine(t)
	const secret = "sk-test-secret-value"
	r := container.Runtime{Container: "box"}
	if _, err := r.Run(t.Context(), faden.Command{Key: "test", Env: []string{"API_TOKEN=" + secret}, Argv: []string{"claude"}}); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(calls(t, log), "\n")
	if strings.Contains(all, secret) {
		t.Fatalf("secret in arguments:\n%s", all)
	}
	if !strings.Contains(all, "--env API_TOKEN ") {
		t.Fatalf("want --env API_TOKEN in:\n%s", all)
	}
}

func TestRunReportsExitCodeAsResult(t *testing.T) {
	fakeEngine(t)
	t.Setenv("FAKE_EXIT", "7")
	res, err := container.Runtime{Container: "box"}.Run(t.Context(), faden.Command{Key: "test", Argv: []string{"claude"}})
	if err != nil || res.ExitCode != 7 {
		t.Fatalf("result = %+v, err = %v, want exit 7", res, err)
	}
}

func TestRunHashesArbitraryKeys(t *testing.T) {
	fakeEngine(t)
	for _, turn := range []string{"", "../x", "a b", "a/b"} {
		if _, err := (container.Runtime{Container: "box"}).Run(t.Context(), faden.Command{Key: turn, Argv: []string{"x"}}); err != nil {
			t.Errorf("turn %q: %v", turn, err)
		}
	}
}

const fakeScript = `#!/bin/sh
printf '%s' "$*" | tr '\n' ' ' >> "$FAKE_LOG"
printf '\n' >> "$FAKE_LOG"
case "$*" in *setsid*) exit "${FAKE_EXIT:-0}";; esac
exit 0
`

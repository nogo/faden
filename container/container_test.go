package container_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nogo/faden/container"
)

// fakeEngine puts a program named podman on PATH. It appends its arguments to
// a log, one invocation per line with fields joined by a space, and exits with
// the code in $FAKE_EXIT for the start command.
func fakeEngine(t *testing.T) (logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\necho \"$*\" >> \"$FAKE_LOG\"\ncase \"$*\" in *'exec $$'*|*'echo $$'*) exit \"${FAKE_EXIT:-0}\";; esac\nexit 0\n"
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
	r := container.Runtime{Container: "box", Turn: "t1", Dir: "/work"}
	if _, err := r.Run(t.Context(), []string{"claude", "--resume", "abc"}); err != nil {
		t.Fatal(err)
	}
	got := calls(t, log)
	if len(got) != 2 {
		t.Fatalf("calls = %q, want kill then start", got)
	}
	if !strings.Contains(got[0], "kill -9") || !strings.HasSuffix(got[0], "/tmp/faden/t1.pid") {
		t.Errorf("first call = %q, want the kill of /tmp/faden/t1.pid", got[0])
	}
	start := got[1]
	for _, want := range []string{"exec --interactive --workdir /work box sh -c", "echo $$ > \"$1\"", "sh /tmp/faden/t1.pid claude --resume abc"} {
		if !strings.Contains(start, want) {
			t.Errorf("start call %q lacks %q", start, want)
		}
	}
}

func TestEnvValueNeverReachesArguments(t *testing.T) {
	log := fakeEngine(t)
	const secret = "sk-test-secret-value"
	r := container.Runtime{Container: "box", Turn: "t2", Env: []string{"API_TOKEN=" + secret}}
	if _, err := r.Run(t.Context(), []string{"claude"}); err != nil {
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
	res, err := container.Runtime{Container: "box", Turn: "t3"}.Run(t.Context(), []string{"claude"})
	if err != nil || res.ExitCode != 7 {
		t.Fatalf("result = %+v, err = %v, want exit 7", res, err)
	}
}

func TestRunRejectsUnsafeTurn(t *testing.T) {
	fakeEngine(t)
	for _, turn := range []string{"", "../x", "a b", "a/b"} {
		if _, err := (container.Runtime{Container: "box", Turn: turn}).Run(t.Context(), []string{"x"}); err == nil {
			t.Errorf("turn %q accepted", turn)
		}
	}
}

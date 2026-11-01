package faden_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nogo/faden"
)

func TestLocalRunsInDirAndReportsExitCode(t *testing.T) {
	dir := t.TempDir()
	res, err := faden.Local{Dir: dir}.Run(t.Context(), []string{"sh", "-c", "pwd; echo err >&2; exit 4"})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(dir)
	if res.ExitCode != 4 || !strings.Contains(string(res.Output), want) || !strings.Contains(string(res.Output), "err") {
		t.Fatalf("result = %d %q", res.ExitCode, res.Output)
	}
}

func TestLocalKillsProcessGroupWhenStopped(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := faden.Local{}.Run(ctx, []string{"sh", "-c", "sleep 30 & wait"})
	if err == nil || time.Since(start) > 10*time.Second {
		t.Fatalf("err = %v after %v, want a stop", err, time.Since(start))
	}
}

func TestLocalReportsMissingProgram(t *testing.T) {
	if _, err := (faden.Local{}).Run(t.Context(), []string{"faden-no-such-program"}); err == nil {
		t.Fatal("want an error for a program that does not exist")
	}
}

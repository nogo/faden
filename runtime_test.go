package faden_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nogo/faden"
)

func TestLocalRunsInDirAndReportsExitCode(t *testing.T) {
	dir := t.TempDir()
	res, err := faden.Local{PIDDir: dir, Dir: dir}.Run(t.Context(), []string{"sh", "-c", "pwd; echo err >&2; exit 4"})
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
	_, err := faden.Local{PIDDir: t.TempDir()}.Run(ctx, []string{"sh", "-c", "sleep 30 & wait"})
	if err == nil || time.Since(start) > 10*time.Second {
		t.Fatalf("err = %v after %v, want a stop", err, time.Since(start))
	}
}

func TestLocalReportsMissingProgram(t *testing.T) {
	if _, err := (faden.Local{PIDDir: t.TempDir()}).Run(t.Context(), []string{"faden-no-such-program"}); err == nil {
		t.Fatal("want an error for a program that does not exist")
	}
}

func TestLocalSyncsGroupRecordBeforeCommandRuns(t *testing.T) {
	dir := t.TempDir()
	result, err := (faden.Local{PIDDir: dir, Dir: dir}).Run(t.Context(), []string{"sh", "-c", `test "$(cat process.pid)" = "$$"`})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("record not visible before exec: %+v, %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "process.pid")); !os.IsNotExist(err) {
		t.Fatalf("completed command left pid file: %v", err)
	}
}

func TestLocalRequiresPIDDirectory(t *testing.T) {
	if _, err := (faden.Local{}).Run(t.Context(), []string{"true"}); err == nil {
		t.Fatal("missing PIDDir accepted")
	}
}

func TestLocalRejectsInvalidGroupRecord(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "process.pid"), []byte("0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := (faden.Local{PIDDir: dir}).Run(t.Context(), []string{"touch", filepath.Join(dir, "ran")}); err == nil {
		t.Fatal("invalid process group accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, "ran")); !os.IsNotExist(err) {
		t.Fatalf("command ran: %v", err)
	}
}

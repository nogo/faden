package faden_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nogo/faden"
)

func TestLocalRunsInDirAndReportsExitCode(t *testing.T) {
	dir := t.TempDir()
	res, err := faden.Local{PIDDir: dir, Dir: dir}.Run(t.Context(), faden.Command{Key: "test", Argv: []string{"sh", "-c", "pwd; echo err >&2; exit 4"}})
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
	_, err := faden.Local{PIDDir: t.TempDir()}.Run(ctx, faden.Command{Key: "test", Argv: []string{"sh", "-c", "sleep 30 & wait"}})
	if err == nil || time.Since(start) > 10*time.Second {
		t.Fatalf("err = %v after %v, want a stop", err, time.Since(start))
	}
}

func TestLocalReportsMissingProgram(t *testing.T) {
	if _, err := (faden.Local{PIDDir: t.TempDir()}).Run(t.Context(), faden.Command{Key: "test", Argv: []string{"faden-no-such-program"}}); err == nil {
		t.Fatal("want an error for a program that does not exist")
	}
}

func TestLocalSyncsGroupRecordBeforeCommandRuns(t *testing.T) {
	dir := t.TempDir()
	result, err := (faden.Local{PIDDir: dir, Dir: dir}).Run(t.Context(), faden.Command{Key: "test", Argv: []string{"sh", "-c", fmt.Sprintf(`read -r pid start < %s; test "$pid" = "$$" && test -n "$start"`, keyRecord(dir, "test"))}})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("record not visible before exec: %+v, %v", result, err)
	}
	if _, err := os.Stat(keyRecord(dir, "test")); !os.IsNotExist(err) {
		t.Fatalf("completed command left pid file: %v", err)
	}
}

func TestLocalRequiresPIDDirectory(t *testing.T) {
	if _, err := (faden.Local{}).Run(t.Context(), faden.Command{Key: "test", Argv: []string{"true"}}); err == nil {
		t.Fatal("missing PIDDir accepted")
	}
}

func TestLocalCommandDirOverridesRuntimeDir(t *testing.T) {
	runtimeDir, commandDir := t.TempDir(), t.TempDir()
	res, err := faden.Local{PIDDir: t.TempDir(), Dir: runtimeDir}.Run(t.Context(), faden.Command{Key: "test", Dir: commandDir, Argv: []string{"pwd"}})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(commandDir)
	if strings.TrimSpace(string(res.Output)) != want {
		t.Fatalf("ran in %q, want %q", res.Output, want)
	}
}

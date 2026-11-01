package container

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestScriptsSyncRecordStreamAndRemoveOnCompletion(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "turn.pid")
	script := strings.ReplaceAll(startScript, pidDir, dir)
	cmd := exec.CommandContext(t.Context(), "setsid", "sh", "-c", script, "sh", record, "sh", "-c", `read -r line; read -r pid start < "$1"; test "$pid" = "$PPID" && test -n "$start" || exit 8; echo "$line"; echo error >&2; exit 7`, "sh", record)
	cmd.Stdin = strings.NewReader("input\n")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 7 || stdout.String() != "input\n" || stderr.String() != "error\n" {
		t.Fatalf("shell: %v, stdout %q, stderr %q", err, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Fatalf("record remains: %v", err)
	}
}

func TestStopScriptChecksIdentityAndStopsGroup(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "turn.pid")
	ready, signal, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer ready.Close()
	defer signal.Close()
	cmd := exec.Command("setsid", "sh", "-c", strings.ReplaceAll(startScript, pidDir, dir), "sh", record, "sh", "-c", "echo ready; sleep 600 & wait")
	cmd.Stdout = signal
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); cmd.Wait() }()
	var data [6]byte
	if _, err := ready.Read(data[:]); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	// The same PID with a different start time belongs to another process.
	if err := os.WriteFile(record, []byte(fmt.Sprintf("%d 0\n", cmd.Process.Pid)), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.CommandContext(t.Context(), "sh", "-c", stopScript, "sh", record).CombinedOutput(); err != nil {
		t.Fatalf("stop stale: %v: %s", err, out)
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", cmd.Process.Pid))
	if err != nil || strings.Contains(string(stat), ") Z ") {
		t.Fatalf("stale identity killed process: %s, %v", stat, err)
	}
	if err := os.WriteFile(record, original, 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.CommandContext(t.Context(), "sh", "-c", stopScript, "sh", record).CombinedOutput(); err != nil {
		t.Fatalf("stop: %v: %s", err, out)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("group was not killed")
		}
	case <-ctx.Done():
		t.Fatal("group still running")
	}
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Fatalf("record remains: %v", err)
	}
}

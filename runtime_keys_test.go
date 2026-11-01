package faden_test

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/nogo/faden"
)

func keyRecord(dir, key string) string {
	return filepath.Join(dir, fmt.Sprintf("%x.pid", sha256.Sum256([]byte(key))))
}

func TestLocalConcurrentKeysBothFinish(t *testing.T) {
	dir := t.TempDir()
	local := faden.Local{PIDDir: dir, Dir: dir}
	input, release, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer release.Close()
	done := make(chan faden.Result, 1)
	failures := make(chan error, 1)
	go func() {
		result, err := local.Run(t.Context(), faden.Command{Key: "first", Argv: []string{"sh", "-c", "touch ready; read -r line; echo survived"}, Stdin: input})
		failures <- err
		done <- result
	}()
	pollLocal(t, func() bool { _, err := os.Stat(filepath.Join(dir, "ready")); return err == nil })
	second, err := local.Run(t.Context(), faden.Command{Key: "second", Argv: []string{"echo", "second"}})
	if err != nil || second.ExitCode != 0 || string(second.Output) != "second\n" {
		t.Fatalf("second: %+v, %v", second, err)
	}
	fmt.Fprintln(release, "continue")
	first := <-done
	if err := <-failures; err != nil || first.ExitCode != 0 || string(first.Output) != "survived\n" {
		t.Fatalf("first: %+v, %v", first, err)
	}
	for _, key := range []string{"first", "second"} {
		if _, err := os.Stat(keyRecord(dir, key)); !os.IsNotExist(err) {
			t.Fatalf("record remains: %v", err)
		}
	}
}

func TestLocalTornRecordDoesNotBlockStart(t *testing.T) {
	for _, data := range []string{"", "123", "123 "} {
		t.Run(fmt.Sprintf("record-%q", data), func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(keyRecord(dir, "turn"), []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			result, err := (faden.Local{PIDDir: dir}).Run(t.Context(), faden.Command{Key: "turn", Argv: []string{"echo", "ran"}})
			if err != nil || result.ExitCode != 0 || string(result.Output) != "ran\n" {
				t.Fatalf("result: %+v, %v", result, err)
			}
		})
	}
}

func TestLocalDoesNotKillReusedPID(t *testing.T) {
	dir := t.TempDir()
	input, release, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer release.Close()
	var output bytes.Buffer
	other := exec.Command("sh", "-c", "read -r line; echo survived")
	other.Stdin = input
	other.Stdout = &output
	other.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { other.Process.Kill(); other.Wait() }()
	// Start time zero cannot identify this live process.
	if err := os.WriteFile(keyRecord(dir, "turn"), []byte(fmt.Sprintf("%d 0\n", other.Process.Pid)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := (faden.Local{PIDDir: dir}).Stop(t.Context(), "turn"); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(release, "continue")
	if err := other.Wait(); err != nil || output.String() != "survived\n" {
		t.Fatalf("unrelated process stopped: %v, %q", err, output.String())
	}
}

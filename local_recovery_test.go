package faden_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nogo/faden"
)

func localRecoveryBackend(dir string) *fake {
	return &fake{run: func(ctx context.Context, turn faden.Turn, _ json.RawMessage, _ func(faden.Event) error) (json.RawMessage, error) {
		argv := []string{"sh", "-c", `echo $$ > child; sleep 600 & echo $! > grandchild; wait`}
		if turn.Attempts > 1 {
			argv = []string{"true"}
		}
		_, err := (faden.Local{PIDDir: dir, Dir: dir}).Run(ctx, faden.Command{Key: turn.RequestID, Argv: argv})
		return nil, err
	}}
}

func TestLocalRecoveryHost(t *testing.T) {
	dir := os.Getenv("FADEN_LOCAL_RECOVERY")
	if dir == "" {
		return
	}
	store := openStore(t, filepath.Join(dir, "store"))
	runner, err := faden.Open(store, map[string]faden.Backend{"fake": localRecoveryBackend(dir)})
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	if _, err := runner.Submit(t.Context(), sub("s", "r", "invented")); err != nil {
		t.Fatal(err)
	}
	wait(t, runner, "r")
}

func pollLocal(t *testing.T, condition func() bool) {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for !condition() {
		select {
		case <-timer.C:
			t.Fatal("condition did not become true")
		case <-tick.C:
		}
	}
}

func TestLocalKillsDeadHostsGroupBeforeContinuation(t *testing.T) {
	dir := t.TempDir()
	host := exec.Command(os.Args[0], "-test.run=^TestLocalRecoveryHost$")
	host.Env = append(os.Environ(), "FADEN_LOCAL_RECOVERY="+dir)
	host.Stderr = os.Stderr
	if err := host.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { host.Process.Kill(); host.Wait() })
	var pids []string
	for _, name := range []string{"child", "grandchild"} {
		var data []byte
		pollLocal(t, func() bool {
			data, _ = os.ReadFile(filepath.Join(dir, name))
			return len(strings.TrimSpace(string(data))) > 0
		})
		pid := strings.TrimSpace(string(data))
		pids = append(pids, pid)
		t.Cleanup(func() { var n int; fmt.Sscan(pid, &n); syscall.Kill(n, syscall.SIGKILL) })
	}
	if err := host.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	host.Wait()
	store := openStore(t, filepath.Join(dir, "store"))
	runner, err := faden.Open(store, map[string]faden.Backend{"fake": localRecoveryBackend(dir)})
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	turn := wait(t, runner, "r")
	if turn.Status != faden.Done || turn.Attempts != 2 {
		t.Fatalf("turn = %+v", turn)
	}
	for _, pid := range pids {
		pollLocal(t, func() bool {
			data, err := os.ReadFile("/proc/" + pid + "/stat")
			return os.IsNotExist(err) || err == nil && strings.Contains(string(data), ") Z ")
		})
	}
}

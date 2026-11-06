package claude

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nogo/faden"
)

func TestSteerWaitsForLastResult(t *testing.T) {
	dir := t.TempDir()
	program := `#!/bin/sh
IFS= read -r first
printf '%s\n' '{"type":"assistant","message":{"id":"first","content":[{"type":"text","text":"ready"}]}}'
IFS= read -r second
printf '%s\n' "$second" > second
printf '%s\n' '{"type":"result","result":"first answer"}'
printf '%s\n' '{"type":"assistant","message":{"id":"second","content":[{"type":"text","text":"waiting"}]}}'
IFS= read -r gate < gate
printf '%s\n' '{"type":"result","result":"last answer"}'
cat > /dev/null
`
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(program), 0700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "gate"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	store := openStore(t, filepath.Join(dir, "store"))
	b, err := New(Config{Runtime: faden.Local{Dir: dir, PIDDir: dir}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := faden.Open(store, map[string]faden.Backend{"claude": b})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := r.Submit(ctx, faden.Submission{Session: "s", RequestID: "r", Input: "work", Backend: "claude"}); err != nil {
		t.Fatal(err)
	}
	watch, err := r.Watch(ctx, "s", 0)
	if err != nil {
		t.Fatal(err)
	}
	for item := range watch {
		if item.Event == nil {
			continue
		}
		switch item.Event.Text {
		case "ready":
			if _, err := r.Steer(ctx, "r", "use blue"); err != nil {
				t.Fatal(err)
			}
		case "waiting":
			turn, err := store.Turn(ctx, "r")
			if err != nil || turn.Status != faden.Running {
				t.Fatalf("after first result: %+v, %v", turn, err)
			}
			data, err := os.ReadFile(filepath.Join(dir, "second"))
			if err != nil || !strings.Contains(string(data), "use blue") {
				t.Fatalf("stdin = %s, %v", data, err)
			}
			gate, err := os.OpenFile(filepath.Join(dir, "gate"), os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			_, err = gate.WriteString("go\n")
			gate.Close()
			if err != nil {
				t.Fatal(err)
			}
		case "last answer":
			turn, err := r.Wait(ctx, "r")
			if err != nil || turn.Status != faden.Done {
				t.Fatalf("Wait = %+v, %v", turn, err)
			}
			return
		case "first answer":
			t.Fatal("intermediate result ended turn")
		}
	}
	t.Fatal("stream ended before last answer")
}

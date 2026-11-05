package faden_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/nogo/faden"
	"github.com/nogo/faden/claude"
	"github.com/nogo/faden/codex"
)

type fragmentRuntime struct {
	start, finish chan struct{}
	claude        bool
}

func (*fragmentRuntime) Stop(context.Context, string) error { return nil }

func (r *fragmentRuntime) Run(ctx context.Context, cmd faden.Command) (faden.Result, error) {
	scan := bufio.NewScanner(cmd.Stdin)
	for scan.Scan() {
		line := scan.Text()
		if !r.claude {
			switch {
			case strings.Contains(line, `"method":"initialize"`):
				fmt.Fprintln(cmd.Stdout, `{"id":1,"result":{}}`)
				continue
			case strings.Contains(line, `"method":"thread/start"`):
				fmt.Fprintln(cmd.Stdout, `{"id":2,"result":{"thread":{"id":"invented"}}}`)
				continue
			case !strings.Contains(line, `"method":"turn/start"`):
				continue
			}
		} else if !strings.Contains(strings.Join(cmd.Argv, " "), "--include-partial-messages") {
			return faden.Result{}, fmt.Errorf("partial messages were not enabled")
		}
		select {
		case <-r.start:
		case <-ctx.Done():
			return faden.Result{}, ctx.Err()
		}
		if r.claude {
			io.WriteString(cmd.Stdout, "{\"type\":\"stream_event\",\"event\":{\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"live\"}}}\n")
			io.WriteString(cmd.Stdout, "{\"type\":\"stream_event\",\"event\":{\"type\":\"content_block_delta\",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"thought\"}}}\n")
		} else {
			fmt.Fprintln(cmd.Stdout, `{"method":"item/agentMessage/delta","params":{"delta":"live"}}`)
		}
		select {
		case <-r.finish:
		case <-ctx.Done():
			return faden.Result{}, ctx.Err()
		}
		if r.claude {
			fmt.Fprintln(cmd.Stdout, `{"type":"result","result":"done"}`)
		} else {
			fmt.Fprintln(cmd.Stdout, `{"method":"turn/completed","params":{"turn":{"status":"completed"}}}`)
		}
		return faden.Result{}, nil
	}
	return faden.Result{}, scan.Err()
}

func TestWatchBackendFragments(t *testing.T) {
	for _, name := range []string{"claude", "codex"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			store := openStore(t, t.TempDir())
			rt := &fragmentRuntime{start: make(chan struct{}), finish: make(chan struct{}), claude: name == "claude"}
			var b faden.Backend
			var err error
			if rt.claude {
				b, err = claude.New(claude.Config{Runtime: rt})
			} else {
				b, err = codex.New(codex.Config{Runtime: rt})
			}
			if err != nil {
				t.Fatal(err)
			}
			r, err := faden.Open(store, map[string]faden.Backend{name: b})
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			_, err = r.Submit(ctx, faden.Submission{Session: "s", RequestID: "r", Backend: name, Input: "hello"})
			if err != nil {
				t.Fatal(err)
			}
			updates, err := r.Watch(ctx, "s", 0)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case u := <-updates:
				if u.Event == nil || u.Event.Kind != faden.UserMessage {
					t.Fatalf("first update: %+v", u)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			close(rt.start)
			kinds := []faden.FragmentKind{faden.FragmentText}
			if rt.claude {
				kinds = append(kinds, faden.FragmentThinking)
			}
			for _, kind := range kinds {
				select {
				case u := <-updates:
					if u.Fragment == nil || u.Fragment.RequestID != "r" || u.Fragment.Kind != kind || u.Fragment.Text == "" {
						t.Fatalf("fragment: %+v", u)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			close(rt.finish)
			if turn, err := r.Wait(ctx, "r"); err != nil || turn.Status != faden.Done {
				t.Fatalf("turn: %+v %v", turn, err)
			}
			events, err := faden.CollectEvents(store.Events(ctx, "s", 0))
			if err != nil || len(events) != 2 {
				t.Fatalf("stored: %+v %v", events, err)
			}
		})
	}
}

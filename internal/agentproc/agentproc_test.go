package agentproc

import (
	"bufio"
	"context"
	"strings"
	"testing"

	"github.com/nogo/faden"
)

// echo is a runtime whose command writes back each line it reads, until its
// stdin closes or it is stopped.
type echo struct{}

func (echo) Run(ctx context.Context, c faden.Command) (faden.Result, error) {
	lines := make(chan string)
	go func() {
		defer close(lines)
		scan := bufio.NewScanner(c.Stdin)
		for scan.Scan() {
			select {
			case lines <- scan.Text():
			case <-ctx.Done():
				return
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return faden.Result{}, ctx.Err()
		case line, ok := <-lines:
			if !ok {
				return faden.Result{}, nil
			}
			c.Stdout.Write([]byte(line + "\n"))
		}
	}
}

func (echo) Stop(context.Context, string) error { return nil }

func TestReadEndsWhenTheTurnEndsAndCloseForgetsTheProcess(t *testing.T) {
	var r Running
	p, err := r.Start(t.Context(), echo{}, faden.Command{Key: "r1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"a", "end"} {
		if err := p.Send(m); err != nil {
			t.Fatal(err)
		}
	}
	var seen []string
	ended, err := p.Read(func(line []byte) (bool, error) {
		seen = append(seen, string(line))
		return string(line) == `"end"`, nil
	})
	if !ended || err != nil || strings.Join(seen, ",") != `"a","end"` {
		t.Fatalf("ended %v, err %v, lines %v", ended, err, seen)
	}
	if _, err := p.Close(true); err != nil {
		t.Fatal(err)
	}
	if len(r.procs) != 0 {
		t.Fatalf("%d processes still held after Close", len(r.procs))
	}
}

// sink is a runtime whose command reads its stdin to the end, even after a
// stop, and reports the lines it read.
type sink struct{ got chan []string }

func (k sink) Run(ctx context.Context, c faden.Command) (faden.Result, error) {
	var lines []string
	scan := bufio.NewScanner(c.Stdin)
	for scan.Scan() {
		lines = append(lines, scan.Text())
	}
	k.got <- lines
	return faden.Result{ExitCode: 1}, ctx.Err()
}

func (sink) Stop(context.Context, string) error { return nil }

func TestInterruptSendsItsMessageThenStops(t *testing.T) {
	var r Running
	k := sink{got: make(chan []string, 1)}
	p, err := r.Start(t.Context(), k, faden.Command{Key: "r1"}, func() any { return "cancel" })
	if err != nil {
		t.Fatal(err)
	}
	r.Interrupt("r1")
	r.Interrupt("r1") // only the first call sends
	if _, err := p.Close(false); err == nil {
		t.Fatal("a stopped process reported no error")
	}
	if got := strings.Join(<-k.got, ","); got != `"cancel"` {
		t.Fatalf("agent read %q, want the interrupt message", got)
	}
}

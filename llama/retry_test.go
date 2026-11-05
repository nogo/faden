package llama_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nogo/faden"
	"github.com/nogo/faden/llama"
)

func TestHostedHeaderAndRetryStatus(t *testing.T) {
	for _, status := range []int{400, 429, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer invented-token" {
					t.Error("wrong path or authorization")
				}
				if calls.Add(1) == 1 {
					if status == 429 {
						w.Header().Set("Retry-After", "2")
					}
					w.WriteHeader(status)
					io.WriteString(w, "invented-token")
					return
				}
				io.WriteString(w, `{"choices":[{"message":{"content":"done"}}]}`)
			}))
			defer srv.Close()
			store := openStore(t, t.TempDir())
			b, err := llama.New(llama.Config{URL: srv.URL + "/api/v1/", Key: "invented-token"})
			if err != nil {
				t.Fatal(err)
			}
			waits := llama.RecordWaits(b)
			got := run(t, store, b, "r1", "hi")
			if strings.Contains(got.Error, "invented-token") {
				t.Fatal("key in error")
			}
			wantCalls, wantStatus := int32(2), faden.Done
			if status == 400 {
				wantCalls, wantStatus = 1, faden.Failed
			}
			if calls.Load() != wantCalls || got.Status != wantStatus {
				t.Fatalf("calls=%d turn=%+v", calls.Load(), got)
			}
			if w := waits(); status == 429 && (len(w) != 1 || w[0] != 2*time.Second) {
				t.Fatalf("waits = %v, want the 2s of Retry-After", w)
			}
			events, err := faden.CollectEvents(store.Events(t.Context(), "s1", 0))
			if err != nil {
				t.Fatal(err)
			}
			answers := 0
			for _, e := range events {
				if e.Kind == faden.AssistantMessage {
					answers++
				}
				if strings.Contains(e.Text+string(e.Native), "invented-token") {
					t.Fatal("key stored")
				}
			}
			wantAnswers := 1
			if status == 400 {
				wantAnswers = 0
			}
			if answers != wantAnswers {
				t.Fatalf("stored %d answers", answers)
			}
		})
	}
}

func TestRetryRefusedConnection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("unexpected authorization")
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"done"}}]}`)
	}))
	defer srv.Close()
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := closed.Addr().String()
	closed.Close()
	var calls atomic.Int32
	transport := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		if calls.Add(1) == 1 {
			addr = address
		}
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}
	defer transport.CloseIdleConnections()
	b, err := llama.New(llama.Config{URL: srv.URL + "/v1", Client: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	llama.RecordWaits(b)
	got := run(t, openStore(t, t.TempDir()), b, "r1", "hi")
	if got.Status != faden.Done || calls.Load() != 2 {
		t.Fatalf("calls=%d turn=%+v", calls.Load(), got)
	}
}

type failingTransport func(*http.Request) (*http.Response, error)

func (f failingTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCancellationStopsRetryAfter(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(503)
		cancel()
	}))
	defer srv.Close()
	store := openStore(t, t.TempDir())
	turn, err := store.SubmitTurn(t.Context(), faden.Turn{Session: "s1", RequestID: "r1", Backend: "llama"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(t.Context(), faden.Event{Session: "s1", RequestID: "r1", Kind: faden.UserMessage, Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	a, err := faden.NewAttempt(t.Context(), store, turn)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := llama.New(llama.Config{URL: srv.URL + "/v1"})
	start := time.Now()
	if err := b.Run(ctx, turn, a); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	if calls.Load() != 1 || time.Since(start) > time.Second {
		t.Fatal("cancellation did not stop retry")
	}
}

func TestTransportErrorRedactsKey(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: failingTransport(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("invented-token")
	})}
	b, _ := llama.New(llama.Config{URL: "http://example.invalid/v1", Key: "invented-token", Client: client})
	waits := llama.RecordWaits(b)
	got := run(t, openStore(t, t.TempDir()), b, "r1", "hi")
	if got.Status != faden.Failed || strings.Contains(got.Error, "invented-token") {
		t.Fatalf("turn=%+v", got)
	}
	if calls.Load() != 4 {
		t.Fatalf("calls=%d, want 4", calls.Load())
	}
	if w := waits(); !slices.Equal(w, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}) {
		t.Fatalf("waits = %v, want 1s, 2s, 4s", w)
	}
}

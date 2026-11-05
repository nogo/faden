package llama_test

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nogo/faden"
	"github.com/nogo/faden/llama"
)

func TestQuotaAfterRetriesCarriesRetryAfter(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	b, err := llama.New(llama.Config{URL: srv.URL + "/v1"})
	if err != nil {
		t.Fatal(err)
	}
	llama.RecordWaits(b)
	before := time.Now()
	got := run(t, openStore(t, t.TempDir()), b, "r1", "hi")
	if got.Status != faden.Failed || got.Failure != faden.FailureQuota {
		t.Fatalf("turn = %+v", got)
	}
	if got.RetryAt.Before(before) || got.RetryAt.After(time.Now().Add(time.Second)) {
		t.Fatalf("RetryAt = %v, run began %v", got.RetryAt, before)
	}
}

func TestOtherServerErrorIsNotAKind(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	b, err := llama.New(llama.Config{URL: srv.URL + "/v1"})
	if err != nil {
		t.Fatal(err)
	}
	got := run(t, openStore(t, t.TempDir()), b, "r1", "hi")
	if got.Status != faden.Failed || got.Failure != "" || !got.RetryAt.IsZero() {
		t.Fatalf("turn = %+v", got)
	}
}

func TestRefusedConnectionAfterRetriesIsUnavailable(t *testing.T) {
	t.Parallel()
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	url := "http://" + closed.Addr().String() + "/v1"
	closed.Close()
	b, err := llama.New(llama.Config{URL: url})
	if err != nil {
		t.Fatal(err)
	}
	llama.RecordWaits(b)
	got := run(t, openStore(t, t.TempDir()), b, "r1", "hi")
	if got.Status != faden.Failed || got.Failure != faden.FailureUnavailable {
		t.Fatalf("turn = %+v", got)
	}
}

package llama

import (
	"net/http"
	"testing"
	"time"
)

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"3", 3 * time.Second},
		{now.Add(5 * time.Second).Format(http.TimeFormat), 5 * time.Second},
		{now.Add(-time.Second).Format(http.TimeFormat), 0},
		{"invalid", 0}, {"-1", 0}, {"3junk", 0},
	} {
		if got := retryAfter(tc.value, now); got != tc.want {
			t.Errorf("retryAfter(%q)=%v, want %v", tc.value, got, tc.want)
		}
	}
}

package codex

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/nogo/faden"
)

func TestUsageLimitIsQuotaWithReset(t *testing.T) {
	for name, limit := range map[string]string{
		"exec shape":       `{"rate_limits":{"primary":{"used_percent":100,"resets_at":1800000000}}}`,
		"app-server shape": `{"rateLimits":{"primary":{"usedPercent":100,"resetsAt":1800000000}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			limits := &limitState{}
			limits.note(message{Method: "account/rateLimits/updated", Params: json.RawMessage(limit)})
			failed := message{Method: "turn/completed", Params: json.RawMessage(`{"turn":{"status":"failed","error":{"message":"You've hit your usage limit.","codexErrorInfo":"usageLimitExceeded"}}}`)}
			ended, err := notification(failed, func(faden.Event) error { return nil }, limits)
			var quota *faden.QuotaError
			if !ended || !errors.As(err, &quota) || !quota.ResetsAt.Equal(time.Unix(1800000000, 0)) || quota.LimitType != "primary" {
				t.Fatalf("ended=%v err=%v", ended, err)
			}
		})
	}
}

func TestOtherFailedTurnIsNotQuota(t *testing.T) {
	failed := message{Method: "turn/completed", Params: json.RawMessage(`{"turn":{"status":"failed","error":{"message":"boom","codexErrorInfo":"other"}}}`)}
	_, err := notification(failed, func(faden.Event) error { return nil }, &limitState{})
	var quota *faden.QuotaError
	if err == nil || errors.As(err, &quota) {
		t.Fatalf("err = %v", err)
	}
}

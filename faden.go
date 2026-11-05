package faden

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"time"
)

// ErrLocked means another process owns the store.
var ErrLocked = errors.New("faden: store is locked by another owner")

// ErrNotFound means the store holds no such session or turn.
var ErrNotFound = errors.New("faden: not found")

// Status is where a turn stands.
type Status string

// The statuses of a turn. Done, failed and cancelled are final.
const (
	Pending   Status = "pending"
	Running   Status = "running"
	Done      Status = "done"
	Failed    Status = "failed"
	Cancelled Status = "cancelled"
)

// Final reports whether the turn will not run again.
func (s Status) Final() bool { return s == Done || s == Failed || s == Cancelled }

// Kind says what one event is.
type Kind string

// The kinds of event. Each is one completed item of a turn.
const (
	UserMessage      Kind = "user_message"
	AssistantMessage Kind = "assistant_message"
	ToolCall         Kind = "tool_call"
	ToolResult       Kind = "tool_result"
	ErrorEvent       Kind = "error"
	Handover         Kind = "handover"
	TurnStarted      Kind = "turn_started"
	TurnEnded        Kind = "turn_ended"
)

// Session is a thread: one resume cursor per backend it has used. A cursor is
// opaque JSON that only its backend reads.
type Session struct {
	ID      string                     `json:"id"`
	Cursors map[string]json.RawMessage `json:"cursors,omitempty"`
	Created time.Time                  `json:"created"`
}

// Turn is one user input and everything until the backend's final answer.
// RequestID is unique per store. The submission fixes every field but
// Progress, which changes while the turn runs.
type Turn struct {
	RequestID string `json:"requestId"`
	Session   string `json:"session"`
	Input     string `json:"input"`
	// Context is sent before Input on every attempt, but never enters events.
	Context string `json:"context,omitempty"`
	// Files are paths relative to the attempt's Setup.Dir. Only the paths are
	// stored; a backend reads the bytes when it sends the turn.
	Files   []string  `json:"files,omitempty"`
	Backend string    `json:"backend"`
	Model   string    `json:"model,omitempty"`
	Created time.Time `json:"created"`
	Progress
}

// Progress is the part of a turn that changes while it runs. A store's
// SubmitTurn starts a turn at Progress{Status: Pending}, and SaveTurn replaces
// the whole Progress, so a store needs no change when a field is added here.
type Progress struct {
	Status   Status `json:"status"`
	Attempts int    `json:"attempts"`
	Error    string `json:"error,omitempty"`
	// Failure says why a failed turn failed: "" for the agent or anything else,
	// FailureQuota or FailureUnavailable. RetryAt and LimitType hold what the
	// backend knew of a quota stop; they are zero when unknown.
	Failure   FailureKind `json:"failure,omitempty"`
	RetryAt   time.Time   `json:"retryAt,omitempty"`
	LimitType string      `json:"limitType,omitempty"`
	// Usage is the total per model over all attempts.
	Usage []ModelUsage `json:"usage,omitempty"`
}

// ModelUsage is the tokens and cost reported by a backend for one model.
// InputTokens includes cache reads and writes; OutputTokens includes reasoning tokens.
// CostUSD is zero when the backend reports no cost.
type ModelUsage struct {
	Model            string  `json:"model"`
	InputTokens      int64   `json:"inputTokens"`
	OutputTokens     int64   `json:"outputTokens"`
	CacheReadTokens  int64   `json:"cacheReadTokens"`
	CacheWriteTokens int64   `json:"cacheWriteTokens"`
	CostUSD          float64 `json:"costUSD"`
}

// Event is one completed item of a turn. Native keeps the backend's own
// payload beside the normalized fields, for a backend that rebuilds its
// transcript from it; a backend whose CLI keeps its own session leaves it
// empty. Seq counts from 1 within a session and is set by the store.
type Event struct {
	Session   string          `json:"session"`
	RequestID string          `json:"requestId"`
	Seq       int64           `json:"seq"`
	Kind      Kind            `json:"kind"`
	Time      time.Time       `json:"time"`
	Text      string          `json:"text,omitempty"`
	Tool      string          `json:"tool,omitempty"`
	Action    string          `json:"action,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Output    string          `json:"output,omitempty"`
	// Failed marks a tool result whose tool reported failure: an error, a
	// non-zero exit or a declined call. Output says why.
	Failed bool            `json:"failed,omitempty"`
	Native json.RawMessage `json:"native,omitempty"`
}

// Store keeps sessions, turns and events. A write the caller relies on is
// durable when the method returns. Missing things are reported as ErrNotFound.
type Store interface {
	// SubmitTurn stores t with Progress{Status: Pending} and returns it,
	// creating its session if needed. If t.RequestID is known, it returns
	// the existing turn and changes nothing.
	SubmitTurn(ctx context.Context, t Turn) (Turn, error)
	// SaveTurn replaces the Progress of a stored turn.
	SaveTurn(ctx context.Context, t Turn) error
	// Turn returns the turn with this request id.
	Turn(ctx context.Context, requestID string) (Turn, error)
	// Unfinished returns the pending and running turns, oldest first.
	Unfinished(ctx context.Context) ([]Turn, error)
	// Session returns a session with its cursors.
	Session(ctx context.Context, id string) (Session, error)
	// AppendEvent stores e at the next sequence number of its session and
	// returns it with Seq set.
	AppendEvent(ctx context.Context, e Event) (Event, error)
	// Events yields the events of a session with Seq above after, in order,
	// holding few of them at a time. An unknown session or a failed read is
	// yielded as the error, and ends the sequence. The store holds no lock
	// between events, so the loop may call the store.
	Events(ctx context.Context, session string, after int64) iter.Seq2[Event, error]
	// NextEvent returns the first event above after, or nil when caught up.
	// It reads at most one event, regardless of session length.
	NextEvent(ctx context.Context, session string, after int64) (*Event, error)
	// SetCursor stores the resume cursor of one backend of a session.
	SetCursor(ctx context.Context, session, backend string, cursor json.RawMessage) error
	// Close releases the store.
	Close() error
}

// CollectEvents gathers what Store.Events or Attempt.Events yields into a
// slice, for a reader that needs the whole range at once.
func CollectEvents(events iter.Seq2[Event, error]) ([]Event, error) {
	var out []Event
	for e, err := range events {
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// Backend runs one turn of an agent.
type Backend interface {
	// ContinuesWithoutPrompt reports whether the backend can continue an
	// interrupted turn with no input. If not, the runner sends a prompt.
	ContinuesWithoutPrompt() bool
	// Stop stops t's work, including a CLI whose host died. The runner calls
	// it on cancellation, before continuation and when giving up a turn.
	Stop(ctx context.Context, t Turn) error
	// Run runs the turn. t.Input is what to send; it is empty when the
	// backend continues without a prompt. Send t.Context before the input on
	// every attempt, without emitting it. Run emits each completed item
	// through a, and saves its resume cursor there as soon as it has one.
	Run(ctx context.Context, t Turn, a *Attempt) error
}

// FailureKind tells a host why a turn failed, so it can wait instead of
// blaming the agent.
type FailureKind string

// The kinds of failure beyond the agent's own. The host decides when to send
// a new turn; the runner never waits.
const (
	FailureQuota       FailureKind = "quota"
	FailureUnavailable FailureKind = "unavailable"
)

// QuotaError is what a backend returns when the provider refuses work until
// a limit resets. A zero ResetsAt means the backend did not learn the time.
type QuotaError struct {
	ResetsAt  time.Time
	LimitType string
}

func (e *QuotaError) Error() string {
	msg := "faden: provider quota reached"
	if e.LimitType != "" {
		msg += " (" + e.LimitType + ")"
	}
	if !e.ResetsAt.IsZero() {
		msg += ", resets at " + e.ResetsAt.UTC().Format(time.RFC3339)
	}
	return msg
}

// UnavailableError is what a backend returns when the agent never ran: it
// is not logged in, or its endpoint refused the connection.
type UnavailableError struct {
	Reason string
}

func (e *UnavailableError) Error() string { return "faden: agent unavailable: " + e.Reason }

package llama

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/nogo/faden"
	"github.com/nogo/faden/handover"
)

const bashName = "bash"

// Tool is a function the model may call.
type Tool struct {
	Name        string
	Description string
	// Parameters is the JSON Schema of the arguments; empty means no arguments.
	Parameters json.RawMessage
	// Safe marks a tool that may run again after a crash cut off its call.
	Safe bool
	// Run gets the arguments the model sent. An error becomes the result text.
	Run func(ctx context.Context, args json.RawMessage) (string, error)
}

func (t Tool) schema() json.RawMessage {
	if len(t.Parameters) == 0 {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return t.Parameters
}

func sortedTools(m map[string]Tool) []Tool {
	return slices.SortedFunc(maps.Values(m), func(a, b Tool) int { return strings.Compare(a.Name, b.Name) })
}

// toolset is the host's tools plus the built-in ones, bound to one attempt.
func (b *Backend) toolset(a *faden.Attempt, session, key string) map[string]Tool {
	tools := map[string]Tool{}
	for _, t := range b.cfg.Tools {
		tools[t.Name] = t
	}
	if b.cfg.Runtime != nil {
		tools[bashName] = bashTool(b.cfg.Runtime, key, a.Setup())
		if b.cfg.FileTools {
			for _, t := range fileTools(b.cfg.Runtime, key, a.Setup(), b.cfg.MaxOutput) {
				tools[t.Name] = t
			}
		}
	}
	tools[handover.ThreadTool] = readThreadTool(a, session)
	return tools
}

// bashTool runs a command line through the runtime, in the attempt's Dir and
// with its Env. It is never Safe. A
// non-zero exit is an error, so the result is marked failed.
func bashTool(rt faden.Runtime, key string, setup faden.Setup) Tool {
	return Tool{
		Name:        bashName,
		Description: "Run a shell command and return its output and exit code.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`),
		Run: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Command string `json:"command"`
			}
			if err := json.Unmarshal(args, &a); err != nil || a.Command == "" {
				return "", errors.New("arguments need a non-empty command")
			}
			res, err := rt.Run(ctx, faden.Command{Key: key, Dir: setup.Dir, Env: setup.Env, Argv: []string{"bash", "-c", a.Command}})
			if err != nil {
				return "", err
			}
			out := string(res.Output)
			if res.ExitCode != 0 {
				return "", fmt.Errorf("%s\n[exit code %d]", out, res.ExitCode)
			}
			return fmt.Sprintf("%s\n[exit code %d]", out, res.ExitCode), nil
		},
	}
}

// readThreadTool is handover's read_thread, bound to the attempt's session.
func readThreadTool(a *faden.Attempt, own string) Tool {
	return Tool{
		Name:        handover.ThreadTool,
		Description: handover.ThreadToolDescription,
		Parameters:  json.RawMessage(handover.ThreadToolSchema),
		Safe:        true,
		Run: func(ctx context.Context, args json.RawMessage) (string, error) {
			return handover.ReadThread(ctx, attemptEvents{a}, own, args)
		},
	}
}

// attemptEvents serves handover.Events from an attempt. The attempt is bound
// to one session, the only one ReadThread asks for, so session is not used.
type attemptEvents struct{ a *faden.Attempt }

func (e attemptEvents) Events(ctx context.Context, _ string, after int64) ([]faden.Event, error) {
	return e.a.Events(ctx, after)
}

// cutOutput shortens s to at most limit bytes without splitting a rune and
// appends a marker that says it was cut. It returns s itself when it fits.
func cutOutput(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	n := limit
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return fmt.Sprintf("%s\n[output cut at %d bytes]", s[:n], limit)
}

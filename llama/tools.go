package llama

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/nogo/faden"
	"github.com/nogo/faden/handover"
)

const (
	bashName      = "bash"
	maxToolOutput = 32 << 10
)

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

// toolset is the host's tools plus the built-in ones, bound to one session.
func (b *Backend) toolset(session, key string) map[string]Tool {
	tools := map[string]Tool{}
	for _, t := range b.cfg.Tools {
		tools[t.Name] = t
	}
	if b.cfg.Runtime != nil {
		tools[bashName] = bashTool(b.cfg.Runtime, key)
	}
	tools[handover.ThreadTool] = readThreadTool(b.cfg.Store, session)
	return tools
}

// bashTool runs a command line through the runtime. It is never Safe.
func bashTool(rt faden.Runtime, key string) Tool {
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
			res, err := rt.Run(ctx, faden.Command{Key: key, Argv: []string{"bash", "-c", a.Command}})
			if err != nil {
				return "", err
			}
			out := string(res.Output)
			if len(out) > maxToolOutput {
				out = out[:maxToolOutput] + "\n[output cut]"
			}
			return fmt.Sprintf("%s\n[exit code %d]", out, res.ExitCode), nil
		},
	}
}

// readThreadTool is handover's read_thread, bound to one session.
func readThreadTool(store Store, own string) Tool {
	return Tool{
		Name:        handover.ThreadTool,
		Description: handover.ThreadToolDescription,
		Parameters:  json.RawMessage(handover.ThreadToolSchema),
		Safe:        true,
		Run: func(ctx context.Context, args json.RawMessage) (string, error) {
			return handover.ReadThread(ctx, store, own, args)
		},
	}
}

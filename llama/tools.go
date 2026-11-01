package llama

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/nogo/faden"
)

const (
	bashName       = "bash"
	readThreadName = "read_thread"
	maxToolOutput  = 32 << 10
	maxThreadItems = 100
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
	out := make([]Tool, 0, len(m))
	for _, t := range m {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// toolset is the host's tools plus the built-in ones, bound to one session.
func (b *Backend) toolset(session string) map[string]Tool {
	tools := map[string]Tool{}
	for _, t := range b.cfg.Tools {
		tools[t.Name] = t
	}
	if b.cfg.Runtime != nil {
		tools[bashName] = bashTool(b.cfg.Runtime)
	}
	tools[readThreadName] = readThreadTool(b.cfg.Store, session)
	return tools
}

// bashTool runs a command line through the runtime. It is never Safe.
func bashTool(rt faden.Runtime) Tool {
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
			res, err := rt.Run(ctx, []string{"bash", "-c", a.Command})
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

// readThreadTool returns the events of one session by sequence range. It
// reads only, and refuses any session but its own.
func readThreadTool(store Store, own string) Tool {
	return Tool{
		Name:        readThreadName,
		Description: "Read events of this conversation by sequence number, from first to last inclusive.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"from":{"type":"integer"},"to":{"type":"integer"}},"required":["from","to"]}`),
		Safe:        true,
		Run: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Session string `json:"session"`
				From    int64  `json:"from"`
				To      int64  `json:"to"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("bad arguments: %w", err)
			}
			if a.Session != "" && a.Session != own {
				return "", errors.New("only this conversation can be read")
			}
			if a.From < 1 {
				a.From = 1
			}
			events, err := store.Events(ctx, own, a.From-1)
			if err != nil {
				return "", err
			}
			var lines []string
			for _, e := range events {
				if e.Seq > a.To {
					break
				}
				if len(lines) == maxThreadItems {
					lines = append(lines, fmt.Sprintf("[cut: ask again from %d]", e.Seq))
					break
				}
				e.Native, e.Session, e.RequestID = nil, "", ""
				line, _ := json.Marshal(e)
				lines = append(lines, string(line))
			}
			return strings.Join(lines, "\n"), nil
		},
	}
}

# faden

A Go module for running agent turns that survive a restart and can move between models.
Hosts embed it: an app hands faden a user's message, and faden runs it on Claude Code, Codex or a local llama.cpp model until the answer is done. If the process dies halfway, the next start picks the turn up again.

"Den Faden wieder aufnehmen": pick up the thread again.

## When to use it

Use faden when **an agent's work must not be lost to a crash, a deploy or a reboot**, and you don't want to build that bookkeeping again in every app.

| Use faden when | Look elsewhere when |
|---|---|
| A turn runs for minutes and nobody watches it | A request is one short model call: retry it |
| The host restarts while agents work (deploys, unattended machines) | You need a general workflow engine with timers and task trees |
| A conversation should move to another model (quota, cost, a local model for private data) | You only ever talk to one hosted API and never restart |
| You want the full transcript in your own store, not only in the agent CLI's files | You need many machines writing one store |
| Agents run in containers you already manage | You want faden to build or start containers for you |

What it costs you: the standard library only, a store (JSONL files or your SQLite database), and one runner per store.

## The idea in one picture

```
            host app (sidenote, forge, invest, ...)
                 |  Submit(session, requestId, input, backend)
                 v
   +---------------------------- faden.Runner ----------------------------+
   |  one turn at a time per session, oldest first                        |
   |  stores the user message, the handover, every event, the cursor      |
   +---------+--------------------------+---------------------------+----+
             |                          |                           |
       claude backend             codex backend               llama backend
       (Claude Code CLI)          (codex app-server)          (faden's own loop on
             |                          |                      /v1/chat/completions)
             +------------+-------------+                           |
                          v                                         v
                 runtime: Local process                       llama-server
                          or container exec
                          |
                          v
                   store: jsonl files or the host's SQLite (*sql.DB)
```

- A **session** is a thread. It remembers its current backend and one resume cursor per backend.
- A **turn** is one input and everything until the final answer. It is stored before it runs.
- An **event** is one completed item: user message, assistant message, tool call, tool result, handover, error.

## What happens on a crash

```
 attempt 1            host killed             next Open()
 ---------            -----------             -----------
 turn stored  ---->   events so far    ---->  attempts = 2 (stored first)
 user message         stay in store           Stop(turn): kill what attempt 1 left running
 tool call A                                  continue:
 tool result A                                  llama  -> rebuilds messages, goes on
 ...killed                                      claude -> resume + stored events + "Continue
                                                          where you left off."
                                                codex  -> resume + stored events, no prompt
                                              after 3 attempts: failed, "send a new turn"
```

Measured on Claude Code 2.1.316 and Codex 0.166.0: their own resume forgets a killed turn's results.
With faden's stored events in the continuation, both continued without redoing the finished step (`docs/decisions.md`, "Measured").

A retried `Submit` with a known `requestId` returns the existing turn and runs nothing, so a client may retry freely.

## What happens on a handover

```
 turn 1 on claude     turn 2 on llama              turn 3 on claude
 ----------------     ---------------              ----------------
 events 1..4          handover claude -> llama     handover llama -> claude
                      catch-up: events 1..4        catch-up: only events since
                      (budgeted selection)         claude's last turn, plus
                                                   --resume of its own session
```

A handover is a **budgeted selection, not a summary**: the first user message, then the newest items until the budget (default 16,000 bytes) is spent.
The new request is never shortened. Omitted ranges are named, and the agent can fetch them with the `read_thread` tool (served over MCP for Claude Code).

## Packages

| Package | Use it for |
|---|---|
| `faden` | `Runner`, `Store`, `Backend`, `Runtime`, `Local` (run commands on the host) |
| `jsonl` | A store of append-only files, one per session. One owner process (file lock). |
| `sqlite` | A store in your own `*sql.DB`; you bring the driver. `WithTx` joins your transaction. |
| `claude` | Claude Code through `--print` stream-json. faden creates the session id. |
| `codex` | Codex through `codex app-server`. |
| `llama` | faden's own loop on an OpenAI-compatible server, with your Go functions as tools. |
| `container` | Run a CLI inside a container you started (podman or docker `exec`). |
| `handover` | The selection behind a handover; hosts rarely call it. |
| `mcp` | `read_thread` for Claude Code. |
| `storetest` | The conformance suite a custom store must pass. |

## Quick start

A local model with one tool, stored in files:

```go
store, err := jsonl.Open("/var/lib/myapp/faden")
if err != nil {
	return err
}
defer store.Close()

model, err := llama.New(llama.Config{
	URL:   "http://localhost:8080",
	Model: "qwen-27b",
	Store: store,
	Tools: []llama.Tool{{
		Name: "now",
		Safe: true, // reads only, so it may run again after a crash
		Run: func(context.Context, json.RawMessage) (string, error) {
			return time.Now().Format(time.RFC3339), nil
		},
	}},
})
if err != nil {
	return err
}

runner, err := faden.Open(store, map[string]faden.Backend{"llama": model})
if err != nil {
	return err
}
defer runner.Close()

if _, err := runner.Submit(ctx, faden.Submission{
	Session: "s1", RequestID: "r1", Backend: "llama", Input: "What time is it?",
}); err != nil {
	return err
}
turn, err := runner.Wait(ctx, "r1")
```

The answer is in the session's events: `store.Events(ctx, "s1", 0)`.

To add Claude Code, give it a runtime with a durable directory for its process records:

```go
local := faden.Local{PIDDir: "/var/lib/myapp/pids", Dir: "/srv/workspace"}
agent, err := claude.New(claude.Config{
	Runtime: local,
	Store:   store,
	Args:    []string{"--permission-mode", "acceptEdits"},
})
// faden.Open(store, map[string]faden.Backend{"llama": model, "claude": agent})
```

A submission names its backend. Submitting the next turn of a session with another backend is a handover.

## What the host owns

faden never touches these, by design:

- credentials: a CLI gets them from its environment, never from an argument or the store;
- the workspace, the container and its lifetime, and the CLI's own session files (`~/.claude`, `~/.codex`) across restarts;
- scheduling, the user interface, and who may read which session.

## Status

Personal project, young, API may change. Live-tested on one machine with Claude Code 2.1.316, codex-cli 0.166.0 and llama.cpp.
The reasons behind each choice are in `docs/decisions.md`; the goal is in `docs/project.md`.

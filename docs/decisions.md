# Decisions

## Decided 2026-11-01 — the first version

### Name and scope

- **The module is `github.com/nogo/faden`.** "Durable" named one property; the module is a thread that survives restarts and model switches. `github.com/nogo/durable` stays with the proactive gate (`../produrable`).
- **Durability is per turn.** A turn is one user input and everything until the backend's final answer. pi durable checkpoints every step because it owns the loop; Claude Code and Codex own theirs, so a turn is the finest unit all backends share. Rejected: pi's task tree, phases, documents and forks.
- **Sources read for the design:** pi durable (`earendil-works/pi`, `packages/durable`, and the post at earendil.com/posts/pi-durable), t3code (`pingdotgg/t3code`: `apps/server/src/serverRuntimeStartup.ts`, `provider/Layers/ClaudeAdapter.ts`, `docs/internals/context-handoffs.md`, `docs/user/portable-handoffs.md`), and the existing code in `invest/internal/analyst`, `sidenote/internal/harness` and `forge/internal/{harness,container}`.

### Model

- **A session is a thread.** It records its current backend and one resume cursor per backend it has used.
- **A turn** carries a session, a `requestId`, the input text, the backend and model, a status (`pending`, `running`, `done`, `failed`) and an attempt count. A session runs one turn at a time; later turns wait as `pending`, oldest first.
- **`requestId` is unique per store.** Submitting a known `requestId` returns the existing turn and runs nothing.
- **An event is one completed item** of a turn: user message, assistant message, tool call, tool result, error, handover, turn started, turn ended. It keeps the backend's native payload beside the normalized fields (text, tool name, action, input, output), as sidenote's `harness.Event` does. Streaming fragments are not stored.
- **A resume cursor is opaque JSON owned by its backend.** faden stores it and hands it back; only the backend reads it (as t3code does).

### Restart

- **On open, every `running` turn continues.** Its attempt count goes up in the store before anything is sent, so a crash during the continuation still recovers on the next open (t3code writes "starting" for the same reason). After 3 attempts the turn fails with a message that says to send a new one.
- **A backend declares whether it continues without a prompt.** llama.cpp does (faden owns its transcript); Claude Code gets "Continue where you left off." (t3code's text), preceded by the turn's stored events, selected and shortened like a handover.
- ~~Claude Code gets "Continue where you left off."~~ *Changed 2026-11-02: measured, `--resume` keeps only the killed turn's user prompt and Claude answered `FIRST_FILE_UNKNOWN` (see below). The runner now adds the turn's own stored events to the continuation of a backend that needs a prompt (#14).*
- ~~**A CLI left running by a dead host is stopped before the turn continues.** Otherwise two agents write to one workspace. A local process dies with its parent (Linux `Pdeathsig`, process group); a process inside a container is found by a pid file the start command writes and killed by the next owner.~~ *Superseded 2026-11-01: the parent-death signal does not stop grandchildren.*
- ~~**Local leftovers are found in `Local.PIDDir/process.pid`.** The host supplies an existing, durably created directory for one sequential command owner and reuses it after restart. Before every command, Local kills the recorded process group, ignoring a group that no longer exists. A shell waits on a pipe while Local writes the new group id, syncs the file and syncs its directory; only then does Local release the command. Local kills remaining group members and removes the record on completion. `Pdeathsig` is supplementary: it targets the direct child and depends on the starting OS thread. Container leftovers still use the container pid file.~~ *Changed 2026-11-02: see "Decided 2026-11-01 — commands keyed by turn" (#17).*

### Backends

- **A backend runs one turn:** it gets the turn, the resume cursor and an emit function, and returns the new cursor. faden stores what it emits.
- **Claude Code:** faden creates the session UUID and passes `--session-id` on the first turn and `--resume` on later ones, so the id is stored before the process starts. `--print` with stream-json input and output; one process per turn (as sidenote does). The cursor also keeps the last assistant message id. The host supplies the permission flags.
- **llama.cpp:** faden's own loop against an OpenAI-compatible `/v1/chat/completions`, ported from invest's analyst. Each model answer is stored; a tool call is stored before it runs and its result after. On continue, the messages are rebuilt from the events. A stored call without a result runs again only if its tool is marked safe; otherwise its result says it was interrupted.
- **llama.cpp tools** are the host's Go functions plus `bash` (run through the runtime, never safe to replay) and `read_thread`.
- **`faden.Runtime` runs one command to its end** and returns output and exit code; a non-zero exit is a result, not an error. `faden.Local` runs it through `os/exec` in a directory and kills the process group when stopped.
- ~~**llama.cpp stores the user message itself** on the first attempt. A continuation that finds no stored user message for its turn fails with "send a new turn", because the runner no longer holds the input then.~~ *Changed 2026-11-02: a crash before that event failed a turn whose input was in the store (forge review of #1).*
- **The runner stores each turn's user message,** once, after any handover event and before the backend runs. A backend stores only what its agent says and does. The input is in the store from `Submit` on, so no crash can lose it, and a continuation prompt never enters the thread as something the user said.
- ~~**Codex is parked** until Claude and llama.cpp work.~~ *Superseded 2026-11-02: the `codex` backend exists, see "Decided 2026-11-02 — Codex".*

### Runtimes

- **A runtime starts a command:** locally with `os/exec`, or inside a running container with `podman exec` or `docker exec`. The host creates the container and owns its lifetime; faden never starts one.
- **The CLI's own session files must outlive a turn.** In a container, Claude's `~/.claude` lives on a volume or the container lives as long as the session; otherwise `--resume` finds nothing. The host is responsible; faden says so in the runtime's documentation.

### Stores

- **`Store` is an interface in the root package.** faden ships `jsonl` and `sqlite`, and one conformance suite (`storetest`) that every store passes.
- **`jsonl`:** one append-only file per session in a directory, synced before a write returns. A torn last line is cut at open. A lock file (`flock`) makes one process the owner; a second gets `ErrLocked` at once.
- **`sqlite`:** takes the host's `*sql.DB`, so faden imports no driver. Tables carry a prefix (default `faden_`) so a host database can hold them. A host can bind the store to its own `*sql.Tx`, so a submission commits with the host's own write (invest stores a question with its book).
- **`modernc.org/sqlite` is a test dependency of `sqlite` only.** The conformance suite needs a real driver; invest already uses this one (pure Go, no cgo). No non-test file imports it.

### Handover

- **A handover is a budgeted selection, not a summary** (t3code's design). It is recorded as an event.
- **The new backend gets what it has not seen:** the full thread if it never took part, otherwise the events since its last turn.
- **Selection:** the session's first user message, then the newest items backward until the budget is spent. Selected user and assistant messages keep their full text; other items shrink to 240 characters. The new request is never shortened. Default budget 16,000 bytes.
- **Omitted items are referenced** by their sequence range, so the agent can fetch them with `read_thread`.
- **llama.cpp gets the selection as chat messages; Claude Code as one attributed text block** before the request.

### Thread reading

- **`read_thread` returns a session's events by sequence range,** read only, limited to the session it serves.
- **llama.cpp gets it as a native tool. Claude Code gets it from an MCP server** the host process serves over HTTP on loopback (`--mcp-config`). Its token reaches Claude through the environment, never through an argument.

## Open

- **Thread reading from a container:** loopback on the host is not reachable from a container without host networking, which forge's boundary forbids. Candidates: a unix socket mounted into the container, or no `read_thread` for container runs.
- ~~**Codex without a prompt:** read how t3code's Codex adapter continues a turn.~~ *Answered 2026-11-02: `turn/start` with no input items after `thread/resume` (t3code `CodexSessionRuntime.ts`, capability `promptlessTurnContinuation`).*

## Measured 2026-11-02 — Claude resume after a kill

**`--resume` does not see the work of a killed turn.** `FADEN_LIVE=1 go test -v ./claude -run TestLiveResumeAfterKill` (claude 2.1.316) killed the host after `first.txt` was written. Claude's own session file then held only the user prompt; the resumed session inserted "No response requested." in place of the killed turn's work. Resumed with "Continue where you left off.", Claude answered `FIRST_FILE_UNKNOWN` and wrote `first.txt` again. One run, but the session file is direct evidence.

Consequence: faden's stored events are the only record of a killed Claude turn. The continuation prompt alone repeats the work; a continuation that carries the turn's stored events is proposed in an issue, because it changes "Claude Code gets 'Continue where you left off.'" above.

## Decided 2026-11-01 — Claude runtime streaming

Claude consumes stdin and emits completed items while it runs. ~~Its backend requires `RunStream`; `Local` implements it beside the existing buffered `Run`.~~ *Changed 2026-11-02: see "Decided 2026-11-01 — commands keyed by turn" (#17).* Stderr stays separate from the JSON stream. The host supplies the environment through `Local.Env`. The backend checkpoints its UUID through the host's store before launch, and checkpoints the last assistant id during the stream. A custom backend name must match its key in the runner.

## Decided 2026-11-01 — handover replay

A submission selects its backend. The runner records a handover when execution reaches a different backend, so queued submissions follow execution order. The event keeps the destination, the last event seen by that backend, and the host's byte budget. A retry uses that same boundary and budget. `faden.Options.Budget` sets the budget when opening the runner; zero uses 16,000 bytes. The request consumes the budget but remains intact. The first user message in the unseen range remains intact even when it exceeds the remaining budget. Selection counts text bytes, excluding attribution and omission notices.

llama.cpp rebuilds its native messages and inserts portable messages at incoming handovers. Foreign native payloads never become llama.cpp tool calls. Claude receives portable text before its request while retaining its own resume cursor.

## Decided 2026-11-01 — thread reading over MCP

`mcp.Server` listens on a loopback port and serves `read_thread` as JSON-RPC over HTTP POST (`initialize`, `ping`, `tools/list`, `tools/call`). Each session has its own token, granted per turn and revoked when the turn ends. A wrong token gets 401; a `session` argument other than the token's session is a tool error. `claude.Config.Threads` makes the backend write a temporary `--mcp-config` file whose `Authorization` header is `Bearer ${FADEN_MCP_TOKEN}`. The token reaches Claude only through its environment, ~~so this needs `faden.Local` (`Local.WithEnv`)~~; since #17 it travels in `Command.Env`. Container runs stay open, as above.

## Decided 2026-11-02 — Codex

Package `codex` runs `codex app-server` through a runtime ~~with `RunStream`~~ and speaks JSON-RPC over stdio: `initialize`, `initialized`, then `thread/start` (new session) or `thread/resume` with the stored thread id, then `turn/start`. The cursor is `{"thread_id": ...}`. It is stored as soon as `thread/start` answers, before `turn/start` is sent.

Completed items become events, each with the notification as its native payload: `agentMessage` is assistant text; `commandExecution`, `fileChange` and `mcpToolCall` are a tool call (with its action) followed by a tool result; `turn/completed` is the end of the turn, and a status other than `completed` fails the turn. The backend does not store the user message.

Codex continues without a prompt: after `thread/resume`, `turn/start` carries an empty `input` list. `ContinuesWithoutPrompt` is true, so the runner sends no continuation text. Approval requests from Codex are declined (`decline`; the legacy requests get `denied`); other server requests get a "not supported" error. The host's sandbox and approval settings pass as `Config.Args` to `codex app-server` (`-c sandbox_mode=...`). A handover puts the catch-up block before the request, as the Claude backend does. When the input is empty the catch-up is not added.

Stdin is an `os.Pipe`, not an `io.Pipe`: a pipe the child reads directly leaves no copying goroutine that could keep the runtime waiting after a crashed Codex.

The protocol shapes come from sidenote's adapter and t3code as described in the issue; neither source nor a `codex` binary was available when this was written, so the fake in `codex/codex_test.go` uses invented lines in the shapes remembered from the app-server schema. Check them against a real run.

### Measured — Codex resume after a kill

**Measured 2026-11-02 (codex-cli 0.166.0): Codex does not know the killed turn's result either.** `FADEN_LIVE=1 go test -v ./codex -run TestLiveResumeAfterKill` killed the host after `first.txt` was written. Codex's own rollout kept the killed turn's assistant message and its first tool call, but not the tool's output. Continued with no prompt, Codex answered `FIRST_FILE_UNKNOWN`, then checked the workspace and found `first.txt` instead of writing it again. One run. Like Claude, the killed turn's results live only in faden's store; #14 proposes carrying them into the continuation.
## Decided 2026-11-01 — commands keyed by turn

### Restart

This supersedes the earlier Restart rule that assigns `Local.PIDDir/process.pid` to one sequential owner. It also supersedes the parent-death signal as a recovery aid. ~~Backends call `Stop(ctx, t.RequestID)` before continuing an attempt above one, including turns that run no command.~~ *Changed 2026-11-02 (b14c8e6, 6a5d6ad): `Backend` has `Stop(ctx, turn)`, and the runner calls it before an attempt above one and when `Open` gives a turn up after three attempts; a backend forwards to its runtime.* Each key has one command owner at a time. Hosts that share a record directory across stores must give those stores distinct request IDs.

A record contains the process group leader's PID and its Linux start time from `/proc/<pid>/stat`. Recovery kills the group only when that identity still matches. Missing, malformed, and stale records are discarded. Local leaves the leader alive after host death so recovery can still verify it. Cancellation and normal completion kill the local group.

### Runtimes

This supersedes `RunStream`, `Local.WithEnv`, and the fixed container turn field in the earlier runtime, streaming, MCP, and Codex decisions. `Runtime.Run` takes one `Command` with a turn key, argv, extra environment, stdin, and stdout. Nil stdout buffers both output streams in `Result.Output`; streamed stdout leaves stderr there. Claude, Codex, and llama use the request ID as the key. Claude passes the MCP token through the command environment with every runtime. The host must make its MCP config file and server reachable from the command.

Local records live in the host's durable `PIDDir`; container records live in `/tmp/faden` inside the host's container. Both name records `<sha256(key)>.pid`, so arbitrary keys cannot escape the directory. Each runtime writes a temporary file, syncs it, renames it, and syncs the directory before releasing the command. Commands remove their records when they end. Starting a command recovers only its own key. The container supplies Linux `/proc`, `setsid`, `awk`, and `sync` with file operands. Faden does not own the container lifecycle.

## Decided 2026-11-02 — killed-turn continuation through the runner

This supersedes the Codex decision to continue without a prompt. Codex requests a continuation prompt from the runner, which includes the killed turn's stored events. Its native transcript can omit tool results after a kill, as measured above. An empty input remains supported by the backend protocol.

A continuation without a handover uses the host's `Options.Budget`. A continuation with a handover keeps the budget recorded in that handover. Both select stored events through the same byte budget rules.

## Measured 2026-11-02 — continuation with stored events

**With the killed turn's stored events in the continuation, both CLIs know what they already did.** `FADEN_LIVE=1 go test -v ./claude ./codex -run TestLiveResumeAfterKill` (claude 2.1.316, codex-cli 0.166.0, through the runner and a `jsonl` store since #18) answered `FIRST_FILE_KNOWN` on both. Claude did not write `first.txt` again; Codex kept it and continued with the sleep. One run each. Before #14 both answered `FIRST_FILE_UNKNOWN`.

# Decisions

## Decided 2026-11-01 — the first version

### Name and scope

- **The module is `github.com/nogo/faden`.** "Durable" named one property; the module is a thread that survives restarts and model switches. `github.com/nogo/durable` stays with the proactive gate (`../produrable`).
- **Durability is per turn.** A turn is one user input and everything until the backend's final answer. pi durable checkpoints every step because it owns the loop; Claude Code and Codex own theirs, so a turn is the finest unit all backends share. Rejected: pi's task tree, phases, documents and forks.
- **Sources read for the design:** pi durable (`earendil-works/pi`, `packages/durable`, and the post at earendil.com/posts/pi-durable), t3code (`pingdotgg/t3code`: `apps/server/src/serverRuntimeStartup.ts`, `provider/Layers/ClaudeAdapter.ts`, `docs/internals/context-handoffs.md`, `docs/user/portable-handoffs.md`), and the existing code in `invest/internal/analyst`, `sidenote/internal/harness` and `forge/internal/{harness,container}`.

### Model

- ~~**A session is a thread.** It records its current backend and one resume cursor per backend it has used.~~ *Changed 2026-11-02: a session records one resume cursor per backend; nothing read its current backend. See "Decided 2026-11-02 — the attempt".*
- ~~**A turn** carries a session, a `requestId`, the input text, the backend and model, a status (`pending`, `running`, `done`, `failed`) and an attempt count. A session runs one turn at a time; later turns wait as `pending`, oldest first.~~ *Changed 2026-11-02: cancellation must end a turn permanently, including after restart.*
- **A turn** carries a session, a `requestId`, the input text, the backend and model, a status (`pending`, `running`, `done`, `failed`, `cancelled`) and an attempt count. Done, failed and cancelled are final. A session runs one turn at a time; later turns wait as `pending`, oldest first.
- **`requestId` is unique per store.** Submitting a known `requestId` returns the existing turn and runs nothing.
- **An event is one completed item** of a turn: user message, assistant message, tool call, tool result, error, handover, turn started, turn ended. ~~It keeps the backend's native payload beside the normalized fields (text, tool name, action, input, output), as sidenote's `harness.Event` does.~~ *Changed 2026-11-02: only llama.cpp keeps a native payload; Claude Code and Codex events carry the normalized fields (text, tool name, action, input, output). See "Decided 2026-11-02 — memory".* Streaming fragments are not stored.
- **A resume cursor is opaque JSON owned by its backend.** faden stores it and hands it back; only the backend reads it (as t3code does).

### Restart

- **On open, every `running` turn continues.** Its attempt count goes up in the store before anything is sent, so a crash during the continuation still recovers on the next open (t3code writes "starting" for the same reason). After 3 attempts the turn fails with a message that says to send a new one.
- **A backend declares whether it continues without a prompt.** llama.cpp does (faden owns its transcript); Claude Code gets "Continue where you left off." (t3code's text), preceded by the turn's stored events, selected and shortened like a handover.
- ~~Claude Code gets "Continue where you left off."~~ *Changed 2026-11-02: measured, `--resume` keeps only the killed turn's user prompt and Claude answered `FIRST_FILE_UNKNOWN` (see below). The runner now adds the turn's own stored events to the continuation of a backend that needs a prompt (#14).*
- ~~**A CLI left running by a dead host is stopped before the turn continues.** Otherwise two agents write to one workspace. A local process dies with its parent (Linux `Pdeathsig`, process group); a process inside a container is found by a pid file the start command writes and killed by the next owner.~~ *Superseded 2026-11-01: the parent-death signal does not stop grandchildren.*
- ~~**Local leftovers are found in `Local.PIDDir/process.pid`.** The host supplies an existing, durably created directory for one sequential command owner and reuses it after restart. Before every command, Local kills the recorded process group, ignoring a group that no longer exists. A shell waits on a pipe while Local writes the new group id, syncs the file and syncs its directory; only then does Local release the command. Local kills remaining group members and removes the record on completion. `Pdeathsig` is supplementary: it targets the direct child and depends on the starting OS thread. Container leftovers still use the container pid file.~~ *Changed 2026-11-02: see "Decided 2026-11-01 — commands keyed by turn" (#17).*

### Backends

- ~~**A backend runs one turn:** it gets the turn, the resume cursor and an emit function, and returns the new cursor. faden stores what it emits.~~ *Changed 2026-11-02: it gets the turn and a `faden.Attempt`. See "Decided 2026-11-02 — the attempt".*
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

Claude consumes stdin and emits completed items while it runs. ~~Its backend requires `RunStream`; `Local` implements it beside the existing buffered `Run`.~~ *Changed 2026-11-02: see "Decided 2026-11-01 — commands keyed by turn" (#17).* Stderr stays separate from the JSON stream. The host supplies the environment through `Local.Env`. ~~The backend checkpoints its UUID through the host's store before launch, and checkpoints the last assistant id during the stream. A custom backend name must match its key in the runner.~~ *Changed 2026-11-02: it checkpoints both through `Attempt.SaveCursor`, and its name is its key in the runner; see "Decided 2026-11-02 — the attempt".*

## Decided 2026-11-01 — handover replay

A submission selects its backend. The runner records a handover when execution reaches a different backend, so queued submissions follow execution order. The event keeps the destination, the last event seen by that backend, and the host's byte budget. A retry uses that same boundary and budget. `faden.Options.Budget` sets the budget when opening the runner; zero uses 16,000 bytes. The request consumes the budget but remains intact. The first user message in the unseen range remains intact even when it exceeds the remaining budget. Selection counts text bytes, excluding attribution and omission notices.

llama.cpp rebuilds its native messages and inserts portable messages at incoming handovers. Foreign native payloads never become llama.cpp tool calls. Claude receives portable text before its request while retaining its own resume cursor.

## Decided 2026-11-01 — thread reading over MCP

`mcp.Server` listens on a loopback port and serves `read_thread` as JSON-RPC over HTTP POST (`initialize`, `ping`, `tools/list`, `tools/call`). Each session has its own token, granted per turn and revoked when the turn ends. A wrong token gets 401; a `session` argument other than the token's session is a tool error. `claude.Config.Threads` makes the backend write a temporary `--mcp-config` file whose `Authorization` header is `Bearer ${FADEN_MCP_TOKEN}`. The token reaches Claude only through its environment, ~~so this needs `faden.Local` (`Local.WithEnv`)~~; since #17 it travels in `Command.Env`. Container runs stay open, as above.

## Decided 2026-11-02 — Codex

Package `codex` runs `codex app-server` through a runtime ~~with `RunStream`~~ and speaks JSON-RPC over stdio: `initialize`, `initialized`, then `thread/start` (new session) or `thread/resume` with the stored thread id, then `turn/start`. The cursor is `{"thread_id": ...}`. It is stored as soon as `thread/start` answers, before `turn/start` is sent.

Completed items become events, ~~each with the notification as its native payload~~ *(changed 2026-11-02: with normalized fields only, see "Decided 2026-11-02 — memory")*: `agentMessage` is assistant text; `commandExecution`, `fileChange` and `mcpToolCall` are a tool call (with its action) followed by a tool result; `turn/completed` is the end of the turn, and a status other than `completed` fails the turn. The backend does not store the user message.

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

## Decided 2026-11-02 — memory

Memory is a requirement, not a tuning step. A host keeps faden running for months, and tool output is the bulk of a thread.

- **Claude Code and Codex events carry no native payload.** Both CLIs keep their own session, and nothing in faden read their native lines. What faden must know lives in the normalized fields: the store still tracks every completed item, and a killed turn continues from those fields (#14). Only llama.cpp keeps a native payload, because it rebuilds its transcript from it. Measured on one invented Claude turn with a 21 KB tool output (`/tmp/fadenmeasure`): 69.7 KB stored before, of which 45.3 KB native. The lost parts are the CLI's envelope: ids, token usage, cost, and Claude's second copy of a tool's output. Rejected: one native line per item, which still doubled every tool output. Measured after the change: `FADEN_LIVE=1 go test -v ./claude ./codex -run TestLiveResumeAfterKill` answered `FIRST_FILE_KNOWN` on both CLIs, so the continuation needs no native payload. One run each.
- **`jsonl` keeps events in the file, not in memory.** It holds one file offset per event and reads events when asked. Sessions, turns and cursors stay in memory. Open streams the file line by line. Measured on 2,000 events of 8 KB: 17.9 MB held after open before, 24 KB after.
- **The runner reads a session's events once per turn,** not once for each of catch-up, continuation and the input check.
- **A handover renders only the events it selects,** and copies at most what the 240-rune cut keeps. Measured on 500 tool outputs of 100 KB: 152 MB allocated per selection before, 66 KB after.
- **`read_thread` has one owner, `handover.ReadThread`,** used by the llama tool and the MCP server.

## Decided 2026-11-02 — tool failure

**A tool result says whether its tool failed (`Event.Failed`).** Without native payloads, faden's store lost the failure of a Claude tool (`is_error`) and of a Codex command (exit code). A flag, not an exit code: Claude reports no code, only the flag. Output still says why.

- Claude: `is_error` on a `tool_result` block.
- Codex: status `failed` or `declined`, a non-zero `exitCode`, or an MCP call with an error. The exit code itself is not kept. These shapes are unverified, like the rest of the Codex protocol above.
- llama.cpp: a tool error, an unknown tool, an interrupted call, or a non-zero `bash` exit. `bash` now reports a non-zero exit as an error, so its output starts with `error: ` and still ends with `[exit code N]`.
- A handover renders a failed result as `tool_result failed …`. `read_thread` returns the field as `"failed": true`.

Rejected: an exit-code field, which Claude cannot fill; `[exit N]` appended to Output, which is text a reader must parse.

## Decided 2026-11-02 — the attempt

**The store goes in once, to `faden.Open`.** Before, a host passed it to `faden.Open` and again to every backend's config. It also gave every backend a `Name` that had to match its key in the runner, or cursors landed under the wrong key. Nothing checked that.

- **`Backend.Run(ctx, t, a *faden.Attempt) error`.** The runner makes one `Attempt` per run, bound to the turn's session and backend. It offers `Emit`, `Cursor`, `SaveCursor`, `Events` and `CatchUp`, and is a backend's only way to the store. The `claude`, `codex` and `llama` packages declare no store type and no `Name`; a backend's name is its key in the runner's map.
- **`Attempt` is a struct, not an interface.** faden owns it, so a later method breaks no backend (Go wiki, Code Review Comments, "Interfaces"; Go blog, "Keeping your modules compatible"). `faden.NewAttempt(ctx, store, turn)` builds one without a runner, for a backend's tests; it has no catch-up.
- **A backend saves its cursor through `SaveCursor`; `Run` returns no cursor.** Claude and Codex already saved the same cursor during the run, so the returned one was a second path to the same write.
- **`Turn.CatchUp` and `Turn.HandoverBudget` moved to `Attempt.CatchUp()`.** They were never stored but sat on the public `Turn`, so a host saw them empty after `Wait`. `handover.Prompt(t, a)` renders them; the root package cannot, because `handover` imports it.
- **`Session.Backend` is gone,** with the `Store` rule that wrote it on every submit. Nothing in faden read it: the handover boundary comes from executed turns. jsonl now writes a session record only when it creates the session; sqlite inserts the session row once. Old files and rows still load, because their extra field is ignored.
- **`NewAttempt` fails for an unknown session** with `ErrNotFound`, so the mistake shows where it is made, not at the first `Emit`.

Measured after the change: `FADEN_LIVE=1 go test -v ./claude ./codex -run TestLiveResumeAfterKill` (claude 2.1.316, codex-cli 0.166.0) answered `FIRST_FILE_KNOWN` on both and finished all three files. One run each.

Rejected: one constructor for all backends (`faden.New("claude", cfg)`), which needs one config whose fields mostly apply to one backend; a `faden.Open` that creates jsonl or sqlite itself, which is an import cycle; an interface for the attempt, which is easier to fake but breaks every backend when it grows.

## Decided 2026-11-02 — release

- **`go 1.27` in `go.mod`, not a patch version.** The `go` line is a minimum for every host that embeds faden; `go 1.27.3` would force sidenote (on 1.27.1) to upgrade. `mise.toml` still pins the toolchain faden is checked with.
- **MIT license.** The shortest common license that pkg.go.dev recognizes. Rejected: Apache-2.0, whose patent grant and NOTICE rules weigh more than a personal library needs; BSD-3, whose extra clause adds nothing here.
- **CI runs `mise run check`,** the local gate, on Linux. Actions are pinned by commit SHA.

## Decided 2026-11-02 — prepare each attempt (#21)

**The host sets up every attempt through one hook: `faden.Options.Prepare func(ctx, faden.Turn) (faden.Setup, error)`.** One runner then serves sessions that each have their own workspace, rules and tools (sidenote's views, invest's dated prompt, forge's coding prompt).

- The runner calls `Prepare` before every attempt, a continuation included. `t.Attempts` is 1 for a first attempt and above 1 for a continuation, so a host can keep the workspace a killed attempt left. The backend reads the result through `Attempt.Setup()`. A runner without a hook gives the zero `Setup`.
- `Setup{Dir, Args, Env, System}`:
  - Claude: `Dir` is the command's directory, `Args` follow the backend's own, `Env` joins the command's, `System` is `--append-system-prompt`. Claude ignores a new system prompt on a resumed session (sidenote, checked with haiku), so `System` reaches it on a session's first attempt only. A host that changes rules mid-session puts the change in the prompt.
  - Codex: `Dir` is `cwd` and `System` is `developerInstructions`, on `thread/start` and `thread/resume`. `Args` follow the backend's own and `Env` joins the command's. The two thread parameters are unverified against a live Codex here; sidenote verified them.
  - llama: `System` is the first system message of each request. It is not an event, so the stored transcript never holds it. `Dir` and `Env` go to `bash`. `Args` has no meaning and is ignored.
- `faden.Command.Dir` is new. `Local` and `container` use it before their own `Dir`.
- A `Prepare` error fails the turn, as a backend error does. The failed attempt is not retried.
- `Setup` is never stored: `Env` may carry a credential.
- Effort and model flags come from the host's own record of the turn, through `Args`. `Turn` gets no new field.

Rejected: one hook per backend config, which wires the same thing in three places; values in the context, which hide a required input from the signature.

## Decided 2026-11-02 — llama reasoning (#22)

**llama keeps `reasoning_content` only in the assistant's native payload.** The next model request carries it, including after continuation rebuilds the messages. Empty reasoning is omitted. `Event.Text` remains the answer; handover and `read_thread` expose no reasoning.

Measured with `mise exec -- go test ./llama -run TestReasoningStaysNative -v`: one invented JSONL run stored 1,695 bytes before and 1,741 bytes after. The server returned “I will add two and three.” with an `add` call, followed by “All done.” The measurement counts the session file, including turn records and events. Reasoning adds storage for the native field; it is not copied into normalized text. This is a storage measurement, not a heap-memory claim.

## Decided 2026-11-02 — llama limits (#23)

**llama limits tool output, answer tokens and model calls.** `Config.MaxOutput` (12,000 bytes), `Config.MaxTokens` (sent as `max_tokens` when not zero) and `Config.MaxSteps` (40) bound a small model's context and a run's length.

- A tool result longer than `MaxOutput` is cut at a rune boundary and ends with `[output cut at N bytes]`. This covers `bash`, host tools and `read_thread`, and error text too.
- **The store keeps the cut result.** The model saw the cut text, so a rebuild matches it. The old `bash`-only cut at 32 KiB is gone; one cut now serves every tool. The cut comes after the exit-code line of `bash`, so a very long output loses that line.
- A tool still returns its whole output to the loop. The loop drops the excess before it stores or sends it.

Measured with `go test ./llama -run StoredBytes -v` (session file with one user message, two answers, one call and one 100 KB tool result): 103,990 bytes with `MaxOutput` set to 1 GiB (no cut, as before), 13,619 bytes with the default 12,000. This is a storage measurement, not a heap measurement.

Rejected: cutting inside each tool, which leaves host tools uncut; a limit in tokens, which needs a tokenizer.

## Decided 2026-11-02 — hosted llama endpoints and retry (#24)

**`llama.Config.URL` includes the API version.** The backend appends `/chat/completions`, so local `/v1` and OpenRouter `/api/v1` bases use the same loop. This supersedes the implicit unversioned base in the first backend design. No host consumes it yet.

**`Config.Key` is an optional bearer token supplied by the host.** It stays in backend configuration and the Authorization header. Errors omit response bodies and redact the key from transport errors, because either can echo credentials.

**A model call gets at most four attempts.** Network failures, 429 and 5xx retry after 1, 2 and 4 seconds. A longer `Retry-After` wins, in seconds or HTTP date form. Other HTTP failures and malformed JSON do not retry. Cancellation interrupts requests and waits. Failed calls emit no events; successful earlier calls in the turn remain stored. Retries do not consume additional `MaxSteps`, which bounds model answers in the loop.

## Decided 2026-11-02 — llama file tools (#25)

**`llama.Config.FileTools` adds `read`, `write` and `edit`.** forge drops pi, so local models need to read, write and edit files where `bash` runs.

- They run through `faden.Runtime` with `sh`, so a container host edits files in the container. Paths are relative to `Setup.Dir`. The runtime needs `readlink`, `head`, `tail`, `tr`, `wc` and `mkdir`.
- A path is cleaned in Go, so `..` cannot hide behind a missing directory. The script then resolves symlinks of the deepest existing ancestor and rejects a path that leaves `Dir`. `..`, an absolute path and a symlink are all rejected.
- `read` takes `path`, `offset` (line, from 1) and `limit` (lines; 0 reads to the end). It is `Safe`. The runtime output is capped at `MaxOutput` plus one byte, so a huge file is never held whole. A file with a NUL byte in its first 8 KiB (binary, image) is an error.
- `write` replaces the file and creates missing directories. `edit` takes `path`, `old` and `new`; `old` must match exactly once. Neither is `Safe`.
- `edit` holds the file in memory, so it refuses files over 1 MiB.
- `FileTools` needs `Runtime`. With `FileTools` on, a host tool named `read`, `write` or `edit` is an error at `New`.

Rejected: reading and writing files from Go, which touches the host's disk instead of the container's; line-numbered `read` output, which models then copy into `edit`.

## Decided 2026-11-02 — turn usage (#26)

**`Turn.Usage` holds totals per model, across attempts.** Backends call `Attempt.AddUsage`; the runner saves totals when an attempt returns, including failure and shutdown. Usage adds no events. A hard kill can lose usage that the backend has not returned to the runner.

**Token totals include their subsets.** Input includes cache reads and writes; output includes reasoning. Claude's result reports uncached input separately, so faden adds cache reads and writes to its input total. Claude's per-model costs take precedence over `total_cost_usd`; that total is a fallback when no model breakdown exists. Codex and llama report no cost, so their cost is zero. Faden does not estimate prices.

**Codex uses `thread/tokenUsage/updated`.** The installed CLI's generated schema defines `tokenUsage.total` and `tokenUsage.last` with camel-case token fields. Faden subtracts the total before the first reported call from later totals. This excludes previous turns and avoids counting repeated snapshots. The thread response supplies the model, falling back to the turn's requested model.

Rejected: usage on each event, which grows with tool activity; keeping native CLI lines, which duplicates their other payloads.

## Decided 2026-11-02 — failure kinds (#27)

**`Turn.Failure` says why a turn failed: `""` (agent or other), `"quota"` or `"unavailable"`.** `Turn.RetryAt` and `Turn.LimitType` hold the reset time and limit name of a quota stop, when the backend knew them. A backend returns `*faden.QuotaError` or `*faden.UnavailableError`, wrapped or not. The runner finds it with `errors.As` and stores the kind with the failed turn. `Turn.Error` keeps the text.

**The host decides when to send a new turn.** The runner never waits for a reset. The host owns the schedule, and forge already has its breaker.

- **Claude:** a result with `error: "rate_limit"`, or `quotaLimits.status: "rejected"`, is quota. `resetsAt` (Unix seconds) and `rateLimitType` fill `RetryAt` and `LimitType`. `error: "authentication_failed"` or a "Not logged in" result is unavailable. Checked live with `--output-format stream-json`: the logged-out CLI puts `error` on the assistant line before the result, so the backend keeps it until the result arrives. The quota shapes come from forge's parser and are tested with invented lines. They were not triggered live.
- **Codex:** a failed turn with `codexErrorInfo: "usageLimitExceeded"` is quota. The reset is `primary.resets_at` (`codex exec --json`) or `primary.resetsAt` (camel case), read from the last notification that carries a rate-limit snapshot. The `app-server` names of both are unverified, because `codex` is not installed here. Codex has no unavailable kind yet.
- **llama:** a 429 after the retries is quota. `RetryAt` is the time of the last answer plus its `Retry-After`, and zero when none was sent. A refused connection after the retries is unavailable.

Rejected: the runner waits for the reset and retries itself.

## Decided 2026-11-02 — cancel one turn (#28)

**`Runner.Cancel(ctx, requestID)` stores a final `Cancelled` status before stopping the backend.** Pending turns never start; running turns receive context cancellation and `Backend.Stop`. Done, failed and cancelled turns return unchanged. The session worker waits for the attempt to return before starting the next turn. A stored cancellation wins over completion and restart.

**The thread records cancellation as `TurnEnded` with text `cancelled`.** This uses the existing event that handover and `read_thread` already expose. Codex sends `turn/interrupt` with its thread and turn IDs before cancelling the runtime context. Claude and llama use context cancellation and the runtime; Claude's control protocol stays out of scope.

## Decided 2026-11-02 — drain before the host stops (#29)

**`Runner.Drain(ctx)` starts no turn and waits for the running turns to end.** A pending turn stays pending, including one submitted during the drain, and the next `Open` runs it. When `ctx` is done first, Drain closes as `Close` does and returns the context's error; unfinished turns stay running and continue on the next `Open`. The host sets the grace period with the deadline of `ctx`.

## Decided 2026-11-02 — watch a session (#30)

**Watch replays completed events, then follows the same sequence from the store.** Each watcher keeps a sequence number and one coalesced wake signal. Appends wake watchers after storage succeeds. Registration precedes replay, so an append during replay cannot fall between history and live delivery. Context cancellation or runner closure ends the watch. A read error closes its channel; the host can reconnect from its last sequence.

**Store gains `NextEvent`, a single-item read.** This breaks store implementers once, rather than loading a whole session for every watcher. JSONL reads one indexed record; SQLite uses `LIMIT 1`. Direct host writes to the store do not produce live notifications.

**Fragments never enter the store.** Claude requests partial messages and reads text and thinking deltas. Codex reads `item/agentMessage/delta`. A backend calls `Attempt.EmitFragment`; the attempt supplies the request ID. A standalone attempt has no watchers. llama remains unchanged.

**Each watcher has 16 fragment slots, each limited to 4,096 text bytes.** Overflow and oversized fragments are dropped. One fragment can also wait at the output, so retained text is at most 69,632 bytes (17 × 4,096). On amd64 each fragment value occupies 48 bytes: 768 bytes for queue values, plus one 48-byte pending value. Request IDs reference the turn's existing string; no watcher copies them. Channel headers, the watcher, cursor, and goroutine add fixed overhead. The output channel has zero slots.

**Completed items retain their original size.** A watcher holds at most one decoded event, never a history slice. Thus its bound is the largest single stored event plus the fixed fragment bound above, independent of session length. There is no new limit on completed event bytes. JSONL decoding temporarily holds one encoded record as well. `TestWatcherFragmentBound` guards queue limits and reports value sizes; `TestWatchReplayLiveAndSlowReader` proves an unread watcher does not block a turn.

Rejected: buffering completed events, which either blocks the turn or loses durable history; polling full history, whose memory grows with session length.

## Decided 2026-11-02 — files and images with a turn (#31)

**`Submission.Files` and `Turn.Files` hold paths relative to `Setup.Dir`.** The store keeps paths only. A backend reads the bytes when it sends an attempt, so a continuation sends the files again. A path outside `Dir`, or a file that cannot be opened, fails the attempt.

**An image is known by its bytes, not its name.** PNG, JPEG, GIF or WebP, at most 10 MB. A larger image goes as a path line. Claude sends an image as a base64 block and Codex as `localImage`. Every other file, and every file for llama, is an `Attached file: <path>` line.

**The user message event and the handover carry the path lines for every file,** including images. They never carry bytes.

**Claude streams each image into its stdin as base64 from the open file.** No image is held whole in memory. `TestImageBytesAreNotHeldWhileSending` guards it.

**Codex sends nothing on a continuation without a prompt:** its thread already holds the files.

## Decided 2026-11-02 — context with a turn (#32)

**`Submission.Context` is stored in `Turn.Context` and sent before the input on every attempt.** Context is host text for that turn, including a continuation after restart. It never enters user message events, handover selection or `read_thread`. Handover budgets count the input without context. Backends add context when sending, without storing another copy in events. Empty context keeps the existing behavior.

## Decided 2026-11-02 — memstore, a store without durability (#35)

**Package `memstore` is a `faden.Store` in process memory.** `memstore.New()` returns an empty one. The runner reads back what it writes: `Wait` polls the turn, `Unfinished` finds the next turn, and handover, continuation, `read_thread` and llama's message rebuild read the events. So the store keeps sessions, turns, events and cursors, and behaves like `jsonl` and `sqlite` while the process lives. A known `requestId` runs once and queued turns run in order.

**Restart does nothing and there is no lock.** A new store is empty, so `Open` finds no turn to continue. One process owns its memory. `Close` keeps the data, so a reopen within one process passes `storetest`.

**It holds every event of every session until the store is dropped.** It suits a runner that lives for one job. A host that runs for months uses `jsonl` or `sqlite`.

Rejected: a store that drops events and keeps only turns and cursors. It works for Claude and Codex on one backend, but breaks llama, handover, continuation and `read_thread`.

## Decided 2026-11-03 — design review of #34

- **`Turn.Progress` holds what changes while a turn runs:** status, attempts, error, failure kind, retry time, limit type and usage. `SubmitTurn` starts a turn at `Progress{Status: Pending}` and `SaveTurn` replaces the whole `Progress`, so a new field needs no store change. Before, each of the three stores listed these fields twice, and the lists disagreed: `SubmitTurn` cleared a caller's failure but kept its usage. `Progress` is embedded, so the stored JSON keys stay the same and old records load. `Turn` literals that set these fields still compile, because Go 1.27 accepts promoted fields in struct literals.

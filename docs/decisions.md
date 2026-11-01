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
- **A backend declares whether it continues without a prompt.** llama.cpp does (faden owns its transcript); Claude Code gets "Continue where you left off." (t3code's text).
- **A CLI left running by a dead host is stopped before the turn continues.** Otherwise two agents write to one workspace. A local process dies with its parent (Linux `Pdeathsig`, process group); a process inside a container is found by a pid file the start command writes and killed by the next owner.

### Backends

- **A backend runs one turn:** it gets the turn, the resume cursor and an emit function, and returns the new cursor. faden stores what it emits.
- **Claude Code:** faden creates the session UUID and passes `--session-id` on the first turn and `--resume` on later ones, so the id is stored before the process starts. `--print` with stream-json input and output; one process per turn (as sidenote does). The cursor also keeps the last assistant message id. The host supplies the permission flags.
- **llama.cpp:** faden's own loop against an OpenAI-compatible `/v1/chat/completions`, ported from invest's analyst. Each model answer is stored; a tool call is stored before it runs and its result after. On continue, the messages are rebuilt from the events. A stored call without a result runs again only if its tool is marked safe; otherwise its result says it was interrupted.
- **llama.cpp tools** are the host's Go functions plus `bash` (run through the runtime, never safe to replay) and `read_thread`.
- **`faden.Runtime` runs one command to its end** and returns output and exit code; a non-zero exit is a result, not an error. `faden.Local` runs it through `os/exec` in a directory and kills the process group when stopped.
- **llama.cpp stores the user message itself** on the first attempt. A continuation that finds no stored user message for its turn fails with "send a new turn", because the runner no longer holds the input then.
- **Codex is parked** until Claude and llama.cpp work. sidenote's `codex app-server` adapter is the starting point.

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

- **Claude resume after a kill:** does `--resume` see the work before the kill? Measure with a live test before the continue prompt is final.
- **Thread reading from a container:** loopback on the host is not reachable from a container without host networking, which forge's boundary forbids. Candidates: a unix socket mounted into the container, or no `read_thread` for container runs.
- **Codex without a prompt:** read how t3code's Codex adapter continues a turn.

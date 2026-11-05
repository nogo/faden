# faden

## Vision

A conversation with an agent is a thread that outlives the process that runs it and the model that answers it.
A restart picks the thread up where it was cut. A switch to another model hands the thread over.
"Den Faden wieder aufnehmen": pick up the thread again.

## Outcome

**A Go module that sidenote, forge and invest can embed to run agent turns that survive a restart and can move between Claude Code, Codex and llama.cpp.**

1. **Turn durability.** A submitted turn is stored before it runs. After a crash, the next process finds every unfinished turn and continues it.
2. **Backends.** Claude Code and Codex run their own loop as a CLI; faden starts them and reads their events. llama.cpp runs in faden's own loop, which stores every model call and tool call inside a turn.
3. **Runtimes.** A CLI runs as a local process or inside a container the host owns.
4. **Handover.** A session can move to another backend. The new backend gets a budgeted selection of the thread, not a summary.
5. **Thread reading.** An agent can read the parts of the thread a handover left out.
6. **Stores.** A `Store` interface with two implementations: JSONL files and SQLite through the host's `*sql.DB`.

**Measure.** A turn killed with SIGKILL at any point finishes after a restart, and a retried submission never runs twice.

## Consumers

- **invest.** The analyst already runs pi's durable design on llama.cpp (`invest/internal/analyst`). It moves onto faden and keeps its sessions in the book through the SQLite store.
- **sidenote.** Chat sessions run on Claude Code and Codex (`sidenote/internal/harness`). faden takes over the adapters and the restart.
- **forge.** Runs agent CLIs in throwaway containers. It uses the container runtime and the Claude backend.
- **produrable** (the proactive gate). A writer step may run as a faden turn.

## Value

- **Nothing is lost to a restart.** The owner leaves runs unattended on a remote machine.
- **The model is replaceable.** Quota, cost or quality decide which model answers next, not the history.
- **Small enough to read.** A host developer reads the module in an afternoon.
- **Grown from need.** The first version is the core; a consumer pulls the next feature.

## Constraints

- **Go, standard library only.** The host brings the SQLite driver. Every other dependency needs a line in `docs/decisions.md` first.
- **The host owns everything outside the thread:** the workspace, the container, credentials, the schedule and the user interface.
- **No secret in the store.** A credential reaches a CLI through its environment or a file the host provides, never through the thread or an argument.
- **One owner per session.** One process runs a session's turns at a time; several processes on one machine share a store.

## Open questions

- Does Claude Code, killed mid-turn and resumed with `--resume`, see the work it did before the kill? The continue prompt depends on it.
- How does a CLI inside a container reach the thread-reading server on the host, without opening a channel forge's boundary forbids?
- How does Codex continue a turn without a prompt? t3code declares it (`promptlessTurnContinuation`); the mechanism is not read yet.

## Parked

- The Codex backend, after Claude and llama.cpp work.
- A summary written by the outgoing model as another handover mode.
- Approvals: a tool call that waits for the owner's answer.
- Moving the consumers onto faden. Each move is its own change in its own repository.

## Rejected

- **A general task engine** (pi durable's task tree, phases, documents and forks). A turn is the only unit the consumers need.
- **A summary as the default handover.** It costs a model call and can invent what was never said. t3code also chose a selection.

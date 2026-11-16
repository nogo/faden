# faden

## Vision

A conversation with an agent is a thread that outlives the process that runs it and the model that answers it.
A restart picks the thread up where it was cut. A switch to another model hands the thread over.
"Den Faden wieder aufnehmen": pick up the thread again.

## Outcome

**A Go module that a host app can embed to run agent turns that survive a restart and can move between Claude Code, Codex, ACP agents and llama.cpp.**

1. **Turn durability.** A submitted turn is stored before it runs. After a crash, the next process finds every unfinished turn and continues it.
2. **Backends.** Claude Code, Codex and any agent that speaks the Agent Client Protocol (ACP) run their own loop; faden starts them and reads their events. llama.cpp runs in faden's own loop, which stores every model call and tool call inside a turn.
3. **Runtimes.** A CLI runs as a local process or inside a container the host owns.
4. **Handover.** A session can move to another backend. The new backend gets a budgeted selection of the thread, not a summary.
5. **Thread reading.** An agent can read the parts of the thread a handover left out.
6. **Stores.** A `Store` interface with three implementations: JSONL files, SQLite through the host's `*sql.DB`, and process memory for short jobs.

**Measure.** A turn killed with SIGKILL at any point finishes after a restart, and a retried submission never runs twice.

## Value

- **Nothing is lost to a restart.** The owner leaves runs unattended on a remote machine.
- **The model is replaceable.** Quota, cost or quality decide which model answers next, not the history.
- **Small enough to read.** A host developer reads the module in an afternoon.
- **Grown from need.** The first version is the core; a consumer pulls the next feature.

## Constraints

- **Linux and macOS.** Hosts run faden on both; CI checks both.
- **Go, standard library only.** The host brings the SQLite driver. Every other dependency needs a line in `docs/decisions.md` first.
- **The host owns everything outside the thread:** the workspace, the container, credentials, the schedule and the user interface.
- **No secret in the store.** A credential reaches a CLI through its environment or a file the host provides, never through the thread or an argument.
- **One owner per session.** One process runs a session's turns at a time; several processes on one machine share a store.

## Open questions

- How does a CLI inside a container reach the thread-reading server on the host, without host networking or another channel out of the container?

## Parked

- A summary written by the outgoing model as another handover mode.
- Approvals: a tool call that waits for the owner's answer.
- Several machines on one store. One machine with several processes works.

## Rejected

- **A general task engine** (pi durable's task tree, phases, documents and forks). A turn is the only unit the hosts need.
- **A summary as the default handover.** It costs a model call and can invent what was never said. t3code also chose a selection.

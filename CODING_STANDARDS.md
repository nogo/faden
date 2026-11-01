# Coding standards

Language rules for faden. Stack, dependency policy and project rules are in `AGENTS.md`.

Base: [Effective Go](https://go.dev/doc/effective_go), [Go Code Review Comments](https://go.dev/wiki/CodeReviewComments), [Google Go Style](https://google.github.io/styleguide/go/). Where this file is silent, they apply.

## Check

`mise run check` (vet, gofmt, tidy, `go test -race`) must pass before a change is done.

## Go

### Structure
- faden is embedded by other systems. The root package `faden` holds the session, turn, event, `Store` and `Backend` types and the runner. Every other concept is a package at the module root (`jsonl/`, `sqlite/`, `storetest/`, `claude/`, `llama/`, `container/`, `handover/`); a package no host imports lives under `internal/`.
- One concept per package, named for it, never `util` or `common`.
- Every package starts with a package comment saying what it is and when to reach for it; `doc.go` when the package has several files.
- An interface is declared where it's consumed and holds only the methods that consumer calls. `Store` and `Backend` are the exceptions: hosts implement them, so they live in the root package.
- A package doesn't construct its own collaborators. The host wires stores, backends and runtimes.

### Naming
- Short receivers, same name on every method of a type.
- Exported identifiers read well with the package: `jsonl.Open`, not `jsonl.OpenJSONLStore`.
- Constants for closed sets of strings, typed when the set is a kind (`faden.Status`, `faden.Kind`).

### Errors
- Messages start with the package: `"faden: submit"`, `"jsonl: append"`.
- Wrap with `%w` and add the context the caller lacks (session, turn, file, line). Don't wrap twice with the same context.
- Callers branch on sentinels (`ErrLocked`, `ErrNotFound`) via `errors.Is`, never on message text.
- No `panic` outside tests and impossible states.

### Durability
- A write the caller relies on is synced before the function returns. Name the sync in a test.
- Store before acting: a turn before it runs, a tool call before it runs, an attempt before a continuation.
- Every new store behaviour gets a case in `storetest`, so all stores keep the same contract.

### Concurrency
- Every goroutine and every child process has an owner that stops it; `Close` waits for it.
- A mutex guards named fields; say which in a comment on the field or struct.
- Tests run with `-race`; a race is a bug, not flake.

### Tests
- Fakes over mocks: an in-memory type with the real interface, asserting on outcomes, not calls. A fake CLI is a small program that prints recorded native output.
- A crash test kills a real child process with SIGKILL and reopens the store; it never simulates the crash in the same process.
- Temporary files via `t.TempDir()`; no fixed paths, no shared state between tests.
- Table-driven only when the cases share one shape; otherwise separate tests with descriptive names.
- Tests that need a real model or a real CLI are `Live` tests, gated by `FADEN_LIVE=1`.
- Wait for a signal (channel, event, condition), never `time.Sleep`.
- A bug fix starts with a test that fails for the bug's reason.
- A pure refactor changes no test assertions.

## Overrides

- None yet. Add one when a review hits the same defect twice.

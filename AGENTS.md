# faden — agent instructions

A Go module that runs agent turns which survive a restart and can move between Claude Code, Codex and llama.cpp. Hosts (sidenote, forge, invest) embed it.
It is a personal project.

## Read first

- `docs/project.md`: the north star (vision, outcome, consumers, constraints, open questions).
- `docs/decisions.md`: what is decided and why. Don't reopen a decision in code; propose the change in that file.
- `CODING_STANDARDS.md`: the Go rules a review checks against.

## Stack

- **Go**, standard library only. The SQLite driver is the host's. Every other dependency needs a line in `docs/decisions.md` first.
- `mise run check` (vet, gofmt, tidy, `go test -race`); `mise run test` is forge's acceptance gate.

## Rules

- **No secrets** in files, commits, test fixtures or the store. A credential reaches a CLI through its environment or a host file.
- **No private data.** Tests use invented sessions and recorded output from invented prompts.
- **Decisions live in `docs/`, edited in the same change as the code** that implements or overturns them. To change one, strike the old line through with the date and the reason.
- **Memory is a requirement.** Hosts run faden for months, and tool output is most of a thread. Keep no whole session or file in memory when one pass or a file offset does. Store and copy the same bytes once. A change that claims a memory win shows the numbers before and after. The rules are in `CODING_STANDARDS.md`, "Memory".
- **Verify before claiming.** Name the command or test that proves it.
- **Small changes.** Do the task, not the adjacent improvement.

## Commits

- `area: imperative summary`, at most 72 characters, in English. No `feat:` or `fix:`.
- A body only when the diff cannot show why: the problem, why this way, what was rejected.

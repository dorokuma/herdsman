<div align="center">

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="./assets/logo-dark.svg">
  <source media="(prefers-color-scheme: light)" srcset="./assets/logo-light.svg">
  <img src="./assets/logo-light.svg" alt="herdsman logo" width="64" height="64">
</picture>

# herdsman

**Archived daemon-backed observability layer for AI coding agents — read structured session ledgers (`pi`, `agy`, `claude`) instead of scraping raw terminal logs. Superseded by `herdr-core` (`hc`).**

[![Status: Archived](https://img.shields.io/badge/Status-Archived-71717a?style=flat-square&labelColor=18181b)](https://github.com/dorokuma/herdsman)
[![Superseded by herdr-core (hc)](https://img.shields.io/badge/Superseded%20by-herdr--core%20(hc)-64748b?style=flat-square&labelColor=18181b)](#migration-guide)
[![License: MIT](https://img.shields.io/badge/License-MIT-52525b?style=flat-square&labelColor=18181b)](./LICENSE)

[Migration](#migration-guide) · [Design](#what-it-built) · [Reference](#historical-reference) · [License](#license)

**English** · [简体中文](./README.zh-CN.md)

</div>

> [!NOTE]
>
> **Archived — 2026-11-11.** This repository is frozen as a read-only historical reference. It is no longer developed and no new versions will be released. For day-to-day work, migrate to `herdr-core`, whose `hc` CLI replaces herdsman's capabilities (`hc ls` / `hc get` / `hc wait` / `hc read` / `hc exec`). The install and run instructions below are kept for historical context only — do not follow them to set up a new system.

## Migration Guide

Day-to-day tasks move from the old `herdsman` CLI to `hc` in `herdr-core`:

| You used to run… (herdsman) | Use instead (`hc`) | What it does |
| --- | --- | --- |
| `herdsman agent list [--json]` | `hc ls` | List agents with latest cached status |
| `herdsman agent get <name>` | `hc get <name>` | Inspect one agent's metadata and compact history |
| `herdsman agent read <name> --limit N` | `hc read <name>` | Read structured session ledger messages |
| Poll `agent list` for a finished outcome | `hc wait <name>` | Block until an agent settles or an outcome lands |
| Pi skill / `/herdsman` owner notifications | `hc exec <cmd>` | Dispatch and observe agent runs directly |

## What It Built

Herdsman was a daemon-backed observability layer for coding agents running in Herdr. Four ideas carried it:

1. **Read the ledger, not the screen.** Instead of scraping terminal scrollback, it parsed each agent's own session store — `pi` session files, `agy` conversation SQLite databases, and `claude` JSONL transcripts — so callers got structured message excerpts and tool results without terminal parsing.
2. **Durable event index.** A SQLite event index with a cursor-based acknowledgement state machine tracked which `agent.*` events had been delivered and settled, so notifications survived restarts.
3. **Courteous wake-ups.** When an agent finished, Herdsman waited if the user was mid-typing (detected half-typed drafts and deferred), and silently filtered transient upstream errors (`429`, `rate_limit_error`, `ECONNRESET`) instead of waking the user for noise.
4. **Plain Go, no Node.** Two small Go binaries (`herdsman` CLI + `herdsman-daemon`) talked over a Unix socket authenticated by peer credentials — zero Node.js runtime required.

## Historical Reference

<details>
<summary><b>CLI usage (historical — superseded by <code>hc</code>)</b></summary>

- `herdsman agent list`: returned the daemon's latest cached status and last user/assistant excerpts for the selected workspace.
- `herdsman agent get <target>`: explicit detail lookup — one agent's metadata, compact history, and latest compact tool result.
- `herdsman agent read <target> --limit N`: explicit history read — latest N user/assistant/`tool_result` messages.

```bash
herdsman agent list --json
herdsman agent get reviewer --json
herdsman agent read reviewer --limit 20 --json
# from outside Herdr, pass a scope:
herdsman agent list --all --json
herdsman agent get reviewer --workspace wB --json
```

`<target>` matched a pane id, terminal id, Herdr live name, or a unique agent kind; `--session <name>` disambiguated across running sessions.

</details>

<details>
<summary><b>Architecture notes</b></summary>

- **Session resolution by official ids only.** Each pane's session history was resolved from the identifier Herdr itself reported — never by scanning agent directories or guessing by mtime. `pi` panes reported a session path used directly; `agy` panes reported a conversation id that resolved to `<home>/.gemini/antigravity-cli/conversations/<id>.db`; `claude` sessions registered their id through `herdsman register-claude` and resolved to `<root>/<project-dir>/<id>.jsonl`. Any other agent deliberately reported no history.
- **Daemon + CLI over Unix socket.** The daemon watched running Herdr sessions (60s rescan), kept the durable agent index in `~/.herdsman`, and served the CLI over a Unix socket with peer-credential checks.
- **Owner-scoped notifications.** One Pi terminal could claim `/herdsman on` ownership per Herdr session to receive cached agent context, pending counts, and automatic wake turns; outcomes during an ownerless window were not replayed.
- **Toolchain.** Pure Go module under `go/` (Go 1.26), `make check` as the single gate (gofmt, `go vet`, `go test -race`, both builds). DB schema lived in `go/internal/db/schema.go` with replayed migrations under `go/internal/db/migrations/`.

</details>

<details>
<summary><b>Build from source (historical)</b></summary>

```bash
git clone https://github.com/dorokuma/herdsman.git
cd herdsman/go
go build -o /tmp/herdsman ./cmd/herdsman
go build -o /tmp/herdsman-daemon ./cmd/herdsman-daemon
```

The daemon watched running Herdr sessions and kept runtime state under `~/.herdsman` (override with `HERDSMAN_HOME`). These steps are preserved for archaeology — the project is archived; use `hc` from `herdr-core` instead.

</details>

For deeper design context see [AGENTS.md](./AGENTS.md) and [docs/plans/](./docs/plans/).

## License

[MIT](./LICENSE)

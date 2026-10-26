---
name: herdsman
description: "Inspect the status, progress, latest messages, compact structured history, and recent tool results of coding agents managed by Herdr using Herdsman. Use whenever the user asks what another Herdr-managed coding agent is doing, whether it is working, blocked, idle, or done, what it recently reported or changed, or needs structured context before coordinating with that agent. Also use outside Herdr when the user provides an explicit Herdr workspace or session scope for agent inspection."
compatibility: "Requires the Herdsman CLI and daemon. Probe Herdr via `herdr pane current` / `herdr session list` (nested shells often lack HERDR_ENV). Explicit Herdsman workspace or session scopes work outside Herdr."
---

# Herdsman agent inspection

Use Herdsman for structured coding-agent status, compact message history, and recent compact tool results. Use the local `herdr` skill for live workspace, tab, pane, terminal input/output, focus, spawn, and wait operations.

## Ensure the daemon is running

Check the daemon before the first Herdsman query:

```bash
herdsman daemon status
```

If the JSON response has `state: "stopped"`, the daemon is not running. Herdsman has no CLI start/stop command. On a production host the daemon is managed by the systemd unit `herdsman.service` and is meant to stay running (`systemctl restart herdsman.service`); for a development or throwaway environment, run the daemon entrypoint in the foreground with an explicit temporary data directory (`HERDSMAN_HOME=/tmp/<name> node <package>/dist/src/cli/herdsman-daemon.js`) instead of the production one. Do not stop or restart a daemon that is already running unless the user asks.

## Workspace orchestrator ownership (`on` / `off` / `status`)

Inside a Herdr pane (`HERDR_ENV=1`, `HERDR_SOCKET_PATH`, `HERDR_PANE_ID`, `HERDR_WORKSPACE_ID`), you can inspect or toggle whether the current terminal is the active Herdsman orchestrator owner for its workspace:

```bash
herdsman status
herdsman on
herdsman on --if-unowned
herdsman off
```

When `HERDSMAN_AUTO_ON=1` is set on main-agent startup (`pi` or `agy`), the session automatically claims workspace ownership if the workspace does not already have an active owner.

## Select the scope

Prefer the current Herdr workspace when the CLI can see it (probe with `herdr pane current --current` / `herdr session list`; nested shells often lack `HERDR_ENV`):

```bash
herdsman agent list --json
```

If the current workspace cannot be determined, do not guess or fall back to `--all`. Ask for an explicit scope.

Outside Herdr, or when the user names a scope, pass it explicitly:

```bash
herdsman agent list --workspace <workspace-id> --json
herdsman agent list --all --json
```

Use `--session <name>` when workspace ids or agent targets are ambiguous across running Herdr sessions.

## Inspect an agent

Start with `agent list`. Each row contains an optional Herdr live `name`, such as `reviewer`, and a runtime `agent` kind, such as `codex`. Herdsman resolves an exact pane id, terminal id, or Herdsman agent id first; an exact live name second; and a unique runtime kind only as a fallback. Use an exact pane id or terminal id when the caller has already selected a row. Otherwise use its live name when present. Do not call `agent list` repeatedly in a polling loop, and inspect `updatedAt` when freshness matters.

Use `agent get` or `agent read` for explicit current detail after selecting the exact target.

```bash
herdsman agent get <target> --json
herdsman agent read <target> --limit 20 --json
```

Add the same `--workspace` and `--session` scope used for `agent list` when operating outside the current Herdr workspace.

`agent get` returns metadata, status, compact history, and the latest compact tool result. `agent read` returns recent user, assistant, and compact `tool_result` messages; it does not return raw full terminal output.

Agent status uses `working`, `blocked`, `idle`, `done`, or `unknown`. `done` means the agent finished and its pane has not yet been viewed.

## Coordinate through the local Herdr skill

When a task also requires live terminal output, pane control, input, spawning, focus, or waiting, use the local `herdr` skill (`~/.pi/agent/skills/herdr/SKILL.md`). Probe with `herdr session list` / `herdr pane current --current`. Do not treat `herdr --skill` upstream text as authoritative when it conflicts with the local skill.

## Boundaries

- Herdsman returns agents from running Herdr sessions only.
- Use Herdsman for structured semantic history, not raw terminal fidelity.
- Herdsman does not start, prompt, wait for, focus, or send terminal input to agents.
- Use the local `herdr` skill for live terminal state and control.

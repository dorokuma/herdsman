---
name: herdsman
description: "Inspect the status, progress, and recent output of coding agents managed by herdr-core (hc). Use whenever the user asks what another hc-managed coding agent is doing, whether it is working, blocked, idle, or done, or needs structured context before coordinating with that agent."
---

# herdr-core (`hc`) Agent Inspection

Legacy `herdr` and `herdsman` have been replaced by `herdr-core` (`hc`). Use `hc` directly to inspect and coordinate with child terminals and coding agents.

## Core Inspection Commands

1. **List all live terminals** (`--all` includes finished terminals):
   ```bash
   hc ls
   hc ls --all
   ```
   When piped or run via tool, `hc ls` returns JSON with `terminals: [{address, name, parent, agent, cwd, state, status, turn, ...}]`.

2. **Inspect a single terminal's state and status**:
   ```bash
   hc get <address-or-name>
   ```
   Key fields:
   - `state`: `"live"` or `"finished"`
   - `status.state`: `"working"`, `"idle"`, `"done"`, `"blocked"`, or `"error"` (reported via OSC 7501)
   - `parent`: parent terminal address (matches `$HC_TERMINAL_ID` for children started by this session)

3. **Read terminal screen and history**:
   ```bash
   hc read <address-or-name> --lines 120
   ```

4. **Send a message or instruction to a running agent**:
   ```bash
   hc send <address-or-name> "<message>"
   ```

5. **Clean up a completed child terminal**:
   ```bash
   hc kill <address-or-name>
   hc rm <address-or-name>
   ```
   Only clean up child terminals this session started: `parent` in `hc ls`
   must equal your own terminal id (`$HC_TERMINAL_ID`). Never touch terminals
   owned by other sessions, and do not run `hc server stop`/`hc server restart`
   or any other service lifecycle command.

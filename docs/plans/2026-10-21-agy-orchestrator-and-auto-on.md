# Agy Orchestrator, Turn-Completion Hook & Auto-On Plan

**Status:** Completed

**Goal:** Enable Antigravity CLI (`agy`) to participate in Herdsman both as a low-latency observed agent (`agent.turn.completed` on `Stop`) and as a full workspace orchestrator owner (`herdsman on|off|status` + `PreInvocation`/`Stop` lifecycle hooks), while automatically enabling Herdsman ownership on new main-agent sessions (`pi` and `agy`) when the current Herdr workspace has no active owner.

**Architecture:** Extend `@dorokuma/herdsman` with `subscriberKind: "pi" | "agy"` presence registration, Herdr-pane-bound owner lease semantics for hook/CLI-driven `agy` owners, CLI orchestrator subcommands (`on`, `off`, `status`, `agy-hook`), and `HERDSMAN_AUTO_ON=1` unowned-scope auto-claim in both `herdsman-pi` and `agy` hooks.

**Tech Stack:** TypeScript (ESM, NodeNext), Node.js >= 22.12.0, Vitest, Biome.

## Progress

- [x] **Step 1:** Create active plan and feature branch `feature/agy-orchestrator-and-auto-on`.
- [x] **Step 2:** Extend RPC contracts, schemas, `ObservabilityRpcServer`, and `AgentIndexService` for `agy` presence, lease liveness, and turn-completion signal wakeup.
- [x] **Step 3:** Implement `src/agy/hook.ts` (`PreInvocation` and `Stop` lifecycle hook handlers) and wire CLI commands (`herdsman on|off|status` and `herdsman agy-hook`) in `src/cli/herdsman.ts`.
- [x] **Step 4:** Add `HERDSMAN_AUTO_ON=1` unowned auto-claim support to `packages/herdsman-pi/src/index.ts`.
- [x] **Step 5:** Add unit and integration tests covering `agy` RPC presence/lease, `herdsman on|off|status`, `agy-hook`, and `herdsman-pi` auto-claim; pass `pnpm check`, `pnpm build`, and `pnpm package:check`.
- [x] **Step 6:** Record architectural decision note in `.agents/notes/`, wire local `agy` / `pi` wrappers, `hooks.json`, and `SKILL.md`, and address `reviewer` + `oracle` dual-review findings.

## Next steps

1. Implement Step 2 (`contracts.ts`, `schemas.ts`, `observability-server.ts`, `agent-index-service.ts`).
2. Implement Step 3 (`src/agy/hook.ts`, `src/cli/herdsman.ts`).
3. Implement Step 4 (`packages/herdsman-pi/src/index.ts`).
4. Run full verification (`pnpm check`, `pnpm build`, `pnpm package:check`).

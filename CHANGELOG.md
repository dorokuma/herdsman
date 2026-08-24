## 0.8.5

- 修复空 assistant 历史回传、用户 ESC 后事件无限重传，以及收敛前重复 reclaim 投递。

## 0.8.4

- 增强 daemon 的 orchestrator ack 拒绝与事件投递结构化日志，记录拒绝原因、期望事件及投递批次摘要。
- orchestrator 游标推进时批量清理游标以下遗留 pending/delivered 事件，避免垃圾行污染候选扫描。
- 调查 register 重放、pending 列表来源及 AgentIndexService 上下文快照路径；未发现服务端缓存或重放已删除事件的路径。


- Turn completion signal (route 2): the Pi extension now notifies the daemon after its own final assistant message is written to the session file (bounded stat fallback, timeout still signals with actual status), and the daemon waits up to a bounded window for that signal before emitting `agent.done` / `agent.blocked` events for Pi agents, refreshing the agent right before appending so outcomes carry a non-empty `lastAssistantMessage`. Older extensions that never signal keep working: the daemon times out and generates events as before with a warning.
- 修复陈旧 turn-completion 信号被下一轮等待错误命中的竞态，仅接受等待开始后记录的新信号。
- 修复 turn-completion RPC 信任客户端自报身份的问题，服务端改用 socket 已注册的 Pi presence 身份记录信号。

## 0.7.0 (2026-08-23)

- 接入 grok/agy 历史读取与发现，新增 `grokHome` 元数据及安全校验，移除 shepherd 命名残留。


- Fix an infinite re-wake loop: acknowledging an invalidated orchestrator event is now rejected with a distinct "no longer pending" error, and the extension treats that as terminal - it prunes the outcome from its pending set and advances past the delivered batch instead of retrying forever. Acknowledged ids returned by the server also prune stale pending events on register and after each ack (1aa2612..cce0a5a).
- Suggestion-tier improvements: Pi id-kind session refs resolve their file by id before falling back to discovery; candidate cwd comparison normalizes trailing/repeated slashes; pinned-context retain compares all entries sharing a pane id; snapshot excerpts are capped at 2000 characters; unchanged context snapshots are no longer re-pushed (1aa2612).

## 0.6.5

- Hotfix: the pi extension crashed on load ("Cannot find module '@/shared/json-lines.js'") because 0.6.4 introduced a cross-package path alias that does not resolve when the extension is loaded as standalone TypeScript. The JSON-lines decoder is now vendored inside the herdsman-pi package (same 1 MiB semantics), the alias import is gone, and a regression test loads the extension independently and rejects an oversized frame (4d41230).

## 0.6.4

Full re-audit hardening (correctness / security / reliability), all findings verified against source:

- Delivery cursor deadlocks: transient disconnects no longer advance the failed-wake cursor past unacknowledged events, so batches are redelivered and acknowledged after reconnect; rejected acks only drop the batch (266fdc9).
- Idle events now count as outcomes only when transitioning from working, aligning the delivery predicate with acknowledgement (266fdc9).
- Replacing an agent invalidates its pane's events, so orphans can neither be delivered nor wedge the cursor (266fdc9).
- Authoritative path session refs are validated against the session allowlist at registration; invalid refs fall back to discovery (266fdc9).
- Pending scans paginate past noise windows instead of truncating at 1000 rows (266fdc9).
- History discovery is bounded (depth 4, 2000 files, 256KB cwd prefix) and hardened: role session roots require daemon-owned roots and regular, unlinked, owner-matching files (726d92f).
- Data directory permissions are enforced (0700 home, 0600 db/wal/shm); the socket is created under a private umask (726d92f).
- Reconnect backoff resets once a subscription is established, and protocol-incompatible daemons stop reconnect loops until the next session (726d92f).
- Readiness failures escalate SIGTERM to SIGKILL and only remove the pid file after death is confirmed (726d92f).
- Changed occupied-session sets force history re-discovery, replacing stale fallback snapshots; pinned-context retain detects pane reuse by agent id (266fdc9, 726d92f).

## 0.6.3

- History ownership: Pi fallback discovery now scans dispatched role session roots, requires an exact cwd match, and skips session files already owned by another agent, so a worker pane can no longer be attributed the orchestrator's own transcript (f47e9c1).
- Late session refs are treated as an identity change, replacing snapshots that were built from a fallback guess (f47e9c1).
- Closed panes disappear from injected context immediately: the pinned snapshot is intersected with the newest one by pane identity, with agent reuse detected via agent id (f47e9c1, 31045b4).

## 0.6.2

- Acknowledgement: allow the orchestrator cursor to advance past events that became undeliverable after delivery (for example when a worker pane is retired), instead of rejecting the ack and freezing the cursor. Skipping ahead of still-deliverable events is still refused, and sealed events remain unacknowledgeable (9007766).
- Lint and formatting debt from the hardening rounds cleared; biome check is now clean and enforced by the pre-commit hook (288adab).

## 0.6.1

- Delivery pipeline: unified the deliverable-event predicate across pending discovery, acknowledgement, and publication so filtered events can no longer pin the queue (ddbc5ca, 49b6218).
- Wake correctness: dropped empty wake turns, isolated interactive Pi observers from dispatched roles, and stopped aborting in-flight turns on transient disconnects (53c3513, 302c837, 97a54bf).
- Observation chain: rebuild the Herdr client with exponential backoff on subscription failure, discard stale session refreshes via a mutation epoch, and harden session-path classification (3b1e922).
- Security and process hardening: private observability sockets, non-preemptive scope claims, bounded JSON-lines frames, exclusive daemon PID startup, readiness polling, SIGKILL escalation, SQLite WAL/busy timeout, and log rotation (adff7ef).
- Daemon status now reports a reachable daemon without a PID file as running and exposes pidFileMissing (5a1f78d).
- Test suite grew from 240 to 255 cases across 33 files, covering disconnect, frame-limit, path-classification, and acknowledgement-cursor regressions (5f47567).

## 0.6.0

Forked from @ryonakae/herdsman at v0.5.1 (dfdd3a2). Previous history is inherited from upstream.

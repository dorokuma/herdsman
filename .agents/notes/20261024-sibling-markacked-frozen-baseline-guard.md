---
status: active # active | superseded
superseded_by: ""
supersedes: ""
# 模块可选值: observability, daemon, cli, config, db, herdr, shared, herdsman-pi, herdsman-herdr-plugin, release
模块: "observability"
---

# 同批次兄弟 markAcked 范围扫表对未确认 frozen-baseline 终态守卫的影响

## 一句话结论

`AgentIndexService` 在查询已投递终态轮次历史（`deliveredTerminalHistories`）时，必须将 `type = 'agent.idle'` 显式约束为 `json_extract(payload_json, '$.from') = 'working'`（与 `isDeliverableAgentEvent` 的可投递终态事件集合一致，并复用 `#invalidatePaneCore` 与 `latestCompletedTurnEvent` 对 `agent.idle` 的 `$.from = 'working'` SQL 谓词），不能仅凭 `status in ('delivered', 'acked')` 排除非完成轮次的 `unknown -> idle` / `done -> idle` / `blocked -> idle` 行——因为同工作区兄弟子代理先完成并被 `markAcked(eventId, scope)` 确认时，会按 `id <= ?` 将同工作区内更早的永不投递 `agent.idle` 行批量标为 `status = 'acked'`。

## 背景

生产 `w37` 同批次双子代理派工真实故障（`/root/.herdsman/state.db` 事件 `72733`–`72751`，计划 `39770`）：

1. 主代理在 `w37` 同时派发两个子代理 `w37:p1H`（verifier）与 `w37:p1J`（reviewer）。
2. `w37:p1H` 与 `w37:p1J` 启动时分别产生 `agent.idle`（`from: "unknown", to: "idle"`，`id=72734` 与 `id=72736`）。`AgentEventStore.append()` 插入时初始状态均为 `status = 'pending', deliverable = 1, delivery_attempts = 0`；因 `isDeliverableAgentEvent` 对 `from != 'working'` 的 `agent.idle` 返回 `false`，`reservePending` 从不会投递它们。
3. `w37:p1J` 先完成首轮（`id=72742`，`agent.done`）并唤醒主代理；主代理 ack `72742` 时，`AgentEventStore.markAcked(72742, scope)`（`src/db/agent-events.ts:731-739`）执行 `where herdr_session_name = ? and workspace_id = ? and id <= 72742`，将同工作区 `w37:p1H` 的启动行 `id=72734` 一并改成 `status = 'acked', deliverable = 0`。
4. 随后 `w37:p1H` 完成首轮（计划 `39770`），其最终回答在进入状态事件计划时已落盘在 frozen baseline 中（unconfirmed frozen-baseline 分支，`src/observability/agent-index-service.ts:2909-2976`）。
5. 原 `deliveredTerminalHistories` 查询只写了 `type in ('agent.idle', 'agent.done', 'agent.blocked') and status in ('delivered', 'acked')`，把被批量扫成 `acked` 的启动行 `id=72734` 查出，导致 `hasDeliveredTerminalRow` 变为 `true`，`if (diskDeliverable && (!hasDeliveredTerminalRow || confirmedTerminal))` 为 `false`，将 `w37:p1H` 的首轮回答清空并打上 `degraded: true`（`no_advance_from_input`），随后 `invalidateById(72747, "degraded_retry")` 作废，造成主代理丢失 `w37:p1H` 的完成唤醒。

## 决策

- 在 `src/observability/agent-index-service.ts` 的 `deliveredTerminalHistories` 查询中，将事件类型过滤条件从 `type in ('agent.idle', 'agent.done', 'agent.blocked')` 收紧为：
  `(type in ('agent.done', 'agent.blocked') or (type = 'agent.idle' and json_extract(payload_json, '$.from') = 'working'))`
- 与 `src/db/agent-events.ts` 中的 `isDeliverableAgentEvent`（第 65 行）、`#invalidatePaneCore`（第 341-342 行）和 `latestCompletedTurnEvent`（第 503-504 行）对“已完成可投递轮次”的定义保持严格一致。

## 被放弃的方案（必填）

- **修改 `markAcked(id, scope)` 跳过 `from != 'working'` 的 `agent.idle` 行**：会让这些永不投递行永远留在 `status = 'pending', deliverable = 1`；虽然 `reservePending` 与 `nextDeliverableAfter` 过滤了它们，但让范围结算保留已过期的 `pending` 行会增加表状态噪音，且不能解决历史库里已被标为 `acked` 的行。在读取端按可投递终态轮次定义精确过滤更完备。
- **在 `deliveredTerminalHistories` 仅靠 `delivery_attempts >= 1` 过滤**：虽能排除未经 `reservePending` 投递的被扫行，但语义不如直接与 `isDeliverableAgentEvent` / `latestCompletedTurnEvent` 对齐 `type` + `$.from = 'working'` 清晰直接。

## 来源

- 生产库证据：`/root/.herdsman/state.db` 中 `agent_events` 行 `72734`、`72742`、`72747` 与 `status_event_plans` 行 `39770`。
- 代码：`src/observability/agent-index-service.ts`（`deliveredTerminalHistories` / `hasDeliveredTerminalRow`）、`src/db/agent-events.ts`（`isDeliverableAgentEvent` / `markAcked` / `latestCompletedTurnEvent`）。
- 回归测试：`test/integration/turn-completion-signal.test.ts`（`A2: an unknown -> idle row swept by a same-workspace markAcked does not gate the frozen-baseline release`）。

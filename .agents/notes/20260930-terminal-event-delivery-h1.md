---
status: active # active | superseded
superseded_by: ""
supersedes: ""
# 模块可选值: observability, daemon, cli, config, db, herdr, shared, herdsman-pi, herdsman-herdr-plugin, release
模块: observability
---

# 终态事件投递修复（H1）：孤儿终态行、保留语义与唤醒口径

## 一句话结论

plan 走到终态时 `agents` 行可能已被物理删除（关页/退役），旧实现会因 `FOREIGN KEY constraint failed`
丢掉整条终态事件；现在终态事件降级为**孤儿行**（`agent_id = null`、原 id 留在 payload）写入，`agent.failed`
保留到**首次投递尝试**为止，并且 `agent.failed` / `agent.discarded` 都能唤醒编排者。

## 背景

- 相关前序笔记：[`wake-delivery-r4f1-remaining-risks.md`](wake-delivery-r4f1-remaining-risks.md)（保留行链路、
  `nextDeliverableAfter` 取件闸门、跨批次压制）。
- H1 症状链：`agents` 行被物理删除 → `agent_events.agent_id` 外键 `on delete set null` 清空 →
  `#appendPlanFailedEvent` / `#appendPlanDiscardedEvent` 仍按原 agent 记录落库 → `FOREIGN KEY constraint failed`
  → 终态事件从未入库 → 编排者永远看不到这次失败。存量 19 条 plan 处于同一状态。
- 另有三个"沉默点"：`nextDeliverableAfter` 的取件闸门与 SQL 守卫都排除 `agent_id is null`，
  `markAcked(id <= cursor)` 会把**未投递**的孤儿行直接标成已 ack；reconciler 的 300s 新鲜度规则会把新造的
  孤儿 `failed` 行直接物理删除。

## 决策

1. **落库解耦（取舍 4 的落地点）**：`#appendPlanFailedEvent`（`src/observability/agent-index-service.ts:2245`）、
   `#appendPlanDiscardedEvent`（`:2291`）在 `agents` 行缺失时传 `agentId: null`（`resolved.agentRowPresent` 判定，
   `:2263` / `:2305`），两处都在 payload 保留原始 id（`:2273` / `:2313`）。`AgentEventStore.append` 再加一层
   防御降级：入参 agentId 对应行不存在 → 写为孤儿并 `console.warn`（`src/db/agent-events.ts:234`），
   **不许静默**（`FOREIGN KEY` 崩溃改成了显式降级 + 日志）。
2. **顺序守卫成对改（致命修正 1）**：`nextDeliverableAfter` 的 SQL（`src/db/agent-events.ts:526,531,555`）
   与 JS guard（`:569`）都放行终态失败行。原因是两者缺一不可：只改 SQL 会让 JS guard 继续跳过孤儿行，
   `#ack` 算出"下一个可投递 = 后到的事件"于是放行 ack，`markAcked(id <= cursor)` 就把未投递的孤儿行吞掉。
3. **保留语义成对统一（取舍 3，致命修正 2）**：只保 `agent.failed`，窗口语义是"保留到首次投递尝试为止"，
   三处一致——
   - `#invalidatePaneCore` 步骤 2（`src/db/agent-events.ts:296-313`）：`(done/idle from=working and created_at >= now-300s)
     or (type = 'agent.failed' and delivery_attempts = 0)`；
   - reconciler `isRetainable`（`src/daemon/agent-event-reconciler.ts:243`，用于 `:155`，并在"同 pane 存在但跨代冲突"
     分支 `:139` 用它替代物理删除）；
   - `reclaimDelivered`（`src/db/agent-events.ts:643`）显式加 `delivery_attempts >= 1`：`reservePending` 是唯一把行
     置为 `delivered` 的写入者且必增 `delivery_attempts`，所以这条守卫表达的就是"没投过就不能离开保留窗口"。
   - `agent.discarded` **不保留**；**未新增 schema、未做迁移**（`delivery_attempts` / `deliverable` / `invalidated_reason`
     都是既有列）。
4. **stitch 加代际 + 时序过滤（致命修正 3）**：`#minimalAgentFromLatestEvent`（`:691`）改成按
   `(herdr_session_name, pane_id, pane_generation)` 匹配并加 `created_at <= plan.created_at` 上界。原实现按
   `agent_id` 匹配——而 `ON DELETE SET NULL` 已经把 `agent_id` 清空，所以旧 stitch 恒返回空；只按 paneId 匹配
   则会把旧失败挂到同 pane 的新实例上（代际 + 时序两个约束同时需要）。两处调用方都传 plan 行 `created_at`。
5. **存量 19 条静默入库（取舍 2）**：`#backfillFailedPlanEvents`（`:530`）/ `#backfillDiscardedPlanEvents`（`:604`）
   加 per-row try/catch（旧实现一条抛错会中断整轮补录）、写入后立即 `markAcked`（`:575` / `:629`，只可查、
   不投递、不打扰当前 owner），并报数（`:589` / `:643`，`backfilled/skipped/total`）。**未手工改生产库**。
6. **死信（取舍 4）**：`publishAgentEvent`（`src/daemon/observability-server.ts:244`）去掉 `agentId` 必需门槛
   （保留 `workspaceId` / `terminalId` 要求）；缺投递范围时终态事件仍落库 + `console.warn` 显式记录
   （`:252`，不做 schema 变更 —— 没有可路由的 scope，永久不可投递是接受的）。
7. **把静默失败变可见**：`drainPendingPlans` 的 `allSettled` rejected 项（`:521`）、`#scheduleWaitingHistoryRetry`
   失败（`:331`，原来只是 `console.debug`）。
8. **唤醒口径（取舍 1）**：`packages/herdsman-pi/src/wake.ts:53` 让 `agent.discarded` 也返回 `kind: "failed"`。
   既有压制/死信语义（`presentedEventIds`、`isWakeableEvent`、`failedWakeThroughEventId`、`completedPaneIds`
   pane 级压制、上游错误抑制）**保持原样不动**，身份标签退化链（agentId → paneId）不变。

## 被放弃的方案（必填）

1. **给 `agent_events` 加可空外键 / 允许 dangling agentId 的 schema 变更**：取舍明确要求不新增 schema、不做迁移；
   降级为 `null` + payload 保留 id 已经满足"可查、可唤醒、可追溯"。
2. **只改 SQL 或只改 JS guard**：只改一处比不改更糟——`#ack` 会放行 cursor ack，把未投递的孤儿行静默标成 acked
   （见决策 2 的原因链）。
3. **保留所有终态失败 + 无界重投**：会与既有 7 天硬上限 / 1 小时 invalidated 回收打架，且孤儿 `failed` 一旦投递过一次
   就该回归普通生命周期，所以窗口以"首次投递尝试"为界，而不是时间窗。
4. **孤儿行的补录也走投递**：会让重启后的存量 19 条去唤醒早已结束的会话，取舍 2 明确要求静默入库。
5. **stitch 只按 paneId 匹配（不代际/不加时间上界）**：简单，但同 pane 跨代复用会把旧失败挂到新实例的
   terminal/workspace 上，正是 oracle 点出的致命项。
6. **`agent.discarded` 保持不唤醒**：取舍 1 拍板要求两者都可唤醒（放弃等待同样是"结果永远不会到达"）。

## 来源

- 任务合同（H1 实现单，含 owner 拍板的 4 条取舍与 oracle 的 3 条致命修正）：`/tmp/dispatch-pi-agent-<id>.md`（本轮派发件）。
- 前序笔记：[`wake-delivery-r4f1-remaining-risks.md`](wake-delivery-r4f1-remaining-risks.md)（待办 1「保留行
  `reserve→delivered→acked` 主链路 + reconciler 保留分支缺测试」已由本轮
  `test/integration/agent-event-delivery-lifecycle.test.ts:645`、`test/integration/startup-reconcile.test.ts:468` 覆盖）。
- 回归测试：`test/integration/agent-index-service.test.ts:3224,3307,3386`、
  `test/integration/startup-reconcile.test.ts:468,515`、`test/integration/agent-event-delivery-lifecycle.test.ts:599,645,685,726`、
  `test/integration/observability-rpc.test.ts`（孤儿终态事件推送）、`test/unit/herdsman-pi-wake.test.ts`。

## 遗留 / 下一轮

> 本段口径：双审第二关（oracle）对 H1 的裁定。除 F1 本轮已在本分支修掉外，其余各项**不在 H1 内扩范围**。
> 后续轮次（R5 及 oracle 的 H1/R2 反问）产生的观察项、挂账 chore 与两批提交边界不在此重复，
> 集中登记于 [双审观察项与挂账台账](20260930-terminal-event-delivery-open-items.md)。

0. **F1（跨代关页吞掉未投递孤儿 failed 行）——本轮已在 H1 内修掉**：`src/db/agent-events.ts:308-312`
   （`#invalidatePaneCore` 步骤 2）把 `(${genCondition})` 从整段最外层移入 done/idle 分支，
   使 `or (type = 'agent.failed' and delivery_attempts = 0)` 不再受代际条件约束。
   现在同 paneId 以**新代际**关页时，旧代际/空代际的**未投递**孤儿 failed 行保持
   `deliverable = 1, reason = RETAINED_OUTCOME_PANE_CLOSED`（修前被吞成 `PANE_CLOSED`，
   并在 1h 后被 `deleteInvalidatedOlderThan` 物理删除，终态结果直接丢失）。
   补丁与 oracle 给的最小修复逐字符等价（`/tmp/oracle-evidence/minimal-fix.patch`，4 行 SQL 重排、参数顺序不变）；
   回归用例：`test/integration/agent-event-delivery-lifecycle.test.ts`
   「pane close with a new generation still retains unattempted failed rows of older and null generations」
   （施修前 FAIL：`expected { deliverable: 1, invalidatedReason: 'RETAINED_OUTCOME_PANE_CLOSED' } … received { deliverable: 0, PANE_CLOSED }`；修后 PASS）。
1. **孤儿 `failed` 行的 `pane_generation` 可能为 null**：legacy 关页路径下 plan 行的代际为 null，
   stitch 走 `pane_generation is null` 分支；若将来上游开始上报代际，需要同步前序笔记（a）（b）的词法比较收紧。
   → **词法收紧留 Phase 2**；其中「步骤 2 的代际夹带」这一半按 F1 本轮已修（见上）。
2. **`agent.failed` 保留窗口没有 TTL 上限**：窗口由「首次投递尝试」而非时间界定，若 owner 长期不取件，
   行会一直在保留态直到 `deleteSettledOlderThan` 的 7 天上界。是否要为「从未被取件」的孤儿失败行补一个上限
   （例如 1 小时无取件即回收）需 owner 拍板。
   → **oracle 裁定：留 Phase 2、owner 拍板，H1 内不加短 TTL**（本轮不动）。reviewer 亦标记「保留窗口 TTL 属 owner」。
3. **死信只落库 + 日志，没有可观测面**：`journalctl` 里能 grep 到，但没有 metric/`last_failure_code`，
   取舍 4 明确不做 schema 变更，故未加。→ **oracle 裁定：Phase 2**。
4. **`#scheduleWaitingHistoryRetry` 的 `console.warn` 无测试**：该分支需要构造排程回调抛错，本轮未补。
   → **oracle 裁定：Phase 2**。
5. **跨批次 pane 级压制**（前序笔记第 8 项 ①）：本轮未触碰，噪音级问题保持原状。
   → **oracle 裁定：保持原样**（H1 不覆盖）。
6. **reviewer 建议（留档，不在 H1 内做）**：legacy 空代际的 stitch 集成用例待补（与第 1 条同源）；
   保留窗口 TTL 的取值属 owner 决策（同第 2 条）。
7. **oracle 反问的两条结论**：`agent.discarded` 的文案 `failed (discarded)` 并入 Phase 2；
   新 scope 首次 claim 跳过历史 = 既有语义，H1 不再覆盖。

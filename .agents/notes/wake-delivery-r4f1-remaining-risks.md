# Wake Delivery R4F1 剩余观察项

依据：合同 `/tmp/wake-fix-plan-20260929-final1.md`（r4-final.1）、第一轮复核（`wake-fix-review-findings-20260929T1730.md`）、第二轮独立第二意见（`wake-fix-recheck-oracle-report-20260929.md`）、第二轮挑毛病复核（`herdr-dispatch-tabs/forensics-wP-t9-full.txt`）。

## 非阻塞观察


5. **（补记）`nextDeliverableAfter` 未传 `getAgent` 时的保留行过滤缺陷（本轮已修）**
   - 位置：`src/db/agent-events.ts:485-548`（`agentFilter` 在 `:497-511`，取件闸门在 `:524`，取件循环在 `:531-546`）。
   - 缺陷：第二轮曾把 `agentFilter` 写成 `input.getAgent ? "" : "and (status != 'invalidated' or deliverable != 1)"`，对保留行（`status='invalidated' and deliverable=1`）恒为假 → **未传 `getAgent` 时保留行被无条件排除**，方向与合同 §2 要求相反，且原 `exists (select 1 from agents ...)` 守卫被无条件删除。
   - 修法（本轮）：`agentFilter` 改为「保留行放行 + 非保留行恢复 `agents` 存在性检查」：
     `and ((status = 'invalidated' and deliverable = 1) or exists (select 1 from agents where agents.id = agent_events.agent_id and agents.herdr_session_name = agent_events.herdr_session_name and agents.workspace_id = agent_events.workspace_id and agents.pane_id = agent_events.pane_id))`。
   - 同时发现并修复取件链上的第二个闸门：`agent_events.agent_id` 外键为 `on delete set null`（`src/db/schema.ts:102`），关页物理删除 `agents` 行会把保留行的 `agent_id` 清空，而原 SQL 的 `and agent_id is not null` 会把保留行连同一起排除。现改为 `and (agent_id is not null or (status = 'invalidated' and deliverable = 1))`，并让取件循环对 `agentId === null` 的保留行继续用事件自身元数据判定（`isDeliverableAgentEvent` 的保留分支接收 `undefined` agent，与合同 §6.1 的解耦一致）。
   - 回归测试：`test/integration/agent-event-delivery-lifecycle.test.ts` 用例 `nextDeliverableAfter returns a retained row after the agents record is physically removed`（修复前红：返回 `undefined`；修复后绿）。
   - 残余：仓内两处生产调用（`src/observability/agent-orchestrator-service.ts:111,201`）均传 `getAgent`，未传路径目前只在测试中覆盖。

6. **（补记）客户端 `completedPaneIds` 跨代边缘碰撞**
   - 位置：`packages/herdsman-pi/src/wake.ts:72-80`（收集）与 `:90-92`（压制）。
   - 现状：`completedPaneIds` 按单次投影批次构建。同批次内若同时存在某 pane 的旧代成功事件与紧随其后的新任务 `fallbackOutcome: true` 失败事件，后者会被一并压制（该 pane 的 fallback 失败不再唤醒编排者）。概率极低（需同一 paneId 跨代复用 + 完成态与 fallback 落在同一批次），符合合同 §3.4 的取舍；后续可加入 `paneGeneration` 维度辅助去重。
   - 本项与第一轮「建议 6」为同一风险，本轮补记入档。
   - **已裁定（2026-10-16）**：跨批次压制采纳新增 `paneGeneration` 维度去重，但前置条件是先补齐保留行 `reserve→delivered→acked` 主链路与 reconciler 保留分支的回归测试，排入下一批次，本轮不动 `packages/herdsman-pi/src/wake.ts` 的唤醒语义（见文末「待办（下一轮）」）。

## 本轮（第三轮）新增非阻塞观察项

a. **`pane_generation` 词法比较**
   - 影响：`#invalidatePaneCore` 步骤 1 的代际上界用 `pane_generation < ?`（`src/db/agent-events.ts:262-264`），`pane_generation` 是 TEXT，实为词法比较；上游若改用不等宽命名（如 `gen-10` 与 `gen-2`），`'gen-10' < 'gen-2'` 成立 → 关 `gen-2` 时会误杀 `gen-10` 代的更新行。
   - 位置：`src/db/agent-events.ts:262-271`。
   - 现状：现场库全部 `pane_generation` 为 NULL，判定不触发，属休眠缺陷；若上游开始上报代际需先收紧（可去掉 `or pane_generation < ?`，只留 `= ? or is null`）。

b. **legacy 无代际关页分支步骤 1 仍为 `1 = 1`**
   - 影响：迟到的、无代际的关页事件（`paneGeneration == null`）会作废同名 `paneId` 下所有未终态行，包括新代事件；与（a）方向的现代分支不对称。
   - 位置：`src/db/agent-events.ts:262-266`（`legacy ? "1 = 1" : ...`）。
   - 现状：现场全部无代际，此分支是当前唯一有效路径；保留行由步骤 2 在同一事务内恢复，实际损害限于非完成类行。

c. **保留行在 7 天内可任意取件**
   - 影响：保留成立后可被 `listAfter`/`reservePending`/`nextDeliverableAfter` 在 7 天硬上限内随时取件（不含 300s 新鲜度限制），扩展重启后 `presentedEventIds` 清空 → 可能投递最长约 6 天前的陈旧完成态。
   - 位置：`src/db/agent-events.ts:353-362`（`deleteSettledOlderThan` 7 天回收）、`packages/herdsman-pi/src/index.ts:721,751,1109`（失效面重置 `presentedEventIds`）。
   - 现状：合同 §2 TTL 表已背书（300s 只用于关页/快照缺失判定时刻，保留后取件不受限），属明示取舍。

d. **保留分支未排除交互式 pi agent 的 `agent.idle`**
   - 影响：`#invalidatePaneCore` 步骤 2 只按 `type='agent.idle' and payload.from='working'` 保留（`src/db/agent-events.ts:276-296`），不区分交互式 pi agent；关页反而把用户自己的 Pi 面板 idle 提升为可投递，可能被编排者当作子代理完成唤醒。
   - 位置：`src/db/agent-events.ts:276-296`；对照 `isDeliverableAgentEvent` 的存活分支（`src/db/agent-events.ts:100`）里的 `isInteractivePiAgent` 排除。
   - 现状：未处理；只在关页窗口出现，合同未要求。


g. **保留行 `reserve→delivered→acked` 链路与 reconciler 保留分支缺测试**
   - 影响：`reservePending`（`src/db/agent-events.ts:554-592`）、`markAcked`（`src/db/agent-events.ts:657-672`）、`markRetainedInvalidated`（`src/db/agent-events.ts:307-319`）与 reconciler 的保留分支（`src/daemon/agent-event-reconciler.ts:143-158`）无对应用例；合同「补齐断言 1」只覆盖 drain 成功分支。
   - 现状：R4 当轮未补（当轮只授权补合同 §5 断言 2 与 `nextDeliverableAfter` 回归用例）；**2026-10-16 已裁定**：作为 `wake.ts` 跨批次压制新增 `paneGeneration` 维度去重的前置条件，排入下一批次先行补齐（见文末「待办（下一轮）」）。

## 第四轮（R4）补记

7. **`#runPlanRow` 重试预刷新未保留 degraded 的静默窗口（本轮已修）**
   - 位置：`src/observability/agent-index-service.ts:770-781`（`row.attempts > 0` 的预刷新 catch，原为无条件 `store.markRetry(row.id, new PlanWaitingHistoryError())`）。
   - 缺陷：`StatusEventPlanStore.markRetry`（`src/db/status-event-plans.ts:331-357`，判定在 `:345-347`）按 `errorMessage === "PLAN_WAITING_HISTORY"` 决定耗尽后走 `discarded`（客户端 `packages/herdsman-pi/src/wake.ts:50-53` 对 `agent.discarded` 恒静默）还是 `failed`（生成带 `fallbackOutcome` 的可唤醒事件）。因此“降级计划在一次重试回合的刷新失败”这一交错下耗尽会静默丢弃：既不发唤醒，又把 `last_error` 从 `degraded` 覆盖为 `PLAN_WAITING_HISTORY`，连带使 `#backfillFailedPlanEvents`（`:484-485`）跳过该行，重启补录也不再唤醒。
   - 修法：该 catch 改为 `row.lastError === "degraded" ? new Error("degraded") : new PlanWaitingHistoryError()`，与其余 5 处口径一致。
   - **耗尽站点扫查清单**（全仓 `markRetry` 调用点共 6 处；决定 discarded vs failed 的判定点只有 `markRetry` 一处）：

     | # | 站点（文件:行） | 传入错误 | degraded 是否保留 |
     |---|---|---|---|
     | 1 | `agent-index-service.ts:349-354`（`#retryWaitingPlanRow` 预刷新 catch） | `currentPlan.lastError === "degraded" ? new Error("degraded") : new PlanWaitingHistoryError()` | 是（第二轮已修） |
     | 2 | `agent-index-service.ts:639-642`（`#drainPlanRow` 预刷新 catch） | `current.lastError === "degraded" ? new Error("degraded") : new PlanWaitingHistoryError()` | 是（第二轮已修） |
     | 3 | `agent-index-service.ts:775-781`（`#runPlanRow` 重试预刷新 catch） | `row.lastError === "degraded" ? new Error("degraded") : new PlanWaitingHistoryError()` | **本轮修复** |
     | 4 | `agent-index-service.ts:858-864`（`#runPlanRow` isDegraded 分支） | `new Error("degraded")`（无条件） | 是（天然） |
     | 5 | `agent-index-service.ts:896-903`（`#runPlanRow` `PlanWaitingHistoryError` catch） | `row.lastError === "degraded" ? new Error("degraded") : error` | 是 |
     | 6 | `agent-index-service.ts:962-969`（`#runPlanRow` 通用 catch） | `row.lastError === "degraded" ? new Error("degraded") : error` | 是 |

   - 相关联的非调用点（已核对，无需改）：`StatusEventPlanStore.markDiscarded`（`:320-329`）**无生产调用方**（仅 store 单测）；`#backfillFailedPlanEvents`（`:484-485`）与 `#backfillDiscardedPlanEvents`（`:516-519`）只是按已定状态补录事件，不重新判定 discarded/failed；`markCancelled`（`status-event-plans.ts:311-318`）用于 agent 消失 / pane 关闭 / `from === to`，非耗尽站点。
   - 回归测试：`test/integration/agent-index-service.test.ts` 用例 `degraded retry row failing its retry-round refresh exhausts as wakeable failed`（修复前红：`expected 'discarded' to be 'failed'`；修复后绿，且批量断言 `agent.failed` 推流 + `fallbackOutcome: true` + 无 `agent.discarded`）。

8. **两轮独立意见点出但尚未入档的事实**
   - ① 合同 §3.4 的“跨批次由 `presentedEventIds` 保障压制”与实现不符：`presentedEventIds` 是 **event id 键**的集合（`packages/herdsman-pi/src/index.ts:508,672-674`），只能阻止**同一个 id** 二次注入，无法压制不同 id 的事件。`completedPaneIds`（`wake.ts:72-92`）只作用于单次投影批次，因此同一 pane 上“真实完成事件”与“降级 fallback 失败事件”分属两批时会出现双唤醒（完成收到一次、失败再收一次）。噪音级、不会重复注入同一个 id，也不会丢事件。
   - ④ 既有“deliverable 与 status 恒等”不变量测试（`test/integration/agent-event-delivery-lifecycle.test.ts:464-483`）已成**同义反复**：它断言 `row.deliverable === (["pending","delivered"].includes(row.status) ? 1 : 0)`，而 `mapAgentEvent` 正是按该规则派生的旧语义的镜像；新设计允许 `invalidated + deliverable=1`，该用例既覆盖不到保留行，也不再提供独立证据。建议改名（如“非保留行的 deliverable 镜像”）或改为直接断言各状态组合的期望表。

## 已决（owner 接受）

以下条目已由 owner 明确接受，不再作为待修项：

1. **保留行 7 天内可投递 / 扩展重启后可能重放陈旧完成态**：按合同 §2 TTL 表**接受**（300s 只约束关页/快照缺失判定时刻，保留成立后 7 天内可取件，ack 当场收敛；扩展重启清空 `presentedEventIds` 后可能重放最长约 6 天前的完成态）。见上文第 6 项 c。
2. **`pane_generation < ?` 词法比较**（上文 a）：维持“休眠、先不动”——现场全 NULL，判定不触发。
3. **legacy `1 = 1` 非对称作废**（上文 b）：维持“休眠、先不动”——现场关页全部无代际，该分支是唯一有效路径，保留行由步骤 2 在同一事务内恢复。
4. **交互式 pi `agent.idle` 保留 bypass**（上文 d）：维持“休眠、先不动”——非合同要求项，仅在关页窗口可能多一次可投递事件。
5. **§5 验收标准口径**：采用“**用例名存在且通过 + 缺口入档**”，不要求把 §5 每条“补齐断言”逐字全覆盖（例如“补齐断言 1”只覆盖 drain 成功分支，其余缺口已入档为上文 g / 待办）。

## 待办（下一轮）

1. **保留行 `reserve→delivered→acked` 主链路与 reconciler 保留分支回归测试（上文 g，已裁定排期，2026-10-16）**：保留行 `reserve→delivered→acked` 主链路（`src/db/agent-events.ts:554-583,657-672`）与 reconciler 保留分支（`src/daemon/agent-event-reconciler.ts:143-158`）缺回归测试；已裁定作为第 2 项的前置条件，排入下一批次先行补齐。
2. **跨批次压制（上文第 6 项、第 8 项 ①，已裁定排期，2026-10-16）**：跨批次压制采纳新增 `paneGeneration` 维度去重，但前置条件是先补齐保留行 `reserve→delivered→acked` 主链路与 reconciler 保留分支的回归测试，排入下一批次，本轮不动 `packages/herdsman-pi/src/wake.ts` 的唤醒语义。

## 建议后续动作

> 注（2026-10-17）：非阻塞观察项 1~4、e、f 及事实条目 ②、③ 经复核判定不做，已清理，原文见 git 历史/备份。

- 监控 `paneGeneration` 字段的上游调用分布与命名宽度；若开始上报代际，先收紧（a）的词法上界与（b）的 legacy 全量作废。
- 若新增降级错误码，同步更新 `#retryWaitingPlanRow` 的判断条件（第 2 项）。
- 保留行链路的测试缺口（g）与不变量缺口（f）在下一轮补回归时一并处理。

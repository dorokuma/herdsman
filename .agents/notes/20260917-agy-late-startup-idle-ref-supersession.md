---
status: active
superseded_by: ""
supersedes: ""
模块: db | observability | herdsman-pi
---

# agy 迟到 startup-idle 计划不再吞掉完成轮次的 ref，并被更新计划作废

## 一句话结论

agy 的 `unknown -> idle` 迟到计划不再把最终 assistant ref 登记成“已完成轮次”，且同一 agent 出现更新状态计划时会作废这类既不能唤醒、也已被取代的 idle 计划；`working -> done` 因此仍能发出 `agent.done` 唤醒主代理。

## 背景

实锤现场（pane `w9:p76`，agy scout 任务）：子代理会话正文已完整落库，但主代理一直没被唤醒。

时序：

1. pane 尚未入索引时先报 `idle` → 生成 `unknown -> idle` 计划，历史暂空，进入 `PLAN_WAITING_HISTORY` 重试。
2. agent 真正开始 working → `idle -> working`。
3. 历史出现最终正文 `lastAssistantMessage.ref = #entry=23`。
4. 迟到的 `unknown -> idle` 计划此时才就绪，把 `#entry=23` 登记进 `agent.idle`（payload `from=unknown`）。
5. `working -> done` 计划随后用同一个 ref 走 `isReadyNonPi`，旧实现要求 `currentRef !== prevRef`，于是判定“未就绪”，8 次重试后 `PLAN_WAITING_HISTORY -> discarded`；`wake.ts` 对 `agent.discarded` 不唤醒 → 漏唤醒。同机 p75/p77 成功只是因为它们的 `unknown -> idle` 在结束前就被 discarded，没抢走 ref。

## 决策

1. **ref 对比只认“已完成轮次”的终态**（`src/db/agent-events.ts::latestCompletedTurnEvent`）：`agent.done` 一律计入；`agent.idle` 仅当 payload `from === "working"` 时计入。`agent.status.changed`、`from !== working` 的 idle（startup/unknown 或 done/blocked→idle）、`agent.discarded`、空 assistant 行一律不算，不得消费某轮次的 ref。SQL 侧用 `json_extract` 过滤 `from` 与非空 assistant 文本。
2. **已经投递过就不是“未就绪”**（`src/observability/agent-index-service.ts`）：agy/antigravity 的 `idle`/`done` 计划在 ready 重试后若发现该 ref 已由“已完成轮次”投递，则 `return undefined` 跳过（计划 completed），绝不进入 `PLAN_WAITING_HISTORY`。历史真正一直为空时才允许重试耗尽 → discarded。
3. **新增计划作废被取代的、无法唤醒的 idle 计划**（`src/db/status-event-plans.ts::cancelSupersededPendingPlans`，由 `insertPending` 在同一事务内调用）：同一 `agent_id + herdr_session_name` 上出现目标不同的新计划时，把 `to_status='idle' 且 from_status<>'working'` 的 pending/running 行标记为 `cancelled` + `last_error='PLAN_SUPERSEDED'`。这类计划投递出去是 payload `from != working` 的 `agent.idle`，投递谓词与 wake 策略都会丢弃，作废它不会丢唤醒；取消态沿用既有 settled 清理路径，不新增 schema，也不产生 `agent.discarded`。
4. **legacy（无 herdrEventKey）重试同理**：agy 计划在 top dedup 分支改用 `latestCompletedTurnEvent` 作基线，且已投递时跳过而非 `throw PlanWaitingHistoryError`（非 agy 行为不变，仍是 waiting）。
5. **wake.ts 的 `agent.discarded` 不唤醒语义保留**：本次是修根因（不该产生 discarded、应产生 `agent.done`），不是靠放宽 wake 兜底。

## 被放弃的方案（必填）

- **让 wake 对 `agent.discarded` 唤醒作为主修复**：放弃。discarded 语义是“观察者放弃等待”，拿它唤醒会让放弃与真实故障/正常结束混同（见 20260917-status-event-plan-discarded-lifecycle），且掩盖真正的 ref 判定缺陷。
- **广口径作废：任何 Pending 计划只要 `P.to === N.from` 就取消**：放弃。`working -> done` 计划为等历史而 pending 时，pane 常会 done→idle / done→working 抖动；广口径会取消掉本该送达的完成唤醒，等于用新漏洞换旧漏洞。只取消“即使投递也无法唤醒”的 `from != working` 的 idle 计划。
- **把 `agent.blocked` 也当作已完成轮次**：放弃。blocked 是“请求输入”的交互态，与 done 语义不同；且 blocked→done 同 ref 时仍应唤醒一次完成结果。
- **给 status plan 新增 `superseded` 终态**：放弃。需改 schema/迁移与清理/回填分支，收益仅为区分“取代”与普通 cancelled；改用 `cancelled + last_error='PLAN_SUPERSEDED'` 即可观测且零迁移。
- **只改 isReadyNonPi、不做计划作废**：放弃。迟到的 startup-idle 仍会投递 payload `from=unknown` 的无用 idle 行，无法满足“作废过时计划、避免迟到 startup-idle 完成”的要求。

## 来源

p76（`w9:p76`，agy scout-omarchy-ssh-retry）根因分析 + status_event_plans 10407/10410 记录；本仓改动：`src/db/agent-events.ts`、`src/db/status-event-plans.ts`、`src/observability/agent-index-service.ts` 及对应回归测试；`pnpm check` 651 测试通过（新增 5 个用例：2 个 store 单测 + 3 个 p76 回归集成测试）。回归测试在还原修复前的源码时确实失败（迟到计划未被作废、`working -> done` 零事件），修复后通过。

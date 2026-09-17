---
status: active
superseded_by: ""
supersedes: ""
模块: db | observability | herdsman-pi
---

# Status Plan 等待超时拆分 discarded 终态与唤醒语义解耦

## 一句话结论

status plan 等待超时从 failed 拆出独立 discarded 终态，解耦观察者放弃与 agent 崩溃。

## 背景

`PLAN_WAITING_HISTORY` 耗尽重试次数后此前被标记为 `failed` 并落库 `agent.failed` 事件，导致下游（如 herdsman-pi 等观察者）误报 agent 崩溃而触发假警报。同时存在终态复活（已完成/放弃的 plan 被延迟定时器写回重试态）与重试定时器泄漏问题。

## 决策

1. **状态与事件契约新增 `discarded` / `agent.discarded`**：`status_event_plans.status` 与 `agent_events.type` 增加 `discarded` 和 `agent.discarded`，超时放弃标记为 discarded 而非 failed。
2. **CAS 条件更新防终态复活并清定时器**：`markRetry` 更新时增加 `WHERE status IN ('pending', 'running')` 条件保护，防止终态被异步回调复活，并在状态收敛时清理残留重试定时器。
3. **wake 对 agent.discarded 不触发唤醒（观察者放弃由 agent 自身生命周期事件保底），legacy PLAN_WAITING_HISTORY 的 agent.failed 同样过滤**。
4. **回填跳过历史等待行**：历史等待重试计划在回填处理中显式跳过，避免重启时重复触发失败事件。
5. **清理策略对齐**：`deleteSettledOlderThan` 结算清理范围纳入 `discarded` 终态。

## 被放弃的方案（必填）

- **沿用 failed 并在下游打补丁过滤**：语义混同继续污染事件流，且下游组件必须感知内部重试细节，职责边界不清，放弃。
- **拆成多 commit**：本批改动在 DB 契约、观测服务、下游 wake 处理及测试间强耦合，拆分会导致中间状态破坏 git bisect 可验证性与原子性，放弃。
- **直接删掉等待超时重试**：慢环境或高负载下 WAL 写入存在延迟，直接移除重试会丢失慢环境下仍有价值的重试能力，放弃。
- **wake 穿透映射 failed——假警报未消除，放弃**。

## 来源

本批工作区 diff 与 pnpm check 639 测试通过记录。
终审 oracle 发现白名单遗漏与穿透语义矛盾后修正。

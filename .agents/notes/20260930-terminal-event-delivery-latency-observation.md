---
status: active
superseded_by: ""
supersedes: ""
# 模块可选值: observability, daemon, cli, config, db, herdr, shared, herdsman-pi, herdsman-herdr-plugin, release
模块: observability | herdsman-pi
---

# 终态事件投递延迟观察：检测段 ≈37s + 投递段 ≈72s（wW:p6 / role-worker-6f8e9f0c）

## 一句话结论

2026-09-30 派发到 `wW:p6` 的 worker（`role-worker-6f8e9f0c`）已正常 `stop` 结束，但它的终态事件在 herdsman 账本里
从「报告最后一轮 assistant 完成」到「投递成功」共约 **109.220s**：其中账本侧事件落库（检测段）占 **36.919s**，
事件落库到被取件投递（投递段）再占 **72.301s**。这两段都不是 agent 自身的执行时间。

## 背景

- 本笔记只记录一次**已定性**的延迟现象与取证数据，不包含实现方案或改造步骤（那属于 plan 产出）。
- 观察对象：worker 派发 → 终态事件落库 → 投递到编排者（main proxy pane `wW:p1`）的整条链路。
- 相关前序记录：[`20260930-terminal-event-delivery-h1.md`](20260930-terminal-event-delivery-h1.md)（同批终态投递修复 H1）、
  [`wake-delivery-r4f1-remaining-risks.md`](wake-delivery-r4f1-remaining-risks.md)。

## 完整时间线（UTC，日期 2026-09-30；「相对」以 T0 = 02:41:32.767Z 为基准）

| 时间 | 相对 T0 | 事实 |
| --- | --- | --- |
| 02:37:17.007Z | −255.760s | `agent_orchestrator_scopes` 行最后一次更新：`workspace_id=wW`、`owner_pane_id=wW:p1`、`acked_event_id=46507` |
| 02:41:32.767Z | 0（T0） | worker 报告最后一轮 assistant 消息时间戳，`stopReason=stop`（非降级结束） |
| 02:42:04.637Z | +31.870s | `agent_events` id=46529 `agent.status.changed` 落库（比 46530 早 1ms） |
| 02:42:04.638Z | +31.871s | id=46530 `agent.done` 落库：`status=invalidated`、`deliverable=0`、`invalidated_reason=degraded_retry` |
| 02:42:09.686Z | +36.919s | id=46531 `agent.done` 落库（**此次唯一可投递的终态行**），比 46530 晚 5.048s |
| 02:42:14.440Z | +41.673s | id=46532 `agent.status.changed` 落库 |
| 02:42:14.442Z | +41.675s | id=46533 `agent.idle` 落库（比 46532 晚 2ms） |
| 02:43:21.987Z | +109.220s | id=46531 被取件投递：`status` `pending→delivered`、`delivery_attempts` `0→1`、`last_attempt_at=02:43:21.987Z` |

## 取证方法与原始值

**方法**：对生产库 `/root/.herdsman/state.db` 做**只读**查询，对象是 `agent_events` 与 `agent_orchestrator_scopes` 两张表。
本次落盘未重新查询生产库，也未对 `/root/.herdsman` 下任何文件做写操作；下表原始值来自派发件中记录的该次只读取证结果。

**表名 / 列名对照**（仓内 Drizzle schema）：

- `agent_events`：`src/db/schema.ts:99`（`agentEvents`）；本笔记涉及的列 `id`、`type`、`status`、`deliverable`、
  `delivery_attempts`、`last_attempt_at`、`invalidated_reason`、`created_at`。派发件中的简写
  `reason` / `created` / `attempts` / `last_attempt` 分别对应 `invalidated_reason` / `created_at` /
  `delivery_attempts` / `last_attempt_at`。
- `agent_orchestrator_scopes`：`src/db/schema.ts:172`（`agentOrchestratorScopes`）；涉及的列 `workspace_id`、
  `owner_pane_id`、`acked_event_id`、`updated_at`。

**`agent_events` 原始行**：

| id | type | status | deliverable | delivery_attempts | created_at | last_attempt_at | invalidated_reason |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 46529 | agent.status.changed | pending | 1 | 0 | 02:42:04.637Z | — | — |
| 46530 | agent.done | invalidated | 0 | 0 | 02:42:04.638Z | — | degraded_retry |
| 46531 | agent.done | pending → delivered | 1 | 0 → 1 | 02:42:09.686Z | 02:43:21.987Z | — |
| 46532 | agent.status.changed | pending | 1 | 0 | 02:42:14.440Z | — | — |
| 46533 | agent.idle | pending | 1 | 0 | 02:42:14.442Z | — | — |

**`agent_orchestrator_scopes` 原始行**：`workspace_id=wW`、`owner_pane_id=wW:p1`、`acked_event_id=46507`、
`updated_at=02:37:17.007Z`（未随本次投递推进）。

## 两段延迟的分解

- **检测段 ≈36.919s**（02:41:32.767Z → 02:42:09.686Z）：从 worker 报告最后一轮 assistant 完成（`stopReason=stop`），
  到可投递终态行 46531 落库。这一段内先落了两行：46529（+31.870s）与 46530（+31.871s，`degraded_retry`
  作废的 `agent.done`，`deliverable=0`），46531 比 46530 晚 5.048s。也就是说终态事件并不是在报告完成的那一刻写入的，
  而是由随后的状态/终态检测流程产生，落库时刻比 agent 实际结束晚约 37s。
- **投递段 ≈72.301s**（02:42:09.686Z → 02:43:21.987Z）：从 46531 落库到该行被取件投递（`delivery_attempts` 0→1）。
  派发背景给出的对应关系是「Pi 扩展的**唤醒门只在主代理 pane 空闲时注入**事件」，因此这段等待与编排者 pane
  （`owner_pane_id=wW:p1`）当时的忙碌时长同阶。本笔记只登记该对应关系，未对其代码路径做独立复核。
- **用户可见总延迟 ≈109.220s**（T0 → 投递成功）；若从 scope 行上一次更新（02:37:17.007Z）起算则约 364.980s。

## 影响面

- **编排者可见性滞后**：编排者只有在事件真正投递后才看到子 agent 的终态。本例中「worker 已 stop」与
  「编排者看到终态」相差约 1 分 49 秒，该窗口内编排者对该 worker 的完成/失败不可见。
- **账本时间戳不能当作 agent 结束时刻**：`agent_events.created_at` 只标记投递链路起点，本例 46531 的
  `created_at` 比真实报告时刻晚 36.919s；用 `created_at` 反推 agent 完成时间会系统性低估延迟。
- **pending 堆积的外观**：同批的 46529/46532/46533 在观察时点仍是 `status=pending`、`delivery_attempts=0`，
  单看「pending 行数」会把这种正常排队误读成投递故障。
- **无丢失迹象**：终态最终被投递（46531 `deliverable=1`、`delivery_attempts=1`、`status=delivered`）；
  46530 的 `degraded_retry` 作废行保持 `deliverable=0`，属被替换行而非丢失。

## 残留观察项

1. **46529 / 46532 / 46533 仍为 pending**：观察时点 `status=pending`、`deliverable=1`、`delivery_attempts=0`；
   它们与 46531 同批落库，是否在之后被投递或 ack 未在本次观察窗口内确认。
2. **scope 游标停在 46507**：`acked_event_id=46507`、`updated_at=02:37:17.007Z`，46531 投递后未见推进。
   游标推进是否要求事件 id 区间连续（46508..46530 未被 ack，含上面三条 pending 行）未复核。
3. **「先作废再落可投递行」的过程未复核**：46530（`agent.done` + `invalidated` + `degraded_retry`）与 46531 相隔
   5.048s，这段降级重试与 36.919s 检测段之间的因果关系尚未查证。
4. **投递段成因未做代码级复核**：本次仅记录「唤醒门只在主代理 pane 空闲时注入」这一对应关系；
   另外注意 `packages/herdsman-pi/src/wake.ts` 在本仓工作区处于未提交修改状态，将来复核需以当时的真实源码为基线。
5. **样本量为 1**：以上是单次派发（wW:p6 / role-worker-6f8e9f0c）的观察结果，尚未确认是否可复现。

## 复验判据

主任务改造完成后，按下列判据复测（本次数据仅作对照基线）：

- **检测段**：同一场景下可投递终态行的 `created_at` 与「报告最后一轮 assistant 时间戳」之差应**从分钟级降到秒级**
  （当前基线 36.919s）。
- **投递段**：`created_at` → `last_attempt_at` 是否缩短**取决于唤醒门策略**，需单独复测；当前 72.301s
  **不能**当作「改后必降」的判据（若唤醒门策略不变，这一段可能原地不动）。
- **复测记录要素**：派发 id 与 role、pane id、报告时间戳与 `stopReason`、终态行 id 与 `created_at`/`last_attempt_at`、
  scope 行的 `acked_event_id`/`updated_at`，以便与上面两段分别对照。

## 来源

- 派发件背景事实（2026-09-30，worker → `wW:p6`，`role-worker-6f8e9f0c`）及其记录的只读查询结果；本笔记落盘者未重新查询生产库。
- 表名/列名对照：`src/db/schema.ts:99`（`agent_events`）、`src/db/schema.ts:172`（`agent_orchestrator_scopes`）。
- 相关笔记：[`20260930-terminal-event-delivery-h1.md`](20260930-terminal-event-delivery-h1.md)、
  [`wake-delivery-r4f1-remaining-risks.md`](wake-delivery-r4f1-remaining-risks.md)。
- 说明：本笔记是**现象记录（观察）**，没有决策项，故省略 [_template.md](_template.md) 的「决策」「被放弃的方案」两段。
- 索引：未运行 `scripts/notes-index.sh`（本轮任务约束禁止改动仓内既有文件；`INDEX.md` 本身在 `.gitignore` 内，见 `.gitignore:6`）。

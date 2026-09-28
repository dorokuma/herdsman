# 2026-07-23 empty-wake-events 修复记录

## 触发条件
六触发条件：修复根因明确且涉及 observability/persistence 两模块契约变更（degraded 事件从可投递变为不可投递）。

## 现场事件号
- 空回传主链：43927（non_terminal_assistant  degraded）、43939（expected_text_mismatch degraded）、43950（retry 后 complete）
- 空文本守卫：43892（no_advance_from_input）、43894（同场景二次确认）
- 双事件复现：44092（legacy degraded 行被 listAfter 投递）、44120（latestTerminalEvent 返回无效基线）

## 修复

### 1. degraded 事件不可达化
- **位置**：`src/observability/agent-index-service.ts:819`（`#runPlanRow` 内 `isDegraded` 分支）
- **做法**：检测到 `payload.degraded === true` 时，调用 `this.#stores.agentEvents.invalidateById(event.id, "degraded_retry")` 并将 event 的 status/deliverable 同步置为 invalidated/0，随后 `store.markRetry(row.id, new Error("degraded"))` 将 plan 置为 pending 并调度重试。
- **效果**：degraded 事件写入后立即失效，`listAfter`（`pending`/`delivered` 之外状态不命中）、`nextDeliverableAfter`（同）、`latestTerminalEvent`（`status != 'invalidated'` 过滤，见 agent-events.ts:308）均不返回，消除双事件。

### 2. 空文本守卫扩展
- **位置**：`src/observability/agent-index-service.ts:1681`（turn signal 分支）与 `:1726`（无 signal 分支）
- **做法**：terminal 判断从 `!isTerminalAssistant(advanced)` 扩展为 `!isTerminalAssistant(advanced) || !hasNonEmptyAssistantMessage(advanced)`。
- **效果**：stopReason=stop/length 但 assistant 文本为空时，不再 emit 空 `agent.done`，而是降级为 `non_terminal_assistant` degraded 并进入 retry 路径。

### 3. #drainPlanRow 并发安全与幂等
- **位置**：`src/observability/agent-index-service.ts:566-575`
- **做法**：将两次 `store.get(row.id)` 收敛为一次（在 `runSqliteTransaction` 内读取 candidate 后直接返回，避免第二次 get 读到中间状态）；`:716` 处消费 `store.markRunning(row.id)` 的布尔返回值，若更新行数为 0（已被其它 drain 领走）则清空 timer 并返回。
- **效果**：同一 plan 不会被并发 drain 重复执行。

### 4. status-event-plans.ts 变更
- **位置**：`src/db/status-event-plans.ts:331-358`（`markRetry` 方法）
- **做法**：返回类型从 `void` 改为 `StatusEventPlanRecord | null`，SQL 追加 `where id = ? and status in ('pending', 'running')` 使状态转换具备幂等性；对 `PLAN_WAITING_HISTORY` 错误保持 `discarded` 语义，对 `degraded` 错误统一按 `pending` + `lastError="degraded"` 处理，attempts 递增后由 drain 侧 `#scheduleWaitingHistoryRetry` 重新入队。

### 5. agent-events.ts 查询侧过滤
- **位置**：`src/db/agent-events.ts:295-318`
- **做法**：`hasTerminalEventAfter` 与 `latestTerminalEvent` 在 `type in ('agent.idle','agent.done','agent.blocked')` 基础上追加 `and status != 'invalidated'`，防止 invalidated 的 degraded 行被当作有效基线。

## 第三轮返工

### 6. 存量库 legacy degraded 行一次性清理
- **约定**：drizzle migration（`drizzle/0010_legacy_degraded_cleanup.sql` + `drizzle/meta/0010_snapshot.json` + `drizzle/meta/_journal.json` idx=10）
- **SQL**：`UPDATE agent_events SET status='invalidated', deliverable=0, invalidated_reason='legacy_degraded' WHERE json_extract(payload_json,'$.degraded')=1 AND status != 'invalidated'`
- **幂等**：`status != 'invalidated'` 条件保证重复执行无副作用。
- **覆盖情形**：
  - 全新库：迁移在首次启动时执行，无 legacy 行，0 行受影响。
  - 存量已部署库升级：迁移将修复前写入的 degraded 行置为 invalidated。
  - 重复执行：第二次起 `status='invalidated'` 的行被条件过滤，跳过。
- **接线验证**：`readMigrationFiles({ migrationsFolder: 'drizzle' })` 读出 11 个文件；`pnpm db:check`（drizzle-kit check）通过；`pnpm db:migrate`（`applyMigrations`）会执行。

### 7. 对外不可达负向断言补齐
- **文件**：`test/integration/turn-completion-signal.test.ts`
- **改动**：对 `retries eight times after a received turn signal when history is still empty`、`generates agent.done as-is with a warning when no turn signal arrives`、`emits terminal event when status flips during wait`、`pi timeout with terminal but empty assistant emits degraded non_terminal_assistant`、`degraded empty-assistant event is invalidated and retry delivers only the complete event` 补断言：
  - `_result.events` 不含 `agent.done`
  - `listAfter` 返回 0 条 `agent.done`

### 8. S8 完整生命周期断言恢复
- **文件**：`test/integration/agent-index-service.test.ts`
- **改动**：恢复第二轮 drain 验证。首轮 drain 后 degraded 事件被 invalidated、plan 置为 pending；第二轮 `drainPendingPlans()` 前 re-record turn signal，断言 plan 变为 `completed`、`listAfter` 返回含 `"final answer"` 的完整 `agent.done` 事件。修正原注释「tested by S6」失实问题。

### 9. 测试调试输出清理
- **文件**：`test/integration/agent-index-service.test.ts`
- **改动**：删除 S5 中两处 `console.log`（`dbEvents count`、`degradedInDb count`），删除 S7 中 `console.log("S7 mock callCount" ...)`。

### 10. 决策记录对齐
- **事件号**：43927/43939/43950、43892/43894、44092、44120
- **行号修正**：空文本守卫实际位于 `:1681` 与 `:1726`，非 `:1704`；`markRetry` 位于 `src/db/status-event-plans.ts:331-358`，非 `:366-370`
- **变更说明**：补充 agent-events.ts `latestTerminalEvent`/`hasTerminalEventAfter` 的 `status != 'invalidated'` 过滤，status-event-plans.ts `markRunning`/`markCompleted`/`markCancelled` 的幂等更新条件
- **可观测性取舍**：空事件降级后，运维无法再通过 DB 直接看到「曾空转/超时」的 degraded 事件（已 invalidated 且短期清理）。这是有意代价：degraded 事件本就不应被消费，保留可观测性会重新暴露空回传风险。如需追踪，应通过 plan 的 `lastError="degraded"` 与 attempts 计数在业务层观察。

## 清理路径核验（item 6）

### invalidated 行
- **清理入口**：`AgentEventReconciler.reconcile()` 调用 `this.#events.deleteInvalidatedOlderThan(INVALIDATED_GRACE_MS)`
- **INVALIDATED_GRACE_MS**：`60 * 60 * 1000`（1 小时）
- **与 acked/failed 口径对比**：
  - acked/failed 通过 `deleteSettledOlderThan(RECONCILE_SETTLED_TTL_MS)` 清理，TTL = 7 天
  - invalidated 独立 TTL = 1 小时，口径不一致但有意为之：invalidated 是 transient 状态，预期在 1 小时内被 owner ack 或自然过期；acked/failed 是 terminal 状态，需更长的审计窗口。
- **与投递 ack 窗口对比**：
  - `REDELIVERY_FRESHNESS_MS = 300_000`（5 分钟）
  - 1 小时 grace >> 5 分钟 redelivery window，owner 有充足时间 ack 后 invalidated 行才被清理。
- **结论**：当前口径合理，无需修正。已记录于本 notes。

## 第三轮 oracle 质疑处置

### 1. 迁移 0010 接线验证
- **质疑**：migration 文件虽创建，但未登记到 `drizzle/meta/_journal.json`，`readMigrationFiles` 读不到，守护进程升级路径与 `pnpm db:migrate` 不会执行。
- **处置**：
  - 新建 `drizzle/meta/0010_snapshot.json`（从 0009 复制，因无 schema 变更，更新 `id`/`prevId` 链）
  - `drizzle/meta/_journal.json` 追加 idx=10、tag=`0010_legacy_degraded_cleanup`、breakpoints=true 的条目
  - 验证：`readMigrationFiles({ migrationsFolder: 'drizzle' })` 返回 11 个文件；`pnpm db:check`（drizzle-kit check）通过；`pnpm db:migrate` 会执行 0010 SQL
- **反假覆盖证据（R4 版，已废弃）**：临时移除 journal 条目后测试 FAIL、恢复后 PASS。该反证仅能证明空库首次运行会记录迁移 hash，无法证明存量迁移链路正常生效。
- **反假覆盖证据（R5 修复版）**：测试改为真实存量升级路径——对已应用 0000-0010 的库，插入 legacy 行后删除 0010 的 `__drizzle_migrations` 记录，再调用 `applyMigrations` 重跑。断言：0010 的 `created_at` 等于其 `folderMillis`（1790610020312）、legacy 行被置 `invalidated`、`listAfter`/`nextDeliverableAfter`/`latestTerminalEvent` 均不可达。
- **when 高水位问题（R5 修复）**：R4 时 0010 的 `when` 为 `1759085000000`（2025-09-28），低于 0009 的 `1788403075026`（2026-09-03）。`applyMigrations` 的跳过条件 `lastMigration.created_at >= migration.folderMillis` 对存量库恒成立，0010 被永久跳过。R5 已修复为当前毫秒时间戳 `1790610020312`（严格大于 0009）。
- **防复发**：`test/integration/sqlite-migrations.test.ts` 新增 `when` 单调递增断言，手写 journal 时若时间戳逆序立即 FAIL。

### 2. SQL 条件语义
- **质疑**：为何 `status != 'invalidated'` 统一清理所有非 invalidated 状态的 degraded 行？是否应限制为 `pending`？
- **论证**：degraded 事件写入时通过 `invalidateById` 设为 `invalidated`，但修复前写入的 legacy 行可能处于 `pending` 或 `delivered` 状态（若已被投递）。`listAfter` 和 `nextDeliverableAfter` 的查询条件为 `(status = 'pending' ...) or (status = 'delivered' ...)`，两者都会被投递。因此必须覆盖 `pending` 和 `delivered` 两种状态。`json_extract(payload_json,'$.degraded')=1` 精准定位 degraded 行，不会误伤正常事件。
- **结论**：SQL 条件正确，无需修正。

### 3. json_extract 行为
- **质疑**：SQLite 的 `json_extract` 对缺失字段返回 `null`，`null=1` 为 false，不会误清理无 `degraded` 字段的正常行。
- **论证**：`json_extract(payload_json,'$.degraded')=1` 仅匹配 `degraded` 字段为数值 1 的行。JSON 中 `degraded: true` 反序列化后存储为 `true`，SQLite JSON1 扩展将其视为数值 1；`degraded: false` 视为 0。正常事件无 `degraded` 字段，`json_extract` 返回 `null`，`null=1` 为 false，安全。
- **结论**：SQL 条件安全。

### 4. 状态机并发
- **质疑**：`#drainPlanRow` 的 `runSqliteTransaction` 内两次 `store.get` 是否有竞态？
- **论证**：第三轮修复已将两次 `store.get(row.id)` 收敛为一次（读取 candidate 后直接返回）。`:716` 处消费 `store.markRunning(row.id)` 的布尔返回值，若返回 false（已被其它 drain 领走）则清 timer 并返回。`markRetry` 的 SQL 追加 `where status in ('pending', 'running')`，幂等。
- **证据**：S6 测试验证 attempts=1→2 的 retry 路径；S8 测试验证 degraded→retry→complete 完整生命周期；61 个 agent-index-service 集成测试全绿。
- **结论**：无回归， reviewer 已判通过。

### 5. 清理窗口口径
- **质疑**：invalidated 行 1h grace vs acked/failed 7d TTL，为何不一致？
- **论证**：
  - invalidated 是 transient 状态：事件被标记为 invalidated 后，owner terminal 可能已收到（需保留短暂时间以便 ack），或未收到（本就不应收到）
  - `REDELIVERY_FRESHNESS_MS = 300_000`（5 分钟）是 redelivery 窗口，1h grace >> 5min，owner 有充足时间 ack
  - acked/failed 是 terminal 状态，需要更长的审计窗口（7d）
- **结论**：口径差异有意为之，无需修正。

---

### R5 双审残余风险清单（oracle 第二意见，2026-09-28；均不阻断提交）

- 理论隐患：极端同毫秒——若某存量库 lastMigration.created_at 恰等于 0010 的 when（1790610020312），升级可能被跳过；概率极低，备查。
- 理论隐患：并发启动时多个进程同时执行 applyMigrations（未加锁）可能重复执行或跳过；SQLite 迁移无跨进程事务保护（既有架构问题，非本次引入）。
- 理论隐患：invalidated 行 1h TTL 与 acked/failed 7d TTL 的口径差异（既有设计，互见本笔记「第三轮 oracle 质疑处置」节）。
- 观察项：markRunning 返回值在并发 drain 时的消费路径（已按 CAS/幂等处理）。
- 观察项：degraded 事件在 DB 中仍保留 compact_history_json，但下游不投递（审计留存，按 TTL 清理）。
- 观察项：turn-completion-signal 的 expect(calls).toBe(3) 为 R4 审查建议的恢复项；R5 reviewer 已确认语义稳定（20 次运行无抖动）。
- 收尾验证：oracle 建议的「提交前验证存量升级 + 并发 drain」由本轮收尾执行（pnpm test 全量 + db:migrate 首跑/重跑幂等）。

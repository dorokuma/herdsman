---
status: active # active | superseded
superseded_by: ""
supersedes: ""
# 模块可选值: observability, daemon, cli, config, db, herdr, shared, herdsman-pi, herdsman-herdr-plugin, release
模块: observability | herdsman-pi
---

# 终态事件投递延迟修复（Phase 1）：检测段秒级化、投递段双轨唤醒

## 一句话结论

Phase 1 把「检测段（终态事件落库）」里的指数退避与 degraded 误判拆掉、把「投递段（唤醒编排者）」的
无界等待换成有界自旋，并让新批**并入**（而不是替换）未确认批，使得：终态事件落库从分钟级降到秒级、
编排者忙碌时也不再永久挂起唤醒，且任何已投递未确认的事件都不会被丢。

## 背景

- 现象与取证基线：[`20260930-terminal-event-delivery-latency-observation.md`](20260930-terminal-event-delivery-latency-observation.md)
  （单次样本：检测段 ≈36.9s、投递段 ≈72.3s、用户可见总延迟 ≈109.2s）。
- 同批的前置修复：[`20260930-terminal-event-delivery-h1.md`](20260930-terminal-event-delivery-h1.md)（H1：孤儿终态行、
  `agent.failed` 保留语义、唤醒口径）。**H1 与 Phase 1 是同一工作区上两批未提交改动**，提交顺序约束见下。
- 设计文档：设计 v3.1 §2.3（投递延迟）与 §5 Phase 1（本笔记即 Phase 1 的落地记录）。

## 决策

### Phase 1 六项（检测段 3 项 / 投递段 2 项 / 结算段 1 项）

1. **`#waitForHistoryAdvance` 去掉指数退避**（`src/observability/agent-index-service.ts`）：
   `baseDelay` 固定 200ms、`maxAttempts` 8 次、总预算 1500ms，末段按剩余预算 clamp；原来的
   `500ms → ×2 → 上限 16000ms / 总预算 30000ms` 会把第一轮可投递终态推到数十秒之外。
   判定语义不变：仍以「读不到新字节即视为该轮无新内容」为准。
2. **`turn.confirmed === true` 直通可投递终态**：客户端已确认终态 assistant 消息落盘，就不再走
   `degraded`（`no_advance_from_input` / `expected_text_mismatch` / `non_terminal_assistant`），
   也不再 `invalidateById(event.id, "degraded_retry")` 把已写入的内容作废。
3. **degraded 不得激活 10s 重试环**：新增显式不变式函数 `retryRingAuthorized(error)`（仅真正的
   `PlanWaitingHistoryError` 为真），四个重排点（`#retryWaitingPlanRow`、drain 的刷新异常、
   `#runPlanRow` 预刷新异常、`#runPlanRow` 的 degraded 分支）都改为「合成 `degraded` 只记原因、不挂环」；
   原来的通用失败路径没有挂环，本轮把最后一处「真抛 PlanWaitingHistoryError 但行已是 degraded」的后门
   也堵上（见下）。
4. **投递段有界化**（`packages/herdsman-pi/src/index.ts`）：删除「`ctx.isIdle?.() === false` → 置
   `wakeDeferredUntilSettled = true` 后直接 return」的无定时器挂起路径，改为
   `scheduleDeferredWake`：**100ms 有界自旋** + **5000ms 硬超时**（`WAKE_BUSY_SPIN_MS` /
   `WAKE_DEFERRED_TIMEOUT_MS`），超时后按当前状态强制裁决放行。
5. **空闲直投**（`packages/herdsman-pi/src/wake.ts`）：`WAKE_SETTLE_MS` 由 500 → **0**，
   判定空闲后经 0ms 定时器（微任务）直投，不再等 settle 窗口。
6. **双轨队列（Q1=A）**：编排者空闲 → `pi.sendMessage(..., { deliverAs: "followUp", triggerTurn: true })`
   0ms 直投；忙碌 → 不打断，改投 `triggerTurn: false` 的 follow-up，并经 `pi.on("context")` 以
   `customType: "herdsman-wake-queued"`（含 `details.eventIds`）把事件挂进当前可见上下文；
   队列与定时器一律有上界。

### 本轮追加的两处（用户拍板，承接 Phase 1 收尾）

7. **批次不丢：新批并入而非替换未确认批**。`state.unackedDelivered` 是「已交给 Pi 但未确认」的投递队列
   （id 为键 ⇒ 同 id 只存一次；消费侧一律按 id 升序）。强制放行只释放投递，**不再丢弃**未确认事件：
   放行时即使丢掉 `deliveredBatch` 记录（避免不再运行的唤醒轮长期占闸），事件仍留在队列里，并随下一次
   注入被并回批次，在随后的 settle 里被确认。五条不变量：
   - 新批**并入**队列、绝不替换；合并结果 id 升序、同 id 去重；
   - 投递按 id 升序、同 id 不投第二次（`presentedEventIds` + 队列去重双保险）；
   - 确认水位只增不减（逐 id 升序 ack；`pruneAcknowledgedEvents` 的水位同时清理队列）；
   - 5s 硬超时只做「放行投递」，绝不丢弃未确认事件；
   - 已投递未确认的批**不再**阻塞新批并入与投递（及时），未确认旧事件留在队列等确认（完整）。
   队列条目的移除只剩三条路径：ack 成功、daemon 终态拒绝（死信屏障）、或角色/作用域重置（同时清
   `presentedEventIds`，事件可被重新投递）。**自身 ack 曾失败的事件不在下一次 settle 里重试**（保持 H1
   的「失败不重放、由游标推进收敛」语义与退避计数），但它仍留在队列里，直到游标或作用域重置把它确认掉。
8. **堵住 degraded 绕道重排的后门**：`#runPlanRow` 的 `catch (error instanceof PlanWaitingHistoryError)`
   分支原先无条件 `#scheduleWaitingHistoryRetry`，是「degraded 不许进重试环」唯一还能绕过的口子；
   现在重排前必须过 `retryRingAuthorized(retryError)`——行已记 `degraded` 时只再记一次原因，等下次刷新。

### 双审第二关（oracle）裁定后的两处应修

9. **忙碌路径的批不得 abort 用户 turn**（偏致命）。忙碌时批是以 `triggerTurn: false` 的 queued follow-up 投出的，
   **骑在用户自己的 turn 上**；而 `hasSubstantiveWork` 是 `loseRole`（`packages/herdsman-pi/src/index.ts:879`）与
   `resetForScopeChange`（:922）两处 abort 门的**唯一**闸门——忙碌路径下它为 false 时，owner/作用域变更会
   `ctx.abort()` 掉用户正在跑的 turn（oracle 复现：busy ctx → 5s 强制放行 → `agent.orchestrator.changed` → `aborts:1`），
   违反契约「Never abort a normal user-triggered turn」（`docs/plans/archived/2026-07-14-pi-orchestrator-wake.md:32`）。
   修法 1（采用）：`hasSubstantiveWork: orchestratorBusy || (previousBatch?.hasSubstantiveWork ?? false)`（:836；注入口 `orchestratorBusy = ctx.isIdle?.() === false`，:786）。
   本字段写入点：:836（本处）、:1351（`message_end` 非空正文）、:1360（`tool_execution_start`）、:1364（`tool_result`）；读取点只有两处 abort 门：:879（`loseRole`）、:922（`resetForScopeChange`）；
   `packages/herdsman-pi/src/upstream-error.ts:162` 的 `hasSubstantiveWork(string)` 是同名函数，与本字段无关。
   **选法依据（保守超集）**：两处门的判据都是 `herdsmanTriggered && !hasSubstantiveWork` —— **只有 false 才会 abort**。
   把忙碌路径写入的值从 false 改成 true，只会把「可 abort 的 turn 集合」**单调缩小**（宁可漏 abort，也不误伤用户 turn），
   因此不必新增 `rideAlongTurn` 这种独立字段；代价是下面「补记」里那两组**漏 abort** 的观察项。
   （不再使用「两处门都不需要纯 Herdsman 语义」这个理由：门本身只判 false/true，不能据此证明语义等价。）
10. **ack 尝试计数取活值 + 死信出队**（低）。`acknowledgeEventIds` 原先用调用方传入的**投递队列快照**当基数
   （`(event.attempts ?? 0) + 1`），队列拷贝自注入时就未被刷新，计数会**恒为 1**、`MAX_ACK_ATTEMPTS`（=5）死信在本路径记不出来；
   现在基数先读活投影 `state.pendingEvents.find(...)?.attempts`（回退到队列拷贝），失败后把 `updatedEvent` **回写**队列条目，
   并给死信分支补 `dropUnackedDelivered(event.id)`。「自身 ack 失败后不再由 settle 重试」的语义不变（见下 ①）。
   可达性更正（本项**不是**纯防御）：① 的两条例外会让「曾失败」的事件重新进入 ack 集合（活投影里没有该行 ⇒ `?? 0` 兜底），
   此时计数从队列拷贝继续累加，能真实走到 `MAX_ACK_ATTEMPTS`=5 的死信；已由用例
   `accumulates ack attempts for an event whose live row vanished and dead-letters it at the cap` 钉住
   （施修前形态 FAIL：`expected 6 to be 5`）。

### 审校后必须记清的四条口径（本轮只落笔记、未改代码）

① **settle 的 ack 集合语义**＝**活投递队列 ∩ 「活投影里该行 attempts === 0（或活投影里根本没有该行）」**，且包含跨批 carry-over：
从未尝试过 ack 的旧批事件会随新批一起按 id 升序确认。H1 只定义过「批次 events 快照」这一层，没有定义过这层。
**两条真实例外**让「曾失败」的事件仍会重新进入 ack 集合（本路径**不是**无条件不重试）：
  (a) `syncPendingEventsFromServer` 用服务器列表**整体替换** `state.pendingEvents`（`packages/herdsman-pi/src/index.ts:997-999`）且完全不碰 `unackedDelivered`
      —— 行被替换/消失时失败计数随投影一起丢掉，事件得到一次重试；
  (b) 死信屏障过滤 `id > failedWakeThroughEventId`（:950-951 与 :997-998）—— 被屏障遮住的 id 不在投影里，
      同样落到 `?? 0` 兜底，也会被重试一次。
`?? 0` 兜底已在代码里注明「有意给一次自愈机会」（:1458-1464）：这类事件若不再补一次尝试，就会永远停在 delivered/未确认；
补齐后计数从**队列拷贝**继续累加（=第 10 项的活值读取 + 回写），直到 `MAX_ACK_ATTEMPTS` 死信或游标收敛。
唯一不会重试的情形：活投影里该行**仍在**且 attempts>0（包括 resync/transient 失败留下的 `nextAttemptAt` 行）——那种交给游标推进收敛。
② **忙碌路径 `triggerTurn: false` 下「acked ≠ 编排者已看到」**：queued follow-up 的内容会挂进当前上下文并留待消费，
但 ack 成功只表示 daemon 收到了确认，不保证编排者已经读到（尤其该 follow-up 尚未被消费、或被上下文压缩挤掉时）。
这是**设计取舍**（不打断用户 turn 优先于可见性保证），不是 bug；代价是存在「acked 但未见」窗口。
③ **degraded 行失去 10s 环后的收敛路径变慢**：不再有 10s 重排，收敛依赖 15min 的 `RECONCILE_INTERVAL_MS`
（`src/daemon/service.ts:42`）与 reconcile 候选扫描，最坏（本轮候选未命中、等下一轮）**约 2h** 才落 `agent.failed`。
因此本轮的结论只能说「**检测段**落库秒级」，**不能说**「degraded 行的终态事件秒级」；degraded 行的短收敛路径留给 Phase 2。
④ **空正文放行（confirmed 直放）当前没有任何「空正文」信号**：`src/observability/agent-index-service.ts:1902-1908` 的 `confirmedTerminal`
遇到 `freshIsTerminal && textMatches` 但**正文为空**时会走 `degradeOrRelease` 的 `{ staleSnapshot: false }` 分支（放行、且不标 `degraded`），
除 :1960 那条通用 `console.log(… confirmed=…)`（不带正文状态）外**既无空正文计数也无标记** —— 从日志上无法把「正文非空放行」
与「正文为空放行」区分开 → Phase 2 之前必须补一条空正文计数/日志（或由 Phase 2 的 `upstream_empty_turn` 告警覆盖），否则这条路径无法观测。
**滞留边界（④ 的后半）**：若 pane 一直被用户 turn 占用且此后不再有新事件，投递队列条目与被 daemon 标为 `delivered` 的行
可以**长期停留**（没有超时丢弃），性质为**有界**（条数随该 pane 的未确认事件数有界；条目只在 ack 成功 / 死信 / 作用域重置时移除）
且**不丢不重**（不丢：仍可被下一次注入并回批次确认；不重：`presentedEventIds` ＋ 队列同 id 去重保证同一 id 不二次呈现）。

### 保守超集的代价与其它观察项（补记）

- **漏 abort 的两个组合**（修法 1 的已知代价，方向只会是「漏」，不会误伤用户 turn）：
  (a) 正在跑**我们自己的**唤醒轮、而新事件触发 5s 强制放行时 `ctx.isIdle?.()` 报 busy ⇒ 新批被标 `hasSubstantiveWork=true`，
      于是 owner/作用域变更时连我们自己的那个唤醒轮也不再被 abort（它的内容已呈现、批已作废，属白跑一轮，无用户损失）；
  (b) `wakeForcedRelease` 已置位而 turn 尚未启动的竞态窗口内建立的批，可能继承 busy 标记 ⇒ 同样只表现为漏 abort。
  两者记为观察项：将来若需要「唤醒轮该 abort 就 abort」，得另想办法区分「我们自己的轮」与「用户的轮」，不能只看 `isIdle`。
- **屏障过滤造成的 pending/queue 非对称**：死信屏障只作用于 `pendingEvents`（:950-951、:997-998），投递队列直到 ack 死信分支才出队；
  这期间「投影里看不到、队列里还在」是**正常中间态**（也正是 ①(a)(b) 两次自愈重试的来源），不是不一致 bug。
- **队列条目可长期滞留**：见 ④ 末尾的滞留边界——有界、不丢不重；没有超时丢弃是**有意**的（丢违反完整性，超时重投违反不重）。

### H1 基线关系与提交顺序约束（重要）

- Phase 1（含后续两轮追加：批次并入/队列、degraded 后门、忙碌路径不 abort、ack 计数与死信出队）与 H1 都**尚未提交**，
  同在 `fix/delivery-latency-phase1` 与 `fix/terminal-event-delivery-h1` 两个分支（两分支指向同一提交，无提交、无 stash）。
- 文本层两者**可分离**：把「H1 树 → 当前工作区」的 Phase-1-only patch 应用到 H1 树后与工作区逐字节一致
  （7/7 文件），且同一 patch 也能零 fuzz 应用到干净 HEAD（仅行号 offset）。
- **提交顺序必须是 H1 在前、Phase 1 在后**：Phase 1 的集成断言（`deliverable` 行、`invalidated_reason`、
  孤儿终态行语义）以 H1 的行为为前置；反过来先提 Phase 1 会让这些断言在中间态无意义。
- 核对手段：`git apply -R --check <H1 patch>` + 逐 hunk post-image 逐字比对脚本（见「来源」）。

## 被放弃的方案（必填）

- **保留指数退避、只把上限调小**：调小上限仍需为「历史还没刷出来」付多次翻倍的等待，第一轮终态依旧可能
  等十几秒；固定小间隔 + 硬预算更可预测，且 1500ms 覆盖了实测的写入延迟量级。
- **`turn.confirmed` 仍走 degraded 但延长重试**：degraded 会触发 `invalidateById(..., "degraded_retry")`
  把已写入的终态作废再重排，等于用「已确认的事实」换一圈重试，属于把延迟问题转成丢失风险。
- **忙碌时仍然直接注入（打断当前工具链）**：会打断编排者正在跑的工具链，用户已拍板 Q1=A（不打断）。
- **忙碌时无界等待 settle**：正是本次要修的坏点——settle 可能永不到来（pane 关页、长工具链）。
- **5s 超时「丢弃并重投」旧批（本轮修正掉的实现）**：旧批事件已在 `presentedEventIds` 里，
  重投会被去重挡掉，结果是既没 ack 也没再投，只能靠游标兜底；改为「并入 + 队列留待确认」。
- **未确认旧事件一律在下次 settle 重试 ack**：会重置失败事件的 attempts/退避计数，并破坏 H1 的
  「失败不重放、由游标收敛」语义（K0/K1 收敛用例会退化）；因此按「自身 ack 曾失败就不重试」区分。
- **新增 `rideAlongTurn` 布尔 + 在两处 abort 门加 `&& !batch.rideAlongTurn`**（双审给的备选修法 2）：
  只有当真有一个读取点需要「纯 Herdsman 工作」语义时才必要；把 `hasSubstantiveWork` 的读取点穷举后，
  两处（`loseRole` / `resetForScopeChange`）都只回答「这个 turn 能不能被我们 abort」，所以在**写入侧**标对更小、
  不新增状态字段，修法 2 放弃。

## 来源

- 用户拍板（2026-09-30）：Phase 1 立项、Q1=A，以及本轮两处（批次不丢、堵 degraded 后门、补笔记）。
- 取证基线：`.agents/notes/20260930-terminal-event-delivery-latency-observation.md`（生产库只读取证）。
- 相关笔记：`20260930-terminal-event-delivery-h1.md`、`wake-delivery-r4f1-remaining-risks.md`。
- 代码位置：`src/observability/agent-index-service.ts`（第 1–3、8 项）、
  `packages/herdsman-pi/src/index.ts` 与 `packages/herdsman-pi/src/wake.ts`（第 4–7 项）。
- 回归用例：`test/integration/agent-index-service.test.ts`（S5 / S9 / S10）、
  `test/integration/turn-completion-signal.test.ts`、`test/unit/herdsman-pi-extension.test.ts`
  （idle 直投、5s 强制放行、批次并入、ack 计数累加/死信、`never aborts a user turn that a queued busy-path batch rides on`）、
  `test/unit/herdsman-pi-wake.test.ts`（settle 常量）。
- 双审量裁依据：`docs/plans/archived/2026-07-14-pi-orchestrator-wake.md:32`（owner 变更只允许 abort Herdsman 唤醒轮）。
- abort 回归的 FAIL 证据：`/tmp/failbefore-abort-mut.log`（把 `:836` 还原成 `previousBatch?.hasSubstantiveWork ?? false` 的突变运行，
  与 oracle 复现**同一行、同一断言** `expected 1 to be +0`）；**不要**取 `/tmp/failbefore-abort-pre.log`——那是 H1 树上的运行，
  失败在更早的 `hiddenMessages` 长度断言（H1 忙碌路径根本不投递），证明不了 abort 门。
- ack 累加/死信回归的 FAIL 证据：`/tmp/failbefore-ackaccum-mut.log`（把 :447-454 / :465-470 / :495-501 三处还原后的运行，`expected 6 to be 5`）。
- degraded 日志的可复现样例：`node_modules/.bin/vitest run test/integration/turn-completion-signal.test.ts -t 'W13: pi timeout without advance emits degraded with null lastAssistant' --disableConsoleIntercept`
  （本机实测输出：`Herdsman emitted degraded status event { agentId: 'ag_…', attempts: 0, degradedReason: 'no_advance_from_input', herdrSessionName: 'default', paneId: 'wJ:p2', planId: 1 }`）。
- 索引：未运行 `scripts/notes-index.sh`（本轮任务约束禁止改动仓内既有文件之外的内容；`INDEX.md` 在
  `.gitignore` 内、由本地生成）。

## 遗留 / 下一轮

- **`degradedReason` 可观测性：已落地（日志档，owner 拍板选 B）**——只做可观测、不改行为：
  在降级发生的那一处经 `#logDegradedRelease`（`src/observability/agent-index-service.ts:1748-1757`）打 `console.warn`，
  三个调用点：`degradeOrRelease` 闭包 `:1933`（`no_advance_from_input` / `expected_text_mismatch` / `non_terminal_assistant`，
  仅 `!confirmedTerminal` 时打）、无 turn 信号分支 `:2016` 与 `:2024`。
  日志字段：`planId / agentId / attempts / degradedReason / herdrSessionName / paneId`；消息 `"Herdsman emitted degraded status event"`。
  **没有把 reason 放进失败事件 payload**：7 处 `#appendPlanFailedEvent` 调用点只拿得到 `row.lastError`（=`degraded`），
  要透传具体 reason 得给 plan 行加持久字段 ⇒ schema/迁移，按 owner 的禁止条件退回日志档。
  为让 `planId` 有值，本已存在的内存字段 `StatusEventPlan.planId` 在 `#runPlanRow` 的两处 `activePlan` 构造里填成 `row.id`（`:933`、`:946`，
  无 store 的内存 plan 仍为 `null`）——仅用于日志定位，无行为变化。
- **Phase 2 仍需做的语义修复（未完）**：缺 assistant 内容不得判 `failed`、`wake.ts` 空 `completed` 过滤、
  按 `degradedReason` 分类计数/告警（现在只有原始 warn 日志，没有聚合指标）。
- **④ 的空正文计数/日志尚未实现**（本轮审理明确将 ①②③④ 归为「只改笔记、不改代码」）：Phase 2 前应补一条
  「confirmed 直放但正文为空」的计数/日志，或在 Phase 2 的 `upstream_empty_turn` 告警里覆盖，否则该路径继续静默。
- **degraded 行的收敛仍是 15min 级**（按 ③：最坏 ≈2h 落 `agent.failed`）：短收敛路径留 Phase 2。
- **测试的 idle 语义迁移（需 reviewer 过一遍）**：`WAKE_SETTLE_MS = 0` 后，若干既有扩展用例的
  前置条件由「靠 500ms settle 窗口在定时器触发前翻转 idle」改为「一开始就 busy / 显式 `ctx.setIdle`」。
  断言未弱化（只增不减），但覆盖的时序与旧版不同，属有意的语义迁移。
- **本轮的取证与核对产物都在 /tmp，3 天后可能被清**：H1 基线 patch、Phase-1-only patch、
  FAIL→PASS 日志、逐 hunk 核对脚本、可分性脚本均为 `/tmp` 临时物；如需长期留存，应把结论（而非
  patch）落进笔记或 plan 的 progress 段。
- **Phase 2 四项前置**（Phase 1 期间一行未动）：缺 assistant 内容禁判 `failed`、`wake.ts` 空 `completed` 过滤、
  Herdr `done` 语义映射纠正、上游空轮次告警 `reason: upstream_empty_turn` + 同 pane 抑制去重。
  Phase 1 的「秒级检测 + 有界投递」是它们的前提：不做 Phase 1，告警预算与时序基线都会被分钟级延迟淹没。
- **`deliveredBatch` 记录在大批未确认时仍会被重连规则清掉**（H1 行为，本轮未改）：此时事件留在投递队列
  里等下一次注入并回批次；若长时间无新事件，则靠 daemon 游标/死信路径收敛。

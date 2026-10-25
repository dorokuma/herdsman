---
status: active # active | superseded
superseded_by: ""
supersedes: ""
# 模块可选值: observability, daemon, cli, config, db, herdr, shared, herdsman-pi, herdsman-herdr-plugin, release
模块: "observability"
---

# agy 叫醒竞态：working 完成事件改为 defer、同 terminal 完成事件去重、叫醒前检测输入框草稿（含 ack 游标下界 / 草稿框顶消歧 / peek 写盘复核 / 300s 重投窗口快照四轮加固）

## 一句话结论

`liveWorkingTerminalIds` 命中的完成事件**不能再当永久噪声静默 ack**，必须放进 `deferredWorkingEventIds` 保持「未 ack、未 presented」等下一轮重投，否则 `agent.done` 与 `agent.list` 状态刷新之间的竞态会永久吞掉真正的最终结果；与之配套，同一 `terminalId` 在一批候选事件里出现多条完成事件时，只有**最新一条能产出有效 completed outcome 的完成事件**进入唤醒批次，更早的按 `superseded by newer completion for terminal <id>` 抑制并 ack；另外 `WakeWaiter` 在 `triggerWakePrompt` 前要先 `herdr pane read --lines 40` + `parseAgyComposerDraft` 看 orchestrator 输入框是否有半成品草稿，有草稿就推迟本轮叫醒，但**解析不到框必须回退成照常叫醒**，避免解析器退化把唤醒永久堵死。第二轮审查又补上三处：ack 是 `id <= ?` 的批量结算，高 ID ack 会顺带吞掉低 ID 的未决 deferred 事件，因此 hook 侧引入 `pendingAckEventIds` 与 `< minDeferredId` 游标下界；`readComposerDraft` 的异步等待之后必须复核 `turnEpoch` / `waiterPid` 再叫醒；composer 框顶改为「与框底等宽同款 + 外层非空优先」以挡住草稿内部的「横线 + `>` 行」。第三轮再补两处：**`peekNextWakeBatch` 写盘前必须复核 `turnEpoch` / `waiterPid`，且 `presentedEventIds` 只能并集不能覆盖**（异步等待期间新回合 `PreInvocation` 写入的 presented 会被旧 waiter 用调用前的旧集合抹掉，导致重复注入）；**`sessionState` 里持久化未消费候选事件快照 `retainedCandidateEvents`**，因为 daemon 的 `listAfter` 只重列 `last_attempt_at` 在 `REDELIVERY_FRESHNESS_MS`（300 秒）内的 `delivered` 行，长草稿 / 长时 working-defer 一旦超过 300 秒，事件就从 `connState.events` 消失——既会永久丢通知，也会让 ack 游标下界失去依据而被高 ID ack 顺带吞掉。

## 背景

`20261024-agy-smart-wake-standby-cross-workspace.md` 引入的中间回合抑制只有一个信号：完成事件的 `terminalId` 在实时 `agent.list` 里仍是 `working` ⇒ 判为未决中间回合 ⇒ 进 `suppressedEventIds` 静默 ack。这条规则把「中间回合」和「状态刷新竞态」当成同一件事，但两者在数据上不可区分：

1. **竞态吞事件**：peer 真正跑完最后一轮时，`agent.done` 落库通常早于 Herdr 侧状态从 `working` 翻转。`WakeWaiter` 的 `peek` 若正好落在这个空隙，实时状态还是 `working`，真正的最终结果被当成中间回合**永久 ack**，游标推过去之后再也没有第二次机会——表现就是「工人干完了活，主代理永远等不到唤醒」。
2. **同批重复注入**：改为 defer 之后出现新问题。中途 `agent.done`（未 ack、未 presented）与随后的最终 `agent.done` 会同时留在候选批里；等状态追上后两条一起被投影成 `completed`，一次唤醒往 `PreInvocation` 注入**同一 peer 的两条 outcome**（中途轮文本 + 最终轮文本），既重复又自相矛盾。
3. **叫醒打在草稿上**：`triggerWakePrompt` 走 `herdr agent prompt`，会把文本提交进 agy 输入框。用户正在 orchestrator 窗格里打字（草稿没提交）时叫醒，等于把 `[herdsman-wake]` 混进用户正在编辑的那一行，破坏用户输入。
4. **长草稿 / 草稿内含横线**：`parseAgyComposerDraft` 取「倒数第二条横线」当 composer 框顶。用户在多行草稿里自己敲了一行 `--------` / `────────`（正好命中横线正则）时，倒数第二条横线变成草稿内容，`bottom <= top + 1` 成立 → 返回 `undefined`；即便没返回 `undefined`，框区域也会被截断成只剩横线之后的那一行。同时 `--lines 12` 的读取窗口容不下多行草稿（框顶在窗口之外），同样解析失败。

## 决策

1. **working 命中的完成事件从「永久 suppress」改为「defer」**（`src/agy/hook.ts: projectAgyOutcomes` / `peekNextWakeBatch` / `takeNextWakeBatch`）
   - 返回值新增 `deferredWorkingEventIds`：命中的事件既不 ack 也不进 `presentedEventIds`，日志文案改为 `deferred: peer terminal <id> still working in agent.list`。
   - `peekNextWakeBatch` 返回 `hasDeferredWorking`，`WakeWaiter` 据此置 `retryWakePending = true`，让下一轮 poll（活跃期 `DEFAULT_POLL_INTERVAL_MS = 1500ms`）重新投影，不依赖 push；真·中间回合（peer 一直 `working`）只会一直 defer，不产生叫醒，也不会把事件 ack 掉。
   - `takeNextWakeBatch` 只在 `outcomes.length === 0` 时对**永久**抑制项（无 terminalId、非 outcome 类型、fallback 被完成事件取代、上游错误文本、空 completed 文本）执行 ack；deferred id 不参与 ack/presented，`hasDeferredWorking` 与 `hasOutcomes` 是两条独立信号。
2. **同 `terminalId` 多条完成事件按「最新有效完成」去重**（`projectAgyOutcomes`）
   - 先扫一遍候选事件，按 `terminalId` 记录**最后一条能产出有效 completed outcome 的完成事件 id**：要求 `isCompletionEvent` 且 `outcomeKind === "completed"`，且通过 `fallbackOutcome` 被同 pane 完成事件取代、`isUpstreamErrorText`、空文本三项检查（与主循环同一套判定，避免两条路径口径漂移）。
   - 主循环里（在 working-defer 判定之后）：同 `terminalId` 且 `event.id` 小于该 id 的**完成事件**进 `suppressedEventIds`，日志 `superseded by newer completion for terminal <id>`，随批次一起 ack。
   - 去重只对完成事件生效：同 terminal 更早的 `agent.failed` / `agent.discarded` 不受影响，避免顺手改变失败/停滞事件的投递语义。
   - 最新一条完成事件本身无效（空文本 / 上游错误）时，锚点退回到更早那条有效完成事件；无效的新事件继续走原有的「空 completed 文本 / 上游错误」抑制分支，不会被注入成 `(no assistant message)`。
3. **叫醒前检测 composer 草稿**（`defaultReadPaneText` / `parseAgyComposerDraft` / `WakeWaiter`）
   - `defaultReadPaneText` 用 `herdr pane read <paneId> --lines 40`；`--lines 12` 只看得到 composer 底部几行，多行草稿的框顶落在窗口外就定位不到框。
   - 框底固定为状态栏上方最后一条横线；**框顶从倒数第二条横线往前回溯，取最后一条「下一行以 `>` 开头」（`/^>(?:\s|$)/`）的横线**——agy composer 框顶横线的下一行必然是 `>` 提示行，而用户草稿里的横线后面不会。
   - 找不到这样的框顶、或 `bottom <= top + 1`，返回 `undefined`；`readComposerDraft` 据此返回 `unknown`，`WakeWaiter` **照常叫醒**（回到修复前行为）。这是刻意的防死锁设计：解析器任何一次退化都只可能少推迟一次叫醒，不可能把叫醒永久堵在「一直判定有草稿」上。
   - 命中草稿时只推迟本轮：`retryWakePending = true` 后下一轮 poll 重试；用户自己提交草稿会触发 `PreInvocation`，`takeNextWakeBatch` 照常投递该批次，事件不会因为推迟而丢失。推迟期间 `waitTimeoutMs`（默认 `DEFAULT_WAIT_TIMEOUT_MS = 24` 小时）仍是硬上限。

### 第二轮加固（2026-10-25，`fix/agy-wake-race-and-draft` 第二轮代码审查）

第 1 轮的 defer 修掉了「低 ID 事件被静默 ack」最常见的那条路径，但 ack 的**游标语义**本身还有三个漏口：

4. **`agent.notifications.ack` 是 `id <= ?` 的批量 ack，高 ID 的 ack 会顺带吞掉低 ID 的未决 deferred 事件**
   - 事实：`src/db/agent-events.ts: markAcked` 用 `where ... id <= ?` 把整段标成 `acked`，`src/observability/agent-orchestrator-service.ts: #ack` 再把 scope 的 `ackedEventId` 推到该 ID；而 `peekNextWakeBatch` / `takeNextWakeBatch` 的候选过滤是 `event.id > ackedEventId`。三者叠加 ⇒ 只要对 `id = 105` 发一次 ack，`id = 100` 的 deferred 事件就永久消失。
   - 决策：**在 hook 侧维护一条 ack 游标下界，绝不对 `>= minDeferredId` 的事件发 ack**（daemon 侧 ack 契约不动）。
     - `AgySessionHookState` 新增两个字段，职责严格分离：
       - `deferredEventIds?: number[]`：当前批被 `working` 竞态 defer 的事件 id，纯记录 + 供观测。
       - `pendingAckEventIds?: number[]`：**已经 presented（已抑制或已交付）但 RPC ack 被扣住**的事件 id。与 `deliveredEventIds` 分开是本次加固的核心：`rearmDeliveredEvents` 只操作 `deliveredEventIds`，如果把已交付事件塞回去，宿主忽略 `decision: continue` 时会被二次注入。
     - `peekNextWakeBatch` / `takeNextWakeBatch` 每次投影后写 `sessionState.deferredEventIds = deferredWorkingEventIds`，并算 `minDeferredId = Math.min(...deferredWorkingEventIds)`（空则 `+Infinity`）。`outcomes.length === 0` 分支里遍历 `suppressedEventIds`：`< minDeferredId` 照常 ack + presented；`> minDeferredId` **不发 RPC ack**，只 `presented.add` + 进 `pendingAckEventIds`（仍记 presented，避免每轮重复打抑制日志、重复投影）。
     - `flushDeliveredAcks`（Stop 开头）把 `pendingAckEventIds` 与 `deliveredEventIds` 合并去重排序成 `allToAck`，逐项判断：`< minHoldbackEventId` 才发 ack；`>= ` 的移入 `pendingAckEventIds` 并同时把 `deliveredEventIds` 里对应项清掉。瞬时失败时剩余项按同一规则分流（`>=` 进 pendingAck，`<` 回 `deliveredEventIds` 等下次重试）。
     - `minHoldbackEventId` **不信任记录的 deferral，而是按当前候选集现场重算**（`resolveMinHoldbackEventId` → `fetchLiveWorkingTerminalIds` + `projectAgyOutcomes`，取 `deferredWorkingEventIds ∪ outcomes` 的最小值）。原因：peer 一旦不再 `working`，原来被 defer 的低 ID 事件会**当场变成尚未消费的 outcome**，此时若按「deferral 已解除」放行高 ID ack，照样把它吞掉。内部重算用注入的 no-op logger，避免和紧随其后的 `takeNextWakeBatch` 打重复 warn。
     - 候选集为空时（`peek` / `take` / `flush` 三处）把 `deferredEventIds` 清空，防止过期值永久压住 ack 游标。
   - 效果：任何时刻都不会对 `> minDeferredId` 的事件发 ack；低 ID deferred 事件不会被吞；已抑制/已交付的高 ID 事件始终留在 `presentedEventIds` 里，不会重复注入或重复打日志；deferral 一解除，`pendingAckEventIds` 顺次被 ack。
   - **没有采用**「deferral 解除就当场放行」：那正是会吞掉「刚变成 outcome 的低 ID 事件」的写法，见上。
   - **没有采用**「在 `peek` 里顺手 flush `pendingAckEventIds`」：`peek` 在看到 `outcomes.length > 0` 时本来就什么都不 ack，而此时候选集里存在未决 outcome，提前 ack 高 ID 一样会吞人；统一交给 `flushDeliveredAcks` 的现场重算更安全。
5. **`readComposerDraft` 异步等待后未复核 `turnEpoch` / `waiterPid`**
   - 事实：`readComposerDraft` 是异步 I/O（`herdr pane read`，最长 5 秒）。这期间用户可能已回车提交新回合（`PreInvocation` 把 `turnEpoch` +1），或新的 `Stop` 已拉起新 `waiterPid`。
   - 决策：在 `if (composerDraft === "has-draft")` 的 `else` 分支开头（执行 `rearmDeliveredEvents` / `triggerWakePrompt` **之前**）重读 `sessionState`，若 `turnEpoch` 或 `waiterEpoch` 与本 waiter 的 `waiterEpoch` 不符、或 `waiterPid` 已定义且不是本进程，直接 `return {}`。`waiterPid` 判定带 `!== undefined` 保护：Stop 先写 `waiterEpoch` 再 spawn 再写 `waiterPid`，waiter 可能在 `waiterPid` 落盘前就跑到这里，不能因未定义而误判。
6. **`parseAgyComposerDraft` 框顶消歧：草稿内含「横线 + `>` 行」**
   - 事实：从下往上找第一条「下一行匹配 `/^>(?:\s|$)/`」的横线，会命中用户草稿内部的横线。草稿内横线下一行是空 `>` 时解析器返回 `""`（空草稿），`readComposerDraft` 判 `empty` ⇒ 直接发送 wake，把 `[herdsman-wake]` 混进用户正在编辑的多行草稿。
   - 决策：composer 框由同一个 TUI 组件绘制，框顶与框底**横线字符类型相同且通常等长**。因此：
     1. 先取 `separatorIndexes.slice(0, -1)` 中「下一行匹配 `/^>(?:\s|$)/`」的候选；
     2. 优先筛出与 `lines[bottom].trim()` **完全相等**的候选 `matchingFrame`（非空则用它，否则回退到全部候选）；
     3. 从下往上遍历，算出 `draft`：`draft.trim().length > 0` 立即返回；为空说明最内层那条横线 + `>` 是草稿内容，继续看外层候选；
     4. 全部为空才返回 `""`（框里真是一个空 `>`），一个候选都没有才返回 `undefined`。
   - 之所以可以放心往外层走：agy TUI 只有一个底部固定 composer 框，历史消息里用户发出的 prompt 只显示 `> 文本`，上下**没有**盒绘横线；上方会出现盒绘横线的唯一来源就是草稿本身。

### 第三轮加固（2026-10-25，`fix/agy-wake-race-and-draft` 第三轮代码审查）

第二轮把「异步等待后复核 `turnEpoch`」用在了 `readComposerDraft` 之后，但同一个竞态窗口在 `peekNextWakeBatch` 自己的写盘路径上还没堵；另外第二轮的 ack 游标下界完全依赖 `connState.events`，而 daemon 对 `delivered` 行只重投 300 秒：

8. **`peekNextWakeBatch` 写盘前复核 `turnEpoch` / `waiterPid`，`presentedEventIds` 改为并集**
   - 事实：`peekNextWakeBatch` 在 `fetchLiveWorkingTerminalIds`（`agent.list`）以及抑制项 `agent.notifications.ack` 的 await 期间，用户可能已经提交新回合：`PreInvocation` 把 `turnEpoch` +1，其 `takeNextWakeBatch` 会把这批事件写进磁盘的 `presentedEventIds` 与 `deliveredEventIds`。旧 waiter 的 peek 手里拿的是**调用前**的 `presented` 集合，修复前直接 `freshState.presentedEventIds = [...presented].slice(-500)` **覆盖**写盘 ⇒ 新回合刚记上的 id 被抹成 `[]` ⇒ 下一轮 `PreInvocation` 把同一批再注入一次。
   - 决策：
     - `peekNextWakeBatch` 增加可选参数 `expectedWaiter?: { waiterEpoch: number; waiterPid: number }`，只有 `WakeWaiter` 调用时传 `{ waiterEpoch, waiterPid: process.pid }`；`takeNextWakeBatch`（`PreInvocation` / `Stop` 路径）不传，不启用守卫。
     - 两个写盘点（`candidateEvents.length === 0` 分支、函数末尾）都先 `readSessionState` 拿 `freshState`，再用 `isWaiterSuperseded()` 比对 `turnEpoch` / `waiterEpoch` / `waiterPid`（三项都带 `!== undefined` 保护，理由同第二轮第 5 条）；已被取代就**放弃写盘**并返回 `{ hasDeferredWorking: false, hasOutcomes: false }`，让 waiter 在下一轮循环顶部的同类检查里自然退出。
     - 未被取代时 `presentedEventIds` 写并集：`mergeEventIds(freshState.presentedEventIds?.filter((id) => id > ackedEventId), [...presented]).slice(-500)`。磁盘上独有的 id 一定是更新回合写的，不能被旧 peek 抹掉。空候选分支也改成写 `freshState`（而不是循环开头读的 `sessionState`），避免用过期的整对象覆盖。
   - **没有采用**「只在 `outcomes.length > 0` 时守卫」：deferred / suppressed 分支同样会覆盖 `presentedEventIds` 与 `pendingAckEventIds`。
   - **没有采用**「拿文件锁 / 改成单写者」：hook 是多个短命进程 + 一个常驻 waiter 共用一个 JSON 文件，引入锁只把「少写一次」变成「赌调度」；比较 epoch 后放弃写盘是无锁下最便宜且可推理的作法。
9. **未消费候选事件快照 `retainedCandidateEvents`：跨越 daemon 的 300 秒重投窗口**
   - 事实：`src/db/agent-events.ts: reservePending` 在首次 `agent.orchestrator.get` 时把 `pending` 行改成 `status = 'delivered'` 并记 `last_attempt_at`，但**不刷新**已 `delivered` 行的 `last_attempt_at`；`listAfter` 只返回 `last_attempt_at >= now - REDELIVERY_FRESHNESS_MS`（`300_000`）的 `delivered` 行。⇒ 用户在输入框编辑草稿超过 300 秒，或某个完成事件被 working-defer 超过 300 秒后，`connState.events` 不再包含这条**仍未 ack、仍未消费**的事件：
     - (a) 草稿清空后 `peekNextWakeBatch` 看不到事件 ⇒ 不再叫醒，接下来的 `PreInvocation` 中 `takeNextWakeBatch` 也看不到 ⇒ 完成通知永久丢失（行停在 `delivered`）；
     - (b) 更糟的是 `resolveMinHoldbackEventId` 此时候选集为空返回 `+Infinity`，`flushDeliveredAcks` 会放行高 ID 事件的 ack，`markAcked` 的 `id <= ?` 把低 ID 的未消费事件在库里顺带吞掉。
   - 决策：把「仍未交出去的候选事件对象」持久化进 session state。
     - `AgySessionHookState` 新增 `retainedCandidateEvents?: AgentEventRecord[]`。
     - `collectWakeCandidateEvents(connState, presented, retained)` 按 `event.id` 合并 `retained` 与 `connState.events` 并去重（同 id 以 daemon 返回的为准），其余过滤条件不变；额外加一条：`event.herdrSessionName` 存在时必须等于 `connState.presence.herdrSessionName`，防状态文件跨越 workspace/session 后把别的会话的事件注入进来。
     - `resolveMinHoldbackEventId` / `peekNextWakeBatch` / `takeNextWakeBatch` 三处均传 `sessionState.retainedCandidateEvents`。
     - 写回时机：`peek`（两处）与 `take`（三处）每次写 state 时重算，保留条件 `!presented.has(event.id) || deliveredEventIds.has(event.id)`（`collectWakeCandidateEvents` 已经保证 `id > ackedEventId`），上限 `RETAINED_CANDIDATE_EVENT_LIMIT = 100`，空则 `[]`。
       - 用「未 presented **或** 仍在 `deliveredEventIds`」而不是只看「未 presented」：`Stop` 的 `takeNextWakeBatch` 会先给 `decision: continue` 把事件标 presented + 记进 `deliveredEventIds`，随后 `rearmDeliveredEvents` 又把它从 `presented` 摘回来；只按「未 presented」保留的话，这条事件在 `Stop` 写盘那一刻就会掉出快照，「宿主忽略 `decision: continue` + 用户写 10 分钟草稿」的场景依旧丢事件。等 `flushDeliveredAcks` 真正 ack（或移入 `pendingAckEventIds`）后，它既不在 `deliveredEventIds` 也已 presented，自然淘汰。
       - `peek` 看到 outcomes 时**不**标 presented（真正消费发生在下一轮 `PreInvocation` 的 `takeNextWakeBatch`），所以 outcomes 对应的记录会一直留在快照里，草稿写多久都不会掉；草稿清空后 `peek` 仍能看到 `hasOutcomes: true` 并叫醒，接下来的 `PreInvocation` 从 `retainedCandidateEvents` 取回并注入。
     - 淘汰：事件被 ack 后 `id > ackedEventId` 不再成立，下一次 `peek` / `take` 写盘时自动清掉；`peek` / `take` 的空候选分支也会把非空的 `retainedCandidateEvents` 一并清成 `[]`。
     - `!isLocalOwner`（`PreInvocation`）时与 `deliveredEventIds` / `pendingAckEventIds` 一起清掉 `deferredEventIds` 与 `retainedCandidateEvents`：所有权转移后这批事件的投递节奏归新 owner。
   - 代价：session state 文件多一段事件 JSON（上限 100 条，实际通常 1~3 条；事件记录里的 `createdAt` 经 JSON round-trip 后是 ISO 字符串，hook 侧不读该字段，`parseTimestampMs` 也兼容字符串）；以及「daemon 还认不认这条事件」不再是 hook 的唯一真相来源。
   - **没有采用**「让 hook 定期重新 ack / 刷新 `last_attempt_at`」：`reservePending` 不刷新已 delivered 行是 daemon 的投递语义（防离线 terminal 无限重投），hook 不该反向改 daemon 状态。
   - **没有采用**「把 `REDELIVERY_FRESHNESS_MS` 调大」：300 秒是所有订阅方（pi 扩展、`agent-event-reconciler` 的 E6 判定）共用的重投时限，为 agy 草稿场景放大它会把「关页后仍长期重投」的副作用铺到整条链路；hook 自己记住待办事件是本地状态，影响面最小。

## 边界与权衡（残余窗口与已知取舍）

1. **defer 不是无损的**：真·中间回合（peer 长时间 `working`）期间，其中途 `agent.done` 一直留在未 ack 状态，直到 peer 状态追上或 `waitTimeoutMs`（默认 24 小时）到期。代价是多留若干轮候选事件与每轮一条 `deferred:` warn 日志；收益是竞态下真正的最终结果不再被永久 ack。
2. **同批去重只看 `terminalId`，不看 pane generation**：同一 terminal 在同一批里的两条完成事件必然属于同一窗口的两轮，跨 generation 的取代已由既有的 `completedPanes` / fallback 抑制覆盖，不在此处重复判定。
3. **草稿推迟期间的轮询与日志频率（reviewer consider）**：推迟走的是与 deferred 重试相同的 `retryWakePending` 路径，因此在整个 `pollIntervalMs`（活跃期 `1500ms`）节奏上每轮一次 `herdr pane read` + 一条 `输入框有草稿，推迟叫醒` warn。用户长时间停在草稿上时 warn 会持续累积。当前不引入指数退避或「草稿未变化就跳过读取」的状态机：`WakeWaiter` 生产环境是 `detached` + `stdio: "ignore"` 拉起，stderr 直接进 `/dev/null`，日志量不构成生产压力；而退避状态机会把「草稿何时算变化」变成新的隐式状态，收益不抵复杂度。**作为已知观察项保留**：若将来把 waiter 的 stderr 接入 journal 或出现长草稿高频轮询的实测证据，再考虑对草稿推迟单独设一个更长的重试间隔。
4. **草稿检测只作用于 `triggerWakePrompt`**：`decision: continue` 的 immediate batch 不经过该检测（那条路径本来就不往输入框写文本）。
5. **ack 游标下界是 hook 侧的自律，不是 daemon 侧的强制**：daemon 的 `#ack` 只在「下一个可投递事件是 `pending` 且 `input.eventId > next.id`」时抛 `ORCHESTRATOR_EVENT_OUT_OF_ORDER`；已经 `delivered` 给本 terminal 的行不构成阻塞，所以高 ID ack 在服务端是**合法**的，吞事件只可能在 hook 侧防。若将来有别的事务方（pi 扩展、其它 hook）也往同一 scope 发 ack，需要同样的下界保护，或把约束下沉到 daemon。
6. **`pendingAckEventIds` 的滞留是有界的**：deferral 一直不解除（真·中间回合）时，被扣住的高 ID 事件会一直留在 `pendingAckEventIds` 里。它们已经 presented，不会重复注入；代价是这些行在 DB 里停在 `delivered`，以及 `presentedEventIds` 多占几项（该列表有 `slice(-500)` 上限）。
7. **`resolveMinHoldbackEventId` 每轮 Stop 多一次 `agent.list`**：仅在 `allToAck` 非空时发生，且 `takeNextWakeBatch` 紧随其后本来就要发同样的 `agent.list`，增量成本是一次 RPC。
8. **`retainedCandidateEvents` 是 hook 侧的第二真相来源（第三轮）**：它只保证 hook 自己不再丢事件，DB 里那条行仍停在 `delivered`，daemon 侧 reconciler / 重连重投逻辑照旧。若该行在此期间被 invalidated，`flushDeliveredAcks` 走 `isPermanentAckFailure` 分支把它按已消费处理并从快照淘汰，不会二次注入。快照上限 100 条，极端下超过部分从最旧开始被挤出去（与 `presentedEventIds` 的 `slice(-500)` 同类取舍）。
9. **peek 的守卫只防写盘，不撤回已发出的 ack（第三轮）**：peek 在守卫之前可能已对永久抑制项发过 `agent.notifications.ack`；这些 id 本身就是要么已呈现要么噪声，新回合的 `takeNextWakeBatch` 会从刷新后的 `connState` 看到新游标，不会因此多注入。

## 被放弃的方案（必填）

- **继续永久 ack，另加「状态竞态」白名单**（如给 `working` 命中加时间容差 / 要求 `agent.list` 连续两轮一致）：容差和「连续一致」都是拍出来的新 magic number，且对「中间回合」与「刚完成的竞态」这两类事件依旧给不出判别信号；改成 defer 后不需要区分它们，由下一轮实时状态自己给答案。
- **去重时保留同 terminal 的多条 outcome，仅在展示层合并**：`PreInvocation` 注入的文本就是宿主看到的唯一事实，展示层合并等于把「同一个 peer 两条互相矛盾的 last assistant」继续带给模型；在投影层只保留最新一条有效完成事件才是正确语义。
- **只按「最后一条完成事件」去重（不校验有效性）**：最后一条若是空文本或上游错误文本，前一条有效结论会被它一起吃掉，唤醒批次里只剩一条没有正文的 completed；因此锚点必须是「最后一条**有效**完成事件」。
- **把 `terminalId` 去重扩展到 `agent.failed` / `agent.discarded`**：失败与停滞事件表达的是「这一轮没了」，与更早已完成轮次不是同一维度的取代关系，一并压掉会改变既有投递语义，超出本次修复范围。
- **草稿非空时无限期等待（直到草稿清空）**：解析器一旦误判（新 UI、状态栏变化、框结构改动）就等于永久不叫醒；解析不到框回退成照常叫醒，把最坏情况限制在「推迟若干轮」而不是「永不叫醒」。
- **把读取窗口继续加大（`--lines 80/120`）**：窗口越大，历史消息里的横线与 `>` 引用行越多，误定位概率上升；`40` 行足以覆盖 composer + 状态栏 + 常见多行草稿，且框顶改为「下一行必须是 `>`」后已不依赖窗口大小来消歧。
- **改用 `herdr pane read` 的结构化输出（若将来提供）而不是解析 TUI 文本**：现在 CLI 只给终端文本，解析框线是唯一手段；该风险与回退策略见「边界与权衡」第 3 条。
- **（第二轮）把 daemon 侧 ack 从 `id <= ?` 改成按事件集合 ack**：那是 orchestrator 的对外契约，pi 扩展、`herdsman-pi` 包和历史客户端都依赖「一次 ack 结算到游标」的语义；本次只在 agy hook 侧加下界，不动契约。
- **（第二轮）用记录的 `deferredEventIds` 直接当 ack 下界，不做现场重算**：记录值可能已过期——peer 解除 `working` 后低 ID 事件变成未消费 outcome，此时放行高 ID ack 一样吞事件；而反过来，记录值在事件已被消费后残留又会把 ack 多压一轮。现场重算（`deferred ∪ outcomes`）两种情况都覆盖。
- **（第二轮）把扣住的高 ID 事件继续放在 `deliveredEventIds` 里**：`rearmDeliveredEvents` 会把 `deliveredEventIds` 从 `presentedEventIds` 里摘掉，宿主忽略 `decision: continue` 时这些**已经交付过**的事件会被二次注入；因此单列 `pendingAckEventIds`。
- **（第三轮）peek 写盘前只重读 `presentedEventIds` 而不比较 epoch**：重读只能保证不抹掉磁盘已有值，不能阻止旧 waiter 去 `rearmDeliveredEvents` / 触发 `triggerWakePrompt`，也不能修正 `deferredEventIds` / `retainedCandidateEvents` 被旧集合覆盖；而 epoch 不匹配时根本不该写盘。
- **（第三轮）把未消费事件只记 id、不记完整 `AgentEventRecord`**：注入需要的文本、`paneId` / `terminalId` / `paneGeneration` 全在事件记录里（同 terminal 去重与 fallback 抑制都依赖 `paneGeneration`），只存 id 就无法在 daemon 不再重列时重建 outcome。
- **（第三轮）在 `flushDeliveredAcks` 里也重算并回写快照**：它不投影 outcome、也不改 presented 集合（只改 ack 进度），下一次 `takeNextWakeBatch` 本来就要根据刷新后的 `ackedEventId` 重算并淘汰已 ack 项；多写一次只增加写盘与丢字段的面积。

## 来源

- 双审结论（verifier + reviewer）：同 terminal 多条完成事件同批重复注入（Should Fix 1）、草稿解析在长草稿与草稿含横线时退化（Should Fix 2）、缺 `.agents/notes/` 记录（Should Fix 3）、草稿推迟轮询与日志频率取舍（Consider 4）。
- 第二轮双审结论（verifier + reviewer）：高 ID ack 顺带吞掉低 ID 未决 deferred 事件（Must Fix 1）、`readComposerDraft` 异步等待后未复核 `turnEpoch` / `waiterPid`（Must Fix 2）、`parseAgyComposerDraft` 在草稿含「横线 + `>` 行」时框顶误定位（Should Fix 3）。
- 第三轮双审结论（verifier + reviewer）：`peekNextWakeBatch` 用调用前的旧 `presented` 集合覆盖写盘，抹掉并发 `PreInvocation` 写的 `presentedEventIds`（Must Fix 1）、hook 依赖的 `connState.events` 受 daemon `REDELIVERY_FRESHNESS_MS`（300s）重投窗口限制，长草稿 / 长时 working-defer 会丢事件或被高 ID ack 吞掉（Must Fix 2）。
- 代码事实：`src/agy/hook.ts` 的 `projectAgyOutcomes`、`peekNextWakeBatch`、`takeNextWakeBatch`、`WakeWaiter` 分支、`defaultReadPaneText`、`readComposerDraft`、`parseAgyComposerDraft`、`flushDeliveredAcks`、`resolveMinHoldbackEventId`、`collectWakeCandidateEvents`、`retainWakeCandidates`、`isWaiterSuperseded`、`rearmDeliveredEvents`；`src/db/agent-events.ts` 的 `markAcked` / `listAfter` / `reservePending` / `REDELIVERY_FRESHNESS_MS`；`src/observability/agent-orchestrator-service.ts` 的 `#ack` / `#nextDeliverableAcrossScopes`；`src/daemon/agent-event-reconciler.ts` 对同一 300s 常量的 E6 判定。
- 回归证据：`test/unit/agy-hook-and-cli.test.ts`
  - `intermediate agent.done while peer still working is deferred (not acked) and a later real completion wakes once`（修复前失败于「注入里同时出现中途轮文本 `已改完代码，继续跑测试。`」，修复后只注入 `event: 1126` 一条，旧中途事件被 suppressed 并在该轮 `Stop` 随批次 ack）。
  - `parseAgyComposerDraft covers empty, single-line, multi-line, placeholder-like, and missing box`（修复前对含 `--------` / `────────` 的多行草稿返回 `第三段` 或 `undefined`，修复后返回完整草稿）。
  - `WakeWaiter wakes through retryWakePending alone when one deferred peer settles while another keeps working`（peer A `working → done` 触发 deferred 重试、peer B 持续 `working` 且全程无 push 时仍完成一次叫醒）。
  - `a low-id working-deferred event is never swallowed by the ack of a higher-id suppressed event`（mock 复刻 `markAcked` 的 `id <= ?` 批量语义：event 100 defer 期间 `ack` 绝不以 105 调用；peer A 转 `done` 后 100 正常注入；两个事件都 presented 后 100、105 才被一并 ack）。修复前第一步即 `expect(ackedIds).toEqual([])` 失败于 `[105]`。
  - `WakeWaiter cancels the wake when turnEpoch advances while readComposerDraft is in flight`（受控 Promise 卡住 `readPaneText`，pending 期间把 `turnEpoch` 改成 2：`triggerWakePrompt` 不被调用，`presentedEventIds` 不被 `rearmDeliveredEvents` 清空）。修复前 `wakePrompts` 收到一次 `[herdsman-wake]`。
  - `parseAgyComposerDraft keeps a draft that contains a rule followed by a \> quote line`（`["output", rule, "> 草稿", "--------", ">", rule, ...]` 与同宽 `rule` 变体都返回非空草稿）。修复前两者都返回 `""`。
  - `peekNextWakeBatch does not wipe presentedEventIds written by a turn that started during agent.list`（受控 Promise 卡住 waiter 的第二次 `agent.list`，pending 期间把磁盘 state 改成 `turnEpoch: 2, presentedEventIds: [300], deliveredEventIds: [300]`：放行后旧 waiter 不发 wake，磁盘上的 `presentedEventIds` 仍为 `[300]`）。修复前该断言失败于 `presentedEventIds: []`。
  - `an unconsumed event survives the daemon 300s redelivery window: long composer draft and long working-defer`（mock 复刻 `listAfter`：首次返回 `events: [9001]` 后不再重列该 `delivered` 行。阶段 1：草稿持续期间 daemon 返 `events: []`，草稿清空后仍靠 `retainedCandidateEvents` 叫醒，后续 `PreInvocation` 仍注入 9001；修复前该断言失败于 `draftWakePrompts` 为空。阶段 2：defer 的 event 100 不再被重列、只剩高 ID 噪声 event 105 时，`ackedIds` 仍为空（105 被 100 拦住），peer 转 `done` 后 100 正常注入并一并 ack 100、105；修复前第二条断言失败于 `expected [105] to deeply equal []`）。
- 前序决策：`.agents/notes/20261024-agy-smart-wake-standby-cross-workspace.md`（中间回合抑制与常驻 `WakeWaiter` 的原始设计，本笔记把其中「working 命中即永久静默 ack」一条改为 defer）。

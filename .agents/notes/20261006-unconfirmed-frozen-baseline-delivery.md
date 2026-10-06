---
status: active # active | superseded
superseded_by: ""
supersedes: ""
# 模块可选值: observability, daemon, cli, config, db, herdr, shared, herdsman-pi, herdsman-herdr-plugin, release
模块: observability
---

# unconfirmed 轮「冻结基线」丢更新：A（盘上可投递放行）+ C（关页孤儿挽救）

## 一句话结论

子代理正常完成后更新永久丢失的链条是「扩展 `confirmed=false` → daemon 把 `confirmed` 当未推进臂的硬合取项 → 计划基线创建时已含本轮答案 → `no_advance_from_input` 抹空 + invalidate → 替代行只等 15min tick → 其间编排者关页 → `#drainPlanRow` 解析不到 agent → `markCancelled` → 替代行永不产生」。本批在工作分支 `fix/unconfirmed-frozen-baseline-delivery`（base `f35b022`）上做两项修复，并按双审（reviewer + oracle）的 7 条必修返工：

- **A** 把未推进臂的放行判据从 `confirmed` 改成「盘上可投递」（`isTerminalAssistant && hasNonEmptyAssistantMessage && !staleBaselineDuplicate`），并追加唯一结构约束「该 agent 从未投递过终态行」。返工订正：该结构约束**只对 unconfirmed 轮生效**（`confirmed` 轮直接放行）——否则「confirmed + 有既有投递终态行 + 未推进」会被抹空，且因 confirmed 走 `degradeOrRelease` 的 soften 路径（返回不带 `degraded`）→ 不 invalidate、不 retry、`markCompleted` → 发出**空 `agent.done`**，正是 `20261001-pi-confirmed-turn-frozen-baseline-empty-body.md` 已修掉的静默空正文。
- **C** plan 对应 agent 已消失时不再盲目 `markCancelled`，有合法终态正文就走 H1 既有的孤儿终态事件通道（`agent_id = null`，payload 保留 `paneId`/`agentId`/`herdrSessionName`）把内容发出。返工订正（覆盖 drain 时与**执行中关页**两类入口）：① **发射前先判定可投递性**——scope 能从 pane 自己的事件史 stitch 出来才 append + 推送 + `markCompleted`；拼不出就**不 append、不写幂等键、不 markCompleted**，退回取消路径 + warn；② 挽救写成功后走与正常终态一致的 `#onAgentEvent` 推送（否则编排者已关页空闲时没有推送就没有 `orchestrator.get`，行停在 `pending`）；③ 正文来源①（plan 基线快照）与来源②（pane 记录的尾巴）**同口径**过「已投递窗口比对」——关页时基线经常就是上一轮已投递正文，不过比对会再送一条假 `agent.failed`。

**无 schema 变更、无迁移、`packages/**` 零改动、未 commit/push/merge。本批改的是 daemon：未提交/未发布不影响正在跑的进程，生效条件是完整发布后 `systemctl restart herdsman.service`（见 ⑧）。**

## 现象与实证（本轮链条）

- 现场形态（planner 只读调查已查实、本批复核一致）：
  - `status=invalidated, invalidated_reason=degraded_retry, delivery_attempts=0`（正文在投递前被抹）；
  - `payload degraded=1 / degradedReason=no_advance_from_input`；
  - `compact_history.lastAssistantMessage=null`。
- 机理（每一环都有既有笔记钉住，本批只补 A/C 两个断点）：
  1. 扩展上报 turn 信号但 `confirmed=false`（0.13.5 起扩展是**文本级**确认，见 ⑤，因此对改写/脱敏系统性 never-match）；
  2. daemon 未推进臂（`!isRetry && !historyHasAdvanced(advanced, input.compactHistory, { requireAssistantChange: true })`）把 `confirmedTerminal` 当硬合取项；
  3. 计划行 `compact_history_json` 创建时写一次、之后无 UPDATE，而创建时刻晚于最终 assistant 落盘（实测 131ms/308ms，见 `20261001-pi-confirmed-turn-frozen-baseline-empty-body.md`）⇒ 基线天然已含本轮答案；
  4. `historyHasAdvanced=false`、`staleBaselineDuplicate=false`（该 agent 无已投递终态行）、`confirmedDeliverable=false` ⇒ `degradeOrRelease("no_advance_from_input")` ⇒ `#runPlanRow` 见 `degraded: true` → `invalidateById(event.id, "degraded_retry")`；
  5. `degraded` 是合成原因、`retryRingAuthorized` 为假 ⇒ 不武装 10s 重试环，只等 15min reconcile tick（`RECONCILE_INTERVAL_MS`，`src/daemon/service.ts:42`；决策见 `20260930-phase1-delivery-latency.md` 观察项 ③）；
  6. 编排者在 degrade 与下一个 tick 之间关页（本会话纪律「完成即关」）⇒ `agents` 行被物理删除 ⇒ 下一 tick `#drainPlanRow` 解析不到 agent ⇒ `markCancelled` + `Herdsman cancelling status event plan for missing agent` ⇒ **替代行永不产生**。
- 同族第二种形态：更新到达编排者时 `last assistant` 为空（同一根因的不同表现），也由 A 覆盖。

## A：未推进臂引入「盘上可投递」判据（`src/observability/agent-index-service.ts`）

### 改前 / 改后

改前（`agent-index-service.ts` 改前 `:2074-2096` 一带）：

```ts
const staleBaselineDuplicate = (/* 200 窗口查询 */).some(...);
// A confirmed turn carrying a non-empty terminal assistant message is trusted as-is …
const confirmedDeliverable =
  confirmedTerminal &&
  isTerminalAssistant(advanced) &&
  hasNonEmptyAssistantMessage(advanced) &&
  !staleBaselineDuplicate;
if (!isRetry && !historyHasAdvanced(advanced, input.compactHistory, { requireAssistantChange: true })) {
  if (confirmedDeliverable) {
    compactHistory = advanced;
    payloadExtra = { staleSnapshot: false };
  } else {
    compactHistory = { ...advanced, lastAssistantMessage: null };
    payloadExtra = degradeOrRelease(
      staleBaselineDuplicate ? "stale_baseline_duplicate" : "no_advance_from_input",
    );
  }
}
```

改后（本批 `:2234-2440` 一带，双审返工后；`#waitForHistoryAdvance` 定义在 `:1521`，`turn?.received` 分支起于 `:2209`，`degradeOrRelease` 在 `:2251`）：

```ts
const deliveredTerminalHistories = /* 同一条 200 窗口查询， providers rows */;
const staleBaselineDuplicate = deliveredTerminalHistories.some((row) =>
  sameTerminalAssistantContent(advanced, parseCompactHistoryJson(row.compact_history_json), "pi"),
);
const hasDeliveredTerminalRow = deliveredTerminalHistories.length > 0;  // 存在性判据，不受 200 限制（见订正）
const diskDeliverable =
  isTerminalAssistant(advanced) && hasNonEmptyAssistantMessage(advanced) && !staleBaselineDuplicate;
if (!isRetry && !historyHasAdvanced(advanced, input.compactHistory, { requireAssistantChange: true })) {
  if (diskDeliverable && (!hasDeliveredTerminalRow || confirmedTerminal)) {   // 返工：confirmed 轮无视结构约束
    compactHistory = advanced;
    payloadExtra = { staleSnapshot: false };
    console.warn("Herdsman released a pi status event from a frozen baseline that already holds the on-disk answer", { … confirmed: confirmedTerminal … });
  } else {
    compactHistory = { ...advanced, lastAssistantMessage: null };
    payloadExtra = degradeOrRelease(
      staleBaselineDuplicate ? "stale_baseline_duplicate" : "no_advance_from_input",
    );
  }
}
```

### 为什么这样切

1. **不再把 `confirmed` 当硬合取项**：`turn?.received` 已经保证 turn-end 信号到达，`expectedText` 只是扩展对末尾的猜测（sanitize 之后 + 第三方改写/脱敏，见 ⑤），系统性不可靠；判据只回答「盘上是否载有可交付正文」。这与 M2 mismatch 臂的 `diskDeliverable`（`agent-index-service.ts` 改前 `:2102-2105`）是同一个变量、同一个口径——本批把它**提升到两条臂共用**（`:2376`），没有第二套并行判据。
2. **命中后按正常终态事件放行**：`compactHistory = advanced`（正文不被抹空）、`payloadExtra = { staleSnapshot: false }`、**不置 `degraded`**（置了会让 `#runPlanRow` 对已写内容 `invalidateById(..., "degraded_retry")`）、**不调 `invalidateById`**，计划走 `markCompleted`。另加一条 warn 让这条放行可观测（载荷只带 `confirmed: confirmedTerminal` 身份，**不带 `degradedReason`**，避免污染既有按 `degradedReason` 分组的 canary 口径）。
3. **`staleBaselineDuplicate` 必须保留、且不缩小窗口**：0.13.6 已把它从「单条最新终态行」扩到「该 agent 最近 ≤200 条 delivered/acked 终态行」（`STALE_DUPLICATE_GUARD_SCAN_LIMIT = 200`，`:126`）。本批原样沿用该查询与窗口深度（连 SQL 都没改，只把 `.some(...)` 的结果绑到变量上），并把「窗口 >200 行的漏拦」这一既定代价原样继承。
4. **为什么必须加「该 agent 从未投递过终态行」（双审返工后：该结构约束只对 unconfirmed 轮生效）**：这条臂**无法**区分「冻结基线里就是本轮答案」与「本轮什么都没写、盘上末尾是上一轮已经交付过的答案」。前者该放行，后者一放行就是把上一轮结果冒充本轮结果——正是 `20261001-pi-confirmed-turn-frozen-baseline-empty-body.md` 判据三要防的事。内容级守卫（`staleBaselineDuplicate`）只是**尽力而为**：只有界 200 行、只比 delivered/acked、只比 ref/text，漏判不可证明为零。因此只有结构判据是可靠的：**该 agent 若从未投递过终态行（它的第一个终态轮），就不存在「更早的答案」可被冒充**，盘上正文只能是本轮答案；一旦有既有历史投递，"历史未推进" 就是歧义状态，此时不值得为它冒险重投。实现方式取查库而非内存计数：守卫查询本来就在这条臂里跑，`rows.length > 0` 即答案——一次查询、一个窗口、零额外成本，也无进程重启即失忆的问题；内存计数还要处理 daemon 重启后的口径断层。
   - **返工订正（结构约束只对 unconfirmed 轮生效）**：初版把 `!hasDeliveredTerminalRow` 写成无条件合取项，confirmed 轮也被拦。confirmed 轮的盘上正文已被客户端**文本级**验证过就是本轮最终消息，「该 agent 有既有投递」不再造成歧义；而且 confirmed 轮一旦落入 else 分支走的是 `degradeOrRelease` 的 **soften** 路径——返回**不带 `degraded`** 的载荷 → 不 invalidate、不 retry、直接 `markCompleted` → 发出**空 `agent.done`**（不是「降级」，是静默空正文，`20261001-pi-confirmed-turn-frozen-baseline-empty-body.md` 修掉的同一症状）。故放行条件改为 `diskDeliverable && (!hasDeliveredTerminalRow || confirmedTerminal)`：unconfirmed 轮保持原结构约束（安全性不弱于旧码），confirmed 轮放行。`degradeOrRelease` 的 soften 语义本身**未动**（confirmed 轮磁盘上确实没有正文时仍走 soften 空放 + warn）。
   - **订正（窗口表述）**：`hasDeliveredTerminalRow = rows.length > 0` 是**存在性判据**——窗口内**任意**已投递行都使长度 > 0，**不继承「超过 200 行就看不见」**：一个有 300 条已投递终态行的 agent 仍读作 `> 0`。`limit 200` 只影响**内容重复判据** `staleBaselineDuplicate`（某份旧正文是否被认成重复）。
   - 附带效果与边界：一个「上一轮写了答案但那轮从未投递（行被 cancelled/invalidated）」的 agent 仍会被放行——这是**可接受**的：那份正文编排者从未见过，投出去远好过丢掉（与 H1「结果永远不会到达才是故障」同向）。
5. **15 分钟周期与重试环不动**：`retryRingAuthorized` 及其四个重排点、`#waitForHistoryAdvance` 的 200ms×8 / 1500ms 预算全部原样（`20260930-phase1-delivery-latency.md` 的安全底线）。

## C：关页不得盲目 `markCancelled`，有内容就挽救（drain 时 + 执行中关页，`agent-index-service.ts`）

### 改前 / 改后

改前（`#drainPlanRow` 改前 `:811-821`、`drainPendingPlans` 改前 `:531-547`、`#runPlanRow` 的 `PLAN_CANCELLED` / `undefined && isPaneClosed` / catch-`isPaneClosed` 三处，共五处重复同一段）：

```ts
if (!agent) {
  store.markCancelled(current.id);
  console.warn("Herdsman cancelling status event plan for missing agent", { … });
  return;
}
```

改后：五处全部调用同一个 `#salvageOrCancelMissingAgentPlan(store, row)`（定义 `:996`；调用点 `:545`（drain 入队即失败的兜底）、`:1085`（`#drainPlanRow` 缺失 agent）、`:1317`（`#runPlanRow` 的 `PLAN_CANCELLED`，即执行中关页）、`:1333`（`#runPlanRow` 的 `undefined && isPaneClosed`）、`:1463`（`#runPlanRow` catch 的 `isPaneClosed`））：

```ts
#salvageOrCancelMissingAgentPlan(store, row): AgentEventRecord | null {
  try {
    salvaged = this.#salvageOrphanTerminalEvent(row);
    if (salvaged) { store.markCompleted(row.id); }        // 挽救成功：完成（不再是取消）
  } catch (error) { console.warn("Herdsman failed to salvage a status event plan for a missing agent", { … }); }
  if (salvaged) return salvaged;
  try { store.markCancelled(row.id); } catch { /* row may be gone */ }
  console.warn("Herdsman cancelling status event plan for missing agent", { … });   // 无内容（或不可投递）才取消
  return null;
}
```

`#salvageOrphanTerminalEvent(row)`（`:894`）先取正文、判定可投递性、再发射：

- 正文来源（`#salvageableTerminalBody`，`:810`），按序：
  1. **plan 自己的基线快照**（`compact_history_json`）——冻结基线形态下它**就是**本轮答案（会话文件在计划创建时刻的尾巴），也正是被 `no_advance_from_input` 抹掉、替代行来不及投的那份；
  2. 若基线不可用（或与已投递正文同文，见下）：**该 pane 自己记录下来的、从未投递过的末尾**（`agent_events` 里同 `(herdr_session_name, pane_id, pane_generation)`、`created_at <= plan.createdAt` 的最近 20 行里，取 `delivery_attempts = 0` 且终态非空、且不等于已投递窗口内任一正文的第一条）。**这里无法重读会话文件**：history discovery 需要 `agent_session`，而它只存在于已被物理删除的 `agents` 行上，herdsman 也不猜会话路径（`src/agent-history/discovery.ts`），所以 pane 自己的事件史是那份尾巴的唯一剩余记录。
- **返工订正（来源①也过「已投递窗口比对」）**：两个来源共用同一窗口（同 pane、`created_at <= plan.createdAt` 的 delivered/acked 终态行，`STALE_DUPLICATE_GUARD_SCAN_LIMIT` 深度）与同一条 `alreadyDelivered(body)` 判据。关页时**基线经常就是上一轮已投递正文**（pane 在完成轮后关闭），来源①若见到终态非空就返回，会再送一条 `agent.failed`（假失败 + 旧正文；`fallbackOutcome` 为假，同 pane 已有 completed 也不会压掉它）。比较种类从写死 `"pi"` 改为按最新一条已投递行 `payload_json` 的 `agent` 字段推导（`agentKindFromPayloadJson`，`:3124`）：agy/antigravity 尾巴只比 ref，写死 `"pi"` 会在「同 ref 不同文」时误判为重复而漏掉真挽救。
- **发射前先判定可投递性**（`#orphanScopeDeliverable`，`:978`）：`#appendPlanFailedEvent` 会从 pane 事件史 stitch 孤儿 scope（`created_at <= plan.createdAt`），但**本计划执行期写的事件晚于计划创建**，拼不进来；pane 上若没有更早事件 ⇒ 占位 `terminalId=null` / `workspaceId=""` ⇒ `publishAgentEvent` 死信、`isDeliverableAgentEvent` 拒；而挽救仍 `markCompleted`，且幂等键 `agent.failed:plan:<id>` 会让下次 append 原样返回这条死信——比原来的 cancel 更死。故 scope 拼不出（`terminal_id = null` 或 `workspace_id = ""`）时**不 append、不 markCompleted、不写幂等键**，退回取消路径并打 warn `Herdsman left a status event plan cancellable because the pane scope could not be stitched`。
- **挽救成功后必须推送**：正常终态路径会 `#onAgentEvent`（`:1073`/`:1087`， caller-forwarded）；挽救是提前返回、无人转发返回值，故 `#salvageOrCancelMissingAgentPlan` 把 event 交回调用点，由 `#publishSalvagedEvent`（`:1044`，与 `#retryWaitingPlanRow` failed 分支同一 `#onAgentEvent` 通道）推送。唤醒是 `agent.event` 推送驱动的（`packages/herdsman-pi/src/index.ts` 的 `handleAgentEvent`；keepalive 只 ping）：编排者已关页且空闲时，没有推送就不会 `orchestrator.get`，行会停在 `pending`。
- 发射通道**严格复用 H1**：`#appendPlanFailedEvent`（`:2829`）→ `#resolvePlanOutcomeAgent`（`:2809`，`agents` 行缺失时按 `(herdr_session_name, pane_id, pane_generation)` + `created_at <= planCreatedAt` 从 pane 自己的事件史 stitch 出 pane/terminal/workspace）→ `agentId: resolved.agentRowPresent ? agent.id : null` → payload 保留原始 `agentId` / `paneId` / `herdrSessionName` / `name` / `terminalId` / `workspaceId`，`reason = ORPHAN_SALVAGE_REASON = "pane_closed_before_delivery"`（`:160`），正文随 `compact_history_json` 一起落库。幂等键沿用 H1 形状 `agent.failed:plan:<planId>`，重复 drain 不会双写。
- **为什么是 `agent.failed` 而不是孤儿 `agent.done`**：投递闸门对孤儿行只放行三种形状——保留态 invalidated（`status='invalidated' and deliverable=1`）、`agent.failed`、`agent.discarded`（`src/db/agent-events.ts` 的 `nextDeliverableAfter` 与 `isDeliverableAgentEvent` 第 5 步）。写成孤儿 `agent.done` 会落库但**永远投不出去**（`isDeliverableAgentEvent` 第 6 步 `agent === undefined → false`），等于把「永不产生替代行」换成「产生了但投不出」。`agent.failed` 是 H1 已验证可唤醒编排者的形状，`fallbackOutcome` 走默认计算（本 reason 不是 `degraded`，故为 false）。
- **日志**：挽救时打 `Herdsman salvaged an orphan terminal event for a plan whose agent row is gone`（带 `planId`/`eventId`/`reason`），**不打** `cancelling status event plan for missing agent`；确实无内容（或 scope 拼不出）时维持原 `markCancelled` + 原 warn 不变（不可投递时另打一条说明原因的 warn）。挽救写库失败时 catch 住并退回原取消路径，保证 `drainPendingPlans` 永不 reject（H1 决策 7 的既有契约）。

## 测试（既有文件内新增/修改，共 9 条用例；无新文件）

- `test/integration/turn-completion-signal.test.ts`
  1. `:2367` `A: an unconfirmed turn with a frozen baseline holding the answer delivers the disk body without degrade`（正向）——unconfirmed + mismatch + 冻结基线即本轮答案 + 该 agent 无已投递终态行 ⇒ 事件 `type='agent.done'`、`compactHistory.lastAssistantMessage.text === "final answer"`、`payload.staleSnapshot === false` 且无 `degraded`、`deliverable === 1`、DB 无任何 `status='invalidated'` 行、warn 命中新放行签名且 `confirmed === false`。**改前红**（旧码降级 → invalidate → `executeStatusEventPlan` 返回 undefined）。
  2. `:2576` `A: a confirmed frozen-baseline round over a delivered tail still releases this round's answer`（返工新增，**防空 `agent.done` 回归的关键用例**）——round 1 投递并 `reservePending`（上一轮**真的被消费**，行进入 delivered）后，round 2 **confirmed** 且 `textMatches=false`、其冻结基线即本轮新答案 ⇒ 事件 `type='agent.done'`、`compactHistory.lastAssistantMessage.text === "round-2 answer"`（**不得空**）、`payload.staleSnapshot === false` 且无 `degraded`、无 invalidated 行、放行 warn 命中且 `confirmed === true`。**改前红**：结构约束无条件生效时 confirmed 轮被抹空 → soften 空放 → `expected undefined to be 'round-2 answer'`（空 `agent.done`，无 degrade、无 invalidate、直接 completed）。既有 W17 之所以仍绿，是因为它没有 `reservePending`，「有既有投递」这条臂根本没被触发。
  3. `:2431` `A guard: an unconfirmed frozen baseline that repeats the last delivered body still degrades`（负向一，陈旧防守）——round 1 投递并 `reservePending`（模拟编排者消费）后，round 2 unconfirmed、盘上与基线都等于已投递正文 ⇒ 必须抹空并降级 `stale_baseline_duplicate`（落 invalidated），且**不得**出现放行签名。**改前后皆过**（护栏性质；返工后 confirmed 的放行不波及 unconfirmed 护栏）。
  4. `:2516` `A guard: an unconfirmed frozen baseline over a non-terminal tail still degrades`（负向二，非终态防守）——基线与每次重读都是工具轮（无 `stopReason`）⇒ 不放行半截文本，走 `no_advance_from_input` 降级 + invalidate。**改前后皆过**（护栏性质）。
- `test/integration/agent-index-service.test.ts`
  5. `:2697` `retains and delivers orphan terminal event when pane is closed while plan is pending`（返工修改）——drain 路径挽救：先给 pane 留一条自己的事件（stitch 需要 pane/terminal/workspace），插入含终态正文的 pending 计划，`delete from agents` 模拟关页，触发 `drainPendingPlans()` ⇒ 计划行 `status === 'completed'`（非 cancelled）、产生 `agent_id = null` 且 `deliverable: 1 / status: 'pending'` 的 `agent.failed`、`compactHistory.lastAssistantMessage.text === "final answer"`、payload 保留 `agentId`/`paneId`/`herdrSessionName`/`reason='pane_closed_before_delivery'`、无 `agent.done` 行、日志是挽救签名而非 `cancelling … for missing agent`；**返工新增断言**：`onAgentEvent` 捕获到恰好 1 次推送（内容 = 该孤儿行）——挽救必须推送，不只是落库。**改前红**：`expected 'cancelled' to be 'completed'`（挽救整体）；推送断言单独看为 `expected [] to have a length of 1`（返工第 3 条）。
  6. `:2805` `cancels and appends nothing when a pending plan's baseline repeats an already-delivered body`（返工新增，来源①同口径）——pane 先有一条 delivered/acked 终态行（已投递正文，兼作 stitch 源），pending 计划的基线就是同一正文；关页 + drain ⇒ 计划 `status === 'cancelled'`、无 `agent.failed` 行、warn 是取消签名且挽救签名 0 次。**改前红**：`expected 'completed' to be 'cancelled'`（来源①无条件返回基线 → 假失败 + 旧正文）。
  7. `:2880` `cancels and appends nothing when the pane scope cannot be stitched for an orphan salvage`（返工新增，可投递性前置）——pane **没有任何事件史**（无 stitch 源），pending 计划基线含终态正文；关页 + drain ⇒ 计划 `status === 'cancelled'`、**无 `agent.failed` 行**（不写死信、不写 `agent.failed:plan:<id>` 幂等键）、warn 含 `left a status event plan cancellable because the pane scope could not be stitched`。**改前红**：`expected 'completed' to be 'cancelled'`（挽救不看可投递性 → 落一行死信 + 完成）。
  8. `:3009` `a terminal plan whose agent row is gone mid-execution still cancels when nothing is salvageable`（返工**改名改预期**，原 `appending a terminal plan whose agent row is gone cancels the plan and appends nothing`）——`handleHerdrEventFast` 的 plan（无终态正文）+ 执行前删 agent ⇒ `executeStatusEventPlan` resolve undefined、计划 `status === 'cancelled'`、无 `agent.done`/`agent.failed` 行：走的是同一条挽救入口，但确实无内容 ⇒ 仍取消。
  9. `:3050` `salvages deliverable terminal content when the agent row is gone mid-execution`（返工新增，执行中关页进挽救）——先给 pane 留 stitch 事件，删 agent，再 `executeStatusEventPlan` 一个含终态正文的 plan（`#appendStatusEvents` 内 `findByPane` 失败 → `PLAN_CANCELLED`）⇒ 计划 `status === 'completed'`、孤儿 `agent.failed`（`agent_id = null`、`deliverable: 1`、`status: 'pending'`、正文 `final answer`、payload `reason='pane_closed_before_delivery'`）、无 `agent.done`、`onAgentEvent` 恰好推送 1 次。**改前红**：`expected 'cancelled' to be 'completed'`。
- 确定性：全部走既有注入手段（`staleGuardIndex` / `runConfirmedPlanRound` / 直接 `insertPending` + `drainPendingPlans()` / `executeStatusEventPlan`），不依赖真实时钟与真实 15min tick。
- **每条新分支 ≤1 条用例**，全部落在既有文件内；未为纯文案/注释改动加用例。

## ③ 为什么不先做 B（degrade 后立即重排）

- B 在盘上**确实没有推进**时（本链条正是如此：答案早已落盘，`historyHasAdvanced` 恒为假）会让同一个计划行连续撞同一面墙，`attempts` 一路累加到 `STATUS_PLAN_MAX_ATTEMPTS = 8` → `markRetry` 直接给 `failed`（合成 `degraded` 不是 `PLAN_WAITING_HISTORY`，落 `failed` 而非 `discarded`），把「慢但最终可能成功」换成「快速且确定的失败」。
- 同时它会在盘上未推进的窗口里制造重试风暴：每个未推进轮次都以短退避重跑 `#waitForHistoryAdvance`（200ms×8 / 1500ms 预算）与 pane 刷新，而这期间内容一个都没变。
- 更要紧的是：**A 落地后 B 的问题域基本消失**——冻结基线轮次不再降级，就没有「替代行」需要重排；真正需要短收敛路径的是别的形态（Phase 1 观察项 ③ 登记的「degraded 行失去 10s 环后收敛变慢」，最坏 ≈2h 落 `agent.failed`），那仍按原登记留 Phase 2。本轮**不动** 15min 周期与 `retryRingAuthorized`（有意安全底线）。

## ④ 为什么不动 D（pi 扩展）

- D 改的是 `expectedText`/确认机制（`packages/herdsman-pi/**`），本任务明确零改动；且扩展侧 0.13.5 已完成**文本级**确认（见 ⑤），剩余 never-match 的根因在**第三方改写与脱敏**，不在扩展的判定逻辑里——就算把确认改得更宽松，也无法覆盖非 pi 代理（`agy`）走的同族丢失路径。
- 故障链涉及 **非 pi 代理 `agy`**：`agy` 分支根本不进 pi 的 turn-signal 门（`input.agent.agent === "pi"` 才进），所以只修 pi 扩展对 agy 一侧的同类丢失毫无帮助。A/C 都在 daemon 侧、与 agent 种类无关（C 对任何 pane 生效；A 在 pi 分支内，但 agy 侧另有 `isReadyNonPi` / `latestCompletedTurnRef` 的既有判据，不在本批范围）。
- 一旦 A 生效，`confirmed` 不再是放行前提，D 的边际收益进一步下降（M2 笔记已把「M1 扩展侧退回尺寸兜底」判为**前提消失**而放弃，同理适用于 D）。

## ⑤ 扩展侧确认机制的事实核实（只读核对结论）

**结论：当前实现是「文本级」确认，不是尺寸级。**「尺寸级」是 0.13.5 **之前**的旧实现（`size > initialSize` + `reason: "new_content"`），0.13.5 已删除；两份报告里「尺寸级」那一条描述的是历史状态。

`packages/herdsman-pi/src/turn-signal.ts`（当前 `main` = `f35b022`，本批未改）：

- `:84` `export async function confirmSessionWrite(input)`：轮询文件 tail（`TURN_SIGNAL_TAIL_CHARS = 8000`、`TURN_SIGNAL_POLL_MS = 50`、`TURN_SIGNAL_TIMEOUT_MS = 3000`），**包含 `expectedText`（或其 JSONL 转义形态）才 `confirmed: true`**；
- `:96` `candidate = expectedText.length > 200 ? expectedText.slice(-200) : expectedText`（候选 = 末尾 200 字符）；
- `:100` `escapedCandidate = JSON.stringify(candidate).slice(1, -1)`（JSONL 转义形态，否则多行末条永远匹配不上）；
- `:101-105` `containsText = tail !== null && candidate.length > 0 && (tail.includes(candidate) || tail.includes(escapedCandidate))`；命中即 `{ confirmed: true, reason: "already_written" }`；
- `:106-107` `initialSize = probe.size(path)` 只用于判定 `unavailable`（文件读不到），**不再作为「文件变大即确认」的判据**；
- `:117` 超时才 `{ confirmed: false, reason: "timeout" }`——如实报告未确认，而不是伪造确认。

剩余 never-match 的两类来源（既有笔记已登记，本批只复核不改）：① `message_end` 之后改写型扩展继续改写落盘正文（`no-tables` 把 `### X` 改粗体、表格改 bullet 等）；② 凭据脱敏——`index.ts` 的 `assistantMessageText` 上报 `sanitizeText` 之后的文本（`Bearer …`/`token=`/`sk-…` → `[REDACTED]`），而盘上存原文。两者都让「客户端猜测」与「盘上权威」系统性不一致，这正是 A 把放行判据从 `confirmed` 解耦的原因。

## ⑥ 被放弃的方案与被否决的路线

- **「degrade 但立刻重排」（B）**：本批放弃，理由见 ③（耗尽 attempts → `failed` + 重试风暴 + 问题域已被 A 消掉）。
- **「更新计划行基线（`compact_history_json` 后续 UPDATE）」**：**已被 `20261001-pi-confirmed-turn-frozen-baseline-empty-body.md` 否决**（持久化语义变更，需要 schema 行为 + 迁移 + 并发写语义；判据一已在读取侧自洽）。本批沿用该否决，A 仍是纯读取侧修复。
- **「把 confirmed 直通标 `degraded: true`」**：**已被同一笔记否决**（`#runPlanRow` 会 `invalidateById(..., "degraded_retry")` 把刚写的内容再废一遍，重跑仍撞同一冻结基线）。
- **「给放行条件加 `endsWith(expectedText)` 约束」**：**已被同一笔记否决**（本批目标样本恒为 `textMatches=false`，加约束等于修不成）。
- **「`no_advance_from_input` 一律放行（不区分文本）」**：**已被同一笔记否决**（会把真正无文本的轮次放行成空 `agent.done`）。本批用 `diskDeliverable` 的终态/非空合取 + 两个负向用例把它钉死。
- **「给判据一整体降级/删除 `confirmedTerminal`」**：**M2 笔记已否决整体降级**（`confirmedTerminal` 仍承载 released/degraded 的日志与载荷语义）。本批保持：`confirmedTerminal` 仍用于 `degradeOrRelease` 的 warn 分支与新 warn 的 `confirmed` 字段，只是不再充当放行的硬合取项。
- **「本批给守卫查询加索引」**：**M2 笔记已否决并遗留登记**（会引入 migration）。本批连新查询都没加（守卫查询原样复用），因此不需要任何索引。
- **「把守卫 SQL 沉淀进 `AgentEventStore` 方法」**：M2 遗留 5 登记的重构项，本批继续内联在 service（同样的范围约束）。
- **「关页后照常投递改成孤儿 `agent.done`」**：放弃。投递闸门对非 failed 的孤儿终态行返回不可投递（见 C 节论证），会造出「落库但永不出户」的行；改用 H1 已验证的 `agent.failed` 形状。
- **「挽救时把正文当作本轮 completed 结果投出（`agent.done` + 保留态 invalidated 形状）」**：放弃。那需要新造一种行形状并改投递闸门，超出「严格复用 H1」的边界；已知代价是编排者侧把这批发到的内容渲染为 `failed … last assistant (pre-round): …`（`packages/herdsman-pi/src/wake.ts` 的 `formatAgentOutcomeUpdates` 无法从行上判断该正文就是本轮答案），已登记为遗留 3。
- **「M1：扩展侧退回尺寸兜底」**：**M2 笔记已判为「前提消失」而放弃**；本批重申（松绑 `confirmed` 之后更不需要易误报的尺寸确认）。

## ⑦ 遗留项与触发条件

1. **M2 遗留登记 2：部分处置（不销账）**：`20261002-m2-disk-body-and-canary-read.md` 遗留 2 原文「空正文大头未触及……其中大头 `no_advance_from_input`（38/73）未触及，留待后续评估」。本批 A 是对该格的第一次正面处置，但**只覆盖其中一类（部分处置，不销账）**。生产库只读复核（`~/.herdsman/state.db`，daemon 在跑、WAL 只读快照，本批实测）：`no_advance_from_input` 共 **364** 条（oracle 快照 366，daemon 在跑有轻微漂移），按「同 pane 是否有更早的 acked `agent.done`」分：
   - **222 条**同 pane 已有更早的 acked `agent.done`（同族证据：120 个 pane 有 ≥2 条 acked done，最多 245 条）；
   - **142 条**没有（oracle 快照 144）。
   本批的未推进臂放行**只覆盖其中一类**：142 那类（无更早 acked done ⇒ `!hasDeliveredTerminalRow` 成立 ⇒ 放行）；222 那类里 unconfirmed 轮**仍降级**（结构约束就是防它），confirmed 轮由返工第 1 条放行——但注意这 364 条降级行全部是 unconfirmed 轮写的（confirmed 轮在旧码里走 soften 空放、不留 `degradedReason`，实测这些行 payload 无一含 `confirmed` 字段），所以「confirmed 放行」的收益体现为**少一条空 `agent.done`**，不体现在这 364 的计数里。快路径（`confirmed && textMatches`）**根本不经该约束**。
   同时按 M2 遗留 3 的口径提醒：该组读数必须与**部署后自身基线**比，且 A 会让「曾靠 confirmed 放行的轮次」改走新 warn 签名，`no_advance_from_input` 组预期下移、新签名组从 0 起量。
2. **A 的收窄/放行效应需 canary 观察**：两类轮次的 journal 都需要观察——(a) unconfirmed + 有既有已投递终态行 + 未推进：仍降级（本批安全底线，生产量级未测）；(b) confirmed + 有既有已投递终态行 + 未推进：由「抹空 → 空 `agent.done`」改为「放行 + warn」（返工第 1 条，生产量级未测）。观察量：journal `Herdsman released a pi status event from a frozen baseline that already holds the on-disk answer` 与 `Herdsman emitted degraded status event { degradedReason: 'no_advance_from_input' }` 两组同窗对比，以及 `Herdsman released a confirmed pi status event with no deliverable text`（空放 warn，返工后应随 confirmed 抹空消失而降为 0）。
3. **C 的 failed 形状是有损语义**：挽救行是 `agent.failed`，编排者看到的是「failed + reason + (pre-round) 正文」而不是「completed + 正文」。**触发条件**：若运维反馈「子代理明明完成，编排者却看到 failed」，就把该形状升级为「终态类型 + 保留态 invalidated」并同步改投递闸门（需要单独一轮 + 测试，本批不做）。
4. **`#runPlanRow` 执行中关页：本批已纳入同一挽救（返工第 6 条，原登记项关闭）**：`PLAN_CANCELLED`（append 时 agent 行已消失）、`undefined && isPaneClosed`、catch 的 `isPaneClosed` 三处原为 `markCancelled`，本批全部改走 `#salvageOrCancelMissingAgentPlan`；原钉死旧行为的用例 `appending a terminal plan whose agent row is gone cancels the plan and appends nothing` 已改为 `:3009`「无内容仍取消」+ `:3050`「有内容挽救」两条。**剩余边界**：`executeStatusEventPlan` 无 store 的直通路径（`planId: undefined`）没有计划行可挽救，只能取消——该路径只在 store 未注入时出现。
5. **会话文件尾巴在 `agents` 行被删后不可达**：C 的第二来源只能取 pane 自己记录的事件史。**触发条件**：若要把 C 做成真正的「重读会话文件」，需要先解决「agent 行删除后如何找回 `agent_session`」（例如 tombstone 表扩列），属 schema 变更，须先回报。
6. **A 的窗口成本（订正后）**：`staleBaselineDuplicate` 的**内容重复判据**受 `STALE_DUPLICATE_GUARD_SCAN_LIMIT = 200` 限制——比窗口（最新 200 条 delivered/acked 终态行）更旧的正文可能不被认成重复；`hasDeliveredTerminalRow` 的**存在性判据不受 200 限制**（窗口内有任意已投递行即 `> 0`）。**触发条件**：实测出现「旧正文被当新答案重投」（即 `>200` 行的旧投递绕过内容判据）的实证，再评估加索引/扩窗（须先回报，涉及 migration）。
7. **挽救的范围边界（返工第 4 条派生）**：scope 拼不出时 plan 保持取消。**触发条件**：出现「计划 cancelled、该 pane 在 DB 里有未投递终态正文、但 pane 事件史在 `created_at <= plan.createdAt` 前为空」的实证（即 stitch 不到的挽救），再评估「把 scope 快照进 plan 行」（需要 schema 变更，须先回报）；当前用 warn 可见。
8. **`agent.failed` 挽救行的语义折扣（返工登记）**：编排者侧渲染是 `last assistant (pre-round): …`（`packages/herdsman-pi/src/wake.ts` 的 `formatAgentOutcomeUpdates`）——这是**主动告诉模型这段正文不是本轮产出**，所以 C 只是「库里还有一份」，不等于「编排者会当作完成结果用」。**跟随项是扩展侧按该 `reason` 改渲染**（把 `pane_closed_before_delivery` 呈现为本轮答案而不是 pre-round），**不是本轮新造行形状/改闸门**。
9. **`staleSnapshot` 仍无生产读取方**：沿用 0.13.6 结论，不加 payload 字段、不加扫描。
10. **来源①/②的 agent kind 推导是尽力而为**：`agents` 行已删除，kind 只能取最新一条已投递行 payload 的 `agent` 字段（取不到退回 `"pi"` 分支）。**触发条件**：出现 agy/antigravity pane 的挽救行被误判（同 ref 不同文被当重复跳过，或反之）的实证，再评估把 agent kind 落入 plan 行（需 schema 变更，须先回报）。

## ⑧ 双审返工订正：生效条件、生产库计数、语义折扣、安全边界

### 生效条件：必须重启 daemon

- 本批改的是 **daemon**（`src/**`）：未提交/未发布不影响正在跑的进程，生效条件是**完整发布后 `systemctl restart herdsman.service`**（`npm install --global` 只替换文件，重启之后才生效）——不是「不必重启」。
- `packages/**` 未改 ⇒ 扩展**不必重载**，新旧 pi pane 在扩展侧无差别。
- 重启后的 startup drain 只会处理当时**仍 pending 且 agent 已消失**的计划（本批复核：当前 pending/running 计划 **0** 条）；**已取消的（439 条）与 failed 的（174 条）不会回补**。

### 生产库只读计数（本批对 `~/.herdsman/state.db` 的 WAL 只读快照实测；daemon 在跑，数字会比 oracle 快照轻微漂移）

| 计数 | 本批实测 | oracle 快照 |
| --- | --- | --- |
| `no_advance_from_input` 事件总数 | 364 | 366 |
| ↳ 同 pane 有更早 acked `agent.done` | 222 | 222 |
| ↳ 没有 | 142 | 144 |
| 有 ≥2 条 acked done 的 pane 数（最多 acked done 数） | 120（245） | 120（245） |
| acked 终态行总数（`agent_id` 为 null 的） | 3432（3329） | 3432（3331） |
| 计划 cancelled / failed / completed / discarded | 439 / 174 / 7414 / 16 | 440 / 175 |
| 当前 pending/running 计划 | 0 | 0 |

（口径：364 条 `no_advance_from_input` 的 payload 无一含 `confirmed` 字段 ⇒ 全部是 unconfirmed 轮写的降级行；confirmed 轮的同类轮次在旧码走 soften 空放、不留 `degradedReason`。）

### `agent.failed` 挽救行的语义折扣

- 编排者侧渲染是 `last assistant (pre-round): …`（`packages/herdsman-pi/src/wake.ts` 的 `formatAgentOutcomeUpdates`）——这是**主动告诉模型这段正文不是本轮产出**。所以 C 只是「库里还有一份」，**不等于「编排者会当作完成结果用」**。
- **跟随项应是扩展侧按该 reason 改渲染**（把 `reason='pane_closed_before_delivery'` 呈现为本轮答案而不是 pre-round 旧文），**不是本轮新造行形状/改闸门**。

### `ON DELETE SET NULL` 的安全边界

- 3331/3432 条 acked 终态行的 `agent_id` 已是 null（事件行比 agent 行活得久）。本批结构约束（A 的「该 agent 从未投递过终态行」）的安全边界因此是「**当前 agent 行的寿命**」——守卫查询按 `agent_id = ?` 匹配，agent 行删除后，它此前的历史投递对 pane 上的新 agent 行不可见——**不是这个 pane 的寿命**。本库 `pane_id` 跨 agent 复用计数为 0，故还不是现网冒充源；若将来出现 pane 复用，该边界要连同此判据一起复核。

### 表述订正汇总（对应双审第 2、7 条）

- 「`hasDeliveredTerminalRow` 继承 >200 行看不见」→ **错**：存在性判据不受 200 限制，只有 `staleBaselineDuplicate` 的内容重复判据受（见 A 节第 4 条、遗留 6）。
- 「confirmed 收窄 = 降级」→ **错**：confirmed 轮落入 else 分支的结果是**空完成/静默丢失**（soften 路径不置 `degraded`、不 invalidate、不 retry、直接 completed + 空 `agent.done`），已被本轮返工修掉。

## ⑨ 双审后遗留与决定（reviewer w18:p1S / oracle w18:p1T）

> 记账 delta：本节只记录双审放行后用户当次拍板的决定，以及 reviewer / oracle 提出但**明确不在本批处置**的发现；不改动 ①-⑧ 任何已审通过的行为描述（唯一例外是开头「行号订正」里的数字本身）。§A 照写用户决定；§B/§C/§D 的遗留条目**并入 ⑦ 的清单编号**（顺延 11 起，不销 ⑦ 任何旧账），逐条带触发条件。

### 行号订正（只订正数字，未动任何表述；本节一律使用订正后值）

- `#resolvePlanOutcomeAgent` 定义：`:2806` → **`:2809`**（C 节正文、来源各一处）。
- `#appendPlanFailedEvent` 定义：`:2826` → **`:2829`**（C 节正文、来源各一处）。
- `agentKindFromPayloadJson` 定义：`:3121` → **`:3124`**（C 节正文、来源各一处；其在挽救路径的调用点为 `:848`）。
- reviewer 原文 `~:848`（delivered 窗口）精确化为 **`:829-858`**：`delivered` 查询 `:829-842`（`where` 子句 `:832-834`）、`alreadyDelivered` 判据 `:849-858`。
- reviewer 原文 `~:1015`（`markCompleted`）精确化为 **`:1006`**（`#salvageOrCancelMissingAgentPlan` 内、`#salvageOrphanTerminalEvent` 写成功之后的 `store.markCompleted(row.id)`）。
- 复核无误：`wake.ts:151`（failed 正文标 `last assistant (pre-round)`）、`wake.ts:126`（`stringValue(payload.agent) ?? … ?? paneId`）、`:126`（`STALE_DUPLICATE_GUARD_SCAN_LIMIT`）、`:1999`（`#throwIfShutdownAbort`）、`:1452-1464`（catch 谓词，见遗留 15）、`:2576` / `:2805`（用例行号）。
- 用例锚点（`test/integration/agent-index-service.test.ts`，`rg -n` 实测起始行订正，本 delta「只改数字」）：
  - `cancels and appends nothing when the pane scope cannot be stitched for an orphan salvage`：`:2876` → **`:2880`**（「测试」节条目 7、来源节「测试」共两处）。
  - `a terminal plan whose agent row is gone mid-execution still cancels when nothing is salvageable`：`:3005` → **`:3009`**（「测试」节条目 8、⑦ 遗留 4、来源节「测试」共三处）。
  - `salvages deliverable terminal content when the agent row is gone mid-execution`：`:3046` → **`:3050`**（「测试」节条目 9、⑦ 遗留 4、来源节「测试」共三处）。
  - 三条旧值均比实测少 4 行（`rg -n` 逐条复核命中 `test(` 起始行），属 ①-⑧ 已审部分的既有偏差；订正只动数字，未动任何表述语义（出处置账见遗留 20）。

### A. 用户当次决定（照写，语义未改写）

1. **`agent.failed` + `pre-round` 折扣：接受。** 本批只保证「正文进了 wake」；`wake.ts:151` 对所有 failed 正文都标 `last assistant (pre-round)`、reason 印成 `pane_closed_before_delivery`，模型有理由把它当旧上下文。扩展侧按该 reason 改渲染是**单独一批**的跟随项，本批不改闸门、不新造行形状。（即 ⑦ 遗留 3 / 8 的升级触发器本次**不触发**，维持 `agent.failed` + H1 通道现状。）
2. **来源② `limit 20`：补一条遗留（第 13 条，触发条件见该条），本轮不改。**
3. **catch 宽谓词：本轮维持现状 + 记账（第 15 条，触发条件见该条），不现在收窄。**

### B. reviewer 发现（来源标注 reviewer w18:p1S）

**遗留 11｜`#salvageableTerminalBody` 的 delivered 窗口缺 `pane_generation` 过滤**：来源② recorded 查询（`:819-828`）经 `generationClause`（`:811-812`）带了 `pane_generation` 条件，delivered 窗口查询（`:829-842`，配 `alreadyDelivered` 判据 `:849-858`）却只按 `(herdr_session_name, pane_id, created_at)`——两侧不对称。跨 generation 复用 `pane_id` 时，旧代**已投递**正文会被判 `alreadyDelivered` 而**退回取消**：**丢失方向（该救的没救），不是重复方向**。**触发条件**：本库出现 pane 跨 agent 复用（当前只读计数为 0，见 ⑧）。**不改放行判据本身**；处置 = 给 delivered 查询补 `pane_generation` 条件，或以注释声明该不对称（单独一轮，须先回报）。

**遗留 12｜`markCompleted` 在 append 成功后的抛错极端路径（`:1006`）**：`#salvageOrCancelMissingAgentPlan` 先由 `#salvageOrphanTerminalEvent` append + 落库成功，再 `store.markCompleted(row.id)`（`:1006`）；若这个 markCompleted 抛错，catch 只记账，计划留 `pending`，下次 drain 经幂等键 `agent.failed:plan:<id>` 取回**同一行**重试 → 同一事件**二次推送**。reviewer 判为**无行为缺口**（自愈：下次 drain 重试后计划最终 completed），且 SQLite 本地近乎不可达。处置 = **可不改**（无新触发条件；markCompleted 失败若进入观测视野再评估）。

**表述澄清（不改「测试」节原文）**：`:2576`（`test/integration/turn-completion-signal.test.ts`）与 `:2805`（`test/integration/agent-index-service.test.ts`）两条的「**改前红**」是**对返工前草稿**而言（本批初版：结构约束无条件合取 + 来源①不过已投递窗口比对），**不是对 `main`**——`main` 上 confirmed 冻结基样本就放行（旧码 `confirmedDeliverable` 把 `confirmedTerminal` 当硬合取项），来源①也不过比对。**不得据「对 main 不红」判这两条用例无效**：它们钉的是防回退——`:2576` 防「confirmed 轮被结构约束重新抹空 → 空 `agent.done`」（返工①）；`:2805` 防「来源①无条件返回基线 → 假失败 + 旧正文」（返工⑤）。测试节现有措辞与该语义不冲突，保持原文不动。

### C. oracle 发现（来源标注 oracle w18:p1T）

**遗留 13｜来源② `limit 20` 在 `delivery_attempts` 过滤之前截断（用户决定 A2：本轮不改）**：来源② SQL（`:823`）仍是 `order by id desc limit 20`——先截断 20 行，再在循环里过滤 `delivery_attempts !== 0`（`:867`）。**触发条件**：关页后 plan 基线不可用（或与已投递同文），且 pane **未投递的终态正文落在最近 20 行之外** → 编排者拿不到。主路径是来源①（plan 基线快照，无 20 限制），故可推迟；触发后处置 = 按「终态非空且未投递」在窗口内条件取样再截断，或带触发条件扩深（单开一轮，须先回报）。

**遗留 14｜内容守卫超 200 行漏拦：量级补记**：与 ⑦ 遗留 6 同项，补记生产量级——单 pane acked `agent.done` 最多 **245** 条（oracle 本轮只读复核，见遗留 19），该 pane 确有大段超出 `STALE_DUPLICATE_GUARD_SCAN_LIMIT`（`:126`）200 行的更旧投递；与「超窗漏拦是接受代价」的既定口径一致。触发条件沿用遗留 6（实测「>200 行的旧投递绕过内容判据被当新答案重投」再评估，须先回报）。

**遗留 15｜catch 谓词偏宽：`AbortError` / `message.includes("aborted")` 被当成关页（用户决定 A3：维持现状 + 记账）**：`:1452-1459` 的 `isPaneClosed` 把 `err?.name === "AbortError" || err?.message?.includes("aborted")` 与真实 `agents.isPaneClosed` 并列，命中即走挽救/取消（`:1460-1464`）。**触发条件**：**开着的 pane** 在执行中被判成关页 → agent 行还在时 `#resolvePlanOutcomeAgent`（`:2809`）拿到活 agent → 写出**带 `agent_id`** 的 `agent.failed` 并 `markCompleted`，而 reason 仍是 `pane_closed_before_delivery`。**现状**：暂未找到会把 `"Herdr request aborted"` 打进 `#runPlanRow` 的调用（`sessionSnapshot()` 不传 abort signal），属**潜伏**；shutdown 等待仍由 `#throwIfShutdownAbort`（`:1999`）转成 `PlanWaitingHistoryError`、计划保持可重试，S11 契约未被覆盖。触发后处置 = 收窄谓词并补用例。

**遗留 16｜挽救行 `payload.agent` 为 null：唤醒身份退到 `paneId`**：`#minimalAgentFromLatestEvent`（`:729`）不恢复 kind，唤醒身份取自 `wake.ts:126` 的 `stringValue(payload.agent) ?? stringValue(event.agentId) ?? paneId ?? event.terminalId`，退到 `paneId`；与来源②的**去重 kind**（`agentKindFromPayloadJson`，`:3124`，即 ⑦ 遗留 10）不是一回事，**不挡投递**。**触发条件**：出现「同 pane 多 agent、唤醒归错身份」的实证再评估（当前无证据）。

**遗留 17｜来源②不按 `status` 过滤：潜在第二条 `agent.failed`**：来源②循环（`:866-872`）只要求 `delivery_attempts = 0` + 终态非空 + 未命中已投递窗口，**不看 `status`**——`invalidated` 且 `delivery_attempts=0`、正文仍是非空终态的行，可能被救成**第二条 `agent.failed`**。本批形态受限：降级抹空的行过不了 `hasNonEmptyAssistantMessage`（`:869`）。**触发条件**：出现「同 plan 已挽救写过一条 `agent.failed`、pane 上另有 invalidated 未投递非空终态行」的实证，再评估补 `status` 条件（须先回报）。

**遗留 18｜canary 口径补充**：⑦ 遗留 1/2 的两组 journal 对比必须与**部署后自身基线**比，**不要和这 364 条历史行比**；当前缺的是**观察时长与数值阈值**（不挡合并）。

**遗留 19｜oracle 本轮只读复核生产库计数（`~/.herdsman/state.db`，`mode=ro`）**：`no_advance_from_input` **364**、acked 终态 **3439**（`agent_id` null **3331**）、计划 cancelled **439** / failed **174** / discarded **16** / completed **7423**、**无 pending/running**、单 pane acked `agent.done` 最多 **245**；与 oracle 上轮快照轻微漂移（daemon 在跑），两套数值都已有出处（⑧ 表 + 本节），互不覆盖。

### D. reviewer 单审发现（来源标注 reviewer `w18:p1W`；本 delta 只落盘、不处置）

**遗留 20｜用例锚点偏差已在本批订正（来源 reviewer `w18:p1W` 建议）**：①-⑧「测试」节既有用例锚点 `:2876` / `:3005` / `:3046`，与 `rg -n` 在 `test/integration/agent-index-service.test.ts` 实测的用例起始行 `:2880` / `:3009` / `:3050` 相差 4 行（本 delta 逐条亲测复核，三条均命中 `test(` 起始行）。实测方式：`rg -n` 按用例名匹配——`cannot be stitched for an orphan salvage` → `:2880`、`still cancels when nothing is salvageable` → `:3009`、`deliverable terminal content when the agent row is gone mid-execution` → `:3050`。该偏差属 ①-⑧ **已审部分**的既有偏差，本批以「只改数字」方式订正（共 8 处：「测试」节条目 7/8/9、⑦ 遗留 4 内两条、来源节「测试」三处），未改任何表述语义。

**遗留 21｜reviewer 观察：`⑨` 节条目数与主代理任务书的预期计数口径差（B 3 条 vs 预期 4、C 7 条 vs 预期 8）**：reviewer 实测「所有被点名要求核验的条目均已落盘且带触发条件，无静默丢弃」，判为**计数口径差**而非缺项（如 §A 的用户决定（A1 决定）与开头「行号订正」子清单未计入 B/C 的遗留条数）。**结论：无需补条目**，本条仅作口径说明。**触发条件**：下次有人按任务书计数核对 `⑨` 节时，先按本节口径（§A 决定、行号订正均不计入 §B/§C 遗留条数）再核。

**遗留 22｜reviewer 观察：遗留 19 的生产库计数未在单审中独立复跑**：该条系 oracle 只读快照记账，非代码行为断言；reviewer `w18:p1W` 本轮**未**独立复跑该计数（不挡提交）。**触发条件**：若后续需要以此计数做决策或发布前核对，须自行只读复跑（`mode=ro` 打开 `~/.herdsman/state.db`）并在 `⑧`/`⑨` 标注复跑时间与复跑人。

> 本笔 delta（① 三条用例锚点行号订正共 8 处、② 新增遗留 20-22）经**用户当次指定的单审**（笔记层 delta 只走 reviewer 单审，不走双审）后提交；未改任何源码/测试。

## 来源

- 源码（本批逐处打开核对，行号为改后值）：`src/observability/agent-index-service.ts:126`（`STALE_DUPLICATE_GUARD_SCAN_LIMIT = 200`）、`:160`（`ORPHAN_SALVAGE_REASON`）、`:545`（drain 入队失败兜底调用点）、`:810`（`#salvageableTerminalBody`，来源①/②同口径）、`:894`（`#salvageOrphanTerminalEvent`）、`:935`（可投递性判定调用）、`:978`（`#orphanScopeDeliverable`）、`:996`（`#salvageOrCancelMissingAgentPlan`）、`:1044`（`#publishSalvagedEvent`）、`:1085`（`#drainPlanRow` 缺失 agent 分支）、`:1197`（`#runPlanRow`）、`:1309`/`:1317`（`PLAN_CANCELLED` → 挽救）、`:1333`（`undefined && isPaneClosed` → 挽救）、`:1460`/`:1463`（catch isPaneClosed → 挽救）、`:1521`（`#waitForHistoryAdvance`）、`:2072`（`#appendStatusEvents`）、`:2209`（`if (turn?.received)`）、`:2243`（`confirmedTerminal`）、`:2251`（`degradeOrRelease`）、`:2365`（`hasDeliveredTerminalRow`）、`:2376`（`diskDeliverable`）、`:2423`（冻结基线放行条件）、`:2431`（放行 warn）、`:2709`（agent 行消失 → `PLAN_CANCELLED`）、`:2809`（`#resolvePlanOutcomeAgent`）、`:2829`（`#appendPlanFailedEvent`）、`:3124`（`agentKindFromPayloadJson`）；`src/db/agent-events.ts`（`append` 孤儿降级、`nextDeliverableAfter`、`isDeliverableAgentEvent`、`reservePending`）；`src/agent-history/discovery.ts`（无 `agent_session` 即不猜路径）；`src/daemon/observability-server.ts:244`（`publishAgentEvent` 死信条件）；`packages/herdsman-pi/src/turn-signal.ts:84-117`、`packages/herdsman-pi/src/index.ts:1874`（`handleAgentEvent` 推送驱动唤醒）、`packages/herdsman-pi/src/wake.ts`（`outcomeKind` / `formatAgentOutcomeUpdates`）。
- 测试：`test/integration/turn-completion-signal.test.ts:2367`、`:2431`、`:2516`、`:2576`；`test/integration/agent-index-service.test.ts:2697`、`:2805`、`:2880`、`:3009`、`:3050`。
- 生产库（只读）：`~/.herdsman/state.db` 计数见 ⑧。
- 相邻笔记：`.agents/notes/20261002-m2-disk-body-and-canary-read.md`（M2 mismatch 臂 / 加宽守卫 / 遗留登记 2、3、4、5）、`.agents/notes/20261001-pi-confirmed-turn-frozen-baseline-empty-body.md`（冻结基线机理、判据一/二/三、对「更新基线」「confirmed 置 degraded」「endsWith 约束」「一律放行」的否决）、`.agents/notes/20260930-phase1-delivery-latency.md`（degraded 不触发重试环、收敛依赖 15min tick）、`.agents/notes/20260930-terminal-event-delivery-h1.md`（孤儿终态行机制与投递闸门放行条件）、`.agents/notes/20261002-empty-wake-expectedtext-rootcause.md`（盘上正文为权威、扩展改文本级确认、M1/M2 定义与 `expectedText` never-match 两类来源）。
- 本批验证（详见回传）：`pnpm check`（mise node v26.7.0）+ `pnpm build` + `pnpm package:check` + nvm node v22.23.1 复跑 `pnpm check` + `git diff --check` 全绿。**无 DB schema 变更、无 migration、`packages/**` 零改动、未 commit/push/merge。**

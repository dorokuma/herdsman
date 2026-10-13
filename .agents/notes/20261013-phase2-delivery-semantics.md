---
status: active # active | superseded
superseded_by: ""
supersedes: ""
# 模块可选值: observability, daemon, cli, config, db, herdr, shared, herdsman-pi, herdsman-herdr-plugin, release
模块: observability | herdsman-pi
---

# Phase 2 交付语义：metric key 契约冻结 + 空轮次三项修复（第四项待裁定）

## 一句话结论

本批先把 A2 闸门要求的 **degraded metric key 契约**冻结成具名常量（`src/observability/contracts.ts`），
再按 Phase 1 笔记登记的三项语义修复落地：**① 缺 assistant 内容的轮次不再落 `agent.failed`**（daemon）、
**② 空正文 completed 不再投递**（Pi 扩展 wake 投影）、**④ 空轮次按 pane 抑制的 `upstream_empty_turn` 告警**。
**第三项「Herdr `done` 语义映射纠正」在本批未实现**：笔记只有条目名、没有语义，本批把它作为**待裁定项**交回
（证据与三个候选读法见下节 ③，Herdr 0.9.3 源码实测：`done` 与 `idle` 是**同一个内部状态**、只差 `seen` 位）。

## 背景

- 规格来源（本批逐条引用）：
  - `.agents/notes/20260930-phase1-delivery-latency.md:193`（「Phase 2 仍需做的语义修复（未完）：缺 assistant 内容不得判 `failed`、`wake.ts` 空 `completed` 过滤、按 `degradedReason` 分类计数/告警」）；
  - 同篇 `:204-205`（Phase 2 四项前置：缺 assistant 内容禁判 `failed`、`wake.ts` 空 `completed` 过滤、Herdr `done` 语义映射纠正、上游空轮次告警 `reason: upstream_empty_turn` + 同 pane 抑制去重）；
  - 同篇 `:111-114`（④：confirmed 直放但正文为空**没有任何信号**）+ `:195-196`（该空正文路径「或在 Phase 2 的 `upstream_empty_turn` 告警里覆盖，否则这条路径继续静默」）；
  - `.agents/notes/20260930-terminal-event-delivery-open-items.md:72-80`（A2：Phase 2 做 degraded 聚合前**先冻结 metric key 契约**——key 名、维度、枚举取值的稳定性等级；三个枚举值与 warn 消息前缀当前只是**准冻结**）；
  - 同篇 `:156-161`（B1：`agent.failed` 保留窗口维持 7d 硬上界，**本批不碰 TTL**）。
- 现役守卫与降级现状（本批实现必须与之相容）：`.agents/notes/20261006-unconfirmed-frozen-baseline-delivery.md`
  （盘上可投递判据 `diskDeliverable` / 冻结基线放行 / `staleBaselineDuplicate` / `hasDeliveredTerminalRow`）与
  `.agents/notes/20261002-empty-wake-expectedtext-rootcause.md`（mismatch 与 stale 守卫的路径分区）。
- 工作分支：`feat/phase2-delivery-semantics`，从 `main`（`c96097e`）开；本批**未 push、未部署、未碰生产库**；
  基线 `main` 上 `pnpm check` **EXIT=0**（53 files / 852 tests）。

## 决策

### 前置：A2 metric key 契约冻结（`src/observability/contracts.ts`）

- **契约面**（`contracts.ts` 内注释逐字登记，与本节同口径）：
  - 消息前缀 `DEGRADED_EVENT_MESSAGE = "Herdsman emitted degraded status event"`；
  - `degradedReason` 取值 `DEGRADED_REASON`：`expected_text_mismatch` / `no_advance_from_input` /
    `non_terminal_assistant` / `stale_baseline_duplicate`（第 4 个自
    `.agents/notes/20261001-pi-confirmed-turn-frozen-baseline-empty-body.md` 判据三起就在发射，**本批把它折断成具名常量**，
    属「把现实写进契约」的澄清，不是行为变更）；
  - 空轮次告警 `EMPTY_TURN_ALERT_REASON = "upstream_empty_turn"` + `EMPTY_TURN_ALERT_MESSAGE`；
  - 计划行的合成错误标 `DEGRADED_PLAN_ERROR = "degraded"`（`status_event_plans.last_error` 与 `markRetry` 的 `Error`）；
  - 维度名 `planId / agentId / attempts / degradedReason / herdrSessionName / paneId`（原样保留，A2 已登记）。
- **稳定性等级**：取值字符串即契约。**改名一个取值或消息前缀 = 契约变更**，须同步 `20260930-phase1-delivery-latency.md`
  的「遗留」与本笔记；**新增取值 = 增量契约变更**，同样须登记（本批新增的 `upstream_empty_turn` 即按此办理，见 ③/④ 节）。
- **消费面（四项修复只消费契约）**：`agent-index-service.ts` 的三处 `degradeOrRelease` 调用点、
  `#logDegradedRelease` / `degradeOrRelease` 的形参类型（`DegradedReason`）、两处无信号分支的载荷，
  以及 7 处 `"degraded"` 计划错误标全部改为常量。**没有第二套并行判据**，也没有把新取值塞进旧枚举。
- **无测试**：纯常量定义 + 字面量替换，行为零变化（`pnpm check` 852 → 854 的增量全部来自 ② 与 ④ 的回归）。

### ① 缺 assistant 内容不得判 `failed`（daemon）

- **规格**：`20260930-phase1-delivery-latency.md:193` 与 `:204-205`（「缺 assistant 内容禁判 `failed`」）。
- **实现**：`src/observability/agent-index-service.ts` 的 `#appendPlanFailedEvent`
  （唯一写 `agent.failed` 的入口）在写行之前加一条**同时满足**两个条件的守卫：
  `input.reason === DEGRADED_PLAN_ERROR && !hasNonEmptyAssistantMessage(input.compactHistory)` → `return undefined`。
- **为什么是这两个条件的合取**：`degraded` 是**合成原因**，只在正文被抹空的那几条臂上产生
  （`no_advance_from_input` / `expected_text_mismatch` / `non_terminal_assistant` / `stale_baseline_duplicate`），
  所以它就是「本轮缺可交付 assistant 内容」的现成判据；再用 `hasNonEmptyAssistantMessage` 兜一次
  「基线里真的什么都没有」，避免把**非降级**失败（例如 `RETRY_EXHAUSTED` 的存量补录行）一起吞掉
  ——后者由既有用例 W3 / H1 / W6 的补录断言钉住，本批未放宽。
- **连带的契约收敛**：返回值类型改为 `AgentEventRecord | undefined`，四个消费点同步收紧
  （`#retryWaitingPlanRow` 与 `#drainPlanRow` 的 `if (failedEvent && …)`、`#backfillFailedPlanEvents` 的
  `if (!event) { skipped += 1; continue; }`、orphan 挽救路径的防御性 `if (!event) return null;`）。
  计划行本身**仍落到 `failed`**（那是计划结算态，不是对 agent 的判决），因此补录路径也必须跳过——否则下一轮 drain 会把行补上。
- **语义代价（显式申报）**：H1 引入的「degraded `done` 耗尽重试后发一条**可投递**的 fallback `agent.failed`」因此被**收窄**：
  没有内容的轮次不再以 `agent.failed` 唤醒编排者，改由 ④ 的日志告警承接。该行为有两个既有用例**按定义被翻转**，
  本批把它们改成钉新语义（不是放宽断言）：
  `test/integration/agent-index-service.test.ts` 的 `degraded done exhausting retries records no agent.failed for an empty round`
  （原 `… emits deliverable fallback failed event`）与 `W6: agent.failed is still delivered to owner after agent row is deleted`
  （后者**保留原覆盖意图**：把计划基线换成**带正文**的，使 H1 的孤儿写路径继续被钉住）。

### ② `wake.ts` 空 `completed` 过滤（Pi 扩展）

- **规格**：`20260930-phase1-delivery-latency.md:193` 与 `:204-205`（「`wake.ts` 空 `completed` 过滤」）。
- **实现**：`packages/herdsman-pi/src/wake.ts` 的 `project()` 在算完 `text` 之后、产生 outcome 之前加一条：
  `kind === "completed" && text.length === 0`（`normalizeExcerpt` 已 trim，纯空白同样命中）→ 进静默抑制通道。
- **为什么进抑制通道而不是只丢 outcome**：空 `completed` 原先被渲染成 `last assistant: (no assistant message)`
  投给编排者（正是 ④ 与 `20261001-…-empty-body.md` 反复描述的形态）。只从 `outcomes` 里丢掉会让该事件永远无人 ack
  ——daemon 的投递队列不收敛、pending 计数长期虚高。抑制通道（原 `suppressedUpstreamErrorEventIds`，**本批改名为
  `suppressedEventIds`** 以容纳两族：上游模型错误 + 空 completed）走扩展的**静默 ack** 路径：不 `sendMessage`、
  不 notify、不写 `presentedEventIds`，`rawEvents` 仍保留为证据、不进投影器 `seen`。改名是纯标识符变更
  （`wake.ts` / `index.ts` 两处 / 既有 11 处测试断言），无断言放宽。
- **不动的部分**：`failed` / `blocked` 的空正文行为原样（`failed` 只印 reason 行，`(no assistant message)` 兜底仍在
  `formatAgentOutcomeUpdates` 里保留，只是 `completed` 不再能走到它）；上游模型错误过滤保持在前，语义不变。

### ④ 上游空轮次告警 + 同 pane 抑制去重（daemon）

- **规格**：`20260930-phase1-delivery-latency.md:196`（「confirmed 直放但正文为空」的计数/日志，或在 Phase 2 的
  `upstream_empty_turn` 告警里覆盖）与 `:204-205`（同 pane 抑制去重）；`:111-114` 是它要覆盖的静默路径。
- **实现**：`#noteEmptyTurnAlert` —— 与 degraded 行**并列**的一条 `console.warn`，消息 `EMPTY_TURN_ALERT_MESSAGE`
  （“Herdsman observed an empty upstream turn”），字段与 degraded 行同形并带 `reason: upstream_empty_turn`；
  **按 pane 抑制**：键 `herdrSessionName\0paneId` 存于 `#emptyTurnAlertPanes`，同一条空轮次 streak 只打一行。
- **三个调用点（覆盖全部空轮次结算面）**：
  1. `#logDegradedRelease`（无信号分支与未确认的 `degradeOrRelease` 路径）；
  2. `degradeOrRelease` 的 **confirmed 软化**分支（这条路径永不置 `degraded`，是 ④ 里唯一无法被 degraded 行覆盖的静默放行）；
  3. ① 的守卫跳过点（重试耗尽、行不再落库的那次结算）。
- **抑制的解除**：`#appendAndAckSelfEvent` 在写入**带正文**的事件时删掉该 pane 的键——即「空轮次 streak」以一次交付正文结束，
  下一次空轮次会重新告警。抑制集大小 = 该 daemon 结算过的 pane 数（有界）。

### ③ Herdr `done` 语义映射纠正 —— **未实现，待裁定**

- **事实**：Phase 1 笔记只有条目名（`:205`），A2 台账与其余规格笔记都**没有**给它定义行为；本批逐篇核对
  （`20260930-phase1-delivery-latency.md`、`20260930-terminal-event-delivery-open-items.md` A/B/C/D 全节、
  `20260930-terminal-event-delivery-h1.md`、`20260930-terminal-event-delivery-latency-observation.md`、
  `20261006-leftover-disposition.md`、`20261006-unconfirmed-frozen-baseline-delivery.md`、
  `20261002-m2-disk-body-and-canary-read.md`、`20261002-empty-wake-expectedtext-rootcause.md`）后，
  **三处唯一的「Herdr `done`」命中就是条目名本身**。⇒ 本批**不猜实现**（角色纪律：授权/目标不清先停）。
- **随手取到的 Herdr 0.9.3 一手事实**（外部 checkout `/workspace/herdr`，用于后续裁定，不构成实现）：
  - `src/app/api_helpers.rs:63-71` 的 `pane_agent_status(state, seen)`：`(Idle, false) ⇒ Done`、`(Idle, true) ⇒ Idle`、
    `Working/Blocked/Unknown` 与 `seen` 无关 ⇒ **Herdr 的 `done` 与 `idle` 是同一个内部状态 `Idle`，只差 `seen` 位**；
  - `SKILL.md:38`「`done` means the agent finished, but you have not looked at that finished pane yet.」；
    `docs/next/README.md:153-154`（🔵 done = work finished, not looked at yet；🟢 idle = done and seen）；
  - `CHANGELOG.md:567` 与 `docs/next/CHANGELOG.md:567`：「Background panes that start idle no longer show up as `done`
    or trigger finished-state attention until they have actually transitioned from **working or blocked** to idle.」
    ⇒ Herdr 自己的「finished」口径是「**从 working 或 blocked 进入 Idle**」。
- **三个候选读法（互斥，均未实现；每个都是一处条件改动 + 一条回归）**：
  - **C1｜`blocked → idle` 也是完成**：`isDeliverableAgentEvent` 规则 2 与 `wake.ts` 的 `payload.from === "working"`
    都只认 `working` 起点，而 Herdr 的完成口径含 `blocked`；连带 `status-event-plans.ts` 的
    `cancelSupersededPendingPlans`（`from_status <> 'working'` 的 idle 计划一律作废）也要同步，否则作废规则会吃掉可唤醒计划。
  - **C2｜pi 的 `idle` 与 `done` 等价**：`#appendStatusEvents` 的 pi 终态分支只认 `to ∈ {done, blocked}`，
    而 Herdr 把「已看过的完成」报成 `idle`；若 pi 子代理结束的瞬间 pane 处于 seen 态，这条完成会落成不可投递的
    `agent.idle`（`isDeliverableAgentEvent` 规则 6 对交互 pi 的 idle 一律拒绝）⇒ 可能漏唤醒。
  - **C3｜`done` 也要有来源门**：把「`done` 一律当完成」收窄为「`from ∈ {working, blocked}` 的 `done`」，
    防止启动/稳态的 `done` 造出空完成轮（正是本批 ①②④ 收拾的形态）。
- **裁定后要做的事**：确认读法 → 在对应条件上实现 → 补 1 条回归（先 FAIL 后 PASS）→ 在本笔记「更新」段登记，
  并同步 A2 契约面若涉及取值/维度。

## 被放弃的方案（必填）

- **把 ① 写成「任何失败只要 `hasNonEmptyAssistantMessage` 为假就不落行」**（不含 `DEGRADED_PLAN_ERROR` 合取）：
  已实测会打穿 3 条既有补录用例（W3 / W6 / H1 的 `agent.failed` 补录）——它们钉的是**非降级**失败行的补录契约，
  与本项目标无关；收窄成两条件合取后只剩按定义要翻转的 2 条。
- **把 ① 落在 `markRetry` / `status-event-plans.ts`（把 degraded 耗尽改判 `cancelled`）**：会改计划状态机语义
  （`failed` → `cancelled`）并牵动 reconcile/清理/日志口径，收益仅是少写一行事件；改在唯一的事件写入口更小、更可读。
- **只丢 outcome、不进静默 ack（②）**：daemon 队列不收敛、pending 计数虚高（见 ② 节）。
- **为 ② 另开一个 `suppressedEmptyCompletedEventIds` 字段**：与上游错误族走同一条静默 ack 路径，两个字段只会让
  `index.ts` 两处调用点重复拼接；改名为 `suppressedEventIds` 一句注释即可说明两族。
- **把 `upstream_empty_turn` 追加进 `DEGRADED_REASON` 枚举**：它不是降级原因（没有任何东西被降级/抹空，
  轮次本来就没有内容），混进去会让「按 degradedReason 分组」的既有 canary 口径失真（A2 的 ④ 口径）。
- **带着不确定去实现 ③**：本批是交付语义批次，猜错会改动生产投递行为（C1/C2 都会改变谁能被唤醒）；按角色纪律交回裁定。
- **本批顺手做 degraded 聚合指标 / B1 的 TTL / M2 遗留 b（同正文不同 ref 用例）**：均不在本批四项之内（后者任务书明确排除）。

## 遗留 / 观察项

1. **③ 待裁定**（见上节）：本批的 `pnpm check` 与提交都不含它；裁定后按同规格补一项 + 一条回归。
2. **① 的语义代价需要 canary**：degraded 轮次不再发 fallback `agent.failed`，编排者侧从「收到一条空正文的 failed」
   变成「日志一条 `upstream_empty_turn`」。观察面：journal `Herdsman observed an empty upstream turn`（本批新增，
   按 pane 抑制 ⇒ **不是每轮一条**）与 `Herdsman status event plan failed after max attempts` 的同窗对比；
   预期后者仍会出现（计划行照旧落 `failed`），前者应当是「每 pane 每 streak 一条」。
3. **① 只覆盖 `agent.failed` 事件本身**：计划行仍落 `failed`，`#backfillFailedPlanEvents` 对该类行**计入 `skipped`**
   （不是错误）。若将来有人按 `status_event_plans.status = 'failed'` 与 `agent_events.type = 'agent.failed'` 做对账，
   会看到这一类**有意的不配对**（本批在此登记）。
4. **④ 的抑制是内存态**：daemon 重启后同一 pane 的空轮次会再告警一次（与 D1 的「每进程一次」告警同型）。
   若需要跨重启抑制，得落持久状态（本批不做，属 owner 口径）。
5. **`formatAgentOutcomeUpdates` 的 `(no assistant message)` 兜底仍在**：投影层已不再产生空 `completed` outcome，
   因此 `kind !== "failed" && text === ""` 这条分支从生产路径上**已不可达**；它**没有任何用例钉住**
   （`test/unit/herdsman-pi-wake.test.ts:104-126` 唯一那条空正文断言用的是 `kind: "failed"`，走的是 `reason`-only 分支），
   本批**未删**——它是 `AgentOutcome` 类型层面的兜底而不是本项目标，删除会超出本批范围（如需清理请另开一轮）。
6. **② 的静默 ack 会与水位屏障交互**：与上游错误族共用 `unconfirmedWatermarkBarrier` 逻辑（不改语义），
   但空 completed 从此**不再进入编排者上下文**——若运维希望在卡片上看到「某子代理跑了一轮但没产出」，
   只能看 daemon 日志（本批的有意取舍，登记于此）。
7. **③ 的三个候选都需要独立复核 Herdr 语义**（C1/C2 会扩大唤醒面，C3 会缩小），且都应先确认「真实生产里
   Herdr 到底把哪些完成报成 `done` / `idle`」——本批只有 Herdr 源码与文档证据，**没有生产 journal 的 `agent.idle`
   与 `agent.done` 配对统计**（建议裁定前先只读统计一次）。

## 来源

- 任务书：`[MARK-WORKER-HSM-010]`（Phase 2 四项 + metric key 契约冻结前置；分支 `feat/phase2-delivery-semantics`，
  不 push/不部署/不碰生产库）。
- 规格与口径：`.agents/notes/20260930-phase1-delivery-latency.md`（`:193`、`:195-196`、`:204-205`、`:111-114`）、
  `.agents/notes/20260930-terminal-event-delivery-open-items.md`（A2 `:72-80`、B1 `:156-161`）、
  `.agents/notes/20261006-unconfirmed-frozen-baseline-delivery.md`（现役守卫）、`.agents/notes/20261006-leftover-disposition.md`（遗留处置口径）。
- 代码事实（本批一手核对，行为面）：`src/observability/contracts.ts`（契约常量）、
  `src/observability/agent-index-service.ts`（`#appendPlanFailedEvent` 守卫与返回类型、`#backfillFailedPlanEvents`、
  `#noteEmptyTurnAlert` / `#emptyTurnAlertPanes`、`#logDegradedRelease`、`degradeOrRelease`、`#appendAndAckSelfEvent` 的重置）、
  `packages/herdsman-pi/src/wake.ts`（`project` 的空 completed 抑制、`suppressedEventIds`）、
  `packages/herdsman-pi/src/index.ts`（两处消费点）。
- Herdr 一手证据（外部 checkout `/workspace/herdr`，`herdr --version` = 0.9.3，仅用于 ③ 的裁定材料）：
  `src/app/api_helpers.rs:63-71`、`SKILL.md:38`、`docs/next/README.md:153-154`、`CHANGELOG.md:567`、`docs/next/CHANGELOG.md:567`。
- 测试与验证：`test/integration/agent-index-service.test.ts`（① 回归 + W6 保留覆盖）、
  `test/integration/turn-completion-signal.test.ts`（④ 回归）、`test/unit/herdsman-pi-wake.test.ts`（② 回归）；
  FAIL→PASS 日志路径与命令见交付报告（`/tmp/hsm-phase2-*`，`/tmp` 会被清理，结论已写在上一节）。
- 已知局限：③ 无规格（本批最大偏离）；①②④ 的生产量级（每 pane 空轮次率、被收窄的 fallback 唤醒量）**本批未测**
  ——生产库只读统计不在本批授权面内（任务书禁止碰生产库），故 canary 基线留待部署后按遗留 2 重读。

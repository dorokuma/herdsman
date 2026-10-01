---
status: active # active | superseded
superseded_by: ""
supersedes: ""
模块: observability
---

# pi confirmed 轮次「基线已含答案」导致的静默空正文修复

## 一句话结论

pi 终态分支把「刷新快照与计划行基线相同」当作「历史未推进」而抹空 `lastAssistantMessage`；由于计划行基线是创建时写一次、之后不再更新（且常在最终 assistant 落盘之后创建），confirmed 轮次被静默放行成空正文。现在有三条判据：confirmed + 非空终态 assistant 直接采信该文本（判据一）；该路径的放行理由改为 `console.warn` 可见（判据二）；当采信对象恰好等于**最近一条终态行的正文**（通常是上一轮已交付的正文，也可能是尚未投递的 `idle` 行）时改为拦下（判据三 `staleBaselineDuplicate`，log-only，不置 degraded）。

## 背景

- 现象：Herdsman 扩展端渲染 `(no assistant message)`，编排者收不到子代理结果。
- 位置：`src/observability/agent-index-service.ts` pi 终态分支（turn signal 已收到、`turn.confirmed === true`）。
- 机理一（**基线冻结**）：`historyHasAdvanced(..., { requireAssistantChange: true })` 只比对 assistant 的 `ref`/`text` 与「计划行基线」。计划行的 `compact_history_json` 由 `StatusEventPlanStore.insertPending` 创建时写入一次，之后没有任何 UPDATE（见 `src/db/status-event-plans.ts`）；实测计划行创建时刻晚于最终 assistant 落盘 131ms / 308ms，于是基线天然已含本轮答案 → 刷新快照与基线相同 → 判定 `no_advance_from_input` → `lastAssistantMessage` 置 null。
- 机理二（**`#waitForHistoryAdvance` 的退出条件**，前一版笔记写错，此处更正）：循环条件是 `!historyHasAdvanced(refreshed.snapshot.compactHistory, input.baseline, { requireAssistantChange: input.requireAssistantChange })`（`agent-index-service.ts:1241-1249`），而 `historyHasAdvanced`（`:29-50`）在 `requireAssistantChange !== true` 时**还有第三条**判据：`current.messageCount > (baseline?.messageCount ?? 0)`（`:49`）。调用处传的是 `requireAssistantChange: isRetry`（`:1245`、`:1983`），所以**非 retry 轮次**（`requireAssistantChange=false`）的退出条件是「与冻结基线比 `ref` 不同、**或** `text` 不同、**或** `messageCount` 变大」——**不是只比 `ref`/`text` 逐字**；retry 轮次才只认前两条。`maxAttempts: 8`、`maxTotalMs: 1500`、固定 200ms 间隔（`:1226`、`:1231-1232`）只是循环上限与总预算，**不是**「8 次 / 1500ms 重读兜底」。反过来用这条签名：走到 ≈预算耗尽的长尾（`maxTotalMs = 1500ms` + 最后一次刷新与 IO 开销，量级 ≈1.5s），说明这次循环跑满了预算，即 `ref` / `text` / `messageCount` 三者**都**没变化（上一版写的 1538ms / 1553ms 来自一次无法给出来源的抽样，本轮无从复核，已改述为「≈预算耗尽」，上一批样本 48085 / 48117 的形态说明一并作废）。上一版据此推出的「7 条属读取早于落盘、靠重读兜底」不成立。
- 又因 `confirmed === true`，`degradeOrRelease` 直接返回 `{ staleSnapshot: false }` 并跳过 `#logDegradedRelease`：产出 completed、无 degraded 标记、无任何日志、正文为空的 `agent.done`，运维只能人工读 pane 才能定位。
- 已证伪「pi schema 漂移 / 抽取器过时」：用已安装的 dist 抽取器（`dist/src/agent-history/pi-reader.js` 的 `PiHistoryReader`）读两份会话 jsonl，末条 assistant 正文长度分别为 6444 / 18999 字符（`stopReason` 都是 `stop`），**本轮用同一 dist 复跑结果一致**：`/tmp/herdr-role-sessions/default/role-worker-0ebc784b/2026-10-01T05-32-54-506Z_01a0f5f3-8d69-756b-b7ba-35e8be614604.jsonl`（84 条消息 / 21 条 assistant）与 `/tmp/herdr-role-sessions/default/role-scout-f3c4efdf/2026-10-01T06-00-42-503Z_01a0f60d-0106-76f6-84be-b26739a7f31a.jsonl`（49 条消息 / 1 条 assistant）。

### 数字口径（可复现，前一版的 179 / 71 / 59 / 46 / 43 / 7 已废弃）

前一版那些数字没有查询、时间窗与判定条件，无法复核，故整段替换为下面这张表。「10-01 窗口内 pi 终态、正文为空、无 reason = 48 条，其中 45 / 1 / 2」这个结论**以第二意见（oracle）的独立统计为准**（主代理裁定采用）：

| 口径 | 时间窗 / 探针时刻 | 判定条件 | 数值 | 来源 |
| --- | --- | --- | --- | --- |
| A（裁定采用） | 2026-10-01 窗口（自 2026-10-01T00:00:00Z 起至探针时刻 ≈ 07:22Z） | pi 谱系 + 终态 + 正文为空 + 无 reason | **48** | 第二意见（oracle）独立统计；**本轮未独立复核**（见下） |
| A 的细分 | 同上 | 逐条对比「计划行基线 / 最近一次已交付正文」 | **45** 基线即本轮答案（本批放行）／**1** 基线＝上一轮已交付正文（本轮新增守卫拦下）／**2** 仍空 | 同上，**未独立复核** |
| A 中「仍空」的 2 条 | 同上 | 事件 id | `47940` 基线无 assistant；`48050` 基线 `stopReason=toolUse` 非终态 | 同上；本机只读复核确认这两条事件确为 confirmed 静默放行（`payload.staleSnapshot=false`、无 `degradedReason`、正文为 null），但基线构成未复核（对应 agent 行已删、`status_event_plans` 无行可比） |
| B（本机可复现复核） | `created_at >= 2026-10-01T00:00:00Z` 且 `<` 探针时刻 | `compact_history_json.source='pi-jsonl'` AND `type in ('agent.done','agent.blocked','agent.idle')` AND (`lastAssistantMessage` 为 null 或 `trim(text)=''`) AND `payload_json` 无 `degradedReason` AND `payload_json.staleSnapshot=false`（即 confirmed 静默放行签名） | **52**（探针时刻 2026-10-01T07:37:26Z） | 本轮 worker，`node:sqlite` 只读打开 `/root/.herdsman/state.db`（`new DatabaseSync(path, { readOnly: true })`） |
| B 回放到口径 A 的时刻 | 同 B，cutoff 取 07:22:00Z | 同 B | **48**（cutoff 07:37:26Z=52／07:30Z=50／07:25Z=49／07:22:30Z=49／07:22:00Z=48／07:21:00Z=48／07:15Z=47／07:00Z=46） | 同上 |

结论与用法：这是一个**活窗口**计数——生产 daemon 每多放行一个「confirmed 空正文」轮次就 +1（相邻两次重算之间就出现过 +1），所以引用该数字必须带 cutoff 时刻；把 B 的 cutoff 回放到第二意见的探针时刻（07:21–07:22Z）即得 48，与口径 A 吻合。另外，若把「无 reason」按字面放宽成「payload 无 `degradedReason`」（不再要求 confirmed 放行签名 `staleSnapshot=false`），同一时刻会得到 75 条——多出来的部分是 `agent.idle` 形的空正文（payload 无 `staleSnapshot` 键），不属于本形态，故不要用这个宽口径引用。

## 决策

1. **confirmed + 非空终态 assistant 一律采信（判据一）**：在 `no_advance_from_input` 判定处引入 `confirmedDeliverable`（`agent-index-service.ts:2012-2016`）；命中时 `compactHistory = advanced`（正文保留）、`payloadExtra = { staleSnapshot: false }`（`:2025`）。`no_advance_from_input` 只留给「确实没有可交付 assistant 文本」的情形（真的空 / 非终态末条）。
2. **confirmed 直通路径不再静默（判据二）**：`degradeOrRelease` 的 confirmed 分支（`agent-index-service.ts:1936-1959`）补 `console.warn("Herdsman released a confirmed pi status event with no deliverable text", { agentId, degradedReason, herdrSessionName, paneId, planId, terminalId })`（`:1944-1954`）。**刻意不置 `degraded: true`**：`#runPlanRow` 见到 degraded 会 `invalidateById(event.id, "degraded_retry")` 并重跑，把已写内容再废一遍（冻结基线必然重跑失败）。
3. **陈旧重复守卫（判据三，本批新增）**：`staleBaselineDuplicate = sameTerminalAssistantContent(advanced, latestTerminal?.compactHistory, "pi")`（`agent-index-service.ts:2003-2007`，复用既有 pi 谱系比较：ref 相等，或双非空 text 逐字相等）。`confirmedDeliverable` 追加 `&& !staleBaselineDuplicate`；落空的 reason 为 `stale_baseline_duplicate`（`:2029`），**仍走 log-only、不置 degraded**——原因同判据二。`latestTerminal`（`:1859` = `agentEvents.latestTerminalEvent`，取该 agent 最近一条 `type in ('agent.idle','agent.done','agent.blocked')` 且 `status != 'invalidated'` 的行，未投递的 `pending` 行也算）在本轮事件行写入前读取，因此它是**最近一条终态行**：通常是上一轮已交付的正文，但也可能是**尚未投递**的终态行（例如还没被 ack 的 `idle`）。判据三是内容级比较（`sameTerminalAssistantContent`，`:2677`：ref 相等，或双非空 `text` 逐字相等），对两类参照物都成立。源码注释原先写的「strictly the previous delivery」是夸大，已按本条改为「最近一条非 invalidated 终态行：通常是上一次投递，也可能是未投递的 idle」，两边口径以此为准。
   动机（第二意见 M1）：判据一放行条件太宽，会把「上一轮已经交付过的正文」当成本轮新结果再送一次；审查方（reviewer）提出的反例正是这一条。
   为什么不改成「按 `expectedText` 判定、基线=上轮交付则拦」以外的更严口径：见「被放弃的方案」第 3 条。
4. **不动其它路径语义**：非 confirmed（无 turn signal）分支、`expected_text_mismatch`（`:2037`）与 `non_terminal_assistant`（`:2040`）的既有置 null 行为、`isRetry` 的 `PlanWaitingHistoryError` 分支全部保持原样；无 schema 变更、无迁移、无字段改名、无新 payload 字段。
5. **回归测试先红后绿**：`test/integration/turn-completion-signal.test.ts`
   - W14（判据一）：基线 = 最终答案 + `confirmed`，修复前红（`lastAssistantMessage` 为 null → `?.text === undefined`），修复后绿（`text === "final answer"`、`staleSnapshot === false`）；
   - W14b（判据一，O5 补形态）：**每次读盘都是终态且与冻结基线逐字相同**（W14 走的是「首读非终态」`serveNonTerminalOnce`，不是生产实测形态），`expectedText` 与磁盘不一致（生产前提 `textMatches=false`）→ 仍放行；
   - W15（判据二）：基线为空 + `confirmed`，修复前 `console.warn` 调用数为 0（完全静默），修复后命中带 reason 的放行日志，且事件仍 `staleSnapshot: false`、无 `degraded: true`；
   - W16（判据三）：基线 = 上一轮**已交付**的终态正文 + `confirmed` + 本轮无新文本 → 守卫落地前红（实际值 `lastAssistantMessage = { ref: "m1", text: "round-1 answer", stopReason: "stop" }`，陈旧正文被当新结果放行），落地后绿（正文为空、`staleSnapshot: false`、无 `degraded: true`、`console.warn` 命中 1 次且 reason = `stale_baseline_duplicate`、无 invalidated 行）；
   - W17（判据三不误伤）：基线 = 本轮答案、与最近交付正文（ref 不同）不同 → 守卫不触发，正文照常放行；
   - W18（reviewer 反例的既定语义，钉住）：`expectedText` 与磁盘文本不一致、且该内容从未交付过（最近一次已交付是空正文行）→ 按裁定**仍放行**；`expectedText` 不可靠属单独议题（见遗留 ①）。

## 被放弃的方案（必填）

- **更新计划行基线（`compact_history_json` 后续 UPDATE）**：直接消除「基线早于答案」的信息腐蚀，但它是持久化语义变更（要改 schema 行为 + 迁移 + 并发写语义），本轮目标是最小修复，且判据一已能在读取侧自洽，故不做。
- **把 `confirmed` 直通标 `degraded: true`**：会让 `#runPlanRow` 把刚写入的终态事件 invalidate 并重跑，重跑仍会撞同一冻结基线，等于把「空正文」换成「反复空正文」，放弃。
- **给判据一加 `!expectedText || advancedText.endsWith(expectedText)` 约束**（审查方提案）：不采纳。本批能改变行为的样本**恒为 `textMatches=false`**（生产 45/48 命中该前提），加该约束会把整批目标样本一起打回空正文（等于修不成）；且 `endsWith(expectedText)` 对「本轮 vs 上轮」零分辨力，审查方真正担心的「基线=上一轮已交付正文」由判据三覆盖。`expectedText` 本身系统性不可靠属单独议题（见遗留 ①）。
- **`no_advance_from_input` 一律放行（不区分文本）**：会把真正无文本的轮次也放行成空 `agent.done`，重新引入「空回传被投递」的老问题，放弃。
- **只加日志不修判据**：可观测性有了但仍然交付空正文，编排者依旧拿不到结果，不满足目标，放弃。

## 遗留 / 观察项

1. **pi 扩展侧 `expectedText` 系统性不一致（可能是下一大来源）→ 单独立项**：`packages/herdsman-pi/src/turn-signal.ts:89` 用 `size > initialSize`（文件字节数变大）作为「新内容已落盘」的确认，属**尺寸级**确认而非**文本级**确认——尺寸变大不等于本轮最终 assistant 文本可读，于是 daemon 侧 `expectedText` 与磁盘正文经常不一致（生产 45/48 命中 `textMatches=false`）。本项目前只作为「是否进入 `#waitForHistoryAdvance`」的分支判据，不再被当成放行条件（见被放弃方案第 3 条）；补齐办法是让它做文本级确认（例如 tail 窗口内含 `expectedText` 才算 confirmed）。**本批不改 pi 扩展。**
2. **判据三的覆盖面缺口（守卫只覆盖一条路径）**：`staleBaselineDuplicate` 只在「收到 turn signal + 非 retry + `!historyHasAdvanced(advanced, input.compactHistory, { requireAssistantChange: true })`」这一条分支里生效（`:2017-2031`）。以下路径未被覆盖，在**「转换（`working → done`/`blocked`）在工具阶段被处理 + 5s 内收到信号 + `expectedText` 缺失（或后缀巧合）」**时，仍可能把最近一条终态行的正文当作本轮结果再交付一次：
   - 判据一快路径（`:1960-1966`）：`fresh` 本身就等于最近一条终态行的内容；
   - 判据二快路径（`:1967-1975`）：`isTerminalAssistant(input.compactHistory)` 成立即放行，而 `input.compactHistory` 可能是工具阶段刷新的旧快照；
   - `else` 里 `historyHasAdvanced(advanced, input.compactHistory, { … })` 为真的分支（`:2032-2044`）：`advanced` 与「本轮 in-memory 快照」不同、但与「最近一条终态行」相同；
   - 无信号分支的 `else`（超时 else，`:2088-2090`）：结构同上，区别是额外有 `console.warn("…without a turn completion signal after 5000ms")` 可见。
   生产库只读复核的同类样本（本轮用 `node:sqlite`（`readOnly: true`）取回；形态都是「同 `ref` 同文的多条 `agent.done`」）：`44300` / `44315`（ref `…/2026-09-28T13-42-26-359Z_01a0e840-a736-7292-b168-4f476edf3621.jsonl#entry=b7be4457`，284 字、text sha256 前 12 位 `74480e3c9271`；中间夹着 `44304`（oracle idle）与 `44307`（agy done），故不相邻）；`44913` / `44952` / `44958`（ref `…/2026-09-28T16-04-36-591Z_01a0e8c2-d06f-7699-aff0-253aaa13156c.jsonl#entry=e5e3bba1`，94 字、sha `c98d7ef7d497`；其中 `44952 → 44958` 相邻，是全库唯一一对「相邻 `agent.done` 且同 ref 同文」）。样本取自 2026-09-28、早于本批守卫落地，只作形态证据，不据此断言因果。
3. **`staleSnapshot` 全仓只有写、0 个读取方**：`src` 内 5 处写（`:1955`、`:1958`、`:2025`、`:2078`、`:2086`），除测试断言外没有生产读取。本批维持 log-only、不加 payload 字段；若将来要能定期扫出这类轮次，再另批立项。
4. **canary 验收：陈旧重复一条已按实测改口径**：
   - ① 空正文数下降（用「数字口径」表里的口径 B，带 cutoff 对比）；
   - ② 新增 warn 的 `degradedReason` 分布只含预期几类（`no_advance_from_input` / `stale_baseline_duplicate` / `expected_text_mismatch` / `non_terminal_assistant`）；
   - ③ **陈旧重复不再交付 = 0**（判据三的直接目标）。原写「同 pane 相邻两条非空终态正文逐字相同 = 0」**不可达**：该口径在基线上本就有 **2 组**合法重复（不同 `ref`、文本相同的正常回复：`39563`/`39566`、`39588`/`39591`，都是 14 字、text sha256 前 12 位 `1f86a7bffb19`，ref 分别为 `…#entry=fb4e6a9c`/`…#entry=50584528` 与 `…#entry=c64bc70c`/`…#entry=8498ee2a`；第二意见给的是「2–3 组」，本机实测恰为 2 组）。第二意见建议改成「同 `ref` 相邻非空终态正文 = 0」，但**不加限定同样不可达**：全库基线有 340 对同 `ref` 重复，其中 303 对是 `agent.done(from=working) → agent.idle(from=done)` 的同内容双报（合法形态；判据三只在 `to in ('done','blocked')` 的 pi 分支生效，`agent.idle` 根本不走这段代码）。所以本批采用的口径是**「相邻两条 `agent.done` 且 `ref` 相同、正文逐字相同 = 0」**：

   | canary ③ 口径 | 全库基线（截至 2026-10-01T07:57Z） | 10-01 窗口 |
   | --- | --- | --- |
   | 相邻非空终态正文逐字相同（原口径） | 342（同 `ref` 340 / `ref` 不同 2） | 21（同 `ref` 21 / `ref` 不同 0） |
   | 相邻 `agent.done` 正文逐字相同 | 3（同 `ref` 1 / `ref` 不同 2） | 0 |
   | **采用：相邻 `agent.done` 且同 `ref` 且同文** | **1**（`44952 → 44958`，即遗留 ② 记的未覆盖面） | **0** |
   | `ref` 不同的合法同文回复（应排除） | 2 组：`39563`/`39566`、`39588`/`39591` | 0 |

   （算法：取 `type in ('agent.done','agent.blocked','agent.idle')` 且 `compact_history_json` 里 `lastAssistantMessage.text` 非空的行，按 `(agent_id, herdr_session_name)` 分组、按 id 排序后两两比相邻两条，全库 2634 条；10-01 窗口那 21 对全是 `done(from=working) → idle` 形（18 对 `idle/done`、2 对 `idle/working`、1 对 `idle/unknown`），无一对是 `done → done`。）
5. confirmed 且 `expectedText` 与 `advanced` 文本确实不一致、且内容未曾交付时，按裁定仍放行正文（W18 钉住）；该语义与遗留 ① 的修复强相关，① 落地后需要回看这一条是否收紧。
6. 「读取早于落盘」变体（pi 侧 `message_end` 早于 `#appendMessage` 落盘）：靠 `#waitForHistoryAdvance` 在预算内看到新 `ref`/`text` 才放行（见机理二，**不是** 8×1500ms 兜底）；若窗口内正文始终未落盘，confirmed 轮次仍会空正文（现在至少有 warn）。上一版把它归到 46 条里的 7 条属于不可复现口径，已废弃；pi 扩展侧的写入顺序未改动。

## 来源

- 本批修复（worker 任务 `[MARK-FIX2-EMPTY-WAKE-BODY]`，第二轮）：`src/observability/agent-index-service.ts`（pi 终态分支：`staleBaselineDuplicate` 守卫 + 落空 reason）、`test/integration/turn-completion-signal.test.ts`（W14b / W16 / W17 / W18）。第一轮（任务 `[MARK-FIX-EMPTY-WAKE-BODY]`）为同一分支的判据一 / 判据二与 W14 / W15。
- 数字口径：口径 A 来自第二意见（oracle）独立统计（未独立复核）；口径 B 与本轮复核查询由 worker 用 `node:sqlite`（`readOnly: true`）对 `/root/.herdsman/state.db` 现场执行，见「数字口径」表。
- 收尾批（任务 `[MARK-COMMIT-EMPTY-WAKE-BODY]`）只改文字、不改行为：按第二意见修正 canary 口径、遗留覆盖面、机理二的毫秒口径与可复核来源，并把源码注释里「strictly the previous delivery」的夸大措辞改准。本轮重算的证据（探针时刻 2026-10-01T07:57Z）：口径 B 的 cutoff 回放到 `07:22:00Z` = 48、`07:37:26Z` = 52、`12:00Z` = 55（与「数字口径」表一致，属活窗口，引用须带 cutoff）；canary 基线、`39563`/`39566` 与 `39588`/`39591`、`44300`/`44315`、`44913`/`44952`/`44958` 的 ref / 文本 sha 均由本轮 `node:sqlite` 只读实测；`6444` / `18999` 用 `dist/src/agent-history/pi-reader.js` 复跑复核。注：任务书给的 scout 会话文件名 `…_01a0f60d-0106-76f6-b26739a7f31a.jsonl` 少了一段，实际是 `…_01a0f60d-0106-76f6-84be-b26739a7f31a.jsonl`。
- 相关历史笔记：`.agents/notes/2026-07-23-empty-wake-events-fix.md`（空文本守卫与 degraded 不可达化）、`.agents/notes/2026-07-23-fix-turn-completion-gate.md`（stopReason gate 与快路径 advancement gate）、`.agents/notes/20260930-phase1-delivery-latency.md`（空正文放行无信号的观察项 ④）。

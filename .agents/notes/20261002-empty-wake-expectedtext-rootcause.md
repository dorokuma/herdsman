---
status: active # active | superseded
superseded_by: ""
supersedes: ""
# 模块可选值: observability, daemon, cli, config, db, herdr, shared, herdsman-pi, herdsman-herdr-plugin, release
模块: observability
---

# 空唤醒（空 `agent.done`）的 expectedText 根因与四项修复：盘上正文为权威

## 一句话结论

pi 终态回合出现「正文为空的 `agent.done`」有两条并存的根因：**① daemon 侧**在 `expected_text_mismatch` / `non_terminal_assistant` 分支把已落盘的终态正文无条件抹成 `null`；**② 扩展侧**上报的 `expectedText` 是 **sanitize 之后**的文本，而 `message_end` 之后仍有改写型扩展继续改写落盘正文，二者**系统性不一致**。旧扩展又把「文件尺寸变大」当确认，让这类不一致被「确认」而非被拦住。本批四项修复：daemon 采信盘上正文（①）、扩展改文本级确认（②）、快照写入外键防守（③）、`failed` 唤醒带出正文（④）。**这类空回传与 herdr 侧 herdsman 插件无关**（该插件只做展示，已另行修好）。

## 背景

- 现象：编排者收到内容为空的完成唤醒（`last assistant` 为空 / `(no assistant message)`），子代理实际产出却在磁盘上。
- 触发条件（跨模块）：daemon（`src/observability/**`）与 Pi extension（`packages/herdsman-pi/src/**`）之间的 `agent.turn.completed` 协议，加上 Herdr 侧持久化计划行（`status_event_plans.compact_history_json`）。
- 关联前序笔记：`.agents/notes/20261001-pi-confirmed-turn-frozen-baseline-empty-body.md`（冻结基线导致的 confirmed 空正文，判据一/二/三）。本篇是它的后续：那一篇解决「基线即答案被判成未推进」，本篇解决「expectedText 与盘上正文不一致被判成 mismatch 然后抹空」。

## 根因

**根因 ①（daemon 侧无条件抹空）。** `src/observability/agent-index-service.ts` 的 pi 终态分支在 `#waitForHistoryAdvance` 之后，对 `!advancedMatchesExpected`（`!expectedText || advancedText.endsWith(expectedText)` 不成立）走 `expected_text_mismatch`、对非终态/空末尾走 `non_terminal_assistant`，两臂都执行 `compactHistory = { ...advanced, lastAssistantMessage: null }` 再 `degradeOrRelease(...)` —— **无条件**把盘上已经落定的终态正文抹掉。
- 实锤（**本批独立复核**，2026-10-02T11:53Z 重测，可复算）：事件 **50549**（`created_at` = 1790927143153 = **2026-10-02T07:45:43.153Z**）投递出去的是**空正文** `agent.done`（`agent_events.compact_history_json.lastAssistantMessage = null`，payload 只有 `staleSnapshot: false`、无 `degradedReason`），而 journal 同刻（07:45:43.152828Z，planId 24233）的 warn 是 `Herdsman released a confirmed pi status event with no deliverable text { degradedReason: 'expected_text_mismatch' }`，紧随 `emitted pi agent.done after turn completion signal (confirmed=true)`。**事件前盘上最后一条终态 assistant 是 entry `dea4443d`**（`stopReason: "stop"`，ts 2026-10-02T07:45:42.953Z，**5374 字符 / 7678 字节**，正文首行 `## Completed`），距事件 `created_at` **200ms**（距 warn 199.8ms）。即「盘上已有非空终态正文、且就是事件时刻的盘况，投递出去却是空的」。
- **订正（此前两处说法均错）**：① 此前引用的 entry **`7ac7b132`（2555 字符 / 3817 字节）不是事件前的正文**，而是事件**之后**下一轮的终态正文——其 ts = 07:46:57.355Z，在事件**之后 74.202s**，不可能「事件前就在盘上」；② 此前笔记写的「5374 是**字节**、被误记为字」是**反的**：**5374 是 `dea4443d` 的字符数，它的字节数是 7678**；2555 则是 `7ac7b132` 的字符数。③ 旧「47ms」的来源已查明：拿 journal 的**秒级**时间戳 07:45:43.000 减 07:45:42.953 = 47ms；对事件 `created_at`（…43.153）实为 **200ms**。复核结论与 oracle 一致（200ms / `dea4443d` / 7678 字节 / `7ac7b132` 在事件后 74.2s），与旧文本冲突的两处已按上文订正。
- **并证否「那是计划行冻结基线」这一替代解释**：`status_event_plans` id=24233 的基线 `compact_history_json.updatedAt` = 2026-10-02T07:38:38.543Z（= 该轮之前最后一条 user message 的时间），而事件里 compact history 的 `updatedAt` = **07:45:42.953Z**（= `dea4443d`）⇒ 事件用的是事件时刻的**新读**，盘上末尾确为该条终态正文（旁证：同一 compact history 的 `lastToolResult.ref` = `…#entry=9d8b3048`、`lastUserMessage.ref` = `…#entry=86009140`，分别正是事件时刻文件里最后一条 toolResult（5806 字符）与最后一条 user message（「继续」，07:38:38.543Z））。
- 复核方法（可复算）：`sqlite3 -readonly /root/.herdsman/state.db` 取该行 payload／compact history；`journalctl -u herdsman.service -o short-iso-precise --since 2026-10-02T07:45:41Z --until 2026-10-02T07:45:45Z` 取 warn；按 `historyRef.path` 逐行解析 pi 会话 JSONL（183 行），取事件 ts 之前最后一条 `stopReason: "stop"` 的 assistant 条目并算字符数（`len(text)`）与字节数（`len(text.encode("utf-8"))`）。

**根因 ②（扩展侧 expectedText 被加工过，盘上原文才是权威）。** `packages/herdsman-pi/src/index.ts`（约 `:1299-1317`）里 `assistantMessageText(message)` 返回的是 `sanitizeText(text).text`，`signalTurnCompletion(expectedText)` 上报的就是这个 **sanitize 之后**的文本。而在 `message_end` **之后**，改写型扩展仍会继续改写落盘正文（例：`no-tables` 把 `### X` 改成粗体 `**X**`、把表格改成 bullet、以及链接处理）。于是：
- 客户端上报的 `expectedText` 与磁盘原文**系统性不一致**（不是偶发抖动）；
- 磁盘原文是权威正文，`expectedText` 只是客户端的一次猜测。

**让 mismatch 得以发生/被确认的次要机制。** 旧扩展 `packages/herdsman-pi/src/turn-signal.ts` 用 `size > initialSize`（文件字节数变大）当作「新内容已落盘」即 `confirmed: true, reason:"new_content"`。尺寸级确认把「文件因为别的东西变大」也当成「本轮文本已写入」，因此 `confirmed=true` 与「盘上末尾并非 expectedText」可以同时成立，mismatch 被带进 daemon 的已确认分支，再被根因 ① 抹空。生产观测（**口径存疑，勿直接引用**）：曾观测到「48 个 pi 终态信号中 **45/48（94%）** 命中 `textMatches=false`」，但该数的**分子 / 分母 / 样本集均未复现**——怀疑分母是「空正文事件集」而非全部信号（以「已出空正文」为条件回看，天然接近全中）。**可复现的替代数**（2026-10-02 11:09Z 实测）：

- **轮级 mismatch/adopt 路径占比 ≈ 29%~43%**（近 24h 终态行口径）；
- **尾部窗口改写产物率 0.43%（表格产物，精确下界）~ 1.5%（全类型上界）**（与本文「已知残余」的 0.4%~1.5% 一致）；
- **末条消息全文（非仅尾部 200 字符窗口）含改写产物 12.7%（表格）~ 35.3%（全类型上界）**。

无论取哪个口径，「expectedText 与盘上正文不符」都是**系统性**现象，不是偶发抖动。

**明确排除。** 这类空回传与 herdr 侧 herdsman 插件（`packages/herdsman-herdr-plugin`，只做展示）**无关**；该插件的展示问题已另行修好。

## 决策（本批四项修复 + 回归测试）

**① daemon 采信盘上正文（`src/observability/agent-index-service.ts`）**：在已推进分支引入/强化 `confirmedDeliverable` 闸门；当回合已被扩展确认、且盘上末尾是**非空终态 assistant**、且不属于陈旧基线重复时，直接 `compactHistory = advanced`（保留盘上正文），**不置 `degraded`**（`degraded: true` 会触发 `#runPlanRow` 的 `invalidateById(..., "degraded_retry")`，把已写内容再废一遍）。不匹配仍以 `console.warn("Herdsman accepted a confirmed pi status event despite an expectedText mismatch", { degradedReason: "expected_text_mismatch", … })` 留痕，并仅在「mismatch 但采信」这一支写 `payloadExtra = { staleSnapshot: false }`。`non_terminal_assistant` 的含义保持不变：只有非终态/空末尾才降级。

**② 扩展改文本级确认（`packages/herdsman-pi/src/turn-signal.ts`）**：删除尺寸增长确认与 `new_content` 原因；`confirmSessionWrite` 只在文件 tail 窗口内**包含 `expectedText`（或其 JSONL 转义形态）**时返回 `{ confirmed: true, reason: "already_written" }`，否则 `{ confirmed: false, reason: "timeout" | "unavailable" }`。JSONL 转义形态（`JSON.stringify(candidate).slice(1, -1)`）必须保留，否则含换行的多行末条正文将永远无法匹配。根因注释按「客户端文本被加工 / 盘上原文权威」改写，并注明 daemon mismatch 臂现已采信盘上正文。

**③ 快照写入外键防守（`src/db/agents.ts` + `src/observability/agent-context-service.ts`）**：`agent_context_snapshots.agent_id` 引用 `agents(id)`；pane/agent 行可能在 daemon 仍在 drain 持久化计划行时被物理删除（journal 18:17:10 `status event plan drain rejected rows { rejected: 1, errors: [...] }`），插入会以 `FOREIGN KEY constraint failed` 把**整批 drain**一起打挂。新增 `AgentStore.exists(id)`（非抛异常存在性探针），`AgentContextService` 在 agent 行不存在、或插入撞 `SQLITE_CONSTRAINT_FOREIGNKEY`（`errcode === 787`）时，降级为**内存快照**（`inMemorySnapshot(next)`，payload 相同、时间戳取当前时间）并 `console.warn("Herdsman skipped agent context snapshot write for a deleted agent", …)`。删除的 agent 反正读不回来，降级只影响该条快照、不再连坐其它行。

**④ `failed` 唤醒带出正文（`packages/herdsman-pi/src/wake.ts`）**：`failed` 分支原先只输出 `reason:` 行，失败轮已有的正文对编排者不可见。现补 `body` 行；但 `failed` / `discarded` 的正文**来自计划基线快照**（`#appendPlanFailedEvent` 传 `plan.compactHistory`、`#appendPlanDiscardedEvent` 同理），**可能不是本轮产出**，故标注为 `last assistant (pre-round): …`，避免被读成「本轮产出」。无正文时输出与改前**逐字一致**（只有 `reason` 行）。行前缀（两空格缩进）与解析协议不变。

**回归测试清单**

- `test/integration/turn-completion-signal.test.ts`：`50549: a confirmed turn keeps the disk body when the signal expectedText mismatches`（① 命中：确认轮 + expectedText 不符 → 保留盘上 `m2` 正文、`staleSnapshot:false`、无 `degraded`、`console.warn` 命中 `expectedText_mismatch`、事件仍 `deliverable:1 / pending`）。
- `test/integration/agent-context-service.test.ts`：`degrades to an in-memory snapshot when the agent row is deleted before the write`（③ 命中：行删除后 refresh 返回内存快照、`agentContextSnapshots.get(...)` 为 `undefined`、warn 命中 `deleted agent`）。
- `test/unit/herdsman-pi-turn-signal.test.ts`：`does not confirm when the file only grows without the expected text`（② 尺寸不再算确认）、`matches a JSONL-escaped tail so a multi-line message still confirms`（② 转义形态保留），以及本批新增两条语义钉死用例（见下）。
- `test/unit/herdsman-pi-wake.test.ts`：`carries a failed outcome's existing assistant body and omits the line without one`（④：正文行出现、无正文时无 `last assistant`），并把既有 failed 断言同步到 `(pre-round)` 标注。

**本批新增的两条语义钉死测试（`test/unit/herdsman-pi-turn-signal.test.ts`）**

1. `never matches when a rewrite-type extension rewrote the tail window`：构造「尾部 200 字符窗口内含被改写内容（`### X` → `**X**` 形态）」的场景，断言扩展**如实**返回 `{ confirmed: false, reason: "timeout" }`。把「文本级确认面对改写必然失配」从**隐性实现属性固定成有测试的预期行为**。
2. `does not throw when the 200-char cut splits a 4-byte emoji surrogate pair`：构造 `expectedText` > 200 字符、`slice(-200)` 的切点落在 4 字节 emoji 的 UTF-16 代理对中间的场景；断言**不抛异常**，并记录当前行为（这里候选仍以孤立低位代理与原 tail 逐字节命中，故为 `already_written`；`timeout` 亦属可接受）。只固定「不崩」这条底线，具体 reason 允许将来随候选裁剪改为按码点而变。

## 已知残余与后续方向

**扩展的文本级确认必然 never-match（本批无法消除）。** 当 `expectedText` 的尾部窗口内被改写型扩展改写（或正文 ≤ 200 字符、任意位置被改写时整段即候选）时，文本级确认**必然失配** ⇒ 这类回合 `confirmed: false` → 走降级/重试。而 `confirmed` 是 `confirmedDeliverable` 的**硬合取项**，所以 **① 的救回对这类回合无效**。估算量级：约 **0.4%~1.5% 的回合**（按每天轮数折算为**每天个位数**）。

**另一条 never-match 来源：凭据脱敏（本批新登记，独立于改写）。** `packages/herdsman-pi/src/index.ts:1300-1304` 的 `assistantMessageText` 返回的是 `sanitizeText(text).text`，而 `packages/herdsman-pi/src/sanitize-text.ts:26-42` 会把 `Authorization: Bearer …`、`Bearer …`、`token=` / `password=` / `secret=` / `api_key=` 的值以及 `sk-…` 一律换成 `[REDACTED]`（`redacted` 标志无人消费，`index.ts:1355` 直接把该文本喂给 `signalTurnCompletion`）。盘上 JSONL 存的是**原文**，而 `expectedText` 带的是 `[REDACTED]`；`turn-signal.ts:96-104` 的匹配是「文件尾部 8000 字符里是否 `includes` 候选」（候选 = 尾部 200 字符，正文 ≤200 字符时即全文，含其 JSONL 转义形态）⇒ 被脱敏的字面量一旦落在**候选窗口**内（尾部 200 字符内，或正文 ≤200 字符时的任意位置），文本级确认**必然 never-match**，与改写型扩展无关（脱敏发生在 sanitize 阶段，早于任何扩展改写）。**特别注意它未被上面 0.4%~1.5% 那个口径覆盖**：0.43%~1.5% 只统计「尾部窗口内的改写产物（表格等）」，凭据字面量不在该统计规则里 ⇒ 真实 never-match 率是「改写 + 脱敏」两类之和，会高于 1.5% 上界（本批未定量）。同样地，`confirmed` 是 `confirmedDeliverable` 的硬合取项，这一类也靠 ① 救不回，只能等 M2。

**两个候选后续方案**
- **M2（oracle 判为终局方向）**：daemon 侧把 mismatch 臂的 `confirmed` 闸门**降级为「信号已到」**——只要收到过 turn signal（不必是文本级 confirmed），就以**盘上正文为权威**采信，把 `confirmed` 从硬合取项里松绑。这样即使扩展 never-match 也不会丢正文。
- **M1（仅在 ① 已部署上线后才可加）**：扩展侧在文本匹配失败时**退回尺寸增长判定**（旧行为作为兜底）。之所以必须**排在 ① 之后**：在 ① 上线前退回尺寸确认，等于恢复「mismatch 被确认然后被抹空」的老链路。

**当前选择：先放行 + canary，视度量结果再定**（M1 / M2 都不在本批落地）。按 (a) 口径，**canary 只决定紧急度**（提前 / 推迟 M2），**M2 是计划内终局**；见「canary 判据」的升级条件。

## 部署顺序与回滚口径

**部署顺序**
1. **daemon 先**：装 **0.13.5**（`PATH=/root/.nvm/versions/node/v22.23.1/bin:$PATH npm install --global @dorokuma/herdsman@0.13.5`）+ `systemctl restart herdsman.service`，令 ① 生效。
2. 再谈 **M1 / M2**（本批不做）。
3. **扩展无需部署**：pi 扩展直接从本工作树 `packages/herdsman-pi/src/**` 加载，改动对新会话即时生效。

**回滚**
- **daemon**：钉版本装回**上一个完整发布版本** + `systemctl restart herdsman.service`。
- **扩展**：`git checkout -- packages/herdsman-pi/src/turn-signal.ts packages/herdsman-pi/src/wake.ts` 在 0.13.5 之后是**空操作**（已证伪）：这两个文件当前 worktree hash == HEAD hash（`9ab8294f…` / `f7f5912b…`），HEAD 已含修复，该 checkout 只会把文件还原成同一份内容。要真正回滚必须**指名修复前的提交**：`git checkout 821fee5 -- packages/herdsman-pi/src/turn-signal.ts packages/herdsman-pi/src/wake.ts`（`821fee5` = `chore(release): 0.13.4`，即 0.13.4 的发布提交；其版本 hash 为 `4de06d4c…` / `dd951d00…`，与 HEAD 不同），或从 0.13.4 的发布物里取对应文件。**注意：该操作会在 main 上留下脏工作树**（两个文件相对 HEAD 处于已修改状态）——必须**显式提交或显式记录**这次回滚，否则 `git status --porcelain` 类门禁会挂、也没人知道运行时扩展已被回退。**回滚后必须重跑下面的 hash 断言**（否则你无法区分「回滚生效」与「什么都没发生」，后者正是 `git checkout --` 在 HEAD 含修复时给人的假安全感）。
- **验收必须额外断言「worktree 里扩展文件 hash == 发布 tag 里对应文件的 hash」**。否则会出现「npm 包回滚了、行为没回滚」——因为扩展按工作树加载，包回滚并不会改变运行中的扩展行为。校验式（示意）：`git hash-object packages/herdsman-pi/src/turn-signal.ts` 与 `git show v<tag>:packages/herdsman-pi/src/turn-signal.ts | git hash-object --stdin` 必须相等（`wake.ts` 同理）。
- **再加一条更硬的断言**：`git status --porcelain packages/herdsman-pi` **必须为空**——发布之后只要有人动过扩展源，发布物与运行时行为就脱钩（扩展按工作树加载，且**不会**随 npm 包回滚），这条能把「回滚 / 热修留下脏工作树」直接暴露出来。两条一起看：第一条管「内容对不对」，第二条管「发布后有没有人动过」。

## canary 判据（四个信号 + 阈值）

观察窗口：**至少 24 小时或 50 个完成轮次**（取先到者后再看一次）。

**分母与基线（实测时间 2026-10-02T11:53Z；上一轮记的是 2026-10-02 11:09Z）**：分母 = **近 24h pi 终态轮次 318**（口径 = `agent_events.type='agent.done'` ∧ `json_extract(payload_json,'$.agent')='pi'`；上一轮记的 327 就是同一口径在 11:09Z 的读数）；同一窗口全部 `agent.done` = **350**（48h：pi 613 / 全部 689）。**`blocked` 实测为 0 行**（`agent_events` 里 `type='blocked'` 与 `payload.to='blocked'` 都不存在，`status_event_plans.to_status` 也没有 `blocked`）⇒ 「`agent.done` + `blocked`」这个分母在本库实际就等于 `agent.done`。四条信号及其基线：

1. **① 救回量**：journal 计数 `accepted a confirmed pi status event despite an expectedText mismatch`。**基线 0 条**（① 未上线，该计数在 ① 发布前按构造恒为 0；本次实测 journal 全时段 = 0 条）。预期上线后**不是小数**：mismatch/adopt 路径的**轮级占比实测 ≈ 29%~43%**，所以 warn 会接近「每几轮一条」，**别当异常告警**。
2. **② mismatch 降级量**：`agent_events` 里 `payload.degradedReason=expected_text_mismatch` 的条数，**基线 0/24h、0/48h**（全时段 4）。① 上线后预期 = 改写产物率 **0.43%~1.5%** × 轮数（折合每天个位数）；**注意该上界未计入凭据脱敏类 never-match**（见「已知残余」）。
3. **③ 重试压力**：**② 的条数（`degradedReason=expected_text_mismatch`）+ `degraded_retry` 相对基线的变化**，以及「重试→补投」时延 **P95**。**⚠️ 口径订正：旧记法「基线 `degraded_retry` 17/24h（≈5.3% 轮次）」是错的量纲，已废弃——③ 不是 24h 累计计数，而是「近约 1h 内作废、且尚未被清扫的 `degraded_retry` 行数」**（口径 = `select count(*) from agent_events where invalidated_reason='degraded_retry'`，**无窗口条件**）。依据是本批自己到代码里核到的三处（不照抄任何转述）：清扫年龄 `INVALIDATED_GRACE_MS = 60 * 60 * 1000`（`src/daemon/agent-event-reconciler.ts:30`，调用在 `:169`）；删除语句在 `src/db/agent-events.ts:362` 的 `deleteInvalidatedOlderThan`（SQL 在 `:367`：`status = 'invalidated' and deliverable = 0 and coalesce(last_attempt_at, created_at) < now − 1h`）；删除只发生在 reconcile tick 上（`RECONCILE_INTERVAL_MS = 15 * 60 * 1000`，`src/daemon/service.ts:42`）⇒ **真实窗口是 `(1h, 1h15m]`，不是 24h**。且清扫会**停摆**：`reconcile()` 在 Herdr session list 不可用（`src/daemon/agent-event-reconciler.ts:87-88`）或 pane 快照不完整（`:116-117`）时提前 `return`，1h 与 7d 两档 TTL 都不跑 ⇒ 读数**可以超过「近 1h 生成量」**（那是清扫欠账，不是回归）。**旧记法的错处是分子≈1h、分母=24h 的量纲错配，不得再当阈值基准**（结构佐证：本批 2026-10-02T13:51Z 实测存活 3 行，年龄 12.4 / 51.4 / 62.9 分钟——最老一行已越过 1h，正等下一次 tick 清扫）。**要跨时间窗比较请改用持久口径**：`payload.degradedReason` 非空行（终态行保留 7 天，`src/db/agent-events.ts:378` 的 `deleteSettledOlderThan`）与 journal `Herdsman emitted degraded status event`（`src/observability/agent-index-service.ts:1754`，行内带 `degradedReason` / `attempts` / `planId`，可按 `attempts` 分首轮与重试）。**持久基线实测（固定窗口左端 `2026-10-01T12:35:34Z`；**读数时刻不是同一时刻**：`151 / 87` → `2026-10-02T12:38:54Z`；`60 / 341 / 21 / 83` → `12:44:30Z`；`146` 为**同期交叉读数**（非独立基线）；完整口径与命令见 `.agents/notes/20261002-release-0.13.5.md` 的 canary 节）**：`payload.degradedReason` 非空**降级行 151**；**未作废的 `agent.status.changed` 降级行 87（≈26%，每次降级尝试 1 行，不是终态降级轮次）**；**终态降级轮次（`type='agent.done'` 且 `degradedReason` 非空）60/341（≈17.6%；分母 = 同左端的终态轮次，**随读数时刻滑动**，实测 336–341）——其 `invalidated` 成分活不过约 1h（同上 1h TTL），故 60 是**被 1h TTL 截断的下限**（`acked` 成分受 7 天 TTL 保护）；**distinct `pane_id` 21**；**NULL `agent_id` 83**（`12:44:30Z` 那批；`count(distinct agent_id)` 忽略 NULL 会塌成 **5**，**勿用**）；journal **首轮（`attempts: 0`）降级 warn 146（≈43%，同期交叉读数）**。同左端事后重算必然**更小**（本批 2026-10-02T13:51Z 读到 134 / 84 / 50）——差值属 1h 保留窗口效应，**不是改善**。
4. **④ 空正文签名（按 journal 分组，不按字段）**：对 journal 分组 `Herdsman released a confirmed pi status event with no deliverable text`，**按其 `degradedReason` 字段分组**，**目标 = `expected_text_mismatch` 组趋 0**；`stale_baseline_duplicate` / `no_advance_from_input` 组**不是恒定地板**——扩展改成文本级确认（②）后，过去被「尺寸增长」假确认、走 confirmed 放行的轮次现在会 `confirmed=false` **落入降级** ⇒ 这两组**可能上移**，必须与**部署后自身基线**比，**别与部署前的 20 + 2 直接比**（口径依据：`.agents/notes/20261002-release-0.13.5.md` 的「遗留登记」第 2 条）。该签名（不分组合计）基线 **23/24h = 23/48h = journal 全时段 23**（最早一条 2026-10-01T15:01:16.722Z，全部落在近 24h 内；journal 覆盖起点 = 2026-09-29T01:50:07Z，daemon 启动）；**构成 = `no_advance_from_input` 20 / `stale_baseline_duplicate` 2 / `expected_text_mismatch` 1**。DB 侧同口径（`type='agent.done'` ∧ `compact_history_json.lastAssistantMessage IS NULL` ∧ payload 无 `degradedReason`）在近 24h 也是 **23 条**，与 journal 的 23 条**逐条对上**（时间戳差 ≤2ms，如 event 50549 的 07:45:43.153Z vs warn 07:45:43.152828Z）；DB 全时段 358，其中 **266 条早于 journal 覆盖起点**（DB 最早一条在 2026-09-25T13:25:28Z）。

**④ 为何不能沿用「旧空正文签名趋 0」**：`staleSnapshot=false` + 空 body + 无 `degradedReason` 这一签名**也被保留分支产出**——confirmed + `staleBaselineDuplicate`、confirmed + 空终态末尾，而这些分支的 payload **不落 `degradedReason`**。所以整体签名**不可达 0**，原写法会长期误报；只能分组后只盯 `expected_text_mismatch` 组。

**③ 为何废弃绝对阈值**：③ 的既有构成（全时段，实测 2026-10-02T11:53Z）按 `payload.degradedReason` 分组 = `no_advance_from_input` **286**、`non_terminal_assistant` **128**、`expected_text_mismatch` **4**；若只取空正文 `agent.done`（`compact_history_json.lastAssistantMessage IS NULL`）则为 `no_advance_from_input` **101**、`non_terminal_assistant` **55**、`expected_text_mismatch` **2**（上一轮记的 98/53/2 就是后一个口径）。**这两组构成数本身无误，保留。** **量纲订正**：旧文写的「地板 17/24h（≈5.3%）」是**分子≈1h、分母=24h** 的错配（见上条 ③ 的口径订正），**真实降级比例远高于 5.3%**——按持久口径，同窗**首轮（`attempts: 0`）降级 warn ≈43%**、**终态降级轮次 ≥17.6%**。因此**绝对阈值必须废弃**的理由订正为「**真实地板本身已是两位数百分比**」（而不是「地板 5.3% 高于任何小比例阈值」），只能看**相对基线的变化**。

**升级条件（按 (a) 口径）**：

- **(a) 本文采用**：**把 M2 改述为「计划内终局」**——M2 本就在计划内，canary 只决定**紧急度**（提前 / 推迟），不是「超阈值才启动」。触发**提前**的观察：② 从 0 抬头、④ 的 `expected_text_mismatch` 组连续不为 0，或出现「重试耗尽 → `agent.failed reason=degraded`」且可归因于 mismatch。
- **(b) 备选硬阈值**：若要数值门槛，写 **> 2%**（显著超出上界 1.5%），且只对 ②/④ 生效。**原「② 或 ③ 的比例 > 0.5% 轮次」已删除**：它低于真实地板（首轮 warn ≈43% / 终态 ≥17.6%），按字面执行会**自触发误判**。

**度量陷阱**：
- `confirmed: true` **且文本匹配**的正常路径**不写** `staleSnapshot: false`（只有「mismatch 被采信」这一支才写）⇒ **不要**拿 `staleSnapshot` 字段当度量标尺，否则会把正常轮次误算成异常或反之。度量 ① 请直接数 journal 的 warn 文案。
- ④ 必须**按 `degradedReason` 分组**计数；整体计数含非恒定地板（见上），不能当回归信号。
- 判断「是否已投递给编排者」**不要用 `deliverable` 列**（ack 后清零；`deliverable=1` 的**终态行（`done`/`blocked`）全时段 2 行，全表实测 45 行**——后者多为未 ack 的 pending 行）；见遗留 9。

## mismatch 类的路径转移（重要语义防误读）

**0.13.5 并没有让 mismatch 消失，而是把它从 ④（空投递）搬到了 ② / 降级重试路径。** 本版之后，**② 口径的 `payload` 计数**（`payload.degradedReason='expected_text_mismatch'`）只可能来自 unconfirmed 轮：`src/observability/agent-index-service.ts:2063-2065` 的 `else if (!advancedMatchesExpected)` 仍把正文置 `null` 并 `degradeOrRelease("expected_text_mismatch")`；而 **confirmed 轮**改走 ① 的 `console.warn("Herdsman accepted a confirmed pi status event despite an expectedText mismatch", …)`（`:2048-2061`），随后 `:2062` `compactHistory = advanced` ⇒ **保留盘上正文**、且不置 `degraded`（范围写法 `:2048-2062` 即含这次赋值）。

**④ 的签名** `released a confirmed pi status event with no deliverable text`（`src/observability/agent-index-service.ts:1945`，全仓唯一发射点）**只收 confirmed 轮**；confirmed 轮若在其它合取项（`staleBaselineDuplicate`／非终态／空末尾）上失配，仍会沿 `:2063-2065` 被贴上 `expected_text_mismatch` 标签——那是**臂序产物**（`:2066` 的 `non_terminal_assistant` 只在 `advancedMatchesExpected=true` 时可达），不是失配诊断。

因此 **「④ 的 `expected_text_mismatch` 组归零」≠「失配问题解决」**——它只是不再以「空投递」形态出现。要 ② 真正归零，必须等 **M2**：daemon 侧把 mismatch 臂的 `confirmed` 闸门降级为「信号已到」，连 **unconfirmed 轮的盘上正文**也一并采信；在那之前，unconfirmed 轮的失配仍会落进 ② 的降级/重试路径（详见 `.agents/notes/20261002-release-0.13.5.md` 的「语义纠正」段）。

## 被放弃的方案（必填）

- **保持尺寸增长确认（把 M1 提前到现在做）**：在 ① 上线前退回尺寸判定，等于恢复「mismatch 被确认 → 被抹空」的老链路，救不了任何东西。放弃；M1 推迟到 ① 上线之后。
- **让 `expectedText` 改用未 sanitize 的原文**：sanitize 是防注入/去控制字符的既有职责，且它也不是失配的充分原因（改写型扩展在 sanitize 之后照样改写落盘文本）。放弃。
- **扩展侧对 `expectedText` 做规范化后再比对**：无法预知所有改写规则（`no-tables` 等），规范化只能覆盖已知形态，反而制造新的假确认。放弃。
- **`failed` 唤醒维持 reason-only（不输出正文）**：会把失败轮的已有上下文一起藏掉，编排者更难判断，反面教材。放弃。
- **给 `failed` 正文保留原 `last assistant:` 标注**：会诱导编排者把基线快照读成本轮产出。放弃，改 `(pre-round)`。
- **在 wire payload 里持久标记「mismatch 但采信」**：本批最小改动面内不做（列为候选改进，见遗留）。放弃（本批）。
- **更新计划行基线（`compact_history_json` 后续 UPDATE）**：与本批无关，且已在 `20261001-pi-confirmed-turn-frozen-baseline-empty-body.md` 的「被放弃的方案」中放弃。

## 遗留 / 观察项

1. **`staleBaselineDuplicate` 覆盖面不足**：守卫只比对**最近一条未失效终态行**；按**实际代码路径**，错轮投递需三条件同时成立：**（i）尾部 200 字符与 8KB tail 内某条旧消息的尾部相同；（ii）daemon 读到的 `advanced` 恰是那条旧消息；（iii）那条旧正文从未被投递过（故不在 `latestTerminal` 里）**。（原写的「转换在工具阶段被处理 + 5s 内收到信号」与代码不符，已删。）当前样本里未测到反例。建议**最小硬化** = 扩展为「该 agent 是否**曾投递过**同 `ref` 的终态行」，**本批不做**。
2. **`inMemorySnapshot` 不落库**：③ 的降级快照只是内存态，tombstone 期间可能**重复 refresh 与 warn 刷日志**。行为正确（无落库、无连坐），列 canary 观察项。
3. **`failed` 正文来源标注已修**：`last assistant (pre-round): …`，无正文时逐字不变。遗留一个语义缺口：wire 记录不带 assistant `ref`，无法在扩展侧判定「基线 ref 与失败轮 ref 相同」的同源情形；`(pre-round)` 是保守但诚实的标注（如需同源优化，需要在 daemon 侧扩 payload，本批不做）。
4. **测试缺口（短文本 `already_written` 语义）留待**：正文 ≤200 字符时的候选即全文，其 `already_written` 语义未被专门钉死。
5. **payload 里暂无「mismatch 但采信」的持久标记**：该事实只活在 journal warn，**跨重启不可查** ⇒ 记为候选改进（例如在 payload 增设只读诊断位）。
6. **历史残留（与本批无关）**：`docs/plans/2026-07-14-herdsman-test-dogfooding.md:32` 的 wrapper 描述（"installed `herdsman` wrapper executes this checkout's `dist/src/cli/herdsman.js`"）已过时；该条**已在** `.agents/notes/20261001-production-install-channel-npm-registry.md`（遗留 ⑦）登记，**此处不重复，仅指向**。
7. **never-match 轮次现在要付满 3s 确认超时**：文本级确认（②）在 never-match 时必须**等满 3s 才回 `confirmed: false`**（旧实现一有尺寸增长即返回）⇒ 这类轮次比改前**多约 3s** 延迟，且会体现在「重试→补投」**P95** 里。**读 canary 时别把这 3s 当回归**，尤其别只看 P95 单点。
8. **`formatHiddenAgentUpdates` 会原样复发误读（遗留，本批不改）**：`packages/herdsman-pi/src/index.ts:1551`（函数，实际跨度 `:1551-1568`；渲染行为 `:1563`）对 `failed` / `discarded` 事件**仍无条件渲染不带标注的 `last assistant:`**（与 ④ 已在 `wake.ts` 加的 `(pre-round)` 口径不一致），且当前**无生产调用者**（`rg` 全 `src` 与 `packages/herdsman-pi/src` 仅 `:1551` 定义；只有 `test/unit/herdsman-pi-extension.test.ts` 引用）。将来若被接线，会原样复发「把计划基线快照读成本轮产出」的误读。
9. **canary 查询不要用 `deliverable` 列做「是否已投递给编排者」的过滤**：`agent_events.deliverable` 在 ack 后被**清零**，**终态行（`done`/`blocked`）全时段仅 2 行；全表实测 45 行，多为未 ack 的 `agent.status.changed`（28）与 `agent.idle`（14）pending 行，最早可追溯到 2026-08-27** ⇒ 用它筛会几乎筛空。判断投递状态请用 ack / 投递相关字段或 journal 文案，不要用 `deliverable`。
10. **批外待回填：老笔记仍把该不可复现数字当事实引用**：`.agents/notes/20261001-pi-confirmed-turn-frozen-baseline-empty-body.md:57,63` 仍直接引用「45/48 命中 `textMatches=false`」且未带口径标注；建议日后修订为「口径不可复现、勿直接引用」或改为指向本篇笔记。**不影响运行时行为，本批不回填。**

## 来源

- 本批任务：`[MARK-IMPL2-EMPTY-WAKE]`（第二实现批：注释措辞 / `failed` 标注 / 两条语义钉死测试 / 本篇笔记）；第一实现批（同一未提交分支 `fix/empty-wake-delivery`，9 个文件）落地 ①②③④ 的源码与回归测试。
- 本篇 canary 段的阈值 / 口径修正：`[MARK-NOTE-CANARY-FIX]`（第三批，**仅改本篇笔记**，未动任何源码/测试）：④ 改按 journal `degradedReason` 分组、③ 改相对基线、废弃「> 0.5%」自触发阈值、补登 3 条新观察。
- 第四批（本批，工作分支 `fix/test-flock-handshake-and-canary-notes`）：`[MARK-BATCH1-TESTFIX-NOTES]`（本篇笔记部分未动任何源码；同批测试修复见末句）：③ 的「`degraded_retry` 17/24h」口径订正为 1h 残留窗口 + 持久替代口径与持久基线、③ 为何废弃绝对阈值的量纲订正（真实地板为两位数百分比）、新增「mismatch 类的路径转移（重要语义防误读）」小节、④ 的 `stale` / `no_advance` 组由「恒定地板」改为「非恒定地板、须与部署后自身基线比」（按 `.agents/notes/20261002-release-0.13.5.md` 对齐）。同批另修 `test/unit/daemon-process-manager.test.ts` 的 flaky 握手等待（与本篇笔记无关）。
- 根因 ① 的实锤：**本批已独立复核并订正**（事件 50549 `created_at` = 2026-10-02T07:45:43.153Z；事件前最后一条盘上终态正文 = entry `dea4443d`（`stopReason:"stop"`、07:45:42.953Z、5374 字符 / 7678 字节）；事件后 74.202s 才落盘的 `7ac7b132`（2555 字符 / 3817 字节）不属事件前盘况，旧文本的「前 47ms / 2555 字符」已按上文订正）。复核方法、命令与两条口径对照（journal + DB）均写入本篇「根因」「canary 判据」两节。
- oracle 旧观察「45/48 命中 `textMatches=false`」：**分子/分母/样本集未复现**（怀疑分母是「空正文事件集」），已在上文标注为**不可直接引用**，并给出可复现替代数（轮级 mismatch/adopt 占比 ≈29%~43%、尾部窗口改写产物率 0.43%~1.5%、末条消息全文含改写产物 12.7%~35.3%）。
- canary 基线：**本批重测刷新**（测量时间 **2026-10-02T11:53Z**；分母 pi 318 / 全部 `agent.done` 350，`blocked` 0 行 / ① 0 / ② 0（全时段 4）/ ③ 即时残留 17（1h 窗口，不可当 24h 读；持久基线见 `.agents/notes/20261002-release-0.13.5.md` 的 canary 节）/ ④ 23（= 48h = journal 全时段；构成 20+2+1）/ DB 全时段空正文 358 / ③ 构成按 `payload.degradedReason` 为 286+128+4、按空正文为 101+55+2 / `deliverable=1` 终态行 2 行、全表 45 行（口径见遗留 9））；上一轮记的 2026-10-02 11:09Z 版本（327 / 0 / 12 / 23 / 358 / 98+53+2）已被上文取代，此处仅作对照。
- 根因 ② 的源码位置：`packages/herdsman-pi/src/index.ts` 约 `:1299-1317`（`assistantMessageText` → `sanitizeText`）；`packages/herdsman-pi/src/turn-signal.ts`（旧 `new_content` 尺寸确认，本批已删）。
- 相关笔记：`.agents/notes/20261001-pi-confirmed-turn-frozen-baseline-empty-body.md`（冻结基线空正文、判据一/二/三）、`.agents/notes/20261001-production-install-channel-npm-registry.md`（生产安装渠道、wrapper 过时条目）、`.agents/notes/20260930-terminal-event-delivery-open-items.md`（投递时延与空正文观察）。
- 部署渠道依据：项目 AGENTS.md「生产部署渠道（唯一，registry）」，daemon 装 `@dorokuma/herdsman@<version>` + `systemctl restart herdsman.service`。

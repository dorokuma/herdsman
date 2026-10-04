## [Unreleased]

## 0.14.2

- 删除 `HERDSMAN AGENT CONTEXT` 预览块（`packages/herdsman-pi`，`ce316b5`）：`pi.on("context")` 不再向编排者上下文注入 `herdsman-agent-context` 自定义消息。该预览块把每个 agent 的报告经 `truncateSummary(..., 100)` 截略成单行（tabTitle 60 字符上限）后随 context 注入，设计意图是「速览」，实际反复造成「报告被截断」的误判——审计确认数据从未丢失（wake 全文轨始终投递完整报告），预览块信息价值为零，盘面状态本应实时查 herdr。同步删除仅服务该块的死代码：`formatHiddenAgentContext`、`truncateSummary`、`formatTimestamp`、`HerdsmanState.pinnedContext` 及只为它服务的 `retain` 快照求交；被 wake 侧复用的 `formatHiddenAgentUpdates`、`sanitizeAndCleanContextText` / `cleanContextText` 与 wake.ts `normalizeExcerpt` / `formatAgentOutcomeUpdates` 保留，全文投递路径零改动。`context` 钩子保留为入口清洗：重放旧会话时把 herdsman 自己的旧 context/wake 条目从传入列表过滤掉（`herdsman-wake-context` 唤醒全文本体不过滤，它是 turn 消费的证据）。测试：删除 22 个引用预览块/pinnedContext 的用例（17 个预览格式 + 3 个 cached-context pinning + `context intersection regressions` describe 4 个），改写 2 个（ack 排序用例去掉预览断言、busy-defer 用例改为断言 context 钩子零注入），其余用例未动；用例总数 852 → 830。
- busy 唤醒投递单轨化（`packages/herdsman-pi`，`0a6a8a2`）：busy 路径不再经 `context` 钩子钉 `herdsman-wake-queued` 第二份副本，子代理回传只走 follow-up 消息一条轨（busy = 排队 `triggerTurn:false`，idle = `triggerTurn:true`），按 eventId 升序、一条不丢、一条不重。
- 续传预算按事件独立记账（`packages/herdsman-pi`，`fea08a4`）：驱动计数器由单一共享值改为按 event id 记账的 `wakeContinuationDrives`，任一事件总驱动次数硬上界 ≤5 后必走 write-off，新事件注入不再刷新滞留事件的预算（修复同一事件被全量刷屏的线上空转循环）。
- 版本与引用同步：四个 manifest（`package.json`、`packages/herdsman-pi/package.json`、`packages/herdsman-herdr-plugin/package.json`、`packages/herdsman-herdr-plugin/herdr-plugin.toml`）同步至 0.14.2；`README.md` 与 `packages/herdsman-herdr-plugin/README.md` 的 Herdr 安装 tag 同步至 `v0.14.2`。
- 范围：本版本包含 0.14.1 之后的全部内容（`fea08a4` 续传预算 + `0a6a8a2` 单轨化 + `ce316b5` 预览块删除，及相应留痕笔记）；仅 `packages/herdsman-pi/src/index.ts`、`test/unit/herdsman-pi-extension.test.ts` 与 `.agents/notes/` 有改动，`packages/herdsman-herdr-plugin` 与 `src/**` 无源码改动；无 schema 变更、无迁移。

## 0.14.1

- 幽灵唤醒循环修复（`packages/herdsman-pi`，`2bad14f`；配套留痕 `5d6d6b9`），四项：① 续传预算（`MAX_WAKE_CONTINUATION_ATTEMPTS` = 5 次）用尽后 `writeOffStrandedWakeDelivery`——滞留事件 id 退出 `wakeAwaitingConsumption` 并走普通路径 ack，治「永不 ack、daemon 幽灵重投」；② 显式关页（`pane.closed`）隐含消费——已投递行在 `#invalidatePaneCore` 步骤 0 直接 ack，不再走关页保留，从未投递行仍保留一次投递机会；③ wake 摘要与 wake/上下文正文归一化保留换行与行内缩进（只折叠 3+ 连续换行、去行尾空白），治「截断假象」；④ `REDELIVERY_FRESHNESS_MS` 维持 300s 未动。涉及 `packages/herdsman-pi/src/index.ts`、`packages/herdsman-pi/src/wake.ts`、`src/db/agent-events.ts`、`src/observability/agent-index-service.ts`。
- 测试：`test/unit/event-dedup-pane-generation.test.ts` 新增关页保留/丢弃与死信回填用例；`test/unit/herdsman-pi-extension.test.ts`、`test/unit/herdsman-pi-wake.test.ts` 覆盖 write-off、隐含消费 ack 与正文归一化。用例总数 847 → 850。
- 已知残余（诚实说明，详见 `.agents/notes/2026-10-04-ghost-wake-fix-followups.md`）：write-off notify 文案仍写「交给另一个终端让 daemon 重投」，与 write-off 已同步 ack 的新行为不符，留待下个版本改文案；write-off ack 在存在 id 更小的 pending 行时可能以 `ORCHESTRATOR_EVENT_OUT_OF_ORDER` 失败，退化为「永久静默 churn」（唯一信号是一行 warn），上线后观察该 warn 是否出现；保留换行后含大代码块的更新会全量进入 wake 投影文本，token 成本高于折叠为一行的旧行为。
- 版本与引用同步：四个 manifest（`package.json`、`packages/herdsman-pi/package.json`、`packages/herdsman-herdr-plugin/package.json`、`packages/herdsman-herdr-plugin/herdr-plugin.toml`）同步至 0.14.1；`README.md` 与 `packages/herdsman-herdr-plugin/README.md` 的 Herdr 安装 tag 同步至 `v0.14.1`。
- 范围：本版本包含 0.14.0 之后的全部内容（`2bad14f` pi 包修复 + `5d6d6b9` 双审观察留痕笔记）；`packages/herdsman-pi/src` 与 `src/db/agent-events.ts`、`src/observability/agent-index-service.ts`、`test/unit/**` 均有改动，`packages/herdsman-herdr-plugin` 无源码改动；无 schema 变更、无迁移。

## 0.14.0

- root 包 agent 历史支持面收窄为「只解析 pi 与 agy」（`bd97dd2`）：pi 走官方 `agent_session.kind === "path"` 直用通路；agy 走官方 id → `conversations/<uuid>.db` 单一模板；其余 agent（claude/codex/opencode/grok/gemini/omp）一律不解析、不读取。删除 `claude-reader.ts` / `codex-reader.ts` / `opencode-reader.ts` / `grok-reader.ts` / `gemini-reader.ts` 五个 reader 及相关模板，以及旧扫盘、mtime 排行、终端标题猜目录等兜底。显式读取不再回退持久 ref（`preferredHistoryRef` 已删）：官方值瞬时不可读时返回空并打一条 warn（诚实失败、可立即重试）；负缓存只降频后台常规刷新，`agent.get` / `agent.read` 与 `forceRefresh:true` 调用点恒绕过。omp 按设计不打 warn（官方给 `path`，discovery 层解析成功、reader 层拒绝）。`agents.grok_home` 遗留列恒写 `null` 且无读取方，删列需 drizzle migration，本批不做。
- 升级影响（本版本删除了一项对外可见的历史读取能力）：升级后，claude / codex / opencode / grok / gemini / omp 这些 agent 的 pane 不再有历史可读——`herdsman agent get` 与 `herdsman agent read` 对它们返回空历史；其中以上报 id 的 agent，日志会附一条 warn 说明该 agent 不受支持，而以上报 path 的 agent（如 omp）不产生该 warn。不再回退落盘缓存、也不再扫盘或按终端标题猜目录，官方会话值瞬时不可读时如实返回空、可立即重试。支持面收窄为 pi 与 agy 两类（与 README 的 "supports exactly two agents" 一致）。若按 patch 发布（`0.13.7`），`^0.13.x` 的 caret 安装者会在普通升级中被静默带入这次删除；因此本版本改发 minor `0.14.0`，使其落在 `^0.13.x` 范围之外，避免静默升级。
- pi 扩展唤醒投递以消费证据为准（`packages/herdsman-pi`，`1ad8a55`）：只有隐藏唤醒消息的 `message_end`（`details.presentedEventIds`）证明内容已进 transcript，缺该证据的消息只留 warn、不确认任何 id（宽表 `details.eventIds` 不再当证据）；确认只推进交付队列的连续可确认前缀（id 升序、单调水印，跳过未确认的更小 id 会把它永久标成 acked）；已有消费证据的 id 即使该轮以 error 结束也照常确认；续跑限次 3 次并在用尽时给出用户可见且可操作的提示；role/scope 复位时未消费在途 id 转入跨 scope 抑制集合，被拦截重投落限流日志；批次已结算但队列仍有滞留项时 settlement 仍尝试结算（覆盖断连期间内容已消费、重连后无新事件的情形）。
- 版本与引用同步：四个 manifest（`package.json`、`packages/herdsman-pi/package.json`、`packages/herdsman-herdr-plugin/package.json`、`packages/herdsman-herdr-plugin/herdr-plugin.toml`）同步至 0.14.0；`README.md` 与 `packages/herdsman-herdr-plugin/README.md` 的 Herdr 安装 tag 同步至 `v0.14.0`。
- 范围：本版本包含 0.13.6 之后的全部内容（`bd97dd2` root 包 + `1ad8a55` pi 包）；`src/**` 与 `packages/herdsman-pi/src/index.ts` 均有改动，`packages/herdsman-herdr-plugin` 无源码改动；无 schema 变更、无迁移。

## 0.13.6

- daemon 侧「采信盘上正文」的判据与 `confirmed` 解耦（M2，`src/observability/agent-index-service.ts`）：此前 pi 终态回合只有在 `turn.confirmed === true` 时才可能采信盘上正文——`confirmed=false` 且上报的 `expectedText` 与盘上尾部不符（扩展文本级确认 never-match：尾部被改写型扩展改写、或多行/凭据脱敏形态）时，即便盘上已有合法终态正文也会被抹成 `null` 并 `degradeOrRelease("expected_text_mismatch")`，投出空 `agent.done`（0.13.5 只救了 confirmed 轮）。本版把「是否 confirmed」从**采信判据**里松绑为「信号已到 + 盘上有可交付正文」：外层 `if (turn?.received)` 已保证信号到达，`diskDeliverable = isTerminalAssistant(advanced) && hasNonEmptyAssistantMessage(advanced) && !staleBaselineDuplicate`（`:2102`），命中即采信盘上正文、不置 `degraded`（置了会让 `#runPlanRow` 对已写内容再 `invalidateById(..., "degraded_retry")` 废一遍）。`confirmed` 只保留给 released/degraded 的**日志与载荷语义**：④ 的 `Herdsman released a confirmed pi status event with no deliverable text`（`:1971`）仍**只在** confirmed 轮发射；① 的 mismatch warn（`:2108-2121`）现在分 `confirmed` / `unconfirmed` 两个字符串——`Herdsman accepted a confirmed pi status event despite an expectedText mismatch` 与 `Herdsman accepted an unconfirmed pi status event despite an expectedText mismatch`，两条都含子串 `despite an expectedText mismatch`，载荷新增 `confirmed` 字段，`payloadExtra` 仍写 `{ staleSnapshot: false }`（仅 mismatch 但采信这一支）。
- `staleBaselineDuplicate` 守卫加宽（`src/observability/agent-index-service.ts:122`、`:2040-2060`）：旧守卫只比对「该 agent 最近一条未失效终态行」，M2 从**单条**扩到「该 agent 最近 **≤200 条**可投递终态行」（常量 `STALE_DUPLICATE_GUARD_SCAN_LIMIT = 200`），逐条 `sameTerminalAssistantContent(..., "pi")` 比对；查询参数化，只在 `#waitForHistoryAdvance` 之后的兜底分支执行（`order by id desc limit 200`，读窗口内 JSON 几百 KB），快路径不跑。三点必须写清：① **为什么是 200**：`agent_events` 的 settled TTL 为 7 天，窗口内单个 agent 的已投递/acked 终态行实测上限 **249**（「around 250」），守卫关心的正文总是最近某次投递，200 留了余量又把读量收在几百 KB；② **>200 行的漏拦**：更旧正文若在尾部复现，会被再次当本轮答案放行——这是「有界扫描换确定性」（不遍历该 agent 全部终态行）的**既定代价**；③ **口径变更**：新查询只统计 `status in ('delivered','acked')` 的行——7d 窗口内带 ref 的 `agent.idle` 行 **1075** 条，其中 **981 条 `agent_id` 为空**（守卫按 `agent_id = ?` 查，看不到它们）；守卫作用域内只有 **94** 条、其中 **93** 条在守卫窗口内，**~50% of ref-bearing `agent.idle` rows within the guard's scope (`agent_id` non-null: 47/94) carry the same ref as a `done` row of the same agent; they are never deliverable.**（非 `working` 起点那 49 条里有 46 条如此）；若把它们计入窗口，会与同 ref 的可交付轮争同一尾部（构成比约一半），存在抹空可交付轮的风险；never deliverable 仅对其中非 working 起点的 49 条成立。因此**新守卫对从未投递的行拦得比旧版少、不再是旧逻辑的严格超集**；同一变量 `staleBaselineDuplicate` 同时被「基线未推进」臂（`:2065-2069` 的 `confirmedDeliverable`）消费，口径变更对该臂同样生效。
- 测试：`test/integration/turn-completion-signal.test.ts` 新增 unconfirmed + mismatch 采信盘上正文（`:1294`）、无合法终态正文仍降级（`:1384`）、已投递同 ref 不重复投递（branch B，护栏性质，**仅覆盖 unconfirmed + mismatch** 的情形，改前后皆过，`:2235`）、守卫扩窗（`:2148`）；`test/integration/agent-index-service.test.ts` 改写 S7（`:4483`，由「unconfirmed mismatch ⇒ 降级」对齐为「⇒ 采信盘上正文」）与 S8（`:4560`，首轮改非终态工具轮，以保住「降级→重试→完成」状态机覆盖）。用例总数 **847 → 851**。同版本另含 flaky 测试握手修复（`test/unit/daemon-process-manager.test.ts`：读 holder PID 前轮询到文件就绪，消除并行下的 `ENOENT`）与根因笔记 canary 口径回写（`.agents/notes/20261002-empty-wake-expectedtext-rootcause.md`）。
- 已知残余（诚实说明）：① 本版目标格（`confirmed=false` 且 mismatch）在现网极稀：可覆盖窗口（2026-09-29T01:50:07Z→2026-10-02T15:29Z，≈3.57 天）内 `confirmed=false` 仅 **1** 条（同期 `confirmed=true` **841** 条），且该轮盘上有 **6186** 字非空终态正文、无 `degradedReason` ⇒ **不在目标格**，观测到的 M2 收益 = 0。M2 影响面很小；② 空正文大头**本版未触及**——pi 空正文 24h **73/279 ≈26%**（构成：`no_advance_from_input` **38**（含 acked 35 / invalidated 3）+ 无 reason **22** + `non_terminal_assistant` **13**；无 reason 与 `non_terminal_assistant` 都不在 M2 射程），M2 只覆盖 mismatch 那一格；③ 守卫扩窗可能使 journal ④ 的 `stale_baseline_duplicate` 组**合法上移**（基线 **2/24h**）：过去被尺寸假确认、走 confirmed 放行的轮次现在会落进该组；④ **未加索引**——`(agent_id, herdr_session_name, id)` 的部分索引留待后续批次；⑤ 窗口查询仍**内联在 service**（`AgentEventStore.listRecentTerminalHistories` 的沉淀留待重构）；⑥ **无 schema/migration、无扩展侧改动**（`src/**` 仅 `agent-index-service.ts`，`packages/**` 无源码改动）。
- canary 读法（M2 生效后怎么读）：
  - **机制信号**：`journalctl -u herdsman.service --since <重启时刻> -o cat | grep -c "accepted an unconfirmed pi status event despite an expectedText mismatch"`（新串）；**取值时把 confirmed 与 unconfirmed 两串合计看**（`grep -c "despite an expectedText mismatch"`），因为 confirmed 串本身在本窗口也是 0（本机 24h 与全窗口两串均 0，2026-10-02T15:36Z 实测）。分母看同窗口 `after turn completion signal (confirmed=false)`。
  - **伤害读数**（必须与**部署后自身基线**比，别与部署前直接比）：journal ④ 按 `degradedReason` 分组——`no_advance_from_input` 基线 **19/24h**、`stale_baseline_duplicate` 基线 **2/24h**、`expected_text_mismatch` 基线 **1/24h**（合计 **22/24h**）；`agent.failed reason=degraded` 基线 **75–77/24h**（日噪声约 ±8/24h ⇒ ±2 量级的变化不可分辨）；pi 空正文占比基线 **73/279 ≈26%**（构成：`no_advance_from_input` 38 + 无 reason 22 + `non_terminal_assistant` 13）。
  - **窗口**：目标格在可覆盖窗口内 **0–1 条/3.57 天（≈0.28 轮/24h）**⇒ 7 天（≈2000 轮）期望命中仅 **≈2 条**，仍判不了「有效率」。canary 只做两件事：确认机制被走到 + 确认没有量级级伤害。
  - **会被误读成「变好」的假象**：②/④ 的 `expected_text_mismatch` 组趋 0（② 不是恒 0、④ 受臂序影响）、③ 的 1h 锯齿（清扫欠账，非回归）、空正文占比下降（大头 `no_advance_from_input` 不在 M2 射程）、**新串（机制信号）在数天内为 0 属预期**，不能据此判 M2 未生效；判「有没有换版本」只看 `npm ls -g` / `MainPID` / 重启时刻。**M2 之后空正文占比可能微升**——守卫扩窗把部分「带正文放行」换成「抹空」，那不是失败。另：「已投递正文被抹空」在 mismatch 臂里挂的是 `expected_text_mismatch`（臂序：该分支先于 `non_terminal_assistant`），只有未推进臂才挂 `stale_baseline_duplicate`；② 的 `no_advance_from_input` 组（基线 38/24h）与 ④ 的 stale 组是同一变量（未推进臂）的两面，任何一组单独下降都不能读成「修好了」。
  - **回滚触发阈值**（与部署后自身 24h 基线比、连续两天成立才动）：④ stale 组 > 基线+10；④ `no_advance_from_input`+stale 合计 > 基线×1.5；pi 空正文占比 >35%（基线 ≈26%）；`agent.failed reason=degraded` > 基线×1.5（本机基线 75–77/24h）；或出现「同一 ref 的正文跨轮重复投递」的实证。**0.13.5 笔记 (d) 的「出现 `agent.failed reason=degraded` 即变差」不可沿用**——本机基线 75–77/24h，照字面读会每日自触发。
- 回滚口径：**最便宜 = 先不发布**；已部署后钉版本装回上一完整发布版本 —— `PATH=/root/.nvm/versions/node/v22.23.1/bin:$PATH npm install --global @dorokuma/herdsman@0.13.5`（**包名是 `@dorokuma/herdsman`**）+ `systemctl restart herdsman.service`，随后核版本 / `MainPID` / `herdsman daemon status` / `herdsman agent list` / 新串归零 / 优雅关停 `exitCode: 0`；**无需 DB 回退**（无 schema 变更；期间 acked 行不会被重投）。**回滚不会纠正 M2 已经投出的内容**（那些行已 acked、正文已被编排者消费），只止住后续；若真出现「旧正文被当本轮答案」，需在该轮另行说明/标注。
- 版本与引用同步：四个 manifest（`package.json`、`packages/herdsman-pi/package.json`、`packages/herdsman-herdr-plugin/package.json`、`packages/herdsman-herdr-plugin/herdr-plugin.toml`）同步至 0.13.6；`README.md` 与 `packages/herdsman-herdr-plugin/README.md` 的 Herdr 安装 tag 同步至 `v0.13.6`。
- 范围：本版本包含 0.13.5 之后的全部内容（0.13.5 发布落地记录 + 根因笔记 canary 口径回写 + flaky 测试握手修复 + M2）；`src/**` 有改动（`src/observability/agent-index-service.ts`），**无 schema 变更、无迁移、无扩展侧改动**。

## 0.13.5

- 空唤醒（空 `agent.done`）修复（`ba18967`，合并 `874bf09`）——已确认轮次的盘上正文与唤醒文本不再被丢弃，四项。① daemon 采信盘上正文（`src/observability/agent-index-service.ts`）：pi 终态分支在回合已被扩展确认、盘上末尾是**非空终态 assistant** 正文时，不再因为上报的 `expectedText` 与之不符而把正文抹成 `null`（事件 50549 的形态：该轮投递出去的 `agent.done` 正文为空——`lastAssistantMessage` 为 `null`、journal 同刻 warn 的 `degradedReason` 是 `expected_text_mismatch`——而事件落库前 **200ms**，盘上已是 `stopReason: "stop"` 的非空终态正文：entry `dea4443d`，**5374 字符 / 7678 字节**，07:45:42.953Z；事件 `created_at` = 07:45:43.153Z。此前文字引用的 `7ac7b132`／2555 字符是事件**之后 74.2s** 才落盘的下一轮终态正文，不属于事件前盘况，已订正）；mismatch 仍以 `console.warn("Herdsman accepted a confirmed pi status event despite an expectedText mismatch", { degradedReason: "expected_text_mismatch", … })` 留痕，但**不置** `degraded`——置了会让 `#runPlanRow` 对已写入内容再 `invalidateById(..., "degraded_retry")` 废一遍；`non_terminal_assistant` 的含义不变：只有非终态/空末尾才降级。② pi 扩展落盘确认改为文本级（`packages/herdsman-pi/src/turn-signal.ts`）：删除「文件尺寸变大即视为落盘」这条假确认来源（连同 `new_content` 原因——它把「文件因别的东西变大」也当成「本轮文本已写入」，正是 mismatch 得以被「确认」的次要机制），只在文件 tail 窗口内包含 `expectedText` 或它的 JSONL 转义形态（`JSON.stringify(candidate).slice(1, -1)`，多行正文必须靠它才匹配得上）时返回 `{ confirmed: true, reason: "already_written" }`，到点如实返回 `{ confirmed: false }`（`timeout` / `unavailable`）。③ `agent_context_snapshots` 写入对「agent 行已删除」做外键防守（`src/db/agents.ts` + `src/observability/agent-context-service.ts`）：新增非抛异常的 `AgentStore.exists(id)`，agent 行不存在、或插入撞 `SQLITE_CONSTRAINT_FOREIGNKEY`（`errcode === 787`）时降级为内存快照（`inMemorySnapshot`，payload 相同、时间戳取当前时间）并 `console.warn("Herdsman skipped agent context snapshot write for a deleted agent", …)`，不再让一条 `FOREIGN KEY constraint failed` 把整批 drain 一起打坏。④ `failed` / `discarded` 唤醒带出已有正文（`packages/herdsman-pi/src/wake.ts`）：原先只输出 `reason:` 行，失败轮已有的正文对编排者不可见；现补 `last assistant (pre-round): …` 一行——标注 `(pre-round)` 是因为该正文来自计划基线快照（`plan.compactHistory`）、可能不是本轮产出，无正文时输出与改前**逐字一致**。
- 测试：四项修复各配「先 FAIL 后 PASS」回归用例（含复刻事件 50549 的 `50549: a confirmed turn keeps the disk body when the signal expectedText mismatches`，断言保留盘上正文、`staleSnapshot: false`、无 `degraded`、warn 命中 `expectedText_mismatch`），并新增两条语义钉死用例——尾部 200 字符窗口被改写型扩展改写时扩展**必然**如实返回 `confirmed: false`（把「文本级确认面对改写必然失配」从隐性实现属性固定成有测试的预期行为），以及 200 字符切片落在 4 字节 emoji 代理对中间时不抛异常。本版本 `pnpm check` 绿（53 files / 847 tests，node v22.23.1）。
- 已知残余（本版**不是**彻底修复）：文本级确认面对「尾部 200 字符窗口内被改写」——或正文 ≤200 字符时任意位置被改写——必然失配，这类轮次走降级/重试补投；实测该类回合量级约 0.4%~1.5%（折合每天个位数），且 `confirmed` 是 ① 的硬合取项，故 ① 救不回这类轮次，它们还要付满 3s 确认超时。后续方向：**M2**（daemon 侧把 mismatch 臂的 `confirmed` 闸门降级为「信号已到」、以盘上正文为权威）/ **M1**（扩展侧在文本匹配失败时退回尺寸兜底，**仅在 ① 部署之后**才可加，否则等于恢复老链路）。两者本版都不落地。
- 覆盖范围（诚实说明）：本版 ① 只修 `expected_text_mismatch` 这一臂；占比更大的 `no_advance_from_input` 臂本版**未改**，属后续工作（M2 方向）。窗口 = 2026-10-02 前后 48h 实测（测量时间 2026-10-02T11:53Z；journal 覆盖自 2026-09-29T01:50Z 起，窗口内 48h 与 24h 两条口径都是 23 条）：窗口内实测「空正文投递」样本 23 条，构成 = `no_advance_from_input` 20 / `stale_baseline_duplicate` 2 / `expected_text_mismatch` 1 ⇒ 本版覆盖到的只是其中 1 条（窗口内分母：pi 终态轮次 318 / 全部 `agent.done` 350）。
- 留痕：新增笔记 `.agents/notes/20261002-empty-wake-expectedtext-rootcause.md`（根因、四项修复、已知残余与 M1/M2、部署顺序与回滚口径、canary 四个信号与阈值）；同版本收录 0.13.4 发布落地记录（`2d0793e`）与 npm 账号姿态检查决策记录（`903eaf9`）。该轮空回传与 `packages/herdsman-herdr-plugin`（只做展示）无关。
- 范围：本版本包含 0.13.4 之后的全部内容（0.13.4 发布落地记录 + 账号姿态检查决策记录 + 上述修复及其回归测试与留痕笔记）；本版 `src/**` 有改动，但**无 schema 变更、无迁移**（`src/db/agents.ts` 只新增只读存在性探针 `exists()`）。
- 版本与引用同步：四个 manifest（`package.json`、`packages/herdsman-pi/package.json`、`packages/herdsman-herdr-plugin/package.json`、`packages/herdsman-herdr-plugin/herdr-plugin.toml`）同步至 0.13.5；`README.md` 与 `packages/herdsman-herdr-plugin/README.md` 的 Herdr 安装 tag 同步至 `v0.13.5`。

## 0.13.4

- 0.13.3 发布落地记录（`3f08e12`）：新增笔记 `.agents/notes/20261002-release-0.13.3.md`，登记 0.13.3 的发布落地事实（registry / git / Release 三面），并把两处待登记事项留在同一份记录里——`## Preconditions` 的 `HEAD = origin/main` 一条在 push 之前按构造不成立（属重跑产物，不是发布失败），以及 npm 账号页级检查（verified email / write 2FA）0.13.3 轮未做、记为用户决策。
- 发布流程文档缺口修补（`306ced6`，`docs/releasing.md`）：补 registry 传播时长口径——新版本在 `npm view` 上可见的时长不恒定，0.13.3 的 `@dorokuma/herdsman-pi` 曾在连续十次重试内保持 E404 才出现，按文档只重试 `npm view`、不 republish（重发会 E409）；并写明 `HEAD = origin/main` 检查的时点说明——该检查就地成立（发布开始时与 `git push origin main` 之后各一次），在发布提交之后、push 之前重跑整段 `## Preconditions` 会让它按构造为假，那是重跑的产物，不是跳过该门禁的理由。这两处即 0.13.3 笔记遗留清单里的两条。
- 版本与引用同步：四个 manifest（`package.json`、`packages/herdsman-pi/package.json`、`packages/herdsman-herdr-plugin/package.json`、`packages/herdsman-herdr-plugin/herdr-plugin.toml`）同步至 0.13.4；`README.md` 与 `packages/herdsman-herdr-plugin/README.md` 的 Herdr 安装 tag 指向 `v0.13.4`。
- 范围：本版本包含 0.13.3 之后的全部内容（0.13.3 发布落地记录 + 发布流程文档缺口修补）；`src/**` 零改动——无产品行为改动、无 schema 变更、无迁移。

## 0.13.3

- 测试稳定化（`105286f`）：把三处「固定 sleep 窗口当同步手段」改为轮询到条件真正落定——`test/unit/daemon-service.test.ts` 里两处「沉降须在一个宏任务 turn 内完成」的结构性假设改为 `vi.waitFor` 轮询（`interval: 10` / `timeout: 5_000`，每次尝试先 tick 再让出一个宏任务），`test/unit/daemon-process-manager.test.ts` 里 SIGKILL 之后那处固定 50ms「内核清理」延迟改为 `waitForCondition(() => !isFlockHeld(lockPath))`。三处屏障用例补 10s 用例级预算，使轮询自身的 5s 超时先报出谓词错误，而不是被默认 `Test timed out in 5000ms` 掩盖；`daemon-service` 的 D9 用例外层预算 30s → 45s（其两个内部等待串行、各上限 20s）。既有断言的 `expect` 行一字未改，只是移进轮询体。本版本 `pnpm check` 绿（53 files / 840 tests）。
- 补降级 release 屏障的覆盖（`0a1d6b7`）：`readChildPids` 读不到 `/proc/<pid>/task/<pid>/children` 时的 catch + 一次性 `console.warn` 分支此前无用例，新增两条用例——① 同一模块实例内 warn 恰好一次（两次成功 acquire 期间读失败，经 `resetModules` 取新的模块副本）；② 该文件不可读时 release 只等 flock 进程，文件可读的对照组等 flock 进程与共享 fd 的子进程，并带「seam 报出的状态字节为 `Z`」的承重自检（放宽该状态会让对照组短路通过）。注入用部分 `vi.mock("node:fs")`：`importOriginal` 保留真模块，只包装 `readFileSync` 且只对 children 路径形状生效，`failChildrenRead` 仅在两条用例内抬起。
- 测试硬化（`482ada0`）：无界等待加界——`child.on("exit")` 的三处（SIGKILL 屏障与两个锁竞争者）改为有界 `waitForCondition`，超时以具名描述报错而非裸 `Test timed out`；失败路径各自释放——降级用例里两个 flock handle 的 `release()` 移进 `finally`（`release()` 幂等），两并发进程用例与 200 轮压测用例把杀子进程补进 `finally`，`test/integration/herdr-socket-client.test.ts` 用例把 `controller.abort()` / `client.close()` 挪进 `finally`（连接不拆会让 `afterEach` 的 `server.close()` 撞 10s `hookTimeout`）；同文件那条原先被丢弃的 `Promise.race` 结果改为显式断言（50ms 内不得有事件），并把错误原因带进失败信息（`stream-error: <err>`）。断言只增不减，用例数不变。
- 台账卫生（`482ada0`，含 `105286f` / `0a1d6b7` 的追加）：`.agents/notes/20260930-terminal-event-delivery-open-items.md` 的 D12/D15/D17 段逐条处置 `/tmp` 锚点与引用数字（重定向到现树可复算锚点，或标注为「历史值 @ 提交」），把跨提交的锚点位移映射收进 D12.4 一处，登记「批次 → 提交」映射，并把 `pnpm check` 里的 `Bad substitution` 噪声归因（harness 固定 `/bin/sh -c` = dash）。
- 运维文档（`76e1beb`）：`AGENTS.md` 把生产部署钉到 registry 单一渠道——`PATH=/root/.nvm/versions/node/v22.23.1/bin:$PATH npm install --global @dorokuma/herdsman@<version>`（必须钉版本号、显式带 nvm PATH 前缀使 `npm prefix -g` 落在 nvm 前缀、`npm install` 只替换文件而 `systemctl restart` 之后才生效），部署前置改为「完整发布」（两个包都在 registry 上有对应版本且 `v<version>` tag 已推到远端，校验用两条 `npm view` 与 `git ls-remote --tags --exit-code`），「部署后核对」扩充为版本 / MainPID / socket / `agent list` / 优雅关停 journal 五项，回滚定义为同一渠道钉版本装回上一个完整发布版本；`README.md` 的生产小节点明包来自 npm registry、需匹配 systemd 单元所用的 Node 工具链，源码安装标注为仅本地开发用。新增笔记 `.agents/notes/20261001-production-install-channel-npm-registry.md`。
- 发布文档与记录（`84f7f4e`）：`docs/releasing.md` 补给五处缺口——发布提交的 `git add` 清单补上 `CHANGELOG.md`（原清单会让紧随的 `test -z "$(git status --porcelain)"` 必然失败）；写明 `npm profile get` 在自动化 token 下必然 E403、不构成失败判据（账号姿态不在自动门禁覆盖范围内）；写明 tag 必须在发布提交 push 之后创建；新增 `## Update the CHANGELOG` 段（发布段只写提交时刻已成立的事实、默认不写状态行并以 0.12.1 为先例、registry 侧事实移出发布提交、状态下标只保留给还没进发布提交的 bump）；补部分发布的恢复流程（`dist-tag` 回退与长期中止分支）。新增笔记 `.agents/notes/20261001-release-0.13.2-post-release-docs.md` 记录 0.13.2 的发布落地事实。
- 版本与引用同步：四个 manifest（`package.json`、`packages/herdsman-pi/package.json`、`packages/herdsman-herdr-plugin/package.json`、`packages/herdsman-herdr-plugin/herdr-plugin.toml`）同步至 0.13.3；`README.md` 与 `packages/herdsman-herdr-plugin/README.md` 的 Herdr 安装 tag 指向 `v0.13.3`。
- 范围：本版本包含 0.13.2 之后的全部内容（测试稳定化 + 运维/发布文档 + 台账卫生）；`src/**` 零改动——无产品行为改动、无 schema 变更、无迁移。

## 0.13.2

- pi 终态事件不再把已就位的最终正文抹空：`turn completion` 已确认（`confirmed === true`）且磁盘上有非空终态 assistant 时，不再因为「计划行基线已含本轮答案」（计划行的 `compact_history_json` 只在创建时写一次、之后不更新）而把正文置空、发出渲染成 `(no assistant message)` 的空 `agent.done`；该放行路径不再静默——原因经 `console.warn("Herdsman released a confirmed pi status event with no deliverable text", { agentId, degradedReason, herdrSessionName, paneId, planId, terminalId })` 落日志（只含非敏感运行标识），仍**不置** `degraded`（置了会让 `#runPlanRow` 对已写事件 `invalidateById(..., "degraded_retry")` 再废一遍）。同时新增陈旧重复守卫：待交付正文恰好等于**最近一条终态行的正文**（通常是上一轮已交付的正文）时改为拦下（`degradedReason = stale_baseline_duplicate`，同样 log-only），不再把上一轮结果当本轮新结果重复投递。无 schema 变更、无迁移、无新 payload 键；非 confirmed 分支、`expected_text_mismatch` / `non_terminal_assistant` 的既有语义不变。遗留（本版本不改 pi 扩展侧的落盘确认逻辑）：pi 扩展侧用文件字节数变大做落盘确认，`expectedText` 与磁盘正文会系统性不一致，本版本只把它当分支判据、不再当放行条件；未覆盖的路径（工具阶段处理转换 + 信号 + `expectedText` 缺失）仍可能在极端条件下重发旧正文。
- 测试：新增 W14 / W14b（基线已含答案时保留正文）、W15（放行理由进日志且仍非 degraded）、W16 / W17 / W18（陈旧重复守卫命中、不误伤、以及从未交付内容仍放行的既定语义）；`pnpm check` 绿（53 files / 838 tests）。

- `daemon status` 增加只读监督事实：默认 JSON 多两个**可选**键 `managedBy`（pid 的 `/proc/<pid>/cgroup` 是否落在 herdsman.service 系统单元内 → `systemd:herdsman.service` / `unmanaged` / `unknown`）与 `restartCount`（`systemctl show herdsman.service -p NRestarts --value`，仅在 `managedBy` 为该单元时才探测，其它情况省略该键），并新增人类可读视图 `herdsman daemon status --text`（拿不到的 `restartCount` 显式写 `unavailable`）。默认 stdout 仍是 JSON，既有字段、`state` 取值与退出码语义零变化。口径：`unmanaged` 只表示"不归 herdsman.service 系统单元管"，不代表没人托管、也不代表可以杀；`restartCount` 是 systemd 的 `NRestarts`，`reset-failed`、`stop`+`start`、单元未加载都会把它清零，所以 `0` 不等于"从没崩过"。探针超时是硬上界：`spawnSync` 带 `killSignal: "SIGKILL"`，会无视 SIGTERM 的子进程也会被终止（默认 SIGTERM 时可被卡死 shim 拖住 8s）。

- 终态事件投递生命周期修复（H1）：plan 走到终态时 `agents` 行可能已被物理删除（关页/退役），旧实现会因 `FOREIGN KEY constraint failed` 丢掉整条终态事件——现在 `#appendPlanFailedEvent` / `#appendPlanDiscardedEvent` 在 agent 行缺失时以 `agentId = null` 降级落库（原 id 保留在 payload），`AgentEventStore.append` 再加一层防御降级 + `console.warn`（不静默）；`nextDeliverableAfter` 的 SQL 与 JS guard 成对放行孤儿终态行，避免 `markAcked(id <= cursor)` 把未投递的孤儿行吞掉。保留语义统一为「保留到首次投递尝试为止」：`#invalidatePaneCore`、reconciler 的 `isRetainable`、`reclaimDelivered`（补 `delivery_attempts >= 1`）三处一致，跨代关页不再吞掉未投递的孤儿 failed 行（`genCondition` 移入 done/idle 分支），`agent.discarded` 不保留。stitch 改按 `(herdr_session_name, pane_id, pane_generation)` + `created_at <= plan.created_at` 匹配，旧失败不再挂到同 pane 的新实例。存量静默入库的孤儿失败行由 `#backfillFailedPlanEvents` / `#backfillDiscardedPlanEvents` 逐行 try/catch 回填（写完即 `markAcked`，只可查、不投递、不打扰当前 owner，并报 `backfilled/skipped/total`）。唤醒口径：`packages/herdsman-pi/src/wake.ts` 让 `agent.discarded` 也映射为 `kind: "failed"`（既有压制/死信语义不变）。死信与静默点可见化：`publishAgentEvent` 去掉 agentId 必需门槛（保留 `workspaceId` / `terminalId` 要求），缺投递范围时终态事件仍落库 + `console.warn`；`drainPendingPlans` 的 `allSettled` rejected 项与 `#scheduleWaitingHistoryRetry` 失败不再静默。无 schema 变更、无迁移。
- Phase 1 唤醒延迟与检测退避：检测段 `#waitForHistoryAdvance` 去掉指数退避（原 `500ms ×2` 上限 16000ms / 总预算 30000ms），改固定 200ms × 8 次、总预算 1500ms（「读不到新字节即视为该轮无新内容」的判定语义不变）；`turn.confirmed === true` 直通可投递终态——已确认的终态 assistant 落盘即投递，不再判 degraded、也不再 `invalidateById(event.id, "degraded_retry")` 作废已写入内容；新增 `retryRingAuthorized(error)` 不变式，四个重排点（含 `#runPlanRow` 的 `catch (PlanWaitingHistoryError)` 后门）改为「合成 degraded 只记原因、不挂 10s 重试环」。投递段有界化：删掉「busy → 置 `wakeDeferredUntilSettled` 后直接 return」的无定时器挂起，改 `scheduleDeferredWake`（100ms 有界自旋 + 5000ms 硬超时后按当前状态强制裁决放行）；`WAKE_SETTLE_MS` 500 → 0，空闲经 0ms 定时器直投。双轨队列：编排者空闲用 `deliverAs: "followUp"` + `triggerTurn: true` 直投；忙碌时不打断用户 turn，改投 `triggerTurn: false` 的 follow-up 并经 `pi.on("context")` 以 `customType: "herdsman-wake-queued"` 挂进当前可见上下文。批次不丢：新批并入（id 去重升序）而非替换未确认批，5s 硬超时只放行投递、绝不丢弃未确认事件（未确认事件留在队列等 ack/游标收敛）；ack 尝试计数改读活投影并于失败后回写队列，到 `MAX_ACK_ATTEMPTS` 才出死信。忙碌路径的批不再 abort 用户 turn（`hasSubstantiveWork` 计入 `orchestratorBusy`，`loseRole` / `resetForScopeChange` 两处 abort 门不再误伤骑在用户 turn 上的批）。降级原因进日志：`Herdsman emitted degraded status event` 携带 `planId / agentId / attempts / degradedReason / herdrSessionName / paneId`（只做可观测，不改行为）。
- daemon 操作锁存活判定不再把「读不到 `/proc`」当作进程已死：`isChildProcessActive` 对 `/proc/<pid>/stat` 读失败按 errno 分流——`ENOENT` / `ESRCH` 判死，其余（`EACCES` / `EPERM` / `EIO` / 未知）以及空读、截断一律按活（procfs 的 `st_size` 为 0，读可能被截断，「解析不出」不构成进程已死的证据）；消除高负载下「子进程已写出 READY（已持锁）→ 一次瞬时读失败 → 被 SIGKILL → 抛 `operation lock is held`」的假失败。`Z` / `X` 判死保留、`T` 本批不动；新增仅测试用的 `FlockHandleDependencies.readProcessStat` 缝（缺省与生产读法逐字等价）。
- daemon 操作锁 release 屏障改为等「全部共享 flock fd 的 helper 进程」消失：`flock` 会 fork 出执行命令的进程并继承 fd，内核只在最后一个持有该 fd 的进程消失后才释放锁，旧实现只等 flock 进程 ⇒ release 返回后锁仍可能被占；现在在 READY 时刻经 `/proc/<pid>/task/<pid>/children` 记下这组 pid，release 只判这组（100ms 预算不变），procfs 取不到 children 时回退旧行为并每进程 `console.warn` 一次（不静默降级）。acquire 侧在同一 1000ms 窗口内最多 4 次 spawn helper（间隔 1ms，单次 attempt 窗口夹紧到剩余预算），`owner.json` 指向活进程时不重试，消除 release → 立即 acquire 的假「锁被持有」。复现对照：修复前串行 10 轮 1 次 / 并发 20 轮 4 次锁失败，修复后 30 轮与独立 2 路并发 A/B 均 0 次。
- 子进程判活相位化：`isChildProcessActive` 增显式 `phase`（`"ready"` / `"pre-ready"`）参数——pre-READY 阶段（本 attempt 尚未观测到 READY）的 `T`（SIGSTOP/SIGTSTP）按「可放弃」处理，走既有「杀 + 重试」快路径；READY 建立之后 `T` 恒算活，不再杀掉仍持锁的 helper。只有 pre-READY 那一个调用点传 `"pre-ready"`，`Z` / `X` / `ENOENT` 判死路径、errno 分流、窗口预算与 attempt 结构零改动。
- 测试（同版本收录）：并行复跑稳定性——`test/integration/orchestrator-disconnect-grace.test.ts` 4 处把真实 `sleep(20)` 当同步手段改为轮询屏障（事件循环被饿死时旧写法会让虚拟定时器被孤立、owner 永不清），`observability-rpc.test.ts` 的一处 `sleep(75)` 改为轮询到重新认领成功，`rpc-test-client.ts` 的 `waitForNotification` 预算约 200ms → 2s；`daemon-service.test.ts` / `package-publication.test.ts` / `agent-index-service.test.ts` / `herdsman-pi-extension.test.ts` 各补 30s 单例预算（不动全局 `vitest.config.ts`）；`daemon-process-manager.test.ts` 的 200 轮锁压测超时抬到 30s（单跑约 1.4s、3 路并发 4–6.5s，会撞穿 vitest 默认 5s）。断言一律未放宽。
- 文档与笔记（同版本收录）：新增决策/取证笔记 7 篇（终态投递生命周期 H1、Phase 1 延迟、投递延迟观察、双审观察项与挂账台账、daemon 锁 release 屏障、`daemon status` 监督事实位、pi confirmed 基线空正文）；`0607eca` 为零代码改动的台账批次。
- 版本与引用同步：四个 manifest（`package.json`、`packages/herdsman-pi/package.json`、`packages/herdsman-herdr-plugin/package.json`、`packages/herdsman-herdr-plugin/herdr-plugin.toml`）同步至 0.13.2；两个 README 的 Herdr 安装 tag 随发布提交同步至本版本创建的 `v0.13.2`。
- 范围：本版本包含 0.13.0、0.13.1 与 0.13.2 的全部内容（0.13.0 与 0.13.1 都未打 tag、未单独发布，内容随 0.13.2 一起发布；其中 0.13.1 只做了仓库内版本递增）。
- **已发布**：0.13.2 发布到 npm 的两个包（`@dorokuma/herdsman`、`@dorokuma/herdsman-pi`），并创建 tag `v0.13.2`；npm 上的 latest 由 0.12.1 变为 0.13.2。本机生产用本地 tarball 安装本仓构建产物（不依赖 npm 发布），装后按部署流程重启，并用版本、PID、socket 逐条核对。

## 0.13.1

- daemon 优雅关停总预算：`stop()` 的关闭序列（index drain → reconcile scheduler → watch manager → RPC server）改为跑在一个 `SHUTDOWN_BUDGET_MS = 5000` 的总预算下，每步按剩余预算限时并保留 `SHUTDOWN_MIN_STEP_MS = 250` 兜底；超时只记 warn 并继续下一步、退出码仍为 0（SIGTERM 是有意停止，非零退出码会被 `Restart=on-failure` 拉回来），`finally` 的清理（pid 文件 / 实例锁 / socket）不受预算影响。修的事故形态是「任一步永不返回 → 10s 内不退出 → 被 `TimeoutStopSec=10` SIGKILL → 跳过清理并留下 flock 助手子进程」。
- 关停先 abort 再 drain：daemon 持有一个关停 `AbortController`，`stop()` 一开始即 abort——`agent-index-service` 的在飞状态等待（历史窗口 / turn completion / readiness polling）按「等价于 pane closed、plan 行保持可重试」结束，关停后新注册的 waiter 立即 abort；`herdr-session-watch-manager` 同步停 tick 并拆掉全部 watcher（`PLAN_DRAIN_GRACE_MS` 12000 → 3000，必须 ≤ 关闭预算），预算切掉 watchManager 那一步也不会留下活 watcher。
- 外围调用加超时与硬上限：`herdr-socket-client` 的请求加 `HERDR_REQUEST_TIMEOUT_MS = 2000`（超时=失败，成功路径语义不变），`herdr session list` 的 `execFile` 加 `HERDR_SESSION_LIST_TIMEOUT_MS = 2000` 并补 `killSignal: "SIGKILL"`（默认 SIGTERM 可被子进程无视，且迟到成功会被当成成功）。
- 关停日志与二次信号幂等：关停进入 / abort / 每步开始（剩余预算与限时）/ 每步结束（耗时）/ 超时 / 结束（总耗时与退出码）都有日志（只含步骤名、毫秒数与 `pid` / `exitCode` 这类非敏感运行标识）；第一次 SIGTERM 进入关停时重新挂上 SIGINT/SIGTERM，第二个信号做最小清理后 `exit(0)`，不再落到 Node 默认动作留下 pid / socket / 锁残留；在飞 tick 的 rejection 就地 `.catch`——关停预算截断 `watchManager.stop` 时 daemon 自己的 `unhandledRejection` 监听器已被移除，未处理的 rejection 会按 Node 默认行为终止进程并跳过清理。
- 回归测试：daemon 关停预算 / 超时跳过 / `finally` 清理 / 第二次信号，watch manager 收到 shutdown signal 立即拆 watcher 与在飞 tick 抛错不逃逸，socket client 对静默对端超时；隔离目录端到端复测：假 herdr socket 完全不应答时 SIGTERM 后 2.01s 优雅退出（exit 0），pid 文件 / RPC socket / 实例锁均清除。
- 文档批次（同版本收录）：提交门禁失败提示可区分（`.husky/pre-commit` 的 mise 缺失与工具未装分开提示）；`AGENTS.md` 与 `docs/releasing.md` 澄清运行时事实（生产 daemon 走 nvm 的 node v22.23.1、提交门禁走 mise 的 node 26.7.0、普通 shell 以现场 `node -v` 为准，部署前用生产面复跑 `pnpm check`）；`.agents/notes/` 的模块枚举补 `release`。
- 版本与引用同步：四个 manifest（`package.json`、`packages/herdsman-pi/package.json`、`packages/herdsman-herdr-plugin/package.json`、`packages/herdsman-herdr-plugin/herdr-plugin.toml`）同步至 0.13.1；两个 README 的 Herdr 安装 tag 保持指向已存在的 `v0.12.1`（`v0.13.1` 的 tag 尚未创建），tag 替换在发布提交时执行。
- **未发布**：仓库内版本号已递增到 0.13.1，但未打 tag、未发布到 npm；npm 上的 latest 仍是 0.12.1。本机生产用本地 tarball 安装本仓构建产物（不依赖 npm 发布），装后按部署流程重启，并用版本、PID、socket 逐条核对。

## 0.13.0

- CLI 接口移除：删除 `herdsman daemon start|stop|restart`，`herdsman daemon` 只保留只读 `status`；生产启停只走 systemd（`herdsman.service` 是唯一托管者），socket 报错文案改指 `systemctl status herdsman.service`。
- daemon 启动行为回归简单：入口不再带“是否被 systemd 托管”的策略判断，只做参数校验后启动；同一数据目录同时只允许一个实例，仍由实例锁保证（flock，先拿锁再打开数据库），被拒的进程完全不碰数据库。锁被占用时的提示改指 `systemctl status herdsman.service`。
- wake 投递链修复（r4f1）：保留行恢复两道取件闸门（放行 `deliverable=1` 的 invalidated 保留行，并去掉会排除保留行的 `agent_id is not null` 过滤），关页外键置空后仍可取件并当场 Ack 收敛；状态计划耗尽不再落到静默 `discarded`，改为保留 `degraded` 原因并走 `agent.failed` 兜底事件；`executeStatusEventPlan` 只返回事件，推流统一由调用方转发，消除 socket 上的重复 `agent.event`。
- observability 加固：migration 0010 以单调 `when` 注册进 `_journal.json`，既有库经 `applyMigrations` 高水位跳过正常升级；空 wake 处理与状态计划收口（`#drainPlanRow` 事务去重、空 `compactHistory` 守卫、降级事件 `invalidateById`、`markRunning` 返回 boolean 由调用方收敛 CAS/重试）。
- 测试：新增保留行关页存活与取件回归、耗尽兜底回归、S1-S4 断言、真实升级路径回归与 `when` 单调性断言；两条 flock 用例改为轮询到条件成立 + 明确超时，不再靠固定 sleep。
- 仓库清理：移除上游项目痕迹（README / metadata / docs 索引与包校验脚本同步上游移除，删除 NOTICE 与 README.ja.md，LICENSE / CHANGELOG / 归档文档白名单保持不动）。
- 工具链对齐：`AGENTS.md` 与 `docs/releasing.md` 的 PATH 示例改为 mise 的 node `26.7.0` 与 pnpm 安装目录本身（mise 的 pnpm 安装目录没有 `bin/`），并写清本机运行时事实：生产 daemon 由 systemd 单元固定用 nvm 的 node v22.23.1 启动；提交门禁把 mise 钉版前置，实际跑 mise 的 node 26.7.0 与 pnpm 11.9.0；普通 shell 以现场 `node -v` 为准。支持面与生产面是 node 22，因此部署/发布前要求用 nvm 的 v22.23.1 复跑一次 `pnpm check`。`.husky/pre-commit` 的 mise 定位改为带错误检查的写法（`|| { echo …; exit 1; }`），不再用 `export X=$(…)` 这种赋值内命令替换失败不中止的形式。
- 发布流程文档：`npm publish` 直接用本机写权限的 CI/自动化 token（无交互 2FA），并记录 0.12.1 发布中观察到的 E404 CDN 传播与 E409 staged-version 处理。
- 版本与引用同步：全仓 npm 包与 Herdr 插件配置同步至 0.13.0。两个 README 的 Herdr 安装 tag 保持指向已存在的 `v0.12.1`（`v0.13.0` 的 tag 尚未创建），发布提交时再替换。
- **未发布**：仓库内版本号已递增到 0.13.0，但未打 tag、未发布到 npm；npm 上的 latest 仍是 0.12.1。本机生产用本地 tarball 安装本仓构建产物（不依赖 npm 发布），装后按部署流程重启，并用版本、PID、socket 逐条核对。

## 0.12.1

- Pi 终态门禁：turn completion 以可选 `expectedText` 指纹为准（daemon RPC 透传到 turn-completion registry，快路径与 wait 路径都覆盖），`stopReason` 非终态时不再发终态；降级语义改为 `lastAssistantMessage=null` + 明确原因（`expected_text_mismatch` / `no_advance_from_input` / `non_terminal_assistant`），降级计划回到有界重试，等历史补齐后重发真实终态。
- 测试：新增真实重试路径 S6、端到端"降级→重试→完成" S8、快路径 mismatch 负例、RPC 级 `expectedText` 接受与跨实现文本一致性套件；`pnpm check` 绿（794 tests / 53 files）。
- 范围：本版本包含 0.11.7 与 0.12.0（见下）的全部内容——那两次 bump 只改了四文件版本号、没有单独发布，内容随 0.12.1 一起发布。
- 版本与引用同步：四文件版本号同步至 0.12.1（本版本只动了版本文件，未同步 README 安装 tag，故 README 仍写 `v0.11.6`）。

## 0.12.0

- 上游模型报错过滤：新增 `wake.filter_upstream_errors`（默认开）与 `wake.extra_upstream_error_patterns`，命中 429/529/overloaded/rate limit/网络超时等报错形文本时静默丢弃 outcome（不唤醒、不注入上下文、不通知、无兜底），但仍静默 Ack 以收敛 daemon 投递队列；长报告里顺带提到状态码不受影响。
- 版本与引用同步：四文件版本号同步至 0.12.0。
- **未发布**：本版本只做了仓库内 bump，没有打 tag，也没有发布到 npm。

## 0.11.7

- 修复 status plan 等待超时误报并新增 discarded 生命周期：`status_event_plans` 超时重试耗尽由 `failed` 改为 `discarded`，新增 `agent.discarded` 事件类型把观察者等待超时与 agent 崩溃解耦，CAS 条件更新防终态复活并清理残留重试定时器；`herdsman-pi` wait 对 `agent.discarded` 与 legacy 等待历史失败事件都不再误唤醒。
- agy 迟到 idle 不再吃掉已完成 turn 的 ref：迟到 `unknown -> idle` startup plan 不再把最终 assistant ref 注册成 `agent.idle` 终态；仅当终态事件已代表完成 turn（`agent.done`，或 payload `from=working` 的 `agent.idle`）才把该 ref 当已投递基线，插入新 plan 时取消被取代的 pending idle plan（`cancelled` + `last_error='PLAN_SUPERSEDED'`，无 schema 变更）。
- 规范与协作基础设施：补齐 agent 协作骨架与开发规范（AGENTS.md 铁律、`.agents/notes/` 决策笔记系统及索引脚本），并接上统一的 commit message 校验钩子（`.husky/commit-msg` 委托本机全局校验器）。
- 版本与引用同步：四文件版本号同步至 0.11.7。
- **未发布**：本版本只做了仓库内 bump，没有打 tag，也没有发布到 npm。

## 0.11.6

- wake 展示线脱敏管道统一：`formatHiddenAgentUpdates` 接入 `sanitizeAndCleanContextText`，零宽与 bidi 控制字符剥除后统一脱敏（测试 114/114 通过）。
- 版本与引用同步：全仓 npm 包、Herdr 插件配置及文档安装 tag 统一同步至 0.11.6。

## 0.11.5

- 通路 B 遗留收尾：格式化时间戳支持跨年 MM-DD HH:MM:SS，truncateSummary 增加单词边界截断，隐藏上下文文本接入 sanitizeAndCleanContextText 清理脱敏管道。
- 文本防御加固：truncateSummary 增加早空格下界保护（`limit - 20`），避免超长 URL 截断抹除有效前缀；cleanContextText 剥除 Unicode 零宽与不可见格式字符（\u200b-\u200f / \u2060-\u2064 / \ufeff），防御绕过密钥脱敏。
- 版本与引用同步：全仓 npm 包、Herdr 插件配置及文档安装 tag 统一同步至 0.11.5。

## 0.11.4

- register 用 Linux SO_PEERCRED 绑定连接进程 cwd 到 Herdr pane，拒绝错配 connector。
- 历史读取与 Pi 扩展脱敏补裸 Bearer 与 sk- 前缀。
- readJsonl 改为流式读取；超限走 32MiB 尾窗，单记录超窗抛 JsonlTooLargeError。

## 0.11.3

- 短命新 pane 通过先验 working→idle 唤醒 owner，避免错过 pane-specific 订阅窗口。
- Herdr socket 禁止 wildcard status；同连接二次 subscribe 会 RST，订阅重启改走新连接；拓扑事件按 pane 过滤。
- 接缝测试锁定 from=working→idle 唤醒路径。

## 0.11.2

- discovery 会话根从 `/tmp/pi-role-sessions` 迁到 `/tmp/herdr-role-sessions/<herdr-session>/`，回退扫描按 own-session 子目录隔离，根除跨 session 错配。
- 恢复派发子代理 wake 正文；interactive-pi 派发前缀跟新根，修复 idle 误杀。

## 0.11.1

- W1：状态迁移与 status_event_plans 写入同 sqlite 事务，避免状态已改、计划未落库。
- W12：Pi turn-signal 到达后强制重读会话，防止沿用旧历史快照。
- W13：Pi 超时且历史未推进时清空正文并打 `noAdvance` 标，避免空/陈旧终态。
- W14：非 agy keyed 同内容显式 skip，不再误当成推进。
- W2：终态 duplicate skip 收窄，崩溃后重放不再被旧事件吞掉。
- W3：failed plan 启动 drain 幂等回填 `agent.failed`，重启不丢失败可见性。
- W6：`agent.failed` 豁免 agent 行/pane 匹配，并按当前代生成。
- W8：投递耗尽打结构化 error 日志，便于定位卡住批次。

## 0.11.0

- agy 终态就绪闸：终态事件（done/idle）发射前检查正文就绪，空正文不落库 `status.changed` 与终态事件；投递闸拦截非 pi 空 `done`/`idle`（`agent.failed` 豁免），`blocked` 维持中间态豁免。Protobuf 解析仅提取真实 message 字段；compact 历史读取剥离已消费/上一轮残留。
- WAL 指纹识别：SQLite 历史指纹纳入 `-wal`/`-shm` 的 mtime 与 size，实时感知 WAL 写入。
- `PLAN_WAITING_HISTORY` 重试链：未就绪计划写入 pending 带哨兵，由 10s timer 接续重试；`refreshAgent` 抛错时维持哨兵，Pi keyed retry 增加 assistant 变更校验避免假 advance。
- plan failed 可见性：新增 wire 事件类型 `agent.failed`（幂等键 `agent.failed:plan:id`），status plan 耗尽 attempts 后落库并可投递；wake / agent-update-ui 渲染失败原因。
- daemon 实例锁：以内核 `flock` + READY 握手替代 PID 探测，消除裂脑、假锁与 PID 复用；启动链事务性回滚（reconcile 失败释放锁、清理 pid、关闭 server）。
- 事件 reclaim 加固：`reclaimDelivered` 以 `agent_orchestrator_scopes` 租约为唯一事实源，孤儿 delivered 立即回收，不再依赖连接回调。
- 插件平台声明收窄为 Linux（flock 实例锁为内核依赖）

## 0.10.2

- 终态事件持久化补偿：新增 `status_event_plans` 表（0009 migration），状态迁移事件落库为计划行，daemon 启动与周期 reconcile 时 drain 重试（幂等、per-agent 串行队列、attempts 封顶、watch manager 12s drain grace），彻底修复终态事件在运行时丢失的问题。
- mismatch 场景（herdr 事件状态与本地记录不一致）下 done/blocked 终态仍生成，仅 pane 关闭才取消计划；equivalent 守卫改查 status_event_plans 修复双计划。
- 历史推进判据改为 `lastAssistantMessage.ref` / `messageCount`（`historyHasAdvanced`），不再依赖 message 文本比较。
- 事件投递加固：`reclaimDelivered` 对无活跃连接的投递终态立即回收（回调按事件行自身 session 判定，防跨会话误判）。
- 单调逻辑时钟防回拨：`daemon_meta` 持久化 `logical_now_ms`，墙钟回拨与 daemon 重启均不回退事件时间戳。
- daemon instance lock：裸入口 `herdsman-daemon.js` 独占 `HERDSMAN_HOME`，避免 CLI 操作锁之外的双 daemon 并存。
- reconciler 将 generation-less 存活 pane 视为存活，避免误删在跑事件。
- 去重脚本 lib 化（拒活 daemon / wal 备份 / cursor 修理），runtime.json pid 可选化（pid 文件为唯一事实源）。

## 0.10.1

- 修复 daemon 启动入口守卫与 PID 生命周期管理：PID 文件改由 daemon service 自身在 `server.start()` 成功后写入，`stop()` 时使用 try/finally 兜底校验当前 PID 匹配后清理；正常退出 `exit(0)`，异常捕获后显式 `exit(1)`。
- 修复 socket 冲突处理并移除 `orphaned` 语义：`ObservabilityRpcServer.start()` 启动前探测 socket 可达性，可达即拒绝启动并报错，仅残留不可达 socket 允许 unlink；daemon 状态统一将 socket 可达判定为 `running`（stalePid 降级为元数据，移除歧义的 `orphaned` 状态信号），彻底避免双 daemon 并存与 PID 覆写。

## 0.8.6

- 修复事件查询 SQL 括号与 legacy close 全量失效；持久化隔离 Grok HOME，并加强 runtime record、session 路径所有权校验。
- 修复 wake 请求发送空窗与 turn 信号重复消费；启动 reconcile 不释放 owner，补齐 runtime record 与 session 校验回归测试。

## 0.8.5

- 修复空 assistant 历史回传、用户 ESC 后事件无限重传，以及收敛前重复 reclaim 投递。

## 0.8.4

- 增强 daemon 的 orchestrator ack 拒绝与事件投递结构化日志，记录拒绝原因、期望事件及投递批次摘要。
- orchestrator 游标推进时批量清理游标以下遗留 pending/delivered 事件，避免垃圾行污染候选扫描。
- 调查 register 重放、pending 列表来源及 AgentIndexService 上下文快照路径；未发现服务端缓存或重放已删除事件的路径。


- Turn completion signal (route 2): the Pi extension now notifies the daemon after its own final assistant message is written to the session file (bounded stat fallback, timeout still signals with actual status), and the daemon waits up to a bounded window for that signal before emitting `agent.done` / `agent.blocked` events for Pi agents, refreshing the agent right before appending so outcomes carry a non-empty `lastAssistantMessage`. Older extensions that never signal keep working: the daemon times out and generates events as before with a warning.
- 修复陈旧 turn-completion 信号被下一轮等待错误命中的竞态，仅接受等待开始后记录的新信号。
- 修复 turn-completion RPC 信任客户端自报身份的问题，服务端改用 socket 已注册的 Pi presence 身份记录信号。

## 0.7.0 (2026-08-23)

- 接入 grok/agy 历史读取与发现，新增 `grokHome` 元数据及安全校验，移除 shepherd 命名残留。


- Fix an infinite re-wake loop: acknowledging an invalidated orchestrator event is now rejected with a distinct "no longer pending" error, and the extension treats that as terminal - it prunes the outcome from its pending set and advances past the delivered batch instead of retrying forever. Acknowledged ids returned by the server also prune stale pending events on register and after each ack (1aa2612..cce0a5a).
- Suggestion-tier improvements: Pi id-kind session refs resolve their file by id before falling back to discovery; candidate cwd comparison normalizes trailing/repeated slashes; pinned-context retain compares all entries sharing a pane id; snapshot excerpts are capped at 2000 characters; unchanged context snapshots are no longer re-pushed (1aa2612).

## 0.6.5

- Hotfix: the pi extension crashed on load ("Cannot find module '@/shared/json-lines.js'") because 0.6.4 introduced a cross-package path alias that does not resolve when the extension is loaded as standalone TypeScript. The JSON-lines decoder is now vendored inside the herdsman-pi package (same 1 MiB semantics), the alias import is gone, and a regression test loads the extension independently and rejects an oversized frame (4d41230).

## 0.6.4

Full re-audit hardening (correctness / security / reliability), all findings verified against source:

- Delivery cursor deadlocks: transient disconnects no longer advance the failed-wake cursor past unacknowledged events, so batches are redelivered and acknowledged after reconnect; rejected acks only drop the batch (266fdc9).
- Idle events now count as outcomes only when transitioning from working, aligning the delivery predicate with acknowledgement (266fdc9).
- Replacing an agent invalidates its pane's events, so orphans can neither be delivered nor wedge the cursor (266fdc9).
- Authoritative path session refs are validated against the session allowlist at registration; invalid refs fall back to discovery (266fdc9).
- Pending scans paginate past noise windows instead of truncating at 1000 rows (266fdc9).
- History discovery is bounded (depth 4, 2000 files, 256KB cwd prefix) and hardened: role session roots require daemon-owned roots and regular, unlinked, owner-matching files (726d92f).
- Data directory permissions are enforced (0700 home, 0600 db/wal/shm); the socket is created under a private umask (726d92f).
- Reconnect backoff resets once a subscription is established, and protocol-incompatible daemons stop reconnect loops until the next session (726d92f).
- Readiness failures escalate SIGTERM to SIGKILL and only remove the pid file after death is confirmed (726d92f).
- Changed occupied-session sets force history re-discovery, replacing stale fallback snapshots; pinned-context retain detects pane reuse by agent id (266fdc9, 726d92f).

## 0.6.3

- History ownership: Pi fallback discovery now scans dispatched role session roots, requires an exact cwd match, and skips session files already owned by another agent, so a worker pane can no longer be attributed the orchestrator's own transcript (f47e9c1).
- Late session refs are treated as an identity change, replacing snapshots that were built from a fallback guess (f47e9c1).
- Closed panes disappear from injected context immediately: the pinned snapshot is intersected with the newest one by pane identity, with agent reuse detected via agent id (f47e9c1, 31045b4).

## 0.6.2

- Acknowledgement: allow the orchestrator cursor to advance past events that became undeliverable after delivery (for example when a worker pane is retired), instead of rejecting the ack and freezing the cursor. Skipping ahead of still-deliverable events is still refused, and sealed events remain unacknowledgeable (9007766).
- Lint and formatting debt from the hardening rounds cleared; biome check is now clean and enforced by the pre-commit hook (288adab).

## 0.6.1

- Delivery pipeline: unified the deliverable-event predicate across pending discovery, acknowledgement, and publication so filtered events can no longer pin the queue (ddbc5ca, 49b6218).
- Wake correctness: dropped empty wake turns, isolated interactive Pi observers from dispatched roles, and stopped aborting in-flight turns on transient disconnects (53c3513, 302c837, 97a54bf).
- Observation chain: rebuild the Herdr client with exponential backoff on subscription failure, discard stale session refreshes via a mutation epoch, and harden session-path classification (3b1e922).
- Security and process hardening: private observability sockets, non-preemptive scope claims, bounded JSON-lines frames, exclusive daemon PID startup, readiness polling, SIGKILL escalation, SQLite WAL/busy timeout, and log rotation (adff7ef).
- Daemon status now reports a reachable daemon without a PID file as running and exposes pidFileMissing (5a1f78d).
- Test suite grew from 240 to 255 cases across 33 files, covering disconnect, frame-limit, path-classification, and acknowledgement-cursor regressions (5f47567).

## 0.6.0

Forked from @ryonakae/herdsman at v0.5.1 (dfdd3a2). Previous history is inherited from upstream.

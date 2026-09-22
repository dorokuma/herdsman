---
status: active
superseded_by: ""
supersedes: ""
模块: config | herdsman-pi
---

# 过滤模型上游报错：静默丢弃 + 静默 Ack，不唤醒主代理

## 一句话结论

上游模型报错（429/529/overloaded/rate limit/网络超时等）命中后一律静默丢弃：不产生 wake outcome、不 sendMessage、不 notify、不注入上下文，也不做任何超时/连击兜底；但必须对 daemon 静默 Ack，否则投递队列无法收敛。

## 背景

上游模型持续报错时 Herdr 把 pane 置为 done/idle，Reader 把报错文本当成普通 `lastAssistantMessage.text`，于是生成 `agent.done`，经 `wake.ts` 投影后 `pi.sendMessage` 唤醒主代理。代码里此前不存在任何上游报错识别，导致每次模型 4xx/5xx 都把编排者唤醒在一个必然失败的事件上。

## 决策

1. **路线 A+C**：分类器（纯函数模块 `packages/herdsman-pi/src/upstream-error.ts`）+ Pi 投影层过滤（`wake.ts` 的 `project`/`scheduleWake`/`startWake` 三处投影共用同一个 `wakeFilter`）。不改 daemon 事件生成与投递语义，不改 RPC / wire 契约，不改 `src/agent-history/**`。
2. **默认开启且可配置**：Runtime schema（TypeBox/Ajv，`src/config/schema.ts`）新增可选 `wake.filter_upstream_errors`（默认 `true`）与 `wake.extra_upstream_error_patterns`（默认 `[]`）；省略整段 `wake` 也视为开启。daemon 只校验不消费。
3. **Pi 侧零依赖读 YAML 子集**：`packages/herdsman-pi/src/wake-filter-config.ts` 手工解析 `$HERDSMAN_HOME/config.yaml` 的文档化子集（顶层 `wake:`、2 空格缩进、`true`/`false`、`- item` 列表、`#` 注释、引号字符串），解析失败回落默认值 + `logHerdsmanPi("warn", ...)`，绝不阻断扩展；环境变量 `HERDSMAN_WAKE_FILTER_UPSTREAM_ERRORS` / `HERDSMAN_WAKE_EXTRA_UPSTREAM_ERROR_PATTERNS` 优先于文件。配置只在扩展实例化时读一次（改 yaml 需重启 Pi，与 daemon 配置模型一致）。
4. **误伤靠「报错形门闩」**：短文本必须开头命中错误信封或强 token，或任意位置命中结构化错误码，或状态码与错误上下文同现（`error/err/failed` 后接 `status/code/with` 与 `:=`/`:`/`=`）；且**世界文本都要开头锚定**，避免“Documented error 429 handling”之类中间埋词误判。normalize 后长度 > 400 字必须前 120 字命中信封正则（`api error` / `error:` / `connection error:` / `the model is overloaded` / `econnreset` / `etimedout` / `enotfound` / `eai_again` / `socket hang up` / `fetch failed` / `und_err_` / `request timed out` / `timeout of Nms exceeded` / `rate_limit_error` / `overloaded_error` / `resource_exhausted` / `insufficient_quota` / `quota exceeded` / `429|503|529 too many requests|service unavailable|overloaded|…`；注意 `failed`、裸 `rate limit`、`频率限制` **不在**信封正则内）。门闩通过后仍可否决内置命中：含三反引号代码块、`^#{1,6}\s` 标题、或（>400 字且不以信封开头）。自定义 pattern 绕过门闩（`/re/flags` 或大小写不敏感子串），非法正则跳过且不影响其它规则。
5. **内置模式分层**：T1 显式错误码/错误类型（`api error: 429/529/5`、`rate_limit_error`、`overloaded_error`、`resource_exhausted`、`insufficient_quota`、`the model is overloaded`），T2 `rate limit`/`tokens per minute`，T3 传输层（整段或开头 `connection error`、`econnreset`/`econnrefused`/`etimedout`/`enotfound`/`eai_again`、`socket hang up`、`fetch failed`、`und_err_`、`request timed out`、`timeout of \d+ms exceeded`），T4 门闩已通过且以 `error/err/failed` 开头时，再接 `429/503/529` 即命中；其中 `status`/`code`/`with` 与 `:=`/`:`/`=` 均为**可选**（正则里是 `(?:status|code|with)*` 与 `[:=]?`），因此 `error 429`、`failed 503` 无填充也命中，但后接必须为末尾/标点/错误类词（正则：`/^(?:error|err|failed)\b\s*(?:(?:status|code|with)\s*)*[:=]?\s*(?:429|503|529)\b(?=$|\s*[:：,，。.！!?？;；)]|\s+(?:rate|overload|limit|exceed|quota|unavailable|busy|slow|too\s+many|retry|please|try)\b)/i`）。**不再有**裸 `\b429\b`、`please try again later`、`tokens per minute` 的自动命中；但新增 T5 裸状态行 `^(?:429|503|529)\s+(?:too many requests|service unavailable|overloaded|bad gateway|gateway timeout)`（第七轮），因此 `429 Too Many Requests` 这类“状态码+短语”行是有意命中的，而孤立的 `\b429\b` 仍不命中。禁止裸匹配 `\btimeout\b` 或 `\berror\b`。
6. **命中即静默 Ack**：`wake.ts` 的 `AgentOutcomeProjection` 新增 `suppressedUpstreamErrorEventIds`，被抑制 id 不进 `outcome`、也不进投影器的 `seen`（rawEvents 仍可见，保持 evidence-only）。`index.ts` 的 `scheduleWake` 分三支：wakeable > 0 走现有 `WAKE_SETTLE_MS` + `sendMessage` 路径（该路径禁止静默 Ack）；wakeable === 0 且 suppressed > 0 → 安排静默 Ack（不 sendMessage、不 notify、不进 `deliveredBatch`、不写 `presentedEventIds`，用户非 idle 时也执行；`deliveredBatch` 或 `ackInFlight` 时推迟以免抢游标）；两者皆 0 保持现状。Ack 统一走抽出的 `acknowledgeEventIds(events, { notify }, ctx)`，RPC 仍为 `agent.notifications.ack`、按 id 升序、失败分类复用 `classifyAckFailure`（transient 退避、耗尽才抬死信水位），因此 on-success 或 `ORCHESTRATOR_EVENT_ALREADY_ACKED` 都会把 `failedWakeThroughEventId` 抬到 `max(...)`。禁止用「只抬死信水位、不 Ack」当丢弃手段。
7. **不改 Reader 打标**：分类输入是 `lastAssistantMessage.text`（`kind === "failed"` 时另查 `payload.reason`），不看 `lastToolResult`。

## 被放弃的方案（必填）

- **方案 B（daemon 侧丢弃事件）**：上游报错识别属于「读者语义」，放进 daemon 会污染事件生成/投递契约，也会让其它消费者拿到被吞掉的事件；放弃。
- **改 RPC / wire 契约传配置**：Pi 是独立 npm 包、零运行时依赖，为配置扩 RPC 会同时改动 daemon 与扩展的协议面；手工解析 YAML 子集更小；放弃。
- **Reader 打标（在 `readers.ts` 标注「上游报错」）**：把判定塞进历史读取层会让 CLI/其它消费者也受影响，且读层不该携带唤醒策略；放弃。
- **否决失败兜底唤醒（超时提醒 / 连续失败 N 次提醒）**：用户明确要求命中即静默、完全无兜底；兜底会在上游长时间故障时重新制造打扰；放弃。
- **`content.includes("429")` 之类的朴素匹配**：会把长报告里顺带提到的 429/timeout/rate limit 也吃掉（误伤），必须用「报错形门闩」把长正文排除；放弃。
- **不做静默 Ack（只过滤、不确认）**：daemon 投递队列永不收敛，pending 反复列出，60s + 10 次后变 DELIVERY_ATTEMPTS_EXCEEDED，后续真正 outcome 的 Ack 还可能撞 ORCHESTRATOR_EVENT_OUT_OF_ORDER；放弃。

## 来源

本批工作区 diff（`packages/herdsman-pi/src/upstream-error.ts`、`wake-filter-config.ts`、`wake.ts`、`index.ts`、`logger.ts`、`src/config/schema.ts` 及对应单测），`pnpm check` / `pnpm build` / `pnpm package:check` 全绿（53 个测试文件 / 782 用例）。

## 二轮修复（双审修单）

### 阻断 R1：静默 Ack 与退避定时器互锁，队列永不收敛

**真实机制**：纯上游报错场景下首次 `agent.notifications.ack` 遇暂态失败（断连 / ORCHESTRATOR_BUSY / 网络抖动）写入 `nextAttemptAt = now + ackBackoffMs`。退避定时器到期后 `scheduleWake` 重算 `nextAttemptAt` 时只过滤 `value !== undefined`，没过滤「已过期」时间戳，于是 `Math.max(0, nextAttemptAt - Date.now())` 恒为 0，先挂 0ms `wakeTimer`；随后 `scheduleSilentUpstreamErrorAck` 的入口守卫 `if (state.wakeTimer || state.wakeRequested) return` 把 Ack 挡回自己的定时器；0ms 定时器再次 `scheduleWake` → 循环：Ack 永远停留在第一次失败，daemon pending 队列不收敛，后续正常 outcome 的 Ack 可能撞 `ORCHESTRATOR_EVENT_OUT_OF_ORDER`。

**修复方式**：在 `scheduleWake` 的 `wakeable.length === 0` 分支里，先计算 `dueSuppressed = suppressedEvents.filter(isWakeableEvent)`；若非空则只调用 `scheduleSilentUpstreamErrorAck` 并立即 `return`，**不再先挂 wakeTimer**。仅当 `dueSuppressed` 为空时才对 `nextAttemptAt` 设退避定时器，且 `nextAttemptAt` 过滤条件改为 `value !== undefined && value > Date.now()`（严格大于当前时间、排除过期时间戳），延时直接用 `nextAttemptAt - Date.now()` 而非 `Math.max(0, ...)`。

### 阻断 R2：报错形门闩过宽，默认开启 + 无兜底时误吞正常短完成

**真实机制**：`isErrorShaped` 对 normalize 后长度 ≤ 400 字「无条件放行」，且内置模式含孤立裸状态码（`\b429\b`、`\b503\b`、`\b529\b`）与日常词（`rate limit`、`tokens per minute`、`please try again later`、`^connection error\b`、`频率限制` 等）。这导致大量正常短答复（如“查过了，日志里一共发现了 3 次 503 错误。”、“Done. Added HTTP 503 retry in the client.”）被误判为上游报错；用户已拍板无兜底，误判 = 正常 `agent.done` 永不唤醒主代理。

**修复方式**：取消“短文本 ≤ 400 无条件视为报错形”，改为短文本也必须具备错误信封或强 token（`isErrorShaped` 对短文本要求：开头命中信封正则 / 开头命中强 token（`rate limit hit`、`socket hang up`、`fetch failed`、`request timed out`、`timeout of \d+ms exceeded`、`quota exceeded`、`you exceeded your current quota`、`the model is (?:currently )?overloaded`、中文强 token）/ 任意位置命中结构化错误码（`rate_limit_error`、`overloaded_error`、`resource_exhausted`、`insufficient_quota`、`ECONNRESET`、`ETIMEDOUT`、`ENOTFOUND`、`EAI_AGAIN`、`und_err_*`）/ 状态码与错误上下文同现（`(?:status|code|error|err|failed)\s*[:=]?\s*(?:429|503|529)`，明确不含 `http` 前缀以免“HTTP 429 状态码…”误判）。内置模式同步收窄：删除裸 `\b429\b`/`\b529\b`/`\b503\b`，删除裸 `rate limit` / `tokens per minute` / `please try again later`（仅报错信封同现才命中），`connection error` 改为信封式 `\bconnection error\b\s*[:：]`，`频率限制` 保留为强 token（纯解释性句子“已说明频率限制与配额的区别”以“已说明”开头、不命中强 token 前缀、也不命中单独的 `频率限制` 内置词）。

### 为何无兜底仍成立

两项阻断修复后，门闩把“正常短答复”排除在抑制集之外：正常完成的 `agent.done` 依旧产生 outcome 并唤醒主代理，没有被静默吞掉；真正形态的上游报错依旧命中并被静默 Ack 收敛队列。没有引入任何超时/连续失败提醒兜底，命中即静默的策略保持不变。

### 非阻断 3 条

1. **acknowledgeEventIds 断连假成功**：`state.client?.request` 改为请求前判 `!state.client` 则 `throw`，走既有 `classifyAckFailure` 失败路径（transient 分类 → 退避写 `nextAttemptAt` → finally 重新 `scheduleWake`），确保断连不假成功、不推进 `failedWakeThroughEventId`。与 `agent_settled` 入口守卫行为一致。
2. **环形依赖**：抽出 `packages/herdsman-pi/src/logger.ts` 承载 `logHerdsmanPi`（保持原签名与行为），`index.ts` 与 `wake-filter-config.ts` 均从 `./logger.js` 导入；全仓其它引用（`index.ts` 内部多处使用、`herdsman-pi-extension.test.ts` 通过 `extensionModuleUrl` 导入）保持行为不变。
3. **静默 Ack 暂态失败重试单测**：`herdsman-pi-extension.test.ts` 新增“retries a silent ack after a transient failure across the backoff window”，模拟首次 ack 抛 `ORCHESTRATOR_BUSY`，推进假时钟越过退避窗口，断言第二次 `agent.notifications.ack` 确实发出、且过程中无 0ms 定时器空转（100ms/150ms 内 ackAttempts 保持 1）。

## 第四轮修复（精确正则 + 中文强 token 独立通道）

### 阻断 R1：上一轮“放宽填充”导致误吞正常短完成句

第三轮去掉了 `isErrorShaped` 对短文本的门闩，把短文本无条件视为报错形，同时 `STATUS_CONTEXT_PATTERN` 采用 `/^(?:error|err|failed)\b\s*(?:(?:status|code|with)\s*)*[:=]?\s*(?:429|503|529)\b/i`；这导致以错误前缀开头的正常短完成句被误判命中，例如 "Error 429 was documented in the README."、"Failed 503 times in the test suite."、"频率限制已修复。"。注意 "Ferrari 429" 从未命中过——该正则要求以 `error|err|failed` 开头；"error 429: rate limited" 是必须命中的真实报错，不是误伤。

**这三句的后续状态**：「频率限制已修复。」已由第四轮负向预查询修；「Error 429 was documented…」与「Failed 503 times…」也已在第七轮由 T4 后置 lookahead 修掉。三句现在均为 miss。

**修复方式**：在 `STATUS_CONTEXT_PATTERN` 中明确只允许 `error/err/failed` 后接 `status/code/with`，且必须开头锚定。中文 token（`频率限制`/`请求过于频繁`/`模型过载`/`资源耗尽`）在 `START_STRONG_TOKEN_PATTERN` 与 `BUILT_IN_PATTERNS` 中改为开头锚定 + 负向预查 `(?![与和及已的])`（字符集已在第七轮扩到 `[与和及已的以而还也但并了在是也]`，见下），两处写法逐字符一致，使「频率限制已修复。」「频率限制与配额的区别已说明。」这类说明句不再被判为报错形。新增 `(?:the\s+)?request\s+failed|` 作为 `START_STRONG_TOKEN_PATTERN` 的第一个备选项，使“The request failed with status 503”等自然句法由强 token 承载，不再依赖放宽的 T4。

**残留误伤（第四轮状态，已在第七轮清零）**：第四轮时负向预查字符集为 `[与和及已的]`，未收录 `以`/`是`/`也` 等续字，因此「频率限制以及…」类句子会被误判。第七轮已把字符集扩到 `[与和及已的以而还也但并了在是也]`，这类句子现为 miss（详见「第七轮修复」段）。

### 残余取舍

- `status: 429` / `code: 503` 形式仍漏滤：未扩展允许裸 key: value（可接受假阴性）。
- “The request failed with status 503” 由强 token 命中，不依赖 T4。
- `extraPatterns` 仍绕过门闩；算作显式授权行为。
- 中文预查字符集已扩至 `[与和及已的以而还也但并了在是也]`，覆盖已/与/和/及/的/以/而/还/也/但/并/了/在/是 等常见续字。

### 决策 4/5 修正

决策 4（误伤靠报错形门闩）原描述中的“≤400 字无条件放行”与第三轮实际残留不符；第四轮改为：短文本也必须开头锚定且符合强 token/结构化错误码/状态码上下文同现，世界文本一律开头锚定。中文 token 独立通道增加负向预查。

决策 5（内置模式分层）删除“裸 `\b429\b`、`please try again later`、`too many requests`、`tokens per minute` 的自动命中”，改为仅以 `error/err/failed` 开头的 `status/code/with` 与 `:=` 才命中 `429/503/529`。现有 `rate_limit_error`、`overloaded_error`、`resource_exhausted`、`insufficient_quota`、`the model is overloaded`、`频率限制` 保持强 token/结构化命中。

### 代码相符性核验

- `STATUS_CONTEXT_PATTERN` 常量（~L62）与 `BUILT_IN_PATTERNS` 中 `label="status code"`（~L99）正則一致，两者均带 T4 后置 lookahead。
- `START_STRONG_TOKEN_PATTERN`（~L43）首选项含 `(?:the\s+)?request\s+failed|`。
- 中文四 token 在 `BUILT_IN_PATTERNS`（~L103-106）与 `START_STRONG_TOKEN_PATTERN`（~L44）两处均为 `^…(?![与和及已的以而还也但并了在是也])`，逐字符一致。

### 用例统计（实跑：`pnpm exec vitest run test/unit/upstream-error.test.ts`）

- 正例：46 条
- 反例：48 条
- 单测总数：94 条 parameterized + 12 条单测 = 106 条（本文件单独跑通过；全仓 `pnpm check` 为 53 文件 / 782 用例）

## 第七轮修复（四项遗留清零）

用户批准把之前列为「遗留」的四项一并修掉。四项改动与验证如下（均有实跑支撑）：

1. **中文负向预查扩集**：`START_STRONG_TOKEN_PATTERN` 与 `BUILT_IN_PATTERNS` 的中文四 token 预查字符集从 `[与和及已的]` 扩到 `[与和及已的以而还也但并了在是也]`。效果：「频率限制以及…」「请求过于频繁是…」「资源耗尽也被…」「模型过载了…」「模型过载在…」均转为 miss；四 token 带标点/无标点（后跟 `请`）的真实报错仍全部 hit。
2. **`ECONNREFUSED` 进门闩**：加入 `CODE_STRONG_TOKEN_PATTERN`，与 `BUILT_IN_PATTERNS` 中的 `\bECONNREFUSED\b` 对齐；裸 `ECONNREFUSED` 与 `connect ECONNREFUSED` 现在均 hit。
3. **裸状态行（新 T5）**：`ENVELOPE_PATTERN` 与 `BUILT_IN_PATTERNS` 新增 `^(?:429|503|529)\s+(?:too\s+many\s+requests|service\s+unavailable|overloaded|bad\s+gateway|gateway\s+timeout)`。裸 `429 Too Many Requests` / `503 Service Unavailable` / `529 overloaded` 现在 hit；仍是开头锚定，`Documented 429 Too Many Requests handling.` 仍 miss。
4. **T4 后置 lookahead**：`STATUS_CONTEXT_PATTERN` 与 `BUILT_IN_PATTERNS` 的 `status code` 项均加上 `(?=$|\s*[:：,，。.！!?？;；)]|\s+(?:rate|overload|limit|exceed|quota|unavailable|busy|slow|too\s+many|retry|please|try)\b)`。效果：`Error 429 was documented in the README.`、`Failed 503 times in the test suite.`、`error with 503 retries configured.` 转为 miss；而 `error 429`、`failed 503`、`Error 429: rate limited`、`error 429 rate limited` 等 must-hit 正例全部保留。

第七轮验证：定向测试 106 passed；全仓 `pnpm check` 53 文件 / 782 用例全绿；主代理与双审各自实跑 59 条对抗样本全部符合预期。


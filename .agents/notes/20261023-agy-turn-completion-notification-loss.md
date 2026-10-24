---
status: active # active | superseded
superseded_by: ""
supersedes: ""
# 模块可选值: observability, daemon, cli, config, db, herdr, shared, herdsman-pi, herdsman-herdr-plugin, release
模块: observability
---

# Agy 完成通知丢失根因分析与弹性重试修复

## 一句话结论

修复了 worker（agy）完成回合后因 Herdr 状态与 sqlite 写入时序差导致的通知静默丢失问题：将原固定的 10 秒等待重试升级为指数退避（5s→10s→20s→40s→60s 封顶）且总窗口默认 15 分钟的弹性窗口；对丢弃计划产出 `fallbackOutcome: true`，并在编排侧投影为 `stalled`（`agent.failed` 严格保持为 `failed`）；支持 stalled 之后迟到回复的自动补发（按 `latestCompletedTurnRef` 幂等去重只发一次），并对疑似假 idle 计入总窗口但扣除/暂停 working 时段，彻底杜绝静默假死。

## 背景与事故定证

在主代理编排 coding agent（如 agy 执行体 worker）的场景中，偶发出现 worker 回合已实际执行完成，但主代理一直等不到唤醒通知的假死现象。

经现场定证，事故的根因链如下：
1. **时序差**：worker（agy）完成回合时，Herdr 先上报 pane 为 `idle`，而 agy 将 assistant 消息落盘至 `conversation.sqlite` 的 `steps` 表发生在回合末尾。
2. **计划过早建立与耗尽**：Herdr 上报 `idle` 后，Herdsman 建立了 `working -> idle` 的状态事件计划（status event plan）；旧逻辑使用固定 10 秒间隔重试，最多 8 次（约 100 秒）；由于 agy 迟迟未落盘或写入存在短暂延迟，8 次重试耗尽后计划被标记为 `discarded`。
3. **静默丢弃**：旧逻辑在计划 discard 时仅产出一条不带 `fallbackOutcome` 的 `agent.discarded` 事件；编排端（`packages/herdsman-pi/src/wake.ts`）将 `PLAN_WAITING_HISTORY` 的 discarded 投影为 `undefined`（过滤掉），导致编排侧没有任何唤醒输出，主代理无限等待。

## 决策与修复方案

### 1. B1: 弹性等待窗口、指数退避与 Working 豁免

- **退避序列**：退避因子为 2，初始退避 5s，序列为 5s -> 10s -> 20s -> 40s -> 60s 封顶。
- **总窗口与假 idle 约束**：增加 `total_window_ms`（默认 15 分钟）。在窗口期内，若 Herdr 已报 `idle` 但历史读取器仍未就绪（疑似假 idle），保持计划为 `pending` 状态不被提前 discard，持续退避重试；只有在处于疑似假 idle 状态的有效等待时间达到 15 分钟后才 settle 为 `discarded`。
- **Working 豁免与倒计时暂停/重置**：起算时刻为首次见 idle（plan 创建时）；期间若 Herdr 重新上报或检测到 `isLiveWorking === true`，享有 working 豁免继续等待，且将倒计时锚点同步更新为当前时刻（重置/暂停倒计时），working 期间绝不消耗 15 分钟的 idle 超时预算。
- **启动时配置解析**：支持 `config.yaml` 的 `observability.status_plans` 以及环境变量（`HERDSMAN_STATUS_PLAN_INITIAL_BACKOFF_MS`、`HERDSMAN_STATUS_PLAN_MAX_BACKOFF_MS`、`HERDSMAN_STATUS_PLAN_BACKOFF_FACTOR`、`HERDSMAN_STATUS_PLAN_TOTAL_WINDOW_MS`）覆盖；配置在 daemon 启动时解析一次，修改 `config.yaml` 或环境变量后需执行 `systemctl restart herdsman.service` 生效（无运行时热加载/动态覆盖）。

### 2. B2: stalled 降级唤醒与精准投影

- **Discard 携带 Fallback**：在 `#appendPlanDiscardedEvent` 中写入 `fallbackOutcome: true`。
- **投影语义修正（M1）**：
  - `agent.failed` 分支严格保持投影为 `failed`（回退原语义，真实错误绝不谎报为 stalled）；
  - 只有 `agent.discarded` 分支（`PLAN_WAITING_HISTORY` / `degraded` 且 `fallbackOutcome === true`）投影为 `stalled`，并格式化输出明确的停滞通知，打破静默。
- **向前向后兼容**：老版本 `herdsman-pi` 插件在消费事件时，若未识别 `stalled` 结果，其类型/结构兜底逻辑会安全回退为 `failed`，避免引发插件运行时崩溃。

### 3. B2.1: stalled 之后迟到回复的补发（只发一次）

- **场景与要求**：某回合因窗口耗尽发出了 `stalled`（`agent.discarded` + `fallbackOutcome`）；之后该回合迟到的 assistant 回复才写入 conversation sqlite。此时必须再补发一条正常的完成通知（走 `agent.idle`/`agent.done` 语义，不得再用 discarded/stalled 冒充完成）。
- **幂等与去重**：以 `latestCompletedTurnRef()` 为基准比对。只有当历史中的 assistant ref 与最近一次已完成事件（`agent.done` 或 `agent.idle from working`）不同时，才生成补发事件，并以 `agent.late_completed:${agent.id}:${ref}` 作为数据库 idempotencyKey。补发写入后，该事件自身成为最新的 completed turn，后续任何再次刷新均命中 `currentRef === latestRef`，天然静默，绝不重复唤醒。
- **触发点与完备性**：
  1. 周期性巡检（`#refreshHerdrSessionNow`）：针对处于 stalled 状态的 agent 自动标记 `dirty` 并启用 `forceRefresh: true`，即使 Herdr 毫无状态变化，后台 watch manager 的 10 秒巡检也能自动从磁盘拉取最新历史并补发；
  2. 状态变更（`#handleHerdrEventNow` 的 `pane.agent_status_changed`）：状态跃迁时挂接迟到检测；
  3. 优雅关停（`#handleHerdrEventNow` 的 `pane.closed`）：Tab 关闭前先抢救刷新并补发迟到结果，再 retire/invalidate pane。

### 4. B3: 消除虚假 Drain 日志与事件可预留保证

- 修正日志：在 `#appendPlanDiscardedEvent` 结尾新增如实反映状态的 debug 日志 `"Herdsman plan discarded event recorded and published"`（HEAD 上该函数原本没有日志，不存在需要改写的虚假日志）。
- 投递与预留机制保证：`agent.discarded` 事件写入时状态固定为 `pending` 且 `deliverable = 1`；在线时通过 `onAgentEvent -> publishAgentEvent` 直接向 socket 实时推送；重连或轮询时由 `orchestrator.pending() -> reservePending()` 扫描选中并预留（单元测试已钉死验证）。

### 5. A1 & A2: 历史读取器未就绪原因与 Pi 空读降噪

- **未就绪原因携带**：历史读取器在未就绪时携带具体 `unreadyReason`（`empty_history`、`no_user_message`、`no_assistant_message`、`assistant_only_before_last_user`），写入上层 warn 日志结构体，便于定位。
- **Pi 空读降噪**：针对 Pi 的空 assistant 读取从 warn 降级为 debug，并增加内存计数器 `getPiEmptyAssistantCount` 供指标与排查使用；Agy 等其它 agent 保持 warn 级别。

### 6. 定证日志强化

- 在 `AntigravityHistoryReader` 中，每次读取时在 debug 日志中输出 sqlite 文件路径、`steps` 表行数、最后一条 step 的时间戳及 ID。

### 7. 第三轮：补发链路的四条失效路径

现场复测发现补发机制仍有四个断点，均已修复并各自钉死回归测试（`test/integration/agent-index-service.test.ts` 的 `third-round` describe，逐条做过“改坏即红”的变异验证）：

1. **F1 重复投递**：`#reconcileLateCompletion` 自己调 `#onAgentEvent` 推送，而三个调用点又把同一事件放进各自返回的 `events` 数组，由 `HerdrSessionWatchManager#publishResult -> publishAgentEvent` 再推一次；`publishAgentEvent` 没有 id 级去重，socket 上同一条 `agent.event` 出现两次，破坏 exactly-once。修复：补发只写库并返回，投递权一律归调用方（与 `executeStatusEventPlan` 的既有约定一致）。
2. **F4 关窗抢救缺失**：`pane.closed` 分支只做 retire/invalidate，迟到回复在 Tab 关闭前没有被抢救。修复：先按 `#isAgentStalled` 判定刷新并补发，再 retire；补发事件由 `invalidatePane` 的保留策略标为 `RETAINED_OUTCOME_PANE_CLOSED`（`invalidated` 但 `deliverable = 1`），能通过 `isDeliverableAgentEvent` 的共享投递谓词。
3. **F6 idle/working 抖动**：Herdr 在 `idle` 与 `working` 之间抖动时，每个重试 tick 都看到 live `working`，`idleStartedAt` 被反复重置，15 分钟窗口永远攒不满，计划在 18 次无进展 tick 后走 stall 分支结算为 `PLAN_STALLED`。该原因原先不在 `fallbackOutcome` 集合里，补发因此失能、pane 彻底静默。修复：`fallbackOutcome` 收敛为共享常量 `STALLED_PLAN_ERRORS = [PLAN_WAITING_HISTORY, PLAN_STALLED, DEGRADED_PLAN_ERROR]`；编排侧（`src/agy/hook.ts` 与 `packages/herdsman-pi/src/wake.ts`）把 `PLAN_STALLED` 投影为自解释的 `stalled`（带 `fallbackOutcome`）/ `failed`（无标记的存量行）。pi 的 discard 走不到该分支，语义不变。
4. **F7 补发被意外解除武装**：`#isAgentStalled` 原先问“最新的 terminal 事件是不是带 `fallbackOutcome` 的 discard”，补发前只要落进一条无关的 `agent.failed`（或一条无标记的 discard、一条 `agent.status.changed`），补发就被永久解除武装。修复：判定改为读 `status_event_plans` 记录——同一 `agent_id` + session 下最新一条 `discarded` 的 `last_error` 是否属于 `STALLED_PLAN_ERRORS`。
   - **解除武装条件（M1 补齐）**：`#isAgentStalled` 在上述“武装”之外还带三条解除条件——① 补发成功（`lateCompletion: true` 的完成事件）之后，因其事件 id 大于该 discard 计划对应的事件 id，自动解除武装；② 同一 pane 后续回合产出新的完成回合事件（`agent.done` 或 `from = working` 的 `agent.idle`）；③ 后续计划行 `status = 'completed'`。解除武装后，10s 巡检不再因 stalled 标脏并 `forceRefresh: true` 重读 sqlite，避免补发完成后仍长达 7 天（事件保留期）每轮强制重读。
   - **回合边界（M1 补齐）**：`#reconcileLateCompletion` 仅在 agent 处于 `idle` / `done` 时触发；`#refreshHerdrSessionNow` 的 stalled 判定同样只在 `idle` / `done` 时成立。pane 处于 `working`（下一回合进行中）时即使历史里出现新的中间 assistant 消息（新 `ref`），也绝不补发，避免误发 `agent.idle (from: working, lateCompletion: true)` 假完成通知。

另：`#planProgressTrackers` 的清理缺口已在 `#markPlanRetry`、`stopWaitingHistoryRetries`、`#clearWaitingTimer`、`#salvageOrCancelMissingAgentPlan` 四处补齐 `#planWindowTrackers`，避免已终结算法的计划留下永不释放的 idle 锚点。

## 已知边界

- `#isAgentStalled` 按 `status_event_plans.agent_id` 判定，因此 **pane 在新 agent id 下重建后**（`agents` 行 id 变化、事件表 `agent_id` 被置空）补发按的是真正 stall 的那个 agent 实例，不会串到新实例；反之，若 pane 重建恰好产生新的 agent id，则不会补发上一实例的迟到回复。
- 补发幂等键为 `agent.late_completed:${agent.id}:${ref}`，只覆盖“同一 agent + 同一 assistant ref”；同一 ref 在不同 pane/agent 实例下会被视为不同事件。
- `PLAN_STALLED` 的投影只在 `agent.discarded` 分支生效；`agent.failed` 分支严格保持 `failed`。
- 补发事件的 `from` 固定填 `"working"`（即使原丢失计划来自 `unknown -> idle`）。这是为了让 `latestCompletedTurnEvent`（只接受 `agent.done` 或 `from = working` 的 `agent.idle`）与下游编排投影器将其识别为“已完成回合”，代价是 payload 里的 `from` 不忠实反映原计划的起始状态。
- 15 分钟弹性窗口只对 agy 的疑似假 idle 生效；pi 等非 agy 计划仍受 `STATUS_PLAN_MAX_ATTEMPTS = 8` 约束。但在指数退避序列下，7 次退避等待（`attempts = 1..7`：5s + 10s + 20s + 40s + 60s + 60s + 60s）累计 255s，加上 8 次尝试各自的 4s race 共约 287s（第 8 次尝试即结算；旧固定 10s 退避下为 7×10s = 70s 退避、含 race 约 102s）。
- **C5 观察项闭环（`DEGRADED_PLAN_ERROR` 为何留在 `STALLED_PLAN_ERRORS`）**：`src/observability/contracts.ts:311-315` 的 `STALLED_PLAN_ERRORS` 包含 `DEGRADED_PLAN_ERROR`（`"degraded"`）属对存量/防御性 discard 行的向后兼容：现行 `src/db/status-event-plans.ts` 的 `markRetry`（`:372-378`）只把 `PLAN_WAITING_HISTORY` 结算为 `discarded`，其余错误（含 `degraded`）一律结算为 `failed`，因此新代码不再产出 `last_error = 'degraded'` 的 discard 行；保留该常量项只为让老版本或异常路径留下的同类行仍投影为 `stalled`，不被静默。
- **与同日早期笔记的关系**：本笔记对 `PLAN_STALLED`（带 `fallbackOutcome: true`）投影为 `stalled` 的决策，更新了同日早期笔记 `.agents/notes/20261023-agy-orchestrator-review-considerations.md`（第 77 行）中“`PLAN_STALLED` 及其他失败原因则投影为 `failed` 唤醒编排者”的描述——现行语义为：带 `fallbackOutcome` 的 `PLAN_STALLED` discard 行投影为 `stalled`，无 `fallbackOutcome` 标记的存量行与 pi 侧仍保持 `failed`。

## 后续项：用 agy Stop hook 的 agent.turn.completed 作为回合结束权威信号

主代理已只读核查相关链路代码事实（带 `file:line` 溯源）：
1. **发送端（`src/agy/hook.ts:424-437`）**：`terminationReason && terminationReason !== "NO_TOOL_CALL"` 时静默 `return`，不发送 `agent.turn.completed`（用户 Esc / 中断 / 异常退出等均不发送）。
2. **发送端（`src/agy/hook.ts:444-454`）**：先 `registerAgyPresence` 再发 `agent.turn.completed`，`confirmed` 取决于是否能从 transcript 读到 assistant 文本。
3. **接收端（`src/observability/turn-completion.ts:35-138`）**：`TurnCompletionRegistry` 索引键为 `${herdrSessionName}\0${terminalId}`，为纯内存 Map 不落库，TTL 为 `TURN_SIGNAL_RETENTION_MS = 600000`（10 分钟），消费即删。
4. **服务端（`src/daemon/observability-server.ts:551-564`）**：`#requirePiPresence` 实际只查 `socket -> presence` Map（未校验 `subscriberKind`），因此 agy 信号技术上能进 registry，但服务端接收零日志。
5. **消费端（`src/observability/agent-index-service.ts:3086-3109`）**：非 pi 路径只在 `!isReadyNonPi` 的 500ms race 里使用该信号提前唤醒，不驱动状态跃迁；pi 路径在 `:2583-2610`。

**局限与结论**：
该信号当前不可事后核验、不可按 pane 查询；且 Stop 只在正常 `NO_TOOL_CALL` 终止时发，崩溃/中断时不发。因此不能作为回合结束的单一权威依据。
**建议后续演进方向**：
若后续重启该工作，建议：registry 支持 paneId 索引 + 接收端补齐结构化可观测性日志 + 由 hook 信号主动结算关联的 status plan（而非取代 Herdr 状态）。

## 被放弃的方案

1. **单纯延长固定重试次数（如 8 次改为 80 次）**：
   - 放弃原因：固定 10 秒间隔会导致高频轮询 sqlite，不仅增加无谓 I/O，而且重试间隔不平滑，缺乏弹性窗口治理。
2. **在 Herdr 未就绪时强行阻塞等待 sqlite**：
   - 放弃原因：违背 Herdsman 的响应式架构与单线程事件循环设计，极易造成 daemon 整体卡顿或 socket RPC 超时。
3. **Discard 时直接投影为 success**：
   - 放弃原因：在历史读取失败且超时的严重异常下，伪造成功会误导编排决策，引发下游错误操作；投影为 `stalled` 既打破了静默，又忠实反映了状态停滞。

## 老插件向后兼容性 (Backward Compatibility)

- 单元测试（`herdsman-pi-wake.test.ts`）已验证：未升级的老版本消费端如果将 `stalled` 视为通用非成功状态，将平滑降级（如判定为 `failed`），不会抛出反序列化异常。

## 回滚方案

若需回滚：
1. 本次修改集中于 `fix/notify-loss-waiting-history` 分支，未修改生产数据库 schema（`state.db`），纯逻辑与配置层变更。
2. 切回原分支重新构建发布，或设置环境变量 `HERDSMAN_STATUS_PLAN_TOTAL_WINDOW_MS=100000` 后执行 `systemctl restart herdsman.service` 使启动时配置解析重新生效，即可快速缩小等待窗口。

## 来源

- 事故定证及设计方案：工作分支 `fix/notify-loss-waiting-history` 任务书及 PRD。

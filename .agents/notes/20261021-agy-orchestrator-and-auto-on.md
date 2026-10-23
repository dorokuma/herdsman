# AGY 编排者支持与主代理自动启用 Herdsman（`HERDSMAN_AUTO_ON=1`）

日期：2026-10-21

## 背景与目标

用户希望达成以下两个核心目标：

1. 让 Antigravity CLI（`agy`）既能作为高质量被观察执行端（在回合结束时立即发送 `agent.turn.completed` 信号，消除 500ms 轮询等待延迟），也能作为当前 Herdr workspace 的总控编排者（支持 `herdsman on|off|status` 与自动唤醒注入 `[HERDSMAN AGENT UPDATES]`）。
2. 在启动交互式主代理（无论是 `pi` 还是 `agy`）时，自动为当前无主的 Herdr workspace 开启 Herdsman 监听，免去每次手动输入 `/herdsman on` 或 `herdsman on`；同时确保 `dispatch-agent` 派发的角色子代理（`scout` / `worker` / `planner` 等）绝不触发自动抢占，且在同一 workspace 打开第二个交互窗格时不会抢走已有 owner 的所有权。

## 架构决策

### 1. 不引入 Herdsman MCP Server，采用 CLI + 生命周期 Hook + 守护进程租约架构

- **原因**：`agy` 的 `mcp_config.json` 是按 `HOME` 全局生效的。若将 Herdsman 封装为 MCP Server，每个被派发的 `agy` 子代理窗格都会额外拉起一个长驻 Node MCP 进程，并将编排工具暴露给纯执行子代理，造成进程膨胀与上下文污染。
- **方案**：复用 `@dorokuma/herdsman` 现有 CLI 与守护进程 UDS RPC，新增：
  - `herdsman on [--if-unowned] [--json]`
  - `herdsman off [--json]`
  - `herdsman status [--json]`
  - `herdsman agy-hook <PreInvocation|Stop|WakeWaiter>`

### 2. Daemon 端支持 `subscriberKind: "pi" | "agy"` 与终端存活租约

- `pi` 扩展拥有进程内长连接 socket，断连后走 5s `#scheduleDisconnect` 宽限期。
- `agy` 的 CLI 与 Hook 是每次回合触发的短生命周期进程，不能在 socket 关闭 5s 后释放 owner。
- 因此在 `ObservabilityRpcServer` 中维护 `#agyPresenceByTerminal` 编排者租约：
  - 仅在非 `pi` 终端成功认领所有权（`agent.orchestrator.set({ enabled: true })`）或已持有所有权的 `agy` 终端重连注册时写入/刷新 `#agyPresenceByTerminal`；只读 `herdsman status`、子代理 `Stop`（`sessionRef.source === "agy-subagent"`）或 `pi` 窗格内的 CLI 调用绝不写入该租约，避免阻塞 `pi` 的 5s 断连释放。
  - `#hasTerminalPresence` 在检查活跃 socket 之余，对 `#agyPresenceByTerminal` 中的租约结合 `paneGeneration` 与 `connectedAt` 调 `agents.isPaneClosedSince` 校验（兼容带 `pane_generation` 的生产关闭墓碑与无代 legacy 墓碑）；只要对应 Herdr 窗格未关闭且未切换为非 `agy` 代理，即视为在线。
  - 当窗格关闭（`pane.closed` -> `retirePane`，优先取事件 `terminal_id`，缺失时回退取 `retired` 记录的 `agent.terminalId` 调 `releaseIfOwnerIdentity`）或在 `reconcileAgentLocations` 中检测到终端关闭/切换为其他代理时，自动清理 `#agyPresenceByTerminal` 并释放所有权。
- **UDS 连接进程与窗格 `cwd` 绑定（`peerBoundToPaneCwd`）**：
  - `agy` 触发 `hooks.json` 钩子或通过工具执行 `herdsman` CLI 时，子进程可能隔着 `sh -c` / wrapper 或 `hooks.json` 所在目录运行。因此 `peerBoundToPaneCwd` 允许沿父进程链向上最多检查 4 层（`MAX_CWD_ANCESTOR_DEPTH = 4`），且一旦任一祖先 `/proc/<pid>/cwd` 不可读即立即失败关闭（fail-closed）。

### 3. `agy` 双角色自适应生命周期 Hook（`src/agy/hook.ts`）

- **状态目录与原子写**：默认使用用户私有目录 `${HERDSMAN_HOME:-~/.herdsman}/agy-state`（`0700`），通过临时文件 + `renameSync` 原子持久化会话状态。
- **子代理判定**：通过 `CTXMODE_ROLE` 环境变量识别 `dispatch-agent` 派发的角色子代理。
- **PreInvocation**：
  - 子代理（`CTXMODE_ROLE` 非空）直接跳过。
  - 主代理注册 `"agy"` 在线状态；若 `HERDSMAN_AUTO_ON=1` 且本会话尚未尝试过自动认领、且当前 workspace 无活跃 owner，则自动调用 `agent.orchestrator.set({ enabled: true })`。
  - 若当前终端是 workspace owner，通过 `injectSteps: [{ ephemeralMessage: batch }]`（符合 `agy` protojson 规范）注入未消费的 `[HERDSMAN AGENT UPDATES]`。不在 `PreInvocation` 中提前 ack（因同一回合内工具调用会多次触发 `PreInvocation`），统一在回合正常结束的 `Stop` 中按升序逐条 ack。
- **Stop**：
  - 若 `terminationReason` 非空且不等于 `"NO_TOOL_CALL"`（如 `"USER_CANCELED"` 或 `"ERROR"`），将本回合已注入但尚未 ack 的 `deliveredEventIds` 从 `presentedEventIds` 回滚移除，确保下一回合重新投递而不丢事件。
  - 当 `terminationReason === "NO_TOOL_CALL"` 时，无论主代理还是子代理，均从 `transcriptPath` 提取最终回复文本并发送 `agent.turn.completed`（同时 `agent-index-service.ts` 的 `isReadyNonPi` 循环将该信号与 500ms 轮询 race，实现 0ms 唤醒落盘读取）。
  - 子代理以 `sessionRef.source = "agy-subagent"` 注册并发送完信号后直接退出，不写入 `#agyPresenceByTerminal` 也不覆盖主代理 `sessionRef`。
  - 主代理若为当前 workspace owner：先按事件 ID 升序逐条 ack 本轮已消费事件（遇到已作废事件自动跳过，遇到瞬态错误保留剩余未 ack 队列）；若有立即可投递的新事件则直接返回 `{"decision": "continue", "reason": "..."}` 同步续跑；若当前无待处理事件但同 workspace 下仍有活跃同伴代理，则递增 `turnEpoch`/`waiterEpoch`、以 `detached: true` + `stdio: "ignore"` 拉起后台 `herdsman agy-hook WakeWaiter` 并同步记录 `waiterPid`，随后 `Stop` 钩子立即返回 `{}` 退出，不阻塞主代理 TUI。

### 4. 主代理自动启用（`HERDSMAN_AUTO_ON=1`）防抢占机制

- **启动器注入**：
  - `/root/.local/bin/agy`：当 `[ -z "$ROLE" ]` 且 `HERDR_ENV=1` 时默认导出 `HERDSMAN_AUTO_ON=1`。
  - `/root/.pi/agent/bin/pi`：当未传入 `--name role-*` 且未指定 `/tmp/herdr-role-sessions/*` 且 `HERDR_ENV=1` 时默认导出 `HERDSMAN_AUTO_ON=1`。
- **仅认领无主工作区**：
  - `packages/herdsman-pi/src/index.ts` 与 `src/agy/hook.ts` 均只在每个会话首次注册且 `!response.state?.owner` 时自动发起 claim，既保证开新主对话默认开启 Herdsman，又不会在同工作区开第二个主代理窗格时抢夺所有权，也不会在用户手动 `/herdsman off` 或 `herdsman off` 后因重连而意外重开。

### 5. V2 修订：分离式后台 `WakeWaiter` 与完全异步唤醒（2026-10-22）

- **问题根因**：V1 在 `Stop` 钩子内执行长阻塞 `while` 循环等待子代理完成，导致 `agy` 主代理卡在 `Running Stop hooks...`，子代理运行期间用户无法在主代理终端交互或插话。
- **V2 分离式后台 `WakeWaiter` 方案**：
  - **只读客户端零改动与 V2 相对 V1 守护进程无新增改动**：全分支完全不修改 src/herdr/socket-client.ts（保持 v0.7.5 只读边界）；src/daemon/observability-server.ts（#agyPresenceByTerminal 租约）与 src/daemon/client.ts（onNotification 推送回调）属本分支 V1 阶段既有改动，V2 修订相对 V1 未再对守护进程与 RPC 客户端新增任何修改。
  - **`Stop` 非阻塞**：`Stop` 完成 `agent.turn.completed` 与 `flushDeliveredAcks` 后，若 `takeNextWakeBatch` 有即时事件则同步返回 `{ decision: "continue", reason }`；若无即时事件且存在活跃同伴，则递增 `turnEpoch` 与 `waiterEpoch` 落盘，拉起分离式后台 `herdsman agy-hook WakeWaiter` 后**同步立即写入 `waiterPid` 并落盘**，随即返回 `{}` 释放主代理输入框。
  - **Epoch Fencing、`awaitingNextTurn` 与写序容错**：`Stop` 钩子在回合结束（含 `NO_TOOL_CALL` 及非 `NO_TOOL_CALL` 取消/错误分支）持久化 `awaitingNextTurn = true`，兼容 `agy` 在部分场景下 `invocationNum` 为会话级递增计数（非每回合重置为 1）的行为；每次新回合首轮 `PreInvocation`（`awaitingNextTurn === true`、`invocationNum === 1` 或未传、存在 `waiterPid`、或 `turnEpoch` 未初始化）精确推进 `turnEpoch` 并清除 `awaitingNextTurn`/`waiterPid`/`waiterEpoch`。`WakeWaiter` 每轮校验 `turnEpoch === waiterEpoch`，首轮（`isFirstIteration`）容忍父进程 `waiterPid` 写序交错（仅校验 `turnEpoch`），次轮起同时校验 `waiterPid === process.pid`，一旦失配立即静默退出。
  - **主代理 `idle`/`done` 门控（`selfReadyToWake`）与无副作用只读窥探（`peekNextWakeBatch`）**：实测 Herdr 在主代理回合结束返回提示符时会将窗格状态置为 `"done"`（或 `"idle"`），因此 `selfReadyToWake` 兼容 `selfAgentStatus === "idle" || selfAgentStatus === "done" || selfAgentStatus === undefined`。`WakeWaiter` 先调只读 `agent.list` 检查同伴状态与自身窗格状态，仅在 `selfReadyToWake` 为真且满足检查条件（首次检查、收到推送通知、唤醒重试或已无活跃同伴）时才调用 `agent.orchestrator.get`，避免主代理 `working` 期间触发 `pending()` 的 `reservePending` 副作用。`peekNextWakeBatch` 在 `outcomes.length === 0` 时静默 ack 被抑制事件（如 429 报错或空文本完成）而不触发唤醒；在 `outcomes.length > 0` 时**不发 ack、不写 `presentedEventIds` 或 `deliveredEventIds`**，仅调 `herdr agent prompt <paneId> "[herdsman-wake]"` 触发新回合，由新回合的 `PreInvocation` 注入 `[HERDSMAN AGENT UPDATES]` 并在该回合正常结束的 `Stop` 中按序 ack。
  - **`inspectWorkspaceAgents` 未决回合检测与 `settleGraceMs`（默认 12s）收尾宽限期**：`inspectWorkspaceAgents` 除判断 `peer.agentStatus === "working"` 与新代理 `newAgentGraceMs`（默认 15s，覆盖所有非 `working` 初始状态）外，还结合 `peer.history` 检测未决回合（`lastUserMessage` 存在但 `lastAssistantMessage` 尚未刷盘，或 `userMs > assistantMs`，且在 10 分钟 `UNSETTLED_TURN_MAX_AGE_MS` 内）与近期历史更新（`updatedAt` 或 `lastAssistantMessage.timestamp` 在 `settleGraceMs` 内），确保 `Stop` 钩子不会因同伴状态先一步变为 `"done"`/`"idle"` 而漏派生 `WakeWaiter`。同时，当 `WakeWaiter` 观察到同伴曾活跃或工作区存在同伴（`sawActivePeers || hasPeers`）且后续 `hasActivePeers` 变为 `false` 时，维持默认 12s 的 `settleGraceMs` 收尾宽限期（期间继续响应 `agent.event` 推送与轮询），覆盖同伴 `lastAssistantMessage` 已落盘（`timestamp: null`）但守护进程 `agent.done` 事件尚未落库推送的窗口；宽限期届满仍无事件时自动清理自身 `waiterPid` 并退出。
  - **RPC 请求超时防护（`requestTimeoutMs`）**：`WakeWaiter` 对每次 `client.request(...)` 包装默认 15s `Promise.race` 超时，轮询循环内对瞬态 RPC 错误最多重试 3 次，连续失败或首包注册超时则关闭连接并打印 `console.warn` 退出，且在 `finally` 中兜底清理自身 `waiterPid`，防止守护进程异常挂起时孤儿 `WakeWaiter` 泄漏。
- **双审 Consider 项与边界说明**：
  - **TTY 终端注入在 `idle` / `done` / `working` 下的物理边界**：`herdr agent prompt <paneId> "[herdsman-wake]"` 本质是向目标窗格 PTY 写入文本加回车。通过 `WakeWaiter` 门控仅在主代理 `selfReadyToWake`（`idle` 或 `done`）且 `turnEpoch === waiterEpoch` 时触发，可将碰撞窗口收窄到毫秒级；若极端情况下用户恰好在主代理空闲时正在输入框打字尚未回车，终端层注入仍可能与未提交草稿拼接，属于终端按键注入机制的固有物理边界。
  - **`hooks.json` 中 `Stop` timeout 建议值**：由于 `Stop` 钩子已改为非阻塞（仅执行一次本地 UDS RPC 往返与后台进程派生），建议将 `~/.gemini/antigravity-cli/hooks.json` 中 `Stop` 的 `timeout` 从 `86400` 收紧为 `30` 秒。
  - **断连即退出退化为下次交互兜底**：当守护进程重启或 UDS 连接断开/超时，`WakeWaiter` 捕获异常打印 `console.warn` 后直接退出，未消费事件保留在守护进程 SQLite 队列中，待用户下次在主代理输入任何内容触发 `PreInvocation` 时自动兜底注入，事件不丢失。
  - **`herdsmanEntry` 解析规则**：`defaultSpawnWakeWaiter` 优先取 `resolve(process.argv[1])`（即当前正在执行的 `herdsman` CLI 入口脚本），若 `process.argv[1]` 为空则回退至相对于当前模块的 `../cli/herdsman.js`。

### 6. V2.1 补充修复：`antigravity-sqlite` 元数据时间戳解码、F1 0ms 死循环修复与关页保留完成事件投递（2026-10-23）

- **`antigravity-sqlite` `steps.metadata` 时间戳解码（`src/agent-history/antigravity-proto.ts` & `src/agent-history/antigravity-reader.ts`）**：
  - 真实 `agy` 会话 SQLite 的 `steps` 表仅含 `(idx, step_type, step_payload, metadata)` 列，不含 `created_at`/`timestamp` 列，导致 `timestampFrom(row.time)` 恒为 `null`，使第 2 轮起基于 `userMs > assistantMs` 与 `updatedAt` 的未决回合检测失效。
  - 新增 `decodeAntigravityMetadataTimestamp(value)` 解析 `steps.metadata` protobuf blob 中嵌套的 `google.protobuf.Timestamp` 子消息并取最大有效时间戳，回填至 `message.timestamp` 及 `compact.updatedAt`。
- **`WakeWaiter` F1 0ms 死循环修复、瞬时 RPC 容错与 `isPeerActiveOrSettling` 兜底（`src/agy/hook.ts`）**：
  - 修复 Must-Fix F1：在 `if (!hasActivePeers && !hasWakeBatch && selfReadyToWake && !retryWakePending)` 分支后补充 `else { settleDeadline = undefined; }`，防止 `settleDeadline` 倒计时期间主代理切换为 `working`（`!selfReadyToWake`）或触发 `retryWakePending` 后 `settleRemainingMs` 跌至 `0` 引发 `if (sleepMs <= 0) continue;` 的 0ms 同步死循环。
  - 在 `WakeWaiter` 循环内增加瞬时 RPC 错误重试（最多 `MAX_WAKE_PROMPT_ATTEMPTS = 3` 次），并在 `finally` 块中调用 `clearOwnWaiterPid(stateDir, sessionId)` 确保任何退出路径均清理自身 `waiterPid`。
  - `Stop` 钩子非 `NO_TOOL_CALL` 分支开头增加 `if (isDispatchedSubagent(env)) return {};`，避免子代理取消/报错污染会话状态。
  - `isPeerActiveOrSettling` 增加 `stopReason` 非终态（如 `"toolUse"`）检测，以及时间戳缺失时的 `parseRefStepIndex` 步号比较兜底（`userStep > assistantStep`）。
- **关页保留/孤儿完成事件二次读取放行（`src/db/agent-events.ts`）**：
  - 当同伴窗格关闭触发 `retirePane` 删除 `agents` 行（`ON DELETE SET NULL` 将 `agent_events.agent_id` 置为 `null`）后，保留的 `agent.done`/`agent.idle` 事件在首次被 `WakeWaiter` 的 `agent.orchestrator.get`（内部调 `reservePending`）读取时状态由 `'invalidated'` 变为 `'delivered'`（`deliverable = 1, agent_id = null`）。随后 `[herdsman-wake]` 触发 `PreInvocation` 二次读取时，原 `isDeliverableAgentEvent` 因 `status !== 'invalidated'` 且 `agent === undefined` 将其过滤丢弃。
  - 在 `isDeliverableAgentEvent` 与 `nextDeliverableAfter` 中同步支持 `isOrphanOutcome`（`agentId === null && deliverable === 1 && (type === "agent.done" || type === "agent.idle")`），确保孤儿完成事件及首次 `reservePending` 后的关页保留完成事件在 300s 新鲜度窗口内可被 `PreInvocation` 正常消费。

### 7. V2.1 审查缺陷修复：区分两类 Drain 保障 working 豁免、对齐 Pi 端 agent.discarded 中间态豁免与孤儿 interactive 抑制生效（2026-10-23）

- **区分启动/关停 Drain 与周期 Reconcile Drain，消除 15 分钟 working 豁免天花板（`src/observability/agent-index-service.ts` & `src/daemon/service.ts`）**：
  - 启动与关停 drain 时持久化 `agents` 表数据可能已陈旧不可信，维持不豁免（`allowWorkingExemption: false`，attempts 超限即收口）；
  - 周期 reconcile drain（`PeriodicReconcileScheduler` 每 15 分钟执行一次）运行在正常运行状态、数据新鲜，保留 working 豁免（`allowWorkingExemption: true`），通过 live agent 状态持续保护长任务中活跃执行的 agent；
  - 修复 drain 刷新失败路径直接调用 `store.markRetry` 导致丢失 options 与遗留 tracker 的问题，统一收口至 `#markPlanRetry` 封装；修复 `#clearWaitingTimer` 误清 `planProgressTrackers` 导致 stall 计数器每轮被重置的问题。
- **对齐 Pi 与 Agy 编排者消费面对 `agent.discarded` 的门控处理与 stall 收口（`packages/herdsman-pi/src/wake.ts` & `src/agy/hook.ts`）**：
  - 两端投影器（Pi 的 `outcomeKind` 与 Agy 的 `outcomeKind`）使用一致的门控语义：仅当 `payload.agent` 为 `"agy"`、或为空/`null`（鉴于 `PLAN_WAITING_HISTORY` 只能由 `agy` 的 plan 产生）时，豁免 `PLAN_WAITING_HISTORY` 与 `degraded` 且无 `fallbackOutcome` 的中间态（`return undefined`）；
  - Pi 自身子代理（`payload.agent === "pi"`）一律不豁免，即使带着 `degraded` 且无 `fallbackOutcome` 也必须如实报出真实失败；
  - 引入独立的 stall 关闭原因常量 `PLAN_STALLED`：当判定 stall 触发 plan 收口为 discarded 时写入 `reason = PLAN_STALLED`，两端投影器均将其映射为 `"failed"`，确保真正判死的 agent 能唤醒编排者，消除漏报。
- **补全交互式 Pi Agent 事件生产写入并消除孤儿 idle 绕过抑制（`src/observability/agent-index-service.ts` & `src/db/agent-events.ts`）**：
  - 在生成事件 payload 时通过 `isInteractivePiAgent(agent)` 显式写入 `interactive: true`，使退役/关页后的孤儿 `agent.idle` 能够被 `isDeliverableAgentEvent` 的 `payload.interactive === true` 守卫精确拦截；同时在 `nextDeliverableAfter` 确保孤儿事件无 `getAgent` 时亦执行 JS 门禁校验；
  - 更新 `agent-index-service.ts` 约 1071 行关于「孤儿 `agent.done` 永不投递」的陈旧失实注释。

### 8. V2.1 复审六项缺陷收口（修 A–修 F，2026-10-23）

1. **修 A（漏报级：stall 收口独立常量与唤醒上报）**：新增 `PLAN_STALLED` 常量。stall 收口写出 `agent.discarded` 时携带 `PLAN_STALLED`，Pi 与 Agy 两端投影器将其映射为 `"failed"` 正常唤醒编排者；`PLAN_WAITING_HISTORY` 维持纯中间态静默语义不变。
2. **修 B（一致性：`agent.discarded` 豁免门控对齐）**：`src/agy/hook.ts` 与 `packages/herdsman-pi/src/wake.ts` 统一门控为 `isAgyOrEmpty = agent === undefined || agent === "agy"`。Pi 自身子代理不豁免、如实上报。
3. **修 C（通用异常处理走重试封装）**：`agent-index-service.ts` `#runPlanRow` 通用错误 catch 改走 `#markPlanRetry` 并透传 options，保持 pending 时 throw 时序不变；working 中的 agy 遇通用异常享受 working 豁免保持 pending。
4. **修 D（设计确认：运行时重试环恢复豁免确属正确设计）**：运行时重试环（`#retryWaitingPlanRow`）在 daemon 正常运转期间执行，内存与 DB 数据鲜活，故始终保持 working 豁免（options 缺省即 `allowWorkingExemption: true`）；与启动期恢复重试按 stale 处理不予豁免的语义分立，不改代码行为，明确注释与笔记。
5. **修 E（Tracker 资源释放）**：`#salvageOrCancelMissingAgentPlan` 终态收口清理 `#planProgressTrackers`。
6. **修 F（双重 stall 防线：总等待时间上限）**：新增 `WORKING_PLAN_MAX_WAIT_MS = 30 * 60 * 1000`（支持 options/clock 注入缩短），与指纹连续 18 次不变并存，任一触发即判定 stall 写入 `PLAN_STALLED` 收口。



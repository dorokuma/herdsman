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
  - **Epoch Fencing 与写序容错**：每次新回合 `PreInvocation`（`invocationNum === 1` 或未传、或存在 `waiterPid`、或 `turnEpoch` 未初始化）推进 `turnEpoch` 并清除 `waiterPid`/`waiterEpoch`；`WakeWaiter` 每轮校验 `turnEpoch === waiterEpoch`，首轮（`isFirstIteration`）容忍父进程 `waiterPid` 写序交错（仅校验 `turnEpoch`），次轮起同时校验 `waiterPid === process.pid`，一旦失配立即静默退出。
  - **主代理 `idle` 门控与无副作用只读窥探（`peekNextWakeBatch`）**：`WakeWaiter` 先调只读 `agent.list` 检查同伴状态与自身窗格状态，仅在自身窗格已处于 `idle`（或测试环境未列出自身窗格）且满足检查条件（首次检查、收到推送通知、唤醒重试或已无活跃同伴）时才调用 `agent.orchestrator.get`，避免主代理 `working` 期间触发 `pending()` 的 `reservePending` 副作用。`peekNextWakeBatch` 在 `outcomes.length === 0` 时静默 ack 被抑制事件（如 429 报错或空文本完成）而不触发唤醒；在 `outcomes.length > 0` 时**不发 ack、不写 `presentedEventIds` 或 `deliveredEventIds`**，仅调 `herdr agent prompt <paneId> "[herdsman-wake]"` 触发新回合，由新回合的 `PreInvocation` 注入 `[HERDSMAN AGENT UPDATES]` 并在该回合正常结束的 `Stop` 中按序 ack。（注：WakeWaiter 在主代理 working 期间跳过事件检查，待主代理回落 idle 后依赖同伴后续 agent.event 推送通知或无活跃同伴 !hasActivePeers 兜底触发检查；因 isFirstEventCheck 延迟消费，事件不丢失。）
  - **RPC 请求超时防护（`requestTimeoutMs`）**：`WakeWaiter` 对每次 `client.request(...)` 包装默认 30s `Promise.race` 超时，超时即关闭连接并打印 `console.warn` 退出，防止守护进程异常挂起时孤儿 `WakeWaiter` 泄漏。
- **双审 Consider 项与边界说明**：
  - **TTY 终端注入在 `idle` / `working` 下的物理边界**：`herdr agent prompt <paneId> "[herdsman-wake]"` 本质是向目标窗格 PTY 写入文本加回车。通过 `WakeWaiter` 门控仅在主代理 `agentStatus === "idle"` 且 `turnEpoch === waiterEpoch` 时触发，可将碰撞窗口收窄到毫秒级；若极端情况下用户恰好在主代理 `idle` 时正在输入框打字尚未回车，终端层注入仍可能与未提交草稿拼接，属于终端按键注入机制的固有物理边界。
  - **`hooks.json` 中 `Stop` timeout 建议值**：由于 `Stop` 钩子已改为非阻塞（仅执行一次本地 UDS RPC 往返与后台进程派生），建议将 `~/.gemini/antigravity-cli/hooks.json` 中 `Stop` 的 `timeout` 从 `86400` 收紧为 `30` 秒。
  - **断连即退出退化为下次交互兜底**：当守护进程重启或 UDS 连接断开/超时，`WakeWaiter` 捕获异常打印 `console.warn` 后直接退出，未消费事件保留在守护进程 SQLite 队列中，待用户下次在主代理输入任何内容触发 `PreInvocation` 时自动兜底注入，事件不丢失。
  - **`herdsmanEntry` 解析规则**：`defaultSpawnWakeWaiter` 优先取 `resolve(process.argv[1])`（即当前正在执行的 `herdsman` CLI 入口脚本），若 `process.argv[1]` 为空则回退至相对于当前模块的 `../cli/herdsman.js`。


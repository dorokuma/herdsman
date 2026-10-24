---
status: active # active | superseded
superseded_by: ""
supersedes: ""
# 模块可选值: observability, daemon, cli, config, db, herdr, shared, herdsman-pi, herdsman-herdr-plugin, release
模块: "observability"
---

# agy 智能叫醒：中间回合抑制、常驻 WakeWaiter 与跨工作区派工

## 一句话结论

`agent.done` / `agent.idle` 只是"这一轮结束"，不是"这一批工作结束"：中间回合的完成必须靠实时 `working` 状态识别并静默 ack，`Stop` 无论是否已有 immediate batch / 是否有活跃同伴都必须留下常驻 `WakeWaiter`，且同一 Herdr session 下无 owner 的兄弟工作区的事件与 agent 列表要并入 session orchestrator 的可见范围；此外 `WakeWaiter` **必须继承 pane 工作目录**（`cwd`），不能继承 agy 执行 hook 时的 `hooks.json` 目录，否则它会被 daemon 的 `ConnectorPaneMismatchError` 拒连并在 `Stop` 后 50ms 内当场退出。

## 背景

生产 `w37:p2` 真实故障（事件 1118/1119/1126）：

1. 工人在同一 Stop-to-Stop 窗口内跑内部工具回合，先写 `agent.done`（1118，中间回合），随即写 `agent.status.changed`（1119，`from=done`、`to=working`）继续工作；9 分钟后才真正完成（1126）。
2. 主代理 `Stop` 调用 `takeNextWakeBatch` 时只看到 `agent.done`，把中间回合当作最终结果消费并 `return { decision: "continue" }`，**没有**拉起 `WakeWaiter`；宿主忽略了 `decision: continue`，场上没有 waiter，1126 到达时无人唤醒。
3. 脚本派工（`herdr tab create` + `dispatch-agent` + `herdr pane run`）在刚建窗/启动间隙不在 `agent.list` 里，`Stop` 的 `if (!hasActivePeers) return {}` 与 `WakeWaiter` 的"无同伴即退出"使等待窗口完全缺失。
4. 跨工作区派工时，`agent.list`、`orchestrator.pending()` / 事件 ack 都按 `workspaceId` 过滤，工人所在工作区若没有自己的 orchestrator owner，其事件永远不进入主代理的唤醒批次。

## 决策

1. **中间回合静默抑制**（`src/agy/hook.ts`）
   - `projectAgyOutcomes` 增加 `liveWorkingTerminalIds` 输入：某 `agent.done` / `agent.idle(from=working)` 的 `terminalId` 在实时 `agent.list` 中仍是 `agentStatus === "working"`，则该完成事件属于未决中间回合，进 `suppressedEventIds` 静默 ack，不参与唤醒。
   - 抑制只有这一个信号：`agent.status.changed` 行在 `isDeliverableAgentEvent` 里被显式排除，`connState.events` 永远不含该类型，因此**不存在**可用的「事件链 supersede」——早期版本里的 `findSupersededCompletionEventIds` 是运行时不可达的死代码，已删除，不要按它理解行为。`fetchLiveWorkingTerminalIds` 失败时降级为空集，不影响原有事件投影。
2. **`Stop` 恒定留下 WakeWaiter**（`src/agy/hook.ts`）
   - 即使 `takeNextWakeBatch` 产出了 immediate batch（`decision: continue`），也在返回前写 `turnEpoch`/`waiterEpoch`/`waiterPid` 并 spawn `WakeWaiter`；`hasActivePeers === false` 也不再早退，只有显式 `exitWhenNoPeers: true` 才恢复旧的"无同伴不 spawn / 提前退出"行为。
   - `WakeWaiter` 把 `deliveredEventIds`（已 delivered 未 ack）视为待唤醒批次（`hasUnackedDeliveredEvents`）；对"只由 unacked delivered 组成"的批次，先让出一个 poll 间隔等宿主自己进下一轮 `PreInvocation`（`turnEpoch` 递增即退出），未进才发 `[herdsman-wake]` 兜底。发兜底唤醒来之前把这批 id 从 `presentedEventIds` 移除（`rearmDeliveredEvents`），否则新回合的 `PreInvocation` 会因 presented 过滤而注入空批次。
3. **常驻待命而非轮询/退出**（`src/agy/hook.ts`）
   - 无活跃同伴时不再退出：靠 `ObservabilityRpcClient` 的 `agent.event` / `agent.orchestrator.changed` 推送唤醒，单次休眠拉长到 `DEFAULT_IDLE_POLL_INTERVAL_MS = 60_000`。
   - 退出条件收窄为：`turnEpoch !== waiterEpoch`、`waiterPid` 被接替、失去本地 owner、自身 pane 从 `agent.list` 消失（先见过 self 才判定）、或 `waitTimeoutMs`。
4. **跨工作区并入**（`src/observability/agent-orchestrator-service.ts`、`src/daemon/observability-server.ts`、`src/observability/schemas.ts`、`src/db/agent-events.ts`）
   - `AgentOrchestratorService.ownerlessSiblingScopes()`：同 session 下没有 owner 行的其它 workspace。
   - `agent.orchestrator.get/register` 的 `events`：owner 除自身 workspace 外一并扫描无主兄弟工作区（`pending({ extraScopes })`）；ack 允许这些工作区的事件。
   - **跨工作区 ack 只单行结算**：当被 ack 的事件 `workspaceId !== 请求方 workspaceId` 时，只调单主键 `markAcked(eventId)`，**绝不**调带 `{herdrSessionName, workspaceId}` 的范围式 `markAcked`，也**绝不**调 `scopes.ack()`——否则会把本工作区 `id <= eventId` 的 `pending`/`delivered` 行一起改成 `acked` 并把 `acked_event_id` 推过去，静默吞掉本工作区从未投递的事件。本工作区 `ackedEventId` 保持不变；已结算的兄弟行不会被重投（`listAfter` 只扫 `pending`/`delivered`/`retained invalidated`）。
   - **乱序守卫跨全部合并 scope**：事件 id 是全表单调的，因此无论 ack 的是本工作区还是兄弟工作区的事件，守卫（以及 `ack` 异常分支算 `expectedNextEventId`）都在 `[input, ...ownerlessSiblingScopes(input)]` 上做 `nextDeliverableAfter` 取最小 id：只要存在更小 id 的 `pending`（或保留态 `invalidated deliverable=1` 未投递给本 owner）候选，就抛 `ORCHESTRATOR_EVENT_OUT_OF_ORDER`。本工作区 ack 因此不会把游标推过兄弟工作区尚未投递的事件。
   - `agent.list` 新增可选 `includeOwnerlessWorkspaces`：`hook.ts` 的 `inspectWorkspaceAgents` / `fetchLiveWorkingTerminalIds` 都带上该参数。
   - `publishAgentEvent`：事件所在 workspace 无 owner 时，若该 session 只有唯一活跃 owner，就把推送路由给它；**多 owner 时不广播**（广播会让同一事件投给多个 orchestrator，破坏 exactly-once 与 ack 归属），保持 pull-only——各 owner 靠 `pending()` 轮询拉取：活跃期 `1.5s` 一轮（`DEFAULT_POLL_INTERVAL_MS`），空转待命期最多 `60s` 一轮（`DEFAULT_IDLE_POLL_INTERVAL_MS`）。首个 ack 者单行结算该事件，其余 owner 之后再拉到同一 id 时因行已 `acked` 而静默跳过（既不重复展示也不报错）。
   - `reclaimDelivered` 分支 (c) 的 in-flight 判定改为按 session 匹配 owner，跨工作区已投递事件不会在 owner 仍持有 session scope 时被提前回收重投。
5. **`WakeWaiter` 的 cwd 必须解析到 pane 目录**（`src/agy/hook.ts`）
   - 新增导出 `resolveWakeWaiterCwd(payload, { baseCwd?, pid? })`，优先级：`payload.cwd` → `payload.workspacePaths` 中第一个存在的目录（全都不存在时取第一个非空项）→ Linux 下沿 `/proc/<pid>/status` 的 `PPid:` 向上最多 4 层祖先里第一个 cwd 与 `baseCwd`（默认 `process.cwd()`）归一化后不同的目录（与 `peerBoundToPaneCwd` 的深度对齐）→ `baseCwd`。
   - `AgyHookPayload` 新增 `workspacePaths?: unknown`（agy 的 `Stop` payload 用它携带窗格目录）；`Stop` 分支 spawn `WakeWaiter` 时用 `cwd: resolveWakeWaiterCwd(payload)`，不再用 `nonEmptyString(payload.cwd) ?? process.cwd()`。
   - 实测根因链：agy 按官方规范把执行全局 hook 的子进程 cwd 固定为 `hooks.json` 所在目录（`hooks.md:131`："The working directory is set to the directory containing `hooks.json`"），产线上就是 `/root/.gemini/config`；`Stop` 用 `detached: true` 拉起 `WakeWaiter` 后立即退出，子进程被 PID 1 收养（`PPid=1`）；`WakeWaiter` 连 daemon 调 `agent.orchestrator.register` 时，`ObservabilityRpcServer.#assertConnectorBoundToPane` → `peerBoundToPaneCwd(peerPid, pane.cwd)` 先查 `step=0`（自身 cwd = `/root/.gemini/config` ≠ `/root/workspace/pi-extensions`），再取 `PPid=1` 立即终止循环返回 `false`，抛 `ConnectorPaneMismatchError("Connector process does not match Herdr pane")`；`WakeWaiter` 在 `catch` 中吞掉该异常（`stdio: "ignore"` 还会丢掉 stderr 警告），`finally` 里 `clearOwnWaiterPid` 删掉 `waiterPid` 后退出，导致每次 `Stop` 结束后不到 50ms waiter 就消失。
   - 对照：`PreInvocation` / `Stop` **同步运行**时没有这个问题——它们的父/祖先进程还是窗格里的 `agy` 主进程（`/proc/<pid>/cwd` 为 pane 目录），祖先追溯能匹配；只有被 `detached` + 立即退出孤立成 `PPid=1` 的 `WakeWaiter` 会掉出追溯链。

## 边界与权衡（残余窗口与已知取舍）

1. **跨工作区 ack 只单行结算**：ack 无主兄弟工作区的事件时只调单主键 `markAcked(eventId)`，不调范围式 `markAcked`、不调 `scopes.ack()`，本工作区 `ackedEventId` 保持不变。乱序守卫覆盖 `[input, ...ownerlessSiblingScopes(input)]` 全部合并 scope（取最小可投递 id 比较），因此本工作区的 ack 也不会把游标推过兄弟工作区未投递的事件。
2. **多 owner 会话下无主兄弟工作区不发广播 push**：广播会把同一事件投给多个 orchestrator，破坏 exactly-once 与 ack 归属，所以退化为 pull——各 owner 靠 `pending()` 轮询拉取（活跃期 `1.5s`/轮，空转待命期最多 `60s`/轮）。首 ack 者单行结算后，其余 owner 再拉到同一 id 时因行已 `acked` 而静默跳过；代价是该事件在被拉取前最多延迟一个轮询周期（最长 `60s`），且不保证是哪个 owner 先拿到。
3. **主窗格离开索引而尚未触发 `agent.orchestrator.changed`**：常驻 `WakeWaiter` 不会即时退出，最多在当前轮询周期结束（`1.5s` 或 `60s`）时退出，并在 `finally` 里 `clearOwnWaiterPid` 清理 `waiterPid`；这段窗口内可能多跑一次空轮询 RPC，但不会误唤醒（唤醒仍需可投递批次）。
4. **根因一的残余极窄窗口**：中间回合抑制只依赖实时 `agentStatus === "working"`（事件链不可用，见下方「被放弃的方案」）。若 `peek` 恰好落在「中间回合 `agent.done` 已落库」与「下一回合状态翻转为 `to=working`」之间的毫秒级空隙，实时状态仍是 `done`/`idle`，该中间完成仍会被当作最终结果投影，仍可能触发一次提前唤醒。这是已知残余风险，没有可靠第二信号可补，不做额外兜底。

## 被放弃的方案（必填）

- **只靠事件链识别中间回合**：`agent.status.changed`（含 `to=working`）在交付层被明确排除（`isDeliverableAgentEvent` 直接返回 `false`，`publishAgentEvent` 也不推），hook 拿到的 `events` 里永远不会有它，事件链 supersede 逻辑运行时不可达；反过来只用实时状态则漏掉「peek 恰在中间回合 `agent.done` 落库与下一回合 `to=working` 翻转之间」的毫秒级窗口。因此只用实时 `agent.list` 的 working 集合做抑制，并如实记录该残余窗口（见下方「边界与权衡」）。
- **`WakeWaiter` 基于 RPC 长连接命令（服务端主动 wake）**：需要新增 RPC method 与鉴权/生命周期语义，且 daemon 重启即断；保持"hook 侧等待 + push 通知"更贴近现有架构，改动力度小。
- **跨工作区广播给所有 owner**：同一 session 多 owner 时会把同一事件投给多个 orchestrator，违反 exactly-once 交付与 ack 语义；改为"唯一活跃 owner 才 push，其余 pull"。
- **在 `pending()` 里直接放开 workspace 过滤（`all`）**：会把有自身 owner 的工作区事件也塞给主 owner，造成双投递；只在"无 owner 的兄弟工作区"这一条件下放开。
- **让 `WakeWaiter` 收到 unacked delivered 就无条件立即 prompt**：在宿主正常响应 `decision: continue` 时会产生重复叫醒（新回合已带上同一批更新）；用一个 poll 间隔的 grace + `turnEpoch` 守卫消除重复，同时保留"宿主忽略 continue"的兜底。
- **在 daemon 侧放宽 pane 绑定校验（或对 `agy-hook` 来源跳过 cwd 校验）**：会把跨窗格伪造/串窗的 RPC 一并放行，等于拆掉 exactly-once 与 orchestrator owner 归属的边界；根因在 hook 侧拉起子进程时丢了 pane cwd，应在源头修正。
- **只信 `payload.cwd`，不给 `workspacePaths` / 祖先 cwd 兜底**：真实 `Stop` payload 根本没有 `cwd` 字段，只信它等于继续用 `process.cwd()`（产线即 `/root/.gemini/config`），修复无效。
- **让 `Stop` 先 `chdir` 到 pane 目录再 spawn**：`Stop` 自身并不需要该 cwd，改全局进程状态属于隐式副作用，且 `workspacePaths` 之外仍要处理缺字段的情况；显式解析并只传给子进程更可控。

## 来源

- 生产事件证据（`w37:p2`）：`~/.herdsman/state.db` 中 `agent.done` → `agent.status.changed(from=done, to=working)` → 后续 `agent.done` 序列。
- 生产事件证据（`w37:p1`，`/root/.herdsman/state.db` 真实行，`status_event_plans` / `agent_events`）：
  - `id=72458`：15:23:07 创建，`last_attempt` 直到 **15:35:56** 才发生；
  - `id=72502`：16:08:30 创建，`last_attempt` 直到 **16:28:28**；
  - `id=72504`：16:15:37 创建，`last_attempt` 同样等到 **16:28:28**。
  - 三次窗口内**均无 `WakeWaiter` 进程存活**（0.5s 探针复现证实），事件要等到用户 16:28:28 发消息触发下一轮 `PreInvocation` 才被消费——与上面的 `ConnectorPaneMismatchError` 退出链一致。
- agy 规范：`hooks.md:131`（hook 子进程 cwd = `hooks.json` 所在目录，即 `/root/.gemini/config`）、`hooks.md:152`（`Stop` stdin payload 用 `workspacePaths` 而非 `cwd`）。
- 代码：`src/agy/hook.ts`（`Stop` / `WakeWaiter` / `resolveWakeWaiterCwd` / `projectAgyOutcomes` / `inspectWorkspaceAgents`）、`src/daemon/observability-server.ts`（`#assertConnectorBoundToPane`）、`src/shared/unix-peercred.ts`（`peerBoundToPaneCwd` 的 4 层祖先追溯）、`src/observability/agent-orchestrator-service.ts`、`src/db/agent-events.ts`、`src/observability/schemas.ts`。
- 回归测试：`test/unit/agy-hook-and-cli.test.ts`（中间回合抑制（仅实时 working）、`decision: continue` 兜底与 `PreInvocation` 取消、脚本派工启动期常驻、`Stop` 从 `workspacePaths` 解析 `WakeWaiter` cwd、跨工作区无主工作区可见性）、`test/integration/agent-orchestrator-service.test.ts`（有主兄弟工作区仍属 out-of-scope；交错 id 的跨工作区 ack：两个方向都抛 `ORCHESTRATOR_EVENT_OUT_OF_ORDER`；跨工作区 ack 只单行结算、不推进本工作区游标且不吞掉延后事件）与 `test/integration/observability-rpc.test.ts`（唯一活跃 owner 时推送无主兄弟工作区事件；两个 owner 时退化为 pull，不推送）。

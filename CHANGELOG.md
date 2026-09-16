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

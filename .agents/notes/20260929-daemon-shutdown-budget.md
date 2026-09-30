---
status: active # active | superseded
superseded_by: ""
supersedes: ""
# 模块可选值: observability, daemon, cli, config, db, herdr, shared, herdsman-pi, herdsman-herdr-plugin, release
模块: daemon
---

# daemon 关闭序列加总预算：超时只 warn、不 fail，且必须先 abort 在飞等待

## 一句话结论

关闭序列（`src/daemon/service.ts` 的 `stop()`）现在跑在一个 `SHUTDOWN_BUDGET_MS = 5000` 的总预算下，每一步单独限时（耗尽预算的步骤仍得 `SHUTDOWN_MIN_STEP_MS = 250` 的兜底），**超时只记 warn 并继续下一步、退出码仍为 0**；同时 daemon 持有一个 shutdown `AbortController`，`stop()` 一开始就 abort 在飞等待（index 的历史等待窗口 / turn wait、watch manager 的 watcher），并把 `socket-client` / `session list` 的外部调用加上 2s 超时。

## 背景

- 事故：daemon 收到 SIGTERM 后 10s 内不退出，被 systemd `TimeoutStopSec=10` SIGKILL。被 SIGKILL 就会跳过 `stop()` 的 `finally`（释放实例锁、删 pid 文件、`process.exit`），并留下 flock 助手子进程。
- `stop()` 原本依次 `await` 四个 promise（`index.drainInFlightPlans()` → `reconcileScheduler?.stop()` → `watchManager.stop()` → `server.stop()`），**全无超时**：任一不返回就永不退出。可达的长等待有三处：`agent-index-service` 的历史等待窗口 `maxTotalMs = 30000`（关停不触发 abort）；`socket-client` 的请求无超时（对端不回包即永不 settle），`session-list` 的 `execFile` 无 timeout；`herdr-session-watch-manager` 的 `PLAN_DRAIN_GRACE_MS = 12_000` 本身已大于单元停止超时。
- 关闭路径当时没有任何日志，事故期间 journal 空白，无法定位卡在哪一步。

## 决策

1. **总预算 + 每步限时**：`runShutdownSteps()`（导出以便直接单测）用一个 deadline 串起四步；每步 `timeoutMs = max(剩余预算, SHUTDOWN_MIN_STEP_MS)`，超时记 `warn` 后继续下一步。最坏总耗时 = `SHUTDOWN_BUDGET_MS + (步数-1) * SHUTDOWN_MIN_STEP_MS`，仍远小于 `TimeoutStopSec`；`finally` 的清理不受预算影响，一定执行。
2. **超时只算 warn，不计入退出码**：退出码仍由「步骤抛非超时错误」决定（保持原语义，原有 `exit(1)` 测试不变），超时一律 exit 0。理由是这类 SIGTERM 是**有意停止**，非零退出码会让 `Restart=on-failure` 把 daemon 拉回来；超时信息通过 `warn` 日志暴露。
3. **关停先 abort，再 drain**：`stop()` 先 `shutdownController.abort()` 再跑各步。`AgentIndexService` 收到 `shutdownSignal` 时 abort 全部 active waiter（等价于 pane closed 的处理：不补事件、plan 行保持可重试），之后新注册的 waiter 立即 abort；`HerdrSessionWatchManager` 收到同一 signal 时同步进入 `#beginShutdown()`（停 tick、abort 所有 watcher）。这样关停不再依赖 30s / 12s 这类窗口自然到期，也保证预算切掉 `watchManager.stop()` 那一步时 watcher 仍被拆掉。
4. **外部调用加超时**：`HerdrSocketClient` 的 `#request` / `#requestOnce` 加 `HERDR_REQUEST_TIMEOUT_MS = 2000`（请求超时=失败，成功路径语义不变），`createHerdrSessionListRunner` 的 `execFile` 加 `HERDR_SESSION_LIST_TIMEOUT_MS = 2000`。
5. **`PLAN_DRAIN_GRACE_MS` 12000 → 3000**：必须 ≤ 关闭预算，否则 grace 比单元停止超时还长。常量留在 watch manager 内（不 import service 以免循环依赖），由 `test/unit/daemon-service.test.ts` 的断言守住两者关系。
6. **第二次 SIGTERM 幂等**：第一次 SIGTERM 进入 `stop()` 时就重新挂上 SIGINT/SIGTERM 处理函数（`once` 已被消耗），第二个信号走最小清理（删 pid / 删 socket / 释放锁）后 `exit(0)`，不再落到 Node 默认动作而留下残留。
7. **关停全链路日志**：进入、abort、每步开始（含剩余预算与限时）、每步结束（耗时）、超时、结束（总耗时与退出码）都有 info/warn；只输出步骤名、毫秒数以及 `pid` / `exitCode` 这类非敏感运行标识，不含任何凭据或外部内容。
8. **在飞 tick 的 rejection 就地兜住**：`HerdrSessionWatchManager` 的 `#tickInFlight` 赋值处即挂 `.catch` 并 warn（`step: "active-revision-poll"`），构造函数里 `void this.#beginShutdown()` 同样补 `.catch`（`step: "begin-shutdown"`）。只靠 `stop()` 里的 `.catch` 不够：关停预算切掉 `watchManager.stop` 那一步时 `stop()` 走不到自己的 `.catch`，在飞 tick 抛错就会变成未处理 rejection。而 `stop()` 一开始就把 daemon 注册的 `unhandledRejection` / `uncaughtException` 监听器 `off` 掉了（`process.off(...)`），此时这个 rejection 没有任何监听器，于是落到 Node 的默认行为：把异常抛出并终止进程，**exit code 1**；`finally` 的清理（pid 文件 / 实例锁 / 关停结束日志）随之被跳过。（`src/daemon/herdr-session-watch-manager.ts` 里 `#tickInFlight` 赋值处的注释按同一因果链校正：监听器已被移除 → 默认 throw → exit 1。）

## 被放弃的方案（必填）

- **只缩短那些 magic number（30s / 12s / 2s）**：治不了「任一步永不返回」这一类问题（例如 watcher 卡在一次 `server.stop()`/`sessionSnapshot` 上），必须有一个不依赖具体步骤的总兜底。
- **超时即 `exit(1)`**：语义上像失败，但会让 systemd 的 `Restart=on-failure` 在停止流程后重新拉起 daemon；且此时进程本身就处在被 SIGKILL 的边缘，退出码不是可靠信号。改用 warn 日志。
- **等分预算（每步 `budget/步数` 硬上限）**：会让 `watchManager.stop()` 的 grace 被压到份额以下，grace 与常量自相矛盾；改用「剩余预算 + 每步兜底」，先完成的步骤把预算让给后面的步骤。
- **把 shutdown signal 透传进 `refreshHerdrSession` / `sessionSnapshot` 取消在飞刷新**：需要改 client 与 registry 的签名（`Pick<HerdrSocketClient, "close" | "sessionSnapshot">` 等被测试广泛 stub），风险大于收益；改为给 socket 层加超时，把在飞刷新限制在 2s 内（E2E 实测 watch manager 步 ≈1.6s）。
- **`server.stop()` 主动 `closeAllConnections()`**：能避免被空闲 RPC 客户端拖住，但会改变既有服务的连接语义，超出本批范围；目前由预算兜底（该步超时则 socket 文件留给下次启动的 stale-socket 清理路径，已有测试覆盖）。
- **第二次 SIGTERM 直接 `process.exit(0)`（不清理）**：仍会留下 pid / socket / 锁残留，正是本批要消除的现象；改为做最小清理再退出。

## 遗留清单 / Known leftovers

### 已修（本批，两关点名的应修项）

- **在飞 tick 的未处理 rejection**：`#tickInFlight` 赋值处就地 `.catch` + warn（步骤名/原因，无敏感值）；`src/daemon/herdr-session-watch-manager.ts`。回归用例：`test/unit/herdr-session-watch-manager.test.ts` →「logs a failing active-revision tick and stop() still resolves」（修复前：`process.on("unhandledRejection")` 捕获到 `Error: session list boom` 且无 warn；修复后：warn 命中、无未处理 rejection、`stop()` 正常 resolve）。
- **`execFile` 的 timeout 不是硬上限**：`createHerdrSessionListRunner` 补 `killSignal: "SIGKILL"`（`src/herdr/session-list.ts`），timeout 值/参数/返回语义不变。原因是默认 `killSignal=SIGTERM` 可被子进程无视，且迟到成功时回调没有 error、会被当成功。

### 已知未闭环（逐条）

1. **`daemon-process-manager` 偶发 flake（与本批无关）**：`pnpm test` 中 `stress test: 200 rounds…`（5s 上限，起两个 tsx 子进程）偶发超时失败；复跑与单跑该文件均通过。未定位、未修。
2. **关停 abort 会消耗 plan 重试预算（语义取「关停中断 ≈ 一次失败重试」，本批接受并记录风险）**：`abortPendingWaits()` 让 waiter 抛 `PlanWaitingHistoryError`，`StatusEventPlanStore.markRetry`（`src/db/status-event-plans.ts`）据此把 `attempts` 从当前值 `+1`（**每一次 `markRetry` 都累加一次 attempts**，不区分起因，关停中断照计）。`attempts >= STATUS_PLAN_MAX_ATTEMPTS`（8）时按 `last_error` 分流：`PLAN_WAITING_HISTORY` → `discarded` 终态，其他（如 `degraded`）→ `failed`。
   - `discarded` **不是「静默丢弃、不补事件」**：它只是与 `failed` 并列的一个终态。行落到该终态后不再重试，也不再补发它原本要表达的那次状态迁移事件（例如 `working → done`）；但 drain 结束时 `#backfillDiscardedPlanEvents()` 会按幂等键 `agent.discarded:plan:<id>` 补写一条 `agent.discarded` 事件落库（`#appendPlanDiscardedEvent`），并有 `console.info("Herdsman status event plan discarded after max attempts")` 记录。准确表述是：**落库事件有（`agent.discarded`），但它按设计不触发下游 wake**（观察者放弃与 agent 崩溃解耦，详见 [20260917-status-event-plan-discarded-lifecycle.md](20260917-status-event-plan-discarded-lifecycle.md)）。
   - 风险：若一天内重启 8 次且每次都撞上在飞 plan，这些 plan 会落到 `discarded`——原本的 `working → done` 状态迁移不会再有事件补上，观察者最多看到一条不触发唤醒的 `agent.discarded`。这是部署日频繁重启下的理论加速。
   - 可选后续方案：在 `markRetry` 里区分「关停中断」并不计入 attempts（或不计入 discarded 判定）。
3. **`sqlite.close()` 与 `finally` 段不在关停预算内**：两者都在 `runShutdownSteps()` 之后/之外，预算耗尽也一定执行（有意为之）。本机 `close()` 为 ms 级、WAL 有 autocheckpoint 上界，未测最坏情况。后续可给关停日志补 `sqlite.close()` 耗时（当前该段无耗时日志，`shutdown finished` 的 `elapsedMs` 不含它）。
4. **启动失败 catch 分支的清理链仍是无预算 await**：`runObservabilityDaemonService` 的启动 catch 里依次 `await reconcileScheduler?.stop() / watchManager.stop() / server.stop()`，没有 `runShutdownSteps()` 的预算兜底；本批只给它加了 `shutdownController.abort()`。触发概率低，但某步卡住仍是无限等待。
5. **`SHUTDOWN_BUDGET_MS` 硬编码，前提是本单元 `TimeoutStopSec=10s`**：代码里没有对 unit 的运行时校验。换机器、或加 drop-in 把 `TimeoutStopSec` 改小到接近预算 + `(步数-1) * SHUTDOWN_MIN_STEP_MS` 的 overshoot 时，预算不再安全。
6. **测试里的步数手工同步**：`test/unit/daemon-service.test.ts` 按 4 步断言 overshoot 上界；将来加第 5 步不会被自动发现（没有从 `stop()` 的步骤数组导出的步数常量）。
7. **2000ms 超时同样作用于常规刷新路径（不只关停）**：`HERDR_REQUEST_TIMEOUT_MS`（socket-client）与 `HERDR_SESSION_LIST_TIMEOUT_MS`（session-list）在正常运行时也生效。正常耗时有两个数量级余量，验证判据 = journal 中 `timed out after 2000ms` 的出现频率。降级行为安全（超时→失败→重连/重试），唯一例外是「迟到成功被当成功」，已由本批应修项「`execFile` 的 timeout 不是硬上限」堵掉。
   - **例外且致命：startup 路径上的 session-list 超时不是降级**。`watchManager.start()` → `rescanNow()` → `sessionList()`（`src/herdr/session-list.ts` 的 `execFile`）一旦超时即抛错，`start()` 的调用位于 `runObservabilityDaemonService` 的启动 `try` 内，于是走启动 catch 分支：清理（abort + watchManager.stop + server.stop + pid / 锁 / sqlite）后把错误 `throw` 出去，最终由 `src/cli/herdsman-daemon.ts` 的 `main().catch` 打错误并 `exit(1)`。也就是说，**2s 的 session-list 超时在启动路径上是致命失败（exit 1），systemd 会按 `Restart=on-failure` 重启**（表现为启动—失败—重启的循环），而不是运行时降级。只有 daemon 已启动之后的调用才落在「超时→失败→重连/重试」的降级语义里。
8. **测试盲区**：缺「对端慢但会在超时前成功返回」的用例（超时值与真实耗时之间的余量没有被测试守护），也缺 `killSignal` 生效的用例（本批受「至多一条测试」约束，只加了 tick 错误分支的用例）。
9. **`stop()` 的 `finally` 不 force-rm socket 文件**：`onSecondSignal` 才会 `rmSync()` socket；被预算截断（如 `server.stop()` 超时）时，socket 文件留给下次启动的 stale-socket 清理路径（已有测试覆盖）。这是有意取舍，不是遗漏。
10. **第二意见新增观察项（可选改进，本批不改）**：
    - **`raceShutdownStep` 超时后被放弃的步骤若随后抛错，彻底无日志**：`Promise.race` 已经对两侧 promise 挂了反应，步骤在超时后抛出的 rejection 被 race 吸收，不会触发 `unhandledRejection`，而关停日志里也只有「step timed out」一条，看不出该步骤其实是失败退出的（除非它自己在内部记录）。可选改进：给 `finished` 挂一个 `.catch`，在超时之后记录一条「abandoned step failed after timeout」并带上原始错误。
    - **tick 失败日志可再补两点**：当前只记 `error.message`（丢了 `Error` 堆栈），且 `step` 恒为 `"active-revision-poll"`——`#tick()` 走 `full-rescan` 分支（`await this.rescanNow()`）失败时也是这个标签。可选改进：日志传 `Error` 对象本身以保留堆栈，并让 `step` 按分支取 `full-rescan` / `active-revision-poll`。
11. **文案/一致性小疵（不影响行为，待后续统一）**：
    - `src/daemon/service.ts`：`SHUTDOWN_BUDGET_MS` 的注释写「a single wedged step cannot run the sequence past this ceiling」，与 `SHUTDOWN_MIN_STEP_MS` 的 min-step overshoot（最多 `(步数-1) * 250ms`）自相矛盾；实际语义是「允许小幅越过预算，但仍远低于 `TimeoutStopSec`」。
    - `src/observability/agent-index-service.ts`：`#throwIfShutdownAbort` 之前打 `skipping status event generation because … aborted`（debug），但关停路径的实际行为是「保持 pending、下次启动重试」（抛 `PlanWaitingHistoryError`），文案与语义不符。
    - 本笔记「决策 7」原写「只输出步骤名与毫秒数」，与实际日志不符（`shutdown starting` 含 `pid`、`shutdown finished` 含 `exitCode`，均非敏感值）。已在本次更新中校正措辞。

## 来源

- 事故报告与代码勘察：daemon SIGTERM 10s 内不退出被 SIGKILL；本批为 `fix/daemon-graceful-shutdown-budget`（改动：`src/daemon/service.ts`、`src/daemon/herdr-session-watch-manager.ts`、`src/observability/agent-index-service.ts`、`src/herdr/socket-client.ts`、`src/herdr/session-list.ts`）。
- 回归测试：`test/unit/daemon-service.test.ts`（预算/跳过/清理/第二次信号）、`test/unit/herdr-session-watch-manager.test.ts`（shutdown signal 立即拆 watcher、在飞 tick 抛错不逃逸）、`test/integration/herdr-socket-client.test.ts`（静默对端超时）。
- 隔离目录端到端计时：假 herdr socket 只应答第一次 snapshot 后静默 → SIGTERM 后 1.7s 退出（修复前 30s 仍不退出，需 SIGKILL，pid/socket/lock owner 全部残留）。
- 本批（审查补修后）重跑同一隔离场景：假 herdr socket 接受连接后完全不应答 → SIGTERM 后 2.01s 退出（exit code 0；关停日志 `shutdown finished elapsedMs: 1971`，其中 `watchManager.stop` 步 1967ms，等的是被 2s 请求超时兜住的在飞 snapshot；`server.stop` 步 0ms）；pid 文件 / RPC socket / 实例锁 `owner.json` 均清除，flock 助手子进程无残留（唯一在跑的 flock 属于生产 `herdsman.service`，与本次无关）。

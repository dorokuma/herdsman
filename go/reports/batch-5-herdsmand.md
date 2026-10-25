# 第 5 批 · herdsmand 全量 Go 迁移 · 第 3C-2 收官子批报告

- 分支：`go-port`（worktree `/root/workspace/.wt/herdsman/go-port`）
- 目标模块：`go/`（Go module `github.com/dorokuma/herdsman/go`，go 1.26.0）
- 本子批范围：修复 `internal/daemon/service.go` 启动期 SIGTERM 竞态、`gofmt`、全套 `CGO_ENABLED=0` 与 `-race` 验证、`HERDSMAN_IMPL` 包装脚本与 PiG 隔离实测、本报告；其后追加第 4 轮 reviewer 修复（db 闸门 / observability / herdr / daemon 共 6 项，见第 6 节）与 4.2、4.3 两节描述修正；最后追加第 3D-3 精确收尾子批（`observability_rpc_slow_client_test.go` 填满 AF_UNIX `SO_SNDBUF` 并取得真实先 FAIL 后 PASS，见 6.5；`8c4bd1b` 更正为 `5fce550`，见 6.1）
- 未触碰：`/root/.local/bin/herdsman`、`herdsman.service`、`/root/.herdsman`（生产面保持原样，见第 5 节）

---

## 1. 提交列表（`go-port` 自 `main` 以来）

`git log --reverse --pretty=format:'%H %s' main..HEAD`（本子批提交除外，见本节末）：

| # | SHA | 说明 |
|---|-----|------|
| 1 | `a0d76b288bab23546e51eb8f29e3c4d8817fe93a` | feat(go): port shared, observability contracts, and config schema |
| 2 | `c57c3190a81697ed826954c5a7be9ffea7ed645b` | feat(go): port daemon process manager and runtime config resolver |
| 3 | `85fea4d6f1aa043c0cb92233c2b1ca34ba1dd029` | feat(go): port agent-history readers and antigravity parser |
| 4 | `f8cfdcdbf1e6b42262c0b355929a1f114ba45fa8` | feat(go): port agent-history discovery and caching service |
| 5 | `025220372a78b91c7331f5f452046eef59dffa8a` | feat(go): port sqlite client, schema, and foundational db stores |
| 6 | `553814ff13cc9f17f23b005aed2c6a4dbdd47829` | feat(go): port agent-events and agents db stores |
| 7 | `96f98f1b1788fd11236b96d6da6f8759cf0f548d` | feat(go): port herdr socket client, pane resolver, and rpc client |
| 8 | `f45388137c33346bf7b0f5bf16db17e7bb247f23` | feat(go): port agent orchestrator service and delivery lifecycle tests |
| 9 | `494e47b7e0ae9a76ad23d10a68d43919526af571` | feat(go): port agent context service and daemon event reconciler |
| 10 | `b3f86f55acdb303e34a393c6c8dd87497f90a307` | feat(go): port agent index service and pane generation dedup tests |
| 11 | `9aa05c337ca1154e27114a983a4ce7068ff03cf5` | feat(go): port herdr session watch manager and idle seam tests |
| 12 | `29cda845c1fa6b04c0ff365cd2b9324025c60f23` | test(go): port herdr session watch manager unit tests |
| 13 | `0d21f00137c1099042fadfebbe1d2361a4748af1` | feat(go): port observability rpc server and orchestrator disconnect/move tests |
| 14 | `47b3a12321200f81dd1f2f46fed76261dff940ad` | test(go): port observability rpc integration tests |
| 15 | `ae7035046c4e30ce0d9e3378888f4ce9634e702a` | feat(go): port daemon root service and herdsman-daemon entrypoint |
| 16 | `7093f06eacfa4329d54548e61a33c9f4d73bd2ad` | feat(go): port agy-hook, herdsman cli, and wrapper script |
| 17 | `83e14ed63ca9575b3891020fcbec41ca24d82080` | test(go): port agy-hook-and-cli unit tests part 1 |
| 18 | `bcf56d380179e00764684278689974b0f9a871b7` | test(go): port agy-hook-and-cli unit tests part 2 |
| 19 | `6af358c374e15305525321e3c883b31339024622` | Merge branch 'main' into go-port |
| 20 | `7d5da2eaceea92a081daf44f044fd0d1f34cd0ae` | feat(go): sync agy draft check, port index/turn-signal tests |
| 21 | `39bf1c63b7bd5c43e5399c29d203858a62d7901a` | fix(go): resolve daemon startup-signal race and add batch 5 report |
| 22 | `5fce55023119e8e1602585f4bed1640160932f39` | fix(go): resolve agy, cli, and agenthistory reviewer findings |
| 23 | `5e8b7c80ee8594affbc2e50d684ab34ed65dc0a5` | fix(go): resolve db, observability, herdr, and daemon reviewer findings |
| 24 | `b407ee30705be0bf95d0a9e9207f52b8e4ee6bf6` | test(go): saturate unix socket sndbuf in slow-client test and update report |

第 21～23 条是本子批前三轮 reviewer 修复，各轮内容见第 6 节。第 24 条（`b407ee3`，`test(go): saturate unix socket sndbuf in slow-client test and update report`）是第 3D-3 精确收尾子批：让 `observability_rpc_slow_client_test.go` 真正填满 AF_UNIX `SO_SNDBUF`（见 6.5 的修订）并修正 6.1 节对 `8c4bd1b` 的记载；该提交只动 `go/internal/daemon/observability_rpc_slow_client_test.go`（+26/-6）与本报告（+72/-14）。

第 25 条（`fix(go): close final review findings on db lifecycle, rpc ordering, and herdr deadlines`，本轮 3E 终审收口）解决 `reviewer-final` 的全部 8 项问题：`sqlite_gate.go`/`db/client.go` 的 `sql.DB.Close()` 生命周期、`observability_server*.go` 的锁外写顺序、`herdr/socket_client.go` 的并发请求写锁与独立写超时、`turn_completion_test.go` 与 `sqlite_migrations_test.go` 的确定性回归、`agent_context_service_test.go` 的真实 `OccupiedSessionPaths` 边界、三个独立测试文件的合并，以及本报告 4.3 / 6.1 / 6.6 三节的更正与新增专节（见 6.8、6.9）。

> **`8c4bd1b` 的更正**：任务书最初写作 `8c4bd1b`，经 `git show --stat` 核实，其对应的真实提交就是第 22 条 `5fce550`（`fix(go): resolve agy, cli, and agenthistory reviewer findings`）。6.1 节已据此改写，不再保留「短 SHA 不可解析」的旧结论。

变更面（`git diff --stat`，本子批）：

```
 go/internal/daemon/service.go | 168 +++++++++++++++++++++++++++++++-----------
 go/internal/db/agents.go      |   1 -
 2 files changed, 126 insertions(+), 43 deletions(-)
```

`go/internal/db/agents.go` 的 1 行删除是纯 `gofmt` 结果（`type FindAgentByTerminalInput = ...` 后多出的一个空行），无行为变化。

---

## 2. 源码与测试映射表

### 2.1 48 个 TS 源文件 → Go 包/文件

`src/**/*.ts` 共 48 个（`find src -name '*.ts' | wc -l` = 48）：

| # | TS 源文件 | Go 落点 |
|---|-----------|---------|
| 1 | `src/agent-history/antigravity-proto.ts` | `internal/agenthistory/antigravity_proto.go` |
| 2 | `src/agent-history/antigravity-reader.ts` | `internal/agenthistory/antigravity_reader.go` |
| 3 | `src/agent-history/discovery.ts` | `internal/agenthistory/discovery.go` |
| 4 | `src/agent-history/pi-reader.ts` | `internal/agenthistory/pi_reader.go` |
| 5 | `src/agent-history/readers.ts` | `internal/agenthistory/readers.go` |
| 6 | `src/agent-history/service.ts` | `internal/agenthistory/service.go` |
| 7 | `src/agent-history/source-fingerprint.ts` | `internal/agenthistory/source_fingerprint.go` |
| 8 | `src/agent-history/text.ts` | `internal/agenthistory/text.go` |
| 9 | `src/agent-history/tool-compaction.ts` | `internal/agenthistory/tool_compaction.go` |
| 10 | `src/agy/hook.ts` | `internal/agy/hook.go` + `state.go` + `wake_waiter.go` |
| 11 | `src/cli/herdsman-daemon.ts` | `cmd/herdsman-daemon/main.go` |
| 12 | `src/cli/herdsman.ts` | `cmd/herdsman/main.go` + `internal/cli/herdsman.go` |
| 13 | `src/config/load.ts` | `internal/config/load.go` |
| 14 | `src/config/runtime.ts` | `internal/config/runtime.go` |
| 15 | `src/config/schema.ts` | `internal/config/schema.go` |
| 16 | `src/daemon/agent-event-reconciler.ts` | `internal/daemon/agent_event_reconciler.go` |
| 17 | `src/daemon/client.ts` | `internal/daemon/client.go` |
| 18 | `src/daemon/herdr-session-watch-manager.ts` | `internal/daemon/herdr_session_watch_manager.go` |
| 19 | `src/daemon/observability-server.ts` | `internal/daemon/observability_server.go` + `observability_server_handlers.go` |
| 20 | `src/daemon/process-manager.ts` | `internal/daemon/process_manager.go` |
| 21 | `src/daemon/service-supervision.ts` | `internal/daemon/service_supervision.go` |
| 22 | `src/daemon/service.ts` | `internal/daemon/service.go` |
| 23 | `src/db/agent-context-snapshots.ts` | `internal/db/agent_context_snapshots.go` |
| 24 | `src/db/agent-events.ts` | `internal/db/agent_events.go` |
| 25 | `src/db/agent-history-cache.ts` | `internal/db/agent_history_cache.go` |
| 26 | `src/db/agent-orchestrator-scopes.ts` | `internal/db/agent_orchestrator_scopes.go` |
| 27 | `src/db/agents.ts` | `internal/db/agents.go` |
| 28 | `src/db/apply-migrations.ts` | `internal/db/apply_migrations.go` |
| 29 | `src/db/client.ts` | `internal/db/client.go` + `internal/shared/sqlite_transaction.go` |
| 30 | `src/db/herdr-sessions.ts` | `internal/db/herdr_sessions.go` |
| 31 | `src/db/herdr-workspaces.ts` | `internal/db/herdr_workspaces.go` |
| 32 | `src/db/migrate.ts` | 无独立文件：`db:migrate` 开发脚本（openSqlite + applyMigrations），Go 侧能力由 `internal/db/apply_migrations.go` + `internal/db/client.go` 提供；daemon 启动时即调用 `ApplyMigrations` |
| 33 | `src/db/schema.ts` | `internal/db/schema.go` |
| 34 | `src/db/status-event-plans.ts` | `internal/db/status_event_plans.go` |
| 35 | `src/herdr/pane-identity-resolver.ts` | `internal/herdr/pane_identity_resolver.go` |
| 36 | `src/herdr/session-list.ts` | `internal/herdr/session_list.go` |
| 37 | `src/herdr/session-snapshot.ts` | `internal/herdr/session_snapshot.go` |
| 38 | `src/herdr/socket-client.ts` | `internal/herdr/socket_client.go` |
| 39 | `src/observability/agent-context-service.ts` | `internal/observability/agent_context_service.go` |
| 40 | `src/observability/agent-index-service.ts` | `internal/observability/agent_index_service.go` + `agent_index_events.go` + `agent_index_plans.go` + `agent_index_types.go` |
| 41 | `src/observability/agent-orchestrator-service.ts` | `internal/observability/agent_orchestrator_service.go` |
| 42 | `src/observability/contracts.ts` | `internal/observability/contracts.go`（+ `internal/agenthistory/types.go` 侧的共享类型别名） |
| 43 | `src/observability/errors.ts` | `internal/observability/errors.go` |
| 44 | `src/observability/interactive-pi.ts` | `internal/observability/interactive_pi.go` |
| 45 | `src/observability/schemas.ts` | `internal/observability/schemas.go` |
| 46 | `src/observability/turn-completion.ts` | `internal/observability/turn_completion.go` |
| 47 | `src/shared/json-lines.ts` | `internal/shared/json_lines.go` |
| 48 | `src/shared/unix-peercred.ts` | `internal/shared/unix_peercred.go` + `peercred_linux.go` + `peercred_other.go` |

Go 侧无对应 TS 源、属移植基础设施的文件：`internal/daemon/agent_history_adapter.go`（`agenthistory` → `observability` 接口适配，绕开 Go 包循环依赖）、`internal/db/dedup/dedup.go`（移植 `scripts/clean-agent-event-duplicates-lib.ts`，非 `src/`）、`internal/agenthistory/types.go`。

### 2.2 `test/unit/`（35 个文件）→ Go `*_test.go`

| TS 测试 | Go 测试 |
|---------|---------|
| `agent-history-discovery.test.ts` / `agent-history-discovery-regressions.test.ts` | `internal/agenthistory/discovery_test.go` |
| `agent-history-readers.test.ts` | `internal/agenthistory/readers_test.go` |
| `agent-history-service.test.ts` | `internal/agenthistory/service_test.go` |
| `agent-history-text.test.ts` | `internal/agenthistory/text_test.go` |
| `antigravity-proto.test.ts` | `internal/agenthistory/antigravity_proto_test.go` |
| `antigravity-reader.test.ts` | `internal/agenthistory/antigravity_reader_test.go` |
| `history-has-advanced.test.ts` | `internal/agenthistory/history_has_advanced_test.go` |
| `agy-hook-and-cli.test.ts`（2,826 行，`wc -l` 实测） | `internal/agy/agy_hook_and_cli_test.go` + `agy_hook_part2_test.go` + `agy_hook_part3_test.go` + `agy_hook_part4_test.go`（hook 侧）+ `internal/cli/cli_test.go`（CLI 侧） |
| `cli.test.ts` | `internal/cli/cli_test.go` |
| `config-loader.test.ts` | `internal/config/load_test.go` |
| `config-runtime.test.ts` | `internal/config/runtime_test.go` |
| `config-schema.test.ts` | `internal/config/schema_test.go` |
| `daemon-process-manager.test.ts` | `internal/daemon/process_manager_test.go` + `process_manager_lock_test.go`（含真实子进程抢锁、`HERDSMAN_GO_LOCK_WORKER` 重执模式） |
| `daemon-service.test.ts` | `internal/daemon/service_test.go`（含 `HERDSMAN_GO_DAEMON_WORKER` 子进程真实守护进程用例） |
| `herdr-session-watch-manager.test.ts` | `internal/daemon/herdr_session_watch_manager_test.go` |
| `observability-server-frame-limit.test.ts` | `internal/daemon/observability_server_frame_limit_test.go` |
| `observability-contracts.test.ts` | `internal/observability/contracts_test.go` |
| `turn-completion.test.ts` | `internal/observability/turn_completion_test.go` |
| `interactive-pi.test.ts` | `internal/observability/interactive_pi_test.go` |
| `json-lines.test.ts` | `internal/shared/json_lines_test.go` |
| `unix-peercred.test.ts` | `internal/shared/unix_peercred_test.go` |
| `status-event-plan-store.test.ts` | `internal/db/status_event_plan_store_test.go` |
| `clean-agent-event-duplicates.test.ts` | `internal/db/dedup/dedup_test.go`（被测对象是 `scripts/clean-agent-event-duplicates-lib.ts`，非 `src/`） |

**未在 `herdsman` 仓内重写的 unit 测试（属第 4 批 `herdsman-pi` 扩展侧 / 打包与文档面）**：

| TS 测试 | 未重写原因 |
|---------|-----------|
| `herdsman-pi-extension.test.ts` | 第 4 批 `packages/herdsman-pi`（Pi 扩展）侧单测 |
| `herdsman-pi-agent-update-ui.test.ts` | 同上（扩展 UI） |
| `herdsman-pi-turn-signal.test.ts` | 同上（扩展侧 turn 信号；守护进程侧的等价语义由 `internal/observability/turn_completion_signal_test.go` 覆盖） |
| `herdsman-pi-wake.test.ts` | 同上（扩展侧 wake） |
| `upstream-error.test.ts` | 被测对象 `packages/herdsman-pi/src/upstream-error.ts`，扩展侧 |
| `wake-filter-config.test.ts` | 被测对象 `packages/herdsman-pi/src/wake-filter-config.ts`，扩展侧 |
| `final-audit-regressions.test.ts` | 混合：`JsonLineDecoder` 部分由 `internal/shared/json_lines_test.go` 覆盖，`packages/herdsman-pi/src/daemon-client.ts` 部分属扩展侧 |
| `herdr-plugin-package.test.ts` | `packages/herdsman-herdr-plugin` 打包检查，第 4 批 |
| `package-publication.test.ts` | npm 打包/发布面检查（tarball file allowlist） |
| `deployment-guidance.test.ts` | 文档断言（README/AGENTS 部署指引文本） |
| `shared-json-lines-sync.test.ts` | 仓内副本同步检查（`src/shared/json-lines.ts` 与 `packages/herdsman-pi` 副本一致性） |

### 2.3 `test/integration/`（21 个文件，含 2 个 helper）→ Go `*_test.go`

| TS 测试/辅助 | Go 测试 |
|--------------|---------|
| `agent-context-service.test.ts` | `internal/observability/agent_context_service_test.go` |
| `agent-event-delivery-lifecycle.test.ts` | `internal/observability/agent_event_delivery_lifecycle_test.go` |
| `agent-index-service.test.ts` | `internal/observability/agent_index_service_test.go` |
| `agent-orchestrator-service.test.ts` | `internal/observability/agent_orchestrator_service_test.go` |
| `event-dedup-pane-generation.test.ts` | `internal/observability/event_dedup_pane_generation_test.go` |
| `turn-completion-signal.test.ts` | `internal/observability/turn_completion_signal_test.go` |
| `agent-context-snapshot-store.test.ts` | `internal/db/agent_context_snapshot_store_test.go` |
| `agent-orchestrator-scope-store.test.ts` | `internal/db/agent_orchestrator_scope_store_test.go` |
| `agent-store-terminal-identity.test.ts` | `internal/db/agent_store_terminal_identity_test.go` |
| `sqlite-migrations.test.ts` | `internal/db/sqlite_migrations_test.go` |
| `herdr-pane-identity-resolver.test.ts` | `internal/herdr/pane_identity_resolver_test.go` |
| `herdr-socket-client.test.ts` | `internal/herdr/socket_client_test.go` |
| `herdr-session-watch-idle-seam.test.ts` | `internal/daemon/herdr_session_watch_idle_seam_test.go` |
| `history-cache-ttl.test.ts` | `internal/daemon/history_cache_ttl_test.go` |
| `observability-rpc.test.ts` | `internal/daemon/observability_rpc_test.go` |
| `orchestrator-disconnect-grace.test.ts` | `internal/daemon/orchestrator_disconnect_grace_test.go` |
| `orchestrator-pane-move.test.ts` | `internal/daemon/orchestrator_pane_move_test.go` |
| `startup-reconcile.test.ts` | `internal/daemon/startup_reconcile_test.go` |
| `observability-db-harness.ts`（helper） | Go 侧共享夹具：`obsHarness`（`internal/daemon/observability_server_frame_limit_test.go`）+ `internal/observability` 包内各 `*_test.go` 的建库辅助 |
| `rpc-test-client.ts`（helper） | Go 侧 `internal/daemon/observability_rpc_test.go` 内的 `obsRPCFixture` / `obsOpenRPCServer` |
| `herdsman-pi-daemon-client.test.ts` | **未重写**：pi 扩展侧 `ReconnectingDaemonClient`；Go 侧以 `internal/daemon/observability_rpc_dual_client_test.go` 覆盖真实 TS 客户端 ↔ Go 服务的互操作 |

Go 侧多出、无 TS 对应用例：`internal/daemon/observability_rpc_dual_client_test.go`（TS+Go 双客户端同时在线，见 3.4）。

---

## 3. 测试与验证结果原文

### 3.1 修复前的失败基线（先 FAIL 证据）

在**未改动**的 `go-port@7d5da2e` 上执行全量 `-race`：

```
$ cd go && go test -race -buildvcs=false -count=1 ./...
?   	github.com/dorokuma/herdsman/go/cmd/herdsman	[no test files]
?   	github.com/dorokuma/herdsman/go/cmd/herdsman-daemon	[no test files]
ok  	github.com/dorokuma/herdsman/go/internal/agenthistory	1.162s
ok  	github.com/dorokuma/herdsman/go/internal/agy	4.437s
ok  	github.com/dorokuma/herdsman/go/internal/cli	1.028s
ok  	github.com/dorokuma/herdsman/go/internal/config	1.020s
--- FAIL: TestDaemonServiceSecondDaemonOnSameHomeFailsOnInstanceLock (0.16s)
    service_test.go:1058: daemon worker exit = exit status 1, want exit code 0 (stderr: Herdsman daemon worker failed: sql: database is closed
        )
...
FAIL	github.com/dorokuma/herdsman/go/internal/daemon	26.362s
ok  	github.com/dorokuma/herdsman/go/internal/db	12.249s
ok  	github.com/dorokuma/herdsman/go/internal/db/dedup	1.021s
ok  	github.com/dorokuma/herdsman/go/internal/herdr	1.188s
ok  	github.com/dorokuma/herdsman/go/internal/observability	95.148s
ok  	github.com/dorokuma/herdsman/go/internal/shared	1.100s
FAIL
```

失败机理：子守护进程在 `WriteDaemonPidFile` 之后、`watchManager.Start()` 之前收到 `SIGTERM`；`stop()` 并发执行到 `sqlite.Close()`，仍在跑启动步骤的协程随即以 `sql: database is closed` 失败并走 `startupFailure` → 进程 `exit 1`（优雅关停被误判成启动失败）。此外 `stop()` 读取 `reconcileScheduler` 指针与启动路径的赋值构成数据竞态。

### 3.2 修复内容（`go/internal/daemon/service.go`）

1. **`reconcileScheduler` 提前构造**：`NewPeriodicReconcileScheduler(schedulerOptions)` 移到 `attachSignalHandlers(stop)` 之前（`shutdownBudgetMs` 之后），启动末尾只调用 `reconcileScheduler.Start()`。`stop()` 与 `runShutdownSequence` 读到的指针不再与启动协程的赋值竞争；`Start()` 仍在启动末尾，首个 tick 不会早于监听。
2. **启动步骤与关停的显式交接（`startupMu`）**：
   - `stop()` 置 `stopping = true` → `detachSignalHandlers()` / `attachSignalHandlers(onSecondSignal)` → **先 `shutdownController.Abort()`**，再 `startupMu.Lock()`，然后才执行 `runShutdownSequence`（含 `sqlite.Close()`）。正在执行的启动步骤先跑完，`stop()` 才关库；`stop()` 持锁期间后续步骤无法开始。
   - 启动路径改为 6 个 `startupStep`：`server.Start` → `WriteDaemonPidFile` → `reconciler.Reconcile` → `index.DrainPendingPlans(startup)` → `reconcileScheduler.Start` → `watchManager.Start`。每步都在 `startupMu` 内先查 `shutdownOwnsDaemon()`（`shutdownController.Aborted()` 或 `stopping`）：已交接则返回哨兵 `errStartupHandedOver`，**不调用 `startupFailure`**，`RunObservabilityDaemonService` 返回 `nil`，由 `stop()` 以 `exitCode=0` 完成优雅关停。
   - 真实启动失败仍走原 `startupFailure` 清理（在 `startupMu` 内完成，避免与稍后开始的关停序列交错）；错误照旧向上返回。
3. **`gofmt -w cmd internal`**：仅修掉 `internal/db/agents.go` 一个多余空行；`gofmt -l cmd internal` 输出为空。

未新增测试文件：回归由既有 `TestDaemonServiceSecondDaemonOnSameHomeFailsOnInstanceLock`（真实子进程 + 真实信号）承担，修复前 FAIL、修复后 PASS。

### 3.3 修复后 Step 1 证据

```
$ cd go && gofmt -l cmd internal
(空)
```

```
$ cd go && go test -race -buildvcs=false -count=1 ./internal/daemon/...
ok  	github.com/dorokuma/herdsman/go/internal/daemon	25.921s
```

单用例 5 连跑（`-race`，无 `-skip`）：

```
$ go test -race -buildvcs=false -count=5 -run TestDaemonServiceSecondDaemonOnSameHomeFailsOnInstanceLock -v ./internal/daemon/...
=== RUN   TestDaemonServiceSecondDaemonOnSameHomeFailsOnInstanceLock
Herdsman daemon listening on /tmp/TestDaemonServiceSecondDaemonOnSameHomeFailsOnInstanceLock3749145186/001/herdsman.sock
Herdsman daemon shutdown starting (budgetMs=5000 pid=3333)
Herdsman daemon shutdown aborted in-flight waits (elapsedMs=0)
Herdsman daemon shutdown step starting (budgetRemainingMs=4999 step=index.drainInFlightPlans timeoutMs=4999)
Herdsman daemon shutdown step finished (elapsedMs=0 step=index.drainInFlightPlans)
Herdsman daemon shutdown step starting (budgetRemainingMs=4999 step=reconcileScheduler.stop timeoutMs=4999)
Herdsman daemon shutdown step finished (elapsedMs=0 step=reconcileScheduler.stop)
Herdsman daemon shutdown step starting (budgetRemainingMs=4999 step=watchManager.stop timeoutMs=4999)
Herdsman daemon shutdown step finished (elapsedMs=0 step=watchManager.stop)
Herdsman daemon shutdown step starting (budgetRemainingMs=4999 step=server.stop timeoutMs=4999)
Herdsman daemon shutdown step finished (elapsedMs=0 step=server.stop)
Herdsman daemon shutdown steps finished (budgetMs=5000 elapsedMs=0)
Herdsman daemon shutdown finished (elapsedMs=1 exitCode=0)
--- PASS: TestDaemonServiceSecondDaemonOnSameHomeFailsOnInstanceLock (1.18s)
（… 同一用例连续 5 次，5/5 PASS，末行为 --- PASS: … (1.18s)）
PASS
ok  	github.com/dorokuma/herdsman/go/internal/daemon	6.899s
```

### 3.4 Step 2：全量编译 / vet / 测试

```
$ cd go && CGO_ENABLED=0 go build -buildvcs=false ./...
(无输出，exit 0)
```

```
$ cd go && CGO_ENABLED=0 go vet -buildvcs=false ./...
(无输出，exit 0)
```

```
$ cd go && CGO_ENABLED=0 go test -buildvcs=false -count=1 ./...
?   	github.com/dorokuma/herdsman/go/cmd/herdsman	[no test files]
?   	github.com/dorokuma/herdsman/go/cmd/herdsman-daemon	[no test files]
ok  	github.com/dorokuma/herdsman/go/internal/agenthistory	0.030s
ok  	github.com/dorokuma/herdsman/go/internal/agy	2.788s
ok  	github.com/dorokuma/herdsman/go/internal/cli	0.007s
ok  	github.com/dorokuma/herdsman/go/internal/config	0.005s
ok  	github.com/dorokuma/herdsman/go/internal/daemon	11.158s
ok  	github.com/dorokuma/herdsman/go/internal/db	0.394s
ok  	github.com/dorokuma/herdsman/go/internal/db/dedup	0.004s
ok  	github.com/dorokuma/herdsman/go/internal/herdr	0.157s
ok  	github.com/dorokuma/herdsman/go/internal/observability	3.940s
ok  	github.com/dorokuma/herdsman/go/internal/shared	0.020s
```

```
$ cd go && go test -race -buildvcs=false -count=1 ./...
?   	github.com/dorokuma/herdsman/go/cmd/herdsman	[no test files]
?   	github.com/dorokuma/herdsman/go/cmd/herdsman-daemon	[no test files]
ok  	github.com/dorokuma/herdsman/go/internal/agenthistory	1.180s
ok  	github.com/dorokuma/herdsman/go/internal/agy	4.477s
ok  	github.com/dorokuma/herdsman/go/internal/cli	1.023s
ok  	github.com/dorokuma/herdsman/go/internal/config	1.025s
ok  	github.com/dorokuma/herdsman/go/internal/daemon	29.324s
ok  	github.com/dorokuma/herdsman/go/internal/db	12.243s
ok  	github.com/dorokuma/herdsman/go/internal/db/dedup	1.017s
ok  	github.com/dorokuma/herdsman/go/internal/herdr	1.179s
ok  	github.com/dorokuma/herdsman/go/internal/observability	96.671s
ok  	github.com/dorokuma/herdsman/go/internal/shared	1.132s
```

无 `-skip`、无 `-short`；全部 10 个有测试的包全绿。

TS+Go 双客户端同时连 Go 守护进程（`internal/daemon/observability_rpc_dual_client_test.go`，TS 侧通过 `testdata/pi_client_driver.ts` 以真实 tsx 子进程运行未改动的 `packages/herdsman-pi/src/daemon-client.ts`；本 worktree 借用主 worktree 的 `/root/workspace/herdsman/node_modules/.bin/tsx`）：

```
$ go test -buildvcs=false -count=1 -run TestObservabilityRpcServerSimultaneousTsAndGoClients -v ./internal/daemon/
=== RUN   TestObservabilityRpcServerSimultaneousTsAndGoClients
--- PASS: TestObservabilityRpcServerSimultaneousTsAndGoClients (0.23s)
PASS
ok  	github.com/dorokuma/herdsman/go/internal/daemon	0.232s
```

（该用例找不到 tsx 时会 `t.Skipf` 而非静默通过；本次运行日志中没有 `--- SKIP`，即真实执行了 TS 子进程。）

### 3.5 Step 3：端到端隔离实测

编译产物（静态链接，零 CGO）：

```
$ cd go && CGO_ENABLED=0 go build -buildvcs=false -o /tmp/herdsman-go-bin/ ./cmd/...
$ ls -l /tmp/herdsman-go-bin/
-rwxr-xr-x 1 root root 11641943 10月26日 01:26 herdsman
-rwxr-xr-x 1 root root 13556311 10月26日 01:26 herdsman-daemon
$ file /tmp/herdsman-go-bin/herdsman /tmp/herdsman-go-bin/herdsman-daemon
/tmp/herdsman-go-bin/herdsman:        ELF 64-bit LSB executable, x86-64, statically linked, ... not stripped
/tmp/herdsman-go-bin/herdsman-daemon: ELF 64-bit LSB executable, x86-64, statically linked, ... not stripped
```

包装脚本 TS 分支（默认 `HERDSMAN_IMPL` 未设置 / 显式 `ts`；本 worktree 先 `pnpm install --frozen-lockfile --ignore-scripts` + `pnpm build` 生成 `dist/`，二者均被 `.gitignore` 覆盖）：

```
$ env -u HERDSMAN_IMPL ./go/scripts/herdsman --help
Usage:
  herdsman on [--if-unowned] [--json]
  herdsman off [--json]
  herdsman status [--json]
  herdsman daemon status [--text]
  herdsman agent list [--all] [--workspace <id>] [--session <name>] [--json]
  herdsman agent get <target> [--workspace <id>] [--session <name>] [--json]
  herdsman agent read <target> [--limit N] [--workspace <id>] [--session <name>] [--json]
  herdsman agy-hook <PreInvocation|Stop|WakeWaiter>
  herdsman help
... exit=0

$ HERDSMAN_IMPL=ts ./go/scripts/herdsman --help
（同上，exit=0）
```

（首次尝试时 `dist/` 未构建，包装脚本按设计报错：`herdsman wrapper: TS entrypoint not found at .../dist/src/cli/herdsman.js (run: pnpm build)`，exit 1。）

Go 分支：临时 `HERDSMAN_HOME=/tmp/herdsman-e2e-0WMdsR`（`mktemp -d`），Go 守护进程 `/tmp/herdsman-go-bin/herdsman-daemon` 前台常驻，CLI 全程经包装脚本：

```
$ HERDSMAN_HOME=/tmp/herdsman-e2e-0WMdsR HERDSMAN_IMPL=go HERDSMAN_GO_BIN=/tmp/herdsman-go-bin/herdsman ./go/scripts/herdsman daemon status
{"managedBy":"unmanaged","pid":2572258,"pidPath":"/tmp/herdsman-e2e-0WMdsR/herdsman.pid","socketPath":"/tmp/herdsman-e2e-0WMdsR/herdsman.sock","socketReachable":true,"state":"running"}
exit=0

$ ... ./go/scripts/herdsman daemon status --text
state: running
pid: 2572258
socketReachable: true
managedBy: unmanaged
exit=0

$ ... ./go/scripts/herdsman agent list --json
{"agents":[{"agent":"agy","agentSession":{...},"agentStatus":"done","cwd":"/root/workspace/herdsman",...,"paneId":"w3E:p1",...},{"agent":"pi",...,"herdrSessionName":"default","history":{"lastAssistantMessage":{...},"lastUserMessage":{...}},...,"paneId":"w3E:pS",...}]}
exit=0

$ ... ./go/scripts/herdsman status --json          # orchestrator status（见下条说明）
{"context":null,"events":[],"presence":{"connectedAt":1792949317426,"herdrSessionName":"default","paneId":"w3E:pS","subscriberId":"cli:w3E:pS","subscriberKind":"agy","terminalId":"term_65ead53ba759d35a","workspaceId":"w3E"},"state":null}
exit=0

$ ... ./go/scripts/herdsman orchestrator status    # 任务书里的这种拼写
Unknown command: orchestrator
exit=1

$ printf '{"invocationNum":1}' | ... ./go/scripts/herdsman agy-hook PreInvocation
{}
exit=0

$ printf '{}' | ... ./go/scripts/herdsman agy-hook Stop
{}
exit=0
```

关于 `orchestrator`：TS 与 Go 的 CLI 词表里都只有 `on | off | status`（orchestrator 命令组），没有 `orchestrator` 子命令前缀。用 TS 实现跑同一条命令得到**完全相同**的输出 `Unknown command: orchestrator`、exit 1（见 3.6 的 [P1]），因此这是两个实现一致的既有契约，不是 Go 侧缺陷。

优雅关停临时 Go 守护进程（`kill -TERM 2572258`）后进程消失，home 目录只剩实例锁文件（设计如此：锁文件永不 unlink）与 `state.db`，pid 文件与 socket 均已清理：

```
Herdsman daemon shutdown starting (budgetMs=5000 pid=2572258)
Herdsman daemon shutdown aborted in-flight waits (elapsedMs=0)
Herdsman daemon shutdown step finished (elapsedMs=0 step=index.drainInFlightPlans)
Herdsman daemon shutdown step finished (elapsedMs=0 step=reconcileScheduler.stop)
Herdsman daemon shutdown step finished (elapsedMs=0 step=watchManager.stop)
Herdsman daemon shutdown step finished (elapsedMs=0 step=server.stop)
Herdsman daemon shutdown steps finished (budgetMs=5000 elapsedMs=0)
Herdsman daemon shutdown finished (elapsedMs=0 exitCode=0)

$ ls -la /tmp/herdsman-e2e-0WMdsR/
-rw------- 1 root root      0 01:28 herdsman.pid.instance.lock
-rw------- 1 root root 278528 01:31 state.db
```

### 3.6 TS ↔ Go 交叉对照（同一 HERDSMAN_HOME，同一 Go 守护进程）

TS 客户端连 Go 守护进程（跨实现互操作）：

```
[P1] $ HERDSMAN_IMPL=ts ./go/scripts/herdsman orchestrator status
Unknown command: orchestrator
exit=1

[P2] $ HERDSMAN_IMPL=ts ./go/scripts/herdsman daemon status
{"pidPath":"/tmp/herdsman-e2e-0WMdsR/herdsman.pid","socketPath":"/tmp/herdsman-e2e-0WMdsR/herdsman.sock","socketReachable":true,"stalePid":2572258,"state":"running"}
exit=0

[P3] $ HERDSMAN_IMPL=ts ./go/scripts/herdsman agent list --json
{"agents":[{"agent":"agy","agentSession":{"agent":"agy","kind":"id","source":"herdr:antigravity_cli","value":"dc89730d-..."},...}]}
exit=0

[P4] $ printf '{"invocationNum":1}' | HERDSMAN_IMPL=ts ./go/scripts/herdsman agy-hook PreInvocation
{}
exit=0

[P5] $ printf '{}' | HERDSMAN_IMPL=ts ./go/scripts/herdsman agy-hook Stop
{}
exit=0
```

`[P2]` 的字段差异是**有意且已文档化**的：TS 的 `DAEMON_ENTRYPOINT_NAMES = ["herdsman-daemon.js"]`，Go 的 `DaemonEntrypointNames = ["herdsman-daemon.js", "herdsman-daemon"]`（`process_manager.go` 注释：识别任一实现遗留的 pid 文件）。因此 TS 判断该 pid「身份不明」→ 输出 `stalePid` + `running`；Go 认得出 → 输出 `pid` + `managedBy`。两者都正确报告 `state: running`、`socketReachable: true`，也都能正常经 socket 通信。

### 3.7 PiG 隔离实测（`/root/.pig-test/agent`）

```
$ ls -l /root/.local/bin/pig
lrwxrwxrwx 1 root root 30 10月23日 12:21 /root/.local/bin/pig -> /root/.local/opt/pig/0.5.2/pig
$ pig --version
0.5.2+1.4.0
exit=0

$ ls -la /root/.pig-test/agent
drwx------ 8 root root 4096 10月26日 00:50 .
-rw------- 1 root root    2 10月23日 12:23 auth.json        # 仅列存在性/权限，未读取内容
-rw-r--r-- 1 root root   62 10月24日 11:51 auto-continue.json
-rw-r--r-- 1 root root  720 10月25日 16:40 exit.log
drwxr-xr-x 2 root root 4096 10月23日 12:27 extensions
drwxr-xr-x 2 root root 4096 10月25日 02:58 logs
-rw------- 1 root root 8797 10月26日 00:50 models.json
-rw-r--r-- 1 root root  198 10月25日 16:45 pi-vcc-config.json
drwxr-xr-x 28 root root 4096 10月25日 23:11 sessions
-rw------- 1 root root  299 10月23日 12:27 settings.json
drwxr-xr-x 2 root root 4096 10月23日 12:27 skills
drwxr-xr-x 2 root root 4096 10月23日 12:27 themes
```

配置目录切换：PiG 认 **`PIG_HOME`**，不认 pi 的 `PI_CODING_AGENT_DIR`（实测三组候选）：

```
$ PI_CODING_AGENT_DIR=/root/.pig-test/agent pig diagnose   → config root: /root/.pig        agent dir: /root/.pig/agent
$ PIG_HOME=/root/.pig-test            pig diagnose         → config root: /root/.pig-test   agent dir: /root/.pig-test/agent
$ PIG_AGENT_DIR=/root/.pig-test/agent pig diagnose         → config root: /root/.pig        agent dir: /root/.pig/agent
```

指向临时 Go 守护进程的真实模型调用：

```
$ cd /tmp && PIG_HOME=/root/.pig-test HERDSMAN_HOME=/tmp/herdsman-e2e-0WMdsR timeout 240 pig -p "回复 OK" > /tmp/pig-ok.out 2>/tmp/pig-ok.err
exit=0
--- stdout ---
OK
--- stderr ---
(空)
```

```
$ PIG_HOME=/root/.pig-test pig list
No packages installed.
```

限制如实记录：`/root/.pig-test/agent/extensions/` 为空，`pig list` 报 `No packages installed.`，即该隔离配置**没有安装 `@dorokuma/herdsman-pi` 扩展**。所以本次 PiG 实测证明的是「隔离配置下的 PiG 在指向 Go 守护进程 `HERDSMAN_HOME` 时仍能正常运行并返回模型回复」，**没有**覆盖 herdsman-pi 扩展自身的 agent 快照/唤醒链路；扩展侧行为仍由第 4 批的 `packages/herdsman-pi` 单测（TS）与 `observability_rpc_dual_client_test.go`（真实 TS 客户端 ↔ Go 服务）承担。

---

## 4. 与 TS 版的行为对比

### 4.1 CLI 参数

| 面 | TS (`src/cli/herdsman.ts`) | Go (`cmd/herdsman` + `internal/cli`) | 一致性 |
|----|---------------------------|--------------------------------------|--------|
| 命令词表 | `on`/`off`/`status`、`daemon status [--text]`、`agent list/get/read`、`agy-hook <PreInvocation\|Stop\|WakeWaiter>`、`help` | 同上 | 一致（实测 [`--help`](../../go/internal/cli/herdsman.go)、`Unknown command: orchestrator` 两边逐字相同） |
| scope 参数 | `--all` / `--workspace <id>` / `--session <name>` / `HERDR_ENV=1` 环境 | 同上，错误文案同（`CURRENT_HERDR_WORKSPACE_ERROR` / `CURRENT_HERDR_PANE_ERROR`） | 一致 |
| `agent read` | `--limit N` | 同上 | 一致 |
| 守护进程入口 | `herdsman-daemon.js` 无参数，多参数即报错退出 1 | `cmd/herdsman-daemon` 同样拒绝参数 | 一致 |
| 包装脚本 | 无（发布物 bin 直指 `dist/src/cli/herdsman.js`） | `go/scripts/herdsman`，`HERDSMAN_IMPL=ts\|go` 切换，`HERDSMAN_GO_BIN` 覆盖二进制 | Go 侧新增的仓内切换器（生产 bin 未动） |

### 4.2 stdout / stdin JSON 与退出码

两条互相独立的输出路径，Go 与 TS 逐字对齐（`printResult` ↔ `internal/cli/herdsman.go` 的 `printResult`，`runDaemonCommand` ↔ `cmd/herdsman/main.go` 的 `runDaemonCommand`）：

- **RPC 命令默认人读，`--json` 才出 JSON**：`on`/`off`/`status`、`agent list/get/read` 走 `printResult`，它只认 `--json`（`command.json`）——不带 `--json` 时渲染 `formatHumanResult` 的人读视图（orchestrator 一行 `Herdsman is watching/off …`，`agent list/get/read` 多行 `name:/agent:/status:` 字段表），带上才输出单行 JSON。`formatHumanResult` 只覆盖 orchestrator 与三个 agent 子命令，其余一律落回 `JSON.stringify`/`marshalJS`。用例：`TestRunCliCommandRendersHumanAgentList`、`TestRunCliCommandRendersLiveNameAndAgentKindInHumanGetAndRead`。
- **`daemon status` 反过来：默认 JSON，`--text` 才是人读视图**。该命令**不经过** `printResult`，由入口的 `runDaemonCommand` 直接分流：默认 `DaemonStatusJSON(view)` 出单行 JSON，`--text` 走 `FormatDaemonStatus(view)` 出多行人读视图（`state:`、`pid:`、`pidFileMissing:`、`stalePid:`、`socketReachable:`、`managedBy:`、`restartCount:`）。
- 人读视图与 JSON 视图字段集刻意不同：`FormatDaemonStatus` 只保留人扫一眼要的事实，并 systemd 的 `NRestarts` 不可用时拼出 `unavailable` 而非静默丢弃（TS 注释同述）。
- stdin：仅 `agy-hook` 读取，`WakeWaiter` 例外（不读 stdin）；空白/非对象/不可解析 JSON 一律当作空 payload（`parseHookPayload`）。实测两边 `PreInvocation`/`Stop` 均输出 `{}`、exit 0。
- 退出码：成功 0；CLI 错误经 `FormatCliError` 打印到 stderr 并 exit 1（`Unknown command: orchestrator` 实测两侧同为 1）。hook 的**所有**失败都被吞成 `{}` + exit 0（与 TS 同）。
- 守护进程退出码：优雅关停 `0`，启动失败 `1`；第二个 `SIGTERM/SIGINT` 强制退出仍为 `0`（避免 `Restart=on-failure` 拉回），与 TS 一致。
- 实测（同一隔离 `HERDSMAN_HOME=/tmp/herdsman-cli-check/home`，两侧逐字节相同）：`daemon status` → `{"pidPath":"…/herdsman.pid","socketPath":"…/herdsman.sock","state":"stopped"}`；`daemon status --text` → `state: stopped`。

### 4.3 RPC 方法名与消息格式

- 传输：Unix domain socket + JSON-lines（`internal/shared/json_lines.go` ↔ `src/shared/json-lines.ts`），帧上限与 `observability_server_frame_limit_test.go` 对应。
- 方法名逐字一致。请求侧（`observability_server_handlers.go` 的 `#dispatch` switch，共 10 个 case）：`agent.list`、`agent.get`、`agent.read`、`agent.events`、`agent.orchestrator.register`、`agent.orchestrator.get`、`agent.orchestrator.set`、`agent.notifications.ack`、`agent.turn.completed`、`agent.ping`；服务端推送侧共 **3** 个方法：`agent.event`（`PublishAgentEvent`）、`agent.context.changed`（`PublishAgentContext`）、`agent.orchestrator.changed`（`notifyOwnerChanged`），三者都带 `agent.` 前缀，与 `observability_server.go` 顶部 wire contract 注释所列一致。两个方法名注意都带 `agent.` 前缀：`agent.turn.completed`（`internal/agy/hook.go` 的 `client.Call("agent.turn.completed", …)`）与 `agent.notifications.ack`（`client.Call("agent.notifications.ack", …)`）；`internal/observability/contracts.go` 里不带前缀的 `turn.completed` 只是 params 的 type 判别名，不是线上方法名。双客户端用例断言的就是这套 pi 面向面。
- 互操作实测：TS 客户端（`dist/`）经 Go 服务拿到 `agent list`/`daemon status`；`TestObservabilityRpcServerSimultaneousTsAndGoClients` 证明 TS 与 Go 客户端**同时在线**时彼此可见 presence、orchestrator 归属与 turn 信号，且只有持角色的一侧收到推送。

### 4.4 SQLite 表结构与 Drizzle 迁移兼容性

- drizzle 历史共 12 条 `CREATE TABLE`，迁移 `0002_worthless_energizer.sql` `DROP TABLE agent_notification_cursors / agent_notification_subscriptions`，**最终 10 张表**：`agents`、`agent_events`、`agent_history_cache`、`agent_context_snapshots`、`agent_orchestrator_scopes`、`agent_pane_tombstones`、`herdr_sessions`、`herdr_workspaces`、`daemon_meta`、`status_event_plans`。
- Go 的 `ApplyMigrations`（`internal/db/apply_migrations.go`）重放的是与 `drizzle/` 逐字节相同的 SQL/`meta/_journal.json` 内嵌副本，记账表同为 `__drizzle_migrations`，语句分隔符按 drizzle 的 `--> statement-breakpoint` 处理 → 同一 home 目录可被 TS 与 Go 交替打开（本子批实测：同一 `HERDSMAN_HOME` 先由 Go 守护进程建库，TS CLI 正常读到 `state.db` 与 agent 数据）。`internal/db/sqlite_migrations_test.go` 覆盖全量迁移与中途版本（`ApplyMigrationsThrough`）。
- 驱动：TS 用 `better-sqlite3`（native），Go 用 `modernc.org/sqlite v1.61.0`（纯 Go）。因此 `CGO_ENABLED=0` 可编译、可静态链接；SQL 语义按 SQLite 3 行为对齐，`internal/shared/sqlite_transaction.go` 复刻 TS 的事务辅助。

### 4.5 内部机制差异（有意为之，非行为偏差）

| 面 | TS | Go |
|----|----|-----|
| SQLite 驱动 | `better-sqlite3`（`node-gyp` 原生模块，需要 C ABI） | `modernc.org/sqlite`，零 CGO，`CGO_ENABLED=0` 静态二进制 |
| 并发模型 | 单事件循环，`await` 之间的块原子 | 多 goroutine：`lifecycleMu` 守 `stopping`/`exitRequested`/信号注册，`startupMu` 做启动↔关停交接（本子批新增），`instanceLockMu` 守锁释放 |
| `WakeWaiter` | 子进程 `node` + detached | `os.Executable()` 自执 + `syscall.SysProcAttr{Setsid: true}`（`internal/agy/wake_waiter.go:701,712`），脱离当前会话 |
| 未捕获异常 | `unhandledRejection`/`uncaughtException` 仅记录 | Go 无全局钩子：每个 goroutine 自行上报（`service.go` 注释明示） |
| 二期进程锁 | `proper-lockfile` 语义 | `AcquireDaemonLock` + `flock`，`${pidPath}.instance.lock`，锁文件不 unlink（实测残留即为设计） |
| 守护进程身份识别 | 只认 `herdsman-daemon.js` | 同时认 `herdsman-daemon.js` 与 `herdsman-daemon`（见 3.6 的 `[P2]` 差异来源） |

---

## 5. 遗留问题与后续步骤

1. **生产面未动**：`/root/.local/bin/herdsman`（npm 全局软链）与 `herdsman.service`（`ExecStart` → 全局 `herdsman-daemon.js`）按硬规矩保持原样，本子批只在临时 `HERDSMAN_HOME` 与 `/tmp` 产物上实测。切换 Go 实现需第 6 批经爸爸批准后另行执行（届时按 `docs/releasing.md` 走发布面门禁，`pnpm check` + `pnpm build` + `pnpm package:check`）。
2. **包装脚本无 `HERDSMAN_DAEMON_GO_BIN`**：`go/scripts/herdsman` 只切换 CLI（`HERDSMAN_GO_BIN`），没有守护进程二进制开关；本次守护进程由 `/tmp/herdsman-go-bin/herdsman-daemon` 手工启动。任务书里提到的 `HERDSMAN_DAEMON_GO_BIN` 在当前实现中不存在（`grep -r HERDSMAN_DAEMON_GO_BIN go/` 无命中），如需纳入切换器需另开任务。
3. **TS↔Go `daemon status` 字段差异**：TS 对非 `herdsman-daemon.js` 的 pid 报 `stalePid`，Go 认两种入口名报 `pid`/`managedBy`。若第 6 批要求两实现输出逐字段一致，需要单独决策（当前 Go 侧是有意放宽并已在代码注释中记录）。
4. **PiG 隔离配置未装扩展**：`/root/.pig-test/agent/extensions/` 为空、`pig list` = `No packages installed.`，因此 PiG 实测只覆盖「隔离配置 + Go 守护进程 home 下正常问答」，未覆盖 herdsman-pi 扩展的 agent 快照/唤醒链路。需要覆盖时应在该隔离目录安装 `@dorokuma/herdsman-pi` 后复跑。
5. **worktree 里的 TS 构建产物**：为验证包装脚本 TS 分支，本 worktree 执行了 `pnpm install --frozen-lockfile --ignore-scripts` 与 `pnpm build`，产生 `node_modules/`、`dist/`（均被 `.gitignore` 忽略，未进 commit）。`pnpm install` 首次带脚本版本因 `husky: not found` 失败，改用 `--ignore-scripts` 成功；husky 钩子因此未安装，对本子批的 `git commit` 无影响（提交规范由全局 commit-msg hook 校验）。
6. **`src/db/migrate.ts` 无独立 Go 迁移命令**：`db:migrate` 是开发脚本，Go 侧能力由 `ApplyMigrations`（daemon 启动时自动执行）与 `ApplyMigrationsThrough` 提供；若需要等价的 `go` CLI 迁移子命令，属新增功能，需另立任务。

---

## 6. 第 4 轮 reviewer 修复（db / observability / herdr / daemon）

对应提交：`fix(go): resolve db, observability, herdr, and daemon reviewer findings`（本子批第 23 条）。前一节 4.2/4.3 的两处描述错误也在本轮一并修正。

### 6.1 参照提交 `5fce550`（`fix(go): resolve agy, cli, and agenthistory reviewer findings`）

任务书最初写作短 SHA `8c4bd1b`。该短 SHA 在本仓 object 数据库中不可解析（`git cat-file -t 8c4bd1b` 在 worktree 与主仓均报 `Not a valid object name`），但按其描述的提交说明在 `go-port` 上可唯一对应到 **`5fce550`**（`git show --stat 5fce550` 核实：`fix(go): resolve agy, cli, and agenthistory reviewer findings`，9 文件 +705/-33）。**`8c4bd1b` 是任务书的笔误，本报告一律以 `5fce550` 为准。**

`5fce550` 修复了第 1 轮 reviewer（`reviewer-1`）的全部 8 项问题。下表按 `git show 5fce550 -- <file>` 的真实函数名与落点书写（任务书中的近似名称一律以本节为准）：

| # | 面 | 问题与修复（真实落点） |
|---|----|----------------------|
| 1 | `internal/agy/state.go` | `writeSessionState` 的 `readRawStateObject` 返回 nil map 时补一个空 map；`readRawStateObject` 在 `json.Unmarshal` 成功但结果是 JSON `null`（object == nil）时归一成空 map。此前 `overlayKnownStateFields` 往 nil map 赋值会 panic |
| 2 | `internal/agenthistory/antigravity_proto.go` | `readVarint` 第 10 字节宽度拦截：`shift == 63 && (next&0x7f) > 1` 即返回 `errInvalidVarint`。TS 用 BigInt 累加，`2^64 + 154` 不会截断；Go 的 uint64 会丢掉高位，把越界 tag 变成合法 tag（`2^64 + 154` 截断成 154，即 field 19 / wireType 2） |
| 3 | `internal/cli/herdsman.go` | `parseAgentList` 的 `--all` 分支：`command.All = true` 同时把 `HerdrSessionName` 与 `WorkspaceID` 置 nil。TS 的 `all ? { all: true } : scopedOrCurrent(...)` 替换整个 scope 对象，所以显式 `--session`/`--workspace` 是被丢弃而非与 `all` 并列发出 |
| 4 | `internal/cli/herdsman.go` 与 `internal/agy/wake_waiter.go` | `parseJSNumber` 支持无符号 `0x`/`0b`/`0o` 前缀（`strconv.ParseUint` 按 16/2/8 基解析）；带符号前缀（`-0x14`）仍走 `ParseFloat` 得 NaN，与 JS 的 `Number()` 一致。两处各有一份同名实现 |
| 5 | `internal/agy/hook.go` 与 `internal/cli/herdsman.go` | 新增 `restoreJSLineSeparators`：`encoding/json` 总是把 U+2028/U+2029 写成 `\u2028`/`\u2029`，该函数把它们还原成原字符，保持 TS `JSON.stringify` 的逐字节输出 |
| 6 | `internal/agenthistory/discovery_test.go` | **删除**伪辅助测试：`occupiedBoundaryKey` 与 `TestOccupiedFallbackDiscoveryUsesAChangedOccupiedSetAsADiscoveryBoundary` 被整体删去（后者只在测试文件内构造两个 map 字面量、比较排序 join，完全不触达生产代码）。同时在文件顶部留下注释，说明该行为已迁居 agent context service；`discovery.go` 本身无改动。`OccupiedSessionPaths` 边界的真实覆盖见 6.8 的 Finding 5 |
| 7 | `internal/agenthistory` | `parseJSNumber`（见第 4 项）：`0x`/`0b`/`0o` 前缀支持 |
| 8 | `internal/agy` / `internal/cli` / `internal/agenthistory` | 三个包 5 轮 `-race` 全绿（见 6.6） |

其中第 1、2、4、5 项各带新增/改写的单测（`antigravity_proto_test.go` +36、`agy_hook_and_cli_test.go` +301、`cli_test.go` +158，`discovery_test.go` ±36 且净减少），第 8 项以 5 轮 `-race` 作为证据。

### 6.2 六项修复

| # | 面 | 改动 |
|---|----|------|
| 1 | `internal/db` | 新增 `internal/shared/sqlite_gate.go`（`SqliteGate`：按 goroutine 可重入的数据库闸门），`RunSqliteTransaction` 全程持闸；commit 失败时补 `rollback`（失败 commit 会让事务保持打开，下一轮的 `begin immediate` 会撞 "cannot start a transaction within a transaction"） |
| 2 | `internal/observability` | `TurnCompletionRegistry` 的 waiter 增加 `finished` 守卫与 `doneCh`；abort 监听 goroutine 改为 `select`，wait 以任何方式结束后即退出，不再永久占住调用方的 signal channel |
| 3 | `internal/daemon` | RPC 发布路径（`PublishAgentEvent` / `PublishAgentContext` / register handler 的 context 帧 / disconnect 回调）由「持锁写帧」改为「锁内收集 `pendingFrame`、锁外 `writeAll`」 |
| 4 | `internal/daemon` | 关停预算自信号到达时刻起算；新增 `startupDone` + `waitClosed` 有界等待启动交接，超时打 stderr 并以 `startupMu.TryLock()` 放行；`ReconcileOptions` 增 `ShutdownSignal`，启动 reconcile 的会话遍历可被中断；`WriteDaemonPidFile` 的错误不再被吞 |
| 5 | `internal/observability` | `RefreshHerdrSessionSource` 的 epoch 读取、在途判定与安装合并进同一段锁持有，避免同 epoch 的并发调用方拿到两个不同的 `*RefreshSource` |
| 6 | `internal/herdr` | `HerdrSocketClient` 增加 `connect`（带 dial 预算）、`armDeadline`/`clearDeadline`；`readLoop` 识别 `os.ErrDeadlineExceeded` 后继续存活，`requestOnce` 把超时翻译成调用方期待的 request timeout 文案 |

### 6.3 变更面

```
 go/internal/daemon/agent_event_reconciler.go       |  28 ++
 go/internal/daemon/observability_server.go         | 254 +++++++++++++-------
 go/internal/daemon/observability_server_handlers.go| 126 ++++++---
 go/internal/daemon/service.go                      |  80 +++++-
 go/internal/daemon/service_test.go                 | 136 ++++++++++
 go/internal/daemon/startup_reconcile_test.go       |  42 +++
 go/internal/db/client.go                           | 294 ++++++++++++++++++++-
 go/internal/herdr/socket_client.go                 |  83 +++++-
 go/internal/herdr/socket_client_test.go            | 110 ++++++++
 go/internal/observability/agent_index_service.go   |  12 +-
 go/internal/observability/turn_completion.go       |  39 ++-
 go/internal/observability/turn_completion_test.go  |  86 ++++++
 go/internal/shared/sqlite_transaction.go           |  19 +-
 go/reports/batch-5-herdsmand.md                    |  17 +-
```

新增文件：`go/internal/shared/sqlite_gate.go`（132 行）、`go/internal/daemon/observability_rpc_slow_client_test.go`（273 行）、`go/internal/db/sqlite_transaction_test.go`（132 行）、`go/internal/observability/agent_index_refresh_sharing_test.go`（64 行）。

### 6.4 回归测试的先 FAIL 验证（逐项实测，非声称）

对每项修复都把改动临时退回修复前形态、跑对应用例、再恢复。结果分三档，**如实记录，不统一说成「已捕获」**：

**确实捕获（FAIL → PASS）**

- **Finding 1**（数据库闸门）：退回「不持闸」后两条用例同时 FAIL，且正是闸门要关的两种失效形态——`TestRunSqliteTransactionConcurrentDoesNotInterleave` 报 `cannot start a transaction within a transaction`；`TestRunSqliteTransactionRollbackDoesNotDiscardOtherGoroutineWrite` 报 `survivor workspace count = 0`（他人已提交的写被回滚吃掉）。
- **Finding 4**（启动/关停预算）：三条全部 FAIL → PASS。pid 文件用例退回吞错后 `= nil error`；预算用例把有界等待换回无界 `<-startupDone` 后 60s 超时（goroutine 阻塞在该行）；reconciler 用例把 `stopping()` 改成恒假后报 `visited 4 sessions, want 0`。
- **Finding 6**（herdr 读循环存活）：删掉 `readLoop` 的 `os.ErrDeadlineExceeded` 分支后两条持久连接子用例 FAIL——超时以裸 `i/o timeout` 冒出来而不是 request timeout 文案，且 socket 被拆掉，下一次请求报 `use of closed network connection`。**这一项才是 Finding 6 的承重墙。**

**非确定性捕获**

- **Finding 5**（refresh 共享）：把单段锁拆回「读-判定-安装」两段后，用例**并非每次必失败**——`-count=30` 时第 15 轮才报 `caller 15 got a different *RefreshSource than caller 0`；恢复修复后 `-count=50` 全绿。竞态类用例的固有能力，记录在案。

**仅防御性、不可独立观测**

- **Finding 2**：`doneCh` 的 `select` 是真承重的（退回 `<-signal` 后 `listener goroutine did not exit after the wait finished` FAIL，4 > 3 个 goroutine）；但 `finished` 守卫**测不出来**——删掉它用例仍 PASS，因为 `finishLocked` 里的 `stillPending` 判定已经覆盖了「已完成即已移出注册表」这条路径。`finished` 只是让「二次 finish / 二次 close」显式不可能，不依赖该不变式。
- **Finding 6**：`armDeadline`/`clearDeadline` 的 socket 截止时间同理是防御性的——置空后用例仍 PASS，因为逻辑层 `time.AfterFunc` 已经给出了可观测的超时契约（就本子批的小帧而言不会把 `conn.Write` 堵住；大帧会堵，见 6.5）。

### 6.5 Finding 3 的用例已填满 `SO_SNDBUF`（第 3D-3 子批修订）

**本节是第 3D-3 精确收尾子批对 6.5 的改写。** 修订前此处记的是「用例空转、无法把修复前形态判红，需另立任务」；该结论已被本轮实测推翻，现按实测结果重写。

#### 修订前的判断为何不成立

原判断称「AF_UNIX 流式套接字的流控只跟发送方 `SO_SNDBUF`，接收方 `SO_RCVBUF` 不构成上限」，并以「64 帧 × 344 字节 = 22 KB 全部微秒级写完」为据。这个实测本身没错，**错在它把「payload 太小」当成了「平台不可能阻塞」**：22 KB 远未触及 AF_UNIX 默认 `SO_SNDBUF`（本机约 212 KB），写不阻塞是当然的，不能据此推断放大 payload 后仍不阻塞。

#### 修订后的构造

`fillSlowClientBuffer` 不再推 64 帧 344 字节的小帧，改为：

- 单条消息体（`event.Payload` 里的 `body` 字段）放大到 **48 KB**；
- 连续推 **342** 帧（总量约 16 MB，远超 `SO_SNDBUF` 与对端 `SO_RCVBUF` 之和）；
- 保留 `dialNonReadingClient` 的 `unix.SetReadBuffer(1024)`，进一步压低对端接收缓冲。

#### 先 FAIL 后 PASS 实测

**反证（退回修复前形态）**：把 `writeAll(frames)` 放回 `s.mu.Lock()` 与 `s.mu.Unlock()` 之间，即恢复「持锁写帧」，两条用例**双双 FAIL**，且失败信息正是锁被写占住：

```
--- FAIL: TestObservabilityRpcSlowClientDoesNotBlockOtherClients (2.11s)
    observability_rpc_slow_client_test.go:233: agent.ping took 1.701173152s, which is not faster than
    the 1s write timeout: the publish path is holding the server lock across a socket write
--- FAIL: TestObservabilityRpcSlowClientDoesNotBlockShutdown (2.07s)
    observability_rpc_slow_client_test.go:291: server.Stop took 1.702051103s, which is not faster than
    the 1s write timeout: the publish path is holding the server lock across a socket write
```

**恢复锁外写帧后两条用例 PASS**：

```
--- PASS: TestObservabilityRpcSlowClientDoesNotBlockOtherClients (2.07s)
--- PASS: TestObservabilityRpcSlowClientDoesNotBlockShutdown (0.37s)
```

#### 阻塞写确实存在的直接证据

用临时诊断程序（已删除）量得：342 帧 flood 启动后，**第 6 帧就阻塞，且一卡就是 2.00068239s**——正是 `rpcWriteTimeout`（`observability_server_handlers.go` 的 `rpcWriteTimeout = 2 * time.Second`）超时后 `writeFrame` 走 `destroy()` 断开的时长；`PublishAgentEvent` 逐帧同步推时，前 5 帧（约 120 KB）填满缓冲，第 6 帧起 `conn.Write` 真正停在内核 `SO_SNDBUF` 上。

关停用例的时序同样落在阻塞窗口内：flood goroutine 在 50ms / 150ms / 300ms 三个采样点都停在 **pushed = 6**（一帧未多推），`server.Stop()` 随后 **106µs** 返回——`Stop()` 的 `connection.destroy()` 立即解除了那个阻塞写。这正是「关停不被停读客户端拖慢」想要的状态。

**结论**：Finding 3 的修复与它的回归用例现在都是承重的。6.5 修订前记的「用例空转、需另立任务」不再成立，该任务已在第 3D-3 子批闭环。

### 6.6 五轮 `-race` 实测

`CGO_ENABLED=1 go test -buildvcs=false -count=1 -race ./...`，连跑 5 轮，每轮都覆盖全部 12 个包结果行（`go list -buildvcs=false ./...` 共 12 个包：2 个 `cmd/*` + 10 个 `internal/*`，其中 `cmd/herdsman` 与 `cmd/herdsman-daemon` 为 `no test files`，其余 10 个 `internal/*` 出 `ok`）：

| 轮次 | 结果 | 耗时 |
|------|------|------|
| 1 | PASS | 98s |
| 2 | PASS | 95s |
| 3 | PASS | 95s |
| 4 | PASS | 96s |
| 5 | PASS | 96s |

5 轮均无 `DATA RACE`、无 `FAIL`、无 `--- FAIL`。单轮样例（第 5 轮）：`internal/daemon 31.768s`、`internal/observability 95.522s`、`internal/db 14.229s`、`internal/agy 4.589s`、其余包 1s 上下。

同一状态的其它门禁：`go build -buildvcs=false ./...` 通过、`go vet -buildvcs=false ./...` 通过、`gofmt -l .` 无待格式化文件、`go test -count=1 ./...` 全绿。

### 6.7 CLI 输出实测（4.2 节修正的依据）

同一隔离 `HERDSMAN_HOME`，Go 与 TS 两侧逐字节相同：

```
$ herdsman daemon status
{"pidPath":"…/herdsman.pid","socketPath":"…/herdsman.sock","state":"stopped"}
$ herdsman daemon status --text
state: stopped
```

RPC 命令（`on`/`off`/`status`、`agent list/get/read`）默认走 `formatHumanResult` 的人读视图，只有带 `--json` 才出单行 JSON；`daemon status` 不经过 `printResult`，由入口的 `runDaemonCommand` 按 `--text` 分流。

---

### 6.8 第 3E 终审收口子批（`reviewer-final` 全部 8 项）

对应提交：`fix(go): close final review findings on db lifecycle, rpc ordering, and herdr deadlines`（第 1 节第 25 条）。本轮把 `reviewer-final` 的 8 项问题逐一闭环，并合并 3 个独立测试文件。

### 8 项问题与落点

| # | 问题 | 修复落点 |
|---|------|---------|
| 1 | `sql.DB.Close()` 不释放底层连接、不注销 `SqliteGate` | `internal/db/client.go` 的 `serializedConnector` 新增 `Close() error`（`sync.Once` 幂等）：调 `shared.UnregisterSqliteGate(db)`、关闭唯一底层 `modernc.org/sqlite` `driver.Conn`、置 `closed` 使后续 `Connect` 立即返回 `sql.ErrConnDone`；`internal/shared/sqlite_gate.go` 保持闸门本身的可重入语义，单个 `serializedConn.Close()` 仍是 no-op，供连接池复用 |
| 2 | 锁外写帧时后进临界区的协程可能先抢到 `writeMu`，客户端先收新状态帧后收旧状态帧 | `internal/daemon/observability_server.go` 的 `rpcConnection` 增加按连接串行的发送队列（`queueMu` + `queue []string` + `draining`）：持 `s.mu` 期间 `enqueueMessage`/`enqueueAll` 只做内存追加（零网络 I/O，顺序严格由 `s.mu` 固定），释放 `s.mu` 后 `flushQueue`/`flushAll` 由单个 drainer 按 FIFO 取出、加 2s 写超时写入，写失败即 `destroy()` 清空队列 |
| 3 | 持久连接共用 `conn.SetDeadline`，请求 A 完成时 `clearDeadline` 会清空并发请求 B 的 deadline；写失败未重置连接 | `internal/herdr/socket_client.go`：新增 `writeMu` 串行化持久连接的 `conn.Write`；持久连接不再设全局 `SetDeadline`，改为仅在 `writeMu` 内 `SetWriteDeadline(now+timeout)`、写完立即置零；`writeFrameLocked` 写失败即 `resetConnection()` 销毁连接（避免残留半截 JSON 帧）；读超时仍由各请求独立的 `time.NewTimer(timeout)` 与 `ctx.Done()` 控制；`requestOnce` 的独立短连接保持自己的 `SetDeadline`（`armDeadline`/`clearDeadline` 的注释已改写为「仅用于一次性连接」） |
| 4 | `turn_completion` 晚到 finish 缺确定性回归；`RunSqliteTransaction` 的 COMMIT 失败回滚缺回归 | `internal/observability/turn_completion_test.go` 的 `TestFinishedWaiterIgnoresLateSignalAndReleasesListener` 强化为「waiter1 完成并移出注册表后，同 key 注册新 signal 与 waiter2，并触发 waiter1 残留的 timeout/cancel 路径」；`internal/db/sqlite_migrations_test.go` 新增基于 `PRAGMA defer_foreign_keys = ON` 触发真实 `COMMIT` 失败的回归 |
| 5 | `OccupiedSessionPaths` 边界测试未触达生产代码 | `internal/observability/agent_context_service_test.go` 的 `TestAgentContextServiceRediscoversOnOccupiedChange` 改用真实 `agenthistory.CreateAgentHistoryService`（包内无 `NewService`，构造函数名以源码为准），配 `contextPiSession` 写合法 pi JSONL；`internal/agenthistory/discovery_test.go` 顶部注释保持 `5fce550` 的原样 |
| 6 | 报告 6.1 节的函数名与落点不准 | 已按 `git show 5fce550 -- <file>` 逐项改写（见 6.1），不照抄任务书的近似名称 |
| 7 | 3 个新增测试文件未合入既有测试文件 | `go/internal/db/sqlite_transaction_test.go` → `go/internal/db/sqlite_migrations_test.go`；`go/internal/observability/agent_index_refresh_sharing_test.go` → `go/internal/observability/agent_index_service_test.go`；`go/internal/daemon/observability_rpc_slow_client_test.go` → `go/internal/daemon/observability_rpc_test.go`。三个文件均已删除 |
| 8 | 报告 4.3 推送方法列表不全、6.6 包数量表述不准、缺架构决策专节 | 4.3 补全 3 个推送方法；6.6 改为「`go list -buildvcs=false ./...` 共 12 个包：2 个 `cmd/*` + 10 个 `internal/*`」；新增 6.9 记录 `SqliteGate` 与 `rpcConnection` 的架构决策 |

### 本轮新增回归的先 FAIL 后 PASS 取证

**Finding 1（`connector.Close()`）**：退回「`serializedConnector` 无 `Close()`」后 `TestOpenSqliteCloseReleasesConnectionAndGate` FAIL——`db.Close()` 之后 `SqliteGate` 仍在注册表里，且底层连接未关；补上 `Close()` 后 PASS。

**Finding 2（发送队列顺序）**：`TestObservabilityRpcPushedFramesKeepTheirGeneratedOrder` 在把 `enqueueAll` 移回锁外（恢复「锁内构造、锁外 encode+write」的旧形态）后确定性 FAIL，报出 `[2 1]`——客户端收到的帧序号非单调。关键在 payload：用 50 000 个 key 的 map 把单帧撑到约 770 KB、编码耗时约 18ms，临界区才长到足以让乱序稳定复现（900 KB 只有约 2ms 窗口，不够）。修复形态（锁内 enqueue、锁外单 drainer flush）下同一用例 PASS。

**Finding 3（herdr 写锁与写超时）**：`TestHerdrSocketClientConcurrentRequestsKeepTheirOwnTimeouts` 三个子测试。「写失败重置连接」子测试在删掉 `writeFrameLocked` 里的 `resetConnection()` 后 FAIL：`getPane after a failed write err = write unix @->…: use of closed network connection, want Herdr socket closed`。注意「并发请求互不清 deadline」子测试**测不出**差异——请求自身的 `time.NewTimer` 仍是真正的边界，退回共享 `SetDeadline` 形态用例仍 PASS；这一项按防御性记录在案。

**Finding 4（COMMIT 失败回滚）**：`TestRunSqliteTransactionRollsBackAfterFailedCommit` 在临时注释掉 `RunSqliteTransaction` 里 commit 失败后的 `rollback` 后 FAIL，第二次事务报 `cannot start a transaction within a transaction`，与任务书预期逐字一致；恢复后 PASS。

**Finding 4（晚到 finish）**：删掉 abort 监听 goroutine 的 `case <-pending.doneCh` 分支后，`listener goroutine did not exit after the wait finished (4 goroutines, baseline 3)` 确定性 FAIL；恢复后 PASS。`finished` 守卫本身测不出差异（`stillPending` 判定已覆盖同一路径），按防御性记录。

**Finding 5（真实 OccupiedSessionPaths 边界）**：把 `occupiedChanged` 临时恒置 false 后，`second call forceDiscovery = false, want true` FAIL；恢复后 PASS。该用例现在穿过真实的 `DiscoverAgentHistory` / `StatSourceFingerprint` / `ReadCompactRef` 链路，而不是只对着一个 fake 记录的 options 对象。

### 本轮验证

- `gofmt -w cmd internal` 后 `gofmt -l cmd internal` 为空（此前 3 个文件待格式化，均已格式化）。
- `CGO_ENABLED=0 go build -buildvcs=false ./...` 通过；`CGO_ENABLED=0 go vet -buildvcs=false ./...` 通过。
- `CGO_ENABLED=0 go test -buildvcs=false -count=1 ./...`：12 个包全绿（2 个 `no test files` + 10 个 `ok`），其中 `internal/daemon 12.701s`、`internal/observability 5.209s`、`internal/agy 2.805s`、`internal/herdr 1.162s`、`internal/db 0.565s`。
- 5 轮 `-race`（`CGO_ENABLED=1 go test -race -buildvcs=false -count=5 ./internal/db/... ./internal/observability/... ./internal/herdr/... ./internal/daemon/... ./internal/agenthistory/...`）：全部 `ok`，无 `DATA RACE`、无 `FAIL`。耗时 `internal/observability 472.804s`、`internal/daemon 166.643s`、`internal/db 64.859s`、`internal/herdr 6.852s`、`internal/agenthistory 1.516s`、`internal/db/dedup 1.030s`。

---

### 6.9 架构决策：`SqliteGate` 生命周期与 `rpcConnection` 发送队列

以下两项是本轮（第 3E 子批）落地的结构性决策。**因 `PLAN.md` 第 0 节规定 `go-port` 分支只能修改 `go/` 目录下的文件（`git diff --name-only main...HEAD | grep -v '^go/'` 必须为空），本节不写入仓根 `.agents/notes/`，只在本报告内记录。**

### `SqliteGate`：`connector.Close()` 生命周期与 `runtime.Stack` GID 重入设计

`internal/shared/sqlite_gate.go` 实现一个按 goroutine 可重入的数据库闸门：闸门按 OS 线程的 goroutine ID 记账，同一 goroutine 可多次 `Enter`，只有最外层 `Exit` 真正放行；跨 goroutine 的第二次进入则排队，从而把 `database/sql` 连接池退化成「一个时刻只有一个写入者」的串行语义，与 TS 单事件循环里两个 `await` 之间的块原子等价。

- **GID 取自 `runtime.Stack`**：Go 没有暴露 goroutine ID 的公开 API，`sqlite_gate.go` 用一次受控的 `runtime.Stack(buf, false)` 抓取栈帧首行（`goroutine N [running]:`）解析出 N。这是该设计唯一的「非公开契约」依赖，但它是标准库内部实现的稳定格式，且失败时退化为「不识别为重入」而不是放行竞态。
- **重入是必需的，不是优化**：`RunSqliteTransaction` 全程持闸，而事务回调 `fn()` 里还会调 `db.Query`/`db.Exec`；若闸门不可重入，第一次内层调用就会死锁。
- **`connector.Close()` 的生命周期**：`database/sql` 在 `db.Close()` 时若发现 `db.connector` 实现 `io.Closer` 会自动调用 `connector.Close()`。`internal/db/client.go` 的 `OpenSqlite` 因此给自定义 `driver.Connector`（`serializedConnector`）加了 `Close() error`，用 `sync.Once` 保证幂等，在其中做三件事：`shared.UnregisterSqliteGate(db)`（把闸门从注册表摘掉，避免 `db` 被 GC 后注册表仍持引用）、关闭唯一的底层 `modernc.org/sqlite` `driver.Conn`、把 `closed` 置 true 使后续 `Connect` 立即返回 `sql.ErrConnDone`。单个 `serializedConn.Close()` 仍保持 no-op——它归连接池所有，池会反复开关它。
- **为什么注册表需要注销**：闸门按 `*sql.DB` 注册，daemon 在测试或重载场景下会开多个 DB；不注销时注册表随 DB 数量无界增长，且旧 DB 的闸门状态会干扰新 DB 的判断。

### `rpcConnection` 发送队列与 2s 写超时

`internal/daemon/observability_server.go` 的 `rpcConnection` 维护一个按连接串行的发送队列（`queueMu sync.Mutex`、`queue []string`、`draining bool`）。

- **为什么必须「锁内 enqueue、锁外 flush」**：TS 的事件循环保证两个 `await` 之间的块原子，所以「生成顺序」与「写出顺序」天然一致。Go 移植后如果在 `s.mu` 内直接 `conn.Write`，一个慢客户端就能把服务器锁堵住数秒（6.5 实测第 6 帧起阻塞 2.00068239s）；但如果改成「锁内构造帧、锁外写」，后进临界区的协程 B 可能在解锁后先抢到 `writeMu`，客户端就会先收到新状态帧、后收到旧状态帧。
- **队列如何同时解决两者**：持 `s.mu` 期间只 `enqueueMessage`/`enqueueAll`（纯内存追加，零网络 I/O），顺序由 `s.mu` 严格固定；释放 `s.mu` 后 `flushQueue`/`flushAll` 由单个 drainer 按 FIFO 取出帧写入。写操作因此既不在锁内、也不与生成顺序竞争。
- **单个 drainer**：`enqueueMessage` 在没有 drainer 时直接拉起一个，`flushQueue` 不重复拉起，避免两个 drainer 互相竞争把 FIFO 打破。drainer 退出前会重新检查队列，防「刚有新帧入队就退出」的竞态。
- **2s 写超时**（`observability_server_handlers.go` 的 `rpcWriteTimeout = 2 * time.Second`）：每个帧写入前 `SetWriteDeadline(now+2s)`，超时即 `destroy()` 断开该连接并清空队列。取 2s 是因为推送帧本身很小（几十字节到几 KB），正常情况下写入是微秒级；2s 只可能被「对端停读且缓冲区已满」触发，那种连接本就应当被丢弃。
- **与 6.5 的关系**：6.5 已证明慢客户端确实能把 `conn.Write` 堵在 `SO_SNDBUF` 上超过 2s，所以这个超时不是理论值，而是被真实阻塞窗口验证过的边界。

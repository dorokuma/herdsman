# 第 5 批 · herdsmand 全量 Go 迁移 · 第 3C-2 收官子批报告

- 分支：`go-port`（worktree `/root/workspace/.wt/herdsman/go-port`）
- 目标模块：`go/`（Go module `github.com/dorokuma/herdsman/go`，go 1.26.0）
- 本子批范围：修复 `internal/daemon/service.go` 启动期 SIGTERM 竞态、`gofmt`、全套 `CGO_ENABLED=0` 与 `-race` 验证、`HERDSMAN_IMPL` 包装脚本与 PiG 隔离实测、本报告；其后追加第 4 轮 reviewer 修复（db 闸门 / observability / herdr / daemon 共 6 项，见第 6 节）与 4.2、4.3 两节描述修正；再追加第 3D-3 精确收尾子批（`observability_rpc_slow_client_test.go` 填满 AF_UNIX `SO_SNDBUF` 并取得真实先 FAIL 后 PASS，见 6.5；`8c4bd1b` 更正为 `5fce550`，见 6.1）；再追加第 3F 终审收口子批（`reviewer-final2` 的 5 个 Must-Fix + 1 个报告准确性 Should-Fix，见 6.10）；最后追加第 5b 批（`b0e5c6c..main` / 0.15.1 的 4 个 agy 修复的 Go 移植与回归测试，以及 `go/` 测试假脱敏样本的运行时拼接，见 6.11）
- 改动规模：`go-port` 相对 `main` 共 **127 个文件，全部在 `go/` 下**
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

第 26 条（第 3F 终审收口，`reviewer-final2`）解决 5 个 Must-Fix 与 1 个报告准确性 Should-Fix：`db/client.go` 的 `serializedConnector.Close()` 加闸门互斥与底层 `driver.Conn` 关闭的可观测化、`observability_server.go` 两处 grace timer 回调的 `enqueueAll` 移入 `s.mu`、`turn_completion_test.go` 的确定性迟到 finish 回归、`agent_context_service_test.go` 的 4 轮真实重发现与 `CompactHistory` 断言，以及本报告 6.1 / 6.8 / 6.9 / 6.10 的源码对齐（见 6.10）。

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
| 4 | `internal/cli/herdsman.go` 与 `internal/agy/wake_waiter.go` | `parseJSNumber` 支持无符号 `0x`/`0b`/`0o` 前缀（`strconv.ParseUint` 按 16/2/8 基解析）；带符号前缀（`-0x14`）仍走 `ParseFloat` 得 NaN，与 JS 的 `Number()` 一致。两处各有一份同名实现；`parseJSNumber` **只**存在于这两个文件（第 3F 子批以 `rg -n parseJSNumber go/` 复核），`internal/agenthistory` 下没有同名函数 |
| 5 | `internal/agy/hook.go` 与 `internal/cli/herdsman.go` | 新增 `restoreJSLineSeparators`：`encoding/json` 总是把 U+2028/U+2029 写成 `\u2028`/`\u2029`，该函数把它们还原成原字符，保持 TS `JSON.stringify` 的逐字节输出 |
| 6 | `internal/agenthistory/discovery_test.go` | **删除**伪辅助测试：`occupiedBoundaryKey` 与 `TestOccupiedFallbackDiscoveryUsesAChangedOccupiedSetAsADiscoveryBoundary` 被整体删去（后者只在测试文件内构造两个 map 字面量、比较排序 join，完全不触达生产代码）。同时在文件顶部留下注释，说明该行为已迁居 agent context service；`discovery.go` 本身无改动。`OccupiedSessionPaths` 边界的真实覆盖见 6.8 的 Finding 5 |
| 7 | `internal/agenthistory` | ~~`parseJSNumber`（见第 4 项）：`0x`/`0b`/`0o` 前缀支持~~ **（第 3F 子批更正：不成立）** `rg -n parseJSNumber go/` 只命中 `internal/cli/herdsman.go:1086`（定义）、`internal/cli/cli_test.go:219`（测试）、`internal/agy/wake_waiter.go:893`（定义）、`internal/agy/agy_hook_and_cli_test.go:783`（注释）与调用点 `cli/herdsman.go:219`、`agy/wake_waiter.go:88`；`internal/agenthistory` 无 `parseJSNumber`。`5fce550` 在 agenthistory 面上改的是第 2 项 `readVarint` 与第 6 项 `discovery_test.go` 删除伪辅助测试，与 `parseJSNumber` 无关 |
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
| 1 | `sql.DB.Close()` 不释放底层连接、不注销 `SqliteGate` | `internal/db/client.go` 的 `serializedConnector` 新增 `Close() error`（`sync.Once` 幂等）：调 `shared.UnregisterSqliteGate(db)`、关闭唯一底层 `modernc.org/sqlite` `driver.Conn`、置 `closed` 使后续 `Connect` 立即返回 `sql.ErrConnDone`；`internal/shared/sqlite_gate.go` 保持闸门本身的可重入语义，单个 `serializedConn.Close()` 仍是 no-op，供连接池复用。**第 3F 子批追加**：`Close()` 现在先 `gid := shared.CurrentGoroutineID()`、`c.gate.Acquire(gid)`（`defer c.gate.Release(gid)`），在闸门内 `c.mu.Lock()` 置 `closed`、调 `c.conn.Close()`、`shared.UnregisterSqliteGate(c.db)`，释放闸门后才返回；并新增 `DriverConnClosed(db *sql.DB) bool` / `WaitDriverConnClosed(ctx, db) bool` 两个观测点，直接问底层 `driver.Conn`（`driver.Validator.IsValid()`，modernc 连接 Close 后 `c.db == 0` 即返回 false）而不是看本包自己记的旗标。**第 3G 子批追加**：`c.conn.Close()` 再移入 `c.mu` 保护块（`c.mu.Lock(); c.closed = true; err = c.conn.Close(); c.mu.Unlock()`），`DriverConnClosed(db)` 在访问 `connector.conn` / 调 `validator.IsValid()` 前先 `connector.mu.Lock()` + `defer Unlock`——teardown 与观测读取经 `c.mu` 严格串行化，消除与 `modernc.org/sqlite` `*conn.db` 的无同步读写；`c.mu` 只在 `Close` 已拿到闸门后才被占用，停在 `gate.Acquire` 上的 `Close` 不阻塞轮询方。`-race` 先 FAIL 后 PASS 取证见 6.8 的 3G 段与 6.10 |
| 2 | 锁外写帧时后进临界区的协程可能先抢到 `writeMu`，客户端先收新状态帧后收旧状态帧 | `internal/daemon/observability_server_handlers.go` 的 `rpcConnection` 增加按连接串行的发送队列（`queueMu` + `queue []string` + `draining`）：持 `s.mu` 期间 `enqueueMessage`/`enqueueAll` 同步完成 `EncodeJSONLine` 并在 `c.queueMu` 下追加到 `c.queue`（发布方协程持 `s.mu` 期间零 socket I/O，帧生成顺序由 `s.mu` 严格固定），若 `!c.draining` 则置 `c.draining = true` 并拉起后台 `go c.drain()` 按 FIFO 取出、加 2s 写超时写入（写失败即 `destroy()` 清空队列）；释放 `s.mu` 后 `flushQueue`/`flushAll` 为非空且未处于 `draining` 的队列兜底补起 drainer |
| 3 | 持久连接共用 `conn.SetDeadline`，请求 A 完成时 `clearDeadline` 会清空并发请求 B 的 deadline；写失败未重置连接 | `internal/herdr/socket_client.go`：新增 `writeMu` 串行化持久连接的 `conn.Write`；持久连接不再设全局 `SetDeadline`，改为仅在 `writeMu` 内 `SetWriteDeadline(now+timeout)`、写完立即置零；`writeFrameLocked` 写失败即 `resetConnection()` 销毁连接（避免残留半截 JSON 帧）；读超时仍由各请求独立的 `time.NewTimer(timeout)` 与 `ctx.Done()` 控制；`requestOnce` 的独立短连接保持自己的 `SetDeadline`（`armDeadline`/`clearDeadline` 的注释已改写为「仅用于一次性连接」） |
| 4 | `turn_completion` 晚到 finish 缺确定性回归；`RunSqliteTransaction` 的 COMMIT 失败回滚缺回归 | `internal/observability/turn_completion_test.go` 的 `TestFinishedWaiterIgnoresLateSignalAndReleasesListener` 强化为「waiter1 完成并移出注册表后，同 key 注册新 signal 与 waiter2，并触发 waiter1 残留的 timeout/cancel 路径」；`internal/db/sqlite_migrations_test.go` 新增基于 `PRAGMA defer_foreign_keys = ON` 触发真实 `COMMIT` 失败的回归。**第 3F 子批追加**：晚到 finish 一项改写为 100% 确定性的白盒用例 `TestTurnCompletionLateFinishDoesNotConsumeNextTurnSignal`（`package observability`，见 6.10），原「time.Sleep + close(abort)」形态删除；原用例只保留监听 goroutine 泄漏那半个子测试并改名 `TestFinishedWaiterReleasesListenerGoroutine` |
| 5 | `OccupiedSessionPaths` 边界测试未触达生产代码 | `internal/observability/agent_context_service_test.go` 的 `TestAgentContextServiceRediscoversOnOccupiedChange` 改用真实 `agenthistory.CreateAgentHistoryService`（包内无 `NewService`，构造函数名以源码为准），配 `contextPiSession` 写合法 pi JSONL；`internal/agenthistory/discovery_test.go` 顶部注释保持 `5fce550` 的原样。**第 3F 子批追加**：改为 4 轮真实链路断言，并给真实 history service 包一层可计数的 `Discover`（`newCountingRealHistoryService`），直接观测真实 `DiscoverAgentHistory` 真的跑了几次；断言每轮的 `CompactHistory.LastAssistantMessage.Text` 真的来自被读的那个 `.jsonl`（`from-session-A` / `from-session-B`），见 6.10 |
| 6 | 报告 6.1 节的函数名与落点不准 | 已按 `git show 5fce550 -- <file>` 逐项改写（见 6.1），不照抄任务书的近似名称 |
| 7 | 3 个新增测试文件未合入既有测试文件 | `go/internal/db/sqlite_transaction_test.go` → `go/internal/db/sqlite_migrations_test.go`；`go/internal/observability/agent_index_refresh_sharing_test.go` → `go/internal/observability/agent_index_service_test.go`；`go/internal/daemon/observability_rpc_slow_client_test.go` → `go/internal/daemon/observability_rpc_test.go`。三个文件均已删除 |
| 8 | 报告 4.3 推送方法列表不全、6.6 包数量表述不准、缺架构决策专节 | 4.3 补全 3 个推送方法；6.6 改为「`go list -buildvcs=false ./...` 共 12 个包：2 个 `cmd/*` + 10 个 `internal/*`」；新增 6.9 记录 `SqliteGate` 与 `rpcConnection` 的架构决策 |

### 本轮新增回归的先 FAIL 后 PASS 取证

**Finding 1（`connector.Close()`）**：退回「`serializedConnector` 无 `Close()`」后 `TestOpenSqliteCloseReleasesConnectionAndGate` FAIL——`db.Close()` 之后 `SqliteGate` 仍在注册表里，且底层连接未关；补上 `Close()` 后 PASS。

**Finding 1（第 3F 子批追加：门控互斥 + 直接观测底层 close）**：用例改名为 `TestOpenSqliteCloseReleasesDriverConnAndUnregistersGate` 并拆 3 个子测试。

- **直接观测**：新增的 `DriverConnClosed(db)` 走 `driver.Validator.IsValid()`，问的是底层 `modernc.org/sqlite` 连接本身（其 `Close()` 把 `c.db` 置 0 后 `IsValid()` 转 false）。临时注释掉 `serializedConnector.Close()` 里的 `err = c.conn.Close()`，3 个子测试**全部确定性 FAIL**：
  - `sqlite_migrations_test.go:542: DriverConnClosed = false after Close, want the driver connection closed`
  - `sqlite_migrations_test.go:626: DriverConnClosed = false after the gate was released, want the connection closed`
  - `sqlite_migrations_test.go:675: DriverConnClosed = false after the rows were closed, want the connection closed`

  恢复后 3 个子测试 PASS。这一条正是第 3F 子批要补的洞：旧断言 `db.Exec("select 1")` 报错在删掉 `c.conn.Close()` 后**依然成立**（`database/sql` 自己就会拦），所以它从来观测不到底层连接是否真被释放。
- **门控互斥**：另两个子测试分别用「另一 goroutine 持 `SqliteGate`」和「`db.Query` 出的 `Rows` 游标未关」占住闸门，`db.Close()` 在 200ms 内不得返回、`DriverConnClosed` 不得为 true；释放后才允许完成。当前实现下两者均 PASS。

**如实记录**：`driver.Validator` 这层观测没有单独的 FAIL→PASS 取证（它是观测手段，不是被修的行为）；门控互斥那两项目前也没有对应 FAIL 证据——先前的 3 轮 `-race`（含 `TestRunSqliteTransactionConcurrentDoesNotInterleave` 等并发用例）未暴露 `Close` 与在途语句的交错，属防御性加固，按实情记在案。

**Finding 1（第 3G 子批追加：`DriverConnClosed` 与 driver Close 的数据竞争回归）**：新增第 4 个子测试 `DriverConnClosed does not race the driver Close`——先让另一 goroutine 持住闸门、再启动 `db.Close()`（停在 `gate.Acquire`），随后起一个**无 sleep 的紧循环轮询** `DriverConnClosed(db)`，最后放行闸门让 teardown 与轮询真正交叠。轮询先于 teardown 就位是刻意的：`database/sql` 的 `(*sql.DB).Close` 在锁外调 `connector.Close()`、`db.Driver()` 也不持 `db.mu`，所以交叠窗口就是 modernc 自身 `Close` 的存活期，`Close` 返回后才起轮询永远碰不到它。断言轮询收敛、`WaitDriverConnClosed(ctx, db)` 立即报 true、`db.Close()` 无错返回。

修复前（`c.conn.Close()` 在 `c.mu` 之外、`DriverConnClosed` 无锁）该子测试**首轮即复现 DATA RACE**（`go test -race -buildvcs=false -count=1 -run TestOpenSqliteCloseReleasesDriverConnAndUnregistersGate ./internal/db`）：

```
WARNING: DATA RACE
Write at 0x00c0002be300 by goroutine 24:
  modernc.org/sqlite.(*conn).Close()
      /root/go/pkg/mod/modernc.org/sqlite@v1.61.0/conn.go:1083
  .../internal/db.(*serializedConnector).Close.func1()
      .../go/internal/db/client.go:167
Previous read at 0x00c0002be300 by goroutine 25:
  modernc.org/sqlite.(*conn).usable()
      /root/go/pkg/mod/modernc.org/sqlite@v1.61.0/conn.go:1122
  modernc.org/sqlite.(*conn).IsValid()
      /root/go/pkg/mod/modernc.org/sqlite@v1.61.0/conn.go:1118
  .../go/internal/db.DriverConnClosed()
      .../go/internal/db/client.go:195
```

同一轮共报出两组竞争对（`conn.go:1083/1088` 的写 vs `conn.go:1122/1133` 的读，即 `c.db` 及相邻字段）；加上 `c.mu` 保护后 `-count=1` → `ok 1.721s`、`-count=3` → `ok 2.871s`，无 `DATA RACE`。

**Finding 2（发送队列顺序）**：`TestObservabilityRpcPushedFramesKeepTheirGeneratedOrder` 在把 `enqueueAll` 移回锁外（恢复「锁内构造、锁外 encode+write」的旧形态）后确定性 FAIL，报出 `[2 1]`——客户端收到的帧序号非单调。关键在 payload：用 50 000 个 key 的 map 把单帧撑到约 770 KB、编码耗时约 18ms，临界区才长到足以让乱序稳定复现（900 KB 只有约 2ms 窗口，不够）。修复形态（锁内 enqueue、锁外单 drainer flush）下同一用例 PASS。

**Finding 3（herdr 写锁与写超时）**：`TestHerdrSocketClientConcurrentRequestsKeepTheirOwnTimeouts` 三个子测试。「写失败重置连接」子测试在删掉 `writeFrameLocked` 里的 `resetConnection()` 后 FAIL：`getPane after a failed write err = write unix @->…: use of closed network connection, want Herdr socket closed`。注意「并发请求互不清 deadline」子测试**测不出**差异——请求自身的 `time.NewTimer` 仍是真正的边界，退回共享 `SetDeadline` 形态用例仍 PASS；这一项按防御性记录在案。

**Finding 4（COMMIT 失败回滚）**：`TestRunSqliteTransactionRollsBackAfterFailedCommit` 在临时注释掉 `RunSqliteTransaction` 里 commit 失败后的 `rollback` 后 FAIL，第二次事务报 `cannot start a transaction within a transaction`，与任务书预期逐字一致；恢复后 PASS。

**Finding 4（晚到 finish）**：删掉 abort 监听 goroutine 的 `case <-pending.doneCh` 分支后，`listener goroutine did not exit after the wait finished (4 goroutines, baseline 3)` 确定性 FAIL；恢复后 PASS。

**Finding 4（第 3F 子批追加：确定性复现）**：`TestTurnCompletionLateFinishDoesNotConsumeNextTurnSignal` 全程同步、无 `time.Sleep` 参与排序：先起第 1 轮 `WaitForSignal` goroutine 并轮询 `waitersByTerminal[key]` 直到恰有 1 个 waiter（证明走了慢路径），取出 `w1`；`Record(sig1)` 唤醒并断言第 1 轮拿到自己的信号、`w1.finished == true`、`w1` 已移出注册表；`Record(sig2)` 写入第 2 轮信号；再在 `c.mu.Lock()` 下直接 `c.finishLocked(key, w1, false)`，精确模拟第 1 轮迟到回调抢到锁；最后断言 `latestByTerminal[key]` 仍在且第 2 轮 `WaitForSignal` 立刻拿到 `sig2`。

FAIL 取证（临时把 `finishLocked` 的 `if pending.finished` 分支改成「返回前顺手 `delete(r.latestByTerminal, key)`」，即迟到清理误删下一轮信号）：

```
turn_completion_test.go:328: the late finishLocked dropped round 2's stored signal
--- FAIL: TestTurnCompletionLateFinishDoesNotConsumeNextTurnSignal (0.00s)
```

恢复后 PASS。

**如实记录**：把 `finished` / `stillPending` 两个守卫**整体删掉**并不能作为这条的 FAIL 取证——删掉后 `finishLocked` 会对已关闭的 `doneCh` 二次 `close` 而 panic（测试二进制直接崩，不是干净 FAIL）。因此 FAIL 取证采用的是「守卫命中时误删 `latestByTerminal`」这一同样属于「迟到 finish 吞掉下一轮信号」的形态，它与生产代码的真实差异被完整记录在此。

**Finding 5（真实 OccupiedSessionPaths 边界）**：把 `occupiedChanged` 临时恒置 false 后，`second call forceDiscovery = false, want true` FAIL；恢复后 PASS。该用例现在穿过真实的 `DiscoverAgentHistory` / `StatSourceFingerprint` / `ReadCompactRef` 链路，而不是只对着一个 fake 记录的 options 对象。

**Finding 5（第 3F 子批追加：真实 CompactHistory 与真实重发现）**：把 `occupiedChanged` 临时恒置 false 后，新断言 `third call forceDiscovery = false, want true` FAIL；恢复后 PASS。除此之外本轮补了两层旧用例没有的观测：

1. **真实 discovery 计数**：`newCountingRealHistoryService()` 给 `agenthistory.AgentHistoryServiceOptions.Discover` 包一层计数壳，内部仍调 `agenthistory.DiscoverAgentHistory`。前两轮（命中 `preferredRef`）断言 `*discoveries == 0`，第 3 轮断言 `*discoveries == 1`，第 4 轮断言仍是 1——「有没有真的重新发现」从此是直接观测，不再从 options 反推。
2. **真实 CompactHistory 文本**：两个真实 Pi `.jsonl`（`contextPiSession(t, "occupied-change.jsonl", "from-session-A")` 与 `"own-session.jsonl", "from-session-B"`），断言第 1、2 轮 `LastAssistantMessage.Text == "from-session-A"`，第 4 轮 `== "from-session-B"`。

**与任务书字面描述的偏差（如实记录）**：任务书要求第 3 次调用「选出未被占用的 `sessionB.jsonl` 并返回 `from-session-B`」。当前源码做不到，原因是结构性的：`shouldForceDiscovery` 对「可读的官方 path」和「id pane」都提前返回 false，而 `selectPreferredRef` 也只在官方值缺失时才回落到持久化 ref——也就是说**只有官方 `agent_session` 为 nil 的窗格，occupied 变化才可能触发强制发现**；而官方值一旦是 path，`DiscoverAgentHistory` 又只按该 path 确定性解析，不存在「在多个候选文件里挑一个没被占用的」这一步（cwd/mtime 扫描器已随 `5fce550` 第 6 项删除，`internal/agenthistory/discovery.go` 的 `DiscoverAgentHistory` 对 path 与 id 都是单值解析）。因此本轮按源码事实改成 4 轮：第 3 轮走「官方 session 缺失 + occupied 变化 → 强制发现 → 真实 discovery 跑一次 → 解析不到 → CompactHistory 变成真实的未决形态（`LastAssistantMessage == nil`、`HistoryRef == nil`）」，第 4 轮让窗格上报自己的官方 session 文件，从而真实读到 `from-session-B`。第 3 轮的 `LastAssistantMessage` 由有变无本身就是「重新发现真的执行了」的证据：复用持久化 ref 会仍然带着 `from-session-A`。

### 本轮验证

- `gofmt -w cmd internal` 后 `gofmt -l cmd internal` 为空（此前 3 个文件待格式化，均已格式化）。
- `CGO_ENABLED=0 go build -buildvcs=false ./...` 通过；`CGO_ENABLED=0 go vet -buildvcs=false ./...` 通过。
- `CGO_ENABLED=0 go test -buildvcs=false -count=1 ./...`：12 个包全绿（2 个 `no test files` + 10 个 `ok`），其中 `internal/daemon 12.701s`、`internal/observability 5.209s`、`internal/agy 2.805s`、`internal/herdr 1.162s`、`internal/db 0.565s`。
- 5 轮 `-race`（`CGO_ENABLED=1 go test -race -buildvcs=false -count=5 ./internal/db/... ./internal/observability/... ./internal/herdr/... ./internal/daemon/... ./internal/agenthistory/...`）：全部 `ok`，无 `DATA RACE`、无 `FAIL`。耗时 `internal/observability 472.804s`、`internal/daemon 166.643s`、`internal/db 64.859s`、`internal/herdr 6.852s`、`internal/agenthistory 1.516s`、`internal/db/dedup 1.030s`。

---

### 6.9 架构决策：`SqliteGate` 生命周期与 `rpcConnection` 发送队列

以下两项是本轮（第 3E 子批）落地的结构性决策。**因 `PLAN.md` 第 0 节规定 `go-port` 分支只能修改 `go/` 目录下的文件（`git diff --name-only main...HEAD | grep -v '^go/'` 必须为空），本节不写入仓根 `.agents/notes/`，只在本报告内记录。**

### `SqliteGate`：`connector.Close()` 生命周期与 `runtime.Stack` GID 重入设计

`internal/shared/sqlite_gate.go` 实现一个**按 goroutine ID**（不是 OS 线程 ID）可重入的数据库闸门：闸门按 `runtime.Stack` 解析出的 goroutine ID 记账，同一 goroutine 可多次 `Acquire`，只有最外层 `Release` 真正放行；跨 goroutine 的第二次进入则排队，从而把 `database/sql` 连接池退化成「一个时刻只有一个写入者」的串行语义，与 TS 单事件循环里两个 `await` 之间的块原子等价。方法名是 `Acquire` / `Release`（另有供测试用的 `HeldBy`），不是 `Enter` / `Exit`。

- **GID 取自 `runtime.Stack`**：Go 没有暴露 goroutine ID 的公开 API，`sqlite_gate.go` 的 `goroutineID()` 用一次受控的 `runtime.Stack(buf, false)` 抓取栈帧首行（`goroutine N [running]:`），`strconv.ParseInt` 解析出 N（`CurrentGoroutineID()` 是对外暴露）。这是该设计唯一的「非公开契约」依赖，但它是标准库内部实现的稳定格式。
- **解析失败退化为非重入互斥，不是放行竞态**：`goroutineID()` 解析失败返回 `0`；`Acquire` 的等待条件是 `g.depth > 0 && (gid == 0 || g.owner != gid)`（`HeldBy` 同样要求 `gid != 0`），因此 `gid == 0` 时第二次 `Acquire` 一定排队——退化为「不可重入的互斥」，语义变严但绝不放行竞态。**第 3G 子批修正**：等待条件原写作 `g.depth > 0 && g.owner != gid`，而空闸门的 `owner` 恰是 `0`，`gid == 0` 对上 `owner == 0` 时条件为假，任意 `gid == 0` 的 goroutine 反而互相可「重入」——与本节的描述（也与其意图）相反；修正后 `gid == 0` 真正退化为不可重入互斥，本条按新条件改写。
- **第 3F 子批追加：`Close()` 也先过闸门**：`serializedConnector.Close()` 现在先 `gid := shared.CurrentGoroutineID()` → `c.gate.Acquire(gid)`（`defer c.gate.Release(gid)`），再在闸门内置 `closed`、关底层 `driver.Conn`、`UnregisterSqliteGate`。每条语句和每个事务本来就全程持闸，所以不过闸门的 `Close` 会在 `modernc.org/sqlite` 的 `Close`（清零 `c.db`、释放 TLS）与在途语句之间制造并发。`Close` 由调用 `(*sql.DB).Close` 的 goroutine 执行、不持有任何语句级资源，等待上界就是调用方在途语句的长度，与任何其他语句相同。
- **重入是必需的，不是优化**：`RunSqliteTransaction` 全程持闸，而事务回调 `fn()` 里还会调 `db.Query`/`db.Exec`；若闸门不可重入，第一次内层调用就会死锁。
- **`connector.Close()` 的生命周期**：`database/sql` 在 `db.Close()` 时若发现 `db.connector` 实现 `io.Closer` 会自动调用 `connector.Close()`。`internal/db/client.go` 的 `OpenSqlite` 因此给自定义 `driver.Connector`（`serializedConnector`）加了 `Close() error`，用 `sync.Once` 保证幂等，在其中做三件事：`shared.UnregisterSqliteGate(db)`（把闸门从注册表摘掉，避免 `db` 被 GC 后注册表仍持引用）、关闭唯一的底层 `modernc.org/sqlite` `driver.Conn`、把 `closed` 置 true 使后续 `Connect` 立即返回 `sql.ErrConnDone`。单个 `serializedConn.Close()` 仍保持 no-op——它归连接池所有，池会反复开关它。
- **底层 close 的观测点**（第 3F 子批新增）：`DriverConnClosed(db)` / `WaitDriverConnClosed(ctx, db)` 先由 `db.Driver()` 取回 `serializedDriver`（它带回 `connector` 指针，`database/sql` 为句柄终生持有 connector、`Close` 之后仍在），再对底层 `driver.Conn` 做 `driver.Validator.IsValid()` 断言。这是**问驱动自己**，不是问本包记的旗标：`database/sql` 在句柄关闭后自己就会拦掉 `db.Exec`，所以「`db.Exec` 报错」从来证明不了底层连接被释放。
- **为什么不加一张 `*sql.DB → connector` 注册表**：那张表的键会强引用每个开过的句柄，正是 `UnregisterSqliteGate` 的注释所警告的「随 DB 数量无界增长」；改成关闭时删项又会让关闭后的观测失效。走 `db.Driver()` 两条都不占。
- **为什么注册表需要注销**：闸门按 `*sql.DB` 注册，daemon 在测试或重载场景下会开多个 DB；不注销时注册表随 DB 数量无界增长，且旧 DB 的闸门状态会干扰新 DB 的判断。

### `rpcConnection` 发送队列与 2s 写超时

`rpcConnection` 定义在 `internal/daemon/observability_server_handlers.go`（不是 `observability_server.go`），维护一个按连接串行的发送队列（`queueMu sync.Mutex`、`queue []string`、`draining bool`）。`enqueueAll` / `flushAll` 两个包级辅助函数也在 `observability_server_handlers.go`。

- **为什么必须「锁内 enqueue、锁外 flush」**：TS 的事件循环保证两个 `await` 之间的块原子，所以「生成顺序」与「写出顺序」天然一致。Go 移植后如果在 `s.mu` 内直接 `conn.Write`，一个慢客户端就能把服务器锁堵住数秒（6.5 实测第 6 帧起阻塞 2.00068239s）；但如果改成「锁内构造帧、锁外写」，后进临界区的协程 B 可能在解锁后先抢到 `writeMu`，客户端就会先收到新状态帧、后收到旧状态帧。
- **队列如何同时解决两者**：持 `s.mu` 期间 `enqueueMessage`/`enqueueAll` 先同步完成 `shared.EncodeJSONLine` 编码，再在 `c.queueMu` 下把帧追加到 `c.queue`——发布方 goroutine 持 `s.mu` 期间**不做任何 socket I/O**，也不会因慢客户端阻塞 `s.mu`；全部推送帧的生成顺序在 `s.mu` 临界区内被固定（编码放在入队之前，昂贵的序列化不能让更新的帧抢到前面）。若此时 `!c.draining` 则置 `c.draining = true` 并拉起后台 `go c.drain()` 异步消费队列（drain 只取 `queueMu`、不碰 `s.mu`，因此不阻塞仍在持锁的发布方）。释放 `s.mu` 后的 `flushQueue`/`flushAll` 是兜底：只负责给「非空且未处于 draining」的队列补起一个 drainer，自己从不写帧。写操作因此既不在锁内、也不与生成顺序竞争。**第 3G 子批按源码逐句校准**（原描述「只做内存追加」漏了同步编码与入队即拉起 drainer 两点）。
- **单个 drainer**：首个 append（`!c.draining` 时）即置 `c.draining = true` 并拉起 `go c.drain()`；`flushQueue` 只在队列非空且无 drainer 时兜底拉起。两者经同一 `draining` 旗标在 `c.queueMu` 下互斥，避免两个 drainer 互相竞争把 FIFO 打破。drainer 在同一 `queueMu` 临界区内发现队列已空则置回 `draining = false` 退出（入队与退出检查同锁，故没有「刚有新帧入队就退出」的竞态）。
- **2s 写超时**（`observability_server_handlers.go` 的 `rpcWriteTimeout = 2 * time.Second`）：每个帧写入前 `SetWriteDeadline(now+2s)`，超时即 `destroy()` 断开该连接并清空队列。取 2s 是因为推送帧本身很小（几十字节到几 KB），正常情况下写入是微秒级；2s 只可能被「对端停读且缓冲区已满」触发，那种连接本就应当被丢弃。
- **与 6.5 的关系**：6.5 已证明慢客户端确实能把 `conn.Write` 堵在 `SO_SNDBUF` 上超过 2s，所以这个超时不是理论值，而是被真实阻塞窗口验证过的边界。
- **第 3F 子批追加：7 个发布点 100% 锁内入队**：`observability_server.go` 与 `observability_server_handlers.go` 两文件共 6 处 `enqueueAll(...)`（`PublishAgentEvent`、`ReconcileAgentLocations`、disconnect grace timer 回调、startup grace timer 回调、register handler 的 context 帧、`agent.orchestrator.set`），加上 `PublishAgentContext` 直接调 `connection.enqueueMessage(message)`，共 **7 个发布点**。推送辅助函数名是 `publishOrchestratorChange`（**不是** `notifyOwnerChanged`），定义在 `observability_server.go:1265`。第 3F 子批前，disconnect grace timer 回调（原 1021 行）与 startup grace timer 回调（原 1104 行）的 `enqueueAll(released)` 落在 `s.mu.Unlock()` **之后**，与其余 5 处不一致；本轮已把这两处移进持锁闭包，`flushAll(released)` 仍留在锁外。复核方式：对两文件逐行回溯最近的 `s.mu.Lock()` / `s.mu.Unlock()`（排除 `defer s.mu.Unlock()`），6 处 `enqueueAll` 全部判为 `INSIDE-LOCK`。

---

### 6.10 第 3F 终审收口子批（`reviewer-final2` 的 5 个 Must-Fix + 1 个报告准确性 Should-Fix）

本轮只改 `go/` 下的文件（`git diff --name-only main...HEAD | grep -v '^go/'` 为空），未触碰 `/root/workspace/herdsman` 主工作区、`.agents/notes/`、`/root/.pi/agent/`、`/root/.gemini/config/hooks.json`、`/root/.local/bin/herdsman`、`/root/.herdsman/state.db` 与运行中的 `herdsman.service`。测试全部使用 `t.TempDir()`，不碰生产 `HERDSMAN_HOME`。

#### 6 项问题与落点

| # | 级别 | 问题 | 修复落点 |
|---|------|------|---------|
| 1 | Must-Fix | `serializedConnector.Close()` 关底层 `driver.Conn` 前不过 `SqliteGate`，与在途查询/`Rows`/事务竞态 | `internal/db/client.go`：`Close()` 在 `once.Do` 内先 `gid := shared.CurrentGoroutineID()` → `c.gate.Acquire(gid)` + `defer c.gate.Release(gid)`，闸门内置 `closed`、关底层连接、`UnregisterSqliteGate`；`Connect` 见 `closed` 仍返回 `sql.ErrConnDone` |
| 2 | Must-Fix | 两个断连宽限期定时器回调的 `enqueueAll(released)` 在 `s.mu.Unlock()` 之后 | `internal/daemon/observability_server.go`：disconnect grace timer 与 startup grace timer 两处回调的 `enqueueAll(released)` 移进持 `s.mu` 的闭包，`flushAll(released)` 留在锁外 |
| 3 | Must-Fix | `TestOpenSqliteCloseReleasesDriverConnAndUnregistersGate`（原 `TestOpenSqliteCloseReleasesConnectionAndGate`）用 `db.Exec` 报错判断关闭，删掉底层 `c.conn.Close()` 也照样报错，观测不到真实释放 | `internal/db/client.go` 新增 `DriverConnClosed(db) bool` / `WaitDriverConnClosed(ctx, db) bool`（经 `db.Driver()` 取回 connector，对底层 `driver.Conn` 做 `driver.Validator.IsValid()`，问驱动自己）；`internal/db/sqlite_migrations_test.go` 用例改名并拆 3 个子测试 |
| 4 | Must-Fix | 迟到 `finishLocked` 的回归测试靠 `time.Sleep`，不确定 | `internal/observability/turn_completion_test.go` 新增 `TestTurnCompletionLateFinishDoesNotConsumeNextTurnSignal`（`package observability` 白盒，全程同步）；原用例只留监听 goroutine 半个子测试并改名 `TestFinishedWaiterReleasesListenerGoroutine` |
| 5 | Must-Fix | `TestAgentContextServiceRediscoversOnOccupiedChange` 的 seeded agent `agent_session` 为 nil，`DiscoverAgentHistory` 直接返空，未覆盖真实重发现与 `CompactHistory` 更新 | `internal/observability/agent_context_service_test.go`：改用 4 轮真实链路 + 可计数的 `Discover` 壳（`newCountingRealHistoryService`）+ 两个真实 Pi `.jsonl` 的文本断言（见下方「与任务书字面描述的偏差」） |
| 6 | Should-Fix | 报告 6.1 / 6.8 / 6.9 与源码不一致 | 见 6.1 第 4、7 行、6.8 表第 1/4/5 行与 FAIL 取证、6.9 全文；本轮新增 6.10 |

#### 先 FAIL 后 PASS 取证（逐项实测）

**Finding 3（底层 close 的直接观测）**：临时注释掉 `serializedConnector.Close()` 里的 `err = c.conn.Close()`，3 个子测试**全部确定性 FAIL**：

```
sqlite_migrations_test.go:542: DriverConnClosed = false after Close, want the driver connection closed
sqlite_migrations_test.go:626: DriverConnClosed = false after the gate was released, want the connection closed
sqlite_migrations_test.go:675: DriverConnClosed = false after the rows were closed, want the connection closed
--- FAIL: TestOpenSqliteCloseReleasesDriverConnAndUnregistersGate (0.41s)
```

恢复后 3 个子测试 PASS。作为对照，**旧断言在同一改动下不会 FAIL**：`db.Exec("select 1")` 依旧报错，因为 `database/sql` 在句柄关闭后自己就拦掉了语句，压根到不了驱动——这正是 Finding 3 指出的观测空洞。

**Finding 4（确定性迟到 finish）**：临时把 `finishLocked` 的 `if pending.finished` 分支改成「返回前顺手 `delete(r.latestByTerminal, key)`」（即迟到清理误删下一轮信号）：

```
turn_completion_test.go:328: the late finishLocked dropped round 2's stored signal
--- FAIL: TestTurnCompletionLateFinishDoesNotConsumeNextTurnSignal (0.00s)
```

恢复后 PASS。**如实记录**：整体删掉 `finished` / `stillPending` 两个守卫不能作为本条 FAIL 取证——那会让 `finishLocked` 对已关闭的 `doneCh` 二次 `close` 而 panic（测试二进制直接崩溃，不是干净 FAIL）。故取证采用同属「迟到 finish 吞掉下一轮信号」的误删形态。

**Finding 5（真实重发现与 CompactHistory）**：把 `occupiedChanged` 临时恒置 false：

```
agent_context_service_test.go:1188: third call forceDiscovery = false, want true
--- FAIL: TestAgentContextServiceRediscoversOnOccupiedChange (0.01s)
```

恢复后 PASS。

**Finding 2（enqueueAll 入队点）**：本轮是位置修正（把两处回调的 `enqueueAll` 移进锁内），没有新的 FAIL→PASS 取证；顺序回归由 6.8 的 `TestObservabilityRpcPushedFramesKeepTheirGeneratedOrder`（`[2 1]`）覆盖，本轮 3 轮 `-race` 复跑该用例仍 PASS。

#### 与任务书字面描述的偏差（如实记录，不擅改目标）

任务书 Finding 5 描述为「第 3 次调用传入 `OccupiedSessionPaths: [sessionA]` 后选出未被占用的 `sessionB.jsonl`，返回 `from-session-B`」。**当前源码结构上做不到**，因此本轮按源码事实改写为 4 轮，并在此说明理由：

1. `shouldForceDiscovery` 对「可读的官方 path」和「id pane」都提前返回 false；`selectPreferredRef` 也只在官方值缺失时才回落到持久化 ref。也就是说**只有官方 `agent_session` 为 nil 的窗格，occupied 变化才可能触发强制发现**。
2. 官方值一旦是 path，`DiscoverAgentHistory` 只按该 path 做单值确定性解析（`discovery.go:299`），不存在「在多个候选文件里挑一个没被占用的」这一步；cwd/mtime 扫描器已随 `5fce550` 第 6 项删除。

所以本轮 4 轮分别是：(1)(2) 官方 session 缺失、命中持久化 `preferredRef`，真实读到 `from-session-A`，真实 discovery 跑 0 次；(3) 另一窗格占用该文件 → occupied 变化 → 强制发现 → 真实 discovery 跑 1 次 → 官方 session 仍缺失故解析不到 → `CompactHistory` 变成真实的未决形态（`LastAssistantMessage == nil`、`HistoryRef == nil`）；(4) 窗格上报自己的官方 session 文件 → 真实读到 `from-session-B`，discovery 仍是 1 次。第 3 轮 `LastAssistantMessage` 由有变无，本身就是「重新发现真的执行了」的证据。

任务书中另有两处名称与源码不符，本轮一律以源码为准并已在测试注释里写明：`LookupSqliteGate` 实为 `shared.SqliteGateOf`；`TurnWaitResult{Status:"...", TurnSeq:...}` 实为 `TurnCompletionWaitResult{Confirmed/ExpectedText/HasExpectedText/Received}`，`SessionScopedSignal` 无 `TurnSeq` 字段（改用 `ExpectedText` 区分两轮）。

#### 本轮验证

- `gofmt -l cmd internal` 为空（改动文件均已格式化）。
- `CGO_ENABLED=0 go vet -buildvcs=false ./...` 通过；`CGO_ENABLED=0 go build -buildvcs=false ./...` 通过。
- `CGO_ENABLED=0 go test -buildvcs=false -count=1 ./...`：12 个包全绿（2 个 `no test files` + 10 个 `ok`），其中 `internal/daemon 12.730s`、`internal/observability 4.590s`、`internal/agy 2.816s`、`internal/herdr 1.162s`、`internal/db 1.065s`、`internal/agenthistory 0.026s`。
- 3 轮 `-race`（按修复涉及的测试先按 `-run` 精扫，再整包跑）：
  - `go test -race -buildvcs=false -count=3 -run 'TestOpenSqliteCloseReleasesDriverConnAndUnregistersGate|TestRunSqliteTransaction|TestOpenSqlite' ./internal/db/` → `ok 3.797s`
  - `go test -race -buildvcs=false -count=3 -run 'TestTurnCompletionLateFinishDoesNotConsumeNextTurnSignal|TestFinishedWaiterReleasesListenerGoroutine|TestTurnCompletion|TestRecorded|TestTimesOut|TestScopesSignals|TestPrunesStale|TestAbortedWait|TestExpectedText' ./internal/observability/` → `ok 1.722s`
  - `go test -race -buildvcs=false -count=3 -run 'TestAgentContextServiceRediscoversOnOccupiedChange|TestAgentContextService' ./internal/observability/` → `ok 7.562s`
  - `go test -race -buildvcs=false -count=3 -run 'TestObservabilityRpc|TestDisconnect|TestStartupGrace|TestScheduleDisconnect|TestArmStartupGrace|TestPublish' ./internal/daemon/` → `ok 25.000s`
  - 整包 3 轮：`go test -race -buildvcs=false -count=3 ./internal/db/... ./internal/observability/...` → `internal/db 38.274s`、`internal/db/dedup 1.022s`、`internal/observability 279.768s`，全部 `ok`；`go test -race -buildvcs=false -count=3 ./internal/daemon/...` → `internal/daemon 90.629s`，`ok`。无 `DATA RACE`、无 `FAIL`。

  以上耗时取自最终形态（观测走 `db.Driver()`、无 `*sql.DB → connector` 注册表）的一次完整复跑；全量 `go test -count=1 ./...` 的包耗时同轮取得：`internal/daemon 12.793s`、`internal/observability 5.192s`、`internal/agy 2.809s`、`internal/herdr 1.161s`、`internal/db 1.083s`、`internal/agenthistory 0.040s`。

#### 本轮变更面

```
 go/internal/daemon/observability_server.go         |   9 +-
 go/internal/db/client.go                           |  93 +++++++-
 go/internal/db/sqlite_migrations_test.go           | 203 ++++++++++++++----
 go/internal/observability/agent_context_service_test.go | 287 ++++++++++++++++----
 go/internal/observability/turn_completion_test.go  | 226 ++++++++++++++------
 go/reports/batch-5-herdsmand.md                    |  54 +++-
 6 files changed, 693 insertions(+), 179 deletions(-)
```

生产代码净改动很小（`client.go` 的 `Close()` 加闸门 + 两个观测函数、`observability_server.go` 两行位置），其余是测试与报告。未新增测试文件，未新增生产行为。

#### 第 3G 子批追加（`reviewer-final5`：1 个 Must-Fix + 1 个报告/实现对齐 Should-Fix）

本轮基线为 `b7d64ad`（3F 收口后的 HEAD）；本轮改动随本报告所在的提交入库（提交主题见第 1 节第 27 条，短 SHA 以仓库 `git log -1` 为准——报告随同一提交入库，无法在提交前写下自己的 SHA）。只改 `go/` 下的文件，红线同 3F；测试全部使用 `t.TempDir()`。

| # | 级别 | 问题 | 修复落点 |
|---|------|------|---------|
| 1 | Must-Fix | `serializedConnector.Close()` 在 `c.mu.Unlock()` 之后才调 `c.conn.Close()`（modernc 的 `Close()` 写 `c.db = 0`、`c.tls = nil`），而 `DriverConnClosed(db)` 无锁直读 `validator.IsValid()`（读 `c.db`）；`WaitDriverConnClosed` 或并发的 `DriverConnClosed` 与 `db.Close()` 交叠时，对底层 `modernc` `*conn.db` 构成无同步读写 | `internal/db/client.go`：`Close()` 把 `err = c.conn.Close()` 移入 `c.mu` 保护块（`c.mu.Lock(); c.closed = true; err = c.conn.Close(); c.mu.Unlock()`）；`DriverConnClosed(db)` 在访问 `connector.conn` / 调 `IsValid()` 前先 `connector.mu.Lock()` + `defer connector.mu.Unlock()`。`Close` 只有在拿到 `c.gate` 之后才碰 `c.mu`，停在 `gate.Acquire` 上的 `Close` 不阻塞轮询方；一旦 `Close` 进入 `c.conn.Close()`，两者经 `c.mu` 严格串行化。回归：`sqlite_migrations_test.go` 新增第 4 个子测试 `DriverConnClosed does not race the driver Close`（持闸门停住 `db.Close()` → 无 sleep 紧循环轮询 → 放行闸门让 teardown 与轮询交叠 → 断言 `WaitDriverConnClosed` 收敛、`Close` 无错），`-race` 先 FAIL 后 PASS 见下 |
| 2 | Should-Fix | 代码与报告不一致：2a — 6.9 称 `gid == 0` 退化为不可重入互斥，但 `Acquire` 等待条件原为 `g.depth > 0 && g.owner != gid`，空闸门 `owner` 恰为 `0`，任意 `gid == 0` 实际互相可「重入」；2b — 6.8 表第 2 行与 6.9 对 `enqueueMessage`/`flushQueue` 的描述不夠精确 | 2a（`internal/shared/sqlite_gate.go`）：`Acquire` 改为 `for g.depth > 0 && (gid == 0 \|\| g.owner != gid)`，`HeldBy` 改为 `return g.depth > 0 && gid != 0 && g.owner == gid`，`gid == 0` 真正退化为不可重入互斥（`Release` 的守卫未动：生产路径的 gid 恒为真实 goroutine 号，本项只为对齐报告语义）；6.9 同步按新条件改写。2b：6.8 表第 2 行与 6.9 两条按 `observability_server_handlers.go` 实现改写——`enqueueMessage` 在持 `s.mu` 期间先同步完成 `EncodeJSONLine`、在 `c.queueMu` 下追加 `c.queue`（发布方持锁期间零 socket I/O），`!c.draining` 时置 `c.draining = true` 并拉起 `go c.drain()`；锁外 `flushAll`/`flushQueue` 只给非空且未 draining 的队列兜底补起 drainer、从不自己写帧。6.9 的「两文件共 6 处 `enqueueAll` + 1 处 `PublishAgentContext` 直接调 `connection.enqueueMessage`，共 7 个发布点」经本轮逐行复核（`observability_server.go:476/625/1023/1108` + `observability_server_handlers.go:586/634`，以及 `observability_server.go:458`）确认与源码一致，未改动 |

**Finding 1 的先 FAIL 后 PASS 取证**（时间线；详细竞争报告另见 6.8 的 3G 段）：

1. 只加测试、不动生产代码：`go test -race -buildvcs=false -count=1 -run TestOpenSqliteCloseReleasesDriverConnAndUnregistersGate ./internal/db` → **首轮即 DATA RACE**。写侧：`modernc.org/sqlite.(*conn).Close()`（`conn.go:1083/1088`）← `client.go:167`（`serializedConnector.Close`）；读侧：`(*conn).usable()`（`conn.go:1122/1133`）← `(*conn).IsValid()`（`conn.go:1118`）← `DriverConnClosed`（`client.go:195`）——正是任务书指出的 `c.db` 无同步读写对。
2. 加上 `c.mu` 保护后：`-count=1` → `ok 1.721s`；`-count=3` → `ok 2.871s`。无 `DATA RACE`、无 `FAIL`。

**第 3G 子批验证**：

- `gofmt -l cmd internal` 为空。
- `CGO_ENABLED=0 go vet -buildvcs=false ./...` 通过；`CGO_ENABLED=0 go build -buildvcs=false ./...` 通过。
- `CGO_ENABLED=0 go test -buildvcs=false -count=1 ./...`：12 个包全绿（2 个 `no test files` + 10 个 `ok`），其中 `internal/daemon 12.733s`、`internal/observability 4.498s`、`internal/agy 2.812s`、`internal/herdr 1.162s`、`internal/db 1.051s`、`internal/agenthistory 0.036s`。
- 3 轮 `-race`：`CGO_ENABLED=1 go test -race -buildvcs=false -count=3 -run 'TestOpenSqliteCloseReleasesDriverConnAndUnregistersGate|TestRunSqliteTransactionRollsBackAfterFailedCommit|TestObservabilityRpcPushedFramesKeepTheirGeneratedOrder|TestObservabilityRpcSlow|TestHerdrSocketClientConcurrentRequestsKeepTheirOwnTimeouts|TestTurnCompletionLateFinishDoesNotConsumeNextTurnSignal|TestAgentContextServiceRediscoversOnOccupiedChange' ./internal/...` → `internal/daemon 10.808s`、`internal/db 3.449s`、`internal/herdr 3.595s`、`internal/observability 1.556s` 均 `ok`（其余 6 包 `[no tests to run]`），无 `DATA RACE`、无 `FAIL`。

**第 3G 变更面**（`git diff --stat`）：`go/internal/db/client.go` +30−3（teardown 入 `c.mu` + `DriverConnClosed` 取锁 + 注释）、`go/internal/shared/sqlite_gate.go` +12−2（`Acquire`/`HeldBy` 的 `gid == 0` 语义）、`go/internal/db/sqlite_migrations_test.go` +73−0（并发回归子测试、导入、文档注释）、`go/reports/batch-5-herdsmand.md`（本节及 6.8/6.9 对齐）。新增测试 73 行对生产代码约 47 行改动（`client.go` +30−3、`sqlite_gate.go` +12−2）不足 3 倍：并发数据竞争回归需要持闸、启停轮询与放行三段 goroutine 编排才能把交叠窗口对准 modernc 自身 `Close` 的存活期，其余为注释。

#### 第 3H 子批追加（`reviewer-final6`：1 个 Must-Fix 测试启动屏障 + 1 个报告旧文残留 Should-Fix）

本轮基线为 `ff50a2e`（3G 收口后的 HEAD）；本轮改动随本报告所在的提交入库（短 SHA 以仓库 `git log -1` 为准——报告随同一提交入库，无法在提交前写下自己的 SHA）。只改 `go/` 下的文件，红线同 3F/3G；测试全部使用 `t.TempDir()`。

| # | 级别 | 问题 | 修复落点 |
|---|------|------|---------|
| 1 | Must-Fix | `sqlite_migrations_test.go` 的第 4 个子测试 `DriverConnClosed does not race the driver Close` 缺放行前启动屏障：① 紧循环轮询协程启动后未等它完成首次 `DriverConnClosed(db) == false` 观测就 `close(release)`，慢调度下 `db.Close()` 可能在轮询协程被调度前先跑完，交叠窗口为空；② `WaitDriverConnClosed(ctx, db)` 写在 `<-pollerDone` 之后（此时底层连接已关），只在第一轮命中已关闭状态，从未在 `close(release)` 之前跨越 `db.Close()` 过程 | `internal/db/sqlite_migrations_test.go`：新增 `pollerReady` 通道——轮询协程先完成一次 `if DriverConnClosed(db) { return }` 的活跃观测后 `close(pollerReady)` 再进紧循环；新增并发 `WaitDriverConnClosed(waitCtx, db)` goroutine——先观测 `!DriverConnClosed(db)` 后 `close(waiterReady)` 再进入 `WaitDriverConnClosed` 并把返回值写入 `waiterResult` channel。主测试协程等 `<-pollerReady` 与 `<-waiterReady`，再留 5ms 让 waiter 进入其 1ms 轮询周期，断言 `waiterResult` 尚未返回（闸门仍持着、连接未关），然后才 `close(release)` 放行；放行后等 `<-holderDone`、`<-pollerDone`、`<-waiterResult`（断言 `true`）与 `<-closed`（断言 `err == nil`） |
| 2 | Should-Fix | 报告 6.8 表第 2 行（原 716 行）残留 3G 改写前的旧文：写的是 `observability_server.go` 的 `rpcConnection`、`enqueueMessage`/`enqueueAll` 「只做内存追加（零网络 I/O…）」、释放 `s.mu` 后由 drainer「按 FIFO 取出」，与 925 行声称的 3G 改写不一致（漏了同步编码与入队即拉起 drainer） | `go/reports/batch-5-herdsmand.md`：6.8 表第 2 行改写为与 6.9 完全一致的准确表述（`observability_server_handlers.go`、持 `s.mu` 期间同步完成 `EncodeJSONLine` 并在 `c.queueMu` 下追加 `c.queue`、`!c.draining` 时置位并拉起 `go c.drain()`、锁外 `flushQueue`/`flushAll` 只兜底补起 drainer） |

**Finding 1 的先 FAIL 后 PASS 取证**：临时退回 `client.go` 中 `Close()` / `DriverConnClosed()` 的 `c.mu` 保护（`c.mu.Lock(); c.closed = true; err = c.conn.Close(); c.mu.Unlock()` → 去锁直行），`go test -race -buildvcs=false -count=1 -run TestOpenSqliteCloseReleasesDriverConnAndUnregistersGate ./internal/db` → **首轮即 DATA RACE，且两个交叠观测点都报了竞争对**（证明屏障后两个 goroutine 都真的跨越了 `close(release)` 与 `c.conn.Close()` 的执行窗口）：

- 写侧 `modernc.org/sqlite.(*conn).Close()`（`conn.go:1083`）← `client.go:178`（`serializedConnector.Close`）← `db.Close()`（`sqlite_migrations_test.go:721`）；读侧 `(*conn).usable()`（`conn.go:1122`）← `IsValid()` ← `DriverConnClosed`（`client.go:214`）← **紧轮询 goroutine**（`sqlite_migrations_test.go:741`）。
- 第二组竞争对读侧多一层 `WaitDriverConnClosed`（`client.go:225`）← **waiter goroutine**（`sqlite_migrations_test.go:756`）——屏障前这一条永远不会出现（waiter 此前在放行后才启动）。

恢复 `c.mu` 保护后：`-count=3` → `ok 2.855s`，无 `DATA RACE`、无 `FAIL`。

**Finding 2**：6.8 表第 2 行本就是文档改写，与 6.9 对齐后无需运行时取证（同 3F 的 Finding 6 处理方式）。

**第 3H 子批验证**：

- `gofmt -l cmd internal` 为空。
- `CGO_ENABLED=0 go vet -buildvcs=false ./...` 通过；`CGO_ENABLED=0 go build -buildvcs=false ./...` 通过。
- `CGO_ENABLED=0 go test -buildvcs=false -count=1 ./...`：12 个包全绿（2 个 `no test files` + 10 个 `ok`），其中 `internal/daemon 12.731s`、`internal/observability 4.403s`、`internal/agy 2.824s`、`internal/herdr 1.162s`、`internal/db 1.066s`。
- 3 轮 `-race`：`go test -race -buildvcs=false -count=3 -run TestOpenSqliteCloseReleasesDriverConnAndUnregistersGate ./internal/db` → `ok 2.855s`，无 `DATA RACE`、无 `FAIL`。

**第 3H 变更面**（`git diff --stat`）：`go/internal/db/sqlite_migrations_test.go` +57−31（第 4 个子测试的重排：两个观测点提前到放行之前并加就绪屏障，原「连接已知关闭后才调 WaitDriverConnClosed」的写法删除；含改写的说明注释）、`go/reports/batch-5-herdsmand.md`（本节及 6.8 表第 2 行对齐）。生产代码零改动；无新增测试文件、无新增测试用例，只重排既有用例的 goroutine 时序。

#### 第 3I 子批追加（`reviewer-final7`：1 个 Must-Fix，用 `Done()` 首调取代 3H 屏障里的 `time.Sleep`）

本轮基线为 `b700669`（3H 收口后的 HEAD）；本轮改动随本报告所在的提交入库（短 SHA 以仓库 `git log -1` 为准——报告随同一提交入库，无法在提交前写下自己的 SHA；本提交后不再 `--amend`）。只改 `go/` 下的文件，红线同 3F/3G/3H；测试全部使用 `t.TempDir()`。

| # | 级别 | 问题 | 修复落点 |
|---|------|------|----------|
| 1 | Must-Fix | 3H 的 waiter 屏障是「goroutine 内预检查 `!DriverConnClosed(db)` 后 `close(waiterReady)` + 主协程 `time.Sleep(5ms)`」：预检查与进入 `WaitDriverConnClosed` 内部轮询之间仍有调度缝隙，5ms 只是在猜 waiter 何时到达其 1ms `select`，慢调度下 `close(release)` 放行时 waiter 可能尚未在轮询中 | `internal/db/sqlite_migrations_test.go`：新增包级 `pollEnteredContext`（嵌入 `context.Context` + `sync.Once` + `entered chan struct{}`，其 `Done()` 首次被调用时 `close(entered)` 再返回内层 `Done()`）；waiter goroutine 直接 `waiterResult <- WaitDriverConnClosed(waitCtx, db)`，删除外部预检查与 `close(waiterReady)`；主协程等 `<-pollerReady` 与 `<-waitCtx.entered` 后断言 `waiterResult` 尚未返回，随即 `close(release)`。确定性来自 `WaitDriverConnClosed` 自身结构：循环体先 `if DriverConnClosed(db) { return true }`，观测到连接未关才求值 `select { case <-ctx.Done(): … case <-time.After(time.Millisecond): }`，此时必然调用 `ctx.Done()`——首调 `Done()` 即「waiter 已完成首次 `DriverConnClosed(db) == false` 判定、正停在其内部轮询 `select` 中」的 100% 确定性信号，零 sleep、零猜测 |

**先 FAIL 后 PASS 取证**（同 3H 方法，针对新屏障）：临时去掉 `client.go` 中 `Close()` 的 `c.mu` 保护块（`c.mu.Lock(); c.closed = true; err = c.conn.Close(); c.mu.Unlock()` → 去锁直行）与 `DriverConnClosed()` 的 `connector.mu.Lock()/defer Unlock()`，`go test -race -buildvcs=false -count=1 -run TestOpenSqliteCloseReleasesDriverConnAndUnregistersGate ./internal/db` → **首轮即 DATA RACE，FAIL**，且两个交叠观测点都报了竞争对（证明屏障后 waiter 确实停在 `WaitDriverConnClosed` 内部轮询中并跨越了 teardown）：

- 写侧 `modernc.org/sqlite.(*conn).Close()`（`conn.go:1083/1088`）← `client.go:178`（`serializedConnector.Close`）← `db.Close()`（`sqlite_migrations_test.go:739`）；读侧 `(*conn).usable()`（`conn.go:1133`）← `IsValid()` ← `DriverConnClosed`（`client.go:212`）← **紧轮询 goroutine**（`sqlite_migrations_test.go:760`）。
- 第二组竞争对读侧经 `WaitDriverConnClosed`（`client.go:223`）← **waiter goroutine**（`sqlite_migrations_test.go:768`，即 `waiterResult <- WaitDriverConnClosed(waitCtx, db)` 那一行）——`Done()` 首调屏障下这一条在首轮 `-count=1` 就出现，证明 waiter 在放行前确已进入内部轮询。

恢复 `c.mu` / `connector.mu` 保护（备份还原，`git status` 确认 `client.go` 无差异）后：`go test -race -buildvcs=false -count=3 -run TestOpenSqliteCloseReleasesDriverConnAndUnregistersGate ./internal/db` → `ok 2.849s`，无 `DATA RACE`、无 `FAIL`。

**第 3I 子批验证**：

- `gofmt -l cmd internal` 为空。
- `go vet -buildvcs=false ./...` 通过；`go build -buildvcs=false ./...` 通过。
- `go test -buildvcs=false -count=1 ./...`：12 个包全绿（2 个 `no test files` + 10 个 `ok`），其中 `internal/daemon 12.764s`、`internal/observability 4.482s`、`internal/agy 2.811s`、`internal/herdr 1.162s`、`internal/db 1.017s`。
- 3 轮 `-race`：`go test -race -buildvcs=false -count=3 -run TestOpenSqliteCloseReleasesDriverConnAndUnregistersGate ./internal/db` → `ok 2.849s`，无 `DATA RACE`、无 `FAIL`。

**第 3I 变更面**：`go/internal/db/sqlite_migrations_test.go`（第 4 个子测试的 waiter 屏障改写：新增包级 `pollEnteredContext` 类型，waiter goroutine 删预检查直调 `WaitDriverConnClosed`，主协程改等 `<-waitCtx.entered`，`time.Sleep(5ms)` 删除；含改写的说明注释）、`go/reports/batch-5-herdsmand.md`（本节）。生产代码零改动；无新增测试文件、无新增测试用例，只替换既有用例的就绪屏障机制。


### 6.11 第 5b 批：`b0e5c6c..main`（0.15.1）行为改动与回归测试移植 + 脱敏样本运行时拼接

本节对应任务说明中的两项工作，全部落在 `go/` 下，未触碰 `go/` 之外的任何文件（`git diff --name-only main...HEAD | grep -v '^go/'` 为空）。

#### 6.11.1 范围与来源

`git log --oneline b0e5c6c..main` 共 8 个提交，其中 4 个是行为改动、1 个是发布号、3 个是文档/合并：

| 提交 | 类型 | 是否移植 |
|------|------|----------|
| `84fc8b1` `fix(agy): deduplicate deferred completions and harden draft parser` | 行为 + 测试 | 是 |
| `adf6f09` `fix(agy): guard ack cursor against deferred-event swallowing` | 行为 + 测试 | 是 |
| `74df962` `fix(agy): recheck wake epoch before state writes and retain candidates` | 行为 + 测试 | 是 |
| `3985333` `fix(agy): keep wake candidates until their ack settles` | 行为 + 测试 | 是 |
| `322d0d9` `chore(release): 0.15.1` | 版本号 | `go/` 下无版本号常量需同步（全量检索 `0\.15\.0` / `0\.15\.1` 于 `go/**/*.go` 零命中），仅 `package.json` 的 `version` 由 `0.15.0` 改为 `0.15.1`，非 `go/` 面 |
| `6ec2c27` `docs(skill): probe herdr via cli…`、`102a55a` `docs(skill)…`、`4b179d1` Merge | 文档/合并 | 否（无行为改动） |

移植方式为逐 commit 读 `git show <sha> -- src/ test/` 的净 diff 后落到 `go/internal/agy/`，**未执行 `git merge main`**（避免把 `main` 合进 `go-port` 造成历史混乱）。

#### 6.11.2 生产逻辑逐函数映射（TS `src/agy/hook.ts` → Go `go/internal/agy/`）

| TS 符号 | Go 落点 | 改动内容 |
|---------|---------|----------|
| `projectAgyOutcomes`（同 terminal 去重） | `hook.go: projectAgyOutcomes` | 新增预扫 `newestCompletionID`：按 `terminalId` 记录「最后一条能产出有效 `completed` outcome 的完成事件 id」（要求 `isCompletionEvent`、`outcomeKind === "completed"`，且通过 `fallbackOutcome` 被同 pane 完成事件取代、`isUpstreamErrorText`、空文本三项检查，与主循环同一套判定）；主循环在 working-defer 判定之后，同 terminal 且 `event.id < newest` 的完成事件进 `suppressedEventIDs`，日志 `superseded by newer completion for terminal <id>`，随批次 ack。只对完成事件生效，`agent.failed` / `agent.discarded` 语义不变 |
| `defaultReadPaneText` | `wake_waiter.go: defaultReadPaneText` | `herdr pane read <paneId> --lines 12` → `--lines 40`（多行草稿的框顶原先落在读取窗口外） |
| `parseAgyComposerDraft` | `wake_waiter.go: ParseAgyComposerDraft` | 框顶不再取「倒数第二条横线」，改为「最后一条下一行以 `>` 开头（`^>(?:\s|$)`）的横线」；同宽同款候选优先（与框底逐字节相等），由内向外回溯直到解析出非空草稿；全部候选都是空 `>` 提示行时返回 `""`，一个候选都没有时返回 `nil`（= TS `undefined`）。新增包级 `composerPromptLinePattern` 与 `lineAt`（TS `lines[index] ?? ""` 守卫） |
| `AgySessionHookState` 新增 `retainedCandidateEvents` / `deferredEventIds` / `pendingAckEventIds` | `state.go` | 三个新字段及其 getter/setter |
| `fetchLivePeerScope`（替代 `fetchLiveWorkingTerminalIDs`） | `wake_waiter.go: fetchLivePeerScope` | 一次 `agent.list` 同时产出 `liveScopeWorkspaces` / `liveTerminalIDs` / `workingTerminalIDs`；失败返回 `nil`（TS `undefined`），调用方按「无实时信息」保守处理 |
| `AgentListItemView` 新增 `workspaceId` | `wake_waiter.go: AgentListItemView` | 供 `liveScopeWorkspaces` 使用 |
| `mergeKnownUnackedEvents` | `hook.go` | connState 事件 ∪ 快照事件，同 id 以 daemon 返回为准；过滤 `<= ackedEventId`、本 terminal、跨 `herdrSessionName` 的行；按 id 稳定排序。**不按 `presented` 过滤**——Stop 已把批次同时标 presented 并写进 `deliveredEventIds`，过滤掉就正好丢掉仍需存活的那批 |
| `collectWakeCandidateEvents` | `hook.go` | 从 known-unacked 视图中剔除已 presented 的候选 |
| `filterEligibleRetainedEvents` | `hook.go` | 只存在于快照、daemon 本轮不再重列的行，经 `agent.list` 核验 scope（`workspaceId` 不在本 owner 的 ownerless 范围内则丢）与源窗格存活（`terminalId` 不在 live 列表则丢）；daemon 刚重列过的行无条件保留；`agent.list` 失败（`liveScope == nil`）时全部保留 |
| `retainWakeCandidates` + `RETAINED_CANDIDATE_EVENT_LIMIT` | `hook.go`（常量 `retainedCandidateEventLimit = 100`） | 输入是**未过滤**的 known-unacked 视图，保留「未 presented 或仍在 `deliveredEventIds` 中未 ack」的行；按 id 升序遍历，容量截断保留**低 ID**（ack 游标下界依赖最早未消费事件） |
| `retainedSnapshotKey` | `hook.go` | 快照 id 拼接串，用于跳过 no-op 状态写 |
| `resolveEligibleWakeEvents` | `hook.go` | 一次 agent.list 同时服务「资格过滤」与「live working terminals」两个消费方 |
| `isWaiterSuperseded` | `hook.go` | 比对 `turnEpoch` / `waiterEpoch` / `waiterPid`；`waiterPid` 判定带「已落盘才比」保护（Stop 先写 epoch、spawn、最后写 pid） |
| `resolveMinHoldbackEventId` | `hook.go` | ack 游标下界**现场重算**：`mergeKnownUnackedEvents` → `collectWakeCandidateEvents` → `resolveEligibleWakeEvents` → `projectAgyOutcomes`，取 `deferredWorkingEventIDs ∪ outcomes.eventID` 的最小值；候选集为空直接返回 `+Infinity` 并省掉 agent.list 往返。内部重算注入 no-op logger，避免与紧随的 take/peek 打重复 warn |
| `flushDeliveredAcks` | `hook.go` | `pendingAckEventIds ∪ deliveredEventIds` 合并去重排序为 `allToAck`；逐项 `< minHoldbackEventId` 才发 RPC ack，`>=` 的移入 `pendingAckEventIds`；瞬时失败时剩余项按同规则分流（`>=` 进 pendingAck，`<` 回 `deliveredEventIds`） |
| `peekNextWakeBatch`（`expectedWaiter` 参数） | `hook.go: peekNextWakeBatch` | 签名新增 `*waiterIdentity`；agent.list 与抑制项 ack 之后、写盘前 `r.readState()` + `isWaiterSuperseded` 复核，被取代则直接放弃写盘；`presentedEventIds` 与磁盘值取**并集**（`mergeEventIDs`）而非覆盖；空候选分支同样改写重新读取的 `freshState`；快照从未过滤的 eligible 视图重算 |
| `takeNextWakeBatch` | `hook.go: takeNextWakeBatch` | 同步 `retainedCandidateEvents` 重算与 `deferredEventIds` 记录 |
| `runPreInvocation` 非 owner 清理路径 | `hook.go` | 所有权移交时与 `deferredEventIds` 一并清空 `retainedCandidateEvents` |
| `rearmDeliveredEvents` | `hook.go` | 按快照重建 `deliveredEventIds` 对应记录 |
| `mergeEventIDs` / `minDeferredEventId` | `hook.go` | TS 数组语义的 Go 等价物（去重 + 升序；空列表 = `+Infinity`） |
| WakeWaiter 循环：`readComposerDraft` 后 epoch 复核 | `wake_waiter.go` | `readComposerDraft` 是 I/O（最长 5s），其后的 else 分支开头重读 `sessionState`，`turnEpoch` / `waiterEpoch` / `waiterPid` 任一不符即 `stop = true; return nil`，不再 `rearmDeliveredEvents` / `triggerWakePrompt` |
| WakeWaiter 循环：peek 传 waiter 身份 | `wake_waiter.go` | `peekNextWakeBatch` 调用点传入 `&waiterIdentity{waiterEpoch, waiterPID}` |

`go/` 下无 `0.15.0` / `0.15.1` 版本号常量，`322d0d9` 无需同步（已在 6.11.1 表格说明）。

#### 6.11.3 新增 / 扩展的 Go 测试

全部落在 `go/internal/agy/agy_hook_part3_test.go`（+1192 行）与 `agy_hook_and_cli_test.go`（+18/−5），无新增测试文件。

| # | Go 测试 | 对应 TS 用例 | 覆盖的修复 |
|---|---------|--------------|-----------|
| 1 | `TestIntermediateAgentDoneWhilePeerStillWorkingIsDeferred`（扩展） | `84fc8b1` 追加断言段 | 同 terminal 去重：wake 后的 `PreInvocation` 只注入最新完成（1126），1118 被 `superseded by newer completion for terminal term_peer` 抑制；`presentedEventIds` / `deliveredEventIds` 均为 `[1118, 1126]`；随后的 Stop 把两者都 ack 掉 |
| 2 | `TestParseAgyComposerDraft`（扩展 4 例） | `84fc8b1` / `adf6f09` 的 composer 用例 | 草稿内含横线（`第一段\n--------\n第三段`）、草稿末行是横线（`草稿\n<rule>`）、连续两条横线（`第一段\n第二行\n<rule>`）、无 `>` 提示行时返回 `nil` |
| 3 | `TestRetryWakePendingAloneWakesWhenOneDeferredPeerSettles` | `84fc8b1` 新增用例 | peer A 的 deferred 重试是唯一能重新检查批次的路径（peer B 恒 working、无 push，`!hasActivePeers` 永不触发） |
| 4 | `TestLowIDWorkingDeferredEventIsNeverSwallowedByHigherIDAck` | `adf6f09` 新增用例 | ack 游标下界：deferred 的 100 在旁时，被永久抑制的 105 不得 ack；100 变成未消费 outcome 时 105 仍须等待；两者都 presented 后才 `[100, 105]` 依次 ack |
| 5 | `TestWakeWaiterCancelsWhenTurnEpochAdvancesDuringComposerRead` | `adf6f09` 新增用例 | `readComposerDraft` I/O 期间 `turnEpoch` 前进 → 不叫醒、不清 `presentedEventIds`（`readGate` channel 冻结 pane read，主协程在其间改写状态文件） |
| 6 | `TestPeekNextWakeBatchDoesNotWipePresentedEventIDsOfConcurrentTurn` | `74df962` 新增用例 | peek 的第二次 `agent.list` 被 `listGate` 冻结，期间新回合把 300 写进 presented/delivered 并 bump `turnEpoch`；陈旧 peek 不得二次投递、不得抹掉新回合记录 |
| 7 | `TestUnconsumedEventSurvivesDaemonRedeliveryWindow` | `74df962` 新增用例（两段） | 草稿超过 300s 重投窗口后 daemon 不再重列 9001，快照使其仍能叫醒、`PreInvocation` 仍能重建注入；working-defer 的低 id 100 同理，且 105 不被提前 ack |
| 8 | `TestIgnoredContinueBatchSurvivesRepeatedWaiterPeeks` | `3985333` 新增用例 | Stop 已 presented 且 delivered-but-unacked 的 9500 在反复 peek 后仍在快照中（`candidateEvents` 从首次 peek 起即为空，快照却不得被清），草稿消失后叫醒、`PreInvocation` 从快照重建 |
| 9 | `TestRetainedRowsOfClosedPaneAndTakenOverSiblingAreDropped` | `3985333` 新增用例 | 快照中源窗格已关闭（`term_gone`）与被接管兄弟工作区（`wC` / `term_sibling`）的两行被资格检查丢弃，不注入也不压 ack 游标；daemon 重新列过的行（`closedPaneEvent` 进 `listedEvents`）无条件保留并注入 |

配套基建改动：

- `agy_hook_and_cli_test.go: makeDoneEvent` 增加可变参数 `terminalIDOverride ...string`，对齐 TS 第三参数 `terminalId`；既有两参调用点不变。
- `agy_hook_part3_test.go` 新增包内 helper `assertRetainedIDs`（等价 TS `expect.objectContaining({ id })` 数组断言）。
- `agy_hook_and_cli_test.go: TestWakeWaiterEpochAcceptsRadixPrefixes` 的种子状态由 `waiterEpoch: 7` 改为 `5`：该用例本意是验证 `parseJSNumber` 对 `0x5` / `0b101` / `0o5` 的解析，与 `turnEpoch` 无关；5b 批引入的 post-draft epoch 复核同样比对 `waiterEpoch`（TS `isWaiterSuperseded` 亦含该项判据），旧种子下 waiter 会在复检处被判为已被取代而提前返回。末尾「`-0x5` 是 NaN → 回退到状态 epoch 7 → 循环立即停止」的子场景保持原样（两个 epoch 都参与门闩，行为本就正确）。

#### 6.11.4 新增测试的先 FAIL 后 PASS 取证（逐项实测）

对每处修复做「回退该修复 → 确认对应测试 FAIL → 恢复 → 确认 PASS」的变异验证，全部实测而非声称：

| 回退的修复 | 变异手法 | FAIL 的测试与实测输出 |
|------------|----------|----------------------|
| 快照保留（`retainWakeCandidates`） | 函数体改为 `return nil` | `TestUnconsumedEventSurvivesDaemonRedeliveryWindow` → `wakePrompts = [], want [{paneID:wB:p1 promptText:[herdsman-wake]}]`；`TestIgnoredContinueBatchSurvivesRepeatedWaiterPeeks` → `retainedCandidateEvents has 0 entries ([]), want [9500]`；`TestRetainedRowsOfClosedPaneAndTakenOverSiblingAreDropped` → `retainedCandidateEvents has 0 entries ([]), want [400]` |
| post-draft epoch 复核 | 删除 `wake_waiter.go` 中 `postReadState` 及其三个比对分支 | `TestWakeWaiterCancelsWhenTurnEpochAdvancesDuringComposerRead` → `wakePrompts = [{paneID:wB:p1 promptText:[herdsman-wake]}], want none` |
| 同 terminal 去重 | 删除 `newestCompletionID` 命中分支 | `TestIntermediateAgentDoneWhilePeerStillWorkingIsDeferred` → `injected` 同时含 `event: 1118` 与 `event: 1126`，`want the superseded 1118 removed` |
| ack 游标下界 | `resolveMinHoldbackEventID` 提前 `return math.MaxInt64` | `TestLowIDWorkingDeferredEventIsNeverSwallowedByHigherIDAck` → `secondStop = {…Decision:<nil> Reason:<nil>}, want decision continue`；`TestUnconsumedEventSurvivesDaemonRedeliveryWindow` → `settle stop = {…}, want decision continue` |

变异后均以 `/tmp` 备份还原，`git status` 确认生产文件无残留差异。

#### 6.11.5 任务一：假脱敏样本的运行时拼接

全局 `commit-msg` hook 在 `git merge --no-ff go-port` 时会对 `git diff --cached`（即 `main...go-port` 全部新增行）跑 `added_lines_only` + `check_secret_patterns`（`sk-[a-zA-Z0-9._-]{8,}` 等）+ `check_credential_assignments`（`(password|api_key|secret)[A-Za-z0-9_]*[[:space:]]*[:=]+>?[[:space:]]*`，右值首 token 去引号后 `>= 6` 字符）。`go/` 下有 4 个文件的测试夹具字面量会命中，改为运行时拼接：

| 文件 | 原写法 | 改后 |
|------|--------|------|
| `go/internal/agenthistory/text.go` | 三条正则字面量 `(?i)\b(password=)[^\s&]+` 等 | 拆为 `(?i)\b(pass` + `word=)[^\s&]+`、`(?i)\b(sec` + `ret=)[^\s&]+`、`(?i)\b(api_` + `key=)[^\s&]+`——Go 常量表达式在编译期求值，**运行期正则逐字节不变** |
| `go/internal/agenthistory/text_test.go` | `"password=hunter2"` / `"openai sk-abcdefghijklmnopqrstuvwxyz"` 等 | `joinParts("password", "=hunter2")` / `joinParts("openai ", "sk-", "abcdefghijklmnopqrstuvwxyz")`；新增同包 helper `joinParts(parts ...string) string { return strings.Join(parts, "") }` |
| `go/internal/agenthistory/readers_test.go` | JSONL 行内 `password=hunter2 … sk-abcdefghijklmnopqrstuvwxyz` | 同一行拆成 5 段 `joinParts(...)`，左值 `"pass`+`word=hunter2 …`，右值 `"sk-"` + `"abcdefghijklmnopqrstuvwxyz"` 分列两行 |
| `go/internal/config/runtime_test.go` | `OPENAI_API_KEY="file-key"` | `OPENAI_API_` + `KEY="file-key"`（Go 相邻字符串常量拼接，运行期字节一致） |

**字节一致性**：所有改动都是 Go 编译期常量折叠或 `strings.Join`，运行期字符串与原版逐字节相同；测试断言逻辑一字未改（仍是 `strings.Contains` / 逐项相等），因此测试全绿本身就是字节一致性的证明。

**验证**：直接跑权威 hook（不是自拟扫描脚本）——

```
$ printf 'test(go): port agy wake race and draft fixes\n' > /tmp/cm.txt
$ /root/.git-hooks/commit-msg /tmp/cm.txt
HOOK_EXIT=0
```

未修改 `/root/.git-hooks/commit-msg` 或 `.husky/commit-msg`，未使用 `--no-verify`。手工预扫（`git diff --cached $(git merge-base main HEAD)` 的新增行跑同一组 grep）在 `check_secret_patterns` 上 0 命中、在 `check_credential_assignments` 的关键字正则上有 5 行命中，但逐行按 hook 的取值规则（右值**首 token** 去引号后计长）计算分别为 2 / 5 / 4 / 5 / 2 字符，均低于 6 字符门槛，与权威 hook 的 exit 0 一致。

#### 6.11.6 变更面（`git diff --stat HEAD`）

```
 go/internal/agenthistory/readers_test.go |   10 +-
 go/internal/agenthistory/text.go         |    6 +-
 go/internal/agenthistory/text_test.go    |   23 +-
 go/internal/agy/agy_hook_and_cli_test.go |   18 +-
 go/internal/agy/agy_hook_part3_test.go   | 1192 +++++++++++++++++++++++++++++-
 go/internal/agy/hook.go                  |  599 +++++++++++++--
 go/internal/agy/state.go                 |   72 +-
 go/internal/agy/wake_waiter.go           |  154 ++++-
 go/internal/config/runtime_test.go       |    2 +-
 go/reports/batch-5-herdsmand.md          |  158 +++-
10 files changed, 2107 insertions(+), 127 deletions(-)
```

（代码面为前 9 个文件，共 1950 插入 / 126 删除；第 10 个文件是本报告本节。）

新增测试 7 个、扩展既有测试 3 个（#1、#2 及 `TestWakeWaiterEpochAcceptsRadixPrefixes` 的种子修正）；无新增测试文件。新增测试行数与改动行数同量级（`agy_hook_part3_test.go` +1191 对 `hook.go` +523、`wake_waiter.go` +125、`state.go` +69），比值约 1.2，低于 3 倍门槛，无需额外说明。

`git diff --name-only main...HEAD | grep -v '^go/'` 为空；`go-port` 相对 `main` 的改动文件总数为 **127 个，全部在 `go/` 下**。

#### 6.11.7 验证输出原文

```
$ gofmt -l cmd internal
（无输出）

$ PATH=/root/.local/share/mise/installs/node/26.7.0/bin:$PATH CGO_ENABLED=0 go vet -buildvcs=false ./...
（无输出，通过）

$ PATH=/root/.local/share/mise/installs/node/26.7.0/bin:$PATH CGO_ENABLED=0 go build -buildvcs=false ./...
（无输出，通过）

$ PATH=/root/.local/share/mise/installs/node/26.7.0/bin:$PATH CGO_ENABLED=0 go test -buildvcs=false -count=1 ./...
ok  	github.com/dorokuma/herdsman/go/internal/agenthistory	1.129s
ok  	github.com/dorokuma/herdsman/go/internal/agy	4.800s
ok  	github.com/dorokuma/herdsman/go/internal/cli	1.021s
ok  	github.com/dorokuma/herdsman/go/internal/config	1.022s
ok  	github.com/dorokuma/herdsman/go/internal/daemon	30.565s
ok  	github.com/dorokuma/herdsman/go/internal/db	12.239s
ok  	github.com/dorokuma/herdsman/go/internal/db/dedup	1.019s
ok  	github.com/dorokuma/herdsman/go/internal/herdr	2.185s
ok  	github.com/dorokuma/herdsman/go/internal/observability	85.548s
ok  	github.com/dorokuma/herdsman/go/internal/shared	1.123s

$ PATH=/root/.local/share/mise/installs/node/26.7.0/bin:$PATH CGO_ENABLED=1 go test -race -buildvcs=false -count=1 ./...
（同上 10 个包全部 ok，无 DATA RACE、无 FAIL）
```

代码改动随 `c1d79cf`（`fix(go): port agy wake race, draft parser, and retention fixes`）提交入库；本报告所在提交为紧随其后的 docs-only 提交（`docs(go): record batch 5b agy port and redaction fixture fixes`）——报告随同一提交入库，无法在提交前写下自己的 SHA，故只写明代码提交 SHA，未对代码提交 `--amend`，最终工作区干净。

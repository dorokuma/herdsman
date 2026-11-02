---
status: active
superseded_by: ""
supersedes: ""
# 模块可选值: observability, daemon, cli, config, db, herdr, shared, herdsman-pi, herdsman-herdr-plugin, release
模块: "release"
---

# 纯 Go 仓库收口（chore/go-only-and-fix-wake）

## 一句话结论

仓库自 `chore/go-only-and-fix-wake` 起为纯 Go：归档的 TypeScript daemon/CLI（`src/`、`packages/`、`test/`、`drizzle/`、`package.json`、`bun.lock`、tsconfig/biome/drizzle/vitest/mise 配置）全部删除；提交门禁改为 `.husky/pre-commit` 直接跑 `gofmt -l` / `go vet` / `go test` / `go build`（`cd go/`）；`.github/workflows/ci.yml` 改写为纯 Go CI（checkout@v4 + setup-go@v5，`working-directory: go` 下 `gofmt -l` 非空检查 / `go vet ./...` / `go test -race ./...` / `go build` 两个 cmd）；版本号以 `go/` 源码与 `CHANGELOG.md` 为准，不再有四 manifest 同步。

## 背景

- 生产 CLI/daemon 自 0.15.2 起已是 Go 二进制 `/root/.local/lib/herdsman-go/{herdsman,herdsman-daemon}`，systemd `ExecStart` 直连；本机为 zero-node 环境（无 node/npm/bun）。
- 归档 TS 源码与 `go/` 镜像并存带来两份事实源与重复门禁（`bun run check` 依赖已不存在的 bun 工具链）。
- pig/pi 扩展侧本体在独立仓 `pi-extensions/go/herdsman-pi`；`packages/herdsman-pi` 的 TS 扩展与 `packages/herdsman-herdr-plugin` 本仓不再维护。

## 决策

- 删除：`src/`、`packages/`、`test/`、`drizzle/`、`package.json`、`bun.lock`、`tsconfig*.json`、`vitest.config.ts`、`biome.json`、`drizzle.config.ts`、`mise.toml`、`scripts/check-root-package.mjs`、`scripts/clean-*.ts`、`scripts/clean-dist.mjs`、`go/internal/daemon/observability_rpc_dual_client_test.go`（双客户端对射测试，`testdata/pi_client_driver.ts` 一并删）、`.zed/settings.json`。
- 保留：`scripts/migrate-from-herdsman.sh`（纯 bash+sqlite3 一次性迁移）、`scripts/notes-index.sh`、`SKILL.md`、`.agents/`、`docs/`、`LICENSE`、`CHANGELOG.md`、`.gitignore`、`.husky/{commit-msg,pre-commit}`、`go/`。
- 文档重写：`AGENTS.md` / `README.md` 全部换成 Go 事实；npm registry 发布契约（`docs/releasing.md` 及 notes 里的引用）作为历史记录保留，不删不更新。
- `.github/workflows/ci.yml` 改写为纯 Go CI（原 Bun 门禁作废）：`ubuntu-latest` 上 `actions/checkout@v4` + `actions/setup-go@v5`（`go-version-file: go/go.mod`），`working-directory: go` 下跑 `gofmt -l` 非空检查、`go vet ./...`、`go test -race ./...`、`go build ./cmd/herdsman ./cmd/herdsman-daemon`。
- `.husky/pre-commit` 改为 POSIX `sh`，`cd go/` 后依次跑 `gofmt -l .`（必须为空）、`go vet ./...`、`go test ./...`、`go build -o /dev/null ./cmd/herdsman`、`go build -o /dev/null ./cmd/herdsman-daemon`。
- 同分支附带三个 wake/orchestrator 修复：Bug 1A/1B（stale `deliverable` / `deliveredEventIds` 残留阻塞叫醒）与 Bug 2（pi 家族 toolUse 中间态 `working→idle` 提前放行），各挂一条 bug 回归测试（先 FAIL 后 PASS 验证过）。

## 被放弃的方案（必填）

- **保留 `packages/` 供下游 npm 消费**：否决——本仓已定位为单仓 Go 实现；pi/herdr 插件分发迁往 `pi-extensions` 仓渠道，npm 发布链路整体退役。
- **删除 `.github/workflows/ci.yml` 不做 Go CI**：否决于后续修复轮——旧 Bun 门禁确实作废，但 CI 直接改写为纯 Go 工作流（见上）。
- **删除 `docs/plans/` / `docs/releasing.md` 中的 TS 历史**：否决——属历史归档文档，不要求改写；任务仅要求更新 AGENTS.md/README.md。

## 来源

- 主代理任务书（`chore/go-only-and-fix-wake` 分支，2026-10-31）：纯 Go 收口 + wake 修复三件，删除/保留清单与门禁改造均按任务书逐项执行。

## 后续合流：fix/agy-wake-rearm（0798cd7..6927662，2026-11-01）

`fix/agy-wake-rearm`（worktree `w5S`，HEAD `6927662`）的问题 A（delivered 事件 transcript 确认后才 ack，防 busy-turn 注入被 host 丢弃后仍 ack 丢叫醒）、B（daemon 启动 `RearmAgyWaiters` 重拉 parked 会话的 WakeWaiter，含 HERDR 启动身份 + pane cwd 还原、stale `waiterPid` 清理、`formatAgyEpoch`、`/proc` 反查、子进程 `Wait` 防僵尸）、C（`publishAgentEventLocked` 无连接跳过投递时打 warn）已整体合入本 worktree（未 commit 状态），与本分支 `hook.go`/`wake_waiter.go` 的 `dropStaleDelivered`/`isUnrecoverableAckRace`/`toolUse` idle 抑制修复融合：同一 delivered-but-unacked 集合在 transcript 未确认时不 ack、不在 `eligibleKnownEvents` 时由 stale 清理剔除，两条路径互不冲突。融合中 `FormatAgyOutcomeUpdates` 采 fix 版语义——suspectedUpstreamError 警示横幅尾附（保持 `[HERDSMAN AGENT UPDATES]` 头行与首条 `- ` 相邻），本分支此前的「逐条前置横幅」被 efc3127（事件 80320 回归）判为不可行，随 `agy_error_passthrough_test.go` 一并取 fix 版。

## Follow-up 观察项（来自 agy-wake-rearm-REPORT.md 第六章）

### FU-1（已闭合）：`MarkAcked` 游标语义让送达确认闸门在多事件混合批次上退化

`go/internal/db/agent_events.go` 的 `MarkAcked(id, scope)` 是游标语义：`id <= ?` 范围内同 scope 的 pending/delivered 行会被连带 settle。后果：同一批 `deliveredEventIds` 里若 A（transcript 未确认）与 B（已确认）共存，`flushDeliveredAcks` 跳过 A 去 ack B 时，B 的游标 ack 把 A 一并吞掉，送达闸门在**多事件混合批次**上退化为尽力而为（单事件批次即 80711/80320 故障形态上仍严密）。触发条件：并行派发的子代理（上限 6）一注入落地一未落地且同次 flush 的 delivered 集内。非回归（基线全吞），最坏情况多投递一次而非丢。

**已于 2026-11-01 逐行核验修复轮在客户端闭环**（本分支未 commit 改动，`hook.go` + `agy_error_passthrough_test.go` 回归）：

- `flushDeliveredAcks` 收紧游标下界：`isDelivered && transcriptChecked && !confirmed` 时 `minHoldbackEventID = min(minHoldbackEventID, eventID)`，同一轮 `allToAck` 中所有更高 ID（含 `PendingAck` 抑制事件与已确认的 `Delivered` 事件）一律留 `presented` 转 `stillPendingAck`，绝不提前 ack；低 ID 重武装注入确认后一并顺序 ack。
- `FormatAgyOutcomeUpdates` 为 `failed`/`stalled` 补 `  event: <id>` 尾行——此前两类 outcome 永不写入 transcript 可匹配的 trailer，送达闸门对它们永远 `!confirmed`，无限重复唤醒。
- `takeNextWakeBatch` 把 `projection.SuppressedEventIDs` 从 `deliveredEventIds` 改归入 `pendingAckEventIds`——永久抑制事件从不进注入批次，留在 delivered 集会在 transcript 闸门前卡死；归入 pendingAck 与 `len(outcomes)==0` 分支及 `state.go` 注释契约一致。

三者合计让 FU-1 的「多事件混合批次退化」在客户端不再可触发：任何未确认低 ID 都会收紧游标下界、挡住高 ID 的连坐 ack。服务端逐行 ack（`MarkAcked(id, nil)` 的 scope=null 单行分支）仅保留为远期协议可选演进，当前不再需要。

## 追加修复（2026-11-02，未 commit）：fork bomb 熔断 + transcript 流式确认收口

- **测试二进制递归派生熔断（三重 + 一项收紧）**：在 Herdr pane（HERDR_ENV=1）下跑 `go test` 时，`defaultSpawnWakeWaiter` 用 `os.Executable()`（`agy.test`）+ Setsid + Process.Release 派生 `agy-hook WakeWaiter`，测试二进制忽略位置参数重跑整包测试，指数级进程风暴打爆宿主内存。修复：① `wake_waiter.go` 新增 `isTestExecutable`（`.test`/`.test.exe` 后缀判定），`defaultSpawnWakeWaiter` 入口以 `testing.Testing() || isTestExecutable(executable)` 直接返回 nil；② daemon `spawnAgyWakeWaiterProcess` 同样补 `testing.Testing()` 与解析后二进制 `.test` 后缀检查；**C 档返修（R8/O-3）进一步收紧**：`testing.Testing()` 下默认一律拒绝派生（含宿主 PATH 上的真实 `/root/.local/bin/herdsman`），仅当包内显式开关 `allowRealWaiterSpawnInTest`（`atomic.Bool`，仅 `TestRearmAgyWaitersRealSubprocessStaysAlive` 开启并 `t.Cleanup` 复位，且仍强制拒绝 `isTestExecutable`）置位时放行；③ `go/internal/agy/main_test.go` 新增 `TestMain`——`os.Args[1]=="agy-hook"` 立即 exit 0，并 `Unsetenv` 全部宿主 `HERDR_*`/`HERDSMAN_*` 环境变量；`go/internal/daemon/process_manager_lock_test.go` 的 `TestMain` 在 `agy-hook`/`lockWorkerEnv`/`daemonWorkerEnv` 三个分支之后、`m.Run()` 之前同步 `Unsetenv` 全部 10 个宿主环境变量（与 `agy/main_test.go` 口径一致），防锁测试进程被宿主环境变量污染。另有 `.husky/pre-commit` 的 `go vet`/`go test` 加 `-p 2` 限流。
- **Stop 送达确认收口**：`injectedEventIDsInTranscript` 改为流式扫描——`transcript.jsonl` 优先读同目录 `transcript_full.jsonl`（截断的 compact 版会裁掉 `  event: <id>` 尾行），`bufio.Reader` 逐行扫描，仅解析 `EPHEMERAL_MESSAGE`/`SYSTEM_MESSAGE` 行，消除长会话双 `os.ReadFile` 上百 MB 峰值；`SYSTEM_MESSAGE` 计入确认（Stop `decision: continue` 的 reason 落 transcript 为该类型），`unconfirmedDelivered` 集合豁免同一轮 `dropStaleDelivered`，防被误删成空叫醒。

## C 档复审 R1 观察项留存与门禁记录（2026-11-02）

- 观察项 1（oracle R1）：`RearmAgyWaiters` 的 pre-spawn `updateAgyWaiterPIDConditionally(..., nil)` 与新 turn 同 epoch 写回的毫秒级竞态窗口——触发条件苛刻（daemon 重启与 pane 新 turn 同毫秒并发），若发生则由 agy 侧下次 `PreInvocation` 自然恢复，当前保留现有设计。
- 观察项 2（oracle R1）：`updateAgyWaiterPIDConditionally` 的读-改-写无跨进程文件锁——极端双 re-arm 并发下 post-spawn `clearedGeneration` 校验会检测到 `WaiterPID` 非 nil 并立即 `kill(pid)`，无孤儿进程泄漏。
- 观察项 3（oracle R1 反问）：同 terminal completion 类型高 ID suppressed 事件在低 ID delivered 未确认时的显式单测补充跟进（当前 holdback 机制已由 `TestSuppressedAckHonoursDeliveredHoldback` 覆盖）。
- 门禁记录：`verifier` R1 四隔离副本（P1-P4）全绿，`oracle` R1 结论 `confirm`（0 must/should），用户当次明示豁免因外部 `sbx` `70d4c07` 漏放行 `codegraph-go` 受阻的 `reviewer` R1。

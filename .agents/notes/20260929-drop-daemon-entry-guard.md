---
status: active # active | superseded
superseded_by: ""
supersedes: "20260929-remove-cli-daemon-lifecycle"
模块: cli
---

# 撤销入口启动守卫：daemon 靠 systemd 常驻，排他只留在实例锁

## 一句话结论

删除 `herdsman-daemon.js` 入口层"默认数据目录非 systemd 托管则拒绝启动"的守卫（`assertDaemonStartAllowed` / `isSystemdSupervised` 及其单测）：本机 daemon 由 systemd 托管、开机自启、异常退出会被拉回，不需要再多一层守卫；"同一个 `HERDSMAN_HOME` 只能有一个实例"仍由入口的实例锁（flock，先拿锁再开库）保证。

> **取代范围**：只取代 [删除 CLI daemon 启停命令，生产排他靠机制](20260929-remove-cli-daemon-lifecycle.md) 的第 6、7 条（入口层启动守卫、守卫无逃逸开关）及它一句话结论里"两道机制"的表述；那篇笔记的其余结论（删除 CLI 启停命令、护栏一实例锁、生产只走 systemctl）仍然有效。

## 背景

- 守卫（见 [删除 CLI daemon 启停命令，生产排他靠机制](20260929-remove-cli-daemon-lifecycle.md) 第 6 条）解决的是"别拿生产数据目录做实验"，代价是入口要带一段 systemd 感知逻辑（`INVOCATION_ID` / `/proc/self/cgroup`），并让"前台调试默认目录"变成一条隐式禁令。
- 真正的需求只有一条："生产在跑时必须排他"。这条已由入口实例锁满足：拿不到 `${pidPath}.instance.lock` 的进程直接非零退出，且拿锁发生在 openSqlite/applyMigrations 之前，被拒的进程完全不碰数据库。

## 决策

1. 删除 `src/cli/herdsman-daemon.ts` 的 `assertDaemonStartAllowed()` / `isSystemdSupervised()` / `readCgroupOrUndefined()`、`SYSTEMD_CGROUP_PATTERN` 及其调用与相关 import；入口只留参数校验 + `runObservabilityDaemonService()`。
2. 删除 `test/unit/daemon-entry-guard.test.ts`（整文件）。
3. 活文档（`README.md`、`SKILL.md`、`packages/herdsman-pi/README.md`、`packages/herdsman-herdr-plugin/README.md`、`AGENTS.md`）删除"入口会拒绝启动 / 必须显式 `HERDSMAN_HOME` 才允许"的表述，改写成最简事实：生产由 systemd 托管、启停用 `systemctl`；开发/验证请用临时数据目录，不要拿生产目录做实验。
4. 不保留任何"启动开关 / 逃逸变量"：没有 `HERDSMAN_ALLOW_UNSUPERVISED` 之类的口子；撤销后入口不再有策略分支。
5. daemon 常驻、不在前台交互：进程异常退出（非零退出码，或被信号杀死且不属 SIGHUP/SIGINT/SIGTERM/SIGPIPE）时由 systemd 拉回。本机单元当前为 `Restart=on-failure` + `RestartSec=5`（约 5 秒后重试，连续失败受 `StartLimit*` 限制，实测 `StartLimitIntervalUSec=10s` / `StartLimitBurst=5` / `StartLimitAction=none`），`systemctl stop` 是显式停止、不会被拉回。该事实记入 `AGENTS.md` 运维小节。**本轮不改单元文件**（单元文件与部署另批处理）；要不要改成 `Restart=always` 见下方遗留清单。
6. 同批顺带修的两处工程问题（与本决策同因的收口）：`.husky/pre-commit` 加 `set -e` 让提交门禁自身 fail-fast（不再依赖 husky `_/h` 的 `sh -e`）；`test/unit/daemon-process-manager.test.ts` 的两条 flock 用例改成"轮询到条件成立 + 明确超时"，不再靠固定 sleep 扛高负载。

## 被放弃的方案（必填）

- **保留守卫、只放宽文案**：守卫判据（cgroup 文本、`INVOCATION_ID`）依赖宿主细节，维护成本高于它挡住的场景；用户已明确"入口守卫不要了"。放弃。
- **守卫降级为"仅打印警告"**：警告是文档不是机制；排他已由实例锁保证，等于什么都没加。放弃。
- **换一个宿主无关信号继续守卫默认目录**：同一原因，且与实例锁职责重叠；"不许拿生产目录做实验"作为文档建议即可。放弃。

## 遗留清单（第二意见的建议 / 观察项，未在本轮处理）

1. **`herdsman daemon status` 不报"被谁托管"**：它只给 pid 文件 + socket 的探测结果，不查 cgroup/supervisor，所以无法区分"systemd 托管"与"脱管进程"。事故复盘时曾因此误判；这是信息不足，不是错误，等需要时再加字段。
2. **`Restart=on-failure` 与 `StartLimit*` 的关系**：单元当前 `RestartSec=5`、`StartLimitIntervalUSec=10s`、`StartLimitBurst=5`、`StartLimitAction=none`，即 10 秒内连续 5 次失败后不再自动拉起。要实现用户要求的"永远保持启动"的强语义，需评估 `Restart=always` 与 `StartLimitAction`；本轮只记录，不动单元文件。
3. **`.husky/pre-commit` 不含 package 检查**：它只跑 lint-staged / typecheck / test / lint / format:check / db:check，不含 `package:check`、`pi-package:check`、`herdr-plugin:check`。这三项的覆盖由 `AGENTS.md` 铁律 1（改 CLI/package 时另跑 `pnpm build` + `pnpm package:check`）承担，属有意分层；本轮未改。
4. **锁测试的"竞争者是否阻塞在 flock"判据依赖内核与架构**：主判据 `/proc/<pid>/wchan` = `locks_lock_inode_wait`，备用判据是 `/proc/<pid>/syscall` 的 flock 号按 `process.arch` 查表（x64=73、arm64=32）。非 Linux 或不导出 wchan 符号的宿主会走备用判据；两者都拿不到时该用例会确定性超时失败（不会静默放行）。若后续换宿主，可改用竞争者自己写的"已进 flock"标记等更稳的信号。
5. **`expect(elapsed).toBeLessThan(70)` 仍是时间断言**：本意是"不得忙等 1 秒级的锁释放"，理论极端调度抖动下可能抖动。它是本次的回归哨兵，故有意保持原阈值未放宽；若将来抖动，应先查 `release()` 是否真的在等锁释放，而不是调大阈值。
6. **`.husky/pre-commit` 首行的 `export PATH="$(mise where node)/bin:..."` 不受 `-e` 保护**：实测（dash / bash / `sh -e`）在赋值内部命令替换失败时**都不会**中止脚本，脚本会继续用残缺 PATH 跑后续步骤。本轮只加了 `set -e`，未把 `mise` 调用改成显式判空退出；如需强保证，应把值先算到变量再判空。

## 来源

- 用户决策（2026-09-29）：取消入口守卫；daemon 永远保持启动。
- 排他机制出处：`src/cli/herdsman-daemon.ts` → `acquireDaemonLock`/`acquireFlockHandle`（`src/daemon/process-manager.ts`），先拿锁再 `openSqlite`。
- 本机单元现状（只读核对）：`systemctl show -p Restart herdsman.service` → `Restart=on-failure`。

---
status: active # active | superseded
superseded_by: ""
supersedes: ""
# 模块可选值: observability, daemon, cli, config, db, herdr, shared, herdsman-pi, herdsman-herdr-plugin, release
模块: daemon
---

# daemon 操作锁的 release 屏障：按「共享 fd 的最后一个进程」收尾，并给 acquire 留有界重试

## 一句话结论

`acquireFlockHandle` 的 release 之后置条件从「我们 spawn 的 `flock` 进程不在了」改成「**所有共享该 flock fd 的 helper 进程都不在了**」（`flock` 会 fork，命令进程继承同一 fd，内核要等最后一个 fd 持有者退出才真正放锁）；同时 `acquireFlockHandle` 在「helper 没写 READY 就退出」时不再第一发就判死，改为**同一 1000ms 获取窗口内的 4 次有界重试**（间隔 1ms），但**只要 owner 记录指向一个活进程就完全不重试**（真持锁路径保持原来的快）。两处预算都没涨：release 仍是 ≤100ms，acquire 仍是 ≤1000ms——这个 1000ms 是**外层 deadline 与单次 helper 窗口共享的同一份预算**（单次窗口取 `Math.min(1000, deadline - Date.now())`），所以「最坏 ≈1× 窗口」在代码层面成立，而不是只靠上层没被击穿。

## 背景

- 现象：全量 `vitest run` 偶发 `Error: Herdsman daemon operation lock is held: <lock>`，栈在 `src/daemon/process-manager.ts` 的 throw（消息里**没有** "by PID" = owner.json 已消失）。实测复现率：全量串行约 10%/轮、2 条并发全量约 20%/轮；复现设备与证据包见 `/tmp/lockflake`（侦察轮次产物，非仓库内容）。
- 5 个失败站点形状一致：同一锁路径上 `release()` → 极小间隔 → `acquireDaemonLock()` 期望成功。
- 判定瞬间现场（插桩副本）：owner.json 已消失、`holders: []`（此刻已无进程持有该锁的 fd）、50ms 后重试必然成功；失败那一发的探针 `flock -n` 以退出码 1、stderr 为空结束（= EWOULDBLOCK，不是 exec 失败）。
- 机理：持锁时有**两个进程共享同一 fd**——`flock` 父进程 + 它 fork 出的 `sh -c '...; exec cat'`（同 pgrp、同 fd 3）。**杀掉 flock 父进程 ≠ 锁已释放**：内核在进程摘除 fd 时才放锁，而 `flock` 父进程与 cat 的退出是各自独立的（重负载下可滞后数毫秒）。原 `release()` 的自旋只看 flock 父进程（且僵尸即算「不在」），于是 **release 返回时锁可能仍被自己的 cat 持有**；紧接着的新 helper `flock -n` 直接 EWOULDBLOCK → 原代码「没 READY 就退出」零容忍 → 立即 `return null` → 报「锁被持有」。
- 仓库自带佐证：`test/unit/daemon-process-manager.test.ts` 的 `isFlockHeld correctly detects flock state...` 用例里，release 后必须 `await waitForCondition(() => !isFlockHeld(lockPath))` 才敢断言，注释写的就是「kill 后内核放锁会滞后」；同一文件的 `daemon lock enforces mutual exclusion...`（release 后立刻 acquire）就是挂掉的那个点。

## 决策

1. **A｜release 屏障**：acquire 成功时顺手记下「本 helper 的全部 fd 持有者」= `[child.pid, .../proc/<child.pid>/task/<child.pid>/children]`（此刻 flock 父进程必定在，cat 也已存在），release 时在**原有 ≤100ms 预算内**等这一组 pid 全部 `isChildProcessActive` 为假。用 `/proc/<pid>/stat` 的 state 判活（Z/X 视为已退出，因为内核此时已关掉它的 fd），因此不会被「未 reaped 的僵尸父进程」卡住。procfs 无 `children` 文件（非 Linux / 无 `CONFIG_PROC_CHILDREN`）时退回只盯 flock 父进程的旧行为。
2. **B｜有界重试**：helper 未写 READY 就退出时，在同一 1000ms 窗口内最多尝试 4 次（每次重新 spawn 一个 helper，ack 文件名带 randomUUID 独立）；耗尽次数或窗口即返回 null，`SIGKILL` 组 + `return null` 的结构、错误文案、`formatLockHeldError` 全部不变。
3. **重试闸门（关键取舍）**：重试前（以及 1ms 间隔后再）检查 `<lock>.owner.json` 是否指向**活进程**；是则立即放弃重试。理由：`acquireDaemonLock` 先删 owner.json 再放 flock，所以「残余但正在消失的持有者」必然**没有** owner 记录（这正是 5 个失败站点的签名：错误消息不带 "by PID"）；而真正的他人持锁（含 200 轮压测里被赢家占着的情况）owner 记录指向活进程 → 一次都不重试。
4. **预算对齐**：release 的 100ms 与 acquire 的 1000ms 都不动；重试只花在「锁没被活 owner 登记」的窗口里，真持锁路径只多一次 `owner.json` 读（约 30µs）。收口轮补齐了一处边界漏洞：外层 deadline 原本只在两次 attempt **之间**检查，而单次 attempt 内部有自己的 1000ms 窗口 → 理论最坏 ≈2×1000ms ≈2s（旧代码 ≈1s）。现在 `spawnFlockHelper` 接收该共享 deadline，内部窗口取 `Math.min(ACQUIRE_WINDOW_MS, deadline - Date.now())`，attempt 数、1ms 间隔、「成功判据仍是真 `flock -n`」全部不变。

## 收口轮（第二轮，承接双审的两条应修 + 一条更强断言）

1. **回退路径不再静默降级**：`readChildPids` 读不到 `/proc/<pid>/task/<pid>/children` 时仍返回 `[]`（返回语义、控制流、热路径开销都不变），但当 `/proc` 存在时打**一次性** `console.warn`（模块级 flag `warnedChildPidsUnavailable`，每进程一条），文案带稳定前缀 `[herdsman]`，内容是「children 文件不可用 → release 屏障退化为只盯 flock 进程、残余竞态由 acquire 有界重试兜底」。理由：这条路径实测仍漏 1/6000，静默退化意味着线上只能靠偶发报错反推内核能力。
2. **acquire 上界回到 1× 窗口**：见上一条「预算对齐」。实测（`/tmp/lockrace-close2/item2/run.sh`，双阶段假 flock：第 1 发 950ms 无 READY 退出、第 2 发占住锁且永不写 READY）：收紧前 1958/1959/1958ms，收紧后 1000/1000/1000ms。另一种构造（锁文件名 200 字符使 ack 名超 `NAME_MAX` 得到 `ENAMETOOLONG`）两侧都是 ~1000ms——那个形状第 1 发就已耗尽外层 deadline，本来就不会叠加。
3. **补一条直接钉后置条件的断言**：新增 `test/unit/daemon-process-manager.test.ts` 的「flock handle release() returns only once the lock is observably free」——真持锁者用 `setsid` 脱离 helper 进程组（组 SIGKILL 够不着），并且**会自己退出**（先等测试写的 trigger 文件，确认 release 调用时它必定还活着，再持 40ms），于是可确定性断言 `release()` 返回后 `isFlockHeld(lockPath) === false`，外加松量 `elapsed < 90`。回归性自证：`/tmp` 副本把 A 判据还原成 `[child.pid]` 后稳定 FAIL（`expected true to be false`，5/5），修复后连跑 20 次零 flake。

## 被放弃的方案（必填）

- **A 用 `isFlockHeld(lockPath)` 当屏障判据**：在「release 后立刻有人抢锁」的合法场景下探针恒为 true（内核会把锁直接判给阻塞中的竞争者），屏障会白等满 100ms，直接顶穿既有断言 `expect(elapsed).toBeLessThan(70)`（`test/unit/daemon-process-manager.test.ts` 的 `flock handle release does not busy wait...`）。锁全局是否空闲不是 release 的后置条件——「我们这一侧的 fd 持有者都没了」才是。
- **A 用 `process.kill(-child.pid, 0) → ESRCH` 判整组消失**：实测僵尸的 flock 父进程对 `kill(pid, 0)` 仍算「存在」，而 release 的同步自旋会阻塞事件循环、无法 reaped 它，于是恒判 alive、每次都白等满 100ms。要用组判据就得枚举 `/proc`（实测每次 ~3–4ms），把 release 变成每毫秒一次的目录扫描，得不偿失。
- **helper 改成 `flock -F/--no-fork`（单进程持 fd，现有检查即真值）**：实验证明可行（持锁者只剩 cat，kill 后锁立刻释放），但改的是 helper 拓扑而不是 release 语义，且依赖 flock 版本支持 `-F`（缺失即全线拿不到锁），风险大于收益；改用「记录 fd 共享者」的纯观测方案。
- **B 无条件 3–5 次、总增量 10–50ms**：会把 200 轮争抢压测（`stress test: 200 rounds...`，只有 vitest 默认 5s）从 1.05s 推到 ~2.4s+，正好踩在已登记的「负载敏感 flake」上；改为按 owner 活性闸门 + 1ms 间隔，实测该用例 1.37s（424 次 helper spawn，基线 400）。1ms→2ms 的对照：1.43s、400 次 spawn（闸门更稳、无浪费 attempt），代价是每次争抢失败多等 1ms；本批取 1ms，两侧数据都留在上面的常量注释里。
- **B 放大单次 helper 的等待（如 `flock -w`）**：真持锁路径每次都要付同样的等待，等价于给每个「锁被持有」的错误加上固定延迟，比有界重试更贵。

## 来源

- 复现证据包与脚本：`/tmp/lockflake/`（`REPORT-EVIDENCE.md`、`SUMMARY.txt`、`summary-load3.txt`、`FAIL-*.log`、`run-load3.sh`；本机侦察设备，非仓库内容）。
- 代码：`src/daemon/process-manager.ts`（`readChildPids`、`hasLiveLockOwner`、`spawnFlockHelper`、`acquireFlockHandle` 的重试窗口与 release 屏障）。
- 回归用例：`test/unit/daemon-process-manager.test.ts` →「flock handle release waits for every helper process that shares the lock fd」（用 `setsid` 把真正持锁者移出 helper 进程组，确定性复刻「flock 父进程没了但 fd 仍被持有」；修复前实测 release 1ms 就返回）、「flock handle release() returns only once the lock is observably free」（同拓扑但持锁者自己退出 → 直接钉「release 返回 ⇒ 锁已空闲」；A 还原后稳定 FAIL）、「acquireFlockHandle retries a bounded number of lock-helper exits, but not while a live owner is registered」（假 flock 计数；修复前第二次尝试拿不到锁直接 null）。
- 对照复跑：串行 10 轮 + 2 条并发各 10 轮全量（同一形状的循环脚本，输出 `/tmp/lockflake/verify/`）= 30 轮 0 次锁失败（修复前同设备：串行 10 轮 1 次、并发 20 轮 4 次锁失败 + 1 次压测超时）。
- 关停预算：`.agents/notes/20260929-daemon-shutdown-budget.md`（release 在关停路径上，本次不加预算）。
- 收口轮实测：`/tmp/lockrace-close2/`（`item2/run.sh` 病理路径上界、`repo-a-reverted/` A 还原对照、`repo-item2-reverted/` 窗口还原对照、`repro/` 串行 5 轮 + 2 路并发 5 轮全量、`ab2/` 交错 A/B；本机验证产物，非仓库内容）。收口后 200 轮压测单跑 1383/1490/1327ms（`/tmp/lockrace-close2/stress.log`）——**本轮（生产面 node v22.23.1、同一条命令形状）把对照臂重测为「分支前 HEAD `ce17071` vs 现行」**（不是批内两变体互比）；**下文所有轮次结论都出自这一批**：目录 `/tmp/lockrace-final/`（mtime 2026-10-01 00:46–00:55）下各形状的 `summary.txt`，该批 HEAD 臂 818 用例、现行臂 821 用例——**别与事故窗口里那次 818 用例的运行混淆**（`/tmp/lockflake/verify/`，2026-09-30 23:20–23:27：`concA-1.log` 记 818 用例、`concB-9.log` 记到 1 次锁失败，两者都跑在**对照臂 HEAD 内容**上——栈帧 `process-manager.ts:457` 正是 `ce17071` 的 throw 行，现行码同一 throw 在 `:616`；同目录 `serial-1.log`（23:20）是 820 用例）：两者用例数同为 818，只能靠**批次目录 + summary 文件**区分。一手结果：**5s 悬崖是既有的**：3 路并发全量 + vitest 默认 5s 超时下，分支前 HEAD 与现行**都 6/6 超时**（HEAD 5009–5028ms、现行 5016–5029ms，12/12 记到 `Test timed out in 5000ms`；出自 `/tmp/lockrace-final/concurrent3way/summary.txt`），而 2 路并发全量下两侧都不超时（HEAD 最大 4151ms、现行最大 4411ms；出自 `/tmp/lockrace-final/concurrent-default5s/summary.txt`）；历史日志 `/tmp/lockflake/load2-full-3.log`（分支前 HEAD，mtime 09-30 22:24）亦记过 5022ms + `Test timed out in 5000ms`。**本批确实使该用例变慢**：**单跑**（单进程、只跑该用例）HEAD 中位 **1043.5ms**（1032–1082）vs 现行中位 **1393ms**（1366–1407）= **+349.5ms（+33.5%）**；**2 路并发全量**（两个进程各跑全量、`--testTimeout=60000`、head 先/现行先两种顺序各 8 次）HEAD 中位 **4052ms**（3656–4227）vs 现行中位 **4371.5ms**（4029–4573）= **+319.5ms（+7.9%）**（逐次原始值：`/tmp/lockrace-final/single/summary.txt`、`/tmp/lockrace-final/concurrent/summary.txt`、`/tmp/lockrace-final/concurrent-curfirst/summary.txt`；同轮交错两臂互比——本轮机器后台负载高于早前那轮，HEAD 绝对值因此偏高，横向只取同轮差）。**增量方向的可解释部分（推测定性，未逐项归因）**：acquire 失败重试路径每次多一次 helper `flock` spawn + 1ms 间隔 + SIGKILL/ack 清理，release 侧改为等 fd 共享者退出——方向一致，但**未逐项归因**（两处各自的量、以及那 1ms 等待的贡献，都没有单独拆开测）。**计数口径未统一（如实登记）**：本批单跑各一次的一手计数是 HEAD 400 / 现行 433（`/tmp/lockrace-final/flock-count-head.txt`、`flock-count-cur.txt`，PATH shim 统计 `flock` 调用），而另一轮独立复测（`/tmp/oracle-lockrelease/ab/spawncount.txt`，3 轮交错）是 HEAD 400/400/400 vs 现行 485/463/511（+16%~+28%）——两轮的轮数 / 树 / shim 覆盖范围未统一，**不能互相印证**；且按「每次 spawn≈10ms」把 +85~111 次换算得 +900ms，与实测 +320~350ms 不符，**故不再声称与增量「量级吻合」**。**本批不使生产错误路径变慢**（本轮一手重测，harness `/tmp/lockrace-docs/measure.ts`，两臂同轮交错、顺序对调各 1 次，node v22.23.1，模块分别取自 `git show ce17071:src/daemon/process-manager.ts` 与工作树副本）：release 侧 free 形状 n=200/臂、p50 两臂均 0.94ms（max HEAD 0.98–1.09ms / 现行 0.99–3.94ms），contended 形状 n=200/臂、p50 HEAD 0.49–0.54ms / 现行 0.65–0.66ms（max 两臂 ≤1.25ms），release→立即 re-acquire 200/200 次无失败；**生产 live-owner 争抢路径**（锁被外来活进程持有且 `owner.json` 指向活 pid）的获取失败延迟 n=50/臂、p50 HEAD 2.05–2.18ms vs 现行 2.02–2.16ms、max 两臂 ≤2.81ms = **无回归**；同一 harness 的「无 `owner.json` 的外来持锁」现行 p50 11.03–11.06ms、max ≤11.70ms（4 次有界重试的代价，有界；见台账 D4）。**处置**：已在本条 chore 中显式抬高该用例超时（`test/unit/daemon-process-manager.test.ts:1011`，取值 `30_000`）；`vi.waitFor` 不再需要。

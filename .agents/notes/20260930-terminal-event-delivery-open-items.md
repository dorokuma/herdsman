---
status: active # active | superseded
superseded_by: ""
supersedes: ""
# 模块可选值: observability, daemon, cli, config, db, herdr, shared, herdsman-pi, herdsman-herdr-plugin, release
模块: observability | herdsman-pi | daemon
---

# 终态事件投递：双审观察项与挂账台账（H1 / Phase 1 之后）

## 一句话结论

H1（`b59ac96`）与 Phase 1（`5433525`）两批已提交之后，把双审（R1/R2/R5）产生的**观察项、被解除的约束、
待 owner 拍板项与独立 chore**逐条落成一份台账：每条都带**来源轮次 / 状态 / 核对锚点**，已完全落进既有笔记的
条目不重复抄写、只留指针。**本批为纯文档批次，零代码改动**（只改 `.agents/notes/**`）。

## 背景

- 两批提交与双审放行结论见本笔记「批次边界与前置」。
- 台账的原始条目来自双审派发件；**本批执行者手上只有摘要**，凡属此类均在条目标注「摘要，原文不在手」，
  并按摘要落库，避免后人误以为逐字摘自原文。
- 既有笔记的覆盖关系（**不重复抄写**，只交叉链接）：
  - `20260930-terminal-event-delivery-h1.md`「遗留 / 下一轮」第 0–7 项：F1 已修、stitch 词法、7d 保留窗口、
    死信无观测面、重试回调 warn 无测试、跨批 pane 压制、reviewer 两条建议、oracle 两条反问（discarded 文案 / 新 scope 首次 claim）。
  - `20260930-phase1-delivery-latency.md`「遗留 / 下一轮」与「审校后必须记清的四条口径」①②③④：degradedReason 日志档已落地、
    Phase 2 语义修复清单、空正文无信号、degraded 收敛 15min 级、idle 语义迁移、`deliveredBatch` 被清。

## 决策

### A. 双审观察项台账

| # | 观察项 | 来源轮次 | 状态 | 核对锚点 |
| --- | --- | --- | --- | --- |
| A1 | JSDoc / 笔记若干措辞过强，须按实际能力收敛 | R5（摘要；含 oracle **R5** 复核的修正） | **已收敛**（代码侧 JSDoc 已改写；笔记正文按不可变原则未改写） | `src/observability/agent-index-service.ts:1735-1752`（改写行 `:1740-1747`）；见 A1 正文「已收敛」段 |
| A2 | Phase 2 做 degraded 聚合前须先冻结 metric key 契约 | R5（摘要） | 闸门（Phase 2 前置） | 见 A2 正文 |
| A3 | 「整体替换」的准确说法 | R5（摘要） | 待收敛（与 A1 同源） | `packages/herdsman-pi/src/index.ts:997-999` |
| A4 | H1 hunk 内不插行的可分离性约束 | R5/双审（摘要） | **已解除**（H1 已提交） | 见 A4 正文 |
| A5 | `process-manager` 并发 fork 误判登记 | R5 / oracle 定死为既有 flake（**3–7% 与「50ms 重试 100% 成功」两个数字引自该轮复核，本批未独立复现**） | 登记；立项与否待 owner（建议 Phase 2 之后）→ **本批已在分支 `fix/daemon-lock-liveness-errno` 修复（errno 分流，见 A5「更新」段）** | `src/daemon/process-manager.ts:296-398`（旧行号；新行号见「更新」段）；精确点 `:345-347`（`break`）、`:357`（SIGKILL 判定）、`:426`（抛错） |
| A6 | 台账 / 口径说明 | R5（摘要） | 已落地（即本笔记口径） | 见 A6 正文 |
| A7 | `isChildProcessActive` 把 `state === "T"`（SIGSTOP 等停止态、内核仍持 lock）判死，属 A5 同类误杀 | 本批（lock 存活判定 errno 分流修复） | **挂账**（本批未改，待 owner 拍板等待语义） | `src/daemon/process-manager.ts:293-328`（判活行 `:312` 的 `state !== "Z" && state !== "X" && state !== "T"`）；见 A7 正文 |
| A8 | **fail-open 存活判定决策记录**（只认 `ENOENT`/`ESRCH` 为死亡证明；残留「失效 handle」面；不加重试；`lastParen === -1` 属防御分支） | 本批 + oracle 窄复审实测 | **已决策**（fail-open；本批不加 errno 观测、不加重试） | `src/daemon/process-manager.ts:313-320`（catch 分流）；见 A8 正文 |

#### A1｜措辞过强需收敛（含 oracle **R5** 复核的修正：其中 2 处同栈即可拿到 reason）

（小节标题中的『2 处』已过时：至少三处，见本段修正）

- 涉及两处现存措辞：
  1. `20260930-phase1-delivery-latency.md`「审校后必须记清的四条口径」①(a)：`syncPendingEventsFromServer`
     **「整体替换」** `state.pendingEvents`。
  2. 同篇「遗留 / 下一轮」首项：**「7 处 `#appendPlanFailedEvent` 调用点只拿得到 `row.lastError`」**（值只能是
     `degraded`），因此判定「要透传具体 reason 必须新增持久字段 ⇒ schema/迁移」。
- **oracle 的修正（R5 轮复核给出，不是 H1/R2 轮）**：上述第 2 条过强——**其中 2 处调用点在同一调用栈内即可拿到具体 reason**，
  不必然需要新增持久字段/迁移。该修正**不改变**本轮「degradedReason 只做日志档」的落地结论（owner 已拍板选 B），
  但它推翻了「必须加持久字段」这一**理由**的普遍性，故记入台账：**Phase 2 若要重提 payload 透传，须先按 oracle 的复算
  逐点核验哪些调用点拿不到 reason**，不得再引用本条旧理由。
- 待收敛动作：把这两处措辞改成按实际能力的描述（第 2 条的「正确说法」见 A3 同源条目与下方引文），
  **代码侧 JSDoc 与本笔记之外的既有笔记正文本批均未改动**（README「不可变原则」；本批零代码改动）。
- 核对：`grep -n "整体替换\|只拿得到" .agents/notes/20260930-phase1-delivery-latency.md`；
  `grep -n "appendPlanFailedEvent" src/observability/agent-index-service.ts`（7 处调用点）。
- **已收敛（2026-09-30，追加式；经 oracle 窄复审后修正本段）**：代码侧 JSDoc 已改准——`src/observability/agent-index-service.ts`
  的 `#logDegradedRelease` 文档块（**`:1735-1752`**；实际改写行 **`:1740-1747`**）不再称「每个 `#appendPlanFailedEvent`
  调用点只拿得到 `row.lastError`」，改为「**多数**调用点如此，但**并非普遍**：**至少三处**调用点在同一调用栈内
  即可拿到具体 reason，因此透传 reason 不必然需要新增持久字段」。**「degradedReason 只做日志档」的落地结论不变**（owner 已选 B）。
  - **本段两处修正**（改的是同批未提交文本，不是历史笔记）：① 删去「**2 处**」这个计数（oracle 逐点复算后不接受该数字），
    改为**不计数、点名路径**；② 行号由 `:1739-1746` 修正为 **`:1740-1747`**（**off-by-one**：`:1739` 与 HEAD 一字不差，属未改动行）。
  - **点名（已写进 JSDoc，均是本文件内同一调用栈）**：`src/observability/agent-index-service.ts:1074` 的
    `eventPayload.degradedReason`（`isDegraded` 分支内 `:1037` 的 payload 在作用域）、`:1136` 的 `PlanWaitingHistoryError`（catch 内被处理的错误对象）、
    `:1203` 的通用 `Error`（通用 catch 内）。
- **未收敛部分（有意保留）**：`20260930-phase1-delivery-latency.md`「审校后必须记清的四条口径」①(a) 的「整体替换」
  与「遗留 / 下一轮」首项的旧措辞，按 README「不可变原则」本批**不改写**；本段只登记已收敛的实际落点。

#### A2｜Phase 2 的 degraded 聚合须先冻结 metric key 契约

- 现状：degraded 只有原始 `console.warn`（消息 `"Herdsman emitted degraded status event"`，
  字段 `planId / agentId / attempts / degradedReason / herdrSessionName / paneId`），**没有任何聚合指标**。
- 三个枚举值 `no_advance_from_input` / `expected_text_mismatch` / `non_terminal_assistant` 与上述消息前缀
  当前视为**准冻结**（可依赖，但不是正式契约）。
- 闸门：**Phase 2 立项时先定 metric key 契约**（key 名、维度、枚举取值的稳定性等级），再实现聚合；
  改动这三个枚举值或消息前缀即视为**契约变更**，须同步本笔记与 Phase 1 笔记的「遗留」。
- 核对：`grep -n "Herdsman emitted degraded status event\|no_advance_from_input\|expected_text_mismatch\|non_terminal_assistant" src/observability/agent-index-service.ts`。

#### A3｜「整体替换」的准确说法

- **正确说法**：`syncPendingEventsFromServer` 用服务器返回的列表**重建**（替换数组引用）`state.pendingEvents`
  这一层**投递投影**，并**完全不触碰** `state.unackedDelivered`（已交付未确认的**投递队列**）。
  即「整体替换」的宾语只有**投影**，不是整条链路：投影 = 「待交付候选」，队列 = 「已交给 Pi 但未确认」，
  两者是**两条独立状态**，投影被重建时队列原样保留。
- **并非无条件替换**——in-flight 行与 recent-truncated 行会被**保留**（`packages/herdsman-pi/src/index.ts:976-987`）；
  计数丢失只发生在「行不在服务器列表**且**未被保留」时（这正是 A1 的待收敛目标与来源：R5 观察项 3）。
- 代码锚点：`packages/herdsman-pi/src/index.ts:997-999`（重建投影）；队列条目只在 ack 成功 / 死信屏障 /
  作用域重置三条路径移除。
- 直接后果（也是 Phase 1 笔记 ①(a) 例外的成因）：ack 失败计数落在**投影行**上，投影被重建 ⇒ 计数归零 ⇒
  该事件得到一次「自愈重试」（Phase 1 代码在 `?? 0` 处注明这是**有意**给的一次机会）。

#### A4｜H1 hunk 内的插行约束（**已随 H1 提交解除**）

- 原约束：H1 与 Phase 1 **未提交共存期**内，禁止在 H1 patch 的 hunk #32/#33 的 **post 块内插行**——那会让
  H1 patch 不再能零 fuzz 打到干净 HEAD（或打到后与 H1 树逐字节不一致），破坏 **H1-first 可分离性**。
  （hunk 编号按当时双审条目原样记录；核对时以 patch 内实际序号为准。）
- **现状：H1 已于 `b59ac96` 提交，本约束即解除**；Phase 1 已于 `5433525` 提交，两批不再共存于工作区。
- 若将来再次出现「两批共存」场景，核对手法：
  `git apply --check -p1 <H1 patch>`（git 不做模糊匹配，等价于甚至严于 `--fuzz=0`）
  + 交叉验证 `patch -p1 --dry-run --fuzz=0 <H1 patch>`，再对 Phase 1 文件集做 `cmp` 逐字节比对。

#### A5｜既有缺陷登记：`process-manager` 并发 fork 下误判子进程已退出

- 位置：`src/daemon/process-manager.ts:296-398` 的 `if (!isChildProcessActive(child.pid)) break;`；
  精确点：`:345-347`（注释 + `if` + `break`）、`:357`（SIGKILL 判定，kill 在 `:359`）、`:426`（抛 `formatLockHeldError` 的错误行）。
- 机理：高并发 fork 下 `/proc/<pid>` 会**短暂不可读**，`isChildProcessActive` 据此返回 false ⇒ 误判「子进程已退出」
  ⇒ **SIGKILL 掉刚拿到锁的子进程** ⇒ 上层报 `operation lock is held`（实例锁被误认为被占）。
- 实测特征：失败率 **3–7%**；**50ms 后重试 100% 成功**（**引用自 oracle R5 复核，本批未独立复现**）。
- 生产含义：高负载下 **CLI 连续操作 / daemon 启动可能瞬时虚假失败**（报 `operation lock is held`），**重试即好**；
  不丢锁、不损坏 DB。**与本次两批改动零关系**（H1/Phase 1 都不改 `src/daemon/process-manager.ts`）。
- 本会话的两次观测（分两段，勿混）：
  - **(a) 未落盘的一次**：18:13 / 18:26 的 amend 轮全量跑出现 `1 failed | 816 passed`，**输出未落盘、用例名未捕获**
    （worker 自报；这是核对时段的**已知缺口**）。
  - **(b) 已落盘的一次（15:44，`/tmp/i-test.log`，1538 B；因 `/tmp` 会被清理，关键三行摘录于下）**：
    - 用例名：`clean agent event duplicates helpers > refuses write mode when a lock owner is alive and warns on dry-run only`（文件 `test/unit/clean-agent-event-duplicates.test.ts`）
    - 错误行：`Error: Herdsman daemon operation lock is held: /tmp/herdsman-clean-duplicates-2NEZvC/herdsman.pid.instance.lock. …`
    - 栈帧：`❯ acquireDaemonLock src/daemon/process-manager.ts:426:11`（即上文 `:426` 的 `throw`）
    - 汇总：`Tests 1 failed | 815 passed (816)`（当时基线 816 用例）
  - 两次观测后，该文件单独重跑与后续阶段全量多次均 **817/817 全绿，未复现**。
- 处置：**立项与否待 owner 决定**（建议排在 Phase 2 之后）。本批只登记，不含任何代码改动。
- **更新（2026-09-30，追加式）：本条已修（分支 `fix/daemon-lock-liveness-errno`，本批未提交）**——修法＝ errno 分流（决策依据见 A8），
  `catch` 只在 `ENOENT`/`ESRCH` 时判死，其余 errno 与「读空/截断」一律按活；**未加重试**（理由见 A8③）。
  - **锚点位移（旧→新）**：本批在 `FlockHandleDependencies` 的 JSDoc（+3 行）与判活行注释（+4 行）各加了注释，**净 +7 行**。
    - 区段 `:296-398`（旧）→ `acquireFlockHandle` 现为 **`:336-430`**（该区间在本轮加注释前为 `:329-423`，**执行者自核**；+7 行后即 `:336-430`）。
    - 精确点 `:345-347`（注释 + `if` + `break`）→ 现 **`:376-378`**。
    - `:357`（SIGKILL 判定）→ 现 **`:388`**（进程组 kill 在 **`:390`**、`child.kill` 在 **`:393`**）。
    - `:426`（抛 `formatLockHeldError`）→ 错误文案现 **`:451`**、`throw` 现 **`:457`**。
  - **原引用的字面量已变**：本条正文引用的 `if (!isChildProcessActive(child.pid)) break;` 现为
    **`if (!isChildProcessActive(child.pid, deps?.readProcessStat)) { break; }`**（新增第二个**可选**参数＝测试注入缝，不传时读法不变）。
  - **`/tmp/i-test.log` 已不存在**：本批收尾清理 `/tmp` 时把它一并删除（执行者如实上报）。其**内联三行摘录仍然有效**，
    仍是本条唯一落盘证据；但须注意：**errno 相关证据从未落盘**——(a)(b) 两次观测都未捕获 `isChildProcessActive` 读 `/proc` 失败的具体 errno，
    故「/proc 短暂不可读」的真实 errno 至今**未被实测确认**（证据边界与候选见 A8④）。
  - **本批的回归用例**（`test/unit/daemon-process-manager.test.ts:645-678`，未新开文件、未新开用例）：注入缝下三个分支各一次
    （`EIO` 带 errno / **无 `code`** 的 `Error` / **空读**），每次都断言：① 拿到 handle；② **不带缝再取一次锁必须被拒**
    （`expect(acquireFlockHandle(lockPath)).toBeNull()`——钉住「这个 handle 确实持锁」的双主控回归面）；③ `release()` 后
    `isFlockHeld` 经 `waitForCondition` 转为 `false`。200 轮压力用例顺移至 `:680`，内容零改动。

#### A6｜台账 / 口径说明

- 每条观察项必须写清三件事，缺一不可：**来源轮次**（R1 / R2 / R5 / oracle / reviewer …）、
  **状态**（已落地 / 已解除 / 挂账 / 待 owner 拍板 / Phase 2 前置闸门 / 保持原样）、**核对锚点**
  （`文件:行号` 或可直接粘贴的 `grep` 串）。
- **不重复抄写**：若某条已完全落进既有笔记，本台账只保留一行指针（见「背景」的覆盖关系），正文留在原笔记。
- **只增不改**：状态变化用**追加**一行「更新（YYYY-MM-DD）」表达，不覆写原判断（与 README「不可变原则」一致）。
- **来源缺失必须标注**：条目原文不在执行者手上时，写「摘要，原文不在手」，不得伪装为逐字引用。

### B. oracle **H1/R2 轮**的两条反问结论

> 另两条「oracle 反问结论」（`agent.discarded` 文案 `failed (discarded)` 并入 Phase 2、新 scope 首次 claim 跳过历史＝既有语义）
> 出自 **H1/R1 轮**，见 [`20260930-terminal-event-delivery-h1.md`](20260930-terminal-event-delivery-h1.md)「遗留 / 下一轮」第 **7** 项。
> 两组结论同名不同物，引用时务必带上轮次。

#### B1｜「从未取件的孤儿 failed 行保留到 7d」维持现状

- 结论（**留 Phase 2、owner 拍板**）：维持现状——`deleteSettledOlderThan` 的 **7 天硬上界**即保留上限，**不在 H1 内加短 TTL**（与 H1 笔记遗留第 2 项口径一致）。
- 量级：0–1 条/pane；保留窗口由「首次投递尝试」界定（不是时间），从未被取件的行会一直留到 7d 上界。
- 属 **owner 拍板项**（reviewer 亦标记「保留窗口 TTL 属 owner」）。
- 代码锚点：`src/db/agent-events.ts` 的 `deleteSettledOlderThan`；保留语义在 `#invalidatePaneCore` 步骤 2。
- **已由既有笔记覆盖** → 正文不重复：见 [`20260930-terminal-event-delivery-h1.md`](20260930-terminal-event-delivery-h1.md)「遗留 / 下一轮」第 **2** 项与第 **6** 项。

#### B2｜挂账 chore：`scripts/check-root-package.mjs` 直跑被 pnpm 横幅污染

- 现象：直接跑 `node scripts/check-root-package.mjs` 会 `JSON.parse` 失败——脚本用
  `execFileSync("npm", ["pack", "--dry-run", "--json"], …)` 捕获 stdout，而 stdout **前两行**被混入 pnpm 的横幅
  `Already up to date` / `Done in …ms using pnpm v11.9.0`（来自 `prepack: pnpm build` 阶段），于是 `[` 之前多了非 JSON 文本。
  **版本号随环境变**：当时是 mise 的 pnpm v11.9.0；nvm 环境复现时会打印 `pnpm v11.22.0`——**判定时不要匹配具体版本号**。
- **规范入口是绿的**：`pnpm package:check` → `@dorokuma/herdsman@0.13.1: 233 files`。
- 与 H1/Phase 1 **解耦**：无 `package.json` / lockfile 改动；干净 HEAD 树上同样表现；`git status` 无相关条目。
- **挂账为独立 chore**，两个候选方向：① `npm pack` 传 `--ignore-scripts`（跳过 prepack，但需另行保证 `dist/` 已构建）；
  ② 从 stdout 提取**末段 JSON**（最后一个 `[` 起）再 `JSON.parse`。
- 验收（修好后应同时满足）：`node scripts/check-root-package.mjs` **直跑绿**，且 `pnpm package:check` 仍报 `233 files`。
- 核对：`node scripts/check-root-package.mjs`（当前应失败）、`pnpm package:check`（应绿）。

#### A7｜同类误杀登记：`state === "T"`（停止态）被当作死进程

- 位置：`src/daemon/process-manager.ts:293-324` 的 `isChildProcessActive`，判活表达式
  （`:312`；上方 `:308-311` 已加指向本条目的注释）`return state !== "Z" && state !== "X" && state !== "T";`——**`T` 与 `Z`/`X` 被同样判死**。
- 机理：`T`（`SIGSTOP` / ptrace 停止 / cgroup freezer）只是**被暂停**，内核**仍持有其 flock**；而 `Z`/`X` 是
  已退出、内核已释放 flock。因此判死 `T` 与判死 `Z` 后果不同：前者会让 `acquireFlockHandle` 把**正持锁**的
  停止态子进程 SIGKILL 掉并返回 `null` ⇒ 上层 `acquireDaemonLock` 误报 `operation lock is held`（同一类误杀，
  与 A5 同源，只是触发条件从「瞬时读失败」换成「持锁进程被暂停」）。
- **本批未改（有意；理由是「范围控制 + 缺证据 + 属 owner 决策」，不是等待预算）**：
  ① **范围控制**：本批只做 errno 分流（A8），`T` 属另一类语义变更，混入只会扩大 diff 与复核面。
  ② **无 `T` 态的实测触发证据**：本批与 oracle 窄复审都未观测到「停止态持锁进程」导致的误杀，A5 的 flake 也未定位到 `T`。
  ③ **需先区分两种停止态**：「**自身子进程被停止**」（本工具 spawn 的持锁子进程被 `SIGSTOP`/ptrace 暂停）与
     「**外部持锁者被停止**」（生产 daemon 的锁被暂停进程持有）对**等待语义**的要求不同——等多久、是否 kill、是否 fail-open，
     属**需 owner 拍板的等待语义**，不是纯 bug 修复。
- **（同批草稿的理由已废弃）**：本条目早先写的「`T` 改判活会让争用/释放路径 1000ms/100ms 空转」**站不住**，已删除：
  争用路径的子进程是 `flock -n` **秒退**，呈 `Z`/`ENOENT` 而非 `T`，根本不会进 `T` 分支；且 `SIGKILL` 也能终止停止态进程，
  不存在「无法回收」问题。真理由即上一条的三点。
- 处置建议：**单独立项评估**（与 A5 一起），候选做法是先区分「自己 spawn 的子进程」与「外部持锁者」再决定
  `T` 的处理，不在 errno 分流这一批里顺带改。
- 核对：`grep -n 'state !== "Z"' src/daemon/process-manager.ts`。

#### A8｜决策记录：存活判定采取 **fail-open**（只认 `ENOENT`/`ESRCH` 为死亡证明）

> 触发项目铁律 3（重大决策须记笔记）；本条同时是 A5 修复的决策依据，也是「为何不改 `T`、为何不加重试」的依据。

- **① 为何只认 `ENOENT`/`ESRCH` 判死**：这两个 errno 是**内核直接答复「该 pid 不存在」**的硬证据
  （`/proc/<pid>/stat` 无条目 / `ESRCH`），也是本文件**既有先例**（`readDaemonProcessIdentity` `:203-213` 同样把 `ENOENT`/`ESRCH`
  当「已不存在」、把 `EACCES`/`EPERM` 与其他错误当「无法确认」）。其余 errno（`EACCES`/`EPERM`/`EIO`/未知）只说明**我们读不到**，
  不说明进程不在。对这个用途，两类误判的代价不对称：**误判「死」要比误判「活」贵得多**——
  误判死 ⇒ SIGKILL 掉刚拿到 flock 的子进程 ⇒ 上层误报 `operation lock is held`（A5 的现场）；误判活 ⇒ 最多多等一个窗口。
  故非 `ENOENT`/`ESRCH` 一律 fail-open（按活）；同理 `lastParen === -1`（内容读空/截断、无法解析）也按活。
- **② 残留「失效 handle」面的可达条件与后果**：fail-open 是**保守**而非正确，因此存在一个已知残留面。
  **可达条件（需同时叠加）**：`READY` 已写出（子进程已成功 `flock`）**且**子进程在父进程下一次判定前已**真的死亡**（内核已释放 flock）
  **且**该次 `/proc` 读取返回**非 `ENOENT`/`ESRCH`** 的失败、或读空/截断（或 pid 恰好被复用）。
  此时父进程会把「已死」当「活」，返回一个**不持锁的 handle**（失效 handle）。
  **后果**：① **双主控风险**——上层以为已独占实例锁，但内核锁实际已释放，另一个进程可以同时拿到 ⇒「两个实例同时跑」（与 A5 同一类后果，方向相反）；
  ② SQLite 自身的文件锁会兜底，**不会静默损坏 DB**，但会表现为**重复投递 / 写争用 / 报错**。
  该面与 A5 同源（都是 `/proc` 读与真实状态不同步），只是 A5 在当时误判死、这里是之后误判活。
- **③ 本批为何**不加重试**（包括「对 `ENOENT` 无脑重试」）**：争用路径的正确性**依赖 `:376-378` 的快速 `break`**——
  他人持锁时 `flock -n` 子进程秒退，父进程必须立刻跳出并返回 `null`；给 `ENOENT` 加重试会把这条路径拖到 1000ms 空转，
  把「立即失败」变成「一秒后失败」，并直接威胁 `test/unit/daemon-process-manager.test.ts:680` 的 200 轮压力用例
  （该用例只有 vitest 默认 5s 超时，本身已是已知 flake，见 `20260929-daemon-shutdown-budget.md:50`）。
  fail-open 与「不加重试」是配套的：前者已经替代了后者想解决的问题。
- **④ oracle 窄复审的实测证据（本批未独立复现，随 oracle 报告引入）**：
  - `st_size(/proc/<pid>/stat) = 0` **成立** ⇒ 「读空/截断」在原理上可能（procfs 不给 size 提示）。
  - 但 **合计约 95 万次读中 0 次空读/截断**（oracle 报告口径：node22 phase2 的 466 360 次 + node26 的 453 680 次 + 30 000 次 self 读，另含 2 400 个新 spawn 的子进程；**其中仅 node22 phase2 即 46 万+**）⇒ `lastParen === -1 → 按活` 是**防御分支**，**不是本批主修法**；主修法是 errno 分流（①）。
  - **活子进程读到 `ENOENT` 语义上不可能**（pid 存在则条目存在）⇒「原始 flake 的真实 errno 是 `ENOENT`」这个解释不成立；
    更像的候选是 fork 风暴下的资源类 errno（`EMFILE`/`ENOMEM`）——二者都落在 fail-open 分支里。
  - **证据边界**：以上为 oracle 复核实测；执行者侧**没有任何 errno 落盘证据**（A5(a)/(b) 两次观测都未捕获 errno，见 A5「更新」段）。
- **⑤ 未来可观测性建议（本批不做）**：在 `catch` 里按**限频**（每 pid / 每 N 秒一次）`console.debug` 记下 `errno` 与 pid，
  用生产数据验证「修复命中」与真实 errno 分布，再决定是否需要 ③ 之外的等待策略。
  本批**不引入任何日志/指标**（保持零行为变更）——oracle 列为**未来项**、未要求本批落地。
- 核对：`sed -n '313,320p' src/daemon/process-manager.ts`；`grep -n 'ENOENT' src/daemon/process-manager.ts`。

### C. 批次边界与前置（便于半年后回溯）

- 共同基线：**`7c4a36b`**（`Merge branch 'fix/daemon-graceful-shutdown-budget'`）；分支 `fix/delivery-latency-phase1`。
- **`b59ac96` = H1**：10 个源/测试文件 + [`20260930-terminal-event-delivery-h1.md`](20260930-terminal-event-delivery-h1.md)，
  合计 11 项 `+1041 / −144`。内容：孤儿终态行、`agent.failed` 保留窗口、`agent.discarded` 唤醒、stitch 代际匹配、
  F1（跨代关页不吞未投递孤儿 failed 行）与一条回归用例。
  双审：**R1 放行 + oracle 1 条应修（F1）→ F1 修复（与 oracle 最小补丁逐字符等价）→ R2 两关放行**。
- **`5433525` = Phase 1**：7 个源/测试文件 + [`20260930-phase1-delivery-latency.md`](20260930-phase1-delivery-latency.md)，
  合计 8 项 `+1037 / −146`。内容：检测段去指数退避、`turn.confirmed` 直通、degraded 不进重试环、投递段有界化、
  批次并入、忙碌路径不 abort 用户 turn、`degradedReason` 日志档、ack 计数取活值 + 死信出队。
  双审：**R5 两关放行**（自 R5 之后这 7 个文件未再被改过）。
- **amend 前哈希**（追溯用，工作树不变）：H1 `233b106`（message-only amend）；Phase 1 `34763e6`（正文条数 8+1 → 7）。
- 文件集重叠：两批**共用 4 个文件**——`packages/herdsman-pi/src/wake.ts`、`src/observability/agent-index-service.ts`、
  `test/integration/agent-index-service.test.ts`、`test/unit/herdsman-pi-wake.test.ts`；
  H1 独有 6 个、Phase 1 独有 3 个（`packages/herdsman-pi/src/index.ts`、`test/integration/turn-completion-signal.test.ts`、
  `test/unit/herdsman-pi-extension.test.ts`）。
- 提交顺序约束（H1 在前、Phase 1 在后）的**原因**保持有效：Phase 1 的集成断言以 H1 的行为为前置。
  该约束的文本前提（「两批都未提交」）已由本节取代。
- **本批（纯文档）**：只改 `.agents/notes/**`，**零代码改动**，且**未提交**。
- 未跟踪的观察/取证笔记 `20260930-terminal-event-delivery-latency-observation.md`（写作时点 10:51）**与本批一并提交**。
  注记（防误读）：该笔记里「`packages/herdsman-pi/src/wake.ts` 处于未提交修改状态」是**写作时点的事实**，
  在 H1 于 `b59ac96` 提交后**已过时**——历史笔记不改写，`wake.ts` 的 H1 部分自 `b59ac96` 起已进 `HEAD`。

### D. 本批（daemon 操作锁 release 屏障）残余不变量台账

> **追加小节**（2026-10-01，`fix/daemon-lock-release-barrier` 批次提交前收口轮；来源轮次＝「实现轮 → 双审 → 收口轮」）。
> 主体决策、机理与实测见 [`20260930-daemon-lock-release-barrier.md`](20260930-daemon-lock-release-barrier.md)——该笔记第 51 行那段已由本轮的收口重写（去掉证据不足的定量归因、补上本轮一手延迟数据）。
> 口径：只登记**本批未消除**的残余项；「现有兜底」＝本批已落地的机制，不是未来计划；数字凡属**本轮一手实测**均标 harness 与 n/臂，凡引自他轮的写明来源。**只增不改**，不与 A/B/C 各节重复（A7/A8 只留指针）。
> **追加（2026-10-01，`chore/stress-test-timeout` 文档轮）**：D7 在本轮收口——「已修」限定为**「仅该用例」**、取值依据改用**本条 chore 一手实测**（并删去一处悬空引用），`/tmp` 锚点改为**可复跑形状**；同时追加 **D9–D12** 四条同类观察项（来源＝oracle 文档轮指出）。**D9–D12 均未修：D9/D10 已立项、D11 已决定不保留、D12 挂账（台账卫生）。**

| # | 残余项 | 来源轮次 | 状态 | 核对锚点 |
| --- | --- | --- | --- | --- |
| D1 | `children` 文件不可用 → release 屏障降级为只盯 flock 进程，可能带锁返回 | 收口轮（本批）；残余率引自收口轮测量 | **接受**（已加每进程一次告警；残余由 acquire 重试兜底） | `src/daemon/process-manager.ts:312-333`（读 `:312`、告警 `:326`） |
| D2 | release 的 100ms 预算耗尽时仍可能带锁返回 | 本批（A｜release 屏障） | **接受**（best-effort 预算，设计取舍） | `src/daemon/process-manager.ts:574` |
| D3 | acquire 硬上界 ≈**1000ms+ε**；deadline 耗尽后即使锁已空闲也返回失败 | 本批（B｜有界重试）＋收口轮实测 | **接受**（预算语义＝设计取舍） | `src/daemon/process-manager.ts:371`、`:452`、`:515` |
| D4 | 无 `owner.json` 的外来持锁最多 4 次 spawn 后报 held；失败延迟 p50 ~2ms → ~11ms（有界） | 本批（B）；本轮一手重测 | **接受**（有界） | `src/daemon/process-manager.ts:391`；harness `/tmp/lockrace-docs/measure.ts` |
| D5 | owner 闸门依赖 `isProcessRunning`（`kill(pid,0)`，EPERM 也算活）→ 僵尸 / pid 复用的 `owner.json` 会闸掉重试 | 本批（B｜重试闸门） | **接受**（退化为旧行为，不产生新错误） | `src/daemon/process-manager.ts:401-413`、`:216-223` |
| D6 | `isChildProcessActive` 把 `T/Z/X` 都视为已退出 → helper 被 `SIGSTOP` 时 release 提前返回 | 既有（A7 同源） | **挂账**（待 owner 拍板等待语义；本批只做 errno 分流） | `src/daemon/process-manager.ts:353`；见 A7 |
| D7 | 200 轮压测在**并行复跑**下逼近 5s vitest 超时（CI 单进程无风险） | 收口轮 + 本条 chore 重测 | **已修（本条 chore，仅该用例）**：`test/unit/daemon-process-manager.test.ts:1011` 给该用例加显式超时 `30_000`（依据：3 路并发全量**一手实测 6.3–6.7s** >5s）；**只抬了这一条用例**，同类未解见 D9/D10 | 可复跑形状见下方 D7「核对」段（`pnpm vitest run --reporter=verbose`，单进程 / 2 路 / 3 路并发）；本批笔记第 51 行 |
| D8 | D1 的告警分支**无自动化用例**（覆盖靠实验验证 + 审计） | 收口轮（本批） | **挂账**（要钉需新增缝或 `vi.mock("node:fs")`） | `src/daemon/process-manager.ts:326`；`test/unit/daemon-process-manager.test.ts` 内无 `readChildPids` 引用 |
| D9 | `test/unit/daemon-service.test.ts:523`（真 spawn daemon 的重测试）在 **3 路并发 + 高负载（load 21–34）**下 **3/3** 超时；同形状 **load 12–17** 的 3 路运行 **0/6**——与 D7 **同类** | 本条 chore 文档轮（取证引自 oracle 文档轮） | **已立项**（owner 2026-10-01 决定：把“本地并行复跑假失败”这一类一起收拾；D7 已修，D9/D10 与其同类，另开批次处理） | `test/unit/daemon-service.test.ts:523`；复跑形状同 D7「核对」段 |
| D10 | `test/integration/orchestrator-disconnect-grace.test.ts:306` 高载 3 路并发下 **1/3** 断言失败（**非超时**） | 本条 chore 文档轮（取证引自 oracle 文档轮） | **已立项**（owner 2026-10-01 决定：把“本地并行复跑假失败”这一类一起收拾；D7 已修，D9/D10 与其同类，另开批次处理） | `test/integration/orchestrator-disconnect-grace.test.ts:303-307`；复跑形状同 D7「核对」段 |
| D11 | 该压测用例**已无性能探测力**（`30_000` ≈ 单跑耗时的 21×） | 本条 chore 文档轮（取证引自 oracle 文档轮） | **已决定：不保留**（owner 2026-10-01：该用例只做并发正确性，不设性能门限；若将来要性能信号，另开不受并行负载影响的形状） | `test/unit/daemon-process-manager.test.ts:1011` |
| D12 | 台账引用数字**不可复算** + `/tmp` 锚点**全部失效**（全台账通病，非本条引入） | 本条 chore 文档轮 | **挂账（台账卫生）** | `grep -rn "/tmp/" .agents/notes/`；本轮只处理了 D7 一条 |

#### D1｜`children` 文件不可用 → release 屏障降级为只盯 flock 进程

- **现象**：`readChildPids` 读 `/proc/<pid>/task/<pid>/children` 失败时按约定返回 `[]`，于是 `helperPids` 只剩 flock 父进程；release 可能在 fd 共享者（`sh -c` 那段命令进程）仍活着时就返回，锁还在自己一侧多持几 ms。
- **触发条件**：非 Linux，或内核未开 `CONFIG_PROC_CHILDREN` / procfs 被精简、该文件不可读。**本机不可达**（本机 /proc 有 children），只能靠注入缝构造。
- **影响**：release → 极小间隔 → acquire 的那一发可能 `EWOULDBLOCK`（即本批要修的现象复现）；不产生双主控（只是提前返回，锁仍在持有者手里）。
- **现有兜底**：① acquire 侧 4 次有界重试（1ms 间隔，共用同一 1000ms 窗口）；② 每进程一次的 `[herdsman]` 告警（模块级 `warnedChildPidsUnavailable`，只在 `/proc` 存在时打），让「退化」可见而不是静默。
- **状态**：**接受**（设计取舍）。残余实测 **1/6000（回退路径 + B）**——数字引自收口轮测量（本批笔记正文），**本批收口未独立复现**；该分支无自动化用例（见 D8）。
- **核对**：`grep -n "warnedChildPidsUnavailable\|readChildPids" src/daemon/process-manager.ts`。

#### D2｜release 的 100ms 预算耗尽时仍可能带锁返回

- **现象**：release 的收尾是 best-effort 同步自旋，`deadline = Date.now() + 100` 到期即返回，不报错、不重试。
- **触发条件**：helper 一侧的 fd 持有者在极端调度延迟下 >100ms 才退出（或那 100ms 被同进程的事件循环阻塞吃掉——同步自旋本身就阻塞事件循环）。
- **影响**：与 D1 同形状：release 返回时锁可能仍被自己一侧持有，紧接着的 acquire 首发生败。
- **现有兜底**：acquire 有界重试（B）；release 的 100ms 预算**不涨**（关停路径预算见 `.agents/notes/20260929-daemon-shutdown-budget.md`）。
- **状态**：**接受**（best-effort 预算，设计取舍）；本轮一手 release 延迟（harness 同上，n=200/臂）free 形状 p50 两臂均 0.94ms、max ≤3.94ms，未见预算被打满。
- **核对**：`grep -n "Date.now() + 100" src/daemon/process-manager.ts`。

#### D3｜acquire 硬上界 ≈1000ms+ε；deadline 耗尽后即使锁已空闲也返回失败

- **现象**：`deadline = Date.now() + ACQUIRE_WINDOW_MS(1000)`；attempt 之间与 `spawnFlockHelper` 内部窗口都按它钳制（`Math.min(ACQUIRE_WINDOW_MS, deadline - Date.now())`）；用尽即 `return null`，上层 `acquireDaemonLock` 抛 `operation lock is held`。
- **触发条件**：单次 acquire 被外部因素拉满窗口（例如 helper 不写 READY 且一直持着锁、4 次 attempt 用尽）。
- **影响**：可能在「锁其实已经空闲」时返回失败；诊断信息通常不带 ` by PID `（`owner.json` 已消失）。这是预算语义，不是漏检。
- **现有兜底**：无自动重试（有意：窗口内拿不到就失败）；调用方 50ms 后重试即成功（本批早前实测，见本批笔记「背景」）。
- **状态**：**接受**（预算语义）。上界实测（病理构造，收口轮）：收紧前 1958/1959/1958ms → 收紧后 **1000/1000/1000ms**（`/tmp/lockrace-close2/item2/run.sh`）。
- **核对**：`grep -n "ACQUIRE_WINDOW_MS\|deadline" src/daemon/process-manager.ts`。

#### D4｜无 `owner.json` 的外来持锁：最多 4 次 spawn 后失败

- **现象**：外来进程真持锁、但没有（或已被删除）`owner.json` 时，`hasLiveLockOwner` 判 false → 闸门放行 → 同一窗口内最多 4 次 spawn helper。
- **触发条件**：持锁者不是本工具登记的 owner（无 `owner.json`），且持续持锁（含「上一任持有者正在拆除」的残余窗口）。
- **影响**：该路径失败延迟从「一次 spawn」变成「≤4 次 spawn + 3×1ms」；**有界**，不会无限等。
- **现有兜底**：`ACQUIRE_MAX_ATTEMPTS = 4` 与 1000ms 窗口双重有界；`SIGKILL` 组 + ack 清理沿用旧结构。
- **状态**：**接受**（有界）。**本轮一手实测**（harness `/tmp/lockrace-docs/measure.ts`，node v22.23.1，两臂同轮、顺序对调各 1 轮，n=25/臂/轮 = **50/臂**）：HEAD `ce17071` p50 1.99–2.13ms / max ≤2.78ms → 现行 p50 11.03–11.06ms / max ≤11.70ms。（早前轮次曾记 max 61ms，本机本轮未复现，本表以本轮实测为准。）
- **核对**：`grep -n "ACQUIRE_MAX_ATTEMPTS" src/daemon/process-manager.ts`。

#### D5｜owner 闸门依赖 `isProcessRunning`（EPERM 也算活）

- **现象**：`hasLiveLockOwner` 用 `isProcessRunning(pid)`（即 `kill(pid, 0)`；`EPERM` 返回 true）判活；僵尸进程、pid 被复用、或「存在但无权限」都会被判成「活 owner」，于是**一次都不重试**。
- **触发条件**：`owner.json` 指向僵尸 / 已被复用的 pid。
- **影响**：只丢掉了「本来可能成功的那次重试」，行为等同修复前（一次 spawn 后失败）；不引入新错误、不改错误文案。
- **现有兜底**：无（也不需要，语义与修复前一致）；调用方重试即好。
- **状态**：**接受**（闸门的已知保守性）。
- **核对**：`grep -n "hasLiveLockOwner\|isProcessRunning" src/daemon/process-manager.ts`。

#### D6｜`isChildProcessActive` 把 `T` 与 `Z/X` 同样判死

- **现象**：判活表达式是 `state !== "Z" && state !== "X" && state !== "T"`；`T`（`SIGSTOP` / ptrace / cgroup freezer）**内核仍持锁**，却被判死。
- **触发条件**：helper（或外部持锁者）在 release / acquire 判定窗口内被停止。
- **影响**：release 侧提前放行 → 同 D1 形状（可能带锁返回）；acquire 侧同一判活也会把停止态 helper 判死（可 `SIGKILL` 掉正在持锁的子进程并返回 null）→ 上层误报 `operation lock is held`（与 A7 同源）。
- **现有兜底**：本批只做 errno 分流，`T` 语义未动；acquire 有界重试部分兜底。
- **状态**：**挂账**（＝ A7，待 owner 拍板「等待语义」；不属本批范围）。
- **核对**：`grep -n 'state !== "Z"' src/daemon/process-manager.ts`；A7。

#### D7｜200 轮压测在并行复跑下逼近 5s vitest 超时

- **现象**：`stress test: 200 rounds of simultaneous sub-millisecond lock contention yields zero double-masters` 单跑约 **1.33s**（**本条 chore 一手热跑 1316/1325/1351ms**），但并行复跑会越过 vitest 默认 5s（3 路并发全量一手实测 6.3–6.7s）。
- **触发条件**：同一机器同时跑多份全量测试（本机复核 / 本地复跑场景）；**CI 单进程不触发**。
- **影响**：本地并行复跑时该用例假失败（`Test timed out in 5000ms`），非产品缺陷；本批使它变慢约 +320~350ms（本轮交错实测，见本批笔记第 51 行）。
- **现有兜底**：无（测试侧）。
- **状态**：**已修（本条 chore，仅该用例）**——`test/unit/daemon-process-manager.test.ts:1011` 给该用例加显式超时 `30_000`（`}, 30_000)` 第三参形态，与本仓重测试既有写法一致）。
  - **范围限定（重要）**：本条 chore **只抬了这一条用例**的超时，**并未**让「并行复跑全绿」成立——本地并行复跑的假失败**不止这一条**（同类未解项见 **D9 / D10**，D7 只是第一个被收口的）。
  - **取值依据（均为本条 chore 一手实测）**：单跑 ~**1.33s**（热跑 1316/1325/1351ms）；**2 路**并发全量 **4.0–4.4s**（另一轮负载更重时曾到 **10.8–11.2s**，负载档划分见 D9）；**3 路**并发全量 **6.3–6.7s**（>5s ⇒ 默认 5s 超时下**必撞** `Test timed out in 5000ms`）。故取 30s，给最坏形状（3 路 6.7s）约 **4.5×** 余量；`vi.waitFor` 版不再需要。不阻塞本批。
- **核对（可复跑形状；原 `/tmp/lockrace-final/**` 取证已随 `/tmp` 清理失效——`/tmp/lockrace-final` 现不存在），命令形状与对齐口径源自本批笔记第 51 行**：
  - **命令**：`pnpm vitest run --reporter=verbose`；**看该用例那一行的耗时字段**（用例名 `stress test: 200 rounds of simultaneous sub-millisecond lock contention yields zero double-masters`，**行尾裸耗时（如 `1415ms`）**）；**失败只认** `Test timed out in 5000ms`（默认超时被打穿的签名）。
  - **单进程**：**1 个进程**只跑该文件——`pnpm vitest run test/unit/daemon-process-manager.test.ts --reporter=verbose`。
  - **2 路并发**：**2 个进程**同时**各跑全量**（`pnpm vitest run --reporter=verbose`；为免先被 5s 打断可加 `--testTimeout=60000`）。
  - **3 路并发**：**3 个进程**同时**各跑全量**，同上（保持默认 5s，才看得到 `Test timed out in 5000ms`；**但当前树上有前提**：该用例已在 `test/unit/daemon-process-manager.test.ts:1011` 带了第三参超时 `30_000`（本 chore 所加），要复现上面这个签名**须先移除这条显式超时**——不移除的话，该签名不会再从这条用例冒出来（其它未收口的同类用例见 D9））。
  - **负载档**：跑前**记下当时的 1 分钟 loadavg**（`uptime`）再判定；本批观察到的有意义分档是 load **12–17**（中）与 **21–34**（高，见 D9），**同档才横向可比**（跨档绝对值会被后台负载污染）。
- **原始取证（已失效，仅留出处）**：`/tmp/lockrace-final/concurrent3way/summary.txt`、`/tmp/lockrace-final/concurrent-default5s/summary.txt`、`/tmp/lockrace-final/single/summary.txt`——`/tmp` 会被清理，故本条目一律以**可复跑形状**（上）为准。

#### D8｜D1 的告警分支无自动化用例

- **现象**：`readChildPids` 的 catch + 一次性 `console.warn` 分支没有测试覆盖；既有回归用例都走「procfs 正常」路径（`readChildPids` 未导出、测试文件里零引用）。
- **触发条件**：只有在改这段代码或做审计时才暴露——回归不会被测试拦住。
- **影响**：将来若改坏该告警 / 回退语义，测试不会失败（回归面缺口）。
- **现有兜底**：收口轮的实验验证（`/tmp/lockrace-close2/warnprobe/` 注入缝副本）+ 审计复核。
- **状态**：**挂账**（可选；要钉需新增注入缝或 `vi.mock("node:fs")`，本批未做）。
- **核对**：`grep -n "console.warn" src/daemon/process-manager.ts`；`grep -n "readChildPids" test/unit/daemon-process-manager.test.ts`（应为空）。

#### D9｜真 spawn daemon 的重测试在高负载并行复跑下同样超时（与 D7 同类）

- **现象**：`test/unit/daemon-service.test.ts:523`（`a second daemon on the same HERDSMAN_HOME fails on the instance lock; SIGTERM releases it and allows restart`，**会真 spawn daemon** 的重测试）在 **3 路并发 + 高负载**下 **3/3** 记到 `Test timed out in 5000ms`。
- **触发条件**：同一机器上 3 个进程各跑全量，且后台负载处于高挡（实测 **load 21–34**）；同形状在 **load 12–17** 的 3 路运行 **0/6**——即「多进程 + 高负载」同时满足才触发。
- **影响**：本地并行复跑时该用例假失败（`Test timed out in 5000ms`），**非产品缺陷**。与 D7 **同类**：本地并行复跑的假失败**不止 D7 这一条用例**，D7 只是第一个被抬超时收口的。
- **现有兜底**：无（本条 chore 只给 D7 那一条用例抬了超时，**未覆盖本条**）。
- **状态**：**已立项**（owner 2026-10-01 决定：把“本地并行复跑假失败”这一类一起收拾；D7 已修，D9/D10 与其同类，另开批次处理）。
- **核对**：`sed -n '523p' test/unit/daemon-service.test.ts`；复跑形状同 **D7「核对」段**（高负载、3 路并发全量、`--reporter=verbose`，看 `Test timed out in 5000ms`）。

#### D10｜断连宽限集成用例在高负载并行复跑下断言被打穿（非超时）

- **现象**：`test/integration/orchestrator-disconnect-grace.test.ts:306` 在高载 3 路并发下 **1/3** 出现 `AssertionError: expected { paneId: 'wB:p-owner', … } to be null`——**非超时**，失败点是 `:304-306` 的 `expect(orchestrator.status({…})?.owner).toBeNull()`。
- **触发条件**：高负载 + 3 路并发全量复跑；该断言依赖 `scheduler.advance(60)` 之后的 tick/时序假设。
- **影响**：断言对 tick/时序的假设被负载打穿 ⇒ 本地并行复跑出现**假失败**（同类噪声，非产品缺陷）；因**不是超时**，抬高该用例超时**解决不了**，须改断言形状（如把对 tick/时序的假设改成可等待条件）。
- **现有兜底**：无。
- **状态**：**已立项**（owner 2026-10-01 决定：把“本地并行复跑假失败”这一类一起收拾；D7 已修，D9/D10 与其同类，另开批次处理）。
- **核对**：`sed -n '303,307p' test/integration/orchestrator-disconnect-grace.test.ts`；复跑形状同 **D7「核对」段**（高负载、3 路并发全量，看该行的 `AssertionError`）。

#### D11｜该压测用例已不再有性能探测力

- **现象**：`test/unit/daemon-process-manager.test.ts:1011` 的显式超时 `30_000` ≈ **单跑耗时的 21×**（单跑 ~1.3–1.4s）——这个阈值实际只兜「不挂死」，**探测不到** D7 记录的 +320~350ms 级变慢。
- **触发条件**：任何想靠本用例回看性能回归的时刻。
- **影响**：本用例**不再是性能信号**。**不能**简单下调阈值换探测力：`10_000` 这一档已被 oracle 实测证伪（在该档下并行复跑仍会假失败）。
- **现有兜底**：无（本用例现在只保「200 轮无双主控」的**正确性**语义）。
- **状态**：**已决定：不保留**（owner 2026-10-01：该用例只做并发正确性，不设性能门限；若将来要性能信号，另开不受并行负载影响的形状）。
- **核对**：`sed -n '1010,1011p' test/unit/daemon-process-manager.test.ts`（`}, 30_000);`）。

#### D12｜台账卫生：引用数字不可复算、`/tmp` 锚点全部失效（全台账通病）

- **现象**：本条台账（A/B/C/D 各节）大量引用 `/tmp/**` 取证路径与其内数字，但 `/tmp` 会被清理（**本会话已发生一次 `/tmp` 目录整体消失**，`/tmp/lockrace-final` 现已不存在，见 D7 核对段）；且**部分引用数字在台账内查无对应记录、不可复算**——本文件历史里出现过引用「只存在于**未提交版本**中的数字」的例子（写作时点那个数字不在任何已提交版本里，本轮已删除），后来的读者无从核对。**通则**：引用台账数字前，先确认它在**已提交内容**里可查（如 `git log -S<数字>` / `git show <rev>:<文件>`），查不到就不得拿来当核对锚点。
- **触发条件**：任何后来的核对者想按锚点复核时。
- **影响**：锚点失效 ⇒ 台账条目**无法自证**。**属全台账通病，非本条 chore 引入**（本轮只把 D7 一条换成了可复跑形状）。
- **现有兜底**：无（只能靠形状复跑，不能靠路径取证）。
- **状态**：**挂账（台账卫生）**——待统一整改：把 `/tmp` 路径锚点改为**「可复跑形状」**（命令 + 进程数 + 负载档 + 判定字段）与**仓库内**路径/命令。
- **核对**：`grep -rn "/tmp/" .agents/notes/`（列出全部待整改锚点；本轮只处理了 D7）。

## 被放弃的方案（必填）

- **把这些观察项写进 Phase 1 / H1 笔记正文**：违反 README「不可变原则」（历史笔记原则上不改写），
  且本批内容**跨轮次**（R1/R2/R5）、**跨批次**（H1 / Phase 1 / 独立 chore）、**跨模块**（observability / herdsman-pi / daemon），
  塞进任何单篇都会造成事实重复与「以偏概全」的误导。→ 改为**独立台账 + 既有笔记各加一行指针**。
- **就地改掉 A1 的过强措辞（改笔记正文 / 改代码 JSDoc）**：本批禁止改代码；笔记正文只允许**追加**带日期的更新行。
  故 A1/A3 只登记「正确说法」与待收敛动作，不覆写历史文本。
- **顺手修 B2 的脚本（加 `--ignore-scripts` 或改 stdout 提取）**：本批硬性边界禁止改脚本/配置，
  且该 chore 与两批改动解耦，应独立立项、独立双审。
- **给 A5 的 `process-manager` 缺陷直接立项**：owner 未拍板（建议排 Phase 2 之后），本批只登记机理/特征/生产含义，
  避免在无 owner 决策的情况下把既有缺陷并入本批次范围。

## 来源

- 双审条目：**R1（H1 首轮，oracle 在该轮给出 F1「应修」裁定）→ F1 修复 → R2（增量复核）**；
  **R5**（Phase 1 两关；该轮 oracle 复核同时给出 A1 的「2 处同栈可取 reason」修正与 A5 的 3–7% / 50ms 数字）。
  **本批执行者手上只有摘要**（逐条已标注「摘要，原文不在手」），原文以双审派发件为准。
- **锁修复批次（`fix/daemon-lock-liveness-errno`，未提交）**：轮次＝ **R5（A5 登记）→ worker 实现轮（errno 分流＋注入缝＋1 条回归用例）
  → oracle 窄复审（4 条应修＋同批建议：删 A1 计数、改注入缝 JSDoc、A5 锚点位移、A1 off-by-one、新增 A8、A7 理由改写、加强回归用例）
  → 收口轮（即本次改动）**。本批执行者手上**没有** oracle 原文，各条按派发摘要转述；**A8④ 的实测数字（合计约 95 万次读 / 0 次空读）
  引自该轮报告，本批未独立复现**。
- 本会话实测：`node scripts/check-root-package.mjs` 直跑失败（pnpm 横幅污染 stdout）而 `pnpm package:check` 绿；
  `test/unit/clean-agent-event-duplicates.test.ts` 的锁用例 flake **两次**（15:44 一次已落盘、关键三行摘录于 A5(b)；
  18:13 / 18:26 一次输出未落盘，见 A5(a)）。
- 相关笔记（相对链接）：[`20260930-terminal-event-delivery-h1.md`](20260930-terminal-event-delivery-h1.md)、
  [`20260930-phase1-delivery-latency.md`](20260930-phase1-delivery-latency.md)、
  [`20260930-terminal-event-delivery-latency-observation.md`](20260930-terminal-event-delivery-latency-observation.md)、
  [`wake-delivery-r4f1-remaining-risks.md`](wake-delivery-r4f1-remaining-risks.md)。
- 提交：`b59ac96`（H1）、`5433525`（Phase 1）；共同基线 `7c4a36b`。
- 索引：本笔记加入后运行 `scripts/notes-index.sh` 刷新 `INDEX.md`（该文件在 `.gitignore` 内，不提交）。

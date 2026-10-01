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
| A7 | `isChildProcessActive` 把 `state === "T"`（SIGSTOP 等停止态、内核仍持 lock）判死，属 A5 同类误杀 | 本批（lock 存活判定 errno 分流修复） | **挂账**（本批未改，待 owner 拍板等待语义）→ **已修（2026-10-01，`fix/child-process-active-t-state`；owner 两次拍板：一次 S1（`T` 恒算活）→ 同日二次改为「精细版」：pre-READY 的 `T` 可放弃 / READY 之后 `T` 算活，详见 D6）**；正文两处前提（ptrace / cgroup freezer）已订正，见本条「更新」段 | `src/daemon/process-manager.ts:293-328`（判活行 `:312` 的 `state !== "Z" && state !== "X" && state !== "T"`）；见 A7 正文 |
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
  该进程已退出、**它自己**的 fd 已关闭（**订正 2026-10-01**：这不等于锁一定空闲——`flock` 挂在**内核 open file description** 上，**继承该 fd 的子孙**〔如 `setsid` 逃逸者〕仍可能持锁，见 D6 段的 **D1/D2 例外**）。因此判死 `T` 与判死 `Z` 后果不同：前者会让 `acquireFlockHandle` 把**正持锁**的
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
- **更新（2026-10-01，`fix/child-process-active-t-state`；owner 定稿）**：本条的 `T` 误杀**已修**——owner 一次拍板 **S1（`T` 恒算活）**、同日**二次改为「精细版」（pre-READY 的 `T` 可放弃 / READY 之后 `T` 算活）**，原 S1 被其 pre-READY 代价面推翻（修法、机理、三形状实测、FAIL→PASS 取证、未修/残留面全部落在 D6 详情段，本条不重复；表格行状态由「挂账」改「已修」）。
  - **订正①（重要，如实写）：ptrace 不是 `T`**。**本批执行者一手复核**（`bash /tmp/scout-d6/ptrace.sh`，本机 `ptrace_scope=0`）实测：`before: S` → `strace -p` 附着后 tracee 仍是 **`S`**（脚本那行 echo 标作「`t` = tracing stop」，**该标注不准确**，实测不是 `T` 也不是 `t`）→ **在被 trace 的状态下**再 `kill -STOP` 得到 **小写 `t`**（`tracing+SIGSTOP: state=t`）。而原表达式（只排除 `Z`/`X`/`T`）下小写 `t` **本来就已算活**。⇒ 本条正文把「ptrace 停止」列为 `T` 的触发来源**不成立**；ptrace 这条路径**从来不在误杀面上**。
  - **订正②（重要，如实写）：cgroup freezer 很可能不是 `T`，且本机不可验证**。本机 `/sys/fs/cgroup` 下**无 `cgroup.freeze`**（`cgroup.controllers` = `cpuset cpu io memory hugetlb pids rdma misc`，**无 `freezer`**，无 v2 freezer 可写面）⇒ **无法在本机复现或验证**「freezer 会让进程呈 `T`」。⇒ 记为**证据缺口**：正文把「cgroup freezer」列为 `T` 的来源**未经验证**，不得据此下结论。（本行 `ls /sys/fs/cgroup` 为本批一手核对。）
  - ⇒ 本条**唯一被本轮一手证实的 `T` 来源是 `SIGSTOP`/`SIGTSTP`**（探针：对停止态持锁者跑 `flock -x -n` ⇒ `exit=1`，即内核仍持锁）。
  - **本条处置建议③（「自身子进程 vs 外部持锁者」的等待语义）已由 owner 二次拍板的「精细版」收敛**：`T` 算活仍对**外部持锁者面全量生效**（`isFlockHeld` / `hasLiveLockOwner` 不受 phase 影响），而本工具 spawn 的 helper 仅**在 READY 之后**按「`T` 算活」处理，**pre-READY 阶段显式豁免为「可放弃」**（理由与实测见 D6 详情段「更新②」）；残余面见 D6 详情段「新增残留面」与 D1/D2。
  - **核对**：`bash /tmp/scout-d6/ptrace.sh`（形状；`/tmp` 会被清理）；`ls /sys/fs/cgroup`（应无 `cgroup.freeze`、`cgroup.controllers` 无 `freezer`）；`grep -n 'SIGSTOP-paused' test/unit/daemon-process-manager.test.ts`。

#### A8｜决策记录：存活判定采取 **fail-open**（只认 `ENOENT`/`ESRCH` 为死亡证明）

> 触发项目铁律 3（重大决策须记笔记）；本条同时是 A5 修复的决策依据，也是「为何不改 `T`、为何不加重试」的依据。

- **① 为何只认 `ENOENT`/`ESRCH` 判死**：这两个 errno 是**内核直接答复「该 pid 不存在」**的硬证据
  （`/proc/<pid>/stat` 无条目 / `ESRCH`），也是本文件**既有先例**（`readDaemonProcessIdentity` `:203-213` 同样把 `ENOENT`/`ESRCH`
  当「已不存在」、把 `EACCES`/`EPERM` 与其他错误当「无法确认」）。其余 errno（`EACCES`/`EPERM`/`EIO`/未知）只说明**我们读不到**，
  不说明进程不在。对这个用途，两类误判的代价不对称：**误判「死」要比误判「活」贵得多**——
  误判死 ⇒ SIGKILL 掉刚拿到 flock 的子进程 ⇒ 上层误报 `operation lock is held`（A5 的现场）；误判活 ⇒ 最多多等一个窗口。
  故非 `ENOENT`/`ESRCH` 一律 fail-open（按活）；同理 `lastParen === -1`（内容读空/截断、无法解析）也按活。
- **② 残留「失效 handle」面的可达条件与后果**：fail-open 是**保守**而非正确，因此存在一个已知残留面。
  **可达条件（需同时叠加）**：`READY` 已写出（子进程已成功 `flock`）**且**子进程在父进程下一次判定前已**真的死亡**（该进程自己的 fd 已关闭；**订正 2026-10-01**：若存在继承该 fd 的子孙，锁不会被释放、本条件不成立，见 D6 段的 **D1/D2 例外**）
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
> **追加（2026-10-01，`fix/parallel-flake-timing` 批）**：**D9 / D10 已修**（两条状态行已改写，机理与修法写在各条详情段的「本批」块）；D7 的「CI 单进程」表述**已订正**（见 D7 详情段「订正」条）；新增 **D13–D15** 三条挂账（反向用法整组 / 本批未修暂留项 / 未实测项）。本批**只动 `test/**` 与本节台账**，`src/**` 零改动、未提交；D11/D12 状态不变。
> **追加（2026-10-01，`fix/parallel-flake-timing` 批 · 第二关 oracle 的 should-fix 项收口）**：D13 清单改为**全量 33 处四类分列**（含反方向例外 `test/unit/daemon-process-manager.test.ts:518`）；D14① 的「无活 socket / `isTerminalConnected` 恒 false」理由**订正**（该用例第一阶段就有活 socket）；D10「核对」改为修后**两种残余签名**的观察口径（屏障卡住 ⇒ 外层 5s 超时或谓词错误；屏障被绕过 ⇒ 回到原 owner `AssertionError`）；删去形状 A 的不可复算耗时数字、补入**高载档一手实测**（来源＝oracle 本轮）；新增 **D16｜抬预算判据 + 占用率清单**；**D10/D14 注明 `#stopping` 前提与「插入 `await` ⇒ 假失败报警」的耦合**（派发件写作 D8/D14，实际落点在 D10 段 + D14⑤，理由见 D14⑤：D8 讲的是 `readChildPids` 告警分支无覆盖，与屏障无耦合）；D15 的 CI 口径改为**决定**（不主动追样本）。按 D16 判据给两条用例加显式 30s（`test/integration/agent-index-service.test.ts:4889`、`test/unit/herdsman-pi-extension.test.ts:4758`，各 1 行注释 + 第三参）。本批仍**只动 `test/**` 与本节台账**，`src/**` 零改动、未提交。

> **追加（2026-10-01，`fix/child-process-active-t-state` 批）**：**D6 已修**——owner 定稿等待语义 **S1：`T`（`SIGSTOP`/`SIGTSTP`）算活**（判活表达式删 `&& state !== "T"` + 注释改写），并给 `T` 态补 1 条回归用例（`test/unit/daemon-process-manager.test.ts:912-937`，FAIL→PASS 已取证）；**A7 正文的两处前提已订正**（ptrace 得到的是 `S`/小写 `t`、cgroup freezer 未验证＝证据缺口，见 A7「更新」段）。本批**只动 `src/daemon/process-manager.ts` 的一行判活 + 注释、该测试文件追加 1 条用例、本节台账**，未提交；D1/D2/D3/D4/D5/D7–D16 状态不变。

> **追加（2026-10-01 同日二次拍板，同分支 `fix/child-process-active-t-state`）**：owner **推翻 S1，定稿「精细版」语义**——**pre-READY 的 `T` 可放弃（杀 + 重试）/ READY 之后的 `T` 恒算活**（保住 D6 的修点）；改判理由＝ S1 的 pre-READY 代价面（真 `SIGSTOP` 形状 B：S1 打满 1000ms 后失败 3/3，旧代码 7–8ms 自愈成功 3/3）。同分支上把判活函数改为**显式 phase 参数**（`type ChildLivenessPhase`）并追加 1 条 pre-READY 用例（全量 823 条）；**三形状实测对照表与 3 处口径修订全部落在 D6 详情段**。D1–D16 其余状态不变。

> **追加（2026-10-01，`fix/child-process-active-t-state` 批 · reviewer 收口：措辞订正 + 观察项）**：D6 段（并同步 A7 机理正文、A8② 的同源句）把判活语义统一改为「**该进程尚未退出（未关闭自身 fd）才算活**」并补 **D1/D2 继承 fd 例外**（判死 ≠ 锁一定空闲；来源＝reviewer **必须项**）；D15 段新增 1 条**未复现 / 未定位**观察项（`pnpm check` 首跑末步的 `Bad substitution` 瞬时噪声：单步 `pnpm herdr-plugin:check` 与整链复跑均 `EXIT=0`、日志无该字样）；产品注释（判活函数的 JSDoc 与函数内注释）同步订正；回归用例 post-READY 名字改为「模拟」表述（reviewer 建议项，三条断言与缝形状未动）。本批**只动产品注释、测试用例名/注释与本台账**，**未改判活逻辑 / `phase` 语义 / 任何断言**，未提交。

> **追加（2026-10-01，`fix/child-process-active-t-state` 批 · oracle 提交前第二意见的应修项收口：2 应修 + 2 措辞收敛）**：① **错引用订正**——D6 段把 pre-READY 用例的 `elapsed < 500` 写成「实测 ~7ms」，实为 **racy 探针 (B) 臂**的数（该臂不可复现）；该用例实测 **≈58–60ms（空载）/ 墙钟 74–77ms**，且该粗断言 **0 区分力**（oracle 复核：精细版 58/59/60ms、S1 55/56/55ms、HEAD 58/59/58ms），真区分力在 **`helperPids.length >= 2`（重试计数）**；② **(B) 配方前置**——形状 (B) 须用**确定性配方**（fake 慢 `sh` + 真 `SIGSTOP`）才能复现（不带 fake sh 的 racy 触发器 **6/6** 落在 post-READY 侧），确定性配方下 oracle 独立复算（每臂 3 次）：HEAD **60/61/66ms** / S1 **1000/1000/1000ms** / 精细版 **62/62/64ms**；③ **措辞收敛①**——pre-READY 判定的**绝对口气**「尚未宣告 READY ⇒ 协议上不视为合法持有者」收敛为**快照级**「**本 attempt 尚未观测到 READY**」，并新增 **I-1** 登记（形状 (A) **6/9** 命中、有界/无害、HEAD 逐字节同形）；同步给产品注释加 1 处限定词 `as observed by this attempt`；④ **措辞收敛②**——「新增残留面」收敛为**既有外部 `kill` 面的一个子集**（形状 (D) 实测 HEAD 与精细版逐项相同 **2/2 vs 2/2** ⇒ 与 `phase` 无关）；⑤ **D15 补半句**（`Bad substitution`：emitter＝harness 包装层 `package-manager-cli.js`、`/bin/sh`＝`dash`、检查链无 bash-ism、整链独立复跑 `EXIT=0` ⇒ 环境/包装层噪声）。本批**只动本台账 + `src/daemon/process-manager.ts` 判活注释 1 处限定词**，**未改逻辑 / `phase` 语义 / 断言 / 用例 / 窗口常量 / attempt 结构**，未提交。

> **追加（2026-10-01，`fix/ledger-test-timing` 批；`[MARK-LEDGER-TESTTIMING]`）**：**D13 ③ 类的 3 处已全部改成轮询屏障**（逐处锚点与改法见 D13 段「本批」块）——`test/unit/daemon-service.test.ts` 两处（固定 0ms 沉降 → `vi.waitFor` 轮询 + 每次尝试内一次宏任务让步；断言逐字保留）、`test/unit/daemon-process-manager.test.ts` 一处（固定 50ms 窗口 → 既有 `waitForCondition` 轮询，落点 **`:730-732`**，台账原记 `:518`）。**② / ④ 类按台账既有决定不动**；**D14①/②/③ 逐条判定为「保持不动 + 理由」，D14④ 的 ③ 子集即本批所修**（见 D14 段「本批判定」块）。本批**只动 `test/**` 与本节台账**：`src/**`、`vitest.config.ts`、`package.json`、断言**零改动**；**未提交**。
> **口径随之变化（可复算）**：`grep -rn "setTimeout(resolve," test/ | wc -l` **33 → 32**——③ 的 3 处里，`daemon-process-manager` 那处已彻底移出该 grep（改用 `waitForCondition`）；`daemon-service` 两处仍命中，但其 `setTimeout(resolve, 0)` 已从**判定窗口**降级为**轮询内的让步**（判定与超时由 `vi.waitFor` 给出）。
> **本批自身的行号位移（供后续批次；本批未改写 D9 / D13 旧行）**：`test/unit/daemon-service.test.ts` 在 `:132-209` 区间净 **+16 行** ⇒ 该文件 `:209` 之后的既有锚点整体**后移 16 行**：台账 D9 行/详情段引用的用例声明行 `:523` → **`:539`**、外层第三参 `:615` → **`:631`**、该用例内部两处 20s 上界 → **`:572-581`** / **`:601-604`**。按派发口径（D12 / 锚点卫生由后续批次处理）本批只在**此处**登记该位移。

> **追加（2026-10-02，`fix/ledger-test-timing` 批 · 第二轮；`[MARK-FOLLOWUP-LEDGERTIMING2]`）**：① **D9 外层预算＝owner 拍板**——owner 2026-10-02 决定把 `test/unit/daemon-service.test.ts` 该用例的第三参由 `30_000` 抬到 **`45_000`**（落点现 **`:639`**、用例声明现 **`:544`**），**消掉 (30s, 40s] 这个「两段 20s 串联」造成的假失败窗口**（**不是** worker 判断）；详见 D9 段与 D14②。② **三处屏障用例加测试级第三参 `}, 10_000);`**（oracle should-fix S1，可诊断性）：`test/unit/daemon-service.test.ts:151` / `:216` 与 `test/unit/daemon-process-manager.test.ts:745`，**轮询预算仍为 `5_000`**（D16 口径不变）——独立理由＝**轮询到期必须早于测试级超时，谓词错误才报得出来**（否则只剩 `Test timed out in 5000ms`）；**未改** `vitest.config.ts`。③ **措辞收敛（oracle S2）**：D13③ 的「负载下可能假失败」订正为「**结构性假设：沉降必须在一个宏任务 turn 内完成**」（S2a，见 D13 段）；D14② 的「只改失败签名、不改失败面」订正为「**消掉 (30s, 40s] 的假失败窗口，不改真失败面**」（S2b）。④ reviewer / oracle 的 5 条遗留（参数形态不齐、D13 账目算式、锚点漂移、口径外同族、② 类附注）**逐条落进 D17｜遗留清单**。本批**仍只动 `test/**` 与本台账**，未提交。
> **本轮锚点位移（本批自身造成，供后续批次）**：新增测试级第三参与注释使 `test/unit/daemon-service.test.ts` 在 `:145-216` 区间**净 +5 行** ⇒ D9 用例声明 `:539` → **`:544`**、第三参 `:631` → **`:639`**、内部两段 20s 上界 `:572-581` / `:601-604` → **`:577-586`** / **`:606-609`**；`test/unit/daemon-process-manager.test.ts` 在 `:739` 之后**净 +3 行**（该文件 `:739` 之后的既有锚点后移 3 行）。**D12 / 锚点卫生按派单仍由后续批次处理**，此处只登记。

> **追加（2026-10-01，`chore/ledger-hygiene-d12-d15` 批；`[MARK-LEDGER-HYGIENE-D12D15]`；本机 `date` 实测 `2026-10-01`，批内前序条目记为 10-02 属时钟差异，本条按本机实测日期）**：**① 三处测试面小修**（`test/unit/daemon-process-manager.test.ts` 的 D1 降级用例把两个 handle 的 `release()` 移进 `finally`＝**D17(n) 收口**；同文件 SIGKILL 用例的无界 `child.on("exit")` 改为**有界轮询**＝**D17(f) 收口**，落点 `:793-795`，紧接的 flock 释放屏障 `:800-802`（**本轮 fix-round 后为 `:810-812` / `:817-819`**，映射见 D12.4）；`test/integration/herdr-socket-client.test.ts` 那条被丢弃的 `Promise.race` 改为**显式断言**＝**D17(d) 收口**），三处**断言一字未放宽**（`git diff -U0` 只增不删断言），每处都有**负控**（**修前 / 修后对照 + 注入必失败**；**原始输出不在 D15 段**——D15 段内 0 处命中，实际落点是 **D17 段 (d)/(f)/(n) 的「负控」块**，本轮另见下条 `[MARK-D12D15-FIXROUND]`）。**② D12 台账卫生**：`/tmp` 锚点与引用数字逐条处置、位移映射收进 **D12.4** 一处（本节内新增 D12.1–D12.5）。**③ D15 证据边界**：用台账配方（26 个 busy-loop；**订正**：原写「loadavg 24.6–28.0」的高值来自**只落 stdout、已随会话压缩不可复算**的首轮 ⇒ 可复算负载档以 **D15 段落盘记录**为准＝ 24.43 → 26.65 与 11.20 → 21.43）补测三处屏障占用率、D9 用例、`waitForNotification` 峰值、4 处 spawn 点；并把 `pnpm check` 的 `Bad substitution` 噪声**归因验证**（harness 固定 `/bin/sh -c`＝dash；本批一手复现逐字同一文案 `/bin/sh: 1: Bad substitution (exited with code 2)`）。**本批只动 `test/**` 两个文件与本台账**，`src/**` 零改动、`vitest.config.ts` 未动、**未提交**。
>
> **追加（同日第二轮，`chore/ledger-hygiene-d12-d15` fix-round；`[MARK-D12D15-FIXROUND]`）**：按双审结论落完 **oracle 3 条 should + 3 组同类补完 + 3 项登记**（工作树仍未提交，`HEAD` = `0a1d6b7`）。**① should#1（D12.4 轨迹格不可复算）**：逐 rev 按行内容实测订正为 `:62 @ 5433525`–`b6c3b87` → **`:63 @ 2555b2b`** → **`:64 @ 5b7181b`/`105286f`** → **`:127 @ 0a1d6b7`/本批**（与 oracle 实测一致）。**② should#2（指针错）**：顶部条原写「负控…见 **D15 段**「负控」行」，实测 **D15 段内 0 处命中** ⇒ 改指 **D17 段 (d)/(f)/(n)**，并复核该条其余指针（D12.4 / D15 ⑤ 均已落点）。**③ should#3（「都」过泛）**：「同文件其它真 spawn 用例**都**在 `finally` 里杀子进程」实测**只有 3/6**（见 D17(n) 订正），**台账与代码注释两处同口径改**。**④ 同类补完（3 处，各带负控）**：`two real concurrent processes…` 的两处**无界 `on("exit")`** 改**有界轮询**（`:758` / `:761`；负控＝SIGKILL 打空 ⇒ 修前只剩裸 `Test timed out in 5000ms`，修后报**具名** `Timed out after 5000ms waiting for the … after SIGKILL`）；该用例与 `stress test: 200 rounds` 的**杀子进程补进 `finally`**（负控＝让 kill 永不执行 ⇒ 修前残留 `holders=2` / `alive_tsx=2`，修后 `0/0`）；`herdr-socket-client` 那条断言**带上错误原因**（`stream-error: <err>`）且 **teardown 挪进 `finally`**（负控＝断言处必失败 ⇒ 修前 `10064ms` + `Hook timed out in 10000ms` 第二条报错，修后 `62ms` 单条报错）。**⑤ 一处预算新增（需 reviewer 追认）**：该用例按本文件既有口径加第三参 **`}, 10_000);`**——理由与 S1 同（**轮询 5s 到期必须早于测试级超时**，否则谓词错误报不出来；负控两态原文见 D17(r)）。**⑥ 登记（不修）**：D17 新增 **(r)** 假失败窗口（结构性：timer 相位先于 poll 相位；SIGSTOP 可确定性复现；高载实耗余量见条目）、**(s)** 高载测量的背景负载口径（多 agent 会话叠加）、**(t)** 本轮一手新发现的两处残留（SIGKILL 用例的 kill 仍 inline；两处 `readLine` / `nextLine` 是无界等待）。**本轮仍只动 `test/**` 两个文件 + 本节台账**，`src/**` 零改动、`vitest.config.ts` / `package.json` 未动、**未提交**。

| # | 残余项 | 来源轮次 | 状态 | 核对锚点 |
| --- | --- | --- | --- | --- |
| D1 | `children` 文件不可用 → release 屏障降级为只盯 flock 进程，可能带锁返回 | 收口轮（本批）；残余率引自收口轮测量 | **接受**（已加每进程一次告警；残余由 acquire 重试兜底） | `src/daemon/process-manager.ts:312-333`（读 `:312`、告警 `:326`） |
| D2 | release 的 100ms 预算耗尽时仍可能带锁返回 | 本批（A｜release 屏障） | **接受**（best-effort 预算，设计取舍） | `src/daemon/process-manager.ts:574` |
| D3 | acquire 硬上界 ≈**1000ms+ε**；deadline 耗尽后即使锁已空闲也返回失败 | 本批（B｜有界重试）＋收口轮实测 | **接受**（预算语义＝设计取舍） | `src/daemon/process-manager.ts:371`、`:452`、`:515` |
| D4 | 无 `owner.json` 的外来持锁最多 4 次 spawn 后报 held；失败延迟 p50 ~2ms → ~11ms（有界） | 本批（B）；本轮一手重测 | **接受**（有界） | `src/daemon/process-manager.ts:391`；harness `/tmp/lockrace-docs/measure.ts` |
| D5 | owner 闸门依赖 `isProcessRunning`（`kill(pid,0)`，EPERM 也算活）→ 僵尸 / pid 复用的 `owner.json` 会闸掉重试 | 本批（B｜重试闸门） | **接受**（退化为旧行为，不产生新错误） | `src/daemon/process-manager.ts:401-413`、`:216-223` |
| D6 | `isChildProcessActive` 把 `T/Z/X` 都视为已退出 → helper 被 `SIGSTOP` 时 release 提前返回（**且一次拍板的 S1「`T` 恒算活」会把 pre-READY 被停住的半成品也算成要等待的持有者，白耗满 1000ms 窗口**） | 既有（A7 同源）；owner 2026-10-01 一次拍板 S1 → **同日二次拍板：精细版**（原 S1 被其 pre-READY 代价面推翻） | **已修（owner 二次拍板语义＝精细版：pre-READY 的 `T` 可放弃 / READY 之后的 `T` 恒算活）**：判活函数加显式 `phase`（`type ChildLivenessPhase`＝`:349`、签名＝`:351-355`），判活行＝`:378-379`；**只有 pre-READY 调用点**（`:504`）传 `"pre-ready"`。回归用例 **2 条**（post-READY `test/unit/daemon-process-manager.test.ts:913-956` 保留并核定、pre-READY `:958-1030` 本批新增），全量 **823 条**。三形状实测对照、3 处口径修订、S1 代价面见 D6 详情段 | `src/daemon/process-manager.ts:349`（`ChildLivenessPhase`）、`:378-379`（判活行）、`:504`（pre-READY 调用点；原 **`:478`**）；见 A7 |
| D7 | 200 轮压测在**并行复跑**下逼近 5s vitest 超时（CI **结构上同样并行、并不免疫**——订正见详情段「订正」条） | 收口轮 + 本条 chore 重测 | **已修（本条 chore，仅该用例）**：`test/unit/daemon-process-manager.test.ts:1011` 给该用例加显式超时 `30_000`（依据：3 路并发全量**一手实测 6.3–6.7s** >5s）；**只抬了这一条用例**，同类 D9/D10 已由 `fix/parallel-flake-timing` 批收口（D7 与 D9 的机制不同：本条是耗时逼近阈值，D9 是脚手架预算错配） | 可复跑形状见下方 D7「核对」段（`pnpm vitest run --reporter=verbose`，单进程 / 2 路 / 3 路并发）；本批笔记第 51 行 |
| D8 | D1 的告警分支**无自动化用例**（覆盖靠实验验证 + 审计） | 收口轮（本批） | **已修（2026-10-01，`fix/d8-child-pids-warn-coverage`；`[MARK-D8-COVERAGE]`；**未提交**）**：**仅测试侧注入**（`vi.mock("node:fs")` 部分 mock，只拦 `readFileSync` 的 `/proc/<pid>/task/<pid>/children` 路径）＋沿用既有 `readProcessStat` 缝，`src/**` **零改动**；**新增 2 条用例**（告警恰好一次＝`test/unit/daemon-process-manager.test.ts:1315`；降级后 release 只盯 flock 进程＝`:1358`（带 `test.skipIf(!hasProcChildren)` 守卫 + 承重自检）），全量 **838 → 840 条**（fix-round 一手复测确认，见 D8 段）；**负控 3/3 如期失败**（删告警 ⇒ ①`called 1 times, but got 0 times`；回退分支多返一个 pid ⇒ ②`Set{ 30744 } ≠ Set{ 30744, 30745 }`；缝把 `Z` 换成活态 `S` ⇒ 承重自检 `expected 'S' to be 'Z'`）。详见 D8 段 | `src/daemon/process-manager.ts:401`（`readChildPids`）、`:408`（catch）、`:413-417`（一次性告警；`console.warn`＝`:415`、`warnedChildPidsUnavailable`＝`:388`）、`:588`（`helperPids`）、`:705`（release 等待环）——**原记 `:326` 已漂移**（D12） |
| D9 | `test/unit/daemon-service.test.ts:523`（真 spawn daemon 的重测试）在 **3 路并发 + 高负载（load 21–34）**下 **3/3** 超时；同形状 **load 12–17** 的 3 路运行 **0/6**——与 D7 **同类** | 本条 chore 文档轮（取证引自 oracle 文档轮）；本批实现 | **已修（2026-10-01，`fix/parallel-flake-timing`）**：该用例补第三参 `}, 30_000);`（落点 `test/unit/daemon-service.test.ts:615`）→ **2026-10-02 owner 拍板抬到 `45_000`**（消掉「两段 20s 串联」的 (30s, 40s] 假失败窗口；落点现 **`:639`**、用例声明现 **`:544`**，见 D14②）。机理＝真 spawn `node --import tsx` + **整模块图 + migrations** 的脚手架开销跑在默认 5s 上，而该用例内部的 `vi.waitFor` 自己就等 **20s**（内部预算反比外层预算长）；30s ≥ 20s 消除错配 | `test/unit/daemon-service.test.ts:544`（`}, 45_000);` 在 `:639`；原记 `:523` / `:615`，见 D14②）；复跑形状同 D7「核对」段 |
| D10 | `test/integration/orchestrator-disconnect-grace.test.ts:306` 高载 3 路并发下 **1/3** 断言失败（**非超时**） | 本条 chore 文档轮（取证引自 oracle 文档轮）；本批实现 | **已修（2026-10-01，`fix/parallel-flake-timing`）**：依赖结果的 4 处固定 20ms 墙钟 → **轮询屏障** `waitForTerminalDisconnect()`（`test/integration/orchestrator-disconnect-grace.test.ts:447-486`；JSDoc 含本批第二轮补的两条前提 `:468-476`）。机理＝**单一 20ms 墙钟窗口 + 不重试**：事件循环被饿死 >20ms 时，`advance(...)` 跑在 `due=now+50` 宽限定时器**注册之前** ⇒ 定时器被**永久孤立**（虚拟时钟不再前进）⇒ owner 永不清 ⇒ 立刻断言失败 | `test/integration/orchestrator-disconnect-grace.test.ts:303-307`（屏障 `:447-486`）；复跑形状同 D7「核对」段 |
| D11 | 该压测用例**已无性能探测力**（`30_000` ≈ 单跑耗时的 21×） | 本条 chore 文档轮（取证引自 oracle 文档轮） | **已决定：不保留**（owner 2026-10-01：该用例只做并发正确性，不设性能门限；若将来要性能信号，另开不受并行负载影响的形状） | `test/unit/daemon-process-manager.test.ts:1011` |
| D12 | 台账引用数字**不可复算** + `/tmp` 锚点**全部失效**（全台账通病，非本条引入） | 本条 chore 文档轮 | **部分已修（2026-10-01，`chore/ledger-hygiene-d12-d15`；原记「挂账（台账卫生）」；范围与残留见 **D12.1–D12.5**）**：本台账自身 **18 处 `/tmp` 命中（10 个锚点）** + 引用数字**逐条处置**（重定向到现树可复算锚点 或 标注「历史值@提交」）；散落的锚点位移映射收进 **D12.4** 一处；**残留**＝`.agents/notes/` 下**其它 8 篇**笔记的 **24 处** `/tmp` 锚点（按「不可变原则」+ 本批「只动台账文字」边界**未改写**） | **D12.1/D12.2/D12.3/D12.4/D12.5**（本文档内）；`grep -rn "/tmp/" .agents/notes/ \| wc -l` ＝ **42**（9 篇；本批开工时）→ **55**（本批后，**因新增本段对旧锚点的引用**）；本台账自身：**18**（开工）→ **31**（本批后）；**其它 8 篇 = 24**（两时点不变） |
| D13 | `setTimeout(resolve,` 的**全量 33 处四类分列**：其中「断言某事没发生 / 没增长」的反向用法整组＝负载下**检出力下降**（**仅该批**会假通过、不会假失败）；另有 **3 处固定窗口等一个必然发生的结果（2 处沉降错位 + 1 处固定窗口等结果 `:518`；会假失败）**与 **20 处无风险** | 本批（`fix/parallel-flake-timing`）；清单与口径在第二轮补全 | **挂账**（反向整组本批不动：调 sleep 只白加墙钟，不改结构性；**`test/unit/daemon-process-manager.test.ts:518` 是③里的固定窗口等结果**，不在②「不会假失败」的限定内）→ **③ 类 3 处已修（2026-10-01，`fix/ledger-test-timing`；锚点与改法见 D13 段「本批」块，`grep` 口径 33 → 32 → **31**（后者见 D13 段「第三轮」，`chore/ledger-hygiene-d12-d15`））**；② / ④ 仍不动；**③ 中两处「沉降错位」的动因已按 2026-10-02 订正为「结构性假设：沉降须在一个宏任务 turn 内完成」（原写「负载下可能假失败」偏强，见 D13 段「本批」块·第二轮）** | 见 D13 段（逐条 `file:line`；四类计数 **10+3+20=33**；**本批后＝ 10+2+19 = 31**，见 D13 段「第三轮」） |
| D14 | 本批**未修 / 暂留**的点（startup-grace 无屏障、D9 内部 20s 与实际等待上限、新引入轮询的 5s 取值〔第二轮已改为「高载档实测占用 2–4%」〕、D13 组） | 本批 | **挂账**（逐条理由见 D14 段）→ **2026-10-01 `fix/ledger-test-timing` 逐条判定：①/②/③ 保持不动（理由见 D14 段「本批判定」块），④ 的 ③ 子集已由本批修掉** → **2026-10-02：② 已由 owner 拍板修掉（D9 外层 `30_000`→`45_000`）；三处屏障用例加测试级 `}, 10_000);`（oracle S1）；本轮新遗留 5 条见 D17** | 见 D14 段 |
| D15 | 前序取证 + 本批的**未实测项**（证据边界：D9 未打穿 5s、4 处 spawn 未负载实测、`waitForNotification` 峰值未测、CI 无失败样本〔本批改为**决定**，见 D15 段〕、`:39`/`:119` 两点未在探针下失败、**`pnpm check` 末步的 `Bad substitution` 瞬时噪声未复现 / 未定位根因（`fix/child-process-active-t-state` 批，见 D15 段）**） | 前序 scout + 本批 | **挂账（证据边界）** | 见 D15 段 |
| D16 | **抬预算判据** + 逐用例占用率清单（谁该抬、谁不抬；判据 vs 个例；偏离判据的个例必须自带独立理由） | 本批（第二关 oracle should-fix 项） | **已落地**（判据 + 清单齐全；据此本批新增 2 处 30s，其余按判据不动，1 处个例保留原状并写明理由） | 见 D16 段；落点 `test/integration/agent-index-service.test.ts:4889`、`test/unit/herdsman-pi-extension.test.ts:4758` |

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

- **现象**：判活表达式是 `state !== "Z" && state !== "X" && state !== "T"`；`T`（`SIGSTOP`/`SIGTSTP`；**本条原列的 ptrace / cgroup freezer 两项前提已被订正为不成立 / 未验证，见 A7「更新」段**）**内核仍持锁**，却被判死。
- **触发条件**：helper（或外部持锁者）在 release / acquire 判定窗口内被停止。
- **影响**：release 侧提前放行 → 同 D1 形状（可能带锁返回）；acquire 侧同一判活也会把停止态 helper 判死（可 `SIGKILL` 掉正在持锁的子进程并返回 null）→ 上层误报 `operation lock is held`（与 A7 同源）。
- **现有兜底**：本批只做 errno 分流，`T` 语义未动；acquire 有界重试部分兜底。
- **状态**：**挂账**（＝ A7，待 owner 拍板「等待语义」；不属本批范围）。
- **核对**：`grep -n 'state !== "Z"' src/daemon/process-manager.ts`；A7。
- **更新①（2026-10-01，`fix/child-process-active-t-state`；owner 一次拍板等待语义 S1＝`T` 恒算活）**：`T` 判死**已修**（本条原「挂账」判断被 owner 拍板取代）；**但 S1 当日即被自身 pre-READY 代价面推翻，最终落地的是「更新②」的精细版**。
  - **修法（S1，一次拍板的中间态；最终锚点见「更新②」）**：判活表达式删掉 `&& state !== "T"`——语义一句话＝**「该进程尚未退出（未关闭自身 fd）才算活」**（`Z`/`X` 仍判死），并改写上方注释写明 `T` 为何必须算活（含：小写 `t` 本来就已算活、与同文件另两个存活面口径一致）。
    - **锚点位移（S1 中间态）**：判活行 `:353` → `:358`。**只动这一行 + 注释**：`A8` 的 errno 分流、release/acquire 的窗口预算（`ACQUIRE_WINDOW_MS` / `ACQUIRE_MAX_ATTEMPTS` / release 的 100ms）、`Z`/`ENOENT` 判死路径**零改动**（这条边界在**精细版里同样成立**，见「更新②」）。
  - **机理（S1 保留的部分）**：`T`（`SIGSTOP`/`SIGTSTP`）只是**被暂停**，**内核仍持 flock**（scout 一手探针：对停止态持锁者跑 `flock -x -n` ⇒ **`exit=1`**）。判死的后果有两类：
    ① **release 侧提前放行**——自旋屏障看到「无活 fd 持有者」立即返回，锁仍在自己一侧多持几 ms（与 D1 同形状）。**（口径修订 1：已收窄为理论/极窄面，见下「口径修订」条。）**
    ② **acquire 侧杀掉持锁 helper 并返回 `null`**——放弃分支的破坏性响应（现 `:515-523`，原文锚点 `:484`）把**正持锁**的子进程 `SIGKILL` 掉，上层 `acquireDaemonLock` 抛 `operation lock is held` ⇒ daemon 启动失败。**（口径修订 2：「+ systemd 约 5s 重启循环」已收窄为「需多个 attempt 均被停住 / 窗口耗尽才失败」，见下「口径修订」条。）**
  - **OS 事实（本批执行者已一手独立复核，不依赖 scout 转述）**：`flock -x -n fl.lock sh -c '…; exec cat'` 起 helper → `kill -STOP` 后 **flock 父进程与其 `children` 均呈 `T`** → 此时 `flock -x -n fl.lock true` **`exit=1`（锁仍被持）**、`kill -0` 两个 pid **均成功（进程仍在）**；再 `kill -KILL` 两个 pid 后探针才 **`exit=0`（锁释放）**。即「`T` 判死」与「该进程已退出（关掉自己的 fd）」不等价，S1 的语义就是「**该进程尚未退出（未关闭自身 fd）才算活**」。
  - **一致性（S1 是对齐既有口径，不是新造语义）**：同文件另两个存活面本来就按「`T` 活」处理——`hasLiveLockOwner` → `isProcessRunning` → `kill(pid, 0)`（停止态进程的 `kill(pid,0)` 照样成功，`EPERM` 也算活）；`isFlockHeld` → 真 `flock -n` 探针（内核仍持锁 ⇒ 返回 held）。改动后三面口径一致。
- **更新②（2026-10-01 二次拍板，同分支；owner 推翻 S1，定稿「精细版」语义）**：
  - **语义（按协议阶段分流）**：**pre-READY**（helper 还没写 READY／ack 未出现）的 `T` **可放弃** ⇒ 进入既有「杀 + 重试」快路径（＝保持旧代码行为）；**READY 建立之后**的 `T` **恒算活** ⇒ 保住 D6 的修点（不误杀内核仍持锁的存活持有者）。
  - **为什么 pre-READY 可以放弃**：**本 attempt 尚未观测到 READY**（**快照级**表述——原写「helper 尚未宣告 READY ⇒ 协议上不视为合法持有者」属**绝对口气**，采样缝隙见下 **I-1** 条）⇒ 本次判定**按「它不是本 attempt 的合法持有者」处理**；此时杀的是**我们自己 spawn 的半成品**，释放的只是**我们想要的那把锁**（本 attempt 的 ack 是它**私有的随机路径**、除本 attempt 无人读，故无外部进程会经由一个「本 attempt 未观测到 READY」的 helper 持锁）⇒ **不会引入双 master**。**为什么 READY 后 `T` 必须算活**：内核仍持 flock（见上「OS 事实」），判死会造成**误杀 / 双重持有面 = D6 本身**。
  - **I-1（新增登记，如实）｜「刚写出 READY」的采样缝隙**：**刚写出 READY 的 helper**，若父进程的 ack 采样早于该写入落地，就会走到 **pre-READY 判定**；若此时它已被（外部）`SIGSTOP` 成 `T`，精细版判「可放弃」⇒ **`SIGKILL`**。**实测：形状 (A) 下 6/9 命中**（`helpersSeen=2`，多杀 1 个、**+4–6ms**）。判定**有界 / 无害**：① 被杀者**从未被本 attempt 采纳、从未写 `owner.json`**；② ack 是**每次 attempt 私有的随机路径**（除本 attempt 无人读）⇒ 杀的只是「**本 attempt 想要的那把锁**」，重试随即接手（`lockHeldAfterAcquire=true`）；③ **HEAD 在此路径逐字节同形** ⇒ **非本批引入**。
  - **修法（本批落码，≈5 行代码 + 注释；注释锚点）**：给判活函数加**显式 phase 参数**，语义由调用点表达——`type ChildLivenessPhase = "ready" | "pre-ready"`（`src/daemon/process-manager.ts:349`，其上方 JSDoc `:334-348` 写清两个 phase 的理由并引 D6）；签名 `isChildProcessActive(pid, readProcessStat?, phase = "ready")`（`:351-355`）；判活行 `const pausedCountsAsActive = phase === "ready";` + `return state !== "Z" && state !== "X" && (pausedCountsAsActive || state !== "T");`（`:378-379`；函数内注释 `:367-376` 说明「该进程尚未退出（未关闭自身 fd）才算活」与 phase 的关系）；**只有 pre-READY 那一个调用点**（`:504`，等待循环里「child exited before READY」的 `break` 判定）传 `"pre-ready"`，READY 分支（`:486`）、放弃判定（`:515`）、release 屏障（`:607`）都用默认 `"ready"`。**`Z`/`X`/`ENOENT` 判死、`A8` errno 分流、`ACQUIRE_WINDOW_MS`（`:397`）/`ACQUIRE_MAX_ATTEMPTS`/release 100ms 预算、attempt 结构零改动**；**没有**在两个调用点各写一份裸 `state !== "T"` 判断。
  - **措辞订正（2026-10-01，`fix/child-process-active-t-state` 批 · reviewer 必须项）**：本条此前把判活语义写成「内核已丢 fd 才算死」，属**过度承诺**——`flock` 挂在**内核 open file description** 上，进程进 `Z`/`X` 只说明**它自己**的 fd 关了；**若它派生的子孙继承了该 fd**（`setsid` 逃逸那类，**正是 D1/D2 的现实**），OFD 引用计数不为 0，**锁不会被释放** ⇒ 不该读成「判死 ⇒ 锁一定空闲」。⇒ 本段（并同步 A7 机理正文、A8② 的同源句）统一改为「**该进程尚未退出（未关闭自身 fd）才算活**」，并补 **D1/D2 继承 fd 例外：判死 ≠ 锁一定空闲**。产品注释（`src/daemon/process-manager.ts` 判活函数上方 JSDoc 与函数内注释）已同步订正，二者同源。
  - **注释行位移（本批一手）**：措辞订正使注释净增 **9 行**（JSDoc **+5**、函数内注释 **+4**），D6 段此前引用的行号整体位移：`type ChildLivenessPhase` `:349` → **`:354`**、JSDoc `:334-348` → **`:334-353`**、签名 `:351-355` → **`:356-361`**、函数内注释 `:367-376` → **`:372-381`**、判活行 `:378-379` → **`:387-388`**、`ACQUIRE_WINDOW_MS` `:397` → **`:406`**、READY 分支 `:486` → **`:495`**、pre-READY 调用点 `:504` → **`:513`**、放弃判定 `:515` → **`:524`**、release 屏障 `:607` → **`:616`**（**只位移行号，未改任何逻辑**；标识符型 `grep -n 'ChildLivenessPhase\|pausedCountsAsActive'` 核对不受影响）。
  - **回归用例（2 条，均在既有注入缝家族内，未新开文件）**：
    - **post-READY（保留并核定）**：`test/unit/daemon-process-manager.test.ts:913-956`（`a simulated T-state (SIGSTOP-paused seam) lock-holding child counts as alive and keeps its handle`；**reviewer 建议项**：名字点明「模拟、注入缝」而非真信号）——缝改为**只在 READY 已发布时**改写 state 字段（`readyPublished()` 看锁目录里的 `.ack.*` 内容是否以 `READY` 开头；否则那次读会被（正确地）当成可放弃的 pre-READY 读，用例就会跑到另一条路径上），其余读法与健康 procfs 一致；三条断言不变（`handle !== null` / **不带该缝再取一次锁必须被拒**（钉「这个 handle 确实持锁」的双主控回归面）/ `release()` 后 `waitForCondition(() => !isFlockHeld(lockPath))`）。
    - **pre-READY（本批新增 1 条）**：`test/unit/daemon-process-manager.test.ts:958-1030`（`a pre-READY paused (T) helper is abandoned: the acquisition kills it and retries`）——**PATH 缝**放一个「慢 `sh`」（`sleep 0.05` + `exec /bin/sh "$@"`）把第一个 helper 钉在 pre-READY 相 50ms（把「第一个 helper 从未写 READY」变成**事实**而非竞态），`readProcessStat` 缝只把**第一个** helper 的 state 报成 `T`；断言：`handle !== null`、`elapsed < 500`（**实测 ≈58–60ms（空载）**、用例墙钟 **74–77ms**；**此处原写的「实测 ~7ms」是错引用**——`7ms` 属 D6 表里 **racy 探针 (B) 臂**的数，且**该臂不可复现**，见下「(B) 配方前置」条。**该用例的真区分力在 `helperPids.length >= 2`（重试计数）**；`elapsed < 500` 只是**保底粗断言**（oracle 复核：精细版 58/59/60ms、S1 55/56/55ms、HEAD 58/59/58ms ⇒ **该粗断言 0 区分力**；S1 语义下本条也**不打满窗口**，而是在 `helperPids.length >= 2` 上失败，见下「反证」条）、**`helperPids.length >= 2`（杀+重试真发生）**、`isFlockHeld === true` + 不带缝再取被拒、release 后锁空闲。
    - **反证（本批一手，非空跑凑绿）**：把 `pausedCountsAsActive` 临时改成恒 `true`（＝S1 语义）后该用例 **FAIL**：`AssertionError: expected 1 to be greater than or equal to 2`（`test/unit/daemon-process-manager.test.ts:1020`）⇒ 这条用例真的钉住了 pre-READY 语义。
  - **用例计数**：`fix/child-process-active-t-state` 批 **821 → 822**（post-READY 1 条），本批 **822 → 823**（pre-READY 1 条；`pnpm test` 53 文件 / 823 条）。
  - **本批未修 / 不动**：release 侧「带锁返回」残余属 **D1 / D2**（成立要有一个 `kill(-pgid)` 够不着的 fd 持有者，如 `setsid` 出去的进程；仓库自带用例 `test/unit/daemon-process-manager.test.ts:624-688` 本身就断言 `elapsed >= 90` 且 `isFlockHeld === true`），属已接受项、由 acquire 有界重试兜底，**精细版同样不修**。
  - **三形状实测对照（本轮一手，真 `SIGSTOP`；每格 3 次；探针 `/tmp/scout-d6/probe2.ts`，源码三版副本 `current.ts`＝HEAD / `s1.ts`＝S1 / `refined.ts`＝本批精细版；缝只当「何时有 helper」的触发点，`readProcessStat` 返回的始终是**真实** `/proc/<pid>/stat`）**：

    | 形状 | HEAD | S1 | 精细版（本批） |
    |---|---|---|---|
    | (A) READY 之后真 `SIGSTOP`（每个 attempt 都停） | `handle=null`，22–24ms，4 个 helper **全被 `SIGKILL`**（`Z`） | `handle` ✓ 5–7ms，1 个 helper，持锁者保持 `T` | `handle` ✓ 5–8ms，1 个 helper，持锁者保持 `T`（**同 S1**） |
    | (B) pre-READY 真 `SIGSTOP`（停住的那个已持锁、从未写 READY）——**须确定性配方才能复现**（旧探针的 racy 触发器 6/6 落在 post-READY 侧；本行是**赢了竞态的样本、不是稳定量**，见下「(B) 配方前置」条） | `handle` ✓ **7–8ms**（旧探针样本；确定性配方 **60/61/66ms**），2 个 helper（停住的被杀成 `Z`，新的接手） | `handle=null`，**1000/1000/1001ms**（旧探针样本；确定性配方 **1000/1000/1000ms**），1 个 helper | `handle` ✓ **7ms**（旧探针样本；确定性配方 **62/62/64ms**），2 个 helper（**回到 HEAD 快路径**） |
    | (C) release 侧真 `T`（持锁者全组停在 `T` 后 `release()`） | `releaseMs` 0/1/0 | `releaseMs` 0/1/0 | `releaseMs` 1/1/0（**三版无差异**） |

    - (B) 的实测事实：`preReadyAtFreeze=true`、停住者 `stateAfter=T`、**`lockHeldWhileFrozen=true`**（被放弃的那个 helper **确实已持锁**，且 `readyEverAtEnd=false`＝从未写出 READY）；放弃后锁由**新 helper** 接手（`lockHeldAfterAcquire=true`、`helpersSeen=2`）⇒「只释放我们想要的锁、不引入双 master」在实测上成立。
    - **(B) 配方前置（重要；oracle 提交前第二意见的应修②）**：形状 (B) **必须用确定性配方**——**fake 慢 `sh`**（`sleep 0.05` 把 READY 钉在 50ms 之后）＋ **真 `SIGSTOP`**——**才能复现**；**不带 fake sh 的 racy 触发器实测 6/6 落在 post-READY 侧**（`readyAfterStop=true`——READY 在「快照→停止」的 ~200µs 间隙里落地）⇒ 上表 (B) 行的旧探针数字（含 S1 的 `null`/1000/1000/1001ms）是**赢了竞态的样本、不是稳定量**：**结论成立**（S1 打满窗口、精细版回到 HEAD 快路径），**但复现必须用确定性配方**。确定性配方下 oracle 的独立复算（每臂各 3 次）：
      - **HEAD**：`handle` ✓ **60/61/66ms**，`seen=2`，`readyAfterStop=false`，`heldWhileFrozen=true` ⇒ 冻结者成 `Z`、锁交新 helper；
      - **S1**：`null`，**1000/1000/1000ms**，`seen=1` ⇒ 窗口耗尽、锁最终自由（`false`）；
      - **精细版**：`handle` ✓ **62/62/64ms**，`seen=2`（「**回到 HEAD 快路径**」的结论不变）。
    - (A) 的实测事实：HEAD 下每次 attempt 的持锁 helper 都在**写完 READY 之后**被停住 → 判死 → `SIGKILL` → 4 个 attempt 全废 → `handle=null`（**这就是 D6 的原始害**）；精细版与 S1 一样**保留 handle、不杀持锁者**。
  - **口径修订（三条，均为本轮实测）**：
    - **口径修订 1（把「机理①：release 侧提前放行」收窄为理论/极窄面）**：(C) 三版 `releaseMs` **0–1ms、无差异**。原因：`release()` 是 **`SIGKILL` 先行**（`:586-592` 先杀组、再进屏障），而 **`SIGKILL` 对 `T` 进程立即生效** ⇒ 屏障随后看到的已经是 `Z`/消失，`T` 判死与判活的差异**测不出来**。要真落到「带锁返回」上还需一个 `kill(-pgid)` 够不着的 fd 持有者（如 `setsid` 出去的进程），那属既有 **D1 / D2** 面。
    - **口径修订 2（把「daemon 启动失败 + systemd 约 5s 重启循环」收窄）**：改为「**需多个 attempt 均被停住 / 窗口耗尽**才失败」。单次 `SIGSTOP` 形状下**旧代码（HEAD）会杀+重试并成功**——(B) 实测 **3/3 成功**（旧探针 7–8ms；确定性配方 60/61/66ms，见上「(B) 配方前置」条）；即「停一次 ⇒ daemon 起不来」**不成立**，只有停止在窗口内持续生效（如 (A) 每个 attempt 都被停）才走到「返回 `null` ⇒ 抛 `operation lock is held`」。
    - **口径修订 3（如实记入 S1 的代价面）**：(B) 实测 **S1 3/3 打满 1000ms 后失败**（`handle=null`），而**旧代码 3/3 在 7–8ms 自愈成功**（旧探针样本；确定性配方 60/61/66ms）。即 S1 的「`T` 恒算活」把「我们自己 spawn 的、尚未宣告 READY 的半成品被停住」也当成需要等待的持有者，**白耗满整个 acquire 窗口**——这是 S1 被推翻的直接原因。触发需**外部 `SIGSTOP` 自家启动中的 helper**，现实**极窄**；且本仓 detached helper **只对 `SIGSTOP` 可达**（本批一手复核：对 `spawn(..., { detached: true })` 起的孤儿进程组连发两次 `SIGTSTP`，state 恒为 `S`、无变化；改发 `SIGSTOP` 才变 `T`——helper 在 `setsid` 后的**孤儿进程组**里，`SIGTSTP` 被内核丢弃）。**该代价已由精细版消除**（(B) 精细版 3/3 为 7ms（旧探针样本；确定性配方 62/62/64ms，仍与 HEAD 的 60/61/66ms 同水平，见上「(B) 配方前置」条），＝ HEAD 水平）。
  - **新增残留面**（**2026-10-01 收敛**：不是「新增」，而是**既有外部 `kill` 面的一个子集**；owner 已知情并接受）：我们 handle 里的**停止态持锁者若被外部 `kill -9`** ⇒ 该 fd 持有者死亡、内核释放 flock，**锁静默消失，而 daemon 仍以为自己持有**（可达条件需要**外部动作**，不是本工具自己的路径；有 **SQLite 自身文件锁兜底**，不会静默损坏 DB——**但这不等于「不会双实例」：双实例仍可能出现，兜底只保证 DB 不被静默损坏**——表现为重复投递 / 写争用）。它与 A8② 的「失效 handle」面同源（都是「我们以为持有、内核已释放」），只换触发源。**收敛依据（oracle 提交前第二意见；形状 (D) 实测）**：该场景在 **HEAD 与精细版下逐项相同（2/2 vs 2/2）** ⇒ 与 **`phase` 语义无关**，是**既有外部 `kill` 面**的一个子集，**不是本批新增**的残留面（原先按「新增」登记属**措辞过强**）。
  - **未采纳的相邻方案**（owner 只取精细版）：**S2**（给停止态持锁者加限频告警）**未采纳**；**S4**（不动放弃分支的破坏性响应）**未采纳**——放弃分支的 `SIGKILL` 组 + `return null`（现 `:515-523`；原文锚点 `:484`）**在两个 phase 下都保持原样**（这是精细版的改动边界：只改「什么时候算需要等待」，不改放弃时怎么杀）。
  - **覆盖现状**：`T` 态的自动化覆盖**原为 0**（本文件既有用例只覆盖 `EIO` / 无 `code` / 空读三种读失败，**没有任何用例注入过 state 字母**）；`fix/child-process-active-t-state` 批共补 **2 条**，**正好覆盖精细版的两个 phase**（post-READY 1 条 + pre-READY 1 条）。
  - **可选未实现（形状已记，未落码）**：① **release 侧 characterization**——release 前对 helper 全组 `SIGSTOP`，断言释放后锁可见空闲（本轮已按形状 (C) 实测三版 × 3 次、无差异，但**仍未落成用例**）；② **OS 事实用例**——直接断言「停止态持锁者仍被判 held」（`flock -x -n` ⇒ `exit=1`、`kill(pid,0)` 成功），形状见 `/tmp/scout-d6/tstate.sh`。两者**有意未实现**（最小测试；形状记在本条即可）。
  - **核对**：`grep -n 'ChildLivenessPhase\|pausedCountsAsActive' src/daemon/process-manager.ts`；`grep -n 'pre-READY paused\|simulated T-state' test/unit/daemon-process-manager.test.ts`；三形状复跑形状：`pnpm exec tsx /tmp/scout-d6/probe2.ts /tmp/scout-d6/<current|s1|refined>.ts <A|B|C> <1|2|3>`（`/tmp` 会被清理，届时按 D6 本段描述重建：真 `SIGSTOP` 三种形状 + 三版源码副本）。

#### D7｜200 轮压测在并行复跑下逼近 5s vitest 超时

- **现象**：`stress test: 200 rounds of simultaneous sub-millisecond lock contention yields zero double-masters` 单跑约 **1.33s**（**本条 chore 一手热跑 1316/1325/1351ms**），但并行复跑会越过 vitest 默认 5s（3 路并发全量一手实测 6.3–6.7s）。
- **触发条件**：同一机器同时跑多份全量测试（本机复核 / 本地复跑场景）；**CI 不会自己叠并行负载，但结构上并不免疫**——见下条订正。
- **订正（2026-10-01，`fix/parallel-flake-timing` 批；原表述为「CI 单进程不触发」，站不住）**：核对 `.github/workflows/ci.yml`——只有 `pnpm install --ignore-scripts` + `pnpm check`，而 `check` 走 `pnpm test` → `vitest run`，**没有任何串行开关**（无 `--no-file-parallelism` / `poolOptions` / `maxWorkers` / `fileParallelism: false`）；`vitest.config.ts` 也未覆盖 `pool` / `maxWorkers` / `fileParallelism` ⇒ Vitest 默认 **`pool: forks` + 文件级并行 + `maxWorkers = availableParallelism`**。即 **CI 结构与本地一致，同样按文件并行**；差别只在**暴露面**（CI 只有单套、没有我们自己叠的 3 路并发，runner 的核数/负载档也不由我们控制）。**订正来源**＝本批核对上述两个文件后改写（未改动其它笔记：`grep -rn "\bCI\b" .agents/notes/` 显示该相反表述仅存在于本台账 `:269`、`:333` 两处）。
- **影响**：本地并行复跑时该用例假失败（`Test timed out in 5000ms`），非产品缺陷；本批使它变慢约 +320~350ms（本轮交错实测，见本批笔记第 51 行）。
- **现有兜底**：无（测试侧）。
- **状态**：**已修（本条 chore，仅该用例）**——`test/unit/daemon-process-manager.test.ts:1011` 给该用例加显式超时 `30_000`（`}, 30_000)` 第三参形态，与本仓重测试既有写法一致）。
  - **范围限定（重要）**：本条 chore **只抬了这一条用例**的超时，**并未**让「并行复跑全绿」成立——本地并行复跑的假失败**不止这一条**（同类项 D9 / D10，D7 只是第一个被收口的）。**（2026-10-01 追加）** D9 / D10 已由 `fix/parallel-flake-timing` 批收口，见两条状态行。
  - **取值依据（均为本条 chore 一手实测；本段 2026-10-01 订正形状归属与余量口径）**：单跑 ~**1.33s**（热跑 1316/1325/1351ms）；**2 路**并发全量 **4.0–4.4s**；**3 路**并发全量 **6.3–6.7s**（>5s ⇒ 默认 5s 超时下**必撞** `Test timed out in 5000ms`）。故取 30s，给 3 路 6.7s 形状约 **4.5×** 余量。
    - **订正（形状归属）**：原句把「另一轮负载更重时曾到 **10.8–11.2s**」挂在**2 路**形状上，**属形状归属错误**——那组数字的证据形状是 **3 路并发 · 高载档（load 21–34）10.7–11.5s**（见 D9 取证）。
    - **订正（余量口径自洽）**：同一 30s 取值下，3 路中载档 6.7s ⇒ 约 **4.5×**；3 路高载档 11.5s ⇒ 约 **2.6×**（30 / 11.5）。`vi.waitFor` 版不再需要。不阻塞本批。
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
- **状态**：**已修（2026-10-01，`fix/d8-child-pids-warn-coverage`；`[MARK-D8-COVERAGE]`）**——测试侧注入补齐，**`src/**` 零改动**（未新增注入缝，未改 `vitest.config.ts`，未动既有用例），台账之外只改 `test/unit/daemon-process-manager.test.ts`。
  - **本批（2026-10-01，`fix/d8-child-pids-warn-coverage`）现树锚点**（**原记 `:312`/`:326` 已漂移**，按内容重定位）：读 children 文件＝`src/daemon/process-manager.ts:401`（`readChildPids`，仍未导出）→ catch＝`:408` → 「`/proc` 存在且未告警过」＝`:413` → 一次性告警＝`:413-417`（`console.warn`＝`:415`）；模块级标志 `warnedChildPidsUnavailable`＝`:388`；`helperPids = [child.pid, ...readChildPids(child.pid)]`＝`:588`；release 的等待环（`helperPids.some((pid) => isChildProcessActive(pid, deps?.readProcessStat))`）＝`:705`；既有缝类型 `FlockHandleDependencies`＝`:343`（`readProcessStat`＝`:352`，**本批未新增**）。
  - **注入手法**（两条用例共用）：`test/unit/daemon-process-manager.test.ts:47`（`vi.hoisted` 状态）＋`:57`（`vi.mock("node:fs", …)`）＝**部分 mock**，`importOriginal` 保留真模块、只包一层 `readFileSync`，且**只拦 `/^\/proc\/(\d+)\/task\/(\d+)\/children$/` 形状的读**（同形状即拦、两个 pid 不必相等；生产恒相等——措辞已按 fix-round 订正，见 D17(k)；其余调用一律直通真实现；`existsSync("/proc")` 等全部走真模块）。被拦时按用例抬真的 `failChildrenRead` 抛 `ENOENT`（形如 `CONFIG_PROC_CHILDREN` 关 / 精简 procfs 的**文件不存在**），并顺带记录被读的路径与读到的内容（后者＝fd 共享子进程 pid），供断言用。**为什么不污染同文件其它用例**：① 默认直通——只有两条新用例在 `try/finally` 内抬/落 `failChildrenRead`，文件内其它 39 条用例（`/proc/<pid>/stat`、`wchan`、runtime record、ack/lock/owner 文件、真 spawn 压测）走的都是**同一个真实现**，`node:fs` 从未被换成假对象；② 拦截条件是**精确路径**而非「所有 fs 调用」，同文件没有别的路径以 `/children` 结尾；③ 本批两处只读/写 `vi.hoisted` 状态与 spy，均在各用例 `finally` 内复位（`restoreMocks: true` 亦兜底）。**实测**：该文件全量 **41 条全绿**（改动前 39 条，本批 +2），3 路并发各一会儿也全绿。
  - **用例 ①（告警恰好一次）＝`test/unit/daemon-process-manager.test.ts:1315`**（fix-round 后行号）：`an unreadable children file warns exactly once per process (D1 degraded barrier)`。用例内**成功 acquire 两次**（两次都读不到 children 文件），断言 `console.warn` **恰好一次**，并逐字钉住 D1 告警文案（`/children is unavailable` + `release barrier degraded` + `/proc/<pid>/task/<pid>/children` 前缀）——与既有 `probe undefined degrades to running with console.warn (throttled)` 同形（都是「再次调用不得再刷日志」）。**「每进程一次」是模块级状态、外部无法重置**，故该用例用 `vi.resetModules()` + 动态 `import("@/daemon/process-manager.js")` 取**模块新副本**（标志从 false 起算），从而**与同文件其它用例的先后顺序解耦**，不依赖「本用例先跑」。**本用例刻意不加平台守卫**（fix-round 判定）：它**注入**读失败，在任何 Linux（含 procfs 不暴露 children 文件的内核）上都成立且在那样的内核上反而**真触发**并通过，加守卫会漏掉该路径（见 D8 段 fix-round 条与 D17(h)⑤）。
  - **用例 ②（降级后只盯 flock 进程）＝`test/unit/daemon-process-manager.test.ts:1358`**（fix-round 后行号）：`an unreadable children file leaves release watching the flock process alone (D1 degradation)`。**带与邻例同形的平台守卫** `test.skipIf(!hasProcChildren)`（`hasProcChildren` 定义＝`:902`，邻例守卫＝`:904`/`:970`；fix-round 加）——procfs 不暴露 children 文件的内核上本用例的健康对照必然失败（`childrenContent` 空 ⇒ `fdSharingPid`＝`NaN`），守卫后与邻例一致跳过。观测面＝**既有 `readProcessStat` 缝**：release 等待环是唯一消费该缝的地方，故记录它被调用的 pid 就等于记录「release 实际等了哪些进程」。缝把 **flock 进程报成已退出**（`Z`，即刚被 SIGKILL 后等待环看到的真实状态），使 `helperPids.some(...)` 必须继续看下一项。同一用例内做**降级 → 健康**对照：**降级**（读不到 children 文件）时 `helperPids` 只剩 flock 进程 ⇒ 等待集 ＝ **`{flock pid}`**；**健康对照**（可读 children 文件，子进程 pid 由 mock 捕获的文件内容取得）时同一调用等待集 ＝ **`{flock pid, fd 共享子进程 pid}`** ⇒ 断言非空转，降级确实少了 fd 共享子进程那一项。**另带一条承重自检**（fix-round 加，oracle should-fix）：调用 `release()` **之前**用同一条缝断言 flock 进程的 stat 状态字段为 `Z`（`isChildProcessActive` 未导出，故钉的是它读的同一字段）——该自检是「主断言单靠自己挡不住 `Z`→活态 变异」的兜底（负控③）。
  - **负控（本批一手；均在 `/tmp` 下的**副本**内做，原仓库只留正式改动，副本用完已删）**：① 删掉 `:415` 的 `console.warn(...)` ⇒ 用例① **FAIL**：`AssertionError: expected "warn" to be called 1 times, but got 0 times`（0/1 用例失败）；② 让 catch 在降级时多返一个伪造 pid（`return [pid + 1];`）⇒ 用例② **FAIL**：`expected Set{ 30744 } to deeply equal Set{ 30744, 30745 }`；③ **（fix-round 新增）缝里把 flock 的 `Z` 改成活态 `S`** ⇒ 承重自检 **FAIL**：`AssertionError: expected 'S' to be 'Z'`（落点 fix-round 后 `:1402`，在 `release()` 之前）；**同一次变异下若把自检摘掉**，主断言（降级等待集）**静默通过**、由**健康对照**接住（`Set{ 77489 } ≠ Set{ 77489, 77490 }`）⇒ 自检确实承重，也证实主断言单靠自己挡不住「`.some()` 短路」这一支变异。⇒ 三条断言分别真的打在**告警**、**等待集**与**注入状态**上。
  - **预算**：两条用例 3 路并发下实测 **9–14ms**（占默认 5s 的 ≈0.2–0.3%，远低于 D16 的「<30% ⇒ 不动」线）⇒ **不加**逐用例第三参。
  - **双审后续轮（2026-10-01，`[MARK-D8-FIXROUND]`）**：① **用例② 加平台守卫**（与邻例同形 `test.skipIf(!hasProcChildren)`，现树 `:1358`）；**用例① 保持裸 `test(`**（理由见上）。② **用例② 加承重自检**（`Z` 状态断言，落点 `:1396-1402`，在 `release()` 之前）。③ **用例数 838 → 840 数字未改（第一手复测确认）**：口径＝`vitest.config.ts:18` 的 `include`（`test/unit/**/*.test.ts` + `test/integration/**/*.test.ts`）；`git ls-files 'test/**/*.test.ts' | wc -l` ＝ **53**；本批后实跑 ＝ **53 文件 / 840 用例**（`pnpm test` exit 0）⇒ 基线 **838** 成立。另有独立复核给出「55 文件 / 842 用例」，来自其**临时副本**（含额外测试文件），**已被本批第一手测量取代**。④ 双审 consider 项（含 oracle 对用例② 的 `Z` 承重分析）逐条登记进 **D17 (j)–(o)**。
- **核对**：`grep -n "readChildPids\|warnedChildPidsUnavailable" src/daemon/process-manager.ts`（`:388`、`:401`、`:413`、`:414`、`:588`）；`grep -n "node:fs\|children file\|skipIf" test/unit/daemon-process-manager.test.ts`（现树命中 mock 缝 `:47`/`:57`、两条用例 `:1315`/`:1358`（后者带 `skipIf`）、邻例守卫 `:904`/`:970`；**原记「测试文件内零 `readChildPids` 引用」仍成立**——该函数未导出、测试里仍无此符号，覆盖走的是它的**调用点**）。

#### D9｜真 spawn daemon 的重测试在高负载并行复跑下同样超时（与 D7 同类）

- **现象**：`test/unit/daemon-service.test.ts:523`（`a second daemon on the same HERDSMAN_HOME fails on the instance lock; SIGTERM releases it and allows restart`，**会真 spawn daemon** 的重测试）在 **3 路并发 + 高负载**下 **3/3** 记到 `Test timed out in 5000ms`。
- **触发条件**：同一机器上 3 个进程各跑全量，且后台负载处于高挡（实测 **load 21–34**）；同形状在 **load 12–17** 的 3 路运行 **0/6**——即「多进程 + 高负载」同时满足才触发。
- **影响**：本地并行复跑时该用例假失败（`Test timed out in 5000ms`），**非产品缺陷**。与 D7 **同类**：本地并行复跑的假失败**不止 D7 这一条用例**，D7 只是第一个被抬超时收口的。
- **现有兜底**：无（本条 chore 只给 D7 那一条用例抬了超时，**未覆盖本条**）。
- **状态（2026-10-01 改写；「已立项」→「已修」）**：**已修（`fix/parallel-flake-timing` 批）**——`test/unit/daemon-service.test.ts:615` 给该用例补第三参 `}, 30_000);`（先例：`test/unit/herdr-plugin-package.test.ts:72`）。
  - **本批（2026-10-01，`fix/parallel-flake-timing`）**：**只改这一条**，同文件其它用例未动（前序 scout 未测出它们超 5s）；`vitest.config.ts` 的全局 `testTimeout` **未动**（理由：会把本地真失败的反馈从 5s 拖到 20s，并掩盖「病态变慢」信号）。取值理由见下一条注释与本条「机理」：默认 5s 量的是**脚手架开销**（node 起进程 + tsx 转译 + 整模块图 + migrations），不是产品 SLA；30s **≥** 用例内部 `vi.waitFor` 的 20s，消除「内部在等、外层判死」错配。
  - **第二轮（2026-10-02，`fix/ledger-test-timing`；owner 拍板）**：第三参 `30_000` → **`45_000`**（`:639`），理由＝内部两段 20s **串联** ⇒ 病理性上界 40s，`30_000` 之下「合计 ∈ (30s, 40s] 且各自 < 20s」的**本可通过**运行会被外层截断成 `Test timed out`；owner 选择消掉该假失败窗口（详见 **D14②**）。**未动**内部两段 20s；`vitest.config.ts` **未动**。
  - **本批是否打穿该签名**：**没有**——本批在**两档**下各跑一轮「3 路并发全量」（形状 A＝**3 个进程各跑一次全量 `pnpm vitest run`**，两档独立测量）：① 低载档（跑前/跑后 `uptime` 记 1 分钟 loadavg 0.76 → 5.39）；② **高载档**（来源＝oracle 本轮一手：**26 个 busy-loop 把 1 分钟 loadavg 从 19 拉≈28**，落在 D9 原始档 21–34；node v22.23.1）。**两档均 3/3 全绿**（每轮 821 用例 / 53 文件），**耗时随机器负载浮动，不作为锚点**（形状与判定字段见 D7「核对」段）⇒ D9 的原始签名在本批**未被复现**（证据边界见 D15）。
- **核对**：`sed -n '544p' test/unit/daemon-service.test.ts`（用例声明）；`sed -n '639p' test/unit/daemon-service.test.ts`（应为 `}, 45_000);`；**2026-10-02 现树行号，原记 `:523` / `:615`**）；复跑形状同 **D7「核对」段**（高负载、3 路并发全量、`--reporter=verbose`，看 `Test timed out in 5000ms`）。

#### D10｜断连宽限集成用例在高负载并行复跑下断言被打穿（非超时）

- **现象**：`test/integration/orchestrator-disconnect-grace.test.ts:306` 在高载 3 路并发下 **1/3** 出现 `AssertionError: expected { paneId: 'wB:p-owner', … } to be null`——**非超时**，失败点是 `:304-306` 的 `expect(orchestrator.status({…})?.owner).toBeNull()`。
- **触发条件**：高负载 + 3 路并发全量复跑；该断言依赖 `scheduler.advance(60)` 之后的 tick/时序假设。
- **影响**：断言对 tick/时序的假设被负载打穿 ⇒ 本地并行复跑出现**假失败**（同类噪声，非产品缺陷）；因**不是超时**，抬高该用例超时**解决不了**，须改断言形状（如把对 tick/时序的假设改成可等待条件）。
- **现有兜底**：无。
- **状态（2026-10-01 改写；「已立项」→「已修」）**：**已修（`fix/parallel-flake-timing` 批）**——修法＝**轮询屏障**。
  - **本批（2026-10-01，`fix/parallel-flake-timing`）**：`test/integration/orchestrator-disconnect-grace.test.ts` 新增 `waitForTerminalDisconnect(server)`（`:447-486`；JSDoc 在 `:447-477`、函数体 `:478-486`），把**依赖结果**的 4 处 `await socketTick(); scheduler.advance(...)`（现 `:39`、`:119`、`:168`、`:300`）换成它；`:144` / `:149` / `:253` 三处是「断言某事没发生」的**反向用法，保持原样**（见 D13）。**未改**那个 20ms 常数以外的任何东西、**未改** `advance(49)` / `advance(1)` 这类宽限 50ms 的**边界断言**、**未改 `src/**`**。
  - **屏障蕴含关系逐点自证（`file:line`）**：`src/daemon/observability-server.ts:613-622` 的 `#handleSocketClose` 先 `:618` 调 `#unregisterTerminalSocket`（其 `:702-709` 在 socket 集合空时删除 `#socketsByTerminal[key]`），**再在同一同步 turn** `:621` 调 `#scheduleDisconnect`（`:624-636`，`#setTimeout(..., #disconnectGraceMs)`）；`isTerminalConnected` 是同一注册表的直读（`:727-729`）⇒ 观察到 `false` **蕴含**宽限定时器已注册，且 `due = now() + disconnectGraceMs`（此刻无 `advance`，虚拟时钟不前进）。4 点均落在该链上；`:39` 那条的 socket 由服务端 `#scanHeartbeats`（`:717-725`）`socket.destroy()` 触发，走的仍是同一条 close 链。**唯一不套用同屏障的是 startup-grace 用例**（`:179-210`）——**本批第二轮订正**：它不是「无活 socket」（第一阶段 `:188` `connect` + `:189` `await register` 就有活 socket），而是它的三处 `advance` 都**不受 close 竞态影响**（startup timer 在 `start()` 内**同步 arm**、register 走 **`await` 的 RPC 往返**）⇒ **不需要**该屏障（详 D14①）。
  - **FAIL→PASS 一手取证（饿死探针，本批）**：把 `socketTick()` **临时**改成「先 `busy(120ms)` 再 `await` 已到点的 20ms tick」（确定性饿死事件循环，不改产品码、不改边界断言），对**同一份测试文件**的两个副本各跑 3 次：**改前副本**（`git show HEAD:` 逐字节一致 + 该饿死补丁）**3/3 失败**，签名＝`AssertionError: expected { paneId: 'wB:p-owner', … } to be null`（副本 `:306`）＋ `expected { paneId: 'wC:p-owner', … } to be null`（副本 `:174`）；**改后副本**（工作树 + 同一饿死补丁）**3/3 通过（`Tests 10 passed`）**；300ms 饿死档同形（3/3 失败 vs 3/3 通过）。**脚手架副本（`test/integration/tmp-d10-*.test.ts`）已全部删除、`/tmp/d10-fix-probe/**` 属本机临时产物**，收尾 `git status --short` 只余 5 个预期修改文件。
  - **可复跑形状（不依赖任何 `/tmp` 路径）**：① `cp` 一份 `test/integration/orchestrator-disconnect-grace.test.ts`；② 把 `socketTick()` 函数体替换为 `const tick = new Promise((resolve) => setTimeout(resolve, 20)); const end = Date.now() + 120; while (Date.now() < end) { /* block */ } await tick;`；③ 对 **HEAD 版本副本**与**工作树版本副本**各跑 3 次 `pnpm vitest run <副本路径>`；判定字段＝`AssertionError: expected { paneId: … } to be null` 与 `Tests  N failed | M passed`。
  - **本批实测的一个边界（如实登记）**：该饿死形状**只打穿 `:168` / `:300` 两个形状**；`:39` / `:119` 两个屏障点在探针下**未观察到失败**（机理：服务端 destroy 的 close 走 `process.nextTick`、以及继续体所处的 libuv 相位不同，可先于断言把 close 处理掉）⇒ 这两点属于**严格加强**（旧 sleep 在本探针下够用），其「原形状也会假失败」**未被实测**（见 D15）。
  - **同族一并修**：`test/integration/observability-rpc.test.ts:483-492` 的 `sleep(75)` → 轮询到「重新认领成功」（`vi.waitFor(() => piB.request("agent.orchestrator.set", { enabled: true }), { interval: 10, timeout: 5_000 })`）；**轮询无副作用的依据**：被拒的 `set` 在写库之前就抛 `ORCHESTRATOR_SCOPE_ALREADY_CLAIMED`（`src/db/agent-orchestrator-scopes.ts:254-262`）。
  - **前提与耦合（本批新增；落点＝`waitForTerminalDisconnect` 的 JSDoc `:468-476`）**：① 屏障**仅在 server 未 `stop()` 时有效**——arm 断开定时器的那句在 `#handleSocketClose`（`src/daemon/observability-server.ts:613`）里带 `!this.#stopping` 守卫（`:621`），而 `stop()`（`:201-213`）会置 `#stopping = true` 并清空 `#socketsByTerminal`（`:212`）⇒ 已 stop 的 server 上 `isTerminalConnected` 只读注册表就为 `false`，**蕴含不成立**；② 该蕴含**依赖 `#unregisterTerminalSocket`（`:618`）与 `#scheduleDisconnect`（`:621`）处于同一同步 turn**：将来若在两步之间插入 `await`，`false` 不再蕴含「定时器已 arm」——**届时的表现是「要么 green（arm 仍落在下一次 `advance(...)` 之前）、要么下游断言以 `AssertionError` 假失败报警（定时器被孤立）」；它绝不会静默掩盖一次坏掉的 release**，须同时改屏障或加观测缝。两条前提已写进 JSDoc，并与本节 **(B) 残余签名**互为因果。
- **核对**：`sed -n '303,307p' test/integration/orchestrator-disconnect-grace.test.ts`；`grep -n "waitForTerminalDisconnect" test/integration/orchestrator-disconnect-grace.test.ts`（应命中 4 个调用点 `:39`/`:119`/`:168`/`:300` + 1 个定义 `:478`）。
  - **修后残余失败的观察口径（本批改写；原指引「看该行的 `AssertionError`」已过时）**：修后屏障会**先于** `:303-307` 报错，残余失败**只有两种签名**，都**不再**是「屏障正常工作时」的那条 owner 断言：
    - **(A) 屏障卡住**（谓词 5s 内始终不为 `false`，例如 close 根本没被服务端处理）⇒ 因屏障预算 `5_000` 与 vitest 默认用例预算 5s **相等**，通常先撞**外层** `Error: Test timed out in 5000ms.`（落在 test 声明行，如 `:26`）；若外层预算被抬高而屏障先到期，则报 `vi.waitFor` 内**最后一次谓词错误**（`AssertionError: expected true to be false // Object.is equality`，落在谓词行），**不是** `Timed out in waitFor!`——vitest 的 `waitFor` 在 `handleTimeout` 里是 `let error = lastError; if (!error) error = new Error("Timed out in waitFor!")`，合成文案只在谓词从未抛错时出现（`node_modules/vitest/dist/chunks/test.DNmyFkvJ.js`；vitest `4.1.9`，chunk 名随升级漂移故带版本锚点）。
    - **(B) 屏障被绕过**（蕴含被破坏：见上一条②，或有人把谓词反转 / 改成别的可等待条件）⇒ 屏障提前放行，**回到 D10 的原签名**：`AssertionError: expected { paneId: 'wB:p-owner', … } to be null`（`:123` / `:174` / `:306`）。
  - **可复跑形状（本批实测；不依赖任何 `/tmp` 路径）**：
    - (A) 的最小复现：临时用例 `await vi.waitFor(() => expect(true).toBe(false), { interval: 10, timeout: 300 })` ⇒ 本批实测报 `AssertionError: expected true to be false // Object.is equality`，栈标 `❯ vi.waitFor.interval <文件>:<谓词行>`，**无** `Timed out` 文案。
    - (B) 的最小复现：`cp` 一份 `test/integration/orchestrator-disconnect-grace.test.ts`，把 `waitForTerminalDisconnect` 谓词里的 `).toBe(false),` 改成 `).toBe(true),`（等价于「蕴含被破坏」）再 `pnpm vitest run <副本>` ⇒ 本批实测 `Tests 4 failed | 6 passed (10)`：`:123`/`:174`/`:306` 三条报上述 owner `AssertionError`，心跳那条（屏障被卡满 5s）报 `Test timed out in 5000ms` 于 `:26`。**两个临时副本（`test/integration/tmp-d10-signature.test.ts`、`test/integration/tmp-waitfor-signature.test.ts`）已删除**，收尾 `git status --porcelain -uall` 只余 8 个预期修改文件（6 个原有 + 本批 B 的 2 个）。
  - **复跑形状（形状本身）**：同 **D7「核对」段**（3 路并发全量 + 记 `uptime` 负载档）；判定字段＝`Test timed out in 5000ms`（A 型）或 `AssertionError: expected { paneId: … } to be null`（B 型）。

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
- **状态（订正 2026-10-01，`chore/ledger-hygiene-d12-d15`；原记「挂账（台账卫生）」）**：**部分已修**。
  - **范围（已处置）**＝**本台账自身**：① **18 处 `/tmp` 命中 → 10 个锚点，逐条处置**（D12.2）；② 本台账引用的**计数 / 耗时 / 行号 / 哈希 / 提交号**（D12.3）；③ 散落在 D17(c)/(g)/(g-1)/(i) 与各条正文里的**旧锚点**，**收成一处**（D12.4）。
  - **残留（未处置）**＝`.agents/notes/` 下**其它 8 篇**笔记的 **24 处** `/tmp` 锚点（D12.5）：按项目 README「不可变原则」+ 本批边界（**只动台账文字**）**不改写**，仅在本台账登记清单与理由。
  - **纪律**：本段**只动锚点/数字与其订正说明**——不改写历史判断、不覆盖既有结论、不重排表格；所有改变均以「订正」标出，原值保留在正文或本表「旧锚点」列内。
- **订正前状态（原文保留，便于核对）**：「**挂账（台账卫生）**——待统一整改：把 `/tmp` 路径锚点改为**「可复跑形状」**（命令 + 进程数 + 负载档 + 判定字段）与**仓库内**路径/命令。」
- **核对（本批两时点实测；格式＝开工时 → 本批后）**：`grep -rn "/tmp/" .agents/notes/ | wc -l` ＝ **42 → 55**（均为 **9 篇**；**增量全在本文档自身**）；`grep -c "/tmp/" .agents/notes/20260930-terminal-event-delivery-open-items.md` ＝ **18 → 31**；⇒ **其它 8 篇的 24 处两时点不变**（就是 D12.5 的残留）。本台账内 `file:line` 锚点共 **98 个**（去重），已按**现树行内容**逐一打印判读（口径见 D12.5）。**两时点差值的原因：本段为了处置旧锚点而大量引用 `/tmp` 路径（引用旧锚点、标注历史值），不新增任何依赖**——这也是 D17(q) 记下的同一件事：**引用即命中**，故 `wc -l` 只能当“卫生度计”，不能当“残留计”。

##### D12.1｜处置判据（本批定死，二选一 + 两条补充）

- **(a) 重定向到现树可复算锚点**：给 `file:line`（**按内容在现树重新定位**，不照抄旧行号）+ 一条可直接粘的复算命令。适用于**仓库内文件**的锚点。
- **(b) 标注为历史值**：写明「该值在其提交/批次 X 时成立」并给**短哈希**；**值本身不改写**。适用于**测量值 / 已消失的 `/tmp` 取证 / 未提交中间态**。
- **补充①（形状优先于路径）**：`/tmp` 上的**探针/脚本**（一次性实验器材）无法「重定向到仓库」，处置＝把其**可复跑形状**（命令 + 判定字段 + 负载档）内联到 D12.2 的「探针形状」块；其数字按 (b) 记为历史值。
- **补充②（跨笔记边界）**：`.agents/notes/` 下**本台账以外**的笔记**不做改写**（不属「只动台账文字」），只在 D12.5 登记＝**残留**。

##### D12.2｜`/tmp` 锚点处置表（本台账 18 处命中 → 10 个锚点，逐条）

| # | 台账内锚点（行） | 处置 | 结果 |
| --- | --- | --- | --- |
| 1 | `/tmp/i-test.log`（`:117`、`:133`） | (b) 历史值 | 文件**已删**（前序批次收尾清理 `/tmp`）；值＝该处内联三行摘录，**适用时点＝2026-09-30 15:44、适用树＝`5433525` 之后 / `1136729` 之前**；值不改写 |
| 2 | `/tmp/herdsman-clean-duplicates-2NEZvC/herdsman.pid.instance.lock`（`:119`） | (b) 历史值 | 是**报错原文**里的临时目录名（引用，不是取证锚点）；该 temp dir 已不存在；原文保留 |
| 3 | `/tmp/scout-d6/ptrace.sh`（`:198`、`:202`） | (a) 形状内联 | 见下「探针形状」**P1** |
| 4 | `/tmp/scout-d6/probe2.ts` + `current.ts`/`s1.ts`/`refined.ts`（`:377`、`:399`） | (a) + (b) | 形状见 **P2**；**三版源码副本改为用仓库内提交复算**（本批订正）：旧代码＝`git show b012ba6:src/daemon/process-manager.ts`、精细版＝`git show 2555b2b:src/daemon/process-manager.ts`；**S1 是未提交中间态 ⇒ 不可复算，按 (b) 记为历史值** |
| 5 | `/tmp/scout-d6/tstate.sh`（`:398`） | (a) 形状内联 | 见 **P3** |
| 6 | `/tmp/lockrace-docs/measure.ts`（`:289`、`:336`） | (b) 历史值 + 形状 | 值＝D4/D2 两表内数字；**适用树＝`b012ba6`（即台账所称「现行」；该批已提交）**；形状＝两臂各取 `git show <rev>:src/daemon/process-manager.ts` 副本、同轮交错（顺序对调）、n=25/臂/轮、判定字段 p50 / max |
| 7 | `/tmp/lockrace-close2/item2/run.sh`（`:327`） | (b) 历史值 | 上界 1958/1959/1958ms（收紧前）→ 1000/1000/1000ms（收紧后）；**适用树＝`b012ba6`** |
| 8 | `/tmp/lockrace-close2/warnprobe/`（`:426`） | (b) 历史值（**已被取代**） | D1 告警分支**现有自动化用例** ⇒ 现树锚点＝`test/unit/daemon-process-manager.test.ts:1322`（用例①）/ `:1365`（用例②），命令 `grep -n "children file warns exactly once\|leaves release watching" test/unit/daemon-process-manager.test.ts` |
| 9 | `/tmp/lockrace-final/**`（`:413`、`:419`） | (b) 历史值 | 3 路并发 6/6 超时、HEAD 5009–5028ms、现行 5016–5029ms；**适用树＝`61f3295` 之前**（该 chore 已提交、超时已抬到 `30_000` ⇒ 复现该签名须先移除第三参，同段已注明） |
| 10 | `/tmp/d10-fix-probe/**`（`:458`） | (b) 历史值 | 形状**已在该段内联为可复跑步骤**（`cp` 副本 + 替换 `socketTick()` 体）；数字为历史值 |

- **探针形状（把 P1–P3 从 `/tmp` 脚本改成可直接重敲的形状；`/tmp` 被清后按此重建）**：
  - **P1（A7「ptrace 不是 `T`」）**：`cat /proc/sys/kernel/yama/ptrace_scope` → 起一个长跑（或持锁）进程 → `strace -p <pid>` 附着 → 读 `/proc/<pid>/stat` 的 state（**实测仍是 `S`**）→ **在附着状态下**再 `kill -STOP <pid>` → 再读（**实测小写 `t`**；原表达式只排除 `Z`/`X`/`T` ⇒ 小写 `t` 本来就判活）。
  - **P2（D6 三形状）**：`pnpm exec tsx <probe> <process-manager 副本> <A|B|C> <1|2|3>`；副本＝`git show b012ba6:…`（旧）/ `git show 2555b2b:…`（精细版）；形状 A＝READY 后真 `SIGSTOP`、B＝pre-READY 真 `SIGSTOP`（**须确定性配方**：fake 慢 `sh` + 真 `SIGSTOP`）、C＝release 侧真 `T`；判定字段＝`handle` / 耗时 / `helpersSeen` / `readyEverAtEnd` / `lockHeldWhileFrozen`。
  - **P3（A7「OS 事实」）**：`flock -x -n f.lock sh -c 'exec cat' &` → `kill -STOP` 持锁者（父与子均呈 `T`）→ `flock -x -n f.lock true`（**预期 `exit=1`，锁仍被持**）+ `kill -0 <pid>`（成功）→ `kill -KILL` 后 `flock -x -n f.lock true`（预期 `exit=0`）。

##### D12.3｜引用数字处置表

**① 现树可复算（本批一手重算，命令可直接粘）**

| 台账写法 | 现树值（2026-10-01） | 复算命令 / 现树锚点 |
| --- | --- | --- |
| `53 文件` | **53** | `git ls-files 'test/**/*.test.ts' \| wc -l`（口径同 `vitest.config.ts:18`） |
| `840 用例`（现基线） | **840**（＝**53 文件 / 840 用例**，本批 `pnpm test` 实测，`EXIT=0`；本批**未增删用例**，±0） | `pnpm test` |
| `33 处` / `32`（D13 口径） | **31**（本批再 −1：`herdr-socket-client` 那一处**换了写法**，不再命中该 grep 形状） | `grep -rn "setTimeout(resolve," test/ \| wc -l` |
| `/tmp` 命中（全局 / 本台账） | 开工时 **42 / 18**（9 篇）→ 本批后 **55 / 31**；**其它 8 篇恒为 24** | `grep -rn "/tmp/" .agents/notes/ \| wc -l`；`grep -c "/tmp/" <本台账>` |
| 显式逐用例预算（`30_000`/`45_000`/`120_000`/`10_000`） | **41 处命中**（含 `}, 10);` 这类**非** vitest 第三参，口径提示见 D16；**fix-round 新增 1 处**＝ `test/unit/daemon-process-manager.test.ts:775`，值 40 → 41） | `grep -rnE "\}, [0-9_]+\\);" test/` |
| `1000ms`＝`ACQUIRE_WINDOW_MS` | `src/daemon/process-manager.ts:495` | `grep -n "ACQUIRE_WINDOW_MS = " src/daemon/process-manager.ts` |
| `100ms`＝release best-effort 预算 | `:703` | `grep -n "Date.now() + 100" src/daemon/process-manager.ts` |
| `4 次`＝`ACQUIRE_MAX_ATTEMPTS` | `:515`（重试间隔 `:516`） | `grep -n "ACQUIRE_MAX_ATTEMPTS\|ACQUIRE_RETRY_DELAY_MS" src/daemon/process-manager.ts` |
| `200 轮`（压测规模） | 用例名内含；用例声明 `test/unit/daemon-process-manager.test.ts:1450` | `grep -n "200 rounds" test/unit/daemon-process-manager.test.ts` |
| `26 个 busy-loop`（高载配方） | 沿用：`for i in $(seq 26); do ( while :; do :; done ) </dev/null >/dev/null 2>&1 & done`（本机 **12 核**，暖机 90s）；本批实测 1 分钟 loadavg **21.43** / **24.43 → 26.16 → 26.65**（两次同配方轮次，落在 D9 原始档 **21–34**） | `cat /proc/loadavg`；`uptime`；原始输**已就地内联**（见 D15 段；临时文件已在本批收尾清理） |
| `2s`＝`waitForNotification` 总预算 | `test/integration/rpc-test-client.ts:71`（1000 × 2ms）+ 轮询 `:75`；本批高载实测峰值 **6ms**（D15） | `grep -n "attempts = 1_000\|setTimeout(resolve, 2)" test/integration/rpc-test-client.ts` |
| `5s` 默认预算 / `20s` 内部上界 / `10_000` / `30_000` / `45_000` / `120_000` | 现树可复算（锚点见 D12.4） | `grep -n "timeout: 20_000\|}, 45_000);\|}, 10_000);" test/unit/daemon-service.test.ts` |
| 提交号 `b59ac96` / `5433525` / `7c4a36b` / `ce17071` / `d03c3a5` / `0a1d6b7` / `1136729` / `b012ba6` / `2555b2b` / `61f3295` / `b6c3b87` / `105286f` / `5b7181b` | 全部存在且标题可查 | `git log -1 --format='%h %ad %s' --date=short <hash>` |
| amend 前哈希 `233b106` / `34763e6` | **对象仍存在**（message 与 H1 / Phase 1 同标题，可 `git show`） | `git cat-file -t 233b106`；`git log -1 --oneline 233b106` |

- **批次 → 提交映射（本批一手核对；台账多处写「未提交」，现已全部落在 `main`，是 D12 的主要订正依据）**：H1＝`b59ac96`、Phase 1＝`5433525`（共同基线 `7c4a36b`）；**A5/A8（errno 分流）＝`1136729`**（merge `ce17071`）；**D1–D5（release 屏障 + 有界重试）＝`b012ba6`**；**D7 + D9–D12 文档轮＝`61f3295`**；**D9/D10 修复 + D13–D16 + `[MARK-FIX-SHOULD2]`＝`b6c3b87`**；**D6（判活相位化）＝`2555b2b`**；**D13③/D14/D16/D17 两轮＝`105286f`**；**D8 覆盖＝`0a1d6b7`（本批 base）**；`5b7181b`＝daemon status 监督事实位（**本文件行号漂移的主因**，插在判活/锁函数之前）。核对：`git log --oneline -- .agents/notes/20260930-terminal-event-delivery-open-items.md`。**口径差订正（2026-10-01，同日第二轮 fix-round；来源＝oracle）**：`[MARK-FOLLOWUP-LEDGERTIMING2]` 那一批的**台账文字自称「未提交」**，但**其实体内容已随 `105286f` 落地**（本批一手核：`git show 105286f:test/unit/daemon-service.test.ts` ＝ `}, 45_000);` `:639`、`}, 10_000);` `:151`/`:216`；`git show 105286f:test/unit/daemon-process-manager.test.ts` ＝ `}, 10_000);` `:745`；该 commit 同时含整段 FOLLOWUP 台账文字）⇒ **后人不要按「未提交」去寻找中间态**（不存在「提交前的中间版本」）；批次名与 commit 的对应关系以本行及 D12.4 为准。

**② 历史值 @ 提交（值不改写；给适用提交/批次）**

| 数字（台账写法） | 适用批 / 提交 | 备注 |
| --- | --- | --- |
| 用例数 `818` / `820` / `821` / `822` / `823` / `838` / `840` 与各处 `53 文件` | 各批当值 | 本批仍为 **53 文件**；用例数见 D12.3① |
| D3 上界 `1958/1959/1958ms` → `1000/1000/1000ms` | `b012ba6` | 病理构造实测；本批未重算 |
| D4 失败延迟 `p50 1.99–2.13ms → 11.03–11.06ms`（max ≤2.78 / ≤11.70ms） | `b012ba6` | 同上 |
| D1 残余率 `1/6000` | `b012ba6`（引自收口轮） | 本批未复现 |
| D7 增量 `+349.5ms（+33.5%）` / `+319.5ms（+7.9%）` / `+320~350ms` | `b012ba6` | 同轮交错互比 |
| 200 轮压测单跑 `1.33s`（`1316/1325/1351ms`）、2 路 `4.0–4.4s`、3 路 `6.3–6.7s`、高载 3 路 `10.7–11.5s` | `61f3295` | 本批高载单跑一手值（`5163ms` / `5418ms`）见 D15，以**追加**形式并存 |
| D9 用例 3 路低载 `2560ms`；`51%（灰区上沿）` | `105286f` | 本批一手：低载单文件 **327ms**、高载单文件 **1736ms**（D15） |
| `4014ms` / `3025ms`（隔离单跑）、`4084ms` / `3038ms`（低载 3 路，占用 82% / 61%） | `b6c3b87` | 未重算 |
| 高载屏障实耗 `144/113/101/125ms`、重试循环 `198ms`（占用 2–4%） | `b6c3b87` 之前（引自 oracle 轮） | 本批高载一手新值见 D15 |
| flock 调用计数 `400` / `433` / `485` / `463` / `511` | `b012ba6` | 两轮口径未统一（段内已如实登记） |
| D6 三形状 `7–8ms` / `55–66ms` / `60–66ms` / `62/62/64ms` / `1000/1000/1000ms`、`releaseMs 0/1/0` | `2555b2b`（精细版）/ `b012ba6`（旧） | **S1 臂属未提交中间态 ⇒ 不可复算** |
| A5 失败率 `3–7%`、`50ms 后重试 100% 成功` | 引自 oracle R5 复核 | 台账原已标「本批未独立复现」；现统一按 (b) 记 |
| 锁 flake 复现率 `串行约 10%/轮`、`2 条并发约 20%/轮` | `b012ba6` 之前的侦察轮 | 同上 |
| `55 文件 / 842 用例`（另一次独立复核） | 临时副本（含额外测试文件） | 台账已注明「已被第一手测量取代」 |

**③ 不可复算 / 无落盘证据（残留，如实登记，不自行补值）**

- **A5(a)** 的 18:13 / 18:26 那次失败：输出未落盘、用例名未捕获（台账已注明「已知缺口」）。
- **errno 从未落盘**：A5(a)/(b) 两次观测都未捕获 `/proc` 读失败的具体 errno（A5「更新」段已注明）。
- **`/tmp/i-test.log` 的三行摘录**：文件已删，值保留在正文（不可复算）。
- **A8④ 的 `≈95 万次读 / 0 次空读`** 与 **oracle 各轮的全部数字**（高载 3 路 3/3 全绿、屏障点实耗等）：**引自他轮报告，本批无原文**，已在行内标注来源。
- **S1 中间态的一切数字**：未提交 ⇒ 不可复算。

##### D12.4｜锚点位移映射（唯一权威表；各条正文里只留指针）

> 口径：`旧 file:line` @ 适用提交/批次 → **现 `file:line`（2026-10-01 工作树，按内容核对）**。核对手法＝脚本把台账里每个 `file:line` 的**现树行内容**打印出来逐一判读（口径见 D12.5），符号/引文匹配即算命中。

**`src/daemon/process-manager.ts`**（漂移主因＝`5b7181b` 在本文件判活/锁函数**之前**插入 daemon status 监督事实位代码）

| 旧锚点（台账原记） | 现锚点（按内容核对） |
| --- | --- |
| D1 行 `:312-333`（读 `:312`、告警 `:326`） | 读 `readChildPids` **`:401`**、catch **`:407`**、告警 **`:413-417`**（`console.warn` **`:415`**、文案 **`:416`**）；命令 `grep -n "readChildPids\|children is unavailable" src/daemon/process-manager.ts` |
| A8 的 `sed -n '313,320p'` | **`:478-490`**（`const code` **`:483`**、`return code !== "ENOENT" && code !== "ESRCH"` **`:484`**） |
| A7 位置 `:293-324` / `:293-328`；判活行 `:312` | `isChildProcessActive` **`:445-490`**；判活行 **`:476-477`**（`pausedCountsAsActive` `:476`） |
| D6 `type ChildLivenessPhase` `:349`（后记 `:354`）、签名 `:351-355`/`:356-361`、JSDoc `:334-348`/`:334-353`、函数内注释 `:367-376`/`:372-381`、判活行 `:378-379`/`:387-388` | `type` **`:443`**、签名 **`:445-450`**、JSDoc **`:427-442`**、函数内注释 **`:461-475`**、判活行 **`:476-477`** |
| D6 其余：`ACQUIRE_WINDOW_MS` `:397`/`:406`、READY 分支 `:486`/`:495`、pre-READY 调用点 `:504`/`:513`、放弃判定 `:515`/`:524`、release 屏障 `:607`/`:616` | `ACQUIRE_WINDOW_MS` **`:495`**、READY 分支 **`:584`**、pre-READY 调用点 **`:602`**、放弃判定 **`:613`**、release 屏障 **`:705`** |
| A5 区段 `:296-398`；`if (!isChildProcessActive(child.pid)) break;` `:345-347`；SIGKILL 判定 `:357`（kill `:359`）；`throw` `:426`/`:451`/`:457` | `acquireFlockHandle` **`:635-733`**；break 判定 **`:602-604`**；SIGKILL 判定 **`:613-616`**（组 kill **`:615`**）；错误文案 **`:739`**、`throw` **`:745`** |
| D3 `:371` / `:452` / `:515` | 窗口钳制 **`:576`**、attempt 循环 **`:647`**、窗口耗尽 `return null` **`:674`**、抛错 **`:745`** |
| D4 `:391`（`ACQUIRE_MAX_ATTEMPTS`） | **`:515`** |
| D2 `:574`（release 100ms） | **`:703`** |
| D8 现树锚点 `:401` / `:408` / `:413-417` / `:388` / `:588` / `:705` / `:343` / `:352` | **仍命中**；**订正**：catch 是 **`:407`**（`:408` 是 catch 内注释首行，D8 段写 `:408` 属 off-by-one） |

**`test/unit/daemon-process-manager.test.ts`**（漂移＝`0a1d6b7` 批 **+185 行**、本批 **+17 行**（`git diff --numstat`：`+20 −3`；1546 → **1563** 行）= `:788` 起 **+7**（SIGKILL 用例）/ `:1388` 起 **+8** + `:1442` 起 **+2**（D1 降级用例；另两处同行替换净 0），**同日第二轮 fix-round 再 +27 行**（1563 → **1590**；`:739` 起 **+17** / `:1396` 起 **+18** / `:1506` 起 **+27**）；下表已换成**最终（fix-round 后）**行号，旧值在括号里标注

| 旧锚点（台账原记） | 现锚点（按内容核对） |
| --- | --- |
| D13③ 固定窗口 `:518`（＝pre-batch 实测 `:728`） | 该窗口已被轮询取代；等价锚点＝屏障 **`:817-819`**（批 3＝ `:800-802`）。**实测轨迹（本批一手，见下表）**：固定窗口 `:517` @ `5433525`–`b6c3b87` / **`:518` @ `2555b2b`** → **`:727-728` @ `5b7181b`** `b6c3b87`（`setTimeout(resolve, 50)` 在 `:728`）→ **本批（`105286f`）替换为屏障 `:730-732`** → `:793-795` @ `0a1d6b7` → `:800-802` @ 本批（批 3）→ **`:817-819` @ 本批 fix-round**（**注**：旧文一处把替换归因于 `61f3295`，实测应为 **`105286f`**；`61f3295` 只改了压测第三参） |
| D17(a)/D13③ 屏障 `:730-732` | **`:817-819`**（批 3＝ `:800-802`） |
| D13 ② 第三参 `:745` | **`:832`**（`}, 10_000);`；用例声明 **`:777`**）（批 3＝ `:815`/`:760`）；旧值 `:745` 属 **`105286f`** 树（实测）。**本批 fix-round 另新增一处 `}, 10_000);`＝ `:775`（`two real concurrent…`），理由见 D17(r) / D16** |
| D17(f) 无界等待 `:725` | **本批已消除**，改为有界轮询 **`:810-812`**（`description: "the SIGKILLed flock holder to exit"`；批 3＝ `:793-795`） |
| D13④ 轮询 helper 的 sleep `:62` | **分段轨迹（订正于同日第二轮 fix-round；见 D17(c)①）**：`:62 @ 5433525`–`b6c3b87` → **`:63 @ 2555b2b`** → **`:64 @ 5b7181b`/`105286f`** → **`:127 @ 0a1d6b7`/本批**（本批 `grep`：该文件内此形态**仅 1 处**，fix-round 未动它；**原写「`:64 @ 2555b2b`」错 1 行，已订正**） |
| D7/D11/D16 的 `:1011`（旧树即已漂移） | 用例声明 **`:1468`**；第三参 `}, 30_000);` **`:1576`**（批 3＝ `:1450`/`:1549`） |
| D6「反证」落点 `:1020` | **`:1323`**（`expect(helperPids.length).toBeGreaterThanOrEqual(2);`；批 3＝ `:1306`） |
| A5「本批的回归用例 `:645-678`」 | **`:1181-1215`**（用例 `a failed or truncated /proc/<pid>/stat read…`，声明 **`:1181`**；`EIO` 注入 `:1202-1204`；批 3＝ `:1164-1198`，声明 `:1164`）；**旧值 `:645` 实测确属 `1136729` 树**（本批脚本核对） |
| A5「200 轮压力用例顺移至 `:680`」 | **`:1468`**（旧值 `:680` 实测属 **`1136729`** 树；批 3＝ `:1450`） |
| D8 用例① `:1315` / 用例② `:1358` | **`:1339`** / **`:1382`**（`test.skipIf` 行）、`:1383`（用例名）（批 3＝ `:1322` / `:1365` / `:1366`） |
| D6 两条 T 态用例 `:912-937` / `:913-956` / `:958-1030`（D17(g-1) 已判旧树 `:1129`/`:1174`） | post-READY **`:1216`** / pre-READY **`:1261`**（批 3＝ `:1199` / `:1244`） |
| D6 引的相邻用例体 `:624-688` | **`:928-992`**（**按用例标题核对**：＝ `flock handle release waits for every helper process that shares the lock fd`；旧区间起点是空行、±1 行；批 3＝ `:911-975`） |
| mock 缝 `:47` / `:57`、`flockPidOfLastChildrenRead` `:87`、`waitForCondition` 定义 `:114` | **仍命中**；本批**新增调用点一处**＝ `:793`（其后既有的 flock 屏障 `:800-802` **方法未变**、只随行号位移）；**fix-round 后再 +2 处**＝ `:758` / `:761`（`two real concurrent…` 的两处 reap，并在**本档内首次**把测试级第三参从默认抬起，见 D16）⇒ `waitForCondition` **调用点现共 12 处**＝ `:657` / `:758` / `:761` / `:810` / `:817` / `:881` / `:897` / `:1196` / `:1255` / `:1329` / `:1377` / `:1456`（本批 `grep` 核对；批 3 的 10 处＝ `:657` / `:793` / `:800` / `:864` / `:880` / `:1179` / `:1238` / `:1312` / `:1360` / `:1438`；旧文里「4 处既有用法」是**部分列举**） |

**跨提交位移轨迹（本批一手实测；口径＝按行内容在各 rev 的 `git show <rev>:<file>` 里定位行号，**脚本形状见 D12.5**）**——D17(c) 要求的「分段轨迹」即此表：

| 标记（行内容） | `5433525` | `b012ba6`/`61f3295`/`b6c3b87` | `2555b2b` | `5b7181b` | `105286f` | `0a1d6b7` | 本批（含 fix-round） |
| --- | --- | --- | --- | --- | --- | --- | --- |
| **文件总行数** | 756 | 1023 / 1025 / 1025 | 1145 | 1354 | 1361 | 1546 | **1563**（批 3）→ **1590**（fix-round） |
| 本段位移量 | — | +267（@`b012ba6`） | **+1** | **+209** | **+7** | **+185**（头部 +63 / 末尾 +122） | **+17**（`:788` +7 / `:1388` +8 / `:1442` +2；批 3）+ **+27**（fix-round：`:739` +17 / `:1396` +18 / `:1506` +27） |
| `flock held … SIGKILL`（用例名） | 487 | 487 | 488 | 697 | 697 | 760 | **777**（批 3＝ 760） |
| `child.on("exit", resolve)`（无界等待） | 515 | 515 | 516 | 725 | 725 | 788 | **已消除**（批 3 `:793-795` → fix-round **`:810-812`**） |
| `Brief settling delay for kernel cleanup`（D13③ 固定窗口） | 517 | 517 | 518 | 727 | **已消除**（→屏障 `:730-732`） | →`:793-795` | →`:800-802`（批 3）→ **`:817-819`** |
| `}, 10_000);`（SIGKILL 用例第三参） | — | — | — | — | 745 | 808 | **832**（批 3＝ 815）；**另一处新增**：`:775`（`two real concurrent…`，同理由，见 D16/D17(r)） |
| `}, 30_000);`（压测第三参） | — | 1011（`61f3295`/`b6c3b87` 时为 1011） | 1131 | 1340 | 1347 | 1532 | **1576**（批 3＝ 1549） |
| `a failed or truncated /proc…`（A5 用例） | — | — | — | — | — | 1157 | **1181**（批 3＝ 1164） |
| `a failed or truncated /proc…`（@`1136729` 首次出现） | — | **645 @ `1136729`** | — | — | — | — | — |
| `stress test: 200 rounds`（压测用例名） | 645 | 912 | 1032 | 1241 | 1248 | 1433 | **1468**（批 3＝ 1450） |
| `a simulated T-state…`（post-READY 用例） | — | — | 913 | 1122 | 1129 | 1192 | **1216**（批 3＝ 1199） |
| `an unreadable children file…`（D8 用例①） | — | — | — | — | — | 1315 | **1339**（批 3＝ 1322） |
| `async function waitForCondition(`（helper 定义） | 49 | 49 | 50 | 51 | 51 | 114 | **114**（fix-round 未动） |

- **本表暴露的往事（供后续批次）**：① **`5b7181b`（daemon status）是 dpm 测试文件的第二个大位移源（+209）**，与 D8 批（+185）同量级——旧文普遍只记得 D8 批；② `105286f` 只 +7，`2555b2b` 只 +1（它把 +119 加在本表多数标记之后）；③' **本批在同一天对同一文件连加两轮位移**（批 3 **+17** + fix-round **+27**）是**本档首次由同一批次**在同一文件上连加两次 ⇒ 引用批 3 中间值时按 `:739` 起 +17 / `:1396` 起 +18 / `:1506` 起 +27 回推；herdr 文件按 `:254` 起 +3（断言行前）/ 文件尾 +8 回推；③ 两个「已知旧值」经实测各归各树：`}, 10_000);` 的 `:745` 属 **`105286f`**，A5 用例的 `:645` / 压测的 `:680` 属 **`1136729`**。⇒ 后续写锚点时**必须带树名/提交号**，否则差一个批次就差几十到几百行。

**`test/unit/daemon-service.test.ts`**（`105286f` 之后行号；本批未动此文件）

| 旧锚点（台账原记） | 现锚点（按内容核对） |
| --- | --- |
| D9 用例声明 `:523` / 第三参 `:615` | **`:544`** / **`:639`**（`}, 45_000);`） |
| D13③ 沉降错位 `:135` / `:190` | 已改形状：屏障 **`:138-145`**（断言 **`:142`**）/ **`:204-211`**（断言 **`:208`**；用例声明 **`:182`**） |
| `:197-209`（第二轮映射目标） | **`:200-211`** |
| `:130` / `:132-146` / `:138-146` / `:141` / `:151` / `:216` | **仍命中**（`:151` / `:216`＝两条 `}, 10_000);`） |
| 内部 `vi.waitFor` `:577-586`（`timeout: 20_000`）/ `Promise.race([childExit, 20s])` `:606-609` | **仍命中**（`childExit` 定义 `:570`、race 内引用 `:607`） |

**其它文件**

| 旧锚点（台账原记） | 现锚点（按内容核对） |
| --- | --- |
| `test/integration/herdr-socket-client.test.ts:257`（D13④「race 结果被丢弃」） | **本批改为显式断言**：**fix-round 后（最终）**块 **`:252-285`**（`try` `:254`、sentinel `:264`、`await Promise.race` `:265-276`、断言 **`:274`**、`finally` `:279-285` 内含 `controller.abort()` `:283` / `client.close()` `:284`）；批 3 值＝ sentinel `:261`、race `:262-269`、断言 `:271`、`close()` `:277`（**块内 `:257`–`:273` +3、之后 +8**，见本节末「新位移」条）；**该处已不再命中 `setTimeout(resolve,` 口径**（该文件内此形态＝ **0**） |
| `src/daemon/observability-server.ts` `:198` / `:613` / `:613-622` / `:618` / `:621` / `:624-636` / `:702-709` / `:727-729` / `:201-213` | **仍命中**（`#stopping = true` `:202`） |
| `orchestrator-disconnect-grace.test.ts` `:39` / `:119` / `:144` / `:149` / `:168` / `:179-210` / `:253` / `:300` / `:303-307` / `:306` / `:447-486` / `:478` / `:484` / `:494` | **仍命中** |
| `observability-rpc.test.ts` `:422` / `:483-492` / `:491` / `:765` / `:1054` | **仍命中** |
| `rpc-test-client.ts:71` / `:75`；`agent-index-service.test.ts` `:1035` / `:4809` / `:4889`；`herdsman-pi-extension.test.ts` `:4417` / `:4701` / `:4708` / `:4758`；`turn-completion-signal.test.ts` `:459` / `:485` / `:489`；`herdsman-pi-daemon-client.test.ts` `:160` / `:283` / `:375`；`herdr-session-watch-manager.test.ts` `:246` / `:929` / `:1237`；`final-audit-regressions.test.ts` `:37` / `:86`；`herdr-session-watch-idle-seam.test.ts` `:84` / `:282`；`herdsman-pi-turn-signal.test.ts` `:14` / `:42`；`event-dedup-pane-generation.test.ts:676`；`agent-history-discovery-regressions.test.ts:149`；`package-publication.test.ts:78`；`herdr-plugin-package.test.ts:72`；`orchestrator-pane-move.test.ts` `:139` / `:320` | **仍命中**（本批按行内容抽验；**未逐条判读的见 D12.5**） |
| `src/daemon/service.ts:163-164`；`src/db/agent-orchestrator-scopes.ts:254-262`；`packages/herdsman-pi/src/index.ts` `:976-987` / `:997-999`；`src/observability/agent-index-service.ts` `:1074` / `:1735-1752`；`vitest.config.ts:18` | **仍命中** |
| `.agents/notes/20260929-daemon-shutdown-budget.md:50`（引用未带目录前缀） | **仍命中**（本批解析到该文件并抽验＝「`daemon-process-manager` 偶发 flake」那条）；建议后续补全路径写法 |

- **本批自身造成的新位移（供后续批次；行数均本批 `git diff --numstat` + 行数对比实测）**：`test/unit/daemon-process-manager.test.ts` 本批**净 +17 行**（1546 → **1563**；`+20 −3`），**三个位移点**＝`:788` 起 **+7**、`:1388` 起 **+8**、`:1442` 起 **+2**（`:1400` / `:1426` 两处为**同行替换**、净 0）⇒ 该文件 **「:788 之后」的既有锚点 +7**、**「:1388 之后」+15**、**「:1442 之后」+17**；`test/integration/herdr-socket-client.test.ts` 在 `:257` 处**净 +14 行**（379 → **393**；`+15 −1`）⇒ 该文件 **`:257` 之后**的既有锚点 +14。**完整链条示例（可直接复算）**：D13③/SIGKILL 用例的第三参 `:745` @ **`105286f`** → `:808` @ `0a1d6b7`（+63）→ **`:815` @ 本批**（+7）；屏障 `:730-732` → `:793-795` → **`:800-802`**。
- **本批自身造成的新位移（**同日第二轮 fix-round**；行数均 `diff` 对比实测）**：`test/unit/daemon-process-manager.test.ts` **净 +27 行**（1563 → **1590**），**三个位移点**＝`:739` 起 **+17**、`:1396` 起 **+18**、`:1506` 起 **+27** ⇒ 该文件 **「:739 之后」+17**、**「:1396 之后」+18**、**「:1506 之后」+27**；`test/integration/herdr-socket-client.test.ts` 在 `:254` 起 **净 +8 行**（393 → **401**）——**块内不是均匀位移**：`:254` 的 `try {`（+1）与 `:260-261` 的诊断注释（+2）落在**断言行之前** ⇒ `:257`–`:273`（批 3）→ `:260`–`:276`（**+3**）；`finally` 与块尾在断言之后 ⇒ 断言行 `:271`（批 3）→ **`:274`**、文件尾 +8。**链条示例（可直接复算）**：D13② 第三参 `:745` @ `105286f` → `:808` @ `0a1d6b7` → `:815` @ 批 3（+7）→ **`:832` @ fix-round**（+17）；屏障 `:730-732` → `:793-795` → `:800-802` @ 批 3 → **`:817-819` @ fix-round**；本轮**新增**一个 `}, 10_000);`＝ `:775`（`two real concurrent…`，无同族旧值）。

##### D12.5｜残留（本批未处置）与复算口径

- **其它 8 篇笔记的 24 处 `/tmp` 锚点（未改写；本批两时点实测**恒定 24**）**：`20260930-daemon-lock-release-barrier.md`（5）、`20261001-release-0.13.2-post-release-docs.md`（7）、`20261001-production-install-channel-npm-registry.md`（4）、`20260930-phase1-delivery-latency.md`（3）、`20260930-terminal-event-delivery-h1.md`（2）、`20260929-remove-cli-daemon-lifecycle.md`（1）、`20261001-pi-confirmed-turn-frozen-baseline-empty-body.md`（1）、`wake-delivery-r4f1-remaining-risks.md`（1）。**理由**：项目 README「不可变原则」+ 本批边界「只动台账文字」。**特别注意**：`20261001-production-install-channel-npm-registry.md` 与 `20261001-release-0.13.2-post-release-docs.md` 里的 `/tmp/herdsman-global-backup-*.tgz` 是**有主回滚物**（清理时点另见该两篇），**不得**按「清理 `/tmp`」处置。命令：`grep -rc "/tmp/" .agents/notes/*.md`。
- **未逐条判读的测试锚点**（行号仍命中，但描述未在上下文里逐条复核）：`observability-rpc.test.ts` `:459` / `:773` / `:775`；`herdr-session-watch-manager.test.ts` `:976` / `:1014` / `:1051` / `:280` / `:322` / `:362`；`orchestrator-disconnect-grace.test.ts` `:149` / `:253`；`orchestrator-pane-move.test.ts:162`；`herdsman-pi-extension.test.ts:1488`；`turn-completion-signal.test.ts:485`。**风险低**（都在 ②/④ 类同族清单里，行号未变）；要坐实需逐条读上下文（本批未做）。
- **复算口径（本批用的锚点判读脚本）**：读本台账 → 正则抓出所有 `路径:行(-行)` → 对每条打印**现树该行内容**（多条以 `⏎` 连接、截断 110 字符）→ 人工判读是否仍是原文所述之物；不依赖旧行号、不写仓库文件。
- **复算口径（轨迹表）**：对每个 rev 取 `git show <rev>:<file>`，按**行内容**（`line.includes(marker)`）定位行号；本批跑了 **8 个标记 × 9 个 rev**（脚本为一次性产物，已随临时目录清理，形状即本行所述）。
- **本段引用的命令一律在仓库根执行**；标「本批一手」的为本批实测，其余标「适用提交」。

#### D13｜`setTimeout(resolve,` 的**全量 33 处**四类分列（含反向用法整组）

> **口径现状（2026-10-01）**：本段叙述与 33 处清单属**历史口径 @ `b6c3b87`**（可复算：`git grep -n "setTimeout(resolve," b6c3b87 -- test/ | wc -l`）；**现树＝ 31**，位移全在文末「**第三轮**」一条，逐处清单不再重排（不改写历史判断）。

- **枚举口径（本批第二轮改写：不再是手挑清单）**：`grep -rn "setTimeout(resolve," test/` 在本分支工作树上**全量命中 33 处**（`grep -rn "setTimeout(resolve," test/ | wc -l` = 33，本批实测）；下面把 33 处**逐处**归入四类，**无一省略**，四类计数相加 = 33（10 + 3 + 20）。读者可直接用上面那条命令复算行号。
- **四类定义（判定规则）**：
  - **① flake 源（已实测 / 已在本批改形状）**：sleep 到点后立刻断言一个**必然发生**的结果，且**没有**可等待条件兜底 ⇒ 负载下**假失败 / 超时**。D10 的 4 个屏障点与 `observability-rpc.test.ts` 的 `sleep(75)` 属此类，**已在本批改形状**，因此它们**不再是**「固定 sleep」命中（本段为 **0 处**；记账见 D10 段）。
  - **② 检出力下降（反向用法）**：sleep 到点后断言某事「**没发生 / 没增长**」⇒ 负载下只会**假通过**（漏检），**不会假失败**，也**不是 flake 源**。
  - **③ 固定窗口等一个必然发生的结果（假失败向）：2 处沉降错位 + 1 处固定窗口等结果（`:518`）**：沉降不足时后续**手动 tick / 基线清空**会错位 ⇒ **2026-10-02 订正**：原写「负载下**可能假失败**」**偏强**，准确动因是「**结构性假设：沉降必须在一个宏任务 turn 内完成**」——这两处 `daemon-service` 链路**全程只有微任务**（`src/daemon/service.ts:163-164` 的 `#inFlight = Promise.resolve().then(this.#run)`），旧形状在**微任务语义下与负载无关** ⇒ 本批替换属**加固 / 可读性**，**不是**「修已证实的负载 flake」。（订正依据＝oracle 本轮：规范论证 + 4 个 busy-loop 下旧形状 **10/10 全绿**；**本批未独立复现**。原写的「未实测，低强度」同向。）
  - **④ 其余无风险**：轮询 helper（自带 deadline + 断言）、夹具内部的流节奏、**结果被丢弃**的 settle。
- **② 检出力下降（10 处；断言的都是「没发生 / 没增长」）**：
  - `test/unit/daemon-service.test.ts:130`（`setTimeout 0` 沉降后 `expect(runs).toEqual(["started"])`＝断言**未重复启动**）
  - `test/unit/herdr-session-watch-manager.test.ts:929` / `:976` / `:1014` / `:1051`（40ms 后断言未重复订阅 / 未被处理）
  - `test/unit/final-audit-regressions.test.ts:86`（30ms 后 `expect(connections).toBe(1)`）
  - `test/integration/herdr-session-watch-idle-seam.test.ts:84`（80ms 后断言 idle 事件未被投递）
  - `test/integration/agent-index-service.test.ts:1035`（20ms 后 `expect(drained).toBe(false)`）
  - `test/integration/herdsman-pi-daemon-client.test.ts:283`（30ms 后 `expect(connectionCount).toBe(1)`）
  - `test/integration/turn-completion-signal.test.ts:489`（**本批补漏项**：100ms 后断言 `agent.done` 未被 emit、DB 无终态事件）
- **③ 固定窗口等一个必然发生的结果（3 处 = 2 处沉降错位 + 1 处固定窗口等结果 `:518`）**：
  - `test/unit/daemon-service.test.ts:135`（`resolveRun()` + 沉降后**手动 `tick()`**，再 `vi.waitFor` 等第二轮；沉降不足则该 tick 被丢弃、`waitFor` 无从自愈）
  - `test/unit/daemon-service.test.ts:190`（同形：断言重试 cadence 已恢复）
  - `test/unit/daemon-process-manager.test.ts:518` — 固定窗口等结果（本批未修）；**紧随 `child.on("exit")` 之后该 50ms 窗口基本冗余**，残余风险主要是探针 `spawnSync("flock")` 失败，而非 50ms 窗口本身。
- **本批（2026-10-01，`fix/ledger-test-timing`）逐处落点与改法**（锚点均为**现树**，逐处按「内容描述」重新定位；括注为台账旧行号）：
  - `test/unit/daemon-service.test.ts:132-146`（台账 `:135`，**未位移**；用例 `:110`）：`resolveRun?.()` 之后的「固定 0ms 沉降 + 单次 `tick?.()` + `vi.waitFor(...)`」→ **`vi.waitFor(async () => { tick?.(); await new Promise((r) => setTimeout(r, 0)); expect(runs).toEqual(["started", "started"]); }, { interval: 10, timeout: 5_000 })`**（`expect` 在 `:142`）。断言**逐字保留**；内层 `setTimeout(0)` 由**等待窗口**降级为**让步**。
  - `test/unit/daemon-service.test.ts:197-209`（台账 `:190`，**未位移**；用例 `:179`）：同形，断言 `expect(runs).toEqual(["attempt", "attempt"])` 在 `:205`，**逐字保留**。
  - `test/unit/daemon-process-manager.test.ts:730-732`（台账 **`:518` → 现树屏障 `:730-732`**，位移 **+210**；紧随 `child.on("exit")` 之后）：`// Brief settling delay for kernel cleanup` + `setTimeout(resolve, 50)` → **`waitForCondition(() => !isFlockHeld(lockPath), { description: "the killed holder's flock to be observable as free" })`**（与同文件 `:594` / `:1106` / `:1165` / `:1239` 四处既有用法同形；默认 `intervalMs` 20 / `timeoutMs` 5000）；原紧随的 `expect(isFlockHeld(lockPath)).toBe(false);`（现 `:735`）**保留**。
  - **为何 `daemon-service` 两处轮询内仍留一次 `setTimeout(resolve, 0)`**：`tick?.()` 只把 run 排成**微任务**（`src/daemon/service.ts:163-164` 的 `#inFlight = Promise.resolve().then(this.#run)`），**同一同步轮内看不到它的效果**；若不在轮内让步，成功那一轮之后的**下一轮会多 tick 一次**——在 `:179` 那条用例里会让「第二次失败」之后**多跑一个未被断言的 attempt**，并可能把 `console.warn` 打到已 `mockRestore` 的**真实 console**。让步是**轮询内部的一步**，不是判定窗口：超时由 `vi.waitFor` 给出（到期以最后一次谓词错误失败，不会静默通过）。
  - **未放宽断言的证据（可复算）**：`git diff -U0 -- test/ | grep -E "^[-+].*expect"` 只输出 4 行——两条被删的 `await vi.waitFor(() => expect(runs).toEqual([...]))` 与两条新增的 `expect(runs).toEqual([...])`，**匹配器与期望值逐字相同**；无 `retry` / `skip` / `only` 引入、无用例增删（改前改后 `pnpm test` 均 **53 文件 / 838 用例**）。
  - **第二轮（2026-10-02，`[MARK-FOLLOWUP-LEDGERTIMING2]`）｜预算与锚点更新**：三处的**轮询预算仍为 `5_000`**（D16 口径不变），但**三处用例各加了测试级第三参 `}, 10_000);`**（oracle should-fix S1）——落点 `test/unit/daemon-service.test.ts:151`（`skips ticks…`）、`:216`（`start is idempotent…`）、`test/unit/daemon-process-manager.test.ts:745`（`flock held by child process…`）；**独立理由＝轮询到期必须早于测试级超时，谓词错误才报得出来**（否则只剩 `Test timed out in 5000ms`）。**未改** `vitest.config.ts`。
  - **第二轮锚点位移（本批自身造成）**：`test/unit/daemon-service.test.ts` 两处轮询块整体后移——`:132-145`（断言 `:142`，**未变**）与 **`:200-211`**（原 `:197-209`；断言 **`:208`**，原 `:205`；用例声明 **`:182`**，原 `:179`）；两处轮询内**宏任务让步**现为 **`:141`** / **`:207`**（原 `:141` / `:204`）。`daemon-process-manager` 的兄弟调用点因该文件净 **+3 行**位移为 **`:810`** / **`:1109`** / **`:1168`** / **`:1242`**（原 `:807` / `:1106` / `:1165` / `:1239`；`:594` 未变）。
  - **账目（可复算）**：见 **D17（b）**（`33 − 3 + 2 = 32`）。
  - **第三轮（2026-10-01，`chore/ledger-hygiene-d12-d15`）｜口径 32 → 31**：本批把 **④** 里的 `test/integration/herdr-socket-client.test.ts:257`（「`Promise.race([iterator.next(), 50ms])` 结果被丢弃的 settle」）改为**显式断言**（超时即失败；sentinel 现树 `:264`、超时守卫 `:276`、断言在 **`:274`**（批 3 值＝ `:261` / `:268` / `:271`，fix-round 后块内 +3））⇒ 该处**退出 `setTimeout(resolve,` 口径**（现文件内该形态命中数＝ **0**）。**账目**：`32 − 1 = 31`（② 10 未动；③ 遗留 2 未动；④ 20 → 19）；复算＝ `git grep -n "setTimeout(resolve," HEAD -- test/ \| wc -l` ＝ **32** vs `grep -rn … test/ \| wc -l` ＝ **31**。**为什么这处不属于本批反例**：原写法下 race 结果被丢弃 ⇒ 断言实际从未生效，是 ④ 里**唯一**「既无风险又无用」的一条；改后**新增**一个真实断言，是**加强**而非放宽（负控见 D15 段）。
  - **S1 的取证（2026-10-02，本批一手；临时副本已删）**：把轮询内的 `tick?.()` 停掉（`daemon-service`）⇒ 两条用例 **2 failed，报 `AssertionError: expected [ 'started' ] to deeply equal [ 'started', 'started' ]` / `… [ 'attempt' ] …`**（5061ms / 5053ms）——**加第三参之前**同一形状报的是外层 `Test timed out in 5000ms`；把 `SIGKILL` 停掉并跳过 `child.on("exit")` 等待（`daemon-process-manager`，控制里让屏障带锁跑到）⇒ **`Error: Timed out after 5000ms waiting for the killed holder's flock to be observable as free`**（5214ms）⇒ **谓词/屏障错误确实抢在测试级超时之前报出来**（这正是加第三参的目的）。
- **④ 其余无风险（20 处）**：
  - **轮询 helper（5）**：`test/unit/herdr-session-watch-manager.test.ts:1237`、`test/unit/daemon-process-manager.test.ts:62`、`test/unit/final-audit-regressions.test.ts:37`、`test/integration/herdr-session-watch-idle-seam.test.ts:282`、`test/integration/herdsman-pi-daemon-client.test.ts:375`（均为 `while (Date.now() < deadline)` + 断言/抛错；`daemon-process-manager` 那条还带 `Math.min(intervalMs, remaining)` 钳制）
  - **轮询式等待（1）**：`test/integration/rpc-test-client.ts:75`（`waitForNotification` 的 2ms 轮询；本批已把总预算从 ~200ms 抬到 ~2s，见 D15）
  - **夹具内部的流节奏（6）**：`test/unit/herdr-session-watch-manager.test.ts:246` / `:280` / `:322` / `:362`（假 `subscribeEvents` 异步生成器内部的 yield 节奏）、`test/unit/herdsman-pi-turn-signal.test.ts:14`（注入的 `sleep` 缝）、`test/integration/turn-completion-signal.test.ts:459`（假 `eventStream` 在 `pane.closed` 前插的 20ms）
  - **不需要窗口的等待 / 结果被丢弃的 settle（3）**：`test/unit/herdsman-pi-turn-signal.test.ts:42`（15ms 后 grow，断言靠 `pending` 结局；两侧都是真实定时器，相对顺序不随负载变化）、`test/integration/herdr-socket-client.test.ts:257`（`Promise.race([iterator.next(), 50ms])`，**race 结果未被使用**）、`test/integration/herdsman-pi-daemon-client.test.ts:160`（10ms settle 后 `client.close()`，**纯 settle**）（**订正 2026-10-01，`chore/ledger-hygiene-d12-d15`**：`herdr-socket-client` 那处已改为**显式断言**（不再属「结果被丢弃」）⇒ 该小类现为 **2** 处，见本段「第三轮」）。
  - **轮询循环 + 自带上界（1）**：`test/integration/turn-completion-signal.test.ts:485`（`while (!closeProcessed && Date.now() - start < 1000)` 轮询 + 断言，随后才做 100ms 沉降）
  - **4 个 helper 定义本身（4）**：`test/integration/orchestrator-disconnect-grace.test.ts:494`（`socketTick` 定义）、`test/integration/orchestrator-pane-move.test.ts:320`（同前）、`test/integration/observability-rpc.test.ts:1054`（`tick` 定义）、`test/unit/herdsman-pi-extension.test.ts:4417`（`tick` 定义）——定义行本身无断言，其调用点的归属见下一条。
- **★ helper 调用点分列（**不属于**上面 33 处命中，但属同族；本批一并交代，避免「重了定义、漏了调用点」）**：
  - ②（断言没发生）：`test/integration/orchestrator-disconnect-grace.test.ts:144` / `:149` / `:253`；`test/integration/orchestrator-pane-move.test.ts:139` / `:162`；`test/integration/observability-rpc.test.ts:422` / `:459`（**本批补漏项**）/ `:773` / `:775`（**本批补漏项**）；`test/unit/herdsman-pi-extension.test.ts:4701`（断言无 `agent.turn.completed`）。
  - ③（沉降错位，低强度，未实测）：`test/unit/herdsman-pi-extension.test.ts:1650`（沉降后断言 `client.calls` 已含该调用——同向）、`:1488`（沉降后清空通知基线，最坏表现为滞后落地）。
  - ④：`test/integration/observability-rpc.test.ts:765`（清空通知基线前的沉降；后面的 `waitForNotification` 会把同方法的残留消息消费掉，故不致假失败）。
- **为什么本批不动**：语义上必须「给窗口 + 断言窗口内无变化」；把 sleep 调大只是把墙钟加长，不改变「负载越重检出力越低」——那是**结构性**的，除非把负条件改写成可等待条件（多数没有这样的缝）。改动反而会削弱语义（旧写法在轻载下检出更强）。
- **核对**：① 枚举与分类可逐条复算——`grep -rn "setTimeout(resolve," test/` 应输出 **33 行**，与上面 ①–④ 清单一一对应（行号按**当前**工作树）；② 单点上下文用 `awk 'NR==<行号>' <文件>`；③ **口径边界**：本段只覆盖 `setTimeout(resolve,`（Promise 化 sleep）这一形态，`setTimeout(cb, n)` 的非 Promise 用法**不在 33 处之内**、也不在本口径内。（**追加 2026-10-01，`fix/ledger-test-timing`**：该 `wc -l` 已由本批变为 **32**——③ 的 `daemon-process-manager` 那处改用 `waitForCondition` 后**不再命中**；上面的 **33** 属**本批前**口径，账目与逐处改法见本段「本批」块。）（**再追加 2026-10-01，`chore/ledger-hygiene-d12-d15`**：**现树 `wc -l` = 31**（`git grep -n "setTimeout(resolve," HEAD -- test/ \| wc -l` = **32**＝本批 base，`grep -rn … test/ \| wc -l` = **31**＝本批后，本批一手实测）；**−1 的唯一位移＝ ④ 的 `test/integration/herdr-socket-client.test.ts:257`**（原「race 结果被丢弃」）——本批把它换成**显式断言**，其 `Promise.race([iterator.next(), 50ms])` 写法被 `await` 超时守卫取代 ⇒ **退出该 grep 口径**。**四类计数现为：「② 检出力下降 10（一字未动）」+「③ 遗留 2（现降为「轮询内让步」，见下）」+「④ 19」= 31**；**② / ③ 的语义与不动理由不变**，仅 ④ 减 1。

#### D14｜本批未修 / 暂留的点

- **① startup-grace 用例（`test/integration/orchestrator-disconnect-grace.test.ts:179-210`）没有套屏障**（**本批第二轮订正**：原写「该用例**没有活 socket**、`isTerminalConnected` 恒为 `false`」，**不成立**——第一阶段就有**活 socket**：`:188` `RpcTestClient.connect(first.socketPath)` + `:189` `await register(returning, "returning")`）。**订正后的理由**：该用例的三处 `advance`（`:186` `advance(99)`、`:190` `advance(1)`、`:205` `advance(100)`）**都不受 close 竞态影响**——startup timer 由 `start()` 在 `src/daemon/observability-server.ts:198` 的 `#armStartupGrace()` **同步 arm**，而注册走的是 **`await` 的 RPC 往返**（`:189`），所以 advance 只会跑在「timer 已 arm / register 已完成」之后；该用例里唯一的 close（`:192` `returning.close()`）后面跟的是 `await first.server.stop()`，**中间没有 advance**；`:205` 那次 advance 属**新建的 absent server**（其 socket 从未 close 过）⇒ **同一个屏障不需要**（D10 的机理不作用于它，它也不用 `socketTick()`）。若将来要给它加可等待条件，须**新增观测缝**（如暴露 startup timer 是否 armed）或改用 orchestrator 侧可观测状态；本批**原样留着**。
- **② D9 内部 20s 与实际等待上限的最终设计**：本批只把**外层**预算抬到 30s（≥ 单个内部 20s 等待）。该用例内部有**两个串行的 20s 上界**（`vi.waitFor({ timeout: 20_000 })` 与 `Promise.race([childExit, 20s])`），病理最坏叠加 ≈40s > 30s；是否把内部预算收敛成单一超时源（或按阶段分配）留待最终设计。
- **③ 本批新引入轮询的 `timeout` 取值（原写「未实测」，**本批第二轮已更新为高载档实测占用 2–4%**）**：落点为 `test/integration/orchestrator-disconnect-grace.test.ts:484`（`waitForTerminalDisconnect` 内的 `vi.waitFor`）与 `observability-rpc.test.ts:489-492`（`{ interval: 10, timeout: 5_000 },` 在 `:491`），取 `timeout: 5_000`——它是**上界**不是固定墙钟（正常路径毫秒级返回）。**高载档一手实测（来源＝oracle 本轮；本批未独立复现）**：形状＝**26 个 busy-loop 把 1 分钟 loadavg 从 19 拉≈28**（落 D9 原始档 21–34，node v22.23.1）+ **3 路并发全量**，同档各屏障点实耗 `:39`/`:119`/`:168`/`:300` = **144/113/101/125ms**、`observability-rpc` 的重试循环 **198ms** ⇒ 占用 5s 预算 **2–4%**（144/5000=2.9%、113=2.3%、101=2.0%、125=2.5%、198=4.0%）。⇒ 原「够宽但非实测」已更新为「**高载档占用 2–4%**」；仍**未测**的是**长尾峰值**（探针只记了这几处实耗，不是分布）。
- **④ D13 那一整组**（未修：调 sleep 只白加墙钟，不改结构性；清单与四类计数见 D13 段。**注意例外**：`test/unit/daemon-process-manager.test.ts:518` 是**反方向**——断言「锁已释放」这类**必然发生**的结果，负载下会**假失败**，不受「反向整组不会假失败」的限定保护）。
- **⑤ 屏障的「`#stopping` 前提」与「插入 `await` ⇒ 假失败报警」的耦合声明**（本批新增；派发件写作 D8/D14，实际落在 **D10 段 + `waitForTerminalDisconnect` 的 JSDoc `:468-476`**——D8 讲的是 `readChildPids` 告警分支的无覆盖，与屏障无耦合，故不往 D8 里塞）。内容：① 屏障**仅在 server 未 `stop()` 时有效**（`:621` 的 `!this.#stopping` 守卫 + `stop()` 在 `:209-213` 清空注册表）；② 蕴含依赖 `:618`（unregister）与 `:621`（arm）在**同一同步 turn**，将来若在中间插入 `await`，屏障的表现是「**要么 green、要么下游断言 `AssertionError` 假失败**」（定时器被孤立），**绝不会静默掩盖坏掉的 release**，届时须改屏障或加观测缝。详见 D10 段。

- **本批判定（2026-10-01，`fix/ledger-test-timing`；逐子项，回答「能否现在最小修」）**：
  - **① startup-grace 无屏障 → 保持不动（本批一手复核，D14① 订正后的理由成立）**：该用例（`test/integration/orchestrator-disconnect-grace.test.ts:179-210`）**根本不含固定 `sleep`**，三处 `advance` 全部落在**已 await 的工作之后**——`:186` `advance(99)` 在 `await startServer(...)` 之后、`:190` `advance(1)` 在 `await register(returning, "returning")`（RPC 往返）之后、`:205` `advance(100)` 属**从未 close 过 socket 的 absent server** ⇒ D10 的 close 竞态机理不作用于它，`waitForTerminalDisconnect` 的谓词在此处**没有可观测面**。要「加屏障」须**新增观测缝**（暴露 startup timer 是否 armed）或改用 orchestrator 侧可观测状态 ⇒ **不属最小修**（且本批边界禁止动 `src/**`）。
  - **② D9 内部 20s 与实际等待上限 → 已由 owner 拍板修复（2026-10-02）；此前 2026-10-01「保持不动 / 待 owner」的判定被 owner 决定取代**：内部等待是**串联两段**——`:577-586` 的 `vi.waitFor(..., { timeout: 20_000, interval: 100 })`（等子 daemon 的 pid 文件）与 `:606-609` 的 `Promise.race([childExit, 20s])`（等 `SIGTERM` 后的子进程退出）⇒ **病理性上界 40s**。**owner 2026-10-02 选择抬外层**：该用例第三参 `30_000` → **`45_000`**（落点现 **`:639`**、用例声明现 **`:544`**）＝ **消掉 (30s, 40s] 的假失败窗口，不改真失败面**——两段各自打满 20s 时该用例**仍然失败**（无假通过）；改掉的是「两段合计 ∈ (30s, 40s] 且各自 < 20s」这类**本可通过**的运行被外层截断成超时。**依据**：本机 3 路并发低载档实测该用例 **2560ms**（D16 表）⇒ 该窗口需约 **11×** 额外劣化才可达；owner 判断**值得消掉**。**本轮未动内部两段 20s**——「收敛成单一超时源 / 按阶段分配」仍是**最终设计**项。与 D16 判据的关系：本属其**第二条（内部上界 > 外层预算）的严格化**（按单次等待 30s ≥ 20s 已满足，按**串联上界 40s** 不满足），偏差自带本条独立理由。
  - **③ 本批新引入轮询的 `timeout` 取值 → 沿用已落地口径（不改 5s），并登记本批新增 3 处屏障的预算形态**：`observability-rpc.test.ts:491` 与 `orchestrator-disconnect-grace.test.ts` 的 `timeout: 5_000` 按本条正文（高载档占用 **2–4%**）**不改**。本批新增：`test/unit/daemon-service.test.ts:138-146`、`:201-209` 用**显式** `{ interval: 10, timeout: 5_000 }`（与 D10 落地的 `observability-rpc` 同形）；`test/unit/daemon-process-manager.test.ts:730-732` 用 `waitForCondition` 的**默认** `intervalMs` 20 / `timeoutMs` 5000（与同文件 4 处既有用法一致）。**正常路径都是毫秒级**（本批一手：两个 `daemon-service` 用例 **65ms / 55ms**（含屏障）、`daemon-process-manager` 该用例 **227ms**）⇒ 5s 是**上界**而非墙钟；**高载档占用率本批未测**（同 D14③ 证据边界）。**（订正 2026-10-01，`chore/ledger-hygiene-d12-d15`：高载档占用率已测）**：`daemon-service` 两条屏障用 **68ms / 65ms**（轮次 1）与 **76ms / 59ms**（轮次 2）⇒ 占 `5_000` 预算 **1.2–1.5%**；`daemon-process-manager` 那条所属用例 **748ms / 619ms** ⇒ **12–15%**（**用例整体**耗时，**不是**屏障实耗——实耗未插桩）。负载档（1 分钟 loadavg **24.43→26.65**，26 busy-loop / 12 核）与完整证据见 **D15** 段。**第二轮（2026-10-02，oracle S1）追加**：这三处屏障所属的用例**各加了测试级第三参 `}, 10_000);`**（`daemon-service` 两条 + `daemon-process-manager` 一条，落点见 D13 段「本批」块·第二轮），独立理由＝**轮询到期（5s）必须早于测试级超时，谓词错误才报得出来**；**轮询预算仍为 `5_000`**（D16 口径不变），**未改** `vitest.config.ts`。
  - **④ D13 组 → ③ 子集已修（本批），② / ④ 仍不动**：② 的理由（结构性、多数负条件无可用缝）与 ④ 的理由（无风险）均不变；③ 的三处改法见 D13 段「本批」块。**另注意**：D13 段「核对」条写的 `grep -rn "setTimeout(resolve," test/` **33 行**已因本批变为 **32**（口径与账目见本节表前追加条与 D13 段「本批」块）。

#### D15｜未实测项（证据边界；沿用前序 scout 的标注 + 本批补充）

- **D9 未亲自打穿 5s**：前序 scout 观测到的该用例最大耗时 **3184ms**（未过 5000ms）；「3 路并发 + 高载档 3/3 超时」的取证**引自 oracle 文档轮**，本批亦**未复现**（本批**低载档**形状 A 3/3 全绿；**高载档**形状 A 亦 3/3 全绿，来源＝oracle 本轮一手，见 D9 段）。
- **`test/unit/daemon-process-manager.test.ts` 的 4 处 spawn 用例未在高载档单项实测**（D7 抬超时的只是其中「200 轮压测」一条；本批形状 A 是**低载档**，占用率清单见 D16——该清单的「不动」结论**只在低载档成立**）。
- **`waitForNotification` 真实峰值未测**：本批只把预算从 ~200ms 抬到 ~2s（`test/integration/rpc-test-client.ts:71`），**没有**在负载下测出「实际需要多长」——抬预算是按「负载下检出力」而非实测峰值的保守选择。
- **`:39` / `:119` 两个屏障点未在饿死探针下失败**（见 D10 段末尾）：旧形状在这两点「也会假失败」**未被实测**，只能给代码层蕴含证明（高载档下这两点的实耗占用率见 D14③）。
- **CI 无失败样本（本批改为**决定**，不是遗漏）**：`.github/workflows/ci.yml` **结构上并行**（订正见 D7 段），但本台账与本批都**没有** CI 上的失败样本 ⇒「CI 会不会真被打穿」**未实测**。**本批的决定**：**不主动追** CI 失败样本——CI 的暴露面低于本地并行复跑（只单套、没有我们自叠的 3 路并发，runner 的核数 / 负载档也不由我们控制），追样本的成本不抵收益；若将来真出现，**按 D9/D10 的机理口径归因**（先看是 `Test timed out in 5000ms` 还是 owner 断言 / 屏障签名，再对号 D7/D9/D10/ D13），**不新建兜底机制**。
- **`pnpm check` 首跑末步的 `Bad substitution` 瞬时噪声（2026-10-01，`fix/child-process-active-t-state` 批；**未复现 / 未定位根因**）**：该批首跑 `pnpm check` 的末步 `pnpm herdr-plugin:check` 打印过 `/bin/sh: 1: Bad substitution (exited with code 2)`；**同一环境下单步 `pnpm herdr-plugin:check` 与整链 `pnpm check` 复跑均 `EXIT=0`、日志中无该字样** ⇒ 判为**瞬时环境噪声**，**未定位根因**、**未复现**。**它不属本批任何已修条目**（登记口径同本段的证据边界）。**半句补记（2026-10-01，oracle 提交前第二意见）**：**emitter＝agent harness 包装层**（`(exited with code N)` 后缀出自 harness 的 `package-manager-cli.js`）、**`/bin/sh`＝`dash`**、**检查链无 bash-ism**（`${…//…}` / `${…^^}` 类**全库无命中**）、**整链独立复跑 `EXIT=0`** ⇒ **环境 / 包装层噪声，非潜藏缺陷**，**便于后人免复查**。**本批一手补强（机制复现，2026-10-01）**：该文案在**本 harness 自身**即可逐字产生——本批一条收尾命令用 `${PIPESTATUS[0]}`（bash-ism）在 harness 的固定 `/bin/sh -c`（`dash`）下运行时，打印出**逐字相同**的 `/bin/sh: 1: Bad substitution`，并由 harness 包装层追加 `(exited with code 2)` ⇒ **发射器在仓库检查链之上**（本次观测的发射器是 harness 命令包装层；oracle 指出的包装层 `package-manager-cli.js` 属同一层）。但**原观测点那条命令未落盘**，故原判「**未复现 / 未定位根因**」**保持不变**（本条只把“环境/包装层”**从推测变为已演示的一类机制**）。

- **本批高载档一手测量（2026-10-01，`chore/ledger-hygiene-d12-d15`；`[MARK-LEDGER-HYGIENE-D12D15]`；本节数字均为**本批一手**，除另注来源）**——**背景负载口径（oracle consider，fix-round 登记）**：本机是**多 agent 会话**环境（常驻 `pi` / `herdr server`，别的 session 可能同时在跑负担）⇒ 下列负载档与耗时**可能被别的 session 叠加**，不是单一来源的定值档；引用时按 **D17(s)** 注明：
  - **负载档与配方（可复算）**：配方＝台账自己的高载配方 **26 个 busy-loop**，本机 **12 核**（`nproc`）；命令 `for i in $(seq 26); do ( while :; do :; done ) </dev/null >/dev/null 2>&1 & done`，**暖机 90s**。落盘证据（**已就地内联**；临时目录在本批收尾已清理，符合 D12(b)）——`loadavg-run2.txt` 原文：
    ```
    ambient (no loops): 17.23 10.03 14.58 3/997 208880
    loops started: 26
    loops alive: 26/26
    before run1: 24.43 14.33 15.69 27/1018 209275
    RUN1_EXIT=0
    mid: 26.16 15.20 15.96 27/1018 209437
    RUN2_EXIT=0
    after run2: 26.65 15.84 16.15 27/1028 210581
    CLEANUP alive=0 (want 0)
    ```
    ⇒ 1 分钟 loadavg **24.43 → 26.16 → 26.65**（落在 D9 原始档 **21–34** 中段）。另一次同配方轮次（同日 22:36）采样 30s/75s/90s ＝ **11.20 / 20.04 / 21.43**、`loops alive 26/26`、收尾 `alive=0`。**读档提醒**：上面的 `ambient` 行（17.23）是**上一轮 26 个 busy-loop 刚被杀**时的 1 分钟残值（同机真正空载时 1 分钟曾为 **0.68**；同一读数里 15 分钟 13.49–14.58 才是本机环境基线，来自常驻的 `pi` / `herdr server`）——**别把 17.23 当空载档**；本批的「低载档对照」用下表的定值轮（loadavg 峰值 5.4，引自 D16）。
    **收尾证据**：`ps -o pid= -p $(cat busy3.pids) | wc -l` ＝ **0**（cleanup 行 `alive=0 (want 0)`）；`ps -eo pid,pcpu,etime,args --sort=-pcpu | head -5` 里只剩 `pi` / `herdr server`，**无 busy-loop 残留**。所有高载运行 **`EXIT=0`**、全绿（`daemon-service` **18/18**、`daemon-process-manager` **41/41**，两轮各一次）。
  - **① 三处屏障用例的预算占用（高载档，两轮）**：`test/unit/daemon-service.test.ts` 两条（显式 `{ interval: 10, timeout: 5_000 }`）＝ **`skips ticks while a reconcile is in flight…` 68ms / 65ms**、**`start is idempotent and a rejected run does not break the cadence` 76ms / 59ms** ⇒ 占 `5_000` 预算 **1.2–1.5%**（低载档对照＝ **67ms / 58ms**）。`test/unit/daemon-process-manager.test.ts` 那条（`waitForCondition` 默认 `timeoutMs 5000`）所属用例 `flock held by child process…SIGKILL` ＝ **748ms / 619ms** ⇒ **12–15%**（**口径提醒**：这是**用例整体**耗时，**不是**屏障实耗；实耗未插桩，见下「未测」）。
  - **② D9 用例高载档（`a second daemon on the same HERDSMAN_HOME fails on the instance lock; SIGTERM releases it and allows restart`）**：高载 **1736ms**（轮次 1）/ **1284ms**（轮次 2）；低载档 **327ms**。**按现预算重述口径**：占其**实际第三参 `45_000`** 的 **2.9–3.9%**、占**默认 5s** 的 **26–35%** ⇒ **本批高载档未打穿 5s**（与台账旧口径「最大观测 3184ms、未过 5000ms」**同向、同量级**；旧值未改写）。**未测**：仍无「3 路并发 × 高载」样本（本批高载只跑**单进程**）。
  - **③ `waitForNotification` 峰值（插桩测量，单进程）**：临时给 `test/integration/rpc-test-client.ts` 的 `waitForNotification` 加一行探针（`appendFileSync` 记录每次调用耗时 + 方法名），跑 3 个调用方文件（`orchestrator-disconnect-grace` / `orchestrator-pane-move` / `observability-rpc`）：**低载档 18 次调用、峰值 3ms**；**高载档 18 次调用、峰值 6ms** ⇒ 占 **~2s 总预算的 0.3%**。**探针已还原**：`sha256sum test/integration/rpc-test-client.ts` ＝ **`ee8a68ca721175de5cfd51efe2b7c7c882d1b53a1e708e291de85e9b105048f1`**，与改前落盘值**逐字相同**；`git diff --stat -- test/integration/rpc-test-client.ts` **空**。**未测**：只覆盖这 3 个文件的 18 次调用（**不是**全仓分布），且是**单进程**档；峰值（而非分位数/长尾）。
  - **④ 四处 spawn 点在高载档的表现（`test/unit/daemon-process-manager.test.ts`，同一文件内四条）**：

    | 用例（标题） | 低载档（2 文件同跑，loadavg 峰值 5.4） | 高载轮次 1 | 高载轮次 2 | 高载占默认 5s | 该用例现有预算 |
    | --- | --- | --- | --- | --- | --- |
    | `two real concurrent processes competing for daemon lock: exactly one succeeds` | 154ms | 807ms | 748ms | **15–16%** | 默认 5s |
    | `flock held by child process is released immediately upon SIGKILL` | 181ms | 748ms | 619ms | **12–15%** | 默认 5s（+测试级 `10_000`） |
    | `a failed or truncated /proc/<pid>/stat read never counts as a dead lock-holding child` | 355ms | 480ms | 508ms | **9.6–10.2%** | 默认 5s |
    | `stress test: 200 rounds of simultaneous sub-millisecond lock contention yields zero double-masters` | 1283ms | **5418ms** | **5163ms** | **103–108%（>100%）** | `30_000`（D7，既有） |
    | （附）本批改过的 D1 两条：`an unreadable children file warns exactly once per process` | 10ms | 47ms | 52ms | ~1% | 默认 5s |
    | （附）`an unreadable children file leaves release watching the flock process alone` | 10ms | 39ms | 52ms | ~1% | 默认 5s |
  - **④的结论（本批关键新证据）**：**`stress test: 200 rounds` 在高载档单跑 = 5163ms / 5418ms，两次都突破默认 `5_000`** ⇒ **若没有 D7 的 `30_000` 第三参，这条用例在高载档下会稳定假失败**（此前 D7 只有 oracle 的「3 路并发 + 高载 6/6 超时」间接证据）；其余三处 **9.6–16%** ⇒ 低载档「<30% 不动」的判断在高载档**仍成立**（D16 分档口径：高载档 `>50%` 才补抬，**故本批不新增任何预算**）。
  - **⑤ `Bad substitution` 噪声：机制归因（本批一手，可重复；与本节既有条目**同结论、新增一次独立复现**）**：`readlink -f /bin/sh` ＝ **`/usr/bin/dash`**；把一个 bash-ism（`${PIPESTATUS[0]}`）经 **harness 自己的固定 `/bin/sh -c`** 执行时，原样输出：
    ```
    harness shell = /usr/bin/dash
    bash does not complain:
      bash PIPESTATUS= (rc=0)
    now the bash-ism under the harness's own /bin/sh -c:

    /bin/sh: 1: Bad substitution

    (exited with code 2)
    ```
    ⇒ **`/bin/sh: 1: Bad substitution` + `(exited with code 2)` 逐字复现**。反证两侧：同一脚本用 `bash -c` 跑**不报错**（`rc=0`）；写成**脚本文件**再跑时报的是 **`<脚本路径>: 2: Bad substitution`**（前缀是**脚本路径**、行号是 `2`）——即「**`/bin/sh` + 行号 1**」这个体裁本身就指认「经 `-c` 内联、第 1 行」的发射路径。⇒ t34 / oracle 的「**harness 包装层（固定 `/bin/sh -c`）+ dash**」归因**成立且可重放**；`(exited with code N)` 后缀由 harness 包装层追加。**边界（与原判一致，未放软）**：**原观测点那条 `pnpm herdr-plugin:check` 的命令未落盘**、仓库检查链内**仍无 bash-ism 命中** ⇒ 「那一次到底是哪条命令触发的」**仍未定位**，原判「**未复现 / 未定位根因**」**保持不变**；本条只把机制从「推测」升级为「**已演示、可重放**」。
  - **⑥ 本批未测 / 测不了（如实登记；每项给「为何 / 要什么条件 / 风险」）**：
    - **三处屏障的「实耗」未插桩**（只有**用例整体**耗时）：要测需在 `waitForCondition` 内部加探针（会在测量中改被测代码形状，**降证据等级**；且与「单次测量不扰动」冲突）⇒ 未做；风险：低——正常路径毫秒级已由用例整体耗时封顶（`≤76ms`）。
    - **「3 路并发 × 高载」未叠加**：本批的「3 路并发」（改文件 ×3，用于门禁）与「高载」（26 busy-loop **单进程**）是**分开**跑的两个形状；叠加需要 26×3 个 busy-loop 或分批同步启动，本批时间/机器负载不允许⇒ 未做；风险：中（判据的最狠形状仍未完整复现；历史 oracle 轮报过 3 路高载 3/3 全绿）。
    - **长尾 / 分布未测**：只有峰值与单次值，没有分位数（要就得多轮采样并插桩）⇒ 未做；风险：中（「平均够宽」不等于「长尾够宽」，但高载下 200 轮用例已**实测**突破默认线，说明长尾确实存在）。
    - **`waitForNotification` 只测了 3 个文件的 18 次调用**：全仓其它调用点（如 `herdsman-pi-daemon-client` / `herdr-session-watch-*`）未覆盖⇒ 未做；风险：低（同一实现、同一预算常量，且峰值仅 6ms）。
    - **CI 面、非 Linux / procfs 不暴露 children**：同上（D15 上文与 D17(h)①③），本批不变。
  - **本批未改动 D7/D9/D10/D13 的任何历史结论**：上列新值均以**追加**形式并存，附测量日期（2026-10-01）与负载档；旧值一律保留在原文。

#### D16｜抬预算判据 + 逐用例占用率清单（判据 vs 个例）

- **判据（本批定死；只管「用例自身耗时逼近默认预算」这一类，不覆盖性能探测力——那是 D11）**：
  - 在**最狠实测形状**（**3 路并发全量 + 高载档**，见 D9/D10）下，某用例占用**默认 5s 预算 >50%**，**或**用例**内部等待上界 > 外层预算**（如 D9 的内部 `vi.waitFor` 20s > 默认 5s）⇒ **只给该用例**抬预算（`.test.ts` 第三参），**不动** `vitest.config.ts` 的全局 `testTimeout`；
  - **<30% ⇒ 不动**；**30–50% ⇒ 观察**（不抬，记录在案）。
  - **分档执行口径（本批实测只有低载档，故判据必须按档读）**：**低载档 >50% ⇒ 必抬**（单调性下更狠形状只会更高）／**低载档 <30% ⇒ 不动但高载未证**／**30–50% ⇒ 观察**／**高载档只在有样本时复核、>50% 补抬**。
  - **高载档可复现配方**（引自 D9 段）：**26 个 busy-loop 把 1 分钟 loadavg 拉 ≥21**（D9 原始档 **21–34**）。
  - **为何不动全局**：全局抬会把「本地真失败」的反馈从 5s 拖到更久，并掩盖「病态变慢」信号（同 D9 段口径）。
- **占用率清单（除另注来源外均为本批一手）**：形状＝**3 路并发全量**（3 个进程各跑一次 `pnpm vitest run --reporter=verbose`），本机 1 分钟 loadavg 峰值 **5.4**（**低载档**）；占用率＝该用例耗时 ÷ 5000ms（取三轮最大值）。**下表「结论」列一律按低载档读**（高载档未测；配方见上方判据条）：

| 用例（落点） | 隔离单跑 | 3 路并发（低载档） | 占用默认 5s | 现有预算 | 结论（低载档） |
| --- | --- | --- | --- | --- | --- |
| `test/integration/agent-index-service.test.ts:4809`（`degraded done exhausting retries…`） | 4014ms | **4084ms** | **82%** | 本批新增 `30_000`（`:4889`） | **抬**（>50%） |
| `test/unit/herdsman-pi-extension.test.ts:4708`（`omits expectedText from RPC…`） | 3025ms | **3038ms** | **61%** | 本批新增 `30_000`（`:4758`） | **抬**（>50%） |
| `test/unit/daemon-process-manager.test.ts`（`two real concurrent processes competing…`） | 256ms | 758ms | 15% | 默认 5s → **fix-round 改为 `10_000`** | 低载档不动（<30%）；**fix-round 的 `10_000` 不是占用率驱动**，而是**判据第二条的派生**——该用例本轮新引入两处 `waitForCondition`（内部等待上界 5s）⇒ **轮询到期必须早于测试级超时**，否则谓词错误被 `Test timed out in 5000ms` 掩盖（负控两态见 D17(r)；与 S1 同一独立理由） |
| 同上（`flock held by child process is released immediately upon SIGKILL`） | 268ms | 976ms | 20% | 默认 5s（+测试级 `10_000`） | 低载档不动（<30%；高载未证）；其两处等待的**结构性假失败窗口**见 D17(r) |
| 同上（`a failed or truncated /proc/<pid>/stat read…`） | 354ms | 517ms | 10% | 默认 5s | 低载档不动（<30%；高载未证） |
| 同上（`:1011` `stress test: 200 rounds…`） | 1375ms | 6820ms | **136%** | `30_000`（既有，D7） | 已抬（本分支之前轮） |
| `test/unit/herdr-plugin-package.test.ts:72`（`packages the runtime entrypoint…`） | 175ms | 1261ms | 25% | `30_000`（既有） | 低载档不动（<30%；保留既有预算；高载未证） |
| `test/unit/package-publication.test.ts:78`（`includes LICENSE…`） | 185ms | 1400ms | 28% | `30_000`（**本分支前一轮**所加） | 低载档按判据**不动**（高载未证）；该处 30s 的独立理由是「与同文件姊妹用例对齐」，**不撤回也不追加** |
| `test/unit/daemon-service.test.ts:523`（`a second daemon on the same HERDSMAN_HOME…`，D9） | — | 2560ms | 51%（灰区上沿） | `30_000`（本分支前一轮，D9） | 已抬（D9：**内部 20s > 外层 5s** 的错配，属判据第二条） |
| `test/integration/event-dedup-pane-generation.test.ts:676`（`returns empty and warns when every event is noise`） | — | 9578ms | 192% | `120_000`（**HEAD 既有**） | 已抬（判据在**别处**已被执行过；本批未动） |
| `test/unit/agent-history-discovery-regressions.test.ts:149`（`stops discovery at maxFiles=2000…`） | — | 5929ms | 119% | `30_000`（**HEAD 既有**） | 已抬（同上；本批未动） |

- **判据 vs 个例的关系（本段存在的理由）**：上一轮的清单里出现过「42% 的修了、82% 的没修」的落差。本段把**判据**与**个例**分开写：判据是**默认线**（>50% 抬、<30% 不动、30–50% 观察），**偏离判据的个例必须自带独立理由**（本批仅 `test/unit/package-publication.test.ts` 一处属此类：前一轮按「与姊妹用例对齐」加 30s，本批实测 28% < 30% ⇒ **不构成新加理由，也不构成撤回理由**，保持原状）。
- **偏差登记（本批一手实测 vs 派发件给的数字）**：派发件给的占用率（`daemon-process-manager` 两处 41–43%、`herdr-plugin-package` 2245ms≈45%、`package-publication` 42–54%）与本批实测（15%/20%/10%、25%、28%）**不同量级**——本机本轮是**低载档**（loadavg 峰值 5.4），而判据要求的最狠形状是**高载档 21–34**；差异方向（高载档只会更高）不影响「>50% 抬 / <30% 不动」的结论（分档执行口径见上方判据条）。
- **核对**：`grep -rnE "\}, [0-9_]+\);" test/`（列出全部显式预算；本批新增 2 处：`test/integration/agent-index-service.test.ts:4889`、`test/unit/herdsman-pi-extension.test.ts:4758`；**fix-round 再 +1 处** ＝ `test/unit/daemon-process-manager.test.ts:775`（`two real concurrent…`，理由＝判据第二条，见上表尾行）⇒ 现共 **41** 处命中）；- **高载档一手补测（2026-10-01，`chore/ledger-hygiene-d12-d15`；判据要求的最狠形状只补上了「高载」这一维，**3 路并发 × 高载仍未叠加**）**：形状＝**26 个 busy-loop / 12 核 / 暖机 90s**（1 分钟 loadavg **24.43 → 26.16 → 26.65**，落 D9 原始档 21–34）、**单进程**跑目标文件。结果：`test/unit/daemon-process-manager.test.ts` 四处 spawn 点 **748 / 619 / 508 / 5163ms**（轮次 2）与 **807 / 748 / 480 / 5418ms**（轮次 1）⇒ **`stress test: 200 rounds` 在高载档突破默认 5s（**103–108%**）**，直接坐实 D7 给它加的 `30_000` 第三参（此前只有 oracle 的「3 路并发 + 高载 6/6 超时」间接证据）；其余三处 **12–16%** ⇒ 低载档「<30% 不动」的结论在高载档**仍成立**（按分档执行口径「高载档只在有样本时复核、>50% 补抬」）。`test/unit/daemon-service.test.ts`：D9 用例高载 **1736ms / 1284ms**（占其 `45_000` 的 **2.9–3.9%**、占默认 5s 的 26–35%）、两处屏障 **68 / 65ms** 与 **76 / 59ms**（<2%）⇒ **无补抬**。**仍缺的形状**：3 路并发 × 高载（见 D15 段未测清单）；**长尾分布**未测。
- 占用率复跑形状＝**D7「核对」段** + `--reporter=verbose`（看逐用例行尾 `NNNms`）＋跑前后 `uptime` 记负载档。

#### D17｜遗留清单（2026-10-02，`fix/ledger-test-timing` 第二轮；来源＝reviewer / oracle，**本批只登记不动手**）

- **（a）reviewer｜参数形态不齐**：`test/unit/daemon-process-manager.test.ts:730-732` 用 `waitForCondition` 的**默认** `intervalMs` 20 / `timeoutMs` 5000，而 `test/unit/daemon-service.test.ts:138-145` / `:204-211` 用**显式** `{ interval: 10, timeout: 5_000 }` ⇒ **后续统一重构时对齐**（**不阻断**：两条链路语义等价、当前逻辑正确）。**不动手**的理由：改 `waitForCondition` 默认值会牵动同文件**既有 4 处**调用点（`:594` / `:1109` / `:1168` / `:1242`），超出本批「只改三处屏障」的范围。（**fix-round 后这两个数字改正**：该文件 `waitForCondition` 调用点**现共 12 处**＝ `:657` / `:758` / `:761` / `:810` / `:817` / `:881` / `:897` / `:1196` / `:1255` / `:1329` / `:1377` / `:1456`；旧文的「4 处」是**部分列举**。）
- **（b）D13 账目补算式（可复算）**：`grep -rn "setTimeout(resolve," test/ | wc -l` ＝ **33 − 3 + 2 = 32** —— **删 3**＝旧的三处固定窗口（`test/unit/daemon-process-manager.test.ts:728`、`test/unit/daemon-service.test.ts:135`、`:190`）；**增 2**＝两处轮询内的**宏任务让步**（现树 `test/unit/daemon-service.test.ts:141`、**`:207`**；派发件给的 `:204` 是**加测试级第三参之前**的行号）。**并注明**：`daemon-service.test.ts` 仍命中 **3 处**＝1 处**旧有** `:130`（② 类）+ 2 处**新增让步**（`:141` / `:207`）⇒ 该文件**净 0**。
- **（c）锚点漂移（归 D12｜台账卫生，本批只登记）**：① 台账 D13 ④ 引用的 `test/unit/daemon-process-manager.test.ts:62` 的**分段轨迹**（**订正于 2026-10-01 同日第二轮 fix-round，本批一手；oracle 实测同向**）：`:62 @ 5433525`–`b6c3b87` → **`:63 @ 2555b2b`** → **`:64 @ 5b7181b`/`105286f`** → **`:127 @ 0a1d6b7`/本批**。**原写「在现树实为 `:64`」在 `2555b2b` 那一格错 1 行**（`2555b2b` 树的 `:62` 是 `}`、`:63` 才是 `setTimeout(resolve, …)`；`throw` 行始终在 sleep 行 -2）——核对手法＝ `git show <rev>:<file> | sed -n '60,66p'`；同一 `rev` 的 `async function waitForCondition(` 定义行＝ 49 / 50 / 51 / 114（与 D12.4 轨迹表一致）；② 台账 D13 ③ 引用的 `:518`，其 **pre-batch 实测为 `:728`**（本批一手核对），且属**跨提交累积**漂移（`:518→:519→:728`，中间步引自派发件、**本批未独立复核**）——**建议下次写成分段轨迹**（每段的提交/批次 + 位移量），便于后人复算。**本批未改写这些旧锚点。** ⇒ **2026-10-01 由 `chore/ledger-hygiene-d12-d15` 收口：完整「旧→现」映射见 D12.4**（本档内唯一权威表）。
- **（d）口径外同族（新挂账，待定）**：`test/integration/herdr-socket-client.test.ts:257` 的 `await Promise.race([iterator.next(), 50ms])`——**race 结果被丢弃**（D13 ④ 已按「无风险」登记），但它同时意味着**订阅竞态若真发生也不会报警**：**不会假失败、但会漏检**。**挂账待定**（要钉需把负条件改成「断言订阅确实生效」的可等待条件＝改测试语义，非机械替换）。
- **（e）oracle 附注｜② 类的措辞同样偏强（**仅附注，不删原判断**）**：`test/unit/daemon-service.test.ts:130` 这类「断言**未重复启动**」的守卫判定是**同步**进行的——被跳过的 tick（`PeriodicReconcileScheduler` 里 `if (this.#inFlight) return;`）**根本不会在微任务里跑起来** ⇒ 这类检出力**不随负载下降**（**本批一手复核该同步性成立**；② 类其余条目**未逐条复核**）。因此 ② 类「**负载下**检出力下降」的措辞**同样偏强**，建议在 **D12 / 后续统一收敛**时一并对齐（本批**未改** ② 类条目正文）。
- **（f）本批一手发现｜`daemon-process-manager` 那条用例在屏障之前还有一个无界等待（归后续）**：`test/unit/daemon-process-manager.test.ts:725` 的 `await new Promise((resolve) => child.on("exit", resolve));` **没有任何超时**——`SIGKILL` 失效（或杀错对象）时用例会卡在这里，**测试级预算就是该路径唯一的界**。因此 oracle S1 加的 `}, 10_000);` 在这条路径上只是把失败报告从 5s 推到 **10s**（本批负控实测：先看到 `Test timed out in 10000ms`，根本没走到屏障）；走到屏障的那半路径已由 S1 正确覆盖（屏障 `5_000` 先到期，报自己的错误）。**建议后续给该未界加轮询/超时**（属改测试结构，本批不做）。
- **（g）本批（2026-10-01，`fix/d8-child-pids-warn-coverage`，D8 收口 + fix-round；`[MARK-D8-COVERAGE]` / `[MARK-D8-FIXROUND]`）自身造成的锚点位移（供后续批次；本批**不回写**任何旧行，D12 卫生批量处理）**：`test/unit/daemon-process-manager.test.ts` 本批**净 +185 行**（文件头 mock 缝 **+63**、末尾两条新用例 **+122**；含 fix-round 的注释 +1 行与承重自检 +5 行）⇒ 旧树 `:32-1247` 的锚点整体**后移 63 行**，旧树 `:1248` 起（压测用例及其后）整体**后移 185 行**。逐条映射（旧 → 新，fix-round 后一手 `grep`/`sed` 重测）：D17(c) 的 `:64` → **`:127`**；D17(f) 的 `:725` → **`:788`**；D17(a) / D13③ 的 `:730-732` → **`:793-795`**；D6 行引的两条 T 态用例区间 **订正见（g-1）**；D7 / D11 / D16 引的 `:1011`（兼称「压测用例声明」）→ 旧树 `:1011` 实为 `fakeFlock,`、压测用例声明在旧树 `:1248` ⇒ **新树声明行＝`:1433`**（`:1011` 属**旧树即已漂移**，本批未回写；数字同属 D12）。⇒ **本批（`chore/ledger-hygiene-d12-d15`）已把「旧→现」收成 D12.4 一处**，并按内容核实订正后值：两条 T 态用例现树 **`:1199`（post-READY）/ `:1244`（pre-READY）**（＝本条 `:1192`/`:1237` 再加本批 SIGKILL 用例的 **+7**）。
  - **（g-1）D6 行区间订正（按内容核实，本批一手；原写数字错，非本批造成）**：D6 行原引 `:913-956`（post-READY）/ `:958-1030`（pre-READY）**指向的是无关代码**——旧树 `:913` 实为 `const holderScript = join(binDir, "holder.sh");`、`:958` 落在一段 `holderScript` heredoc（`sleep 0.005` / `exec cat`）内（本批 `sed` 逐行核对）；本批写进 (g) 的「位移后值」`:975-1018`/`:1020-1092` 同样指向那段无关代码（同理作废）。**按用例标题定位**，两条 T 态用例的真实位置：旧树 **`:1129`**（`a simulated T-state (SIGSTOP-paused seam) …`）/ **`:1174`**（`a pre-READY paused (T) helper is abandoned…`）⇒ 本批改动后 **`:1192`** / **`:1237`**。oracle 给的 `:1191`/`:1236` 与实测**差 1 行**（本批 fix-round 在文件头注释净 +1 行所致），**以本批实测为准**。⇒ **本批已将两者收进 D12.4**（现树 `:1199` / `:1244`）。
- **（h）本批（2026-10-01，`fix/d8-child-pids-warn-coverage`）D8 收口后新增的证据边界（未做，待定）**：① **`existsSync("/proc")` 为假 ⇒ 不打告警**这条分支**仍无用例**（非 Linux / 无 procfs 场景）——本批只拦 `readFileSync` 的 children 路径，要钉得同时 mock `existsSync`（新挂账；**不动手**的理由：会扩大 mock 面到同文件其它用例共用的事件路径，超出本批“只让读 children 失败”的边界）。② 「每进程一次」是**模块作用域**状态，用例只钉到 **module instance 级**（靠 `vi.resetModules()` + 动态 import 取副本），**跨模块副本 / 跨进程的语义未实测**（代码上是模块作用域，属构造性事实，非实测）。③ 本机 procfs **有** children 文件（D1 已注明本机不可达）：失败是**注入**的，真实内核（`CONFIG_PROC_CHILDREN` 关 / 精简 procfs）**未实测**。④ **降级路径的真实墙钟残余未断言**：用例②只能看「release 等待集」的**成员**，看不了「flock 进程仍活时 release 是否提前返回」的实际时长。⑤ **平台口径（fix-round 订正）**：同文件邻例**有**守卫——`hasProcChildren = existsSync('/proc/<pid>/task/<pid>/children')`（fix-round 后 `:902`）＋ `test.skipIf(!hasProcChildren)`（`:904`/`:970`）；**用例② 本批已加同形守卫**（`:1358`）；**用例① 保持裸 `test(`**（理由：它注入读失败，在任何 Linux——含 procfs 不暴露 children 文件的内核——都成立，且在那样的内核上反而会**真触发**并通过；加守卫会漏掉该路径，与「把不可达分支钉住」的目标相反）。原写「与同文件邻例相同口径，未加平台守卫」**作废**（事实错误）。
- **（i）本批（2026-10-01，`fix/d8-child-pids-warn-coverage`）一手核到的**旧锚点漂移**（源文件侧，归 D12；本批**不回写** D1 / D8 以外的行）**：D1 行（与 D8 原行）引 `src/daemon/process-manager.ts:312-333`（读 `:312`、告警 `:326`），现树实为 **`:401`（`readChildPids`）/ `:413-417`（告警，`console.warn`＝`:415`）**；`warnedChildPidsUnavailable`＝`:388`、`helperPids`＝`:588`、release 等待环＝`:705`（均为本批一手 `grep`）。D1 **状态未变**（仍为接受），只是锚点待下一批收口。⇒ **本批已收口**：现树锚点见 **D12.4** 的 `src/daemon/process-manager.ts` 表（含读 `:401` / catch **`:407`**（`：408` 属 off-by-one）/ 告警 `:413-417` / `warnedChildPidsUnavailable` `:388` / `helperPids` `:588` / release 等待环 `:705`）；**D1 状态与结论未变**。
- **（j）双审 consider｜「裸 catch + 一次性 latch」把任何**瞬时**失败都当成永久退化（本批登记，不动手）**：`src/daemon/process-manager.ts:408`（裸 `catch`）＋ `:413`（`!warnedChildPidsUnavailable` 一次性 latch）⇒ `EMFILE` / `EIO` / 「读时进程被杀」这类**瞬时**错误也会**永久消费唯一的告警预算**，此后再遇**真**退化（procfs 不再暴露 children 文件）就**永远静默降级**。属**已知设计**（模块级 flag 的既定语义，用例① 已把它钉成契约：「恰好一次」＋「第二次不再刷」），缺的是**登记**——(h) 边界清单原先没有这一条。**未做**：区分 errno（`ENOENT`＝结构性退化→latch；其余＝瞬时→不 latch 或按次数节流），属产品语义变更，待 owner / 后续批次。
- **（k）双审 consider｜头注释措辞「exact … path」→ 实为「同形状路径」（本批已收敛措辞，未收紧正则）**：mock 的判据是 `/^\/proc\/(\d+)\/task\/(\d+)\/children$/`——`/proc/1/task/2/children` 这种**两个 pid 不相等**的路径也会被拦；生产恒相等（`readChildPids(pid)` 两侧同 pid），**无实害**。**本批 fix-round 已把注释改为「children-file path *shape*（两个 pid 不必相等）」**；**未**给正则加 `\1` 反向引用（不扩大改动面、也不缩小覆盖面）。
- **（l）双审 consider｜注入面边界（失效方向是「响」的）**：该缝只拦**具名** `readFileSync` 导入路径。`import fs from "node:fs"`（**default** 导入）与 **`node:fs/promises`** 会**绕过**该缝——今天 `src/daemon/process-manager.ts` 只用具名 `readFileSync`（`:11` `} from "node:fs";`，文件内**无** `node:fs/promises`），故缝有效；将来若改成 default / promises，**用例会失败**（不是假绿）。**未做**：不为 default / promises 补拦（会扩大 mock 面）。
- **（m）双审 consider｜跨进程语义未覆盖（设计使然）**：latch 是**模块作用域** ⇒ **每个进程各 warn 一次**（CLI 一次、daemon 一次）是**设计**；用例只钉 module instance 级（见 (h)②），**两个进程各自的告警预算未实测**。
- **（n）oracle consider｜用例② 断言失败会**跳过 `release()`**（本批不动手；建议后续 3–5 行收口）**：句柄释放在 `try` 体内而非 `finally`——用例末尾 `finally`（D8 fix-round 后 `:1426-1429`；**同日第二轮再测：现树 `:1459-1462`**，两个 `release()` 落 `:1460` / `:1461`）只复位 `fsInjection.failChildrenRead` 与 spy（`:1427-1428`）⇒ 断言先炸时，未被 release 的 helper（flock + 其 fd 共享子进程）会把临时锁**把持到本进程退出**；同文件真 spawn 用例的**口径**（**订正 2026-10-01 同日第二轮 fix-round，本批一手；原写「都」过泛**）：该文件共 **6 个真 spawn 用例**，改动前**只有 3 个**在 `finally` 里杀子进程（`flock handle release does not busy wait…` 现树 `:901-910`、`flock handle release waits for every helper process…` `:983-990`、`release() returns only once the lock is observably free` `:1075-1082`）；另 3 个是 **inline 杀**（`two real concurrent processes…`＝ `killGroup` `:753-754`、`stress test: 200 rounds`＝ `:1559-1560`、`flock held … upon SIGKILL`＝ `:803`）。⇒ 本条的自证理由**不依赖邻例口径**，就是「**断言失败不该把锁留在手里**」；**本轮 fix-round 已给前两个 inline 用例补 `finally`**（`two real concurrent…` `:764-771`、`stress test` `:1566-1573`），**只剩 `flock held …SIGKILL` 仍是 inline**（已挂新 (t)①）。**未做**：把 `degraded?.release()` / `healthy?.release()` 挪进 `finally`（或改用「已创建句柄」数组统一释放）。
- **（o）reviewer consider｜`flockPidOfLastChildrenRead`（fix-round 后 `:87`）只有用例② 一个调用点**，后续梳理可降为用例作用域。**本批不做**（与用例① 共用同一套注入态的取 pid 语义，先保持文件级可见性）。

- **（本批状态续记：2026-10-01，`chore/ledger-hygiene-d12-d15`；`[MARK-LEDGER-HYGIENE-D12D15]`；**只追加状态与理由，不覆写原判断**）**：
  - **（a）→ 维持挂账（本批不顺手对齐）**：本批新增的 `waitForCondition` 调用点（`:793`，见 (f) 收口）**沿用同文件既有写法＝默认 `intervalMs` 20 / `timeoutMs` 5000**，**未**改成 `daemon-service` 的显式形式；理由：① 改默认值会牵动该文件**既有 9 处调用点**（现树 `:657` / `:800` / `:864` / `:880` / `:1179` / `:1238` / `:1312` / `:1360` / `:1438`）；② 同文件内**先做到形状一致**优于跨文件对齐，跨文件对齐留待统一重构。
  - **（b）→ 已记，追加第二步账目**：`33 − 3 + 2 = 32`（上批）⇒ 本批 **`32 − 1 = 31`**（`test/integration/herdr-socket-client.test.ts:257` 改写后退出该 grep 口径，见 (d) 与 D13 段「第三轮」）。复算：`git grep -n "setTimeout(resolve," HEAD -- test/ | wc -l` ＝ **32** vs `grep -rn "setTimeout(resolve," test/ | wc -l` ＝ **31**。
  - **（d）→ 本批已收口（弱版）**：`test/integration/herdr-socket-client.test.ts:257`（现树木：sentinel `:261`、超时守卫 `:268`、断言 **`:271`**）把「race 结果被丢弃」改为**显式断言**。**负控**：让假服务器在 ack 后**立刻推一条 `pane.created`**（＝窗口内确有事件）⇒ **HEAD 版（race 丢弃）仍 `1 passed`（漏检）**、**worktree 版 `1 failed`**（`AssertionError: expected 'event' to be Symbol(no event within 50ms)`）。**`10009ms` 的归因（oracle 本轮指出；同日第二轮 fix-round 一手复现 + 修复）**：**不是**「临时副本自身开销」，而是——**失败路径跳过了 `client.close()` ⇒ `afterEach` 的 `server.close()` 等不到连接关闭、挂到 vitest 的 `hookTimeout`（`10000ms`）**，于是在真断言之外**追加第二条报错**（`Error: Hook timed out in 10000ms.`）。**fix-round 已把 teardown 挪进 `finally`**（现树 `:279-285` 的 `controller.abort(); client.close();`）⇒ 失败时不再产生第二条报错、也不再白等 10s。**负控（本轮一手，原始输出）**：注入「在 sentinel 断言处必失败（**不动流**）」——**修前**（批 3 版本）：用例 **10064ms**、`[1/2] Error: NC-INJECT…` + `[2/2] Error: Hook timed out in 10000ms.`（指 `test/integration/herdr-socket-client.test.ts:12`）；**修后**：用例 **62ms**、**只有 `[1/1]`**。**残留（＝强版仍挂账，见新增 (p)）**：改后的断言是**窗口内断言「没到」**＝ D13 **② 类**（负载下检出力下降、**不假失败**）。
  - **（e）→ 本批未改 ② 类正文措辞（只加口径注）**：同意「措辞偏强」，但本批边界是「只动台账文字」，**改写 ② 类整组措辞属批量语义改写**、超出本次卫生轮；本轮只在 D13 段头加「口径现状」一行（33 → 现树 31，逐处清单**不重排**）。⇒ **仍挂账**。
  - **（f）→ 本批已收口**：无界等待（旧树 `:725` → 修前现树 `:788`）改为：

    ```ts
    await waitForCondition(() => child.exitCode !== null || child.signalCode !== null, {
      description: "the SIGKILLed flock holder to exit",
    });
    ```

    （现树 `:793-795`，其后是 flock 释放屏障 `:800-802`。）**负控**：把 SIGKILL 指向**不存在的进程组**（`process.kill(-999999, "SIGKILL")` ⇒ 子进程**永不退出**）⇒ **HEAD 版**被**测试级预算**截断（`× … 10018ms`，**无具名错误**）；**worktree 版**在 **5149ms** 报**具名错误**：

    ```
    × test/unit/tmp-nc2-fixed.test.ts > daemon process manager > flock held by child process is released immediately upon SIGKILL 5149ms
      → Timed out after 5000ms waiting for the SIGKILLed flock holder to exit
    Error: Timed out after 5000ms waiting for the SIGKILLed flock holder to exit
    ```

    ⇒ 该路径不再只靠测试级预算兜底（从「裸 `Test timed out in 10000ms`」变成「5s 到期报屏障自己的具名错误」）；同时**缩短了失败反馈**——原状下先看到的是测试级超时（本批实测 10018ms），现为 **5s**。
  - **（n）→ 本批已收口**：D1 降级用例：两个 handle 提到 `try` 外，`finally` 先 `degraded?.release(); healthy?.release();`（`release()` 幂等，双释放是 no-op）。**断言一字未改**（`git diff -U0 -- test/` 里本用例只增 `finally` 四行与变量声明）。**负控**：把 `expect(healthy).not.toBeNull()` 改成 `expect(healthy).toBeNull()` 强制在**两个 handle 都已获取之后**失败，两侧报**同一条** `AssertionError: expected { release: [Function release] } to be null`；差异只在残留（探针在用例自身 `finally` 末尾扫 `/proc`，列出仍打开锁文件的进程）：

    ```
    # HEAD 版（未修）
    nc1 isFlockHeld=true holders=["124398/fd/3","124399/fd/3"]
    # worktree 版（已修）
    nc1 isFlockHeld=false holders=[]
    ```

    ⇒ 修前：断言失败时两个 helper（flock + fd 共享子进程）把临时锁**持到本进程退出**；修后：**当场释放、无残留持锁进程**。（NC1a＝第一处获取后即失败、NC1b＝健康对照获取后即失败，两变体结果同向。）
  - **（j）/（k）/（l）/（m）/（o）→ 本批未动，维持挂账**：都属**产品语义 / 测试结构变更**（errno 分流、正则收紧、补 default / promises 注入面、跨进程语义实测、helper 作用域），与本批「台账卫生 + 三处小修」无关；本批只借 **D12.4** 收拢它们的**旧锚点**（如 (o) 的 `:87`、(k) 的注释行、(h)⑤ 的 `:902`/`:904`/`:970`）。
  - **（p）（本批新增｜由 (d) 收口派生）**：`test/integration/herdr-socket-client.test.ts` 那条用例现在是**② 类负断言**（50ms 窗口内断言「没有事件到达」）⇒ ① 负载下**检出力下降**（**不假失败**，故**不属** flake 源）；② 它**仍**继承了 (d) 的未尽事项：「订阅竞态若真发生、但比窗口更晚」**仍不会**报警。**要钉死需**把负条件改成可等待的**正**条件（如「先等第一路 iterator 确实拿到订阅生效的证据，再断言第二路被拒」——需客户端暴露「已订阅」的可观测面）⇒ **挂账**（属测试语义 + 可能需 src 侧观测缝）。
  - **（q）（本批新增｜方法学）**：本批发现「**只在 stdout 打印的一次性证据会被会话压缩吃掉**」（高载轮的首轮 `loadavg` 就只落在 stdout，轮次结束后已不可复算）⇒ **后续高载测量把 `loadavg` / 退出码 / 逐用例行写进落盘文件**（本批已按此重测一轮：`loadavg-run2.txt` + 两轮 `hl2-*.log`，且在**写台账时就地内联**、临时目录收尾清理）。与 **D12** 同源：证据要么落到**可复算**处，要么**当场标为历史值**。
- **（r）（本轮新增｜oracle｜结构性假失败窗口：`flock held …SIGKILL` 的两处等待）**：现树 `:810-812`（有界轮询）与 `:817-819`（flock 释放屏障）两处，在**进程被整体去调度 ≥5s** 时会以**超时**收场。**机制（本批复核，与 oracle 一致）**：`waitForCondition` 是「**定时器相位先于谓词相位**」——每轮先 `setTimeout(20ms)` 让步、再查谓词，`deadline` 是**墙钟**；整体去调度期间两者都不推进，恢复后首轮即 `remaining <= 0` ⇒ 报自己的「Timed out after 5000ms」（**不是**被别的东西打穿，是**自己的界**到期）。**取证（oracle 本轮一手）**：用 `SIGSTOP` **确定性**做出「整体去调度」形状，**3 次 1 次命中**超时；**真实负载下从未出现**——高载档该屏障实耗 **max 26ms** ≈ `5_000` 预算的 **0.52%**（loadavg ≤ 34）。**结论（不修）**：「≥5s 完全去调度」在本机**任何可控负载下不可达**（26 busy-loop / 12 核档实耗仍在毫秒级）⇒ 与 D13 ② 同属**结构性**假设，登记不修；代价面＝偶发假失败、**无假通过**。
- **（s）（本轮新增｜oracle consider｜高载测量的背景负载口径）**：本机是**多 agent 会话**环境（常驻 `pi` / `herdr server`，别的 session 可能同时在跑高载配方）⇒ 引用 **D15 段**任何 loadavg / 耗时数字时**必须注明「背景负载可能被别的 session 叠加」**，不得当单一来源的定值档；D15 段那个 `ambient` 行（17.23）就是这种叠加的实例（该段已给读档提醒）。
- **（t）（本轮新增｜fix-round 一手新发现的两处残留；挂账不修）**：① `flock held …SIGKILL`（现树 `:777`）的**杀子进程仍在 inline**（`:803`；与 (n) 同族）——它在**屏障之前的那次断言 `expect(() => acquireDaemonLock(lockPath)).toThrow(…)`（现树 `:799`）失败时**，会把持锁子进程留到本进程退出；修法＝同 (n) 的 `try/finally`（本轮**未做**：不在派单范围，且该用例的 kill 处于「被测行为叙述」的位置，包 `finally` 要顺带改注释口径）。② 同文件仍有**两处无界等待**：`two real concurrent processes…` 的 `readLine`（`:737-740`，`p.stdout.once("data", …)`）与 `stress test` 工作进程的 `nextLine`（`waiters.push(resolve)`，无超时）——worker 若不打印任何一行，用例会挂到**测试级预算**（`10_000` / `30_000`）为止。**两者都属「改测试结构」，不在本轮派单内。**
  - **（fix-round 行号换算（同日第二轮，`[MARK-D12D15-FIXROUND]`）**：本块（状态续记）内标「现树 / 批 3」的 dpm 行号按 **D12.4 末「新位移（第二轮）」** 的规则换算——`:739` 起 **+17**、`:1396` 起 **+18**、`:1506` 起 **+27**；`herdr-socket-client` 块内**断言行之前 +3**、文件尾 +8（例：续记 (d) 的 sentinel `:261` → **`:264`**、守卫 `:268` → **`:276`**、断言 `:271` → **`:274`**；续记 (f) 的轮询 `:793-795` → **`:810-812`**、屏障 `:800-802` → **`:817-819`**；续记 (n) 的 `finally` `:1426-1429`（D8 fix-round 值）→ 批 3 `:1441-1446` → fix-round **`:1459-1462`**（两个 `release()` 在 `:1460` / `:1461`））**。
- **本批对 D17 的总结**：**新收口 3 条**＝(d) 弱版、(f)、(n)（**fix-round 均已加强**：(d) 补 teardown + `10009ms` 归因，(n) 订正过泛措辞并补 2 处 `finally`）；**状态更新 3 条**＝(a) 维持（调用点 9 → **12**）、(b) 第二步账目、(e) 维持；**未动**＝(h)(i)(j)(k)(l)(m)(o)；**新增**＝(p)(q)（第一轮）+ **(r)(s)(t)**（fix-round）。只有 (d)/(f)/(n) 因本批**被派单为小修**而收口，其余不变。

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
- **一次抬掉 `vitest.config.ts` 的全局 `testTimeout`**（把并行复跑的假失败一次清完）：**放弃**——会把「本地真失败」的反馈从 5s 拖长，
  并掩盖「病态变慢」信号；**判据要求按用例单独抬**（D16），全局值保持默认。
- **把 D13 的反向用法整组一并改写成可等待条件**：**放弃**——多数负条件没有可等待的缝，把 sleep 调大只是白加墙钟；
  语义上「给窗口 + 断言窗口内无变化」必须靠墙钟，改动反而**削弱**轻载下的检出强度（D13 段）。
- **照搬派发件给的占用率数字写进 D16**：**放弃**——派发件的 41–54% 与本批实测的 10–28% 不同量级（负载档不同）；
  按 D12（引用数字须可复算）口径，以**本批一手 + 形状描述**为准，并另立「偏差登记」写明差异（D16 段）。
- **为演示 A3 的「屏障超时」而改动 `src/**` 或留下临时测试副本**：**放弃**——屏障签名用**临时副本**演示（已删），
  `src/**` 零改动；这也正好证明了「屏障被绕过时**回到**原 owner 断言」的第二条签名（D10 段）。

## 来源

- 双审条目：**R1（H1 首轮，oracle 在该轮给出 F1「应修」裁定）→ F1 修复 → R2（增量复核）**；
  **R5**（Phase 1 两关；该轮 oracle 复核同时给出 A1 的「2 处同栈可取 reason」修正与 A5 的 3–7% / 50ms 数字）。
  **本批执行者手上只有摘要**（逐条已标注「摘要，原文不在手」），原文以双审派发件为准。
- **锁修复批次（`fix/daemon-lock-liveness-errno`，未提交）**：轮次＝ **R5（A5 登记）→ worker 实现轮（errno 分流＋注入缝＋1 条回归用例）
  → oracle 窄复审（4 条应修＋同批建议：删 A1 计数、改注入缝 JSDoc、A5 锚点位移、A1 off-by-one、新增 A8、A7 理由改写、加强回归用例）
  → 收口轮（即本次改动）**。本批执行者手上**没有** oracle 原文，各条按派发摘要转述；**A8④ 的实测数字（合计约 95 万次读 / 0 次空读）
  引自该轮报告，本批未独立复现**。
- **并行复跑假失败批（`fix/parallel-flake-timing`，未提交）**：轮次＝ **oracle 文档轮（D9/D10 取证）→ owner 2026-10-01 立项 → worker 实现/取证轮（即本批）**。本批只动 `test/**` 与本台账，**未动 `src/**`**、未提交（提交另派）。D10 的 FAIL→PASS 与形状 A 三路复跑为本批一手实测（形状与判定字段见 D10 段）；D9 的「3/3 超时」签名引自 oracle 文档轮；D7 的 CI 表述与 `:338` 形状归属在本批订正。
- **同上批的**第二关收口轮**（2026-10-01，`[MARK-FIX-SHOULD2]`）**：轮次＝ **oracle 第二关（判「可提交」+ 列 should-fix）→ 主代理按从严规则判「全部落掉」→ worker 收口轮（即本次改动）**。本轮改动＝本台账 + `test/**` 两处第三参（`test/integration/agent-index-service.test.ts:4889`、`test/unit/herdsman-pi-extension.test.ts:4758`，各带 1–3 行注释）+ `waitForTerminalDisconnect` 的 JSDoc 补两条前提（`:468-476`）；**未动 `src/**`、未动 `vitest.config.ts`、未提交**。本轮的**一手**新证据：D13 的 33 处全量枚举与四类计数（10+3+20）、两条用例的隔离单跑耗时（4014ms / 3025ms）与低载档 3 路并发占用（82% / 61%）、D16 占用率清单（含 `event-dedup` / `agent-history-discovery` 两处 HEAD 既有限额）、以及 D10 的**两种残余失败签名**（临时副本实测，副本已删）。**引自 oracle 本轮**的数字：高载档形状（26 个 busy-loop，loadavg 19→≈28；node v22.23.1）3/3 全绿、屏障点实耗 144/113/101/125ms、重试循环 198ms。
- 本会话实测：`node scripts/check-root-package.mjs` 直跑失败（pnpm 横幅污染 stdout）而 `pnpm package:check` 绿；
  `test/unit/clean-agent-event-duplicates.test.ts` 的锁用例 flake **两次**（15:44 一次已落盘、关键三行摘录于 A5(b)；
  18:13 / 18:26 一次输出未落盘，见 A5(a)）。
- 相关笔记（相对链接）：[`20260930-terminal-event-delivery-h1.md`](20260930-terminal-event-delivery-h1.md)、
  [`20260930-phase1-delivery-latency.md`](20260930-phase1-delivery-latency.md)、
  [`20260930-terminal-event-delivery-latency-observation.md`](20260930-terminal-event-delivery-latency-observation.md)、
  [`wake-delivery-r4f1-remaining-risks.md`](wake-delivery-r4f1-remaining-risks.md)。
- 提交：`b59ac96`（H1）、`5433525`（Phase 1）；共同基线 `7c4a36b`。
- 索引：本笔记加入后运行 `scripts/notes-index.sh` 刷新 `INDEX.md`（该文件在 `.gitignore` 内，不提交）。

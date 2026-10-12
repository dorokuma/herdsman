---
status: active
superseded_by: ""
supersedes: ""
模块: observability
---

# 批次一小修补轮：遗留登记（超窗缺 warn / M2 ref 用例 / 规则目录脆弱测试 / 审查项处置 / 生产库误施 0011 事故）

## 一句话结论

批次一小修补轮（分支 `fix/todo-batch-1`，基线 `dd60f6b` 之后续提交）把审查项的 should fix #1（AGENTS.md 内部两处口径互相矛盾）与 #2（笔记文件名日期串写错）、consider #3（守卫窗口量级的口径）与 #4（新臂之后的不可达 else）全部修掉，consider #5 并入项 1（测试动机注释写明「防再次漂移」）。本笔记登记 3 条**遗留**：a) 守卫扫描窗口打满（超窗）时缺一次性 warn、超窗漏拦与正常放行**不可区分**（单列后续项）；b) M2 守卫回归测试缺「同正文不同 ref 必须放行」用例；c) `/root/.pi/agent` 规则目录 `test_dispatch_agent.py` 的 `test_pi_override_of_non_pi_role_requires_model` 依赖真实 frontmatter 执行器分布、易假红/假绿（挂规则目录待办，本仓只留指针）。另**如实记录一次生产库事故**：J3 复现时一次包装漏导出 `HERDSMAN_HOME`，使分支代码的 `pnpm db:migrate`（含新建的 `drizzle/0011_lazy_enchantress.sql`）作用于生产库 `/root/.herdsman/state.db`，`EXIT=0`；只读核验现状为 `__drizzle_migrations` 12 行、`agent_events_guard_scan_idx` 已存在且定义与 0011 一致、0011 为**纯加索引**无数据变更、已部署的 0.14.4 不含该迁移；**保留 / 回退留主代理决策**，本笔记不做写库操作。

## 背景

- **轮次与范围**：`[MARK-WORKER-HSM-007]` 批次一小修补，六项——① AGENTS.md 验证步骤节改发布面口径 + 扩 `test/unit/deployment-guidance.test.ts`；② canary 基线笔记改名（创建日口径）+ INDEX / 引用同步；③ `agent-index-service.ts` 守卫窗口注释补口径；④ 同文件末尾不可达 else 的防御性注释；⑤ 本笔记；⑥ `pnpm check` 全量门禁（结果见交付报告，本笔记不写未发生的 commit hash）。
- **基线**：`fix/todo-batch-1` 上 `dd60f6b`（批次一 canary 基线笔记）之后续提交；`main` 不动、不 push、不合并。
- **触发条件**：`.agents/notes/README.md` 触发条件 1（SQLite 索引 / schema 面事实变更 ← 事故 e）、4（遗留 workaround / 特判）、5（与既往口径的有意识分歧 ← 项 1 把「部署前复跑」改为发布面口径），叠加仓库「遗留项必须落盘」的惯例。
- **审查项来源**：批次一评审报告（reviewer should fix #1/#2、consider #3/#4/#5）与 oracle 只读复核（点 1 / 点 3 / 点 4），逐条处置见下 d)。

## 决策

### a) 超窗（扫描窗口打满）缺一次性 warn —— 单列后续项

- **现状**：`STALE_DUPLICATE_GUARD_SCAN_LIMIT = 500` 的取值不动（本轮项 3 只改注释）。守卫查询是 `... and status in ('delivered','acked') order by id desc limit 500`，按 `(agent_id, herdr_session_name)` 分组取最近 500 行。
- **缺口**：当某分组在 7 天 settled TTL 内的 delivered/acked 终态行**超过** 500 时，更老的行对这次查询不可见。（i）「超窗漏拦」——更老的正文复现，被当成本轮答案放行；（ii）「正常放行」——正文确实是本轮答案。两者在事件 payload 与日志上**不可区分**：都命中 `diskDeliverable`，都不带 `degraded`、都不带 `staleSnapshot` 记录，也没有任何一次 warn。也就是说超窗这件事现在**没有观测锚点**（`.agents/notes/20261006-unconfirmed-frozen-baseline-delivery.md` 遗留 14 把它记为既定代价，代价本身成立，缺的是「发生了」的信号）。
- **处置**：**单列**为后续项（oracle 点 3 建议；主代理裁量决定单列而非并入其它遗留）。修法未定：候选是把「窗口被打满」本身作为信号（例如扫描返回行数达到上限时打一条一次性 warn），但是否会引入误报（分组恰好等于 500 行时并不代表一定有漏拦）需先设计；本轮只登记、不动码。
- **触发条件**：下次触及 M2 守卫 / 投递面（`#appendStatusEvents` 的 mismatch 与 frozen-baseline 两臂）时同批设计。

### b) M2 回归测试缺「同正文不同 ref 必须放行」用例

- **现状**：`sameTerminalAssistantContent`（`src/observability/agent-index-service.ts`）对 pi 的口径是「两侧 `ref` 都非 null 时**按 ref 比较**，ref 相同才算重复」，文本比较只在 ref 缺失时才落。因此「正文文本相同、但 ref 不同」**必须**视为新轮次放行。
- **缺口**：M2 的守卫回归用例（`test/integration/turn-completion-signal.test.ts`）覆盖了「判重」方向——`M2 guard` 两例（`:2148`、`:2319`）都以「当前轮的 ref 与已投递行的 ref 相等」为命中条件，而**没有**「同正文不同 ref → 必须放行」这条反向用例（oracle 点 1）。缺它的后果是：将来若把 `sameTerminalAssistantContent` 的 ref 优先级退化成纯文本比较，守卫会把一个新轮次误判为重复而把正文打空（正是 M2 要防的丢正文那类故障），而现有用例不会红。
- **核对边界（如实）**：本轮只做定点核对（两份 M2 守卫用例 + 比较函数本体），确认未见该反向用例，**未**对两个测试文件做全量用例审计；因此这条按「oracle 报缺、本轮未反证」登记。
- **处置**：登记为后续测试项；**本轮不补**——本轮测试面只允许项 1 的既有断言增强（既有文件内加断言，不新增文件、不新增用例主体），补这条需要一条完整的新用例，超出本轮测试纪律。
- **触发条件**：下次触及守卫比较函数或 M2 测试文件时同批补。

### c) 规则目录待办：`test_pi_override_of_non_pi_role_requires_model` 依赖真实 frontmatter 执行器分布

- **位置**：`/root/.pi/agent/scripts/test_dispatch_agent.py:374`（本仓之外，**挂规则目录待办**，本笔记只留指针，不在本仓动作）。
- **脆弱点**：该用例遍历 `ROLES`（`scout/planner/researcher/worker/verifier/reviewer/oracle`），对每个角色读**真实** `agents/<role>.md` frontmatter 的 `executor:`（`_frontmatter_executor`，`:32-41`），据默认执行器分流：默认 `pi` 的角色要求 `--executor pi --print-resolved` 返回 0 并回 `executor=pi`；非 pi 的角色要求返回 2 并回 `--executor pi overriding a non-pi role requires --model …`；最后断言「至少一个 pi 默认角色」（`saw_pi`）与「至少一个非 pi 角色」（`saw_non_pi`）。
- **后果**：判据与被测对象**同一份数据**——只要有人把角色文件的 `executor:` 分布改动（例如把全部角色改成 `pi`，或把最后一个非 pi 角色改成 pi），用例会**假红**（`saw_non_pi` 断言失败）或**假绿**（分支不再被覆盖）；新增 / 删除角色文件（`ROLES` 元组不同步）同样如此。加固方向是让用例自己造 fixture 角色文件（或对分布做显式前置断言并给出清晰失败信息），但那是规则目录自身的改动。
- **触发条件**：规则目录下次批量改角色文件（`executor:` / `model:` / 角色增删）前后各跑一次该用例；或该用例出现红且失败信息指向分布而非被测逻辑时。

### d) 审查项处置记账

| 项 | 内容 | 处置 | 落点 |
| --- | --- | --- | --- |
| should fix #1 | AGENTS.md 内部矛盾：部署 bullet 已写「不复跑」，验证步骤节仍写「部署或发布前必须再跑一次」 | **已修** | `AGENTS.md` 验证步骤节改为发布面口径（点名 `docs/releasing.md`「Validate source and package contents」，并写明部署步骤不复跑）；`test/unit/deployment-guidance.test.ts` 新增第二个用例做两处一致性断言 |
| should fix #2 | canary 基线笔记文件名日期串写错（`20260213-`） | **已修** | `git mv` → `.agents/notes/20261012-todo-batch1-canary-baseline.md`（创建日口径）；正文「背景」补一行改名口径说明；`scripts/notes-index.sh` 重生成 INDEX.md；`rg` 核对仓内无旧路径残留（正文「来源」节引用的是其它笔记，未受影响） |
| consider #3 | 245/207 是 pane 口径，易被读成守卫自身分组口径 | **已修** | `agent-index-service.ts` 的 `STALE_DUPLICATE_GUARD_SCAN_LIMIT` 注释补一句：245/207 为**pane 口径上界代理**，守卫实际按 `(agent_id, herdr_session_name)` 分组、canary 实测该分组最大 **30**；**500 取值不动** |
| consider #4 | 新增 mismatch 臂后末尾 `else` 理论不可达 | **已修** | 同文件末尾 `else` 补防御性注释（说明 `diskDeliverable ≡ 终态 ∧ 非空 ∧ !staleBaselineDuplicate`，各否定分支已被上文各臂认领 → 该臂不可达；保留为非降级的放行兜底，防将来给 `diskDeliverable` 加合取项时无对应臂） |
| consider #5 | 项 1 的测试应写明动机、防两处再次漂移 | **并入项 1** | 新用例的动机注释写明「两处曾经互相矛盾」，并以「两处都点名同一发布面步骤 + 两处都写明不复跑 + 两处都不得再要求部署前复跑」三组断言钉住（注释内含「防再次漂移」） |

- 项 3 与项 4 都落在同一文件、同一 `pnpm check` 轮次，均**纯注释、零行为变化**（`git diff` 只有注释行进出）。

### e) 事故记录：生产库被误施 0011 迁移（`agent_events_guard_scan_idx`）

- **事故**：J3 复现时一次包装漏导出 `HERDSMAN_HOME`，命令 `pnpm db:migrate` 在**分支工作树**执行，作用于**生产库** `/root/.herdsman/state.db`，`EXIT=0`（无报错、无提示）。
- **机制（两个条件叠加，缺一不成）**：
  1. `src/db/migrate.ts` 用 `resolveRuntime()` 解析路径；`HERDSMAN_HOME` 未显式指定时落默认数据目录 `~/.herdsman`（AGENTS.md：「默认数据目录 `~/.herdsman` 即生产；开发/验证请显式指定临时目录」）。
  2. 同文件的 `applyMigrations(sqlite, { migrationsFolder: "drizzle" })` 里 `migrationsFolder` 是**相对 cwd** 的路径，于是读的是**分支工作树**的 `drizzle/`（12 个 `.sql`，含新建的 `0011_lazy_enchantress.sql`），而不是已部署 0.14.4 的 11 个。
- **旁证**：本轮 verifier 的隔离 home `/tmp/verify-j3-home-1` 是**空目录**（预期承载迁移的临时 home 里从未出现 `state.db`——迁移没写在那里）；同轮重跑用的 `/tmp/verify-j3-home-2/state.db` 存在（131072 字节）。
- **现状只读核对（写作时刻，全部 `sqlite3 -readonly`）**：
  - `select count(*) from __drizzle_migrations` = **12**。对照 `git ls-tree v0.14.4 drizzle/` = 11 个 `.sql`（`0000`..`0010`）→ 该表比已部署 0.14.4 **多一行**。
  - 第 12 行 `hash = 6d023a5957a48f13af55ab30e3b2c0761015712ede53621da8228f6dc17b1664`，与 `sha256sum drizzle/0011_lazy_enchantress.sql` 的 **完全一致**（即库里那行就是本分支的 0011）。
  - `sqlite_master` 中 `agent_events_guard_scan_idx` 的 SQL 与 0011 的定义一致（`ON agent_events (agent_id, herdr_session_name, id) WHERE status in ('delivered','acked')`）。
  - `agent_events` 5384 行（活库，随时刻变化）；`degradedReason='stale_baseline_duplicate'` 仍为 **0**。
- **时间口径的局限（重要，勿据库内 `created_at` 判施加时刻）**：`__drizzle_migrations.created_at` 落下的是 `migration.folderMillis`（drizzle journal 的 `when`，即**迁移文件生成时刻**），不是施加时刻——`src/db/apply-migrations.ts` 用它落库、并以 `created_at >= folderMillis` 决定跳过。所以库里**无法**恢复 0011 被施加到生产的确切时刻；0011 的 `when` = 1791816504631 = **2026-10-12T14:48:24Z**（本机 22:48:24），只能证明「文件生成于何时」。
- **影响评估**：
  - 0011 内容**只有一条 `CREATE INDEX`**（部分索引），**不改数据、不加删列、不删行**；生产库上该索引已存在且与定义一致，故**无数据损失、无语义变化**。
  - 唯一实质后果是**版本身份偏移**：生产库 schema 领先已部署的 0.14.4 一个索引。该索引正是本分支的目标产物，**与隔离执行一致**：隔离 home 的 `/tmp/verify-j3-home-2/state.db` 同为 `__drizzle_migrations` **12 行** + 同名 `agent_events_guard_scan_idx`，即误施到生产的 0011 与 J3 隔离执行的结果在内容上一致（0011 本身是纯加索引）。
  - 不对称风险：a) 若将来把 0011 从发布物去掉或改写，生产库会与 registry 发布物分叉——注意 drizzle 以 `folderMillis` 判跳过，**改写文件内容但保留 `when` 的话迁移会被跳过**，库内 hash 与发布物永久不一致；b) 索引存在会让部分查询走该部分索引，而生产 daemon 仍是 0.14.4 的行为，功能面不受影响（查询计划变化不是行为变化）。
- **后续决策（留主代理，本笔记不下结论）**：
  - **保留（本轮默认不动作）**：本分支本来就要发布这条索引，保留即与下一版发布物对齐。
  - **回退**：`DROP INDEX agent_events_guard_scan_idx` + 删 `__drizzle_migrations` 的 0011 行，使库回到 0.14.4 口径，等正式发布时再重新加上——属**写生产库**，须显式授权，且手工写库本身有风险。
  - 两条路都须记住：registry 不允许同版本重发，**版本号即部署身份**（AGENTS.md）；本事故不改动任何已发布制品，也不改变 0.14.4 的部署事实。

## 被放弃的方案（必填）

- **本轮回退生产库（`DROP INDEX` + 删 `__drizzle_migrations` 行）**：放弃——任务要求「现状只读核对」，写生产库须主代理决策；且 0011 是纯加索引、与本分支目标一致，回退的净收益集中在「版本身份整齐」而非数据安全。
- **给 0011 加 `IF NOT EXISTS` 或改成可重复执行**：放弃——会改写**已落库（hash 已按现文件记入）**的历史迁移文件，drizzle 迁移体系下已施加迁移不可改写；改写反而制造「库内 hash 与发布物不一致」这一新问题。
- **不记录这次误施（当作「合并后反正也会施」）**：放弃——命中留痕触发 1（SQLite 索引 / schema 面事实变更），且会让今后生产库与发布物的差异无从解释；生产库已被写这一事实必须留痕。
- **把「`migrationsFolder` 改绝对路径」或「`HERDSMAN_HOME` 缺失时拒绝 migrate」当本轮改动做掉**：放弃——属行为面改动、超出本轮小修补范围；此处**只作为候选加固记录，不登记为待办项**，避免范围蔓延。
- **在项 1 新增测试文件 / 为其它项补测试**：放弃——项 1 按测试纪律在既有文件内加断言（不新增用例主体以外的文件），其余项纯注释 / 改名 / 笔记，均不新增测试。
- **把遗留 a/b 并入同一项或顺手修掉**：放弃——a 需先设计（「窗口打满」不等于一定漏拦，直接打 warn 有误报空间），b 需要一条完整新用例（超出本轮测试面）；两者都单列并写触发条件。

## 来源

- 任务书：`[MARK-WORKER-HSM-007]`（批次一小修补六项，`fix/todo-batch-1` 基于 `dd60f6b` 之后续提交，不 push 不合并）。
- 审查 / 复核：批次一评审报告的 should fix #1/#2 与 consider #3/#4/#5；oracle 只读复核点 1 / 点 3 / 点 4（分别对应遗留 b / 遗留 a / 遗留 c 的指针）。
- 代码与数据事实：`AGENTS.md`（验证步骤节、本机运维部署 bullet、默认数据目录口径）、`docs/releasing.md`「Validate source and package contents」、`src/observability/agent-index-service.ts`（`STALE_DUPLICATE_GUARD_SCAN_LIMIT` 注释、mismatch 各臂与末尾 else、`sameTerminalAssistantContent`）、`src/db/migrate.ts` + `src/db/apply-migrations.ts`（`migrationsFolder: "drizzle"` 相对 cwd、`created_at = folderMillis`）、`drizzle/0011_lazy_enchantress.sql` 与 `drizzle/meta/_journal.json`、`test/unit/deployment-guidance.test.ts`、`test/integration/turn-completion-signal.test.ts`、`/root/.pi/agent/scripts/test_dispatch_agent.py:32-41,374-392`（仓外，指针）。
- 只读核对命令：`sqlite3 -readonly /root/.herdsman/state.db`（`__drizzle_migrations` 计数与 hash、`sqlite_master` 的索引 SQL、`agent_events` 计数与 `stale_baseline_duplicate` 计数）、`git ls-tree --name-only v0.14.4 drizzle/`、`sha256sum drizzle/0011_lazy_enchantress.sql`。
- 相邻笔记：`.agents/notes/20261012-todo-batch1-canary-baseline.md`（改名后的 canary 自身基线，含「守卫按 `(agent_id, herdr_session_name)` 最大 30」的实测）、`.agents/notes/20261002-m2-disk-body-and-canary-read.md`（M2 守卫口径与遗留 3）、`.agents/notes/20261006-unconfirmed-frozen-baseline-delivery.md`（遗留 14：单 pane 245 条量级与超窗漏拦代价）、`.agents/notes/README.md`（留痕规约）、`.agents/notes/_template.md`（frontmatter 范本）。
- 局限：活库计数随时刻变化，引用须连同时刻；生产库 0011 的**施加时刻**不可从库内恢复（见 e 的时间口径小节）。

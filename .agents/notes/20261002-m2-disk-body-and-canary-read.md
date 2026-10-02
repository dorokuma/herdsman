---
status: active # active | superseded
superseded_by: ""
supersedes: ""
# 模块可选值: observability, daemon, cli, config, db, herdr, shared, herdsman-pi, herdsman-herdr-plugin, release
模块: release
---

# 0.13.6 / M2：daemon 采信盘上正文（判据与 `confirmed` 解耦）与 canary 读法

## 一句话结论

0.13.6 的主题是 **M2**：把 daemon 侧「是否采信盘上正文」的判据从 `turn.confirmed` **解耦**——只要 turn-end 信号已到（`turn?.received`）且盘上有可交付的终态正文（`isTerminalAssistant && hasNonEmptyAssistantMessage && !staleBaselineDuplicate`），就采信盘上原文并交付，不再因扩展侧 `confirmed=false`（文本级确认 never-match：尾部被改写型扩展改写、多行/凭据脱敏形态）把已经落盘的正文抹空。`confirmed` 只保留给 released/degraded 的**日志与载荷语义**（④ 的 `released a confirmed pi status event with no deliverable text` 仍只在 confirmed 轮发射；① 的 mismatch warn 拆成 confirmed / unconfirmed 两串，均含子串 `despite an expectedText mismatch`）。配套把 `staleBaselineDuplicate` 守卫由「单条最新终态行」扩到「该 agent 最近 **≤200 条**已投递/acked 的终态行」（`STALE_DUPLICATE_GUARD_SCAN_LIMIT = 200`）。**本版目标格（`confirmed=false` 且 mismatch）在现网极稀：可覆盖窗口（2026-09-29T01:50:07Z→2026-10-02T15:29Z，≈3.57 天）内 `confirmed=false` 仅 1 条（同期 `confirmed=true` 841 条），且该轮盘上有 6186 字非空终态正文、无 `degradedReason` ⇒ 不在目标格，观测到的 M2 收益 = 0。M2 影响面很小**；空正文的大头（`no_advance_from_input`）本版未触及。**无 schema/migration、无扩展侧改动**；`src/**` 仅 `src/observability/agent-index-service.ts`。**截至本笔记落盘，本版未发布、未部署**（本笔记只做发布准备，版本号/文档/记录；不含 commit/push/publish/deploy）。

## 背景

- 上一版（0.13.5）的 daemon 侧修复 ① 只救了 **confirmed 轮**：`confirmed=true` 且 mismatch 时改走 warn + 保留盘上正文。**unconfirmed 轮**（扩展文本级确认 never-match ⇒ `confirmed=false`）仍会落入 `else if (!advancedMatchesExpected)` 把 `lastAssistantMessage` 置 `null` 并 `degradeOrRelease("expected_text_mismatch")` ⇒ 空 `agent.done`。根因、四项修复与 `confirmed` 硬合取项见 `.agents/notes/20261002-empty-wake-expectedtext-rootcause.md`；M2 在其中的「已知残余与后续方向」被列为**终局方向**。
- M2 落地在 `main` 上的两个内容提交（`git log --oneline v0.13.5..HEAD`，含 merge）：`1b49443` → merge `8c2b07b`（`fix(daemon): trust the on-disk body for unconfirmed turns (M2)`）。
- **本版范围（0.13.5 → 0.13.6 净差）**：`git diff --name-status v0.13.5..HEAD` = 6 文件——`src/observability/agent-index-service.ts`（M2）、`test/integration/agent-index-service.test.ts`（S7/S8 改写）、`test/integration/turn-completion-signal.test.ts`（新增用例）、`test/unit/daemon-process-manager.test.ts`（flaky 握手修复）、`.agents/notes/20261002-empty-wake-expectedtext-rootcause.md`（canary 口径回写）、`.agents/notes/20261002-release-0.13.5.md`（0.13.5 发布落地记录，新增）。**无 schema 变更、无迁移、`packages/**` 无源码改动**。

## 改动事实（含真实 file:line）

### 1. M2：采信判据与 `confirmed` 解耦（`src/observability/agent-index-service.ts`）

- 新增解耦的可交付条件（`:2102-2105`）：
  `const diskDeliverable = isTerminalAssistant(advanced) && hasNonEmptyAssistantMessage(advanced) && !staleBaselineDuplicate;`
  —— 不再含 `confirmedTerminal`（旧代码此处是 `confirmedDeliverable = confirmedTerminal && …`）。外层 `if (turn?.received)` 已保证信号到达，故判据只需回答「盘上是否载有可交付正文」。
- mismatch 臂改按 `diskDeliverable` 采信（`:2106` `if (diskDeliverable) {`）：命中时 `compactHistory = advanced`（`:2124`）**保留盘上正文**、`payloadExtra = { staleSnapshot: false }`（`:2122`），**不置 `degraded`**（置了会让 `#runPlanRow` 对已写内容再 `invalidateById(..., "degraded_retry")` 废一遍）。未命中且 mismatch 仍走 `degradeOrRelease("expected_text_mismatch")`（`:2127`），未命中且非终态/空末尾仍走 `non_terminal_assistant`（`:2128-2135`）——两臂语义不变。
- ① 的 warn 拆成两串（`:2108-2121`）：`confirmedTerminal ? "Herdsman accepted a confirmed pi status event despite an expectedText mismatch" : "Herdsman accepted an unconfirmed pi status event despite an expectedText mismatch"`；载荷新增 `confirmed: confirmedTerminal` 字段；两串都含子串 `despite an expectedText mismatch`（既有 canary grep 仍命中）。
- `confirmedTerminal`（`:1954`）**保留**，只用于 released/degraded 的日志与载荷：④ 的签名 `Herdsman released a confirmed pi status event with no deliverable text`（`:1971`）仍**只在** confirmed 轮发射（`if (confirmedTerminal)` 分支，`:1963`）；`degradeOrRelease`（`:1962-1990`）行为不变。最终日志 `Herdsman emitted pi agent.${input.to} after turn completion signal (confirmed=${turn.confirmed})`（`:2142`）不变。
- 新增容错助手 `parseCompactHistoryJson`（`:2772`）：把 `compact_history_json`（可能为 `null` 或非法 JSON）安全解析为 `CompactAgentHistory | null`。

### 2. `staleBaselineDuplicate` 守卫加宽（`src/observability/agent-index-service.ts:122`、`:2040-2060`）

- 旧守卫（0.13.5 及以前）只比对「该 agent 最近一条未失效终态行」（`latestTerminal?.compactHistory`）。
- 新守卫（`:2040-2060`）改查参数化 SQL（`:2043-2049`）：
  ```sql
  select compact_history_json from agent_events
  where agent_id = ? and herdr_session_name = ?
    and type in ('agent.idle', 'agent.done', 'agent.blocked')
    and status in ('delivered', 'acked')
  order by id desc limit ?
  ```
  以 `STALE_DUPLICATE_GUARD_SCAN_LIMIT = 200`（`:122`）为窗口，逐行 `sameTerminalAssistantContent(advanced, parseCompactHistoryJson(row.compact_history_json), "pi")`（`:2054-2060`）判定；`.some()` 命中即 `staleBaselineDuplicate = true`。查询只在 `#waitForHistoryAdvance` 之后的兜底分支执行（`requireAssistantChange` 重试路径），**快路径不跑**。

**三点（与 CHANGELOG 同源）**：

- **① 为什么是 200**：`agent_events` 的 settled TTL 为 7 天（终态行保留上限），窗口内**单个 agent 的已投递/acked 终态行实测上限 249**（本笔记复核 2026-10-02T15:29Z 实测 249，见下基线表）——故 200 覆盖绝大多数、留了余量，同时把读量收在几百 KB JSON（代码注释 `:113-117` 记「around 250」）。
- **② >200 行的漏拦**：窗口外的更旧正文若在尾部复现，守卫不再识别为重复，可能被再次当本轮答案放行。这是「**有界扫描换确定性**」（不遍历该 agent 全部终态行）的**既定代价**，已在代码注释（`:118-120`）与本记录写明。
- **③ 口径变更**：新查询只统计 `status in ('delivered','acked')` 的行——7 天窗口内带 ref 的 agent.idle 行 **1075** 条，其中 **981 条 `agent_id` 为空**（守卫按 `agent_id = ?` 查，看不到它们）；守卫作用域内只有 **94** 条，其中 **93** 条 `status in ('delivered','acked')`（即落在守卫窗口内），**47 条（≈50%）的 ref 与同 `(agent_id, herdr_session_name)` 的 `done` 行相同**（非 `working` 起点那 **49** 条里有 46 条如此）⇒ 若把这类行计入窗口，它们会与同 ref 的可交付轮争夺同一尾部位置（构成比：守卫作用域内 47/94 ≈50%），存在把可交付轮抹空的风险；`never deliverable` 只对其中非 `working` 起点的 49 条成立。（复核 2026-10-02T15:41Z；口径与 SQL 见「伤害读数」表的 idle-ref 行。）因此 **新守卫对从未投递的行拦得比旧版少，不再是旧逻辑的严格超集**。同一变量 `staleBaselineDuplicate` 同时被「基线未推进」臂消费（`:2065-2069` 的 `confirmedDeliverable`，`:2074-2083` 的 `if (confirmedDeliverable)`），口径变更对该臂同样生效。

### 3. 边界

- 仅 3 个源/测试文件（M2 实现 + 两处集成测试），**无 schema/migration、无扩展侧改动、无 transport/persistence 分层改动**。
- 窗口查询**内联在 service**（not in a store method）——见「被放弃的方案」。

## 测试清单

- `test/integration/turn-completion-signal.test.ts`（新增 4 条）：
  1. `:1294` `M2: an unconfirmed turn keeps the disk body when the signal expectedText mismatches` —— 正向：unconfirmed + mismatch + 盘上有合法终态正文 ⇒ 采信盘上正文、`staleSnapshot:false`、无 `degraded`、warn 命中 `expectedText_mismatch`；**改前 FAIL**。
  2. `:1384` `M2: an unconfirmed turn with no terminal disk body still degrades` —— 负向：盘上只有非终态工具轮 ⇒ 仍降级（`expected_text_mismatch`，正文为 `null`）；**护栏性质，改前后皆过**。
  3. `:2148` `M2 guard: a body already delivered as an earlier terminal row is not re-released` —— 守卫扩窗：round 3 复读 round 1 已投递的 `m1`（旧单条守卫只比 `m2`，会漏）⇒ 必须抹空并记 `stale_baseline_duplicate`；**改前 FAIL**。
  4. `:2235` `M2 branch B: an unconfirmed mismatch over an already-delivered tail degrades instead of re-releasing it` —— 护栏：unconfirmed + mismatch 且尾部是已投递过的正文 ⇒ 降级而非重投；**改前后皆过**。
- `test/integration/agent-index-service.test.ts`（改写既有 2 条）：
  - `:4483` S7 由「degraded emission on expectedText mismatch after wait」改写为 `S7: an unconfirmed expectedText mismatch with a terminal body on disk is delivered`（对齐新契约：盘上终态正文被交付，`invalidated` 行 0）。
  - `:4560` S8 改写为 `S8: a degraded event retries until a terminal body arrives then completes`（首轮改成**非终态工具轮** `stopReason: null`，保住「降级→重试→完成」状态机覆盖）。
- `test/unit/daemon-process-manager.test.ts`（flaky 握手修复，见 `:944`、`:1030`）：读 holder PID 前轮询到文件就绪（`waitForCondition`），消除并行压测下 `readFileSync(holderPidFile)` 的 `ENOENT`。**纯测试改动，未动生产代码**。
- **用例总数 `847 → 851`**（0.13.5 = 53 files / 847 tests；本版 +4）。发布面 `pnpm check`（nvm node v22.23.1，2026-10-02T15:14Z 起：53 files / 851 tests，exit 0）与 `pnpm build`（exit 0）。

## canary 读法与假象清单

### 机制信号（M2 是否在动）

以**部署后重启时刻**为窗口左端：

```bash
journalctl -u herdsman.service --since <重启时刻> -o cat \
  | grep -c "accepted an unconfirmed pi status event despite an expectedText mismatch"
```

取值时把两串合计看：`grep -c "despite an expectedText mismatch"`（= confirmed 串 + unconfirmed 串）；**本机 24h 与全窗口两串均为 0**（2026-10-02T15:36Z 实测），且 confirmed 串本身也在 0，故新串为 0 属预期，不能据此判 M2 未生效；判「有没有换版本」只看 `npm ls -g` / `MainPID` / 重启时刻。分母看同窗口：

```bash
journalctl -u herdsman.service --since <重启时刻> -o cat \
  | grep -oE "turn completion signal \(confirmed=(true|false)\)" | sort | uniq -c
```

### 伤害读数（必须与**部署后自身基线**比）

| 指标 | 0.13.6 部署前基线（本笔记 2026-10-02T15:29–15:41Z 实测，主表快照 15:29Z） | 来源命令（可直接粘贴复算） |
| --- | --- | --- |
| 可覆盖窗口 `confirmed=false` / `confirmed=true` | **1 / 841**（窗口 2026-09-29T01:50:07Z→2026-10-02T15:29Z，≈3.57 天；24h 内为 **1 / 229**，2026-10-02T15:37Z 实测） | `journalctl -u herdsman.service -o cat \| grep -oE "turn completion signal \(confirmed=(true\|false)\)" \| sort \| uniq -c` |
| ① 两串（unconfirmed / confirmed） | **0 / 0**（24h 与全窗口均 0；合计 `despite an expectedText mismatch` 也 0；2026-10-02T15:36Z 实测，journal 起点 2026-09-29T01:50:07Z） | `journalctl -u herdsman.service --since "24 hours ago" -o cat \| grep -c "accepted an unconfirmed pi status event despite an expectedText mismatch"`（把 `unconfirmed` 换成 `confirmed` 即 confirmed 串；去掉 `--since` 为全窗口；两串合计用 `grep -c "despite an expectedText mismatch"`） |
| 目标格实测（对应 `agent_events.id=50978`） | `status=acked`、`deliverable=0`、`degradedReason=NULL`、盘上正文 **6186** 字（`stopReason=stop`）⇒ **不在目标格** | `sqlite3 -readonly /root/.herdsman/state.db "select id,type,status,deliverable,ifnull(json_extract(payload_json,'$.degradedReason'),'(none)'),length(json_extract(compact_history_json,'$.lastAssistantMessage.text')) from agent_events where id=50978;"` |
| journal ④ 三组（24h） | `no_advance_from_input` **19**、`stale_baseline_duplicate` **2**、`expected_text_mismatch` **1**，合计 **22** | `journalctl -u herdsman.service --since "24 hours ago" -o cat \| grep -A4 "no deliverable text" \| grep -oE "degradedReason: '[a-z_]+'" \| sort \| uniq -c` |
| `agent.failed reason=degraded`（24h） | **77**（本笔记两次读数 75（15:10Z）/ 77（15:29Z）；日噪声约 ±8/24h ⇒ ±2 量级的变化不可分辨） | `sqlite3 -readonly /root/.herdsman/state.db "select count(*) from agent_events where type='agent.failed' and json_extract(payload_json,'$.reason')='degraded' and created_at >= strftime('%s','now','-24 hours')*1000;"` |
| pi 空正文（24h） | **73/279 ≈26%** | 见下「构成」行 |
| ↳ 构成 | `no_advance_from_input` **38**（含 acked 35 / invalidated 3）+ 无 reason **22** + `non_terminal_assistant` **13** = **73**；**无 reason 与 `non_terminal_assistant` 都不在 M2 射程** | `sqlite3 -readonly /root/.herdsman/state.db "select ifnull(json_extract(payload_json,'$.degradedReason'),'(none)') dr,status,count(*) from agent_events where type='agent.done' and json_extract(payload_json,'$.agent')='pi' and created_at>=strftime('%s','now','-24 hours')*1000 and (json_extract(compact_history_json,'$.lastAssistantMessage.text') is null or json_extract(compact_history_json,'$.lastAssistantMessage.text')='') group by dr,status;"` |
| ↳ 其中无 reason 的 22 行 | **构成未解释的基线行**——行内无 `degradedReason`（confirmed 轮的 `degradeOrRelease` 只返回 `{ staleSnapshot: false }`、不写 `degradedReason`）；将来无法仅从该行判断 M2 是否影响它（其分组只存在于 journal ④ 的 warn 里） | 同上（取 `dr='(none)'` 行） |
| 单 agent 已投递/acked 终态行上限（7d，守卫窗口依据） | **249**（≈248–249，代码注释记 around 250） | `sqlite3 -readonly /root/.herdsman/state.db "select ifnull(agent_id,'(null)'),count(*) c from agent_events where type in ('agent.idle','agent.done','agent.blocked') and status in ('delivered','acked') and created_at>=strftime('%s','now','-7 days')*1000 group by agent_id order by c desc limit 3;"` |
| idle-ref 口径（7d，守卫作用域） | 7d 内带 ref 的 `agent.idle` 行 **1075** 条，其中 **981 条 `agent_id` 为空**（守卫 `where agent_id = ?` 看不到）；守卫作用域内 **94** 条、其中 **93** 条在守卫窗口（`delivered`/`acked`）；**47/94 ≈50%** 的 ref 与同 `(agent_id, herdr_session_name)` 的 `done` 行相同（非 `working` 起点那 49 条里有 46 条如此）⇒ 它们与同 ref 的可交付轮争同一尾部（构成比 47/94 ≈50%），存在抹空可交付轮的风险；never deliverable 只对其中非 working 起点的 49 条成立；2026-10-02T15:41Z 实测 | `sqlite3 -readonly /root/.herdsman/state.db "with idle as (select id,agent_id,herdr_session_name,status, json_extract(compact_history_json,'$.lastAssistantMessage.ref') r, ifnull(json_extract(payload_json,'$.from'),'') f from agent_events where type='agent.idle' and created_at >= strftime('%s','now','-7 days')*1000), done_refs as (select distinct agent_id,herdr_session_name, json_extract(compact_history_json,'$.lastAssistantMessage.ref') r from agent_events where type='agent.done' and agent_id is not null and json_extract(compact_history_json,'$.lastAssistantMessage.ref') is not null) select (select count(*) from idle where r is not null) m_all, (select count(*) from idle where r is not null and agent_id is null) m_nullagent, (select count(*) from idle where r is not null and agent_id is not null) m_scoped, (select count(*) from idle where r is not null and agent_id is not null and f<>'working') m_scoped_nonworking, (select count(*) from idle where r is not null and agent_id is not null and status in ('delivered','acked')) m_guardwin, (select count(*) from idle i where i.agent_id is not null and i.r is not null and exists(select 1 from done_refs d where d.agent_id=i.agent_id and d.herdr_session_name=i.herdr_session_name and d.r=i.r)) k_match;"` |

> **口径与窗口滑动**：上表为 **2026-10-02T15:29–15:41Z** 本笔记快照（`journalctl -u herdsman.service` + `sqlite3 -readonly /root/.herdsman/state.db`）；「来源命令」列均为可直接粘贴复算的命令原文。24h 计数随窗口左端滑动，± 数条属正常（如同一 `strftime('%s','now','-24 hours')` 口径下 pi 空正文在 15:10Z 读到 76/287、15:29Z 读到 73/279）；idle-ref 的 m 也随 7d 左端滑动（15:10Z 1079 → 15:29Z 1077）。该表为「部署前基线」，供「部署后与**自身**比」。

### 窗口（统计力）

目标格在可覆盖窗口内 **0–1 条/3.57 天（≈0.28 轮/24h）**⇒ 7 天（≈2000 轮）期望命中仅 **≈2 条**，仍判不了「有效率」。canary 只做两件事：确认机制被走到 + 确认没有量级级伤害。

### 会被误读成「变好」的假象（逐条写明）

1. **②/④ 的 `expected_text_mismatch` 组趋 0**：②（`payload.degradedReason='expected_text_mismatch'`）**不是恒 0**——盘上确实无合法终态正文时它仍会置位；④ 受臂序影响（mismatch 臂先于 non-terminal 臂判定），趋 0 只是不再以空投递形态出现。
2. **③ 的 1h 锯齿**：③（`invalidated_reason='degraded_retry'`）是「近约 1h 内作废、尚未被清扫」的残留窗口读数，不是 24h 累计；读数上下跳动多为清扫欠账，**不是回归**（见 `.agents/notes/20261002-release-0.13.5.md` 的 (b) 节）。
3. **空正文占比下降**：大头是 `no_advance_from_input`，**不在 M2 射程**；它的变化与本版无关。
4. **新串（机制信号）在数天内为 0 属预期**，不能据此判 M2 未生效；判「有没有换版本」只看 `npm ls -g` / `MainPID` / 重启时刻。取值时把 confirmed 与 unconfirmed 两串合计看（`grep -c "despite an expectedText mismatch"`），因为 confirmed 串本身在本窗口也是 0。
5. **① 的 0 ≠ 没有失配**：① 本来就在 0 附近（本窗口 confirmed 与 unconfirmed 两串 24h 均 0）⇒ 0 不能推出「没有失配」，也不能据此判「换了没」；真正判「换了没」看版本 / `MainPID` / 重启时刻，别用 ①。
6. **「已投递正文被抹空」的挂靠臂**：在 mismatch 臂里挂的是 `expected_text_mismatch`（臂序：该分支先于 `non_terminal_assistant`），只有**未推进臂**才挂 `stale_baseline_duplicate`；② 的 `no_advance_from_input` 组（基线 38/24h）与 ④ 的 stale 组（基线 2/24h）是同一变量（未推进臂）的两面，**任何一组单独下降都不能读成「修好了」**。
7. **M2 之后空正文占比可能微升**：守卫扩窗把部分「带正文放行」换成「抹空」（更旧正文复现被判重复），**那不是失败**——必须与部署后自身基线比。

## 回滚口径

- **最便宜 = 先不发布**（本版是纯 daemon 侧行为改动，未发布即无影响面）。
- **回滚触发阈值（与部署后自身 24h 基线比，连续两天成立才动）**：④ `stale_baseline_duplicate` 组 > 基线 + 10（基线 2/24h ⇒ >12）；或 ④ `no_advance_from_input` + `stale_baseline_duplicate` 合计 > 基线 ×1.5（基线 21/24h ⇒ >31）；或 pi 空正文占比 >35%（基线 73/279 ≈26%）；或 `agent.failed reason=degraded` > 基线 ×1.5（基线 75–77/24h ⇒ >~115）；或出现「同一 ref 的正文跨轮重复投递」的实证。**0.13.5 笔记 (d) 的「出现 `agent.failed reason=degraded` 即变差」不可沿用**：本机基线 75–77/24h，照字面读会每日自触发。
- 已部署后：钉版本装回上一完整发布版本 + 重启：
  ```bash
  PATH=/root/.nvm/versions/node/v22.23.1/bin:$PATH npm install --global @dorokuma/herdsman@0.13.5
  systemctl restart herdsman.service
  ```
  （**包名是 `@dorokuma/herdsman`**，勿写成 `@dorukuma/...`。）
- 随后核对：版本（`npm ls -g --depth=0`）/ `MainPID`（`systemctl show -p MainPID herdsman.service`）/ `herdsman daemon status` / `herdsman agent list` / 新串（`accepted an unconfirmed ...`）归零 / 优雅关停 journal `exitCode: 0`。
- **无需 DB 回退**：本版**无 schema/migration**；回滚期间被 acked 的行不会被重投（回滚只改 daemon 二进制的采信判据，不动 `agent_events` 数据）。回滚**不会纠正 M2 已经投出的内容**（那些行已 acked、正文已被编排者消费），只止住后续；若真出现「旧正文被当本轮答案」，需在该轮另行说明/标注。
- 扩展侧无需回滚：M2 **未改扩展源**（`packages/herdsman-pi` 干净）。

## 遗留登记

1. **目标格（`confirmed=false` 且 mismatch）极稀**：可覆盖窗口（2026-09-29T01:50:07Z→2026-10-02T15:29Z，≈3.57 天）内 `confirmed=false` 仅 **1** 条（同期 `confirmed=true` 841 条），且该轮盘上有 6186 字非空终态正文、无 `degradedReason` ⇒ **不在目标格，M2 观测收益 = 0，影响面很小**；判读需 7 天量级窗口。
2. **空正文大头未触及**：pi 空正文 24h **73/279 ≈26%**（2026-10-02T15:29Z），构成 `no_advance_from_input` **38**（含 acked 35 / invalidated 3）+ 无 reason **22** + `non_terminal_assistant` **13**；其中**无 reason 的 22 行是无 `degradedReason` 的「构成未解释的基线行」**（confirmed 轮 released 路径不写该字段），M2 是否影响它无法仅从该行判定。M2 只覆盖 mismatch 那一格；后续面向 `no_advance_from_input` 的设计另行评估。
3. **守卫扩窗的合法上移**：journal ④ 的 `stale_baseline_duplicate` 组基线 **2/24h**，扩窗后过去被尺寸假确认、走 confirmed 放行的轮次会落进该组 ⇒ 该组**可能上移**，须与部署后自身基线比。
4. **未加索引**：新守卫 SQL 走 `(agent_id, herdr_session_name, id)` 过滤 + `order by id desc limit 200`；`(agent_id, herdr_session_name, id)` 的**部分索引留待后续批次**，本批不加。
5. **窗口查询内联在 service**：`AgentEventStore.listRecentTerminalHistories`（把该 SQL 沉淀到 store，与 `#stores` 分层一致）留待重构；本批为满足「只许改实现文件 + 测试」的范围约束而内联。
6. **无 schema/migration、无扩展侧改动**：`src/**` 仅 `agent-index-service.ts`；`packages/**` 无源码改动；无 DB 迁移。
7. **已知缺口（M2 mismatch 臂最终 `else`，`:2136-2138`）**：当 `staleBaselineDuplicate=true` 且 `advancedMatchesExpected=true`（含多行未上报 `expectedText` 的情形）且末尾为终态非空正文时，落入最终 `else { compactHistory = advanced; }`（`:2136-2138`），**守卫不生效**，旧正文仍可能被当本轮答案投出。故本条守卫只保证「unconfirmed + mismatch 时不重投」，**未封死**该组合。

## 被放弃的方案（必填）

- **把共享变量 `confirmedTerminal` 整体降级 / 删除**：放弃。`confirmedTerminal` 仍承载 ④ 的签名（`released a confirmed pi status event with no deliverable text` 只在 confirmed 轮发射，`:1971`）与 `degradeOrRelease` 的 degraded/released 分支语义；整体降级会同时改掉这些日志/载荷语义，超出「只松一格」（采信判据）的范围。做法是**新增 `diskDeliverable`**（`:2102`），保留 `confirmedTerminal` 与 `confirmedDeliverable`（`:2065-2069`，仅供基线未推进臂）。
- **全表 / 无界历史扫描做陈旧重复守卫**：放弃。会在热路径上遍历该 agent 全部（至多 7 天 TTL 内）终态行，读放大不可控；改用**有界窗口 200**，并把「>200 行的漏拦」作为**既定代价**登记（见改动事实 ②）。
- **本批给守卫查询加索引**：放弃。加索引会引入 schema/migration（`(agent_id, herdr_session_name, id)` 部分索引），超出本批「无 schema 变更」的边界；留待后续批次（遗留 4）。
- **把 `confirmed=false` 的真降级路径（无合法终态正文）也一并放行**：放弃。盘上确无终态正文时采信会投出空/半截正文，M2 必须保留 `isTerminalAssistant && hasNonEmptyAssistantMessage` 两个合取项与 `staleBaselineDuplicate` 守卫（负向用例 `:1384`、护栏用例 `:2235` 钉死）。
- **把守卫 SQL 沉淀进 `AgentEventStore` 方法**（更符合分层）：放弃（本批）。受「只许改实现文件 + 测试文件」的范围约束，查询内联在 service；已在遗留 5 登记其重构去向。
- **M1（扩展侧退回尺寸兜底）与本版同发**：放弃。M2 已在 daemon 侧确立「盘上正文为权威」，无需再引入易误报的尺寸确认；M1 的前提（须在 ① 之后）与必要性均已消失。
- **重新设定绝对数值阈值（如 `>0.5%`）**：放弃。沿用「相对部署后自身基线」判据（见 canary 读法），绝对阈值低于真实地板会自触发误判（前版笔记已登记该量纲订正）。

## 来源

- `docs/releasing.md`（`## Update versions` / `## Update the CHANGELOG` / `## Validate source and package contents`）；`CHANGELOG.md` 0.13.6 段；`AGENTS.md`「生产部署渠道（唯一，registry）」与「架构与模块约束」。
- 相邻笔记：`.agents/notes/20261002-empty-wake-expectedtext-rootcause.md`（空唤醒根因 / 四项修复 / `confirmed` 硬合取项 / M1·M2 定义）、`.agents/notes/20261002-release-0.13.5.md`（0.13.5 发布落地事实、canary 持久口径与 1h TTL 量纲订正）。
- 源码核对点（本笔记逐处打开核对）：`src/observability/agent-index-service.ts:122`（`STALE_DUPLICATE_GUARD_SCAN_LIMIT = 200`）、`:1954`（`confirmedTerminal`）、`:1962-1990`（`degradeOrRelease`）、`:1971`（④ 签名）、`:2040-2060`（加宽守卫查询 + `.some`）、`:2065-2069`（`confirmedDeliverable`）、`:2074-2083`（基线未推进臂）、`:2102-2105`（`diskDeliverable`）、`:2106-2125`（M2 mismatch 臂）、`:2108-2121`（拆分的 warn）、`:2127` / `:2128-2135`（降级臂）、`:2136-2138`（M2 mismatch 臂最终 `else`）、`:2142`（emitted 日志）、`:2772`（`parseCompactHistoryJson`）。
- 测试核对点：`test/integration/turn-completion-signal.test.ts:1294`、`:1384`、`:2148`、`:2235`；`test/integration/agent-index-service.test.ts:4483`（S7）、`:4560`（S8）；`test/unit/daemon-process-manager.test.ts:944`、`:1030`（flaky 握手修复）。
- 本笔记的 canary 基线复测（**只读**）：`journalctl -u herdsman.service`（`turn completion signal (confirmed=…)` 分组、`grep -c "no deliverable text"` + `grep -A4 … | uniq -c`）；`sqlite3 -readonly /root/.herdsman/state.db`（分母 `type='agent.done' and json_extract(payload_json,'$.agent')='pi'`、空正文条件、`agent.failed reason=degraded`、`delivered/acked` 终态行按 agent 计数）；`node` + `node:sqlite` 只读脚本复算 idle ref 交集。**复测时刻 2026-10-02T15:25–15:41Z**（主表快照 15:29Z；① 两串与 24h 窗口分列补测 15:36–15:37Z；S1 口径拆解补测 15:41Z）；脚本落 `/tmp`（一次性，非仓库产物）。
- 发布面本轮复跑（nvm node v22.23.1）：`pnpm check`（2026-10-02T15:14Z 起：53 files / 851 tests，exit 0）与 `pnpm build`（exit 0）；命令 `PATH=/root/.nvm/versions/node/v22.23.1/bin:$PATH pnpm check`。
- 发布准备任务：`[MARK-RELEASE-0136-PREP]`（本批只做版本号 4 处、README 2 处、CHANGELOG 段、本笔记；**未 commit/push/publish/deploy，未启停服务**）。

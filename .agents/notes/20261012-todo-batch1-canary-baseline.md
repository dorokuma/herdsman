---
status: active
superseded_by: ""
supersedes: ""
模块: observability
---

# 批次一遗留（项 6）：生产库 canary 自身基线快照

## 一句话结论

2026-10-12T14:56Z 对生产库 `/root/.herdsman/state.db` 的只读快照：**最近 24h** degraded 终态事件 **44** 条（`non_terminal_assistant` 30 + `no_advance_from_input` 14），**`stale_baseline_duplicate` 0 条**；**库内全部** degraded **162** 条（`no_advance_from_input` 88 + `non_terminal_assistant` 74），`stale_baseline_duplicate` 同样 **0 条**；库内现存 `agent_events` 5387 行（最早 2026-08-27）。批次一（M2 mismatch 臂守卫 + `agent_events_guard_scan_idx` 部分索引 + 守卫扫描窗口 200→500）上线后以此**自身基线**对比：`stale_baseline_duplicate` 组从 0 起的任何上移都须解释（M2 笔记遗留 3 已预告该组可能上移）。

## 背景

- 批次一任务项 6：对生产库做只读快照并落盘，作为 G-13 后续对比的自身基线（相对基线口径，非绝对阈值）。
- 生产库路径按 AGENTS.md「默认数据目录 `~/.herdsman` 即生产」= `/root/.herdsman/state.db`；全程 `sqlite3 -readonly`（未写库、未启停服务、未改生产目录）。
- **表结构纠偏（先 `.schema agent_events` 核对）**：任务示例 SQL 里的 `reason` 列**不存在**于实际 schema。degraded 轮的 reason 落在 `payload_json.degradedReason`，是否 degraded 看 `payload_json.degraded`；`agent_events` 确有 `invalidated_reason` 列，但本次快照中该列**全为 NULL**（失配于 M2/0.13.6 之前的行），故 reason 统计一律走 `payload_json`。
- **`created_at` 单位是毫秒**（`max(created_at)=1791816154941`），24h 窗口必须写 `created_at > strftime('%s','now')*1000 - 86400000`；按秒写会把窗口放大 1000 倍（等价全库），基线失真。

## 决策

### 口径
- 快照时刻：**2026-10-12T14:56Z**；24h 窗口 = `1791730598000..1791816998000`（ms）。
- 库是**活库**：同一分钟内行数从 5385 涨到 5387，故所有计数只在上述时刻内有效。
- 「库内全部」= 库内现存行（最早 2026-08-27，受 settled TTL 清理影响），**不等于**全历史。

### 计数（degraded 口径 = `json_extract(payload_json,'$.degraded')=1`）

| 指标 | 最近 24h | 库内全部 |
| --- | --- | --- |
| degraded 总计 | 44 | 162 |
| ├ `non_terminal_assistant` | 30 | 74 |
| ├ `no_advance_from_input` | 14 | 88 |
| └ `stale_baseline_duplicate` | **0** | **0** |
| `staleSnapshot=true` | 0 | 0 |
| 失效行（`status='invalidated'`） | 21 | 32（`agent.done` 11 / `agent.idle` 10 / `agent.failed` 6 / `agent.status.changed` 5） |
| `agent.done` 空正文（`lastAssistantMessage` 为 null） | 21 | 57 |
| pi `agent.done` 总数 | 95 | 1046（其中 degraded 57） |
| 行总数 | 627 | 5387 |

行类型构成（库内全部）：`agent.status.changed` 3091、`agent.idle` 1168、`agent.done` 1069、`agent.failed` 58、`agent.discarded` 1；状态构成：`acked` 5321、`pending` 34、`invalidated` 32。

### 守卫窗口量级复核（项 4 改 200→500 的依据）

| 分组 | 最大值 |
| --- | --- |
| 守卫自身的行集（`type in (idle,done,blocked)` 且 `status in (delivered,acked)`）按 `(agent_id, herdr_session_name)` | 30 |
| 同上按 `pane_id` | 399 |
| acked `agent.done` 按 `terminal_id` / `pane_id` | 207 |

2026-10-06 的 oracle 只读复核记为 **245** 条（`.agents/notes/20261006-unconfirmed-frozen-baseline-delivery.md` 遗留 14，亦为任务项 4 引用的依据）；2026-10-12 复读为 **207**。两者都远小于新窗口 500，差值属活库/时点差异。

### 复现命令（只读，逐条可复算）

```bash
sqlite3 -readonly -list /root/.herdsman/state.db \
 "select json_extract(payload_json,'\$.degradedReason'), count(*) from agent_events
  where json_extract(payload_json,'\$.degraded')=1
  group by 2 order by 3 desc;"
sqlite3 -readonly -list /root/.herdsman/state.db \
 "select count(*) from agent_events where json_extract(payload_json,'\$.degraded')=1
    and created_at > (strftime('%s','now')*1000 - 86400000);"
```

## 被放弃的方案（必填）

- **照抄任务示例 SQL（`SELECT reason ... GROUP BY reason`）**：放弃——`agent_events` 无 `reason` 列，会 `no such column: reason`；改用 `payload_json.degradedReason`。
- **用 `invalidated_reason` 列统计 reason**：放弃——该列本次全为 NULL，不能承载 reason 统计。
- **以 `journalctl` 串级计数为主口径**：放弃（本轮）——journal 便于看上下文与「④ 组」签名，但受 retention 影响且不可复算；DB 字段可逐步复算，更适合做基线。本轮**未**查 journal。
- **按秒解释 `created_at` / 按「全历史」表述库内计数**：放弃——前者的窗口等于全库、后者忽略 TTL 清理，两者都会让基线虚高。
- **任何写库、DDL、`VACUUM`/维护操作**：放弃——任务要求只读，且生产库由 daemon 持有。

## 来源

- 任务：`[MARK-WORKER-HSM-003]` 项 6（只读快照 + 落盘新笔记）。
- 数据：`sqlite3 -readonly -list /root/.herdsman/state.db`（`.schema agent_events`、若干 `select`，2026-10-12T14:52–14:57Z）；库路径口径见 `AGENTS.md`「默认数据目录 `~/.herdsman` 即生产」。
- 相邻笔记：`.agents/notes/20261002-m2-disk-body-and-canary-read.md`（canary 口径与遗留 3「守卫扩窗后 `stale_baseline_duplicate` 组可能上移」）、`.agents/notes/20261006-unconfirmed-frozen-baseline-delivery.md`（遗留 14：单 pane 245 条量级与「超窗漏拦」既定代价）、`.agents/notes/20261006-leftover-disposition.md`（批次一遗留清单来源）。
- 局限：本轮未查 journal（`journalctl -u herdsman.service`），故无串级对照；活库计数随时刻变化，引用须连同时刻。

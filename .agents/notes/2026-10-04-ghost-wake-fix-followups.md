---
status: active # active | superseded
superseded_by: ""
superseded: ""
# 模块可选值: observability, daemon, cli, config, db, herdr, shared, herdsman-pi, herdsman-herdr-plugin, release
模块: herdsman-pi
---

# 幽灵唤醒修复的遗留观察项（ghost wake fix follow-ups）

依据：幽灵唤醒修复提交 `2bad14f`（`fix(herdsman-pi): end ghost wake loops by writing off stranded deliveries`，分支 `fix/wake-excerpt-and-redelivery`，7 文件 +333/−117）、双审结论（reviewer PASS + oracle 同意）。本笔记登记双审与上线过程产生的 7 项观察/建议（a–g），全部非阻塞；经用户过目后落盘。

## 一句话结论

幽灵唤醒的四项修复（续传预算 5 次用尽后 write-off 并 ack、显式 `pane.closed` 隐含消费已投递事件、wake/上下文正文归一化保留换行缩进、`REDELIVERY_FRESHNESS_MS` 维持 300s）已双审通过并在生产应急部署；遗留 7 项观察均不阻塞，其中 oracle 建议的「第 5 次续传升级」需上线观察 1–2 周后按 warn 计数决定是否立项。

## 背景与范围

- **修复提交 `2bad14f` 的四件事**（细节以提交正文为准，不在此复制）：
  1. 续传预算 `MAX_WAKE_CONTINUATION_ATTEMPTS` = 5 次用尽后 `writeOffStrandedWakeDelivery`：id 退出 `wakeAwaitingConsumption` 并走普通路径 ack，治「永不 ack、daemon 幽灵重投」；
  2. 显式关页（`pane.closed`）隐含消费：已投递行在 `#invalidatePaneCore` 步骤 0 直接 ack，不再走关页保留；从未投递行仍保留一次投递机会；
  3. wake 摘要与 wake/上下文正文归一化保留换行与行内缩进（只折叠 3+ 连续换行、去行尾空白），治「截断假象」；
  4. `REDELIVERY_FRESHNESS_MS` 维持 300s 未动。
- **本笔记的依据**：`docs/releasing.md` 之外的仓内留痕惯例（`.agents/notes/README.md` 触发条件 3/4/6：否决更优方案、临时降级/workaround、性能取值原因），命名沿用目录约定，落盘后运行 `scripts/notes-index.sh` 刷新本地索引。

## 观察项（逐条，非阻塞）

1. **write-off notify 文案已陈旧（reviewer 建议）**
   - 位置：`packages/herdsman-pi/src/index.ts` `writeOffStrandedWakeDelivery` 的 `ctx.ui.notify?.("… hand this workspace to another terminal …")`。
   - 现状：write-off 已同步 ack，daemon 不会再重投该事件，「交给另一个终端让 daemon 重投」的说法失去依据。
   - 后续：下个版本把文案简化为「read the agent directly」一侧的表述即可，不动逻辑。

2. **write-off ack 可能因更小 id pending 行而 ORCHESTRATOR_EVENT_OUT_OF_ORDER 失败（oracle 观察，需观察）**
   - 位置：`packages/herdsman-pi/src/index.ts` `writeOffStrandedWakeDelivery` → `acknowledgeEventIds(stranded, { notify: false }, ctx)` 的 catch 分支（`logHerdsmanPi("warn", … "wake write-off acknowledgement failed" …)`）。
   - 现状：若表中存在 id 更小的 pending 行，ack 会以 `ORCHESTRATOR_EVENT_OUT_OF_ORDER` 失败，退化为「永久静默 churn」——扩展侧已释放（不重投给本会话），daemon 侧仍 pending，每 300s 窗口继续重投，唯一信号是一行 warn。
   - 后续：上线后观察该 warn 是否出现；若出现，需评估 ack 顺序或补按序 ack，另立项。

3. **第 5 次续传可升级为「携带内容注入 transcript 再 write-off」（oracle 建议，暂缓）**
   - 位置：`packages/herdsman-pi/src/index.ts` `driveWakeContinuation` 预算耗尽分支（现为直接 `writeOffStrandedWakeDelivery`）。
   - 现状：当前 write-off 丢弃的是「未被任何 turn 带出的更新」，证据只存在于 DB/agent 侧。
   - 后续：上线观察 1–2 周；若 write-off warn 非零（说明丢弃真实发生），再把第 5 次续传升级为把正文注入 transcript 后再 write-off，确保证据不灭失后再放弃。

4. **daemon 侧隐含消费 ack 完全静默（oracle 观察）**
   - 位置：`src/db/agent-events.ts` `#invalidatePaneCore` 步骤 0（`acknowledgeDelivered` 分支）与 `src/observability/agent-index-service.ts` `pane.closed` 处理。
   - 现状：显式关页的隐含消费 ack 无任何日志，线上无法确认该路径真的在生效。
   - 后续：补一行 `console.warn`（或 daemon 既有日志设施）记录被隐含消费的 eventIds，低优先级。

5. **部署身份错位：已部署字节曾与任何版本号对不上（oracle 观察）**
   - 位置：生产部署面（本修复为应急部署，字节落在 commit 之前）。
   - 现状：已部署字节在 commit 前与任何已发布版本号都对不上，违背「版本号即部署身份」口径。
   - 后续：commit 后尽快走 `docs/releasing.md` 完整发布流程（`@dorokuma/herdsman` 与 `@dorokuma/herdsman-pi` 两包发布 + `v<version>` tag 推远端 + registry 钉版本部署），对齐部署身份。

6. **换行保留后大代码块全量进 wake 文本，token 成本上升（oracle 观察，暂不改）**
   - 位置：`packages/herdsman-pi/src/wake.ts` `normalizeExcerpt`（显式声明「No length cap is applied here」）。
   - 现状：保留换行/缩进后，含大代码块的更新会全量进入 wake 投影文本，token 成本高于折叠为一行的旧行为。
   - 后续：暂不引入长度上限（那会重新制造「截断假象」）；观察实际 token 用量后再议。

7. **体系遗留：dispatch-agent 测试既有 ERROR（双审过程发现，需另立项）**
   - 位置：`test_pi_dry_runs_have_unique_safe_names_and_sessions`（dispatch-agent 测试面）。
   - 现状：planner 默认 agy 未带 `--name`，该测试为既有 ERROR，与本修复无关。
   - 后续：另行立项修复，不并入本幽灵唤醒修复的账。

## 被放弃/推迟的方案（必填）

- **「第 5 次续传携带内容注入 transcript 再 write-off」（观察项 3）**：主动推迟到上线观察 1–2 周、write-off warn 非零后再做；本次先用最简 write-off 止血幽灵重投。
- **「保留 unacked 让 daemon 重投到下个会话」（旧行为）**：正是本次废除的方案——线上被证实会把编排会话刷屏到不可用，write-off + ack 是明示的取舍（丢一条可恢复的更新优于无限空续传）。
- **给 wake 摘要加长度上限**：与「治截断假象」的目标冲突，本次明确不加（观察项 6）。

## 来源

- 修复提交 `2bad14f`（fix(herdsman-pi)，7 文件）；双审结论（reviewer PASS + oracle 同意，用户已授权提交）。
- 观察项 1 来自 reviewer 建议；观察项 2–6 来自 oracle 观察/建议；观察项 7 为双审过程发现的既有失败，需另立项。

---
status: active # active | superseded
superseded_by: ""
superseded: ""
# 模块可选值: observability, daemon, cli, config, db, herdr, shared, herdsman-pi, herdsman-herdr-plugin, release
模块: herdsman-pi
---

# 幽灵唤醒修复的遗留观察项（ghost wake fix follow-ups）

依据：幽灵唤醒修复提交 `2bad14f`（`fix(herdsman-pi): end ghost wake loops by writing off stranded deliveries`，分支 `fix/wake-excerpt-and-redelivery`，7 文件 +333/−117）、双审结论（reviewer PASS + oracle 同意）。本笔记登记双审与上线过程产生的 7 项观察/建议（a–g），全部非阻塞；经用户过目后落盘。其中观察项 5「部署身份错位」已随 0.14.1 完整发布对齐（见该条与 `.agents/notes/20261004-release-0.14.1.md`），其余 6 项仍为未处置观察。

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

> 注（2026-10-17）：观察项 1（write-off notify 文案）、3（第 5 次续传升级正文注入）、4（#invalidatePaneCore 补日志）、7（dispatch-agent 测试 ERROR）经复核判定不做，已清理，原文见 git 历史/备份。

2. **write-off ack 可能因更小 id pending 行而 ORCHESTRATOR_EVENT_OUT_OF_ORDER 失败（oracle 观察，需观察）**
   - 位置：`packages/herdsman-pi/src/index.ts` `writeOffStrandedWakeDelivery` → `acknowledgeEventIds(stranded, { notify: false }, ctx)` 的 catch 分支（`logHerdsmanPi("warn", … "wake write-off acknowledgement failed" …)`）。
   - 现状：若表中存在 id 更小的 pending 行，ack 会以 `ORCHESTRATOR_EVENT_OUT_OF_ORDER` 失败，退化为「永久静默 churn」——扩展侧已释放（不重投给本会话），daemon 侧仍 pending，每 300s 窗口继续重投，唯一信号是一行 warn。
   - 后续：上线后观察该 warn 是否出现；若出现，需评估 ack 顺序或补按序 ack，另立项。


5. **部署身份错位：已部署字节曾与任何版本号对不上（oracle 观察）——已对齐**
   - 位置：生产部署面（本修复为应急部署，字节落在 commit 之前）。
   - 现状：~~已部署字节在 commit 前与任何已发布版本号都对不上，违背「版本号即部署身份」口径。~~
   - 处置：**已对齐（2026-10-04T08:11Z）**。commit `2bad14f` 经分支 `fix/wake-excerpt-and-redelivery` 合入 main（fast-forward `5d6d6b9`）后走 `docs/releasing.md` 完整发布：release commit `05ef5ab`（`chore(release): 0.14.1`），tag `v0.14.1` 已推远端（剥离指向 `05ef5ab`），`@dorokuma/herdsman` 与 `@dorokuma/herdsman-pi` 两包 0.14.1 均已发布且 `latest` 已推进；随后按 registry 单一渠道钉版本部署（`npm install --global @dorokuma/herdsman@0.14.1 @dorokuma/herdsman-pi@0.14.1` + `systemctl restart herdsman.service`），部署后核对（版本 / MainPID 3472026→3510442 / `herdsman daemon status` / `herdsman agent list` / 优雅关停 `exitCode: 0`）全部通过。发布落地事实详见 `.agents/notes/20261004-release-0.14.1.md`。
   - 后续：无遗留；线上版本号（0.14.1）与 registry 发布物、tag、commit 一一对应，「版本号即部署身份」口径恢复。

6. **换行保留后大代码块全量进 wake 文本，token 成本上升（oracle 观察，暂不改）**
   - 位置：`packages/herdsman-pi/src/wake.ts` `normalizeExcerpt`（显式声明「No length cap is applied here」）。
   - 现状：保留换行/缩进后，含大代码块的更新会全量进入 wake 投影文本，token 成本高于折叠为一行的旧行为。
   - 后续：暂不引入长度上限（那会重新制造「截断假象」）；观察实际 token 用量后再议。


## 被放弃/推迟的方案（必填）

- **「第 5 次续传携带内容注入 transcript 再 write-off」（观察项 3）**：主动推迟到上线观察 1–2 周、write-off warn 非零后再做；本次先用最简 write-off 止血幽灵重投。
- **「保留 unacked 让 daemon 重投到下个会话」（旧行为）**：正是本次废除的方案——线上被证实会把编排会话刷屏到不可用，write-off + ack 是明示的取舍（丢一条可恢复的更新优于无限空续传）。
- **给 wake 摘要加长度上限**：与「治截断假象」的目标冲突，本次明确不加（观察项 6）。

## 来源

- 修复提交 `2bad14f`（fix(herdsman-pi)，7 文件）；双审结论（reviewer PASS + oracle 同意，用户已授权提交）。
- 观察项 1 来自 reviewer 建议；观察项 2–6 来自 oracle 观察/建议；观察项 7 为双审过程发现的既有失败，需另立项。

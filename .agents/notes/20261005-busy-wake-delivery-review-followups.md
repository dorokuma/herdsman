---
status: active # active | superseded
superseded_by: ""
supersedes: ""
# 模块可选值: observability, daemon, cli, config, db, herdr, shared, herdsman-pi, herdsman-herdr-plugin, release
模块: herdsman-pi
---

# fix/busy-wake-delivery 审查遗留项（reviewer 建议级发现）+ X-1 补修轮记录

依据：分支 `fix/busy-wake-delivery`（代码基线 `581d183`，相对 `main` 的改动只涉及 `packages/herdsman-pi/src/index.ts` + 两个单元测试文件 + 笔记，已核对）本轮 reviewer 审查判定「可以收口、无 must fix」，另给出 3 条建议级发现。按 `.agents/notes/README.md` 的留痕约定与「建议级发现必须落盘闭环」的仓库惯例，本笔记登记这 3 条，全部非阻塞。**关于 oracle 第二意见的更正（本稿前版记为「未取得、无正文产出」，那是错的、已过时）**：oracle  pane 后续确实出具了第二意见——证伪出一条旁路（X-1，silent upstream-error ack 绕过连续可确认前缀，已证实）、给出 X-2 / X-3 两条观察，X-4 / X-5 找不到反例；覆盖面 8 条，另附未验证项与 3 条反问。主代理据此决策：X-1 必修（已在本轮补修，见下文「X-1 补修轮」），X-2 只改文案、X-3 加有节流的 info 上报，X-6 / X-7 归观察项只落盘。

## 一句话结论

reviewer 判定分支可以收口、无 must fix / should fix，仅余 3 条建议级发现：`pruneAcknowledgedEvents` 的 trio 水位清扫是队列路径遮蔽下的常态不可达双重兜底、OBS-1 边界下写 off 事件离队缺观测锚点、三个 wake 记账集合分散维护可收敛为一张 Map；三条均不阻塞收口，收敛触发器已逐条写入。oracle 第二意见**已取得**（前稿「未取得」的记载有误，在此更正）：X-1 证实静默 upstream-error ack 存在绕过「连续可确认前缀」的旁路（**must fix**，已修），X-2 / X-3 为观察项（已分别按「只改文案」与「加节流 info」落地），X-4 / X-5 未找到反例，覆盖面 8 条，未验证项与 3 条反问如实记录在案。X-6 / X-7 为理论级观察，只落盘不改码。

## 本轮审查结论摘要

- **reviewer**：判定可以收口，无 must fix / should fix；提出 3 条建议级发现，即本笔记遗留项 1–3（下文逐条落盘）。
- **oracle**：第二意见**已出具**（前稿记为「pane 空回传、无正文产出」，有误，特此更正）。结论：证伪出一条旁路并给出两条观察 —— **X-1**（must fix）：静默 upstream-error 确认路径没有 `wakeRetryableEventIds` / `wakeAwaitingConsumption` 屏障判断，而 daemon `markAcked` 是 `where id <= ?` 的水印语义，只要发出一个比被阻塞 id 更大的 ack，那个被阻塞的 id 就会被水印一并吞掉（永不重投、编排者永远看不到正文、无告警）；**X-2**：busy 自旋梯子的「兜底安全网」措辞与 liveness 上界描述需要如实化；**X-3**：屏障/死信导致的长期无进展缺可观测锚点。**X-4 / X-5 找不到反例**。覆盖面 8 条；未验证项与 3 条反问随报告记录，不作为放宽或加严收口标准的依据。

## 背景

- **分支状态**：`fix/busy-wake-delivery`，代码基线 `581d183`（`git diff main HEAD --stat` 核对：仅 `packages/herdsman-pi/src/index.ts`、`test/unit/herdsman-pi-extension.test.ts`、`test/unit/herdsman-pi-wake.test.ts` 与 `.agents/notes/` 下 4 篇笔记）。
- **行号核对口径**：`packages/herdsman-pi/src/index.ts` 自 `4679db8` 起未再变动（`581d183` 之后的提交均为 docs-only，已用 `git diff` 核对为空），故本笔记行号**对 `581d183` 生效**；本轮 X-1 补修段以外的行号未随之刷新（X-1 补修使该文件整体位移，旧行号已不再对当前工作树成立，逐条以「HEAD 核对」为准，详见文末「oracle 收口轮遗留项」H 项）；X-1 补修轮的实际开工基准为 `70963e8`（分支 HEAD，别的会话推进的 docs-only 提交；该文件内容与 `4679db8` 一致，已核对）。
- **为何新建而不是追加既有笔记**：同源三项在上一轮（收尾轮）已登记于 `20261005-busy-wake-review-leftovers.md`（该篇共四条，后经订正修订）；本轮 reviewer 重新提出其中三条，按约定以 `2026-10-04-ghost-wake-fix-followups.md` 为格式范本单独成篇——既让本轮审查结论在 INDEX.md 里有独立可检索的落脚点，也不把两轮审查的输入混为一账。
- **触发条件**：`.agents/notes/README.md` 触发条件 3 / 4 / 6（否决或推迟看似更优方案、留痕 workaround 判定、性能/结构取值原因），叠加「建议级发现必须落盘闭环」的仓库惯例。

## 遗留项（逐条，非阻塞）

### 1. `pruneAcknowledgedEvents` 的 trio 水位清扫是被队列路径遮蔽的双重兜底（reviewer 建议）

- **来源角色**：reviewer（本轮收口评审的建议级发现）。
- **位置**：reviewer 引用 `packages/herdsman-pi/src/index.ts:652-663`。HEAD 核对：`pruneAcknowledgedEvents`（`:623`）尾部的 trio 水位清扫——`wakeRetryableEventIds` 循环 `:662-664` 与 `wakeDeadLetterAttempts` / `wakeDeadLetterRetryAt` 循环 `:668-673`；遮蔽它们的路径是同一函数内的队列循环 `:638-640`，把 ≤ ack 水位的 id 路由进 `dropUnackedDelivered`（定义 `:567-575`，trio 删除 `:572-574`）。该遮蔽关系在源码注释 `:659-661` 里已写明。
- **为什么不阻断**：常态路径下，≤ ack 水位的 id 必在 `unackedDelivered` 里——awaiting 与 dead-lettered 的 id 都留在队列内等 daemon 水位推进（后者是刻意的 ack 水位屏障）——队列循环先把它们全部送进 `dropUnackedDelivered`，同一批字段已在那里删除；`:662-673` 的成组清扫因此在常态下不可达，是防御性对称。它防的是未来出现第二条不经队列的清扫路径时漏清这三个字段，收益在未来。当前保留现状不增加风险，注释已把关系写明，不存在「读者误以为此处是主路径」的问题。
- **后续怎么收敛**：下一次大改 wake 记账的清扫职责时，统一收敛到单一删除入口——只保留 `dropUnackedDelivered` 一条 trio 删除路径、删掉 `pruneAcknowledgedEvents` 内的重复回路（与遗留项 3 的字段收敛天然同批）；若先出现第二条不经队列的清扫路径，则重新评估这对回路是否仍只是纯防御。

### 2. OBS-1 边界下写 off 事件离队缺观测锚点：建议 `clearDeliveryBookkeeping` 补一条 info 日志（reviewer 建议）

- **来源角色**：reviewer（本轮收口评审的建议级发现）。
- **位置**：reviewer 引用 `packages/herdsman-pi/src/index.ts:610-621`。HEAD 核对：`clearDeliveryBookkeeping`（`:598-621`）尾部清空段，`wakeRetryableEventIds` 清空在 `:614`；既有同形状的 carriedOver info 日志在 `:602-606`（`[herdsman-pi] keeping N unconsumed wake event id(s) suppressed across the scope change eventIds=…`）；写 off 入账在 `writeOffStrandedWakeDelivery`（`:1155`，retryable 入列 `:1165`，budget / deadline `:1171-1179`，warn 日志 `:1183`）。
- **边界描述（OBS-1）**：turn abort 未产出 `message_end`（无消费证据）→ settle 把事件写 off → 随后遇 scope 重置 → daemon 重投 → 再注入。写 off 的 id 此时随 `:614` 静默清出 `wakeRetryableEventIds`，日志里只剩写 off 当时的 warn（`:1183`）与未被写 off 的 awaiting id 的 carriedOver info，「哪些写 off id 在重置时离场、之后是否真的被重投」没有任何观测锚点。
- **为什么不阻断**：这是纯观测缺口、零行为争议——不改 OBS-1 本身的结论。复现链依赖 Pi 内部 follow-up 队列行为，扩展侧没有观测点，`createFakePi` 又只记录 `sendMessage` 入参、没有 follow-up 队列模型，能写出的测试只能断言「重置后重投的 id 又被注入了一次」，而这正是当前契约的预期行为（写 off 的语义就是内容没进 transcript、重投是唯一补救），修前修后都过，锁不住任何缺陷。缺口不构成对分支正确性的怀疑，只是可观测性欠一行。
- **后续怎么收敛**：在 `clearDeliveryBookkeeping` 对随 `:614` 清出的写 off id 补一条与 carriedOver 同形状的 info 日志（`eventIds=…`，一次一条、不刷屏），零行为变化、只补锚点。触发时机：需要现场定位「写 off 的 id 到底有没有被重投、是否撞上 scope 重置」时（OBS-1 的复现尝试正需要这个锚点），连同日志形状按 observability 惯例过审一起做。

### 3. 三个 wake 记账集合分散维护，建议收敛为一张状态 Map（reviewer 建议，纯结构整洁度）

- **来源角色**：reviewer（本轮收口评审的建议级发现）。
- **位置**：reviewer 引用 `packages/herdsman-pi/src/index.ts:253-255`。HEAD 核对：三个字段的声明在 `:224`（`wakeRetryableEventIds: Set<number>`）、`:232`（`wakeDeadLetterAttempts: Map<number, number>`）、`:234`（`wakeDeadLetterRetryAt: Map<number, number>`），初始化在 `:455-457`。
- **为什么不阻断**：纯重构项，不改变任何行为，就没有回归收益可为它单独开一轮；trio 的语义（可否再投递 / 已花费预算 / 下次重投时点）在注释里已按成组维护，现状无正确性风险。删除点分布不齐（`dropUnackedDelivered` `:572-574` 删齐 trio、注入后批次清理 `:1465-1466` 只删两项、消费确认清理 `:1998` 只删一键）恰说明收敛有收益，但收益在未来、不在本轮。
- **后续怎么收敛**：下一次触及 trio 任一字段时，顺手收敛为一张状态 Map（`Map<eventId, { attempts: number; retryAt: number }>`，retryable 作键门控），与遗留项 1 的清扫收口、遗留项 2 的日志并入同一轮「wake 记账收敛」，不单独为重构返工。完整的删除点 / 只读点清点（含 `pruneAcknowledgedEvents` 水位循环与 `writeOffStrandedWakeDelivery` warn 日志两处后续补录项）见 `20261005-busy-wake-review-leftovers.md` 遗留项 3；收敛时须注意：消费确认路径现状只删一次键，Map 化后该路径仍只删一次键。

## X-1 补修轮（主代理决策后的落地记录）

本轮不改动上面三条遗留项的处置；只落地 oracle X-1 / X-2 / X-3 与 X-6 / X-7 的落盘。工作基线：HEAD `70963e8`（docs-only），`packages/herdsman-pi/src/index.ts` 内容自 `4679db8` 起零变动（`git diff 4679db8 HEAD -- packages/herdsman-pi/src/index.ts` 为空，已核对）；双审通过后由后续 worker 统一 commit + 合并。**已落库为 X-1 补修提交 `2934393`，并随合并提交 `e63e65b` 合入 main**（原记「未 commit、未切分支」在 HEAD 上已是假陈述，此处改为事实状态）。

### X-1（must fix）：静默 upstream-error 确认必须尊重屏障 —— 已修

- **判据（oracle 定位链，已逐行核对）**：`representableWakeOutcomes` 在「没有非 dead-lettered 带头内容」时只放行 `deadLetterRetryDue` 已到期的 id；退避窗口内或预算耗尽后 `injectableOutcomes` 为空 → 进入 `scheduleWake` 的 `injectableOutcomes.length === 0` 分支 → 若存在 due 的 suppressed 上游错误则调用 `scheduleSilentUpstreamErrorAck`；该路径的筛选（原 `:1246-1253`）只看 `eventId > failedWakeThroughEventId && !presentedEventIds.has(id)`，**没有** `wakeRetryableEventIds` / `wakeAwaitingConsumption` 屏障判断；随后直接 `await acknowledgeEventIds(...)`，而 `acknowledgeEventIds` 不校验连续性——连续性只由 `confirmableDeliveryPrefix` 的 break 保证，静默路径绕过了它。daemon 侧 `markAcked` 是 `update agent_events set status='acked' … where id <= ?`，所以**只要发出一个比被阻塞 id 更大的 ack，那个被阻塞的 id 就会被水印一并吞掉**（此后永不重投、编排者永远看不到正文，且无告警）。
- **修法**：静默确认路径复用与 `confirmableDeliveryPrefix` **同一套屏障**，而不是「加一条日志但照样发 ack」（水位语义下，发出即吞掉，日志毫无意义）。新增 `unconfirmedWatermarkBarrier`（`packages/herdsman-pi/src/index.ts:1165`，定义 `:1165-1190`）：按 id 升序遍历 `unackedDeliveredAscending()`，返回第一个 `wakeRetryableEventIds` 或 `wakeAwaitingConsumption` 成员（= 最小的未确认阻塞 id，附阻塞原因与被阻塞条数）；`scheduleWake` 的静默分支（`:1395-1409`）把 due 的 suppressed 事件过滤成 `id < barrier.eventId` 的 `ackableSuppressed` 后才交给 `scheduleSilentUpstreamErrorAck`，一个都不足以越过屏障时**整个 ack 都不发**（held ids 保持 due，由后续 pass 在屏障 id 被确认/恢复后补确认），并继续走原来的 `nextAttemptAt` 退避计时，队列不会卡死。
- **为什么不存在「某条路径确实必须发 ack」的例外**：被阻塞 id（awaiting 无证据 / dead-letter 未恢复）的内容从未到达 transcript，对它的任何确认都是谎报；屏障之外的 id（已观测消费、已终局 dead-letter 离队）本就不是屏障。若被阻塞 id 本身已失效（daemon 已终局 dead-letter），它早已离开队列、不再是屏障，那时发出 ack 不跨过任何未确认 id——这条路径由 `unconfirmedWatermarkBarrier` 的遍历范围自然保证，无需特例。
- **回归测试（先 FAIL 后 PASS，原始输出见交付报告）**：`test/unit/herdsman-pi-extension.test.ts` 的 `herdsman-pi upstream error wake filter` 下新增两条——「holds a silent upstream-error ack behind a dead-lettered id still inside its retry backoff」（退避窗口内）与「keeps a silent upstream-error ack from swallowing a spent-budget dead-letter id, and notes the hold-back once per state」（预算耗尽 + 节流 note 去重）。断言：静默路径不得发出更大 id 的 ack、被阻塞 id 不得变成 acked（`client.currentEvents` 仍持有、extension `pendingEvents` 仍包含、`wakeRetryableEventIds` 仍标记）。未改代码上两条分别以 `expected [ 92 ] to deeply equal []` / `expected [ 94 ] to deeply equal []` 失败；补屏障后同文件 131 条全过。
- **fake client 水位语义（否则是假绿）**：`createWakeClient` 的 ack 替身原来返回 `{ acknowledged: true }`（不带水位），扩展侧 `pruneAcknowledgedEvents(undefined)` 空转、fake 的 `currentEvents` 也只删被 ack 的那一个 id，「更大 ack 扫过更小被阻塞 id」完全不可观测。已改为返回 daemon 忠实的水位（`{ acknowledged: true, ackedEventId: eventId, state: { ackedEventId: eventId } }`；fake `request()` 钩子据此把 ≤ 水位的 id 全部扫出 `currentEvents`）。该修正**暴露出一条既有的假绿**：`pi batch delivery fixes` 的「acks the rest of a delivered batch after a resync failure on one event」原断言「202 失败后仍 pending、`◆ Herdsman · 1 agent update`」——但 daemon `#ack`（`src/observability/agent-orchestrator-service.ts:201-222`）明确允许在 next candidate 已交付给本 owner 时放行更大 id 的批次 ack（注释：「keeps one stuck delivered event from blocking the whole trailing batch」），`markAcked` 的 `id <= ?` 会把 202 一并扫掉。已按真实语义把该断言更正为 `◆ Herdsman`（202 被批次水印确认、不再重投），测试注释同步说明依据。

### X-2：busy 自旋梯子文案改「轮询降频」（只改注释，不加回硬超时）

- **主代理决策**：不再区分编排者忙/闲来决定投递——一律排队，等 `agent_settled` 或某次 tick 观测到 idle 时再投；主代理 busy 时本来也没法加塞。因此**不加回** `WAKE_DEFERRED_TIMEOUT_MS` 那类「忙等 N 秒就强投」的 deadline；梯子从「兜底安全网（safety net）」改述为「轮询降频（poll-frequency reduction）」，并如实写清 liveness 上界。
- **改动位置**（均为注释/文档，零行为变化）：`packages/herdsman-pi/src/index.ts:298`（`WAKE_BUSY_SPIN_MS` 文档「busy poll ladder」）、`:306`（`WAKE_BUSY_BACKOFF_CAP_MS` 文档「busy poll ladder」）、`:317-334`（`WAKE_BUSY_BACKOFF_MS` 文档整篇重写）、`:236-239`（`wakeBusySpinRung` 字段文档「Consecutive ticks spent waiting」）、`:1301-1322`（`scheduleDeferredWake` 文档整篇重写）、`:1440`（`scheduleWake` 内「deferred on the backed-off poll」）、`:1630`（注入成功后「starts the poll ladder from its first rung」）、`:2201`（`agent_settled` 内「the poll ladder starts from its first rung again」）。
- **最终措辞要点**：liveness 上界 = 「until `agent_settled`, or until one of these ticks observes an idle orchestrator」，never a deadline；梯子存在只为让等待便宜；`scheduleDeferredWake` 文档显式写明「There is deliberately no deadline (nothing like a `WAKE_DEFERRED_TIMEOUT_MS` …)」。

### X-3：屏障/死信阻塞的有节制 info 上报 —— 已加

- **实现位置**：`noteWakeWatermarkHoldBack`（`packages/herdsman-pi/src/index.ts:1211`，定义 `:1211-1243`），节流常量 `WAKE_BARRIER_NOTE_INTERVAL_MS = 60_000`（`:373-381`），节流状态 `lastWakeBarrierNote`（`:484-488`），调用点 `:1403-1409`（静默 ack 因屏障被整体扣住时）。
- **节流策略**：同一状态（`blocked eventId : reason : blocked 条数` 组成 key）在 60s 窗口内只记一条；状态变化立即重记；跨窗后同状态再记一条，长期卡死不会永久沉默。日志沿用既有形状（参考 `writeOffStrandedWakeDelivery` 的 warn 与 `noteSkippedWakeInjection` 的 info）：`[herdsman-pi] wake acknowledgement held eventId=<最小阻塞 id> reason=<dead-letter-budget-exhausted|dead-letter-backoff|awaiting-consumption> blocked=<条数> · the daemon confirms by watermark (id <= ?), so no larger id may be acknowledged while this one is unconfirmed; the held update stays pending in the daemon and recoverable`。
- **最小可行版本的如实说明**：「长期无进展」无法在扩展侧可观测地判定——一个即将释放的屏障与一个永不开锁的屏障在扩展侧形态完全相同。最小可行版本因此定义为：在「因屏障而拒绝一笔本可发出的确认」这个**决策点**上记录（而不是在某个计时器到点时记录），字段为最小阻塞 id + 阻塞原因 + 被阻塞条数。「等 idle」这一路不重复造日志：busy 降级已有 `herdsman-wake` 状态行（`Herdsman · waiting for the current turn to end`，`setHerdsmanUi`）作为既有观测锚点，新日志只覆盖 daemon 水位屏障（死信预算耗尽 / 退避中 / awaiting-consumption）。
- **回归测试**：第二条 X-1 测试同场覆盖——预算耗尽场景下连续 3 次 settlement 重取同一决策，`wake acknowledgement held` 恰好 1 行（`eventId=93 reason=dead-letter-budget-exhausted blocked=1`），证明去重生效、不刷屏。

### X-6 / X-7（理论级观察，只落盘不改码）

- **X-6（理论）**：`clearDeliveryBookkeeping` 只把 awaiting id carry 进 `wakeSuppressedEventIds`，retryable / dead-letter id 不进（`packages/herdsman-pi/src/index.ts:605` 的 carriedOver 只取 `wakeAwaitingConsumption`）。Pi follow-up 队列中陈旧副本之后被排干 + 跨 scope 重置时，同一更新可能被重复呈现一次。只有 reviewer 单方来源、无复现路径（依赖 Pi 内部 follow-up 队列行为，`createFakePi` 无该模型），归观察项；触发条件：OBS-1 现场复现或 Pi 暴露 follow-up 队列查询能力时复核。
- **X-7（理论）**：注入失败处的 `catch`（`:1642-1649` 一带）只清 `deliveredBatch` / `wakeRequested`，无日志、无重新武装 timer。空操作路径在断连/拒绝场景下可观测性为零；同样是纯观测缺口，零行为争议，归观察项；触发条件：需要定位「wake 注入后既无注入记录也无下一轮」的现场时，补一条 warn 并以 `scheduleWake` 重新武装。

## 决策

本笔记登记 3 条 reviewer 建议级发现（全部「当前不做」，触发器已逐条写入）+ X-1 补修轮的落地记录。X-1 必修已修（屏障复用 `confirmableDeliveryPrefix` 的同一判定 + 带 daemon 水位语义的 fake 与两条回归测试）；X-2 只改文案（不加回硬超时、不区分忙闲）；X-3 加有节流的 info（同状态 60s 去重、跨窗重记）；X-6 / X-7 归观察项。三条 reviewer 遗留项与本轮互不排斥：X-3 的日志形状为遗留项 2 提供了同形状范本，但不清偿遗留项 2 本身（它要的是 scope 重置离场锚点，不是水位屏障锚点）。reviewer 引用行号与源码逐行核对如有出入，以各条「HEAD 核对」为准，原引用保留；结论（可收口、无 must fix / should fix）不受行号出入影响。oracle 缺席不作为任何一条提前或推迟处理的理由（且其第二意见实际已取得，前稿记载有误，已更正）。

## 被放弃的方案（必填）

- **X-1 只加一条日志但照样发 ack**：水位语义下发出即吞掉，日志只会记录「我们刚吞了一个 id」，任务说明也明确禁止这种敷衍。改为整个 ack 都不发（held ids 由后续 pass 在屏障解除后补确认）。
- **加回 `WAKE_DEFERRED_TIMEOUT_MS` 那类「忙等 N 秒就强投」的 deadline**：主代理决策不加——不区分忙闲、一律排队等 settled/idle，busy 时本来也无法加塞；强投只会复现「busy delivery never arrives」。X-2 因此只改文案。
- **顺手修 X-6 / X-7**：均为理论级观察、只有单方来源、无复现路径，超出本轮授权范围；落盘观察项与触发条件即可。
- **把 3 条遗留项中的任意一条在本轮顺手做掉**：本轮为 X-1 补修轮，reviewer 无 must fix / should fix；任一落地都会重新打开评审面（日志形状要过 observability 惯例、Map 收敛要动十余个读写点及其测试替身）。
- **因前稿 oracle「空回传」而追加返工或阻塞收口**：oracle 实际已出具第二意见（前稿记载有误，已更正），其 X-1 发现按 must fix 处置正是本轮补修的依据；不给发现加严也不给流程缺口翻旧账。
- **把三条直接改进 `20261005-busy-wake-review-leftovers.md`**：该篇登记的是上一轮（收尾轮）四条发现、且已经过订正修订；本轮审查结论单独成篇，才能各轮各有独立可检索的落脚点（README 的历史笔记不可变原则也不支持把新一轮输入混入旧账）。

## 来源

- 分支 / 提交：`fix/busy-wake-delivery`，reviewer 引用代码基线 `581d183`。截至本 commit、FF 合并回 main 之前：`581d183` → `be6a24b` → `70963e8` → X-1 补修 `2934393` → `6c6c343` 这一串已随合并提交 `e63e65b` 合入 main 并已 push（`git branch --contains` 逐一核对，`main = origin/main = e63e65b`）；**不在 main 的点名排除项**是同分支上位于其后的两个 docs commit `1e8247d` 与 `8ffc70e`（以及本笔记所在提交）。原记「未 push、未合并回 main」在 HEAD 上已是假陈述，此处改为事实状态。`packages/herdsman-pi/src/index.ts` **并非**自 `4679db8` 起未变：`4679db8` → `581d183` 之间该文件确无变动，但其后的 `2934393` 是代码提交，`git diff 581d183 2934393 -- packages/herdsman-pi/src/index.ts` 为 +165/−31（与 `git diff 4679db8 HEAD -- packages/herdsman-pi/src/index.ts` 同值，因中间的 `be6a24b`、`70963e8`、`6c6c343` 与其后的 `1e8247d`、`8ffc70e` 均为 docs-only），均已核对。X-1 补修轮开工基准 = 分支 HEAD `70963e8`（docs-only），该文件彼时与 `4679db8` 内容一致；`2934393` 之后不再有代码提交。
- 评审：reviewer 判定可以收口，无 must fix / should fix，3 条建议级发现见上；oracle 第二意见**已取得**（前稿「未取得、无正文产出」的记载有误，已更正）：X-1 证实旁路（must fix，已修）、X-2 / X-3 观察（已落地）、X-4 / X-5 无反例、覆盖面 8 条、未验证项与 3 条反问记录在案。
- 本轮改动范围（已落库为 X-1 补修提交 `2934393`，并随合并提交 `e63e65b` 合入 main / 已 push；原记「未 commit」在 HEAD 上已是假陈述，此处改为事实状态）：`packages/herdsman-pi/src/index.ts`（X-1 屏障 + X-3 节流 info + X-2 注释）、`test/unit/herdsman-pi-extension.test.ts`（fake client 水位语义 + 两条 X-1/X-3 回归测试 + 一条既有假绿期望更正）、本笔记。客观门槛（mise node 26.7.0 `pnpm check`、生产面 node 22.23.1 `pnpm check`、`pnpm build`、`pnpm package:check`）四条全部退出码 0。
- 相关笔记：`20261005-busy-wake-review-leftovers.md`（上轮收尾轮四项，与本篇前三条同源）、`20261005-busy-backoff-cap-and-retry-sweep.md`（水位清扫对称化的决策记录）、`20261005-dead-letter-retry-budget.md`（dead-letter 记账语义前置）、`20261004-busy-wake-defer-to-settled.md`（busy wake defer 语义前置）、`2026-10-04-ghost-wake-fix-followups.md`（本篇格式范本）。

## oracle 收口轮遗留项（X-1 补修 diff 的第二意见，docs-only 落盘）

本节记录 oracle 对**本轮 X-1 补修 diff**（`fix/busy-wake-delivery`，该 diff 已落库为 `2934393` 并随 `e63e65b` 合入 main / 已 push：`packages/herdsman-pi/src/index.ts` + `test/unit/herdsman-pi-extension.test.ts`）收口评审的第二意见：双审结论文摘要、3 条建议级（B / C / H）、1 条必须记账的观察项（D）、2 条观察项（E / F）、4 条 open decisions。全部**照实转述，不改写语义、不美化**。**行号口径**：本节行号均为本 worker 开工时点的**实际工作树**核对结果（= 分支 HEAD `70963e8` + 已落库为 `2934393` 的 X-1 补修 diff；基准 commit 记 `70963e8`），与 oracle 原引用并列备查。

### 本轮双审结论文摘要（照实）

- **reviewer**：判定可收口、**0 条发现**（无 must fix / should fix）。
- **oracle**：判定**可 commit 并合入 main**——在客户端 ack 出口上找不到绕过新屏障的反例，X-1 在 diff 覆盖范围内闭合；并用不变量论证了屏障对本地队列无遗漏态。附 **3 条建议级（B / C / H）** + **D 记账项** + **4 条反问**。

### B（建议级·文案/字段语义）：`blocked=` 是屏障集合成员数，不是被扣住的 id 数 —— open

- **来源角色**：oracle（本轮收口评审，建议级）。
- **位置**：HEAD 核对（开工工作树）：`packages/herdsman-pi/src/index.ts:1168-1169` 的注释（「How many ids the barrier holds back: awaiting consumption, or still owed a dead-letter recovery (the two sets this walk checks).」）与 `:1170-1173` 的 `blocked` 计算（`new Set([...state.wakeAwaitingConsumption, ...state.wakeRetryableEventIds]).size`，位于 `unconfirmedWatermarkBarrier` 内，定义 `:1165`）；日志文本 `:1228`（`noteWakeWatermarkHoldBack` 的 info 行，含 `blocked=${barrier.blocked}`）。
- **问题**：注释与日志都写「barrier holds back 多少条」，但实际 `blocked=` 取的是屏障集合成员数（awaiting ∪ retryable 的大小），**不是**被扣住的 id 数；长停摆时读数会低估（屏障 91 后面挂着 N 个已消费未 ack 的 id 时，`blocked` 仍是 1）。
- **测试固化点**：HEAD 核对 `test/unit/herdsman-pi-extension.test.ts:4069`（第二条 X-1/X-3 回归，断言 held log 行含 `blocked=1`）——改名或改口径都需同步该断言。
- **oracle 建议（二选一）**：① 字段改名 `blockingIds=`（纯文案、零行为）；② 改为「队列中 ≥ 屏障 id 的条数」（更贴调试、但节流 key 会更易抖动）。
- **状态**：**待定，记为 open item**（本轮不改；见文末 open decisions 反问 1）。

### C（建议级·测试替身忠实度）：fake ack 替身比真实 daemon 宽松 —— open

- **来源角色**：oracle（建议级）。
- **位置**：HEAD 核对（开工工作树）：替身 `test/unit/herdsman-pi-extension.test.ts:4903`（`createWakeClient` 的 ack 钩子同时返回顶层 `ackedEventId` 与 `state.ackedEventId`）；真实 daemon **只**返回 `{ acknowledged: true, state: { ackedEventId } }`（`src/daemon/observability-server.ts:454-462`，`agent.notifications.ack` 分支 `return { acknowledged: true, state: toWireState(state) }`）；扩展读取点 `packages/herdsman-pi/src/index.ts:866`（`ackResponse?.ackedEventId ?? ackResponse?.state?.ackedEventId`）。
- **问题**：替身比真实更宽松（多一个顶层字段）。**此刻不假绿**（扩展 `??` 兜底会读到 `state.ackedEventId`，真实形状同样工作），但宽松替身在未来可能再制造假绿。
- **oracle 建议**：删掉顶层字段，或在替身旁注明真实响应形状。
- **状态**：**待定，记为 open item**（本轮不改）。

### D（观察项·必须记账）：既有冻结面被本轮补修定格

- **来源角色**：oracle（观察项；要求记账——它记录本轮补修的真实代价，不是本 diff 新引入的缺陷类别）。
- **完整触发链（照述）**：某更新 id=500 已 delivered 并注入但 turn 被 abort / Pi 丢消息 → settlement 写 off（`wakeRetryableEventIds={500}`）→ 用户**显式关闭**该 agent 的 pane → daemon `invalidatePane({ acknowledgeDelivered: true })` 把 `delivery_attempts >= 1` 的 pending/delivered 行直接置 `acked` 且**不推进 scope 游标**（HEAD 核对：`src/db/agent-events.ts:291` 起的 `#invalidatePaneCore` ack 分支，置 `status = 'acked'` 的 SQL 在 `:302-309` 一带；调用点 `src/observability/agent-index-service.ts:1494-1514`，`pane.closed` → `invalidatePane({ acknowledgeDelivered: true, … })`）→ 客户端 `syncPendingEventsFromServer` 把 500 从 pending 列表摘掉（HEAD 核对：`packages/herdsman-pi/src/index.ts:1746` 定义、`:1846` 调用）→ 屏障 walk 在 500 上 break、静默路径又拒发 → **永远不会对 500 发 ack**，本会话 ack 水位**永久冻结**，只能靠 scope 重置 / 角色丢失（`clearDeliveryBookkeeping`，HEAD 核对 `:619`）解开。
- **定性（照述）**：不是本 diff 引入的缺陷类别（walk 的 break 是既有设计），但本 diff 移除了它唯一的（错误）释放阀（修前静默路径会 ack 更大 id 从而扫掉这个已被带外 acked 的屏障），因此这是「以静默停滞换掉静默丢失」的真实代价。
- **oracle 建议**：挂 follow-up。触发器 = OBS-1 现场定位或再次出现水位停摆。内容为：「屏障 id 的行不再被 daemon 列出且不在 in-flight 批次内 → 视为已确认并 reap（须沿用 `syncPendingEventsFromServer` 的截断保护），或 daemon 侧在隐含 ack 时同步推进游标」。

### E（观察项·X-3 节流）

- **来源角色**：oracle（观察项）。
- **无界 / 泄露不存在**：单条目 O(1)，只记 id / reason / 条数，不记正文与凭据。
- **节流 key 含 `blocked` 计数**：「稳态 60s 一条」只在状态完全不变时成立；一批事件陆续到达会让计数逐个变化 → 每次变化立即出一条（每 pass 至多 1 行，突发 N 条最多 N 行）。
- **纯冻结且无 pass 时静默**——本笔记「X-3」节已如实说明，不重复。

### F（观察项·屏障规则稳健性）

- **来源角色**：oracle（观察项）。
- **依赖的不变量**：屏障 id 完全由队列遍历得出，其完备性依赖「集合 ⊆ 队列」。
- **风险面**：本笔记遗留项 3 计划把三个集合收敛重写；重构若出现「加了队列条目没入集合」或「清了集合没清队列」，会让屏障**静默失效**（`unconfirmedWatermarkBarrier` `return undefined` → 静默路径放行所有 due suppressed）。
- **oracle 建议**：屏障取 `min(集合最小值, 队列中第一个非 observed 的 id)`（不变量成立时二者等价，不成立时偏保守）。

### H（建议级·笔记行号口径）

- **来源角色**：oracle（建议级）。
- **问题**：「背景」节原写着「本笔记行号对 `581d183` 与当前工作树一致」；本轮 diff（X-1 补修）后，X-1 之前写的行号已不再对当前工作树成立。
- **oracle 实测对照**：`clearDeliveryBookkeeping` 实际 `619-640`（笔记写 `598-621`）、`carriedOver` 实际 `620`（笔记 `605`）、`writeOffStrandedWakeDelivery` 实际 `1259+`（笔记 `1155`）、`dropUnackedDelivered` 实际 `588-596`（笔记 `567-575`）。
- **HEAD 核对（本 worker，开工工作树，基准 = 分支 HEAD `70963e8` + 已落库为 `2934393` 的 X-1 补修）**：`clearDeliveryBookkeeping` 声明 `:619`（函数闭合计至 `:642`；oracle 记上界 `640`）、`carriedOver` `:620`、`writeOffStrandedWakeDelivery` `:1263`、`dropUnackedDelivered` `:588-596`、`pruneAcknowledgedEvents` `:644`（笔记写 `:623`）——与 oracle 量级一致。
- **处置**：原句已限定为「对 `581d183` 生效；本轮 X-1 补修段以外的行号未随之刷新」，本笔记统一以各条「HEAD 核对」为准、原引用保留备查；双审结论不受行号出入影响。

### open decisions（如实记录，不在本轮执行）

1. **反问 1**：`blocked=` 口径 / 改名（本轮 or follow-up）——即 B 项。
2. **反问 2**：D 族是否本轮补 reap（oracle 倾向挂 follow-up：本轮目标是止损，reap 是新行为面、要新回归）。
3. **反问 3**：新日志文案「the held update stays pending in the daemon and recoverable」（`:1228`）在 D 族是假陈述，是否收窄为「held *suppressed* update…」+「blocking id may be released by daemon policy or a scope change」。
4. **反问 4**：`next_attempt_at` 未来值使 daemon `#ack` 守卫失明的盲区是否独立跟踪（当前惰性，无生产写入点）。

## 决策（追加：oracle 收口轮落盘范围）

本轮为 docs-only 落盘 + 提交合并轮：B / C 记 open item（零行为成本，留待 follow-up 一并处置）；D 按 oracle 要求记账并挂 follow-up（触发器 = OBS-1 现场定位或再次水位停摆）；E / F 归观察项（E 已在 X-3 节如实说明；F 作为遗留项 3 重构的前置约束）；H 已按建议限定行号口径。4 条反问全部留档，不作为放宽或加严任何结论的依据。双审结论：reviewer 可收口、0 条发现；oracle 可 commit 并合入 main。

## 被放弃的方案（追加：oracle 收口轮）

- **在本轮顺手落地 B / C / H 任一条**：本轮授权仅为落盘 + commit + 本地合并；B 的改名会动日志字段与测试断言（`:4069`）、C 会改测试替身，均属新代码面，记为 open item 后续再处置。
- **在本轮补 D 族的 reap**：oracle 明确倾向挂 follow-up——本轮目标是止损，reap 是新行为面、要新回归；不等同于「已接受现状」，触发器与候选方案已写入 D 项。

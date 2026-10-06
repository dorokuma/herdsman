---
status: active # active | superseded
superseded_by: ""
supersedes: ""
# 模块可选值: observability, daemon, cli, config, db, herdr, shared, herdsman-pi, herdsman-herdr-plugin, release
模块: herdsman-pi
---

# busy-wake 遗留项低风险闭环（4 条）+ 0.14.3 发布笔记遗留项处置记账

## 一句话结论

0.14.3 已发布并部署后，主代理做了一次全仓遗留项只读盘点；用户把本轮处置范围定为「低风险可立即闭环的 4 条」+「0.14.3 发布笔记的 5 条遗留项」。本笔记逐条记账：4 条已在本分支落地（3 条纯文案/替身收紧 + 1 条新增观测锚点）。**注意**：初版曾把这一切概括为「全部零行为变化」，双审（oracle 第二意见）打回后已**撤回**——其中扩展的 ack 读取点（上一轮报告值 `:876`，本分支现值 `:885`）由「顶层 `ackedEventId` 兜底 + `state`」收窄为**只读 `state`**，属**行为面收窄**（fail-closed），单列于「本轮的行为面改动」一节，其余 3 条仍为零行为变化。5 条发布侧遗留项里 4 条判定为已闭合或本无需动作、1 条（README `--ref` 短窗口）判定为发布流程结构性问题并挂用户决策面；已发布 CHANGELOG 小节按用户拍板**不改写**；busy-wake 评审的其余遗留项与更早的路线图项**全部未纳入本轮**，触发器逐条写入。

## 背景

- **发布基线**：0.14.3 已完成 registry 侧发布与生产部署（release commit `321e3dd`，tag `v0.14.3` 已推远端，GitHub Release 已建，部署后 `MainPID` 846576），事实详见 `.agents/notes/20261006-release-0.14.3.md`。
- **盘点来源**：主代理在 0.14.3 落地后做了一次全仓遗留项只读盘点，汇总面包括 `20261005-busy-wake-delivery-review-followups.md`（reviewer 3 条建议级 + oracle X-1..X-7 + 收口轮 B/C/D/E/F/H + 4 条反问）、`20261005-busy-wake-review-leftovers.md`（上轮收尾轮四条）、`20261006-release-0.14.3.md` 的「遗留项记账」五条，以及关停预算 / 终端事件交付 / M2 / Phase 2 等更早的路线图笔记。
- **用户决策的本轮范围**：只处置「低风险、可立即闭环」的 4 条 + 把 0.14.3 发布笔记的 5 条遗留项逐条给出处置结论。其余一律只记账、不动码。
- **工作分支**：`fix/wake-leftover-cleanup`，从 `main` 开，开工 HEAD = `105e5b9`（= `origin/main`）。本笔记与其记载的 4 条改动在**双审通过之前不 commit、不 push、不合并**，提交与合并由后续任务执行。
- **触发条件**：`.agents/notes/README.md` 触发条件 3 / 4 / 5（否决或推迟看似更优方案、留痕 workaround 判定、与既往规范的有意识分歧），叠加「遗留项必须落盘闭环」的仓库惯例。

## 已决（用户拍板）：已发布 CHANGELOG 小节不改写

`CHANGELOG.md` 的 `## 0.14.3` 小节中三处措辞的订正意见**不落地**，保持与 tag `v0.14.3` 快照逐字一致：

1. `abb357b` / `606edcb` 两条中「编排者回合结束后无人 drain」的因果连接过强（5s 是在 **busy 期间**由 `scheduleDeferredWake` 武装的，并非「回合结束后」才出现；且 settlement 还有 `driveWakeContinuation` 会开一个 `triggerTurn: true` 的 turn）。
2. 「完整延后到 `agent_settled`」略满（`606edcb:1051-1056` 仅在 `ctx.isIdle?.() === false` 时续等；idle 变真时自旋本身也会注入，`agent_settled` 是**主**关闭路径而非唯一）。
3. ack 水位描述省略了 daemon 侧 `markAcked` 的 `herdr_session_name` / `workspace_id` / status 条件（方向对 —— `where id <= ?` 的语义 —— 但不是全局 `id <= ?`）。

**依据**：`docs/releasing.md` 的 tag 不可移动条款（tag 一旦创建即把当时内容封死，改 CHANGELOG 会与 tag 快照分叉）与 0.13.2 段不追溯改写的先例。**订正内容保留在 `.agents/notes/20261006-release-0.14.3.md` 的「遗留项记账」里**，本笔记只做指向，不回填原文。

## 本轮修掉的 4 条（提交 hash 由后续双审后的提交补齐）

> 行号口径：以下行号均为本分支工作树实测（开工 HEAD `105e5b9` + 本轮改动），与遗留项笔记里以 `581d183` / `70963e8` 为基准的旧行号不完全一致。**第四轮收尾**：已按两位复验人给出的实测对照，用 `rg -n` / `sed -n` 在当前工作树上对本文全部行号逐点重测，并校正了个位数的偏移（旧值 → 实测值对照见交付报告；`:876` 保留为「上一轮报告值」，见该处说明）。

### ① `clearDeliveryBookkeeping` 给「被写 off 的 id」补一条同形状 info 日志

- **取证**：`packages/herdsman-pi/src/index.ts` `clearDeliveryBookkeeping`（声明 `:619`）。`carriedOver`（`:620`）只取 `wakeAwaitingConsumption`，随之有一条 info（`:622-627`，`eventIds=…` + `·` 尾注）；而写 off id 所在的 `state.wakeRetryableEventIds` 在 `:650` 被**静默** `clear()`（`:649` 是**另一条** `clear()` —— `wakeSkipLogReasons`，别指错），没有任何观测锚点。`wakeRetryableEventIds` 的写入点唯一：`writeOffStrandedWakeDelivery`（`:1284`，retryable 入列 `:1294`）——即「写 off」的定义集合。
- **改动（第三轮定稿；第四轮仅补一处注释的条件句对齐，见下）**：新增 `:628-641` 一带——在清空前把 `wakeRetryableEventIds` 取成升序数组 `writtenOff`（`:636`），非空时打一条与 carriedOver 同形状的 info（调用 `:638-641`）。**尾句经两轮订正**：
  - 前缀动作词用 `on a delivery reset`（覆盖**全部**调用点：scope change、`loseRole` 的失去 owner / 角色被挪走 / 无 launch identity、`session_shutdown`；只有重连走 `preservePresented: true` 不经过这里），**不再**写成只有 scope change 才发生的 `across the scope change`。
  - 尾句**第二轮**改为如实表述（原尾句「their dead-letter recovery is traded away with the delivery queue」**说反了**：`clearDeliveryBookkeeping` 只把 `wakeAwaitingConsumption` 成员放进抑制集（`wakeSuppressedEventIds`），`wakeRetryableEventIds` 只是被清空、**不进抑制集**（两族互斥：写 off 时删 awaiting、反之亦然），所以 reset 之后 daemon 若重投这些 id 仍可再次注入——被丢掉的只是本地重试记账/预算（`wakeDeadLetterAttempts` / `wakeDeadLetterRetryAt` / 写 off 集合本身）。「恢复被换掉」是上一行 carriedOver 的真实语义，挪到写 off 族是错的）。
  - 尾句**第三轮再修（复审打回「应修」）**：第二轮的尾句 `· updates retained unacked in daemon: only the local dead-letter retry bookkeeping is dropped here, the daemon may still redeliver them` 里，`updates retained unacked in daemon` 是**无条件**断言，但对 **D 族子集为假**——写 off 的 id 在「显式关页 `invalidatePane({ acknowledgeDelivered: true })`」之后会留在 `wakeRetryableEventIds` 里（daemon 侧 SQL 把它置 `acked`、不推进游标，扩展看不到水位，`pruneAcknowledgedEvents` 清不掉它）；此后任何一次 delivery reset 都会打出这行，而那时该 id **已经 acked、不会再被 daemon 列出**（正是 `20261005-busy-wake-delivery-review-followups.md` 里仍标 open 的 D 族）。**已删掉对 daemon 现状的无条件断言**，只写本函数确知的事，定稿为：
    `[herdsman-pi] dropping <n> written-off wake event id(s) on a delivery reset eventIds=<ids,…> · only the local dead-letter retry bookkeeping is dropped here; these ids do not join the suppression set, so if the daemon redelivers them the update may be injected again`
    保持与 `carriedOver` 那条 info 同形状（同 logger、同字段风格、`eventIds=…` 收束结构部分），不新增任何宣称「recoverable / 唯一出口 / daemon 仍握着它」的句子。
  - 同函数内 carriedOver 那行的 `across the scope change` 属**同一型不实**（角色丢失/关机也会走到这里），一并改为 `on a delivery reset`；尾句「daemon redelivery for them is traded away」对 carriedOver 族为真（它们确实进抑制集），保留。
  - 另在 `writtenOff` 上方补注释（`:628-635`），明确「写 off 族**不**进抑制集、daemon 仍可重投、reset 丢的是本地重试记账」，防止下次又被误读。**第三轮按复审意见收窄一处过绝对的说法**：原注释写「the suppression set only ever receives `wakeAwaitingConsumption` members」，是**全局断言**；事实是抑制集（`wakeSuppressedEventIds`）的 `add` 点**不止**本函数——消费处理器也会把刚离开 awaiting 的 id 放进抑制集。已改为「本函数只从 awaiting 集合拷入」（this function only copies ids from the awaiting set (`wakeAwaitingConsumption`) into the suppression set …），核心澄清（写 off 族不进抑制集、丢的是本地重试记账、daemon 仍可能重投）保留。
  - **第四轮：注释末句与日志尾句做条件句对齐**。注释末句原为「What the reset costs is the local retry bookkeeping for it, **not the daemon-side remedy**」——对 D 族 id 而言 daemon 侧的补救在 reset **之前**就已经没了（显式关页 `invalidatePane({ acknowledgeDelivered: true })` 已把它径置 `acked`、不推进游标），所以这句比同一条 info 的尾句**更绝对**，属同一型不实表述。已改为与日志尾句同样的条件句语气，定稿为：
    `What the reset costs is the local retry bookkeeping for it; this function does not assert whether the daemon still holds the update.`
    即只陈述本函数确知的事（本地重试记账被丢掉、这些 id 不进抑制集、**若** daemon 重投它仍可能被再注入），**不断言** daemon 现在是否还握着该更新，也不新增任何其它断言（「本函数只从 awaiting 集合拷入」等第三轮已收窄的表述原样保留，仅重排行数）。**注释整体仍是 8 行（`:628-635`），未改变任何行号**，所以上表全部行号在改后仍成立。日志尾句本身第三轮已定稿为条件句，本轮**未动**。
- **行为**：**零行为变化**（四轮均为纯文案：不改任何分支条件、不改集合语义、不新增/删除任何状态变更、不 ack）。只新增一次只读遍历 + 一条 info。清空仍发生在 `:650`。
- **为什么不用 `unackedDelivered` 记**：`unackedDelivered` 同时含「已注入未观测消费」（其实是 carriedOver 一族）与「写 off」两族，用它记会把 awaiting-consumption 的 id 重复记一遍；`wakeRetryableEventIds` 恰好就是写 off 集合。

### ② `blocked=` 字段正名（**已撤回**）——语义澄清只留在注释里

- **取证**：`packages/herdsman-pi/src/index.ts:1249`（`noteWakeWatermarkHoldBack` 的 info 行）原为 `blocked=${barrier.blocked}`。该值实为 `wakeAwaitingConsumption ∪ wakeRetryableEventIds` 的**集合成员数**（`unconfirmedWatermarkBarrier` 内），不是「被扣住的 id 数」——日志与注释原文都写成「holds back 多少条」，长停摆时会低估（屏障 91 后面挂着 N 个已消费未 ack 的 id 时读数仍是 1）。
- **初版改动（已撤回）**：把日志字段名 `blocked=` 改成 `blockingIds=`，并同步改既有断言 `test/unit/herdsman-pi-extension.test.ts:4069` 的 `blocked=1` → `blockingIds=1`。
- **撤回理由（oracle 判定）**：该值是两集合的**成员数**而不是 id 列表，`blockingIds=1` 紧挨 `eventId=…` 打印会被读成「阻塞的 id 是 1」——**比原名更易读错**；且属日志消费面的**破坏性改名**。`.agents/notes/20261005-busy-wake-delivery-review-followups.md` 的 B 项仍把契约记为 `blocked=<条数>`（含节流 key `blocked eventId : reason : blocked 条数` 与固化断言描述），改名会让契约描述与实现分叉。
- **现状（撤回后）**：日志字段名**恢复为 `blocked=`**，测试断言恢复为 `blocked=1`；两处描述同一取值的**注释纠偏保留**（「这是屏障集合成员数、不是水位被扣住的后续 id 数」这类澄清是净收益）；内部标识符 `blocked`（`unconfirmedWatermarkBarrier` 返回对象属性 / `noteWakeWatermarkHoldBack` 入参属性 / 节流 key）未改名，取值与节流 key 也未变。上一轮加的任何形状/字段不动。
- **仍 open**：`blocked=<条数>` 的**语义歧义保持 open**——真修是改口径（打印「队列中 ≥ 屏障 id 的条数」或拆成两个字段），那是行为面（动节流 key 的抖动特性、改所有读取点），属 `20261005-busy-wake-delivery-review-followups.md` 建议② 的范围，本轮不做。该笔记里的旧契约描述**仍然有效**，无需改写。
- **行为**：**零行为**（撤回后回到纯文案状态，只有注释措辞相对 `main` 有净变化）。`blockedByMissingTurn`（`confirmableDeliveryPrefix` 的返回值）是同名不同义的另一回事（「是否有 id 因缺 turn 被扣住」的布尔），不打印、不在本项范围内，未动。

### ③ 收紧测试里的 fake ack 替身，使其与真实 daemon 同形

- **取证（真实契约）**：`src/daemon/observability-server.ts:461` 的 `agent.notifications.ack` 分支 `return { acknowledged: true, state: toWireState(state) }`（`:788-790` 的 `toWireState` 是全量 state + ISO `updatedAt`），**无顶层字段**。扩展读取点 `packages/herdsman-pi/src/index.ts:885` 一带（`:878-880` 收响应）：`pruneAcknowledgedEvents(ackResponse?.state?.ackedEventId)`（只读 `state`；该读取点的收窄过程见「本轮的行为面改动」）。
- **取证（替身）**：`test/unit/herdsman-pi-extension.test.ts` 有 7 处 ack 替身比真实 daemon 宽或异形——`:1929`（只有顶层 `ackedEventId`、无 `state`）、`:2170` / `2250`（同时给顶层与 `state`）、`:2340` / `:2484`（同上，`k1.id`）、`:3777`（同上）、`:4905`（`createWakeClient` 的水位钩子，同上）。
- **改动**：7 处统一收紧为 `{ acknowledged: true, state: { ackedEventId: … } }`，一律去掉顶层 `ackedEventId`；`createWakeClient` 钩子（`:4895-4905`）补三行注释说明真实 daemon 只回 `state`，防止替身再次放宽。
- **结果**：**收紧后没有任何用例失败**（全量 `pnpm test` 52 文件 / 841 用例通过；`herdsman-pi-extension.test.ts` 单文件 131 通过）。没有出现「靠加回字段掩盖失败」的情况，也没有需要改用例去迁就不存在字段的地方——原因（第三轮按复审意见订正）是**扩展的读取点与 7 处替身都只看 `state.ackedEventId`**；旧的顶层兜底分支**已删除**（见「本轮的行为面改动」），不再存在「两条分支都能读到同一个水位值、两侧等价」这一说。
- **顶层兜底分支（已删，不再是「刻意未动」）**：初版曾记录「扩展 ack 读取点（工作树现值 `:885`）的顶层 `ackedEventId` 兜底分支已不可达、属实现侧清理、超出授权面，本轮保留」。双审判定该前半分支对真实 daemon 既无测试覆盖也无生产发送方，且 `pruneAcknowledgedEvents(undefined)` 本身是空操作——**删掉是 fail-closed**，于是本轮把它删了。详见「本轮的行为面改动」。
- **为什么值得收紧**：宽替身让 `?? ` 的宽松分支被持续覆盖，真实形状下无人验证的字段将来可能被误依赖——正是 oracle 收口轮 C 项记的假绿风险面。

### ④ 屏障日志尾部措辞：**删掉不实从句**（初版「条件化」写法已撤回）

- **取证**：同一条日志 `packages/herdsman-pi/src/index.ts:1249` 尾部原为「the held update stays pending in the daemon and recoverable」。对被「写 off → 显式关页 `invalidatePane({ acknowledgeDelivered: true })`」的族（即 oracle 收口轮 D 项冻结面），该 id 已被 daemon 径置 `acked` 且不推进 scope 游标，**再也不会被 daemon 列出**，任何 later wake 都拿不到它——`recoverable` 是假陈述，屏障可能永久滞留。
- **初版改动（已撤回）**：尾部改成「…recoverable by a later wake unless an explicit pane close has already acknowledged it, in which case only a scope change can release this barrier」——即把两族条件写进同一行。
- **撤回理由（oracle 判定）**：那句里 `stays pending`（对「写 off + 显式关页 ack」族为假）、`only a scope change can release this barrier`（**角色丢失 / 关机同样能放掉本地屏障**——`clearDeliveryBookkeeping` 的调用面不止 scope change）都不成立，且 `it` 指代含糊。用一句**更具体的假话**替换原句比原句更糟。
- **现状（撤回后）**：该行尾部从句**整段删掉**，只保留为真的水位语义部分：
  `[herdsman-pi] wake acknowledgement held eventId=<id> reason=<…> blocked=<条数> · the daemon confirms by watermark (id <= ?), so no larger id may be acknowledged while this one is unconfirmed`
  不新增任何宣称恢复路径 / 唯一出口的句子，**不传参分族**（那是行为面）。
- **断言**：该行日志的既有断言只覆盖 `eventId=` / `reason=` / 计数字段（`test/unit/herdsman-pi-extension.test.ts:4066-4069`），未断言尾部措辞；本轮除 ② 撤回改名（`blockingIds=1` → `blocked=1`）外**无需再改断言**。
- **行为**：**纯文案、零行为**。
- **仍 open**：**D 族 reap（写 off + 显式关页 → 会话 ack 水位永久冻结）仍 open**，触发口径不变（OBS-1 现场定位，或再次出现水位停摆；候选方案 reap / daemon 侧隐含 ack 时同步推进游标已记在 D 项，本轮不实施）。`20261005-busy-wake-delivery-review-followups.md` 的**反问 3 不再是 open 项**——反问 3 问的是「`recoverable` 那句文案要不要收窄」，本 diff 已按**删除不实从句**把该从句整段删掉（见本条「现状」），那句的处置已完毕；**仍然 open 的是 D 族 reap 本身**，与反问 3 已是两件事。

## 本轮的行为面改动

**唯一一处行为面收窄**（初版「全部零行为变化」的结论因此撤回）：

- **位置**：`packages/herdsman-pi/src/index.ts` ack 成功分支（`agent.notifications.ack` 响应处理，上一轮报告记为 `:876`；本分支工作树现值 `:878-885`，`pruneAcknowledgedEvents` 调用在 `:885`）。
- **改前**：`pruneAcknowledgedEvents(ackResponse?.ackedEventId ?? ackResponse?.state?.ackedEventId)`，响应类型同时声明顶层 `ackedEventId?: number` 与 `state?: { ackedEventId?: number }`。
- **改后**：`pruneAcknowledgedEvents(ackResponse?.state?.ackedEventId)`，响应类型收窄为 `{ state?: { ackedEventId?: number } }`，并加注释说明「ack 契约没有顶层字段（见 `src/daemon/observability-server.ts`）」。
- **为什么是 fail-closed**：真实 ack 契约是 `src/daemon/observability-server.ts:461` 的 `{ acknowledged: true, state: toWireState(state) }`，**无顶层字段**；替身收紧后前半分支既无测试覆盖也无生产发送方。`pruneAcknowledgedEvents(undefined)` 本身是空操作（首行 `if (ackedEventId === undefined) return;`），所以去掉前半只会让「拿不到水位时不扫」，不会让扫出错的水位。
- **影响面**：只读 `state`；对真实 daemon 与对全部替身**无生产差异**（替身全部已收紧为只回 `state`，全量 `pnpm test` 无一处失败）。
- **回滚口（第三轮按复审订正）**：恢复需还原**整段旧表达式** `ackResponse?.ackedEventId ?? ackResponse?.state?.ackedEventId`——**按字面只把 `?? ackResponse?.state?.ackedEventId`「加回」不会恢复旧行为**（旧式是**顶层优先**：先读顶层 `ackedEventId`，读不到才落 `state`）；还需一并还原旧的响应内联类型 `{ ackedEventId?: number; state?: { ackedEventId?: number } }`（现类型已收窄为 `{ state?: { ackedEventId?: number } }`，顶层字段不加回连编译都过不了）。纯文案改动（两条 info 的前缀/尾句、`writtenOff` 上方注释）无行为面，无需回滚；其余改动无需回滚。
- **运行时加载事实（第三轮补记）**：这份未提交 diff 已被 **pi 桥加载**——pi 通过 `/root/.pi/agent/extensions/herdsman-pi.ts`（绝对路径导出 `packages/herdsman-pi/src/index.ts`）加载仓库源码，**保存之后新开的 pane 已经在跑本 diff**（含 fail-closed 只读 `state` 的读法）；**已开的旧 pane 仍是内存里的旧代码，要重开才换**；**不需要**重启 herdsman 服务，也**不需要**改 npm 版本。
- **全绿≠钉住 fail-closed（复审接受的反问，第三轮补记）**：**全绿并不等于钉住了 fail-closed 只读 `state`**——当前**没有**负向用例（把不可达的顶层兜底原样加回去，现有 131 个用例仍会绿），本轮按仓库测试粒度铁律**不加用例**；读全绿时**不得**据此认为该收窄被测试锁住。
- **刻意未动**：`get` / `register` 那两处 state 优先读取点未动，其它协议面未动（这两处的同型顶层兜底见「观察项」）。

## 观察项

- **屏障日志 JSDoc 与内联注释仍不一致（第三轮观察）**：`packages/herdsman-pi/src/index.ts:1166`（`unconfirmedWatermarkBarrier` 的 JSDoc）仍写 `how many ids it holds back`，与 `:1221` 内联注释改成后的「how many ids form the barrier」口径不一致；该 JSDoc 不进日志，本轮不改。
- **`get` / `register` 两处同型不可达的顶层兜底（第三轮观察）**：`packages/herdsman-pi/src/index.ts:984`（`get` 分支）与 `:1868`（`register` 分支）仍读 `response?.ackedEventId` / `response.ackedEventId` 作兜底，但真实 `#connectionState` 无顶层字段——属 `main` 既有、本轮刻意未动（与 ack 读取点同型；真要清是行为面，触发条件同收窄项）。
- **「不可达」只能有条件地写（第五轮订正）**：`test/unit/herdsman-pi-extension.test.ts` 的 `connectionResponse`（`:5160`）顶层 `ackedEventId` 只在调用方传入时出现、且与 `state.ackedEventId`（`:5171` 恒为 `options.ackedEventId ?? 0`）**同值**，而扩展侧读 `state.ackedEventId ?? 顶层`、`??` 在左值为数字（含 `0`）时**不求值右支**，所以现有响应（含喂 `ackedEventId: 651` 的那处、`createWakeClient` 的那两处）的顶层兜底右支**仍不会被求值**——真正的风险是将来若改成「顶层优先」，替身会与真实 daemon **分叉**而测试仍绿，**不是**「兜底现已可达」；单测侧因该 helper 仍带顶层字段、覆盖口径与真实 daemon 不同（真实 `#connectionState` 无顶层字段），对真实 daemon 的「不可达」仍只宜有条件地写。
- **ack 替身的 `state` 只复制 `{ ackedEventId }`，不是全量 `toWireState` 形状**：真实 `toWireState`（`src/daemon/observability-server.ts:788-790`）是全量 state + ISO `updatedAt`，而 7 处替身一律只给 `{ ackedEventId: … }`。扩展当前只读该字段，**无风险**；未来若读 `state` 的其它字段，需先把替身补全，否则会出现「替身没这个字段、真实 daemon 有」的假绿（方向与最初的宽替身相反）。**不要为此改测试**（本轮无此需求）。
- **`blocked=` 的语义歧义仍 open**：日志字段名恢复 `blocked=` 后，「成员数」与「被扣住的后续 id 数」仍可能被读混；真修是改口径（行为面），属 followups 建议②，触发条件写在遗留面。
- **假客户端 `request()` 的水位读取顺序与扩展相反（第五轮观察）**：`test/unit/herdsman-pi-extension.test.ts` 假客户端 `request()`（`:5010-5011`）先读**顶层** `ackedEventId` 再落 `state`，与扩展的 `state` 优先相反；当前两者同值所以测不出分叉；ack 替身收紧后这条只影响测试替身自身的水位记账，**扩展不读它**。

## 已闭合 / 本轮无需动作（0.14.3 发布笔记的 5 条，逐条结论）

1. **CHANGELOG 三处措辞**：见上「已决（用户拍板）」，不改写、订正留在发布笔记记账里。
2. **`README --ref v0.14.3` 短窗口**：**已闭合**——远端 tag `v0.14.3` 与 GitHub Release 均已就位，README 的 ref 现在可解析。
3. **daemon 部署只换版本身份**：**已闭合/本无需动作**——`git diff --stat v0.14.2 HEAD -- src` 为空（`v0.14.2..HEAD` 内 `src/**` 相对 `v0.14.2` 零改动），装的 0.14.3 daemon 与 0.14.2 在 `src/**` 上同源。
4. **本机 pi 的修复加载路径**：**已闭合/本无需动作**——pi 通过 `/root/.pi/agent/extensions/herdsman-pi.ts` 桥以绝对路径加载仓库源码，修复**只有新开 pi pane** 才会加载；**不要**把 `@dorokuma/herdsman-pi` 加进 pi 的 `packages`（那会与桥构成双实例）。全局 npm 前缀里的孤儿副本已随发布钉版本升到 0.14.3，但那与桥加载无关、也不构成双实例。
5. **流程性观察（README `--ref` 短窗口）**：见下节，单独记账。

## 流程性观察：`main` 推送之后、tag/Release 落地之前 README 的 `--ref <新 tag>` 短暂不可解析

0.14.1 / 0.14.2 / 0.14.3 **第三次同型**：Herdr 用户按 README 以 GitHub ref 安装会短暂拿到一个还不存在的 ref。**结构性来源在 `docs/releasing.md`**：README 的安装命令必须指向**将要创建**的 tag（`git tag -a` 被规定在 release commit push 到 `main` **之后**执行），于是「README 已指向新 tag」与「tag 已存在」之间必然存在一个窗口。要消除需改发布流程本身（例如 README 指向分支/移动别名、或把 tag 创建前移并配套改动快照校验），**属用户决策面，本轮不动**；本轮仅在 tag / Release 落地后确认窗口闭合。

## 未纳入本轮的遗留面（各注触发条件）

以下全部**只记账、不动码**，统一处理时机 =「下次触及相关模块或大改时同批处理」。

- **busy-wake 评审其余项**（源自 `20261005-busy-wake-delivery-review-followups.md`）：
  - `pruneAcknowledgedEvents` 的 trio 水位清扫职责收敛（遗留项 1）：常态下被队列路径遮蔽的防御性兜底，收敛到 `dropUnackedDelivered` 单一 trio 删除入口。触发：下次大改 wake 记账清扫职责，或出现第二条不经队列的清扫路径。
  - 三个 dead-letter 记账字段（`wakeRetryableEventIds` / `wakeDeadLetterAttempts` / `wakeDeadLetterRetryAt`）收敛为一张状态 Map（遗留项 3）：纯结构整洁度。触发：下次触及 trio 任一字段时顺手做，且必须与上一条同批。
  - 既有冻结面的 reap（oracle 收口轮 D）：关页把 id 置 ack 且不推进游标 → 会话 ack 水位永久冻结，只能靠 scope 重置 / 角色丢失解开。触发：OBS-1 现场定位，或再次出现水位停摆。候选方案（reap 或 daemon 侧在隐含 ack 时同步推进游标）已记在 D 项，本轮不实施。
  - 屏障完备性前置约束（oracle 收口轮 F）：屏障依赖「集合 ⊆ 队列」不变量，Map 化重构若破坏它会让屏障静默失效。触发：与遗留项 3 的 Map 收敛同批，作为前置约束处理。
  - `clearDeliveryBookkeeping` 只 carry `wakeAwaitingConsumption`（oracle X-6，理论级、单方来源、无复现路径）：Pi follow-up 队列中陈旧副本被跨 scope 排干时可能重复呈现一次。触发：OBS-1 现场复现，或 Pi 暴露 follow-up 队列查询能力。
  - 注入失败 `catch` 静默（oracle X-7，理论级）：只清 `deliveredBatch` / `wakeRequested`，无日志、不重新武装 timer。触发：需要定位「wake 注入后既无注入记录也无下一轮」的现场。
- **更早的路线图项**：关停预算（`20260929-daemon-shutdown-budget.md`）、终端事件交付台账（`20260930-terminal-event-delivery-h1.md` / `-open-items.md` / `-latency-observation.md`）、M2（`20261002-m2-disk-body-and-canary-read.md`）、Phase 2。统一口径：下次触及相关模块或大改时同批处理。
- **流程性缺口（实地观察）**：本仓**没有统一的遗留项销账流程**——遗留项散落在各篇笔记的「遗留项记账」「open decisions」小节里，靠人工盘点汇总；本轮的主代理全仓只读盘点正是这个缺口的体现。是否建立统一台账属流程决策面，本轮不动。

## 被放弃的方案（必填）

- **改已发布 CHANGELOG 的 `## 0.14.3` 小节**：会与 tag `v0.14.3` 快照分叉（tag 不可移动），也违反 0.13.2 段不追溯改写的先例。改为保留原文、订正留在 `.agents/notes/20261006-release-0.14.3.md` 的遗留项记账里。
- **重命名字段（`blocked=` → `blockingIds=`）**：**已撤回**（见 ②）。写 off 族的「恢复被换掉」尾句与 carriedOver 行的 `across the scope change` 亦已撤回/改为如实措辞（见 ①）。屏障日志尾部不再写任何恢复路径/唯一出口（见 ④）。
- **顺手清掉扩展 `packages/herdsman-pi/src/index.ts:885` 顶层 `ackedEventId` 的 `?? ` 兜底分支（上一轮报告值 `:876`）**：初版判为「超出授权面、保留」；双审后改为**已做**——删掉是 fail-closed（旧分支不可达），属本轮唯一行为面改动，详见「本轮的行为面改动」。
- **为 4 条里的任何一条新增测试文件或为文案新写用例**：本轮改动属日志文案 / 断言恢复 / 测试替身收紧，硬边界禁止新增用例；只**恢复**了既有断言 1 处（`test/unit/herdsman-pi-extension.test.ts:4069`，`blockingIds=1` → `blocked=1`）。收紧替身后无任何用例失败，也证明现有覆盖已足够。
- **在本轮处置其余 busy-wake 遗留项 / 路线图项**：全部带行为面或需新回归（reap 是新行为面、Map 收敛要动十余个读写点及其替身），超出用户圈定的「低风险可立即闭环」范围；只按上面逐条写入触发条件。
- **消除 README `--ref` 短窗口（改发布流程）**：属用户决策面；本轮只确认窗口已闭合并记账结构性来源。

## 来源

- 用户指令：先开工作分支 `fix/wake-leftover-cleanup`，在分支上实现 busy-wake 遗留项中 4 条低风险可闭环项 + 新增本笔记；不 commit / 不 push / 不 merge（提交与合并在双审之后由后续任务做）；不改 `CHANGELOG.md`、不改 `src/**`。
- 处置依据的遗留项笔记：`.agents/notes/20261005-busy-wake-delivery-review-followups.md`（reviewer 遗留项 1/2/3、oracle B/C/D/E/F/X-6/X-7 与收口轮 4 条反问）、`.agents/notes/20261005-busy-wake-review-leftovers.md`、`.agents/notes/20261006-release-0.14.3.md`（遗留项记账 5 条）、`.agents/notes/README.md`（留痕规约）、`.agents/notes/_template.md`（front matter 范本）。
- 代码事实：`packages/herdsman-pi/src/index.ts`（`clearDeliveryBookkeeping` `:619`、`unconfirmedWatermarkBarrier` `:1184`、`noteWakeWatermarkHoldBack` `:1232`）、`src/daemon/observability-server.ts:461` + `:788-790`（真实 ack 形状）、`test/unit/herdsman-pi-extension.test.ts`（7 处 ack 替身 + `:4069` 断言）、`docs/releasing.md`（tag 不可移动 / 创建时序）。
- 客观门槛（mise node 26.7.0 / pnpm 11.9.0）：`pnpm check`、`pnpm build`、`pnpm package:check`、生产面 node v22.23.1 复跑 `pnpm check`、`git diff --check` —— 结果见交付报告；本笔记不含尚未发生的 commit hash。

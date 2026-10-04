---
status: active # active | superseded
superseded_by: ""
supersedes: ""
# 模块可选值: observability, daemon, cli, config, db, herdr, shared, herdsman-pi, herdsman-herdr-plugin, release
模块: herdsman-pi
---

# 唤醒投递单轨化：删掉 busy 路径的 context pin（含续传预算按事件独立记账）

## 一句话结论

`packages/herdsman-pi` 的唤醒投递从「双轨」收回「单轨」：busy 时不再经 `pi.on("context")` 把 `herdsman-wake-queued` 副本钉进当前上下文，子代理回传消息只走 follow-up 消息一条轨（busy = 排队 `triggerTurn:false`，idle = `triggerTurn:true`），按 eventId 升序、一条不丢、一条不重。同一批修掉线上复现的续传预算被新事件重置缺陷：驱动预算改为按 event id 独立记账，任一事件的总驱动次数硬上界 ≤5 后必走 write-off。分支 `fix/wake-single-track`，未 commit、未发布。

## 背景

- **改造前（0.14.1）的 busy 双轨**：同一份 wake 内容走两条路——① `context` 钩子钉一份 `herdsman-wake-queued` 副本（turn 内可见）；② 排队 followUp（`triggerTurn:false`）停点排干进 transcript。两条路服务同一份内容，主代理因此看到重复。
- **双轨的由来**：Phase 1 投递延迟修复（`20260930-phase1-delivery-latency.md`）为压低「编排者忙碌」时的可见延迟，在 follow-up 之外补了提前可见的 pin。
- **用户拍板的判断**：主代理忙的时候反正看不见，context pin 的「提前可见」没有实际意义；要最简形态。

## 决策

1. **删 pin**：删除 `HerdsmanState.wakeContext` 字段与其状态/初始化/复位/结算清理，删除 `context` 钩子里的 `herdsman-wake-queued` 注入块，`isNormalHerdsmanContext` 去掉该 customType 分支。`herdsman-wake-context`（隐藏唤醒消息本体）保留，只是不再有第二份副本。
2. **单轨语义写进代码注释**：注入点注释改为明写「一条轨、无第二份副本」，并说明为什么 busy 的提前可见没有意义、以及排干兜底由 `driveWakeContinuation` 承担。
3. **顺序与完整性保持既有实现**：`wake.ts project()` 的 eventId 升序排序不变；多事件在同一份 queued follow-up 里一次性投出（注入顺序 = 排干顺序）；`presentedEventIds` 守卫、续传驱动（≤5 次）+ write-off 熔断、daemon 300s 重投窗口、ack 语义全部保留。
4. **消费判据不变且更简**：消费证据只有隐藏唤醒消息 `message_end` 的 `details.presentedEventIds`（本就不再采信 pin），因此本次不需要改任何确认路径——只是删掉了「pin 不是消费证据」这条需要专门解释的例外。
5. **测试**：`delivers one busy-path update …` 用例从「pin 只作提前可见」改写为「单轨只进 transcript 一次且 context 钩子无副本」；`force-releases a busy wake …` 同样改为断言无 pin；新增 `delivers a busy multi-event batch in ascending eventId order on the single track`（三个乱序事件 → 单条 queued follow-up，内容按 201/202/203 升序，`details` 一致，context 钩子无副本）。

## 附带修复：续传预算被「新事件注入」重置（线上复现，事件 54346）

**缺陷**：`wakeContinuationAttempts` 是单一共享计数器，而 busy 分支每次注入后都把它重置为 0。于是「旧事件仍卡在队列里没被排干 + 新事件不断到来并各自触发一次注入」时，计数器每轮都被新注入清零，卡死事件的驱动次数永远到不了 5，5 次熔断形同虚设——线上表现为同一事件被全量刷屏 9 次、daemon 侧早已 invalidated，是纯扩展本地空转循环。

**修法**：把计数器改成按 event id 记账的 `wakeContinuationDrives: Map<number, number>`：

- 每次 `driveWakeContinuation` 给**每个仍在等待消费的 id** 各记一次驱动（不再共享一个数）；
- 注入 busy 副本时，只给**新** id 置 0，已在等待的 id 保留它已花掉的驱动次数（不重置）；
- 达到 `MAX_WAKE_CONTINUATION_ATTEMPTS` 的 id 走 write-off，**只写耗尽自己预算的那些 id**；仍有剩余驱动的兄弟 id 继续排队且保留自己的计数（既不丢、也不获得新上界）；
- 计数器条目随 id 一起消失：ack、死信、被消费（证据到 transcript）、role/scope 复位四条路径各自清理，不依赖「等待集合为空」这种间接推断。

**为什么不用「不重置」这么小的补丁**：只把注入点那行 `= 0` 删掉，共享计数器仍会把「多个事件各自的花费」混在一个数里——一个事件被 write-off 时，另一个从未驱动过的事件会跟着一起被写掉（丢数据），或者反过来被连带刷新。按 id 记账是让「任一事件 ≤5 次」成为可逐 id 验证的不变式的唯一形式。

**测试**：新增 `keeps a stuck event's drive budget when unrelated new events are injected`——事件 301 卡死不被排干，之后每次 settlement 前都注入一个新事件（302…305，均走 5s 硬超时放行成 queued follow-up），断言第 5 次 settlement 后 301 必被 write-off（ack 一次、通知一次、不再注入），且兄弟 id 之后按自己的预算继续写掉。已用「把注入点的重置逻辑改回旧行为」反向验证：该用例失败（`ackedIds()` 为空 = 熔断从未触发）。

## 被放弃/推迟的方案（必填）

- **保留 pin 作为 busy 时的提前可见**：本次明确否决。理由：主代理忙时看不到，提前可见收益为 0；而它是主代理看到重复的直接来源，且让「为什么 pin 不算第二份副本」「为什么 pin 不是消费证据」这类例外说明成为长期维护负担。若未来能证明 busy 时 orchestration turn 真的会消费提前可见内容，可再议，但需同时解决重复问题。
- **把 pin 改成「busy 时唯一投递路径、parked follow-up 作兜底」**：否决。parked follow-up 何时被排干不可控（要等下一个用户消息），把它当唯一路径会把「一条不丢」交给不可观测的 Pi 内部队列时机。
- **用 context pin 顶替 follow-up 以绕开排队**：否决。pin 不进 transcript，无法作为消费证据，也拿不到 ack；等于把投递可靠性和 daemon 的水印语义一起丢掉。
- **只删掉注入点那一行 `wakeContinuationAttempts = 0` 而不改成按 id 记账**：否决（附带缺陷的修法选择）。共享计数器仍会把多个事件的预算混在一个数里：write-off 会误伤从未驱动过的兄弟 id，或反过来给它刷新上界。「任一事件 ≤5 次」只有按 id 记账才是可逐 id 验证的不变式。
- **把驱动预算上界调大（>5）来吸收新事件注入**：否决。缺陷不是预算绝对值不够，而是预算可被无限刷新；调大只是把刷屏从 9 次变成 90 次，且每次驱动都要起一个空 run。
- **按 delivery/batch 记账而不是按事件记账**：否决。線上刷屏的场景里，卡死事件和新事件常常落在同一个 batch（合并投递），按 batch 记账仍然会让新事件刷新旧事件的预算。

## 来源

- 用户拍板：路径一（去掉 busy 双轨，所有子代理 wake 消息走单轨、按序、一条不丢、一条不重）。
- 主代理追加要求：续传计数器 `wakeContinuationAttempts` 会被「新事件注入」重置为 0，导致同一事件驱动预算永远用不尽、5 次熔断形同虚设（线上事件 54346 已全量刷屏 9 次，daemon 侧早已 invalidated，纯扩展本地循环）；要求驱动预算按事件独立计算或至少不被无关新事件重置，确保任一事件总驱动次数有硬上界（≤5）后必走 write-off，并覆盖「有新事件注入时旧事件预算不被重置」。
- 改造前实现：`packages/herdsman-pi/src/index.ts`（`wakeContext` / `herdsman-wake-queued` / `context` 钩子注入 / `isNormalHerdsmanContext`；`wakeContinuationAttempts` 共享计数器）。
- 双轨设计出处：`.agents/notes/20260930-phase1-delivery-latency.md`（Phase 1 投递段双轨唤醒）。
- 0.14.1 基线：`28115f7`（刚发布 0.14.1），工作分支 `fix/wake-single-track`。

## 收口记录（双审通过后）

双审（reviewer PASS + oracle PASS）通过、用户授权提交后的收口备注，逐条落观察与处置。

### reviewer 观察（3 项）

1. **busy 长 turn 的排干延迟属设计确认，不是缺陷**：queued follow-up（`triggerTurn:false`）要等停点才排干，busy 长 turn 期间更新在 transcript 不可见——这正是删 pin 后接受的单轨语义（主代理忙时看不到提前可见内容，pin 收益为 0）；5s 硬超时放行 + `driveWakeContinuation` 补驱动兜底不变。
2. **Map 记账的四条清理路径闭环**：`wakeContinuationDrives` 条目只随 id 本身消失——ack（`forgetWakeDelivery` / consumed 路径）、死信与 scope/role 复位（clear 路径）、被消费（证据到 transcript）、role/scope 复位——四条路径各自 delete/clear，不依赖「等待集合为空」的间接推断，无残留增长。
3. **反向验证有效**：把注入点重置逻辑改回旧行为后，新增用例失败（`ackedIds()` 为空 = 熔断从未触发），证明用例确实锁死了「新事件不刷新旧事件预算」这一不变式。

### oracle 观察（3 项）

1. **预算在 `sendMessage` 前扣是既有语义**：`wakeContinuationDrives` 的递增发生在 `pi.sendMessage` 调用之前（改造前 `wakeContinuationAttempts += 1` 亦然）；若 sendMessage 抛错，该次驱动仍计入预算——保守方向（把拒绝当作一次花费），与 write-off 的非对称取舍一致。
2. **`?? 0` 是软点，靠注释挡未来编辑**：`(state.wakeContinuationDrives.get(eventId) ?? 0) + 1` 在 id 不在表中时静默从 0 起算；正常路径由 busy 注入时置 0 兜底，但未来若新增入口忘记初始化，会静默获得免费预算。已在状态字段注释里写明「An entry leaves the map with the id itself」与初始化约定，防止未来的编辑误删兜底。
3. **发布需双包齐发 + tag**：本改动落在 `packages/herdsman-pi`，发布时必须 `@dorokuma/herdsman` 与 `@dorokuma/herdsman-pi` 两包同版本齐发、且 `v<version>` tag 推到远端，才算「完整发布」；部署渠道唯一（registry 全局安装、钉版本、重启生效）。

### oracle 反问处置

- **warn 日志作为 write-off 的唯一持久痕迹**：接受。write-off 属低频尾部事件，一行 `warn` 加 sqlite 的 ack 水位已足够线上定位；为它引入持久化表的复杂度不划算。
- **commit 拆两条**：采纳。`fix(herdsman-pi): scope wake continuation budgets per event`（54346 预算修复，Map 记账相关改动）与 `refactor(herdsman-pi): single-track busy wake delivery without context pin`（删 pin + 测试 + note）拆成两条提交，正文分节说明变更边界。

### 收口动作

- reviewer 建议项 1：已恢复 `isNormalHerdsmanContext` 的 `message.customType === "herdsman-wake-queued" ||` 分支，并附注释说明其为历史会话（含旧 pin 条目）重放时的防御性清洗，与 `context` 钩子的去重注释保持一致。
- reviewer 建议项 2：已重跑 `scripts/notes-index.sh` 刷新 `.agents/notes/INDEX.md`。
- 门禁与收尾：nvm v22.23.1 下 `pnpm check` 全绿后提交；未 push、未动版本号；ff-only 合并回 main 后删除 `fix/wake-single-track` 分支。

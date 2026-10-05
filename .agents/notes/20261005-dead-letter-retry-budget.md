---
status: active # active | superseded
superseded_by: ""
supersedes: ""
# 模块可选值: observability, daemon, cli, config, db, herdr, shared, herdsman-pi, herdsman-herdr-plugin, release
模块: herdsman-pi
---

# dead-letter 事件的重投改为「骑新事件 / 有界退避重试」

## 一句话结论

`606edcb` 的 dead-letter 语义补上重投闸门：dead-letter 的 id 不再被同一个 settle 周期无条件重新调度（那是无退避、无上限的开 turn 死循环），只有「骑在实际有内容的新事件上」或「经过显式退避的自动重试」才能重新呈现，自动重试上限 2 次（5s → 15s），耗尽后该 id 只保留 ack watermark 屏障与人工可见告警；同时 busy 自旋从固定 100ms 改成指数退避梯子（100/200/500/1000/2000，上限 5s）。

## 背景

`606edcb` 修复了 busy 投递根因，但留下一个 MUST FIX 级死循环：`writeOffStrandedWakeDelivery` 把 id 放进 `wakeRetryableEventIds`，而 `alreadyPresented` 对集合内 id 直接 `return false`（放行）→ 紧接着 `finishBatch()` 无条件 `scheduleWake()` → 该事件立刻又被算成可唤醒 → 编排者刚 settle（idle）→ 500ms 后再次 `pi.sendMessage(triggerTurn:true)` 强开新 turn → 再失败再 dead-letter → 无限循环。双审第一关 reviewer 判定不放行。

同批两条：`WAKE_BUSY_SPIN_MS` 固定 100ms 的自旋在长 turn 期间每秒空转 10 次（SHOULD FIX）；长 turn 期间挂起的 wake 在 UI 上完全不可见（CONSIDER）。

## 决策

1. **`representableWakeOutcomes` 是唯一的重投闸门**（新增于 `alreadyPresented` 之后）：把可唤醒 outcome 分成「带头的」与「dead-lettered 的」两类。
   - 有非 dead-lettered 的带头内容时（用户新事件、尚未交付过的 id），dead-lettered id 全部随之合并注入——这正是 Phase 1 的 batch merge 不变量，**且不消耗重试预算**（那个 turn 属于新内容）。
   - 没有带头内容时（即只剩被写掉的 id 自己），只有 `deadLetterRetryDue` 为真的才准注入。
   - `alreadyPresented` 仍对 `wakeRetryableEventIds` 放行（它的语义只是「transcript 里没有这份内容的副本」），重投时机完全交给新闸门；`scheduleWake` 的定时器决策与 `startWake` 的批次决策用同一个谓词，`agent.orchestrator.get` 之间也不会漏进新 turn。
2. **自动重试上限 `WAKE_DEAD_LETTER_RETRY_LIMIT = 2`，退避 `[5s, 15s]`**：dead-letter 的成因只有「turn 在 stop point 前结束 / 被 abort / Pi 丢消息」，都没有证据表明立即重投有用；第一次覆盖瞬时抖动、第二次覆盖把第一次也带崩的抖动，第三次只是把重试本身变成本轮要修的循环。最坏情况 ~20s 的自动努力后交给人工（写off 的 notify 已写明「read the agent directly…」）。退避刻意远离 100ms busy tick：写off 的那个 turn 刚刚失败，立刻重投只会重放它。
3. **预算计数独立于 daemon 的 `attempts`**：`wakeDeadLetterAttempts` 是「本客户端的自动重投预算」，daemon 的 `attempts` 记的是 ack 失败，两者不是一回事；`dropUnackedDelivered` / `pruneAcknowledgedEvents` / `clearDeliveryBookkeeping` 同步清理，避免把花掉的预算带进下一个 scope。
4. **耗尽后的终态**：id 仍留在 `unackedDelivered` / `wakeAwaitingConsumption` 的等价位置继续挡住 ack watermark（daemon 按 `where id <= ?` 确认，放大 id 会把它吞掉），`pendingEvents` 里仍是未决，每次写 off 仍发一次「已挂起未决、请人工查看」的 warning；但它不再自己开 turn，只能被真正的新内容带着走。
5. **busy 自旋改退避梯子** `WAKE_BUSY_BACKOFF_MS = [100, 200, 500, 1000, 2000]` + `WAKE_BUSY_BACKOFF_CAP_MS = 5_000`（`WAKE_BUSY_SPIN_MS` 保留为第一级）：正确性不变——真 busy 期间依旧不注入，`agent_settled` 仍是主释放路径；退避只让兜底自旋从「每秒 10 次」降到「首秒至多 2 次、之后约每秒 1 次」。`wakeBusySpinRung` 在 `agent_settled` / `cancelWakeTimer` / 注入成功时归零，新 busy 周期从第一级重新开始。
6. **挂起 wake 的 UI 提示**：新增独立 status key `herdsman-wake`（值 `Herdsman · waiting for the current turn to end`），只在 `wakeDeferredUntilSettled && 有待投递 outcome` 时出现，随释放/排空消失。挂起态单独占一行而不是改 `herdsman` 那条：pending 计数本身仍是计数，混在一行里会破坏既有 footer 断言与可读性。

## 被放弃的方案（必填）

- **在 `alreadyPresented` 里按退避返回 `true`（跳过）**：会把 dead-lettered id 从 `outcomes` 里直接剔掉，而 `nextAttemptAt` 定时器是从 `outcomes` 推导的——退避重试的定时器就没人种了，重试会等到一个无关事件才发生。
- **用「自 dead-letter 以来外部事件序号 +1」判断能否搭车**（初版实现）：更接近「合并进新的外部事件」的字面意思，但漏掉一种常见形态——新事件在写 off *之前* 就到了、只是被在飞批次挡住没交付，此时它并不是「新来的」，dead-lettered id 就搭不上车，Phase 1 的 batch merge 不变量（`merges an unconfirmed batch into the next wake` 用例）被破坏。改为「带头内容」判据后，新事件先到、写 off 后到、以及 id 是否新鲜都被覆盖，且不引入第二个状态源。
- **退避用指数翻倍到 30s 上限**（沿用 `ackBackoffMs` 的形状）：30s 的等待对一个「更新还没送到」的用户侧感知过长，而第 3 次之后的收益已趋近 0；取 5s/15s 后最坏 20s，与人工告警的衔接更快。
- **把等待提示并进 `herdsman` footer 文案**：会让 `◆ Herdsman · 1 agent update` 这类既有断言在「busy 但无 settlement」的用例里失败，且把两个不同性质的信息（有多少待投递 / 正在等 turn 结束）压成一行。

## 来源

- 提交：`fix/busy-wake-delivery` 分支（未 push），追加 commit，不改写 `606edcb`。
- 代码：`packages/herdsman-pi/src/index.ts`（`representableWakeOutcomes` / `deadLetterRetryDue` / `deadLetterRetryAt` / `writeOffStrandedWakeDelivery` / `scheduleWake` / `startWake` / `scheduleDeferredWake` / `setHerdsmanUi`）。
- 测试：`test/unit/herdsman-pi-extension.test.ts` 新增 `describe("herdsman-pi dead-letter retry budget (turn-loop regression)")`（4 例）与 `describe("herdsman-pi busy wake fallback backoff")`（2 例）；4 个既有 Phase 1 / dead-letter 用例按其原意图改写为按新契约断言（不再断言同一周期内自动重投）。
- 复核：把闸门还原成旧的无条件放行 → 4 个新 dead-letter 用例 + 4 个改写用例全部失败；把 busy 自旋还原成固定 100ms → 退避用例失败。恢复实现后全量 `pnpm check` 通过。
- 前置：`606edcb`（busy wake defer to settled）及其笔记 `20261004-busy-wake-defer-to-settled.md`；daemon 侧未动。

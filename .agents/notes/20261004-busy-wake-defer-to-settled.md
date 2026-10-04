---
status: active # active | superseded
superseded_by: ""
supersedes: ""
# 模块可选值: observability, daemon, cli, config, db, herdr, shared, herdsman-pi, herdsman-herdr-plugin, release
模块: herdsman-pi
---

# busy wake 一律 Defer 到 agent_settled 后走标准 idle 注入

## 一句话结论

`packages/herdsman-pi/src/index.ts` 里 busy 编排者的唤醒投递改为「完全 deferred，直到 `agent_settled` 之后按标准 idle 路径注入」，删除 5 秒硬超时强放、`triggerTurn: false` 分支与空 marker 续传；write-off 改为 dead-letter 语义（绝不 ack 未被消费的事件）。

## 背景

线上「busy 投递失效」：编排者自身 turn 在流式输出时，wake 要么被 5 秒硬超时强放成 `triggerTurn: false` 的 queued follow-up（跑过 stop point 就永不排干，更新静默丢失），要么停在 follow-up 队列里等下一个用户消息。`MAX_WAKE_CONTINUATION_ATTEMPTS` 的 5 次预算用尽后 write-off 走普通路径 ack，把「可能从未被看到」的事件谎报成 consumed，daemon 从此不再重投。

`0a6a8a2`（single-track busy wake）与幽灵唤醒循环修复（`2bad14f`）都是在这个根因上打补丁：单轨去掉了第二份副本，但 queued（非触发）投递这条轨本身仍然不可靠。

## 决策

1. **busy 一律完全 Defer**：`scheduleDeferredWake` 只保留 `WAKE_BUSY_SPIN_MS`（100ms）自旋；删除 `WAKE_DEFERRED_TIMEOUT_MS`、`state.wakeForcedRelease`、`endWakeDeferral` 与 `wakeDeferredSince`（本改动后只剩写、没有读，属死代码）。`agent_settled` 会把挂起的自旋 timer 清掉，让注入当拍发生而不是再等一个 tick。
2. **一律 `{ deliverAs: "followUp", triggerTurn: true }`**：只有触发才会把消息交给 agent core 并产生正规 `message_end`。`orchestratorBusy` 局部变量随之删除；`hasSubstantiveWork` 不再因 busy 而强制置真（busy 时根本不会注入，注入的都是我们自己的 wake turn）。
3. **删除 `driveWakeContinuation`**：不再发只含提示语的 `herdsman-wake-continuation` 空 run。若确需续传，由下一次 `scheduleWake` 携带真实更新内容重投。
4. **write-off 改 dead-letter 语义**（`writeOffStrandedWakeDelivery`）：删除 `acknowledgeEventIds` 调用；不再写 `presentedEventIds`（那是「内容已进 transcript」的集合，写它会把事件永久锁死）；id 退出 `wakeAwaitingConsumption` 并进入 `wakeRetryableEventIds`（保留在 `unackedDelivered` / 投递队列里）——既解除注入封锁（下一次唤醒有资格重投），又继续挡住 ack watermark（daemon 按 `where id <= ?` 确认，放大 id 会把它一口吞掉）。warn 日志 + UI 通知语义为「updates retained unacked in daemon」，事件在 daemon 保持未决。
5. **`presentedEventIds` 只在 `pi.on("message_end")` 拿到 herdsman-wake-context 的 `details.presentedEventIds` 之后才写入**：注入成功只进 `wakeAwaitingConsumption`（在飞态），`alreadyPresented` 以 `awaiting` 原因阻止重复注入。

## 被放弃的方案（必填）

- **保留 5 秒硬超时但把强放分支从 queued 改成 triggered**：看似改动最小，但会在用户 turn 中途强行插入我们自己的 turn（打断用户），且 busy 判定只是 `ctx.isIdle()` 一个瞬时快照，强放仍然是拿概率换及时性。
- **保留 `triggerTurn: false` + 空 marker 续传**（旧结构）：marker run 除了「让 agent loop 排空 follow-up 队列」不携带任何信息，run 若在 stop point 前结束就再次空转，正是把会话刷满空 turn 的那条循环；且它依赖 `MAX_WAKE_CONTINUATION_ATTEMPTS` 这个「用几次空转算够了」的魔数，无法证明内容真的到了。
- **write-off 时 ack**（旧行为）：把未被消费的事件标记成 consumed，daemon 停止重投，用户侧永久看不到这条更新——与「绝不谎报 ack」直接冲突。
- **dead-letter 时把 id 从 `unackedDelivered` 也删掉**：会让后续 id 的 ack watermark 越过它，把它一口吞掉；所以保留在队列里，另用 `wakeRetryableEventIds` 单独解除注入封锁。

## 来源

- 提交：`fix/busy-wake-delivery` 分支（未 push）。
- 代码：`packages/herdsman-pi/src/index.ts`（`scheduleDeferredWake` / `scheduleWake` 注入点 / `writeOffStrandedWakeDelivery` / `agent_settled`）。
- 测试：`test/unit/herdsman-pi-extension.test.ts`（4 个 Phase 1 契约用例 + 大量既有用例改为按证据驱动）、`test/unit/herdsman-pi-wake.test.ts`（`Pi wake dead-letter accounting` 新增 2 例）。
- 前置：`0a6a8a2` refactor(herdsman-pi): single-track busy wake delivery without context pin；`2bad14f` 幽灵唤醒循环修复。
- daemon 侧 `src/db/agent-events.ts` 属 Phase 2，本轮未动；事件在 daemon 保持未决也依赖这一点。

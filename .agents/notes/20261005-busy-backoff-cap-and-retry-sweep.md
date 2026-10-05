---
status: active # active | superseded
superseded_by: ""
supersedes: ""
# 模块可选值: observability, daemon, cli, config, db, herdr, herdsman-pi, herdsman-herdr-plugin, release
模块: herdsman-pi
---

# busy 退避梯子的 5s 上限落到实现，以及 dead-letter 记账的水位清扫对称化

## 一句话结论

`WAKE_BUSY_BACKOFF_CAP_MS = 5_000` 此前只是被导出、从未参与延时计算，梯子 `[100, 200, 500, 1000, 2000]` 的真实封顶是 2000ms（常量、注释、笔记、报告自称 5 秒）。现在梯子末端就是该常量、并且每次延时都 `Math.min` 到它（沿用同文件 `ackBackoffMs` / `ACK_BACKOFF_CAP_MS` 的形状），真实行为达到 5 秒；`WAKE_BUSY_SPIN_MS = 100` 仍是首级，`agent_settled` 仍是主释放路径。同时 `pruneAcknowledgedEvents` 补上 `wakeRetryableEventIds` 的水位清扫，与 `wakeDeadLetterAttempts` / `wakeDeadLetterRetryAt` 在同一处成组清理。

## 背景

双审（reviewer + oracle）对 `abb357b` 的复核给出两条 MUST FIX 与一条怀疑项：

1. **规格与实现不一致（MUST FIX 一）**：`WAKE_BUSY_BACKOFF_CAP_MS` 导出了但没有读者——`scheduleDeferredWake` 只取 `WAKE_BUSY_BACKOFF_MS[rung]`，梯子末级 2000ms 就是真实上限，与常量名、注释、`20261005-dead-letter-retry-budget.md` 第 5 条以及提交信息里「back off ... up to a 5s ceiling」的说法全部对不上。
2. **状态清理不对称（MUST FIX 二）**：`pruneAcknowledgedEvents` 显式清扫 `wakeDeadLetterAttempts` 与 `wakeDeadLetterRetryAt`，却漏了同一语义的 `wakeRetryableEventIds`；`dropUnackedDelivered` 清七个字段，这里只清六个。
3. **怀疑项 OBS-1（未修）**：骑乘注入后被 abort 时，副本可能落在 Pi 的 follow-up 队列且不进 `wakeSuppressedEventIds`，scope 重置后同一 id 可能被再注入一次。oracle 明确标为怀疑、未找到可复现路径。

## 决策

1. **上限由常量本身承担，并加一道 clamp**：`WAKE_BUSY_BACKOFF_MS` 末级改为 `WAKE_BUSY_BACKOFF_CAP_MS`（前置声明以便引用），`scheduleDeferredWake` 的延时改为 `Math.min(WAKE_BUSY_BACKOFF_MS[rung] ?? WAKE_BUSY_SPIN_MS, WAKE_BUSY_BACKOFF_CAP_MS)`。两道一起：末级引用让「常量＝最高档」在构造上成立；clamp 让某级被改到上限之上时（例如后来想把退避改长）不会悄悄抬高真实上限，而常量、注释、笔记、测试仍写 5s——正是本次漂移的成因。clamp 的形状与同文件 `ackBackoffMs` 一致，不引入新范式。
2. **梯子其余档位与首级不动**：`[100, 200, 500, 1000, 2000, 5000]`，`WAKE_BUSY_SPIN_MS` 仍是导出值兼第一级；`agent_settled` 仍是主释放路径，兜底自旋只在 settlement 不来时起作用；正确性契约（真 busy 期间绝不注入、无 deadline 强放）不变，因此 `abb357b` 的两个既有 busy 用例（逐级退避、挂起提示）与 `606edcb` 的两个 defer 用例都未改动即通过。
3. **`pruneAcknowledgedEvents` 补对称清扫**：在 `wakeSkipLogReasons` 清扫之后、`wakeDeadLetterAttempts` 清扫之前插入 `wakeRetryableEventIds` 的水位清扫，三个字段同一处成组；`dropUnackedDelivered` 内部的清理顺序按要求未动。
4. **OBS-1 判定：不改代码**。理由有两条：
   - **不可复现**：复现链要求「注入 → 未拿到消费证据就 abort → settle 写 off → scope 重置 → daemon 重投 → 再注入 → 陈旧的 follow-up 副本之后才被排干进 transcript」。其中「abort 之后那份 `deliverAs: "followUp"` 的副本是否还在 Pi 的进程级队列里」与「被 abort 的 turn 还会不会发 hidden wake message 的 `message_end`」都是 Pi 内部行为，扩展侧与测试替身都没有观测点（`createFakePi` 只记录 `sendMessage` 的入参，没有 follow-up 队列模型）。能写出来的测试只能断言「重置后重投的 id 又被注入了一次」——而这正是当前契约的**预期行为**（写 off 的语义就是「内容没进 transcript，重投是唯一补救」，`wakeRetryableEventIds` 的注释写明它「on a role/scope reset」离开集合）。那样的测试修前修后都过，锁不住任何缺陷。
   - **不该改**：把写 off 的 id 在重置时并入 `wakeSuppressedEventIds` 并不是小改动。措辞订正（oracle M-1，已核对源码）：`alreadyPresented` 的检查顺序是 retryable → awaiting → presented → suppressed，retryable 在最前，因此只要 id 仍在 `wakeRetryableEventIds` 里，第一行即 return false，suppressed 标记拦不住重投；「并入 suppressed 会杀死写 off id 的 rides 恢复（`representableWakeOutcomes` 的 rides 路径，dead-letter 的主恢复手段）」只有在 id 同时移出 `wakeRetryableEventIds`（scope 重置时即清空）或把 suppressed 检查挪到 retryable 之前时才成立，即影响面限于「写 off 后遇重置再重投」这一路径，而非一概失效——结论（不改）不变。而既有代码对「在飞未消费 id」的跨重置抑制是**显式取舍并打了日志**的（`clearDeliveryBookkeeping`），写 off 的 id 不在其列，这是设计选择而非遗漏。

## 被放弃的方案（必填）

- **只改注释/笔记、把不动的那一级说成 2000ms 上限**：能让五者一致，但把规格改小了——busy 兜底自旋在长 turn 上仍以约每 2s 一次的频率空转，reviewer 指定的方向（让实现真正达到 5 秒上限）被绕开。且导出的常量仍是无读者常量，下一轮审一样会提。
- **只加 clamp、不动梯子**：真实行为能到 5s，但梯子的最高档仍写 2000ms，「梯子经过 5s 档」变成只存在于 clamp 里的隐性事实，读代码/笔记的人仍会对不上；且测试只能靠 clamp 的边界反推，读起来更绕。
- **把 clamp 也去掉，只让梯子末级引用常量**：构造上已一致，但任何一次「把退避改长」的编辑都能让真实上限脱离常量，而常量、注释、笔记、测试不会报错——本次漂移正是这个形状。
- **按 OBS-1 的建议在 loseRole 路径补一次抑制**：见决策第 4 条（该条措辞已订正：suppressed 拦不住仍在 `wakeRetryableEventIds` 里的 id，「杀死写 off id 的 rides 恢复」只在 id 同时移出 retryable 或把 suppressed 检查挪到 retryable 之前时才成立）；放弃理由不变——在不可复现的前提下无法写回归测试；强行写测试只能把一个猜测固化成断言。

## 来源

- 提交：`fix/busy-wake-delivery` 分支追加 commit（未 push，不改写 `606edcb` / `abb357b`）。
- 代码：`packages/herdsman-pi/src/index.ts`（`WAKE_BUSY_BACKOFF_MS` / `WAKE_BUSY_BACKOFF_CAP_MS` / `scheduleDeferredWake` / `pruneAcknowledgedEvents`）。
- 测试：`test/unit/herdsman-pi-extension.test.ts` 既有文件内新增 2 例——`describe("herdsman-pi busy wake fallback backoff")` 的 `caps the busy fallback ladder at the 5s ceiling and keeps the first rung at 100ms`（常量三段一致 + 走到末级后差 1ms 不释放、越过 1ms 才释放），`describe("herdsman-pi dead-letter retry budget (turn-loop regression)")` 的 `sweeps the retry flag, the attempt counter and the deadline together once the daemon confirms the id`（水位清扫三字段成组、越水位的 id 不动）。
- 复核（负控）：把梯子还原成 `[100, 200, 500, 1000, 2000]` → 新 cap 用例失败（常量断言与提前释放各一处）；把 `wakeRetryableEventIds` 清扫删掉 → 新清扫用例失败。`abb357b` / `606edcb` 的既有 busy / defer / dead-letter 用例一字未改且全绿；`dropUnackedDelivered` 内部顺序未动。
- 前置：`abb357b`（dead-letter 重投预算 + busy 自旋退避）及其笔记 `20261005-dead-letter-retry-budget.md`（该笔记第 5 条由本笔记订正，其余条目仍有效，已在文首加链接）；`606edcb`（busy wake defer to settled）。daemon 侧未动。
- 遗留项：收尾轮 reviewer / oracle 的建议级发现（含 OBS-1 更便宜的观测手段、三个 dead-letter 记账字段的收敛重构）已落盘 `20261005-busy-wake-review-leftovers.md`；本笔记仅按 oracle M-1 订正事实措辞，未改变任何决策结论。

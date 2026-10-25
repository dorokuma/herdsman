---
status: active # active | superseded
superseded_by: ""
supersedes: ""
# 模块可选值: observability, daemon, cli, config, db, herdr, shared, herdsman-pi, herdsman-herdr-plugin, release
模块: "observability"
---

# agy 叫醒竞态：working 完成事件改为 defer、同 terminal 完成事件去重、叫醒前检测输入框草稿

## 一句话结论

`liveWorkingTerminalIds` 命中的完成事件**不能再当永久噪声静默 ack**，必须放进 `deferredWorkingEventIds` 保持「未 ack、未 presented」等下一轮重投，否则 `agent.done` 与 `agent.list` 状态刷新之间的竞态会永久吞掉真正的最终结果；与之配套，同一 `terminalId` 在一批候选事件里出现多条完成事件时，只有**最新一条能产出有效 completed outcome 的完成事件**进入唤醒批次，更早的按 `superseded by newer completion for terminal <id>` 抑制并 ack；另外 `WakeWaiter` 在 `triggerWakePrompt` 前要先 `herdr pane read --lines 40` + `parseAgyComposerDraft` 看 orchestrator 输入框是否有半成品草稿，有草稿就推迟本轮叫醒，但**解析不到框必须回退成照常叫醒**，避免解析器退化把唤醒永久堵死。

## 背景

`20261024-agy-smart-wake-standby-cross-workspace.md` 引入的中间回合抑制只有一个信号：完成事件的 `terminalId` 在实时 `agent.list` 里仍是 `working` ⇒ 判为未决中间回合 ⇒ 进 `suppressedEventIds` 静默 ack。这条规则把「中间回合」和「状态刷新竞态」当成同一件事，但两者在数据上不可区分：

1. **竞态吞事件**：peer 真正跑完最后一轮时，`agent.done` 落库通常早于 Herdr 侧状态从 `working` 翻转。`WakeWaiter` 的 `peek` 若正好落在这个空隙，实时状态还是 `working`，真正的最终结果被当成中间回合**永久 ack**，游标推过去之后再也没有第二次机会——表现就是「工人干完了活，主代理永远等不到唤醒」。
2. **同批重复注入**：改为 defer 之后出现新问题。中途 `agent.done`（未 ack、未 presented）与随后的最终 `agent.done` 会同时留在候选批里；等状态追上后两条一起被投影成 `completed`，一次唤醒往 `PreInvocation` 注入**同一 peer 的两条 outcome**（中途轮文本 + 最终轮文本），既重复又自相矛盾。
3. **叫醒打在草稿上**：`triggerWakePrompt` 走 `herdr agent prompt`，会把文本提交进 agy 输入框。用户正在 orchestrator 窗格里打字（草稿没提交）时叫醒，等于把 `[herdsman-wake]` 混进用户正在编辑的那一行，破坏用户输入。
4. **长草稿 / 草稿内含横线**：`parseAgyComposerDraft` 取「倒数第二条横线」当 composer 框顶。用户在多行草稿里自己敲了一行 `--------` / `────────`（正好命中横线正则）时，倒数第二条横线变成草稿内容，`bottom <= top + 1` 成立 → 返回 `undefined`；即便没返回 `undefined`，框区域也会被截断成只剩横线之后的那一行。同时 `--lines 12` 的读取窗口容不下多行草稿（框顶在窗口之外），同样解析失败。

## 决策

1. **working 命中的完成事件从「永久 suppress」改为「defer」**（`src/agy/hook.ts: projectAgyOutcomes` / `peekNextWakeBatch` / `takeNextWakeBatch`）
   - 返回值新增 `deferredWorkingEventIds`：命中的事件既不 ack 也不进 `presentedEventIds`，日志文案改为 `deferred: peer terminal <id> still working in agent.list`。
   - `peekNextWakeBatch` 返回 `hasDeferredWorking`，`WakeWaiter` 据此置 `retryWakePending = true`，让下一轮 poll（活跃期 `DEFAULT_POLL_INTERVAL_MS = 1500ms`）重新投影，不依赖 push；真·中间回合（peer 一直 `working`）只会一直 defer，不产生叫醒，也不会把事件 ack 掉。
   - `takeNextWakeBatch` 只在 `outcomes.length === 0` 时对**永久**抑制项（无 terminalId、非 outcome 类型、fallback 被完成事件取代、上游错误文本、空 completed 文本）执行 ack；deferred id 不参与 ack/presented，`hasDeferredWorking` 与 `hasOutcomes` 是两条独立信号。
2. **同 `terminalId` 多条完成事件按「最新有效完成」去重**（`projectAgyOutcomes`）
   - 先扫一遍候选事件，按 `terminalId` 记录**最后一条能产出有效 completed outcome 的完成事件 id**：要求 `isCompletionEvent` 且 `outcomeKind === "completed"`，且通过 `fallbackOutcome` 被同 pane 完成事件取代、`isUpstreamErrorText`、空文本三项检查（与主循环同一套判定，避免两条路径口径漂移）。
   - 主循环里（在 working-defer 判定之后）：同 `terminalId` 且 `event.id` 小于该 id 的**完成事件**进 `suppressedEventIds`，日志 `superseded by newer completion for terminal <id>`，随批次一起 ack。
   - 去重只对完成事件生效：同 terminal 更早的 `agent.failed` / `agent.discarded` 不受影响，避免顺手改变失败/停滞事件的投递语义。
   - 最新一条完成事件本身无效（空文本 / 上游错误）时，锚点退回到更早那条有效完成事件；无效的新事件继续走原有的「空 completed 文本 / 上游错误」抑制分支，不会被注入成 `(no assistant message)`。
3. **叫醒前检测 composer 草稿**（`defaultReadPaneText` / `parseAgyComposerDraft` / `WakeWaiter`）
   - `defaultReadPaneText` 用 `herdr pane read <paneId> --lines 40`；`--lines 12` 只看得到 composer 底部几行，多行草稿的框顶落在窗口外就定位不到框。
   - 框底固定为状态栏上方最后一条横线；**框顶从倒数第二条横线往前回溯，取最后一条「下一行以 `>` 开头」（`/^>(?:\s|$)/`）的横线**——agy composer 框顶横线的下一行必然是 `>` 提示行，而用户草稿里的横线后面不会。
   - 找不到这样的框顶、或 `bottom <= top + 1`，返回 `undefined`；`readComposerDraft` 据此返回 `unknown`，`WakeWaiter` **照常叫醒**（回到修复前行为）。这是刻意的防死锁设计：解析器任何一次退化都只可能少推迟一次叫醒，不可能把叫醒永久堵在「一直判定有草稿」上。
   - 命中草稿时只推迟本轮：`retryWakePending = true` 后下一轮 poll 重试；用户自己提交草稿会触发 `PreInvocation`，`takeNextWakeBatch` 照常投递该批次，事件不会因为推迟而丢失。推迟期间 `waitTimeoutMs`（默认 `DEFAULT_WAIT_TIMEOUT_MS = 24` 小时）仍是硬上限。

## 边界与权衡（残余窗口与已知取舍）

1. **defer 不是无损的**：真·中间回合（peer 长时间 `working`）期间，其中途 `agent.done` 一直留在未 ack 状态，直到 peer 状态追上或 `waitTimeoutMs`（默认 24 小时）到期。代价是多留若干轮候选事件与每轮一条 `deferred:` warn 日志；收益是竞态下真正的最终结果不再被永久 ack。
2. **同批去重只看 `terminalId`，不看 pane generation**：同一 terminal 在同一批里的两条完成事件必然属于同一窗口的两轮，跨 generation 的取代已由既有的 `completedPanes` / fallback 抑制覆盖，不在此处重复判定。
3. **草稿推迟期间的轮询与日志频率（reviewer consider）**：推迟走的是与 deferred 重试相同的 `retryWakePending` 路径，因此在整个 `pollIntervalMs`（活跃期 `1500ms`）节奏上每轮一次 `herdr pane read` + 一条 `输入框有草稿，推迟叫醒` warn。用户长时间停在草稿上时 warn 会持续累积。当前不引入指数退避或「草稿未变化就跳过读取」的状态机：`WakeWaiter` 生产环境是 `detached` + `stdio: "ignore"` 拉起，stderr 直接进 `/dev/null`，日志量不构成生产压力；而退避状态机会把「草稿何时算变化」变成新的隐式状态，收益不抵复杂度。**作为已知观察项保留**：若将来把 waiter 的 stderr 接入 journal 或出现长草稿高频轮询的实测证据，再考虑对草稿推迟单独设一个更长的重试间隔。
4. **草稿检测只作用于 `triggerWakePrompt`**：`decision: continue` 的 immediate batch 不经过该检测（那条路径本来就不往输入框写文本）。

## 被放弃的方案（必填）

- **继续永久 ack，另加「状态竞态」白名单**（如给 `working` 命中加时间容差 / 要求 `agent.list` 连续两轮一致）：容差和「连续一致」都是拍出来的新 magic number，且对「中间回合」与「刚完成的竞态」这两类事件依旧给不出判别信号；改成 defer 后不需要区分它们，由下一轮实时状态自己给答案。
- **去重时保留同 terminal 的多条 outcome，仅在展示层合并**：`PreInvocation` 注入的文本就是宿主看到的唯一事实，展示层合并等于把「同一个 peer 两条互相矛盾的 last assistant」继续带给模型；在投影层只保留最新一条有效完成事件才是正确语义。
- **只按「最后一条完成事件」去重（不校验有效性）**：最后一条若是空文本或上游错误文本，前一条有效结论会被它一起吃掉，唤醒批次里只剩一条没有正文的 completed；因此锚点必须是「最后一条**有效**完成事件」。
- **把 `terminalId` 去重扩展到 `agent.failed` / `agent.discarded`**：失败与停滞事件表达的是「这一轮没了」，与更早已完成轮次不是同一维度的取代关系，一并压掉会改变既有投递语义，超出本次修复范围。
- **草稿非空时无限期等待（直到草稿清空）**：解析器一旦误判（新 UI、状态栏变化、框结构改动）就等于永久不叫醒；解析不到框回退成照常叫醒，把最坏情况限制在「推迟若干轮」而不是「永不叫醒」。
- **把读取窗口继续加大（`--lines 80/120`）**：窗口越大，历史消息里的横线与 `>` 引用行越多，误定位概率上升；`40` 行足以覆盖 composer + 状态栏 + 常见多行草稿，且框顶改为「下一行必须是 `>`」后已不依赖窗口大小来消歧。
- **改用 `herdr pane read` 的结构化输出（若将来提供）而不是解析 TUI 文本**：现在 CLI 只给终端文本，解析框线是唯一手段；该风险与回退策略见「边界与权衡」第 3 条。

## 来源

- 双审结论（verifier + reviewer）：同 terminal 多条完成事件同批重复注入（Should Fix 1）、草稿解析在长草稿与草稿含横线时退化（Should Fix 2）、缺 `.agents/notes/` 记录（Should Fix 3）、草稿推迟轮询与日志频率取舍（Consider 4）。
- 代码事实：`src/agy/hook.ts` 的 `projectAgyOutcomes`、`peekNextWakeBatch`、`takeNextWakeBatch`、`WakeWaiter` 分支、`defaultReadPaneText`、`readComposerDraft`、`parseAgyComposerDraft`。
- 回归证据：`test/unit/agy-hook-and-cli.test.ts`
  - `intermediate agent.done while peer still working is deferred (not acked) and a later real completion wakes once`（修复前失败于「注入里同时出现中途轮文本 `已改完代码，继续跑测试。`」，修复后只注入 `event: 1126` 一条，旧中途事件被 suppressed 并在该轮 `Stop` 随批次 ack）。
  - `parseAgyComposerDraft covers empty, single-line, multi-line, placeholder-like, and missing box`（修复前对含 `--------` / `────────` 的多行草稿返回 `第三段` 或 `undefined`，修复后返回完整草稿）。
  - `WakeWaiter wakes through retryWakePending alone when one deferred peer settles while another keeps working`（peer A `working → done` 触发 deferred 重试、peer B 持续 `working` 且全程无 push 时仍完成一次叫醒）。
- 前序决策：`.agents/notes/20261024-agy-smart-wake-standby-cross-workspace.md`（中间回合抑制与常驻 `WakeWaiter` 的原始设计，本笔记把其中「working 命中即永久静默 ack」一条改为 defer）。

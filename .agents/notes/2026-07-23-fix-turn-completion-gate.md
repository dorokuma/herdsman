# 2026-07-23 fix/turn-completion-gate 决策记录

## 触发条件
六触发条件：修复根因明确且涉及多文件契约变更、新增单元测试覆盖四个场景、修改门禁与退避策略。

## 决策

### 1. 扩展 AgentHistoryMessage / AgentHistoryExcerpt 携带 stopReason
- **原因**：gate 检查需要知道 baseline 之后最新 assistant 消息的 stopReason 是否为 stop/length。
- **做法**：在 `src/observability/contracts.ts` 中给 `AgentHistoryMessage` 和 `AgentHistoryExcerpt` 增加可选 `stopReason?: string` 字段。
- **影响**：pi-reader 会填充该字段；其他 reader 暂不填充（保持 undefined），不影响现有逻辑。

### 2. 扩展 TurnCompletionSignal / AgentTurnCompletedInput 携带 expectedText
- **原因**：fix 2 要求 turn signal 路径做文本指纹校验，需要 signal 携带扩展端确认时的 expectedText。
- **做法**：在 `src/observability/turn-completion.ts` 和 `src/observability/contracts.ts` 中增加可选 `expectedText?: string`。
- **影响**：扩展端 `packages/herdsman-pi/src/index.ts` 发送 signal 时携带 expectedText；daemon 端 record 时保存。

### 3. #waitForHistoryAdvance 改为指数退避，总窗口 ~30s
- **原因**：原固定 8×500ms=4s 无法覆盖 terminated 恢复场景（77s 后才写出最终报告）。
- **做法**：延迟从 500ms 起步指数翻倍，上限 16s，增加总时长 30s 上限检查；maxAttempts 仍由调用方控制，但内部会因时间上限提前退出。

### 4. turn signal 路径加 gate
- **做法**：
  - 收到 turn signal 并 refresh 出 fresh 后，检查 fresh.lastAssistantMessage 的 stopReason 是否为 `"stop"` 或 `"length"`。
  - 若不是，或 expectedText 与 fresh.lastAssistantMessage.text 后缀不一致，降级走 `#waitForHistoryAdvance`。
  - 降级路径与无 signal 路径保持一致。

### 5. 无 signal 路径补 gate
- **做法**：在无 signal 且 history 未推进时，现有代码已将 lastAssistantMessage 置 null；现在补充：即使 history 有推进，若最新 assistant 消息的 stopReason 不是 stop/length，也按 noAdvance 处理（置 null）。

### 6. 测试覆盖四个场景
- terminated 后 77s 恢复（error→stop）
- signal 指纹不匹配降级
- noAdvance 置 null 回归
- 退避窗口行为

## 偏差与遗留
- 仅 pi agent 路径受 stopReason gate 影响；其他 agent 的 lastAssistantMessage.stopReason 为 undefined，不影响现有逻辑。
- 其他 history reader（claude/codex/gemini/grok/opencode/antigravity）暂不填充 stopReason，因为当前 bug 仅针对 pi agent；若未来需要可逐个 reader 扩展。

## 第三轮修补（oracle 第二轮阻断后）

### 7. schema 补充 expectedText
- `src/observability/schemas.ts` 的 `agentTurnCompletedInputSchema` 增加可选 `expectedText: Type.Optional(Type.String())`。
- 原因：新扩展发的 expectedText 在 RPC 入口被 `additionalProperties: false` 拒掉，daemon 侧透传是死代码。

### 8. degraded 事件语义改为 null + degradedReason
- 三处 degraded 分支（expected_text_mismatch / non_terminal_assistant / no_advance_from_input）从「发最好文本」改回「lastAssistantMessage 置 null + degraded:true + degradedReason 进 payloadExtra」。
- `degradedReason` 枚举值写入 `src/observability/contracts.ts`：`"expected_text_mismatch"` / `"no_advance_from_input"` / `"non_terminal_assistant"`。
- 消费方天然忽略空回传，后续 plan retry 会带完整文本再来。

### 9. 双端口 textFromContent/sanitizeText 同步守护
- daemon 侧 `src/agent-history/text.ts` 与扩展侧 `packages/herdsman-pi/src/sanitize-text.ts` 各加交叉引用注释。
- `test/unit/agent-history-text.test.ts` 新增 parity 测试：textFromContent 与 sanitizeText 两端输出一致。
- 扩展端 `signalTurnCompletion` 在 expectedText 为空串时不发送该字段，避免指纹门禁被空串绕过。

### 10. 测试同步
- W13：断言改为 `lastAssistantMessage: null` + `degraded:true` + `degradedReason:"no_advance_from_input"`。
- S5：退避测试允许最后一次 sleep 越界 30s，断言总时长 ≤ 31.5s（30s + 16s maxDelay）。
- S7：断言改为 `lastAssistantMessage: null` + `degraded:true` + `degradedReason:"expected_text_mismatch"`。
- 新增 RPC 集成测试：带 expectedText 的 agent.turn.completed 请求 accepted:true，且 waiter 能拿到 expectedText。
- 新增 pi 扩展单元测试：thinking-only 消息不发送 expectedText 字段。

## 第四轮修补（oracle 第三轮阻断后）

### 11. degraded 路径不终结 plan
- **原因**：`#runPlanRow` 在 `#appendStatusEvents` 成功 emit 后无条件 `store.markCompleted`，degraded 事件发出后 plan 直接 completed，不再 retry；消费方按约忽略空回传，最终文本永久丢失。
- **做法**：`#runPlanRow` 检测到返回事件的 payload 含 `degraded: true` 时，改走 `store.markRetry(row.id, new Error("degraded"))`，沿用 attempts/上限与 last_error 语义；pending 状态留给后续 periodic drain 重新推进并以完整文本重发。
- **边界**：`markRetry` 达到上限后变为 `failed`（非 `discarded`，因为错误不是 PLAN_WAITING_HISTORY）；drain 侧已具备 `#appendPlanFailedEvent` 回补逻辑。

### 12. turn.received 分支判定顺序对称
- **原因**：turn.received 分支将 `expectedText` 比对放在 `no_advance_from_input` 之前，而无信号分支恰好相反；磁盘未落盘时会被误报 expected_text_mismatch。
- **做法**：将 turn.received 分支的 `!isRetry && !historyHasAdvanced(advanced, input.compactHistory, { requireAssistantChange: true })` 移到 `expectedText` 比对之前，与无信号分支对称。

### 13. S6 测试改真
- **原因**：原 S6 测试在 attempts=0 下运行，mock 第二次 refresh 即返回 stop，走快路径当场发出，从未进 retry。
- **做法**：先 `markRetry` 将计划设为 retry（attempts=1），mock 持续返回 error 状态使首轮 drain 进入 `#waitForHistoryAdvance` 后因无推进抛 `PlanWaitingHistoryError` → markRetry（attempts=2）。

### 14. 补 degraded 端到端集成测试
- **做法**：新增 S8 测试。首轮 drain 因 expectedText mismatch 发出 degraded 事件（lastAssistantMessage=null），plan 进入 retry（attempts=1）；第二轮 drain 历史落盘匹配 expectedText，发出完整文本事件，plan 最终 completed。

### 15. 同步快路径负例
- **做法**：在 sync fast path（signal 先于 handleHerdrEvent 到达）下，mock 文本与 expectedText 不匹配，断言降级为 degraded 事件而非干净匹配事件。

### 16. wake.ts legacy 过滤同步
- **做法**：`packages/herdsman-pi/src/wake.ts` 的 `agent.failed` 过滤除 `PLAN_WAITING_HISTORY` 外，新增 `degraded` 过滤。两类都是 transient retry exhaustion，不应触发 wake。
- **RPC 枚举校验**：当前 `degradedReason` 仅用于 daemon 内部 payloadExtra，不经过 RPC 入口 schema 校验，故不加 RPC 侧枚举校验。

## 最终状态
- `pnpm check` 全绿（792 tests, 53 files）。
- 未重启运行中的 daemon（PID 822771），消息管道保持 intact。

## 第四轮最终状态
- 待验证：tsc --noEmit、pnpm build、全量测试、pnpm check。
- 本轮标记行：见末行 [MARK-WORKER-HERDSMAN-R4]。

## 第五轮修补（验收与补齐 R4B）

### 17. 移除遗留 debug console.log
- `src/observability/agent-index-service.ts` 有上一轮 worker 遗留的 `console.log("EVENT PAYLOAD", eventPayload)`，已移除。

### 18. turn.received 快路径判定顺序修复
- **问题**：快路径在 `input.compactHistory` 非终端时（signal 在事件之后到达，snapshot 从非终端变为终端）直接比对 `textMatches` 返回，未优先检查 `no_advance_from_input`。
- **做法**：快路径条件追加 advancement gate：`(isTerminalAssistant(input.compactHistory) || historyHasAdvanced(fresh, input.compactHistory))`。若 input 已是终端（signal 先于事件）， advancement gate 放行（保持原有立即 emit 行为）；若 input 非终端，则要求 fresh 相对 input 有推进才走快路径，否则进入 `#waitForHistoryAdvance` 并优先检查 `no_advance_from_input`。与无信号分支判定顺序对称。

### 19. TurnCompletionRegistry 补 sleep 选项
- **原因**：测试中传入 `sleep` 选项模拟异步等待，但类型定义不含该字段，导致 `pnpm typecheck` 报 8 个 TS2353 错误。
- **做法**：构造函数选项增加 `sleep?: (ms: number) => Promise<void>`，默认使用 `setTimeout` 包装。

### 20. 当前验收状态
- `pnpm check`：全绿（794 tests, 53 files, typecheck, biome lint/format, drizzle, package checks）。
- 已知无关故障：`test/unit/clean-agent-event-duplicates.test.ts` 和 `test/unit/daemon-process-manager.test.ts` 在并行全量运行时偶发 file lock 竞争失败（单文件独立运行全绿），与本次改动无关。

## 六项验收结论
1. degraded → markRetry ✓（`#runPlanRow` 中 `isDegraded` 分支改走 `store.markRetry`）
2. turn.received 判定顺序对称 ✓（快路径追加 advancement gate）
3. S6 测试真进 retry ✓（attempts 从 1→2，无 agent.done，PlanWaitingHistoryError 触发 markRetry）
4. 同步快路径负例 ✓（expectedText mismatch → degraded, lastAssistantMessage=null）
5. 集成测试覆盖 degraded→retry→complete ✓（S8 测试，计划从 pending 经 degraded 重试后 completed）
6. wake.ts 过滤 degraded ✓（`PLAN_WAITING_HISTORY` 和 `degraded` 均被过滤）；degradedReason 不做 RPC 枚举校验（仅内部 payloadExtra，不经过 RPC schema）

- 本轮标记行：见末行 [MARK-WORKER-HERDSMAN-R4B]。
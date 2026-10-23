---
status: active
superseded_by: ""
supersedes: ""
模块: "observability"
---

# AGY 编排者审查建议项处理与后续考量（Reviewer Considerations）

## 一句话结论

登记独立代码审查中提出的四项 consider 建议，明确修 A 已完成的统一项及未在本次修复的三项考量与后续推进依据。

## 背景

在 `fix/agy-orchestrator-v2-1` 双审（reviewer 与 oracle）阶段，审查者在确认阻断性缺陷（修 A–D）之余，提出了四项关于系统健壮性与可维护性的非阻塞建议（consider items）。为了遵循代码库架构规范并防范隐性技术债务，对这四项考量进行逐项台账化登记。

## 审查建议项处理台账

### 1. raw `store.markRetry` 调用绕过封装与丢失选项（已在本次修复）

- **建议内容**：`drainPendingPlans` 刷新失败路径以及 `#runPlanRow` 通用错误 catch 直接调用底层 `store.markRetry`，丢失了 `isTargetWorking` / `stallDetected` 等 options，且绕过了 `#markPlanRetry` 的 tracker 清理与统一处理逻辑。
- **处置结果**：**已随本次修复彻底收口**。将 `src/observability/agent-index-service.ts` 的 drain 失败分支及 `#runPlanRow` 通用错误 catch 分支统一重构成通过 `#markPlanRetry` 封装调用，完整透传 options 并保障 tracker 正常管理（修 C），同时保持 pending 时的 throw 时序不变。

### 2. 运行时重试环恢复豁免设计确认（修 D，确认属正确设计，不改代码行为）

- **核实背景**：复审阶段曾有意见认为 `#retryWaitingPlanRow` 调用 `#markPlanRetry` 时未显式透传 options 属于遗漏。
- **架构定论**：经核对实际架构设计，确认**当前代码行为完全正确**：
  - 运行时重试环（`#retryWaitingPlanRow`）在 daemon 正常运转期间执行，由 10s 活跃定时器驱动，此时内存与 DB 数据鲜活，故应始终保持 working 豁免（options 缺省即 `allowWorkingExemption: true`）；
  - 这与启动期恢复重试（startup drain，属于关机后残留的未知状态，按 stale 处理不予豁免）具有截然不同的语义分立；
  - 维持当前代码调用方式不变，在源码约 583 行补充详细设计注释，并在本笔记固定该设计依据，彻底关闭歧义。

### 3. `agent.discarded` 豁免门控与 stall 独立常量对齐（修 A 与 修 B）

- **问题根因**：原 stall 收口写入 `PLAN_WAITING_HISTORY`，被两端投影器（Pi 与 Agy）静默忽略，导致 stall 判死的 agent 漏报消失；且 `src/agy/hook.ts` 原先缺少 agent 门控，与 `wake.ts` 语义相反。
- **处置结果**：
  - 新增独立的 stall 关闭原因常量 `PLAN_STALLED`；stall 判定成立收口时写入该 reason，两端投影器对它均返回 `"failed"`，确保真正判死的 agent 能正常唤醒编排者（修 A）；
  - `src/agy/hook.ts` 与 `packages/herdsman-pi/src/wake.ts` 使用一致的门控语义：仅当 `payload.agent` 为 `"agy"`、或为空/`null`（`PLAN_WAITING_HISTORY` 只能由 `agy` 产生）时，豁免 `PLAN_WAITING_HISTORY` 与 `degraded` 且无 `fallbackOutcome` 的中间态（`return undefined`）；Pi 自身子代理（`payload.agent === "pi"`）一律不豁免，即使带 degraded 也必须报出真实失败（修 B）。

### 4. `awaitingNextTurn` 依赖 best-effort 状态写入失败至少增加 warning（未在本次修复）

- **建议内容**：`src/agy/hook.ts` 在 `Stop` 钩子中持久化 `awaitingNextTurn = true` 为 best-effort 写入（写入失败仅 catch 静默忽略）。如果写入失败，下一回合 `PreInvocation` 无法感知未决状态，建议至少输出 warning 日志。
- **未修理由**：
  - `Stop` 钩子运行在用户终端回合结束的关键链路上，当前优先保障进程退出不阻塞 TUI 响应；
  - 写入使用临时文件原子 rename 机制，在用户私有状态目录正常权限下极少失败；
  - `PreInvocation` 除 `awaitingNextTurn` 外，还结合了 `invocationNum === 1`、存在 `waiterPid` 以及 `turnEpoch` 未初始化等多重条件综合判定新回合首轮，具有良好的防单点故障容错能力；
  - 本次变更严格收敛于确认缺陷，该处日志级别优化留待后续统一日志审计时处理。

### 5. 指纹含全量 `lastAssistantMessage` 文本建议改哈希或截断（未在本次修复）

- **建议内容**：`#computeProgressFingerprint` 将 `compactHistory?.lastAssistantMessage?.text` 全量拼入指纹字符串，在特长回复场景下会占用较多内存并增加字符串拼接与比较开销，建议截断或做 SHA-256 哈希。
- **未修理由**：
  - 当前 `compactHistory` 已经在 reader 层被截断处理（如 tail window 及 compaction），进入索引服务的助理回复长度普遍在数 KB 到数十 KB 范围内；
  - 直接拼字符串保持了逻辑的极简与完全确定性，无需引入额外哈希计算开销；
  - 目前仅用于活动计划的 stall 检测（每个 plan 维护一个当前指纹），内存与性能开销可控；
  - 后续若在端到端基准测试中观察到大文本拼接瓶颈，再统一进行指纹表示优化。

### 6. pending 期间 attempts 无上界累加建议封顶或文档化（修 F 落地总时长上限）

- **建议内容**：在 agent 持续 working 豁免期间，每个 10s 重试周期的失败都会导致 `attempts` 累加。若任务持续数小时，`attempts` 可能累加至上千，建议做上限封顶（如 saturated cap）或文档化说明。
- **处置说明**：
  - `attempts` 字段在 SQLite DB 中为 64 位整数，物理上不存在溢出风险，忠实反映了等待历史就绪的实际轮询次数；
  - **修 F 已新增基于真实时间的硬上限**（`WORKING_PLAN_MAX_WAIT_MS = 30 * 60 * 1000`，30 分钟），即使 agent 持续吐出不同 tool_result 使指纹不断刷新，超过 30 分钟也必须判定 stall 并收口为 `PLAN_STALLED`；
  - 结合指纹连续 18 次不变的 stall 防线，彻底杜绝了计划无限挂起问题。

### 7. drain 刷新失败分支 discarded 事件推送判据统一与投影器分工（已在本次修复）

- **建议内容**：`drainPendingPlans` 刷新失败分支（`#appendPlanDiscardedEvent`）生成的 `agent.discarded` 事件仅写入 SQLite 存储，未主动向连接的编排者推送。
- **处置结果**：**已随本次修复彻底解决活性漏报并统一三条路径推送判据**。
  - **漏报成因与判据分裂**：上一轮将该分支接入 `#markPlanRetry` 之后，stall 判定可在此路径成立并收口为 `discarded` 伴随 `PLAN_STALLED`。当时曾尝试仅在 `updated.lastError === PLAN_STALLED` 时才推送。但复审指出：重试环同类分支（`#retryWaitingPlanRow`）是无条件推送，导致两处判据分裂；且 Pi 的计划同样会产生 `PLAN_WAITING_HISTORY` 的 discarded，Pi 投影器契约明确要求必须作为 `failed` 上报唤醒，若在索引服务按 lastError 过滤会导致同族漏报。
  - **三条路径统一判据**：
    1. 活跃重试环（`#retryWaitingPlanRow`）刷新失败 catch 的 discarded 分支；
    2. 启动与周期性 drain（`#drainPlanRow`）刷新失败 catch 的 discarded 分支；
    3. `#drainPlanRow` 收取 `#runPlanRow` 结果后推送的分支（以及 `executeStatusEventPlan` / `HerdrSessionWatchManager#submitPlan` 路径）。
    当前三条路径判据现已完全对齐为**无条件推送**：只要 `#appendPlanDiscardedEvent` 或 `#runPlanRow` 产生合法类型事件，均无条件调用 `#onAgentEvent` 推送，不再以 `lastError` 取值作为过滤门槛。
  - **下游投影器分工与抑制噪声机制**：抑制与噪声控制的责任明确划分给下游投影器（Projector）：
    - **Agy 投影器**（`src/agy/hook.ts`）：对 `payload.agent === "agy"`（或空），常规 `PLAN_WAITING_HISTORY` 与 `degraded` 且无 `fallbackOutcome` 的 discarded 会被投影器返回 `undefined` 静默，不产生任何唤醒噪声；而 `PLAN_STALLED` 及其他失败原因则投影为 `failed` 唤醒编排者。
    - **Pi 投影器**（`packages/herdsman-pi/src/wake.ts`）：Pi 投影器契约要求 Pi 的计划若产生 discarded（包括 `PLAN_WAITING_HISTORY`），必须如实投影为 `failed` 唤醒编排者，彻底消除同族漏报。

### 8. `agent.failed` 分支的等待历史豁免缺少 agent 门控的措辞收窄（Reviewer 建议，未在本次修复）

- **建议内容**：在事件投影层对 `agent.failed` 分支的等待历史/degraded 豁免逻辑中，审查意见认为相关设计注释与豁免说明措辞较宽泛，缺少严格限定仅适用于 `agy` 代理的措辞收窄。
- **未修理由**：
  - 核心行为层面，`src/agy/hook.ts` 与 `packages/herdsman-pi/src/wake.ts` 均已落地精准的 agent 门控（仅豁免 `agy` 产生且无 fallbackOutcome 的合成中间态）；
  - `agent.failed` 作为真实失败事件，目前代码行为已在回归测试中严格锁定，非 agy 失败事件均严格派发不被误豁免；
  - 该建议本质属于文档与注释措辞的进一步精准化提炼，为避免在阻断缺陷修复轮次引入非功能性文案扰动，留待后续文档与注释专项维护时统一修订。

### 9. 极端边界与异常交互测试缺口（Reviewer 建议，未在本次修复）

- **建议内容**：审查指出在极端并发、drain 与活跃重试环交叉异常边界等边缘场景下仍有潜在测试盲区，建议补充更多防御性集成用例。
- **未修理由**：
  - 本轮修复严格聚焦并闭环了双审共同指出的 tracker 误重置阻断缺陷及同型模式，且已补充高价值端到端验证用例覆盖通用异常下的 working 连续超时收口；
  - 遵守项目 shared 最小测试纪律，单次 bug 修复不盲目发散或堆砌超额用例；
  - 其它边界防御用例纳入后续稳定性与压测规划统一排期。

### 10. retry ring 与 drain 竞速重置观察项（Oracle 建议，未在本次修复）

- **建议内容**：活跃定时器驱动的重试环（`#scheduleWaitingHistoryRetry`）与后台巡检/启动恢复（`drainPendingPlans`）可能并发触碰同一 pending plan，在极限竞态下可能产生重复执行或尝试计数重叠。
- **未修理由**：
  - 架构上已有两道严格的互斥保证：一是服务内部对同一 agentId 的 plan 处理通过 `#enqueueAgentPlan` 严格串行化；二是底层 `store.markRunning` 采用原子条件更新（CAS），并发争抢时仅有一方能转为 `running` 成功推进，另一方立即退出；
  - 另外每次调度均显式清理旧定时器，从源头抑制重叠；
  - 现有机制足以保障状态一致性，未观察到实际数据损坏，故作为长期并发观察项持续监控。

### 11. stall 误判风险观察项（Oracle 建议，未在本次修复）

- **建议内容**：引入指纹冻结 18 次与 30 分钟总等待上限作为 stall 判定后，若外部 agent 执行特长耗时工具（如无日志超长构建）可能面临被误判为 stall 的风险。
- **未修理由**：
  - 30 分钟（`WORKING_PLAN_MAX_WAIT_MS`）对于 agent 单次回合的单次 status event 是极其宽松的窗口，绝大多数正常调用均在秒级到数分钟内完成；
  - 只要 agent 在执行过程中产生任何输出、更新 `lastToolResult` 或修改工作区使 revision/mtime 变动，指纹即会更新并重置 18 次计数；
  - 30 分钟绝对硬上限是防范活性永久死锁（liveness deadlock）的必要保障；
  - 当前未见实际正常业务被误伤报告，故保持既有参数作为生产观察项，后续视需求支持动态配置化。

### 12. degraded 分支在 working agy 上的语义变更与架构取舍（确认属预期行为，不改代码）

- **语义变更核查**：在将 `#runPlanRow` 的 degraded 重试路径统一接入 `#markPlanRetry` 并赋予 `isTargetWorking` 豁免后，若 agy 持续处于 `working` 状态，degraded 循环的行为发生了语义变更：
  - **原旧行为**：达到约 8 轮重试（`STATUS_PLAN_MAX_ATTEMPTS`）后必强制判为 `failed` 收口；
  - **现新行为**：享有 `isTargetWorking` 豁免，在每轮周期重试中保持 `pending`，直到触发 stall 检测（指纹连续 18 次不变或总等待时长达到最长 30 分钟 `WORKING_PLAN_MAX_WAIT_MS`）才收口为 `failed`。
- **架构取舍与决策依据**：**本次选择保留该新行为，不做代码改动**：
  1. **与等待历史豁免哲学 100% 一致**：agy 作为长耗时复杂任务代理，在仍在持续 working 执行时，历史未就绪或临时出现 degraded 属于中间过渡态，过早（仅 8 轮、约数十秒至数分钟）直接掐死并判 failed 会对正常运行的大任务造成误杀；
  2. **具备兜底硬上限保护**：修 F 已引入基于真实时间的 30 分钟硬上限（以及 18 次无进展判定），存在严格的超时界限，绝不会无限悬挂；
  3. **职责划分更合理**：将"是否判死"的裁决权统一收口至 stall 检测机制，消除了原先 8 轮死循环的硬编码魔数与不一致体验。

## 来源

- 双审审查报告与 Oracle 独立审查结论（2026-10-23）
- 关联笔记：`.agents/notes/20261021-agy-orchestrator-and-auto-on.md`



---
status: active # active | superseded
superseded_by: ""
supersedes: ""
# 模块可选值: observability, daemon, cli, config, db, herdr, shared, herdsman-pi, herdsman-herdr-plugin, release
模块: herdsman-pi
---

# 删除 HERDSMAN AGENT CONTEXT 预览块（整块，不留开关）

## 一句话结论

`packages/herdsman-pi` 的 `[HERDSMAN AGENT CONTEXT]` 预览块整块删除：context 钩子不再注入 `herdsman-agent-context` 自定义消息，`formatHiddenAgentContext` 及其专属死代码（`truncateSummary`、`formatTimestamp`、`HerdsmanState.pinnedContext` 与快照求交 `retain`）一并移除；wake 全文通道（`herdsman-wake-context` follow-up 单轨 + wake.ts `formatAgentOutcomeUpdates`/`normalizeExcerpt`）零改动。分支 `chore/remove-agent-context-preview`，未 commit、未 push、未动版本号。

## 背景

- 预览块的设计意图是「速览」：把每个 agent 的报告经 `truncateSummary(..., 100)` 截略成单行，连同 tabTitle（60 字符上限）与时间戳，由 `pi.on("context")` 注入主会话上下文。
- 实际效果反复造成「报告被截断」的误判。已审计确认：数据从未丢失——wake 全文轨（隐藏 follow-up 消息）一直在投递完整报告，预览块信息价值为零，盘面状态本应实时查 herdr。
- 用户拍板：整块删除，不留开关。

## 决策

1. **删注入**：`context` 钩子删除 `formatHiddenAgentContext` 注入块。钩子本身保留，但只做入口清洗——把 herdsman 自己的 context/wake 旧条目从传入列表过滤掉（重放旧会话时，防止旧预览文本再次喂给模型）；`herdsman-wake-context`（唤醒全文本体）不过滤，它是 turn 消费的证据。与上一轮 `20261005-wake-single-track-no-context-pin.md` 保留 `herdsman-wake-queued` 重放防御分支的决策同理。
2. **删死代码**（调用链核实仅服务预览块）：`formatHiddenAgentContext` 整体；`truncateSummary`（仅预览块内 tabTitle/assistant 两处调用）；`formatTimestamp`（仅预览块调用）；`pinnedContext` 字段及其 init/`clearAgentContext` 复位、`agent_start` 赋值、`agent_settled` 清理；stream handler 里的 `retain` 快照求交逻辑。
3. **保留**（被 wake 侧复用或属 wake 全文通道，一行未动）：`formatHiddenAgentUpdates`（`[HERDSMAN AGENT UPDATES]` 全文块，wake 结构测试在用）、`sanitizeAndCleanContextText`/`cleanContextText`（被 `formatHiddenAgentUpdates` 复用，非预览专属）、wake.ts `normalizeExcerpt`/`formatAgentOutcomeUpdates`/`WAKE_POLICY` 与 `pi.sendMessage` 单轨投递路径。
4. **测试**：删除 17 个纯预览格式用例、3 个 cached-context pinning 用例、`herdsman-pi context intersection regressions` 整个 describe；改写 2 个——ack 排序用例去掉 `formatHiddenAgentContext` 断言（保留 `formatHiddenAgentUpdates` 断言）、`defers pending updates while a busy user run` 用例把「context 钩子注入了预览」改为「context 钩子零注入」；其余用例未动。

## 被放弃的方案（必填）

- **保留预览块但加开关/config 降级**：否决。用户明确「整块删除，不留开关」；且已审计确认预览信息价值为零，留着只会继续制造截断误判。
- **保留 `context` 钩子但连入口清洗一起删**（整钩子摘除）：否决。入口清洗在重放旧会话时仍有价值——旧 transcript 里的旧预览文本不应再进模型上下文；删钩子等于把这些过期文本重新暴露给 orchestration。
- **顺手删 `isNormalHerdsmanContext` 里的 `[HERDSMAN AGENT CONTEXT]` marker 分支**：暂缓。该分支正是上一条重放防御的组成部分，与上一轮 reviewer 建议保留 `herdsman-wake-queued` 分支同理；保留它不妨碍本次删除目标。
- **把 `sanitizeAndCleanContextText`/`cleanContextText` 当预览死代码一并删**：否决。调用链核实 `formatHiddenAgentUpdates`（wake 结构）仍在用，删了就是误删 wake 通道的工具链。

## 来源

- 用户拍板：预览块反复造成「报告被截断」误判（已审计确认数据从未丢失、预览信息价值为零、盘面状态应实时查 herdr），整块删除、不留开关。
- 改造前实现：`packages/herdsman-pi/src/index.ts`（`formatHiddenAgentContext` / context 钩子 `herdsman-agent-context` 注入 / `pinnedContext` / `retain` / `truncateSummary` / `formatTimestamp`）。
- 前序笔记：`.agents/notes/20261005-wake-single-track-no-context-pin.md`（busy 路径 context pin 已删，本次删剩余预览块）。
- 基线：main HEAD `0a6a8a2`，工作分支 `chore/remove-agent-context-preview`，未 commit、未 push、未动版本号，留待双审。

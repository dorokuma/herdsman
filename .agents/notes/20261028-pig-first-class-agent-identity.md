---
status: active
superseded_by: ""
supersedes: ""
# 模块可选值: observability, daemon, cli, config, db, herdr, shared, herdsman-pi, herdsman-herdr-plugin, release
模块: "observability"
---

# `agent: "pig"` 一等公民识别（pi/pig 家族）

## 一句话结论

pig（PiG）与 pi 共用同一套 herdsman-pi 扩展与 session 格式，因此在「历史读取器路由、交互式主控判定、会话 hint 兼容、状态事件等待/放行、孤儿/失败事件投递白名单、`#isPiTerminal` 终端归属、presence 注册 schema」全部点位上把 `pig`（及 `herdr:pig` / `subscriberKind: "pig"`）与 `pi` 视为同一 **pi 家族**，而非把 pig 当作「非 pi 的其它 agent」。

## 背景

- pig 是 pi 家族 coding agent，其会话写在 `~/.pig/agent/sessions`（白名单已在 `20261026-pig-session-root-whitelist` 接受），运行同一 herdsman-pi 扩展、产出同一 `pi-jsonl` 格式、上报同一 turn-completion 信号。
- 此前所有 agent-kind 判定都是 `=== "pi"` / `=== "agy"` 的二元硬编码，pig pane 会被归入「非 pi」分支：历史读不出来（`pi-jsonl` 不命中）、交互式/派发角色不区分、turn 完成信号不等待、孤儿 `agent.done`/`agent.idle` 被空 body 门槛误杀、终端不被认作 pi 家族终端。
- 扩展此前在 `session_start` 写死 `sessionRef.agent = "pi"` / `source = "herdr:pi"` / `subscriberKind = "pi"`，pig 会话也会被当成 pi 上报。

## 决策

- **家族化而非逐项白名单复制**：所有「是否 pi 家族」的判定收敛到一个语义——`pi || pig`（大小写不敏感）。TS 在 `src/db/agents.ts`（`isPiFamilyAgent`）与 `src/observability/agent-index-service.ts`（`isPiFamily`）各加一个小辅助；Go 在 `internal/db/agents.go`（`isPiFamilyAgent`）与 `internal/observability/agent_index_events.go`（`isPiFamilyAgentKind`）镜像。
- **注册 schema 扩为 union**：`piPresenceSessionRefSchema` 的 pi 分支接受 `agent: "pi" | "pig"`（`kind` 仍是 `path` only），`subscriberKind` 扩为 `"pi" | "pig" | "agy"`。Go 侧 `piPresenceSessionRefPiSchema` 的 `agent` literals、`SubscriberPig` 常量、`subscriberKind` literals 同步。
- **会话 hint 的跨 pi/pig 兼容**：`setSessionRefByTerminal`、`replaceForSession` 的 `sessionHint` 保留、`mapAgent` 的 `compatibleHint`，除原本的「同名 agent 才兼容」外再放行「同 pi 家族互认」。这样同一 terminal 在 pi↔pig 之间改标签时，已写入的会话 hint 不被清掉。
- **扩展按会话路径识别 pig**：`sessionFile.includes("/.pig/")` → `agent:"pig"` / `source:"herdr:pig"` / `subscriberKind:"pig"`；这是把 pig 标签一路带到 daemon 的唯一来源。`--no-session`（`sessionFile === undefined`）按 pi 处理（其实该分支在 `shouldSkipDaemonClient` 前就已短路，type guard 只是防御）。
- **跟随家族不改变 `: "pi"` 的 event-kind 字面量**：`sameTerminalAssistantContent(..., "pi")`、`payloadFrom(event.Payload)`、`agentKind := "pi"` 这类「事件内容/格式」的 `"pi"` 是 pi-jsonl 格式标签而非 agent 身份，pig 也用同一格式，故不改。

## 被放弃的方案（必填）

- **给每个点位塞一个 `=== "pi" || === "pig"` 的裸判断**：散落的家族判断迟早再漏一处；收敛成 `isPiFamily*` 辅助让新增 agent-kind 判定点有统一的扩展方式，也把「为什么 pig 跟着 pi 走」固化在注释里。
- **只改 daemon 侧、不改扩展**：pig 上报 `agent:"pi"` 时 daemon 虽能跑（家族兼容），但 `AgentIndexRecord.agent` 仍是 `pi`——pig 面板在 `agent list`/路由里就与真 pi 不可区分，违背一等公民目标，故扩展必须报自己的真实 kind。
- **把 pig 判进「非 pi 非 agy」的第三方分支**：pig 不是「另一个要补全 reader 的新 agent」，它和 pi 共享同一个 reader/格式/信号通道；放进非 pi 分支只会把它推向「空 assistant body 门槛」「非 pi 就绪门」这条为 agy/claude 设计的路径。
- **让 `subscriberKind` 沿用 `"pi"`**：subscriberKind 不只是「是不是 pi 通道」——`#isPiTerminal` 的 socket 分支用它区分终端归属，pig 用 `"pig"` 才能让 agy 在 pig pane 上正确判断「这个终端不是我要 lease 的 agy 终端」。

## 来源

- 代码：`src/agent-history/discovery.ts`、`src/observability/interactive-pi.ts`、`src/observability/agent-index-service.ts`、`src/observability/schemas.ts`、`src/observability/contracts.ts`、`src/db/agents.ts`、`src/db/agent-events.ts`、`src/daemon/observability-server.ts`、`packages/herdsman-pi/src/index.ts`，及其 Go 镜像 `go/internal/agenthistory/discovery.go`、`go/internal/observability/interactive_pi.go`、`go/internal/observability/agent_index_events.go`、`go/internal/observability/schemas.go`、`go/internal/observability/contracts.go`、`go/internal/db/agents.go`、`go/internal/db/agent_events.go`、`go/internal/daemon/observability_server.go`。
- 前置决策：`.agents/notes/20261026-pig-session-root-whitelist.md`（pig 自有会话根白名单）。
- 测试：`test/unit/agent-history-discovery.test.ts`、`test/unit/interactive-pi.test.ts`、`test/unit/observability-contracts.test.ts`、`test/unit/herdsman-pi-extension.test.ts`、`test/integration/agent-store-terminal-identity.test.ts`、`test/integration/observability-rpc.test.ts`，及 Go 镜像 `go/internal/agenthistory/discovery_test.go`、`go/internal/observability/interactive_pi_test.go`、`go/internal/observability/contracts_test.go`、`go/internal/db/agent_store_terminal_identity_test.go`、`go/internal/daemon/observability_rpc_test.go`。

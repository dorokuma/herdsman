---
status: active
superseded_by: ""
supersedes: ""
# 模块可选值: observability, daemon, cli, config, db, herdr, shared, herdsman-pi, herdsman-herdr-plugin, release
模块: "observability"
---

# `agent: "pig"` 一等公民识别（pi/pig 家族）

## 一句话结论

TS 版 `pi` 运行本仓 `packages/herdsman-pi`，Go 版 `pig`（PiG）运行独立仓 `pi-extensions/go/herdsman-pi` 与 `pi-extensions/go/herdr-agent-state`；二者各自独立，但产出兼容的 `pi-jsonl` 会话格式与 herdsman 协议、上报同一 turn-completion 信号。因此在「历史读取器路由、交互式主控判定、会话 hint 兼容、状态事件等待/放行、孤儿/失败事件投递白名单、`#isPiTerminal` 终端归属、presence 注册 schema」全部点位上把 `pig`（及 `herdr:pig` / `subscriberKind: "pig"`）与 `pi` 视为同一 **pi 家族**，而非把 pig 当作「非 pi 的其它 agent」。

## 返修记录（C 档门禁第 1 轮）

- **回退 TS 端 `herdsman-pi` 扩展的 `/.pig/` 路径猜测**：`packages/herdsman-pi/src/index.ts` 只运行在 TypeScript 版 `pi` 内；Go 版 `pig` 运行的是独立仓 `pi-extensions/go/herdsman-pi`，二者严格分离。`sessionFile.includes("/.pig/")` 既漏掉 `/tmp/herdr-role-sessions/...` 派发会话，也会在普通 `pi` 路径含该片段时误标。已回退至基线 `eb8e83d`，TS 扩展固定上报 `agent: "pi"` / `source: "herdr:pi"` / `subscriberKind: "pi"`；`pig` 身份的 wire 兼容由 daemon 端 schema/家族判定承担。
- **统一 TS 与 Go 镜像的大小写比较语义**：`isInteractivePiAgent` 与 `#isPiTerminal` 曾用 `.toLowerCase()` 后再比对，与 Go 镜像及全仓 schema 字面量的精确小写语义不一致（会把 `"PIG"` 误判为 pi 家族）。已统一为精确小写比较 `agent === "pi" || agent === "pig"`，并用单测断言大写标签不被归类。
- **补齐 pig 作为事件产生方/完成方的投递与等待验证**：`test/integration/observability-rpc.test.ts` 与 `go/internal/daemon/observability_rpc_test.go` 的 pig 用例原本只覆盖「pig 作 orchestrator 接收方」；已扩展为同时覆盖 pig 终端注册到被派发 pane 后，其 `agent.done` 事件通过 `isDeliverableAgentEvent` 门控投递给 workspace owner、以及 `agent.turn.completed` 信号被 `TurnCompletionRegistry` 正常等待接收。

## Oracle 观察项处置（O1–O5）

- **O1** `resolveSessionFileById` 仅处理 `kind: "id"`：成立但无影响——`pig` 与 `pi` 一样只上报 `kind: "path"` 的 sessionRef，`kind: "id"` 分支只为 agy 等服务；pig 会话解析走路径分支，不需要 `resolveSessionFileById` 支持。维持现状。
- **O2** pig 会话根路径白名单：生产环境 pig 会话根确为默认 `~/.pig/agent/sessions` 与 `/tmp/herdr-role-sessions`，沿用既有白名单边界（`DISPATCHED_PI_SESSION_ROOT` 判定 + `agent-history` 读取器白名单），不新增路径。
- **O3** `sessionRef.agent` 与 `subscriberKind` 交叉校验：二者有意独立校验、不作相等限制，以兼容混合部署期（如 Go 扩展发 `agent: "pig"` + `subscriberKind: "pi"`）。已在 `src/observability/schemas.ts` 与 `go/internal/observability/schemas.go` 的注册 schema 处补注释固化该语义。
- **O4** `pig` 仅使用 `kind: "path"`：与 `pi` 一致，schema 的 pi 家族分支 `kind` 只接受 `path`，与实现相符。
- **O5** 大小写比较不一致：已通过上述统一为精确小写比较的返修改正，TS 与 Go 镜像及 schema 字面量一致。

## 背景

- pig 是 pi 家族 coding agent，其会话写在 `~/.pig/agent/sessions`（白名单已在 `20261026-pig-session-root-whitelist` 接受）；TS `pi` 用本仓 `packages/herdsman-pi`、Go `pig` 用独立仓 `pi-extensions/go/herdsman-pi` 与 `pi-extensions/go/herdr-agent-state`，二者各自独立、产出兼容的 `pi-jsonl` 会话格式与 herdsman 协议、上报同一 turn-completion 信号。
- 此前所有 agent-kind 判定都是 `=== "pi"` / `=== "agy"` 的二元硬编码，pig pane 会被归入「非 pi」分支：历史读不出来（`pi-jsonl` 不命中）、交互式/派发角色不区分、turn 完成信号不等待、孤儿 `agent.done`/`agent.idle` 被空 body 门槛误杀、终端不被认作 pi 家族终端。
- 扩展此前在 `session_start` 写死 `sessionRef.agent = "pi"` / `source = "herdr:pi"` / `subscriberKind = "pi"`，pig 会话也会被当成 pi 上报。

## 决策

- **家族化而非逐项白名单复制**：所有「是否 pi 家族」的判定收敛到一个语义——`pi || pig`（精确小写比较，与 Go 镜像及 schema 字面量一致）。TS 在 `src/db/agents.ts`（`isPiFamilyAgent`）与 `src/observability/agent-index-service.ts`（`isPiFamily`）各加一个小辅助；Go 在 `internal/db/agents.go`（`isPiFamilyAgent`）与 `internal/observability/agent_index_events.go`（`isPiFamilyAgentKind`）镜像。
- **注册 schema 扩为 union**：`piPresenceSessionRefSchema` 的 pi 分支接受 `agent: "pi" | "pig"`（`kind` 仍是 `path` only），`subscriberKind` 扩为 `"pi" | "pig" | "agy"`。Go 侧 `piPresenceSessionRefPiSchema` 的 `agent` literals、`SubscriberPig` 常量、`subscriberKind` literals 同步。
- **会话 hint 的跨 pi/pig 兼容**：`setSessionRefByTerminal`、`replaceForSession` 的 `sessionHint` 保留、`mapAgent` 的 `compatibleHint`，除原本的「同名 agent 才兼容」外再放行「同 pi 家族互认」。这样同一 terminal 在 pi↔pig 之间改标签时，已写入的会话 hint 不被清掉。
- **TS 扩展固定上报 `pi`；pig 身份由 Go 扩展自行上报**：`packages/herdsman-pi` 只运行在 TypeScript 版 `pi` 内，固定 `agent: "pi"` / `source: "herdr:pi"` / `subscriberKind: "pi"`；`pig` 运行的是独立仓 `pi-extensions/go/herdsman-pi`，由它自己以 `agent: "pig"` / `subscriberKind: "pig"` 注册，daemon 端 schema 与家族判定负责接收。
- **跟随家族不改变 `: "pi"` 的 event-kind 字面量**：`sameTerminalAssistantContent(..., "pi")`、`payloadFrom(event.Payload)`、`agentKind := "pi"` 这类「事件内容/格式」的 `"pi"` 是 pi-jsonl 格式标签而非 agent 身份，pig 也用同一格式，故不改。

## 被放弃的方案（必填）

- **给每个点位塞一个 `=== "pi" || === "pig"` 的裸判断**：散落的家族判断迟早再漏一处；收敛成 `isPiFamily*` 辅助让新增 agent-kind 判定点有统一的扩展方式，也把「为什么 pig 跟着 pi 走」固化在注释里。
- **只改 daemon 侧、不改 pig 侧上报**：pig 若以 `agent:"pi"` 上报时 daemon 虽能跑（家族兼容），但 `AgentIndexRecord.agent` 仍是 `pi`——pig 面板在 `agent list`/路由里就与真 pi 不可区分，违背一等公民目标，故 `pig` 必须由它自己的扩展（独立仓 `pi-extensions/go/herdsman-pi`）报真实 kind。
- **把 pig 判进「非 pi 非 agy」的第三方分支**：pig 不是「另一个要补全 reader 的新 agent」，它和 pi 共享同一个 reader/格式/信号通道；放进非 pi 分支只会把它推向「空 assistant body 门槛」「非 pi 就绪门」这条为 agy/claude 设计的路径。
- **让 `subscriberKind` 沿用 `"pi"`**：subscriberKind 不只是「是不是 pi 通道」——`#isPiTerminal` 的 socket 分支用它区分终端归属，pig 用 `"pig"` 才能让 agy 在 pig pane 上正确判断「这个终端不是我要 lease 的 agy 终端」。

## 来源

- 代码：`src/agent-history/discovery.ts`、`src/observability/interactive-pi.ts`、`src/observability/agent-index-service.ts`、`src/observability/schemas.ts`、`src/observability/contracts.ts`、`src/db/agents.ts`、`src/db/agent-events.ts`、`src/daemon/observability-server.ts`、`packages/herdsman-pi/src/index.ts`，及其 Go 镜像 `go/internal/agenthistory/discovery.go`、`go/internal/observability/interactive_pi.go`、`go/internal/observability/agent_index_events.go`、`go/internal/observability/schemas.go`、`go/internal/observability/contracts.go`、`go/internal/db/agents.go`、`go/internal/db/agent_events.go`、`go/internal/daemon/observability_server.go`。
- 前置决策：`.agents/notes/20261026-pig-session-root-whitelist.md`（pig 自有会话根白名单）。
- 测试：`test/unit/agent-history-discovery.test.ts`、`test/unit/interactive-pi.test.ts`、`test/unit/observability-contracts.test.ts`、`test/unit/herdsman-pi-extension.test.ts`、`test/integration/agent-store-terminal-identity.test.ts`、`test/integration/observability-rpc.test.ts`，及 Go 镜像 `go/internal/agenthistory/discovery_test.go`、`go/internal/observability/interactive_pi_test.go`、`go/internal/observability/contracts_test.go`、`go/internal/db/agent_store_terminal_identity_test.go`、`go/internal/daemon/observability_rpc_test.go`。

## 返修记录（C 档门禁第 2 轮 / Rework Round 2）

- **RPC 集成测试真覆盖 pig 产生方（TS + Go 镜像）**：`test/integration/observability-rpc.test.ts` 的 `treats a pig terminal like a pi terminal` 与 `go/internal/daemon/observability_rpc_test.go` 的 `TestObservabilityRpcTreatsPigTerminalLikePi` 原本让 `term_b` 以 `agent: "pi"` 入库，只在 `session.register` 里用 `agent: "pig"` 上报，`publishAgentEvent` 投递门走的是 `agents` 行 kind 而非 session hint—— `isDeliverableAgentEvent` 的 `agent.agent !== "pig"`（`src/db/agent-events.ts`）/ `agentKind != "pig"`（`go/internal/db/agent_events.go`）分支从未被 pig 产生方真覆盖。已在 `replaceForSession`/`ReplaceForSession` 新增一条 `agent: "pig"` 的派发 worker 索引记录（`term_pig_worker`），对它 `appendRoutedEvent` 并 `publishAgentEvent`，断言 owner 收到的 `agent.done` 事件携带 `payload.agent === "pig"`，真覆盖 pig 作为事件产生方的投递门。
- **大小写比较恢复精确小写语义（TS + Go 镜像）**：src/observability/agent-index-service.ts 的 isPiFamily 去掉 .toLowerCase() 改为 agent?.trim() 后精确小写比较；go/internal/observability/agent_index_events.go 的 isPiFamilyAgentKind 去掉 strings.ToLower 改为 strings.TrimSpace 后精确小写比较（保持与基线 eb8e83d 一致的精确小写匹配语义，仅保留首尾空白容错）。src/db/agents.ts 与 go/internal/db/agents.go 的大小写不敏感比较保持与基线一致、未改动。
- **统一扩展措辞并修正残留**：全仓代码注释中残留的「pi 和 pig 运行/共用同一套 herdsman-pi 扩展」表述统一改写为「emit the same pi-jsonl session format and herdsman protocol」（各自独立、产出兼容格式与协议），覆盖 `src/agent-history/discovery.ts`、`src/observability/schemas.ts`、`src/db/agents.ts`、`src/observability/agent-index-service.ts`、`test/integration/agent-store-terminal-identity.test.ts`、`test/unit/agent-history-discovery.test.ts`、`test/unit/observability-contracts.test.ts` 及其 Go 镜像 `go/internal/agenthistory/discovery.go`、`go/internal/agenthistory/discovery_test.go`、`go/internal/observability/schemas.go`、`go/internal/db/agents.go`、`go/internal/db/agent_store_terminal_identity_test.go`；旧短语 `git grep` 已 0 命中。本文件的「一句话结论」与「背景」也同步改为 TS `pi` 用本仓 `packages/herdsman-pi`、Go `pig` 用独立仓 `pi-extensions/go/herdsman-pi` 与 `pi-extensions/go/herdr-agent-state` 的准确表述。

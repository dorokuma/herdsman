---
status: active # active | superseded
superseded_by: ""
supersedes: ""
# 模块可选值: observability, daemon, cli, config, db, herdr, shared, herdsman-pi, herdsman-herdr-plugin, release
模块: "observability"
---

# 会话注册白名单接受 pig 自有会话根 `~/.pig/agent/sessions`

## 一句话结论

会话注册白名单（`safeAllowedSessionPath` / `sessionPathAllowedByShape`）的 home 相对根由「只有 `~/.pi/agent/sessions`」扩为「`~/.pi/agent/sessions` 与 `~/.pig/agent/sessions` 两个并列根」：pig（PiG）把会话放在自己的 agent home 下，同一套 herdsman-pi 扩展在 pig 里注册会话路径时若被白名单拒绝，pig pane 的会话就永远注册不上、也就无从索引。

## 背景

- 注册协议：`packages/herdsman-pi/src/index.ts` 经 `agent.orchestrator.register` 上报 `sessionRef`（`kind: "path"`），daemon 侧 `AgentIndexService.#registerPiSessionRefNow` 先过 `safeAllowedSessionPath`（文件安全闸门 + 根白名单）；不通过时，若路径已存在则**直接拒绝注册**。
- 根白名单此前只认两类根：home 相对根 `~/.pi/agent/sessions`，以及固定根 `ALLOWED_SESSION_ROOTS`（`/tmp/herdr-role-sessions`，供派发角色会话用）。
- pig（PiG）把会话写在 `<pig home>/agent/sessions`，默认即 `~/.pig/agent/sessions`。扩展在 pig 内运行时上报的路径落在 pig 的 home 下，不在 pi 的 home 下 → 被白名单拒掉 → pig pane 的会话既不注册也不索引。
- 全仓排查（`pi/agent/sessions` 命中）确认：真正的「会话根目录白名单」只有 TS `src/agent-history/discovery.ts` 与其 Go 移植 `go/internal/agenthistory/discovery.go` 两处实现（各 2 个函数），其余命中都是测试夹具、文档/归档计划、或「派发角色会话根」常量（`DISPATCHED_PI_SESSION_ROOT`，语义不同，不涉及 `~/.pi` 专属），不在本次改动范围。

## 决策

- 在 `src/agent-history/discovery.ts` 中把 home 相对根收敛成常量 `HOME_SESSION_ROOTS = [".pi/agent/sessions", ".pig/agent/sessions"]`，由 `homeSessionRoots(homeDir)` 统一派生（`homeDir` 参数 → `HOME` → `/root`），`safeAllowedSessionPath` 与 `sessionPathAllowedByShape` 共用同一份根列表，避免两处再各写一遍 home 拼接。
- 两个根**同等对待**：同一个文件安全闸门（`lstat` 判软链/非常规文件 → `realpath` → 复核属主与 `mode & 0o022`）、同一套 containment 判定（`relative(root, path)` 不得逃逸）、同一套 home 规范化。pig 根不享受任何更宽松的路径规则。
- Go 移植 `go/internal/agenthistory/discovery.go` 同步加入 `homeSessionRoots(home)`（含 `.pig/agent/sessions`），保持与 TS 实现的镜像一致，避免两套实现在同一条注册协议上给出不同结论。
- 官方值通道（`safeOfficialSessionPath`）**不动**：它本就不带根限制（官方值来自 Herdr 回显，不猜根），与本次「注册协议白名单」是两条不同路径。
- 回归测试：`test/unit/agent-history-discovery-regressions.test.ts` 覆盖「pig home 下的会话文件被接受（pi 行为不变）」与「pig 根边界：同名兄弟目录、跨 home 的越界文件一律拒绝」；Go 侧对应 `TestRegistrationWhitelistAcceptsPigSessionFilesUnderThePigHome`。

## 被放弃的方案（必填）

- **把 `~/.pig/agent/sessions` 直接加进 `ALLOWED_SESSION_ROOTS` 常量**：该常量是「与 home 无关的固定绝对根」语义（`/tmp/herdr-role-sessions`），home 相对根无法在其中表达（`HOME` 运行时才定），混进去会让常量的语义失真。
- **只改 TS、不改 Go 移植**：`go/internal/agenthistory/discovery.go` 是 TS 实现的逐行移植，两边在同一注册协议上会给出不同判定（TS 接受 / Go 拒绝），属实现分叉，必须同改。
- **按 agent 名动态推导会话根（遍历 home 找 `*/agent/sessions`）**：把「明确列出的两个 agent home」变成「凡 home 下带 agent/sessions 的都信」，扩大信任面，与白名单最小化原则相悖。
- **给官方值通道也加 pig 根限制**：官方值本就刻意不受根限制（固定根列表会静默拒绝落在各 agent 自有 home 的官方值），加限制只会制造新的漏读。

## 来源

- 代码：`src/agent-history/discovery.ts`（`HOME_SESSION_ROOTS` / `homeSessionRoots` / `safeAllowedSessionPath` / `sessionPathAllowedByShape`）、`go/internal/agenthistory/discovery.go`（`homeSessionRoots` / `SafeAllowedSessionPath` / `SessionPathAllowedByShape`）。
- 调用方：`src/observability/agent-index-service.ts`（`#registerPiSessionRefNow`）、`packages/herdsman-pi/src/index.ts`（`agent.orchestrator.register` 上报 `sessionRef`）。
- 回归测试：`test/unit/agent-history-discovery-regressions.test.ts`、`go/internal/agenthistory/discovery_test.go`。

## 审查观察项（非阻断）

本节记录红队反证与只读审查提出的 3 条理论观察项。它们**不构成阻断**，本次实现与测试不作任何改动；此处仅留痕，供后续演进时再评估。

1. **跨 home 反向断言的表意略宽**：`test/unit/agent-history-discovery-regressions.test.ts:81-87` 的跨 home 断言传入的 `outside` 文件位于外部临时目录，而不是 `home` 的 `.pig/agent/sessions` 之下，因此「越界」这一断言的表意比「pig 根内但越出 containment」更宽。不过核心正向断言（pig home 下的会话文件被接受、pi 行为不变）与同前缀兄弟目录（`sessions-backup`）的反向断言均已覆盖，不影响正确性。
2. **自定义非默认 `PIG_HOME` 不在白名单内**：白名单只认 `<home>/.pi/agent/sessions`、`<home>/.pig/agent/sessions` 两个 home 相对根与固定根 `/tmp/herdr-role-sessions`；若 pig 被配置为自定义的 `PIG_HOME`（既非 `<HOME>/.pig` 也非 `/tmp/herdr-role-sessions`），其会话根不在白名单内。这属于**有意维持最小可信根**的设计边界（不按 agent 名动态推导根，避免扩大信任面），不是缺陷；官方 `pane.report_agent_session` 通道本就不受根限制，官方值路径不受此影响。
3. **`HOME=""`（显式空串）时的相对路径回退差异**：`homeDir` 未传且环境变量 `HOME` 被显式置为空串时，TS 与 Go 两侧的相对路径回退行为存在**存量**细微差异（非本次改动引入）。常规运行环境的 `HOME` 为绝对路径，且候选文件还须通过 `safeRegularFile` 的属主与权限闸门（软链/非常规文件拒绝 + `realpath` 复核），故不构成安全风险，暂不处理。

---
status: active # active | superseded
superseded_by: ""
supersedes: ""
# 模块可选值: observability, daemon, cli, config, db, herdr, shared, herdsman-pi, herdsman-herdr-plugin, release
模块: observability
---

# Agent history 发现：只支持 pi + agy（官方值直用 / 单一模板），其余一律不解析

## 一句话结论

会话发现的输入只有 Herdr 官方值（只读官方 snake_case 字段名），支持面**就两个 agent**：

- **pi**：`kind == "path"` 的官方会话文件路径，过文件安全闸门后原样采用（这是通用 path 通道，不是 pi 专属分支）。
- **agy**：`kind == "id"` 的官方 conversation id，uuid 校验后按**唯一一个**模板拼出 `<home>/.gemini/antigravity-cli/conversations/<id>.db`，再过闸门 + store 根内校验。

**其余 agent 无论 Herdr 是否支持、是否有集成，herdsman 一律不解析**：不猜目录、不扫盘、不按 id 遍历文件名、不按 mtime/终端标题排行、不挑候选；不可解析 ⇒ 一条明确的 `console.warn`（写清 agent、官方 agent_session、拼出的候选路径）+ 空 ref，且同一份官方值**只打一条 warn、并按 60s 窗口降频**（不每轮重跑）。**强制刷新必须绕过该降频**（本批修正，见决策 1）。

这是**爸爸的裁定（2026-10-04）**：「我只用 pi 和 agy，其他 herdr 支持的我们 herdsman 不需要支持」，因此本批**主动删除**了此前为 claude / codex / opencode / grok 写的模板与 reader、以及 gemini 的死代码。**这不是能力缺失的遗漏，而是产品决定**；README 已按同一口径改写。`discovery.ts` 现 315 行（本批前约 460 行）。

## 背景

- 本仓 `src/agent-history/discovery.ts` 曾是 565 行的「本地推导层」：`scanRoot`/`listJsonlFiles` 递归扫 `~/.claude/projects`、`~/.codex/sessions`、`~/.gemini/tmp`、`~/.pi/agent/sessions`、`/tmp/herdr-role-sessions`，按 `cwd` 过滤 + `mtime` 排序 + `firstSeenAt` 宽限窗口 + 终端标题里的 `role-<id>` 目录提示 + `occupiedSessionPaths` 去重排名，选出一个 `discovered_file` ref。
- Herdr 已提供权威事实：集成经 `pane.report_agent_session` 上报会话标识，Herdr 在 `pane.get` / `pane.list` / `agent.list` / `session.snapshot` 里以 `agent_session: {agent, kind, source, value}` 回显（`herdr api schema --json` → `AgentSessionInfo`；`kind ∈ {id, path}`）。
- 关键前提（实测 + 上游源码）：Herdr **只对 `pi` / `omp` 转出 `kind: "path"`**，其余 agent 一律只给 `kind: "id"`，即使 claude / agy 的官方 hook 已经上报了 `agent_session_path` 也被 Herdr 归一化丢弃。
- 与既往规范的故意分歧：本仓此前把「本地扫盘 + mtime 排行」当兜底防线；本次**故意**取消这条防线（它是猜测，会产生静默错误数据），只保留「官方值 + 确定性模板」的确定性解析。
- 历史沿革（口径已变，勿按旧文行事）：第 7/8 批曾把 claude / codex / opencode / grok / gemini 的官方输入逐个查证并补了确定性模板；**这些实现已按 2026-10-04 的裁定整体删除**，相应叙事在本笔记中已撤除，只保留「为什么删」的解释与证据出处。

## 决策

1. **`discoverAgentHistory(input, options?)` 只吃官方值**，入参收缩为 `{agent, agentSession, herdrSessionName?, homeDir?, paneId?}`。
   - 本批**删除** `cwd` 与 `grokHome` 两个入参：只有已删的 claude / grok 模板需要「官方 id + 官方 cwd」，`grokHome` 只服务已删的 `resolveGrokHome`。负缓存 key 随之收缩为 `{agent, kind, source, value, homeDir}`。
   - `kind === "path"` → `safeOfficialSessionPath()` 文件安全闸门后原样采用。**官方路径不受白名单根限制**：官方值可能落在各 agent 自有 home/state 目录（本仓主要就是 `~/.pi/agent/sessions` 之外的路径），固定根列表会静默拒绝。
   - `kind === "id"` → **只有 agy 一个模板**：`<home>/.gemini/antigravity-cli/conversations/<id>.db`；id 必须过 `isUuidLike`（否则绝不拼进路径），候选必须在 `conversations` 根内（`isInside`），并过同一道文件安全闸门。
   - **其它 agent 一律 `miss(...)`**，且文案分两支（不能自相矛盾）：
     - 不受支持的 agent：`detail = "<agent> is not a supported herdsman history agent (only pi and agy are)"`；
     - **受支持但以 id 形态到来（pi）**：`detail = "pi reported an id; herdsman has no id-based session-file derivation for it (only agy does)"`（pi 是受支持的，只是没有 id 推导；README 专门写了这条缝）。两者都是 `unknown`/无 reader 能读，都不会回退到猜测。
   - **文件安全闸门（`safeOfficialSessionPath` / `safeRegularFile`）**：先 `lstatSync(原始路径)` 判「软链 ⇒ 拒；非常规文件 ⇒ 拒」，**再** `realpathSync`，再对 real 复核 `isFile()` / `uid === euid` / `(mode & 0o022) === 0`。顺序不能反：realpath 之后软链本身已经不存在，在解析结果上判 `isSymbolicLink()` 恒为 false（第 9 批 M1 修的就是这个死代码）。只检查**文件自身**是不是软链，父目录是软链不影响（官方值经软链目录指向的常规文件照读）。`safeAllowedSessionPath`（herdsman-pi 注册协议用的白名单版）走同一 `safeRegularFile` + 根白名单。
   - **失败降频（负缓存）与三类调用者的分工**：失败按 key 记住 60s；窗口内**不再跑 discovery、不再打 warn**（同一 key 全程只打一条），窗口过后自动重试（自愈窗口），key 里任一官方值（或 `homeDir`）变化就立刻重新尝试。降频对象**只有后台常规刷新**，另两类调用者总是重试：
     - **后台常规刷新（daemon 每轮 `refreshAgent`）**：未解析 pane **不再每轮强制 discovery** —— `shouldForceDiscovery` 对「未解析」（`directAuthoritativeRef` 为 null、`agentSession.kind === "id"`、或 `previous?.historyRef` 为 null）一律返回 `false`，于是 `discovery.ts` 的 60s 窗口真正生效（此前它恒返回 true，每轮都真跑 discovery ⇒ 窗口永不过期，抑制层被旁路，负缓存 ≈ 仅剩 warn 去重）。仍会强制的情形只剩**已解析**pane 的 identity 变化 / revision 回退 / revision 前进但 fingerprint 未变 / 源文件消失 / occupied 变化。
     - **操作员显式读取（`agent.get` / `agent.read`，RPC 层）**：传 `forceDiscovery: true` ⇒ **总是重试一次**（跳过持久 ref 复用，并绕过 service 层与 discovery 层两层失败记忆），因为显式动作必须拿新鲜结果，不能被刚刚的失败记住答案。warn 仍按 key 去重，强制重试不会多打日志。
     - **外部强制刷新（既有 `forceRefresh: true` 调用点：plan 重试 / turn completion 等）**：行为不变，继续绕过负缓存。
     - 连带删除：`AgentContextService.preferredHistoryRef`（RPC 不再复用持久 ref ⇒ 无调用方）与 `shouldForceDiscovery` 的 `preferredRef` 入参（不再需要）。
   - **`source` 匹配按整段**：`source` 形如 `herdr:<agent>`，按分隔符切段后整段匹配，`herdr:copilot` 含 "pi" 但不再被读成 pi；`herdr:omp` 也是 `unknown`。
   - **删除面**（本批）：四个 id 模板及其全部支撑代码——`codexRolloutPath` / `stateDatabases` / `querySingleRow` / `codexHome`、`resolveOpenCodeDbPath`、`claudeConfigDir`、`resolveGrokHome`、`isSafeSessionIdSegment`，以及 claude/grok/codex/opencode/gemini 的 reader 文件、`cacheSourcePathForRef` 的 `#session=` 合成 key（`cacheSourcePathForRef` 内联为 `historyRef.path ?? historyRef.value`）、reconciler 里对应的 `#session=` 探测分支。**保留** `isUuidLike`（agy 用）、`containedFileRef`、`isInside`、闸门函数、白名单版 `safeAllowedSessionPath` + `sessionPathAllowedByShape` + `ALLOWED_SESSION_ROOTS`。
   - 不解析的 agent（**含 Herdr 有官方集成的**）：claude / codex / opencode / grok / gemini / omp / copilot / devin / droid / kimi / mastracode / hermes / qodercli / qwen / kilo / cursor / letta …… 一律 warn + 空 history。
2. **reader 注册表只有两个**：`PiHistoryReader`（`pi-jsonl`）与 `AntigravityHistoryReader`（`antigravity-sqlite`）。契约里的 `AgentHistoryRef["source"]` 联合类型收缩为 `"antigravity-sqlite" | "pi-jsonl" | "unknown"`（不留死枚举；cli / db / 观测层所有引用同步收缩，typecheck 无悬挂引用）。
3. **`AgentContextService` 同步**：`historyLookup()` 只传 `{agent, agentSession, herdrSessionName, paneId}`（`cwd` / `grokHome` 传递随之删除）；`selectPreferredRef` 不接 `identityChanged`/`occupiedChanged`/`occupiedSessionPaths`；落盘过的 `discovered_file` ref **不再信任**。`kind === "id"` 且历史快照里已有**同一官方 id** 绑定过的 ref 时复用该 ref（`matchingAuthoritativeIdRef` + `bindAuthoritativeId`）＝唯一的「回落」。
4. **基础列表/状态元数据只吃官方字段名**：`session-list.ts`（`session_dir`/`socket_path`）、`db/agents.ts::replaceForSession`、`agent-index-service.ts` 的 pane overlay / `withPaneRevision` / `paneGenerationOf` / `paneGenerationFromEvent` / `#isClosedPaneAgent`、`agent-event-reconciler.ts`、`herdr-session-watch-manager.ts` 的 camelCase 兼容分支全删（`pane_generation`/`creation_id` 这类官方键之外不再有别名兜底）。`agent-index-service.ts` 里**官方** path 的 `sessionReady` 改用 `safeOfficialSessionPath`（只有 herdsman-pi 自报路径的注册口仍用白名单版）。
5. **`pane-identity-resolver` 收紧**：`pane.get` 只按官方 `pane_info` 信封解包，字段只读 snake_case。
6. **官方 `agent.list` 失败回落本地索引**：`refreshHerdrSession` 仍 reject，但不清空已索引的 base 列表。
7. **grok home 没有 pane 级来源**：Herdr 官方 `AgentInfo` 既没有 `env` 也没有 `pid`，`grokHomeForAgent`（读 pane payload 的 `env.GROK_HOME` 或 `/proc/<pid>/environ`）与 `validateGrokHome` 已删除。本批进一步删除了 `AgentIndexRecord.grokHome` 字段与 `resolveGrokHome()`；`agents.grok_home` 列作为**恒 null** 的遗留列保留（删列要带 migration，超出本批授权，且已无任何读取方）。
8. **测试面**：删掉四个模板的命中/未命中/越界/无 cwd 用例、`grok-home.test.ts`、gemini reader 用例、集成测试里的五模板断言与 `#session=` 合成 key 用例；**pi 与 agy 的覆盖保持完整**（pi path 直用 + 闸门：软链/权限/越界/父目录软链；agy uuid→db 命中与未命中 + 闸门 + 非 uuid 拒绝；未知 agent ⇒ `unknown` + warn + null；负缓存降频与「官方值变化立即重试」；新增 1 条「force 绕过负缓存」）。RPC 集成用例改为：agy 命中、pi 直读、claude/codex/gemini/opencode 四个 pane **在盘上有完整 fixture 的情况下仍报无历史**（端到端证明「不扫盘」与「有意不支持」）。

## 关键证据

**Herdr 只对 pi/omp 给 path（证据固定在 v0.9.3 tag）**：`v0.9.3` `src/agent_resume.rs::session_ref_from_report`（另有单测 `report_ref_prefers_pi_and_omp_paths_and_validates_values` 固化）：

```rust
pub fn session_ref_from_report(
    source: &str,
    agent: &str,
    agent_session_id: Option<String>,
    _agent_session_path: Option<String>,
) -> Option<AgentSessionRef> {
    if !is_official_agent_source(source, agent) {
        return None;
    }
    if agent == "pi" || agent == "omp" {
        return _agent_session_path
            .and_then(AgentSessionRef::path)
            .or_else(|| agent_session_id.and_then(AgentSessionRef::id));
    }
    agent_session_id.and_then(AgentSessionRef::id)
}
```

`v0.9.3` 的 `is_official_agent_source` 全表：`claude / codex / copilot / devin / droid / kimi / omp / mastracode / pi / hermes / opencode / qodercli / qwen / kilo / cursor / antigravity_cli(agy) / grok / letta`——**herdsman 现在只认其中的 pi 与 agy**，其余即使有官方集成也一律不解析。`AgentSessionRefKind = {id, path}`、`AgentSessionInfo = {source, agent, kind, value}`（本机 `herdr 0.9.3` 的 `herdr api schema --json`）。本机 `herdr agent list` 的 14 个 pi pane 全为 `"kind":"path"`，唯一 agy pane（`w9:pQD`）为 `{"agent":"agy","kind":"id","source":"herdr:antigravity_cli","value":"0503be55-…"}`。

**版本差异（必须写清，避免版本歧义）**：本机 `/workspace/herdr` checkout 是 **0.7.1**，它的 `is_official_agent_source` **没有** `antigravity_cli`/`agy`、也没有 `grok`（也没有 `qwen`/`letta`），`src/integration/assets/` 下也没有 `grok`/agy 资产。因此在 0.7.1 上「agy 是官方 source」这个前提**不成立**——0.7.1 的代码会让读者得出与本笔记相反的结论。**本笔记的结论以 v0.9.3 tag（本机 `herdr` 版本 0.9.3）为准，不要用 0.7.1 checkout 复核这条前提**；`session_ref_from_report` 里 pi/omp 优先取 path、其余只取 id 的写法两个版本一致。`herdr integration install` 在 0.9.3 里含 `grok` 与 `antigravity-cli`（本机 `herdr integration list`）。

**版本差异（schema）**：`pane_generation` / `creation_id` 在 **Herdr 0.9.3 schema 中出现 0 次**（`herdr api schema --json` 全量检索），因此 herdsman 的 `paneGenerationOf` 恒为 `null`、closed 事件恒走 `generation == null` 分支。这是**既有事实**（第 9 批之前即如此，非本批引入），这里记明以免后人误判为回归。

**已删除模板的历史证据（留档说明，不是支持声明）**：第 7/8 批为 claude / codex / opencode / grok / gemini 逐个查证过官方输入与本地布局——claude `~/.claude/projects/<cwd 中每个非字母数字字符替换为短横线>/<id>.jsonl`（本仓真实观察 `-Users-ryo-nakae-Dev--sandbox-herdsman-test`，外部磁盘核对 `<cwd>` 中非字母数字 → `-`）；grok `<GROK_HOME 或 ~/.grok>/sessions/<url-encoded cwd>/<uuid>/chat_history.jsonl`（`~/.grok`，观察版本 grok 1.0.25）；codex `<codex home>/state_<n>.sqlite` 的 `threads.rollout_path`（`CODEX_SQLITE_HOME` → `CODEX_HOME`，不解析 `config.toml`）；opencode `<home>/.local/share/opencode/opencode.db` 的 `session` 表；gemini 连官方 source 都不存在（Herdr 无 gemini 集成 ⇒ 官方输入为零）。**这些证据与其代码一起在本批删除**；留档只为解释删除决定、避免后人重复核对或把「删掉的实现」误当回归。

**本机实况端到端（agy）**：见下「实况验证记录」。

## 实况验证记录（本机，2026-10-03）

**agy（本机已装，走通）**——官方值来自 `herdr agent list` 的真实 pane `w9:pQD`（cwd `/root/workspace/metapi`）：

```
agent_session: {"agent":"agy","kind":"id","source":"herdr:antigravity_cli","value":"0503be55-1841-432e-b341-490e5d77d8c7"}
kind: id（agy 从不给 path）
派生 db: /root/.gemini/antigravity-cli/conversations/0503be55-1841-432e-b341-490e5d77d8c7.db
db stat: mode 600 uid 0（真实存在，同目录还有 -shm/-wal）
discovery: {"kind":"agent_session","path":"…/0503be55-….db","source":"antigravity-sqlite","value":"0503be55-…"}
service.read: messages 1, roles ["user"], text lengths [6679]（正文已脱敏，不外泄内容）
fail-closed（把同一个 pane 的 id 换成不存在的 uuid）: historyRef null + 一条 "Herdsman has no usable official Herdr agent session"
```

**失败可见性（对任意 agent 一致）**：官方值缺失、id 无推导、文件缺失、闸门不过任一项时，daemon 日志出现 `Herdsman has no usable official Herdr agent session`（含 agent、官方 agent_session、候选路径、paneId），且同一官方值只报一次、不刷屏（60s 窗口）。pi pane 仍走 `kind:"path"` 直读（本机 14 个 pane 的常态）。

## 被放弃的方案（必填）

- **恢复 cwd + mtime 排行扫盘**（`scanRoot` + `firstSeenAt` 宽限 + `role-<id>` 目录提示 + `occupiedSessionPaths` 去重）：纯猜测（同 cwd 多会话取 mtime 最新、靠宽限窗口区分新旧会话），会产生**静默错误**数据。放弃。
- **恢复「按 id 扫文件名」**（pi 的 `scanRootById`、codex/claude 的按 id 目录遍历）：命中精确但需要**目录遍历**，且遍历范围本身要引入角色目录、去重等猜测逻辑。放弃。**pi 的 `kind:"id"` 缝**（官方 pi hook 在拿不到 session file 或路径非绝对时只发 id）因此保持不解析，并在 README 与本笔记写明「该 pane 无历史」。
- **为 claude / codex / opencode / grok 保留（或补回）确定性 id 模板**：技术上可行且曾经实现、测过、端到端跑过，但按 2026-10-04 的裁定「只用 pi 与 agy」整体删除。**不要因为「代码本来能跑」而复原**——这是产品决定，不是技术判断。
- **为 gemini 扫盘 / 保留 `GeminiHistoryReader`**：Herdr 无 gemini source ⇒ 官方输入为零，reader 没有任何输入来源，属死代码。2026-10-03 已签字「不接受扫盘、README 如实」，2026-10-04 进一步删除该 reader 与 `gemini-json` 映射。
- **用 `foreground_cwd` 或 `cwd ?? foreground_cwd` 参与模板**：第二字段会拼出**另一个**项目目录（静默错误方向）而不是读不到。放弃；现存的 agy 模板也不需要 cwd。
- **把官方 path 限制在固定白名单根内**（旧 `safeAllowedSessionPath` 语义）：会静默拒绝落在自有 home 里的官方路径。放弃，改为文件安全属性校验 + 候选必须在 store 根内。
- **保留 camelCase 字段兼容分支**：会在 Herdr 改字段名时把契约漂移变成静默兼容。放弃。
- **pane 级 `GROK_HOME` 富化（读 pane payload `env`/`pid`）**：官方 `AgentInfo` 没有这两个字段，等于是给不存在的输入写逻辑。放弃（代码已删）。

## 来源

- 本轮改动：`src/agent-history/discovery.ts`、`src/agent-history/service.ts`、`src/observability/contracts.ts`、`src/observability/agent-context-service.ts`（含 `shouldForceDiscovery` 语义与 `preferredHistoryRef` 删除）、`src/observability/agent-index-service.ts`、`src/daemon/observability-server.ts`（RPC `agent.get` / `agent.read` 显式读取接线）、`src/db/agents.ts`、`src/daemon/agent-event-reconciler.ts`、`README.md` 能力声明段；删除 `src/agent-history/{claude,codex,gemini,grok,opencode}-reader.ts` 与 `test/unit/grok-home.test.ts`。
- Herdr：**v0.9.3 tag** `src/agent_resume.rs`（`session_ref_from_report` / `is_official_agent_source` / 其单测）；本机 `herdr 0.9.3` 的 `herdr agent list` / `herdr integration list` / `herdr api schema --json`；本机 `/workspace/herdr`（**0.7.1**，仅用于对比版本差异，不作为结论依据）。
- 门禁：`pnpm check`（typecheck / 52 files · 846 tests / Biome / Drizzle / root package / pi package / herdr plugin 全绿）+ `pnpm build`。

## 遗留 / 观察项

1. **支持面 = pi + agy**（2026-10-04 爸爸裁定）：pi = 官方 path 直用；agy = 官方 id + 单一模板；其余 agent（含 Herdr 官方支持的 claude / codex / opencode / grok 等）**明确不支持**，一律 `warn`（含 agent、官方值、候选路径）+ 空 history，不崩溃、不静默错。
2. **闸门是 check-then-open 的 TOCTOU 残余（oracle 指出，本批如实记录、不改代码）**：`lstatSync` / `realpathSync` 与后续读取之间存在窗口——realpath 之后目标文件可被替换；且闸门只查**文件自身**的 mode/属主，**不检查父目录可写性**（父目录被他人改写 ⇒ 文件本身可被换掉）。在「会话目录属主即用户本人、同机不存在更低权限攻击者」的模型下没有可行攻击者，因此按现状保留；若将来要真正关掉这个窗口，需要 `O_NOFOLLOW` 打开 + `fstat` 同一 fd 的读法，属于接口级改动。
3. **`pane_generation` / `creation_id` 在 Herdr 0.9.3 schema 中 0 次出现**：`paneGenerationOf` 恒 `null`、closed 事件恒走 `generation == null` 分支——既有事实，非本批引入。
4. **omp 的缺口（本批未动，待裁）**：Herdr 对 omp 也给 `kind:"path"`，但 herdsman 没有 `herdr:omp` 源映射（`historySourceFromSessionRef` 返回 `unknown`，无 reader 能读）⇒ omp pane 实际报「无历史」。omp 是**唯一「官方给了 path、本可直读」却被排除**的 agent；其余本机官方列表里的 id-only agent（copilot / devin / droid / kimi / mastracode / hermes / qodercli / qwen / kilo / cursor / letta）同样无 reader ⇒ 报无历史。README 已不再声称 omp 可读；要不要接 omp（或 pi 等价 reader）请你裁。
5. **`agents.grok_home` 列仍在 schema（恒 null）**：删列需要 migration，超出本批授权；代码侧已无读取方（`AgentIndexRecord.grokHome` 与 `resolveGrokHome` 均已删除），写入值恒为常量 `null`。
6. **`occupiedSessionPaths` 语义已失效但代码保留**：仍被 `agent-index-service.ts` 的 `occupancyConflict` 脏检查引用，但 discovery 已不再「占用」文件；未删以免牵动 pi 注册协议。同一官方路径被两个 pane 共用时该检查恒真，会产生冗余重读（不影响正确性）。
7. **负缓存的两个已知取舍**：窗口内（60s）同一个失败 key 在**后台刷新**里不会重新尝试，因此「pane 先报 id、会话文件几秒后才落盘」的场景要么等窗口自然过期，要么由操作员显式读取（`agent.get` / `agent.read`，总是重试一次）立即拿到；warn 只在该 key 首次失败时出现，后续重试（含强制重试）失败不再打日志（要更细的观察需临时调 `DISCOVERY_RETRY_AFTER_FAILURE_MS`）。
8. **第 9 批遗留条目已闭环**：「`GeminiHistoryReader` 是待清理死代码」——本批删除 reader 文件、`gemini-json` 映射、注册项、类型联合成员与相关用例，条目关闭。
9. **测试面净变化**：53 files / 861 tests（第 9 批）→ 52 files / 845 tests（第 10 批：删掉的全部是被测代码已不存在的用例）→ **52 files / 846 tests**（第 11 批：新增 1 条 RPC 级「窗口内修好文件后 `agent.read` 能取到历史」，并把 4 条「未解析 pane 也 `forceDiscovery: true`」的旧预期改为 `false`）。pi 与 agy 的覆盖未削弱。
10. **`discoverySuppressed` 的降频只在后台刷新生效**（本批口径：见决策 1 的三类调用者）：若观察到的现象是「同一个失败 key 在 60s 内反复真跑 discovery」，那说明有第三个调用者没走 RPC 也不走后台刷新（比如直调 `resolveCompactHistory` 且带 `forceDiscovery`），需逐调用点核对；正常的三条路径下不应出现。

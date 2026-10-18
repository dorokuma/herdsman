---
status: active # active | superseded
superseded_by: ""
supersedes: ""
# 模块可选值: observability, daemon, cli, config, db, herdr, shared, herdsman-pi, herdsman-herdr-plugin, release
模块: observability
---

# Agent history 发现：agy 双 HOME（主 HOME + 额外 HOME）会话库解析

## 一句话结论

`src/agent-history/discovery.ts` 为 `agy`（`kind === "id"`）扩展多 HOME 确定性查找：按「主 `HOME` 优先 → 额外 HOME 列表」顺序逐个拼接 `<home>/.gemini/antigravity-cli/conversations/<id>.db`，每个候选都完整执行 `containedFileRef`（`safeOfficialSessionPath` 文件安全闸门 + `isInside` 根内校验），命中结果按 `realpath` 去重并保持主 HOME（Ultra）优先；额外 HOME 列表纳入 `discoveryLookupKey` 负缓存维度。

## 背景

- 本机运行两套 `agy` 订阅分流：主力 Ultra 槽使用默认 `HOME=/root`，Pro 槽通过 wrapper 以独立 `HOME=/root/.agy-pro-home` 启动，凭据与状态目录彼此隔离。
- 早期过渡态下 `/root/.agy-pro-home/.gemini/antigravity-cli/conversations` 曾软链回 `/root/.gemini/antigravity-cli/conversations`；为实现会话库完全物理隔离，需要将该软链拆为独立目录。
- 拆链后，Pro 槽产生的 `<id>.db` 只落在 `/root/.agy-pro-home/.gemini/antigravity-cli/conversations/` 下；而 `herdsman.service` daemon 以 `HOME=/root` 常驻运行，且 Herdr 对 `agy` 只上报 `kind: "id"`（不含路径或进程 `HOME`）。若 `discovery.ts` 只查单一主 `HOME`，拆链后所有 Pro 会话都会解析失败并落入负缓存告警。

## 决策

1. **额外 HOME 来源三级优先级（`resolveAgyExtraHomes`）**：
   - 第一优先级：调用方显式传入 `extraHomeDirs?: readonly string[]`（主要供单测或编程式调用注入，不依赖进程全局环境）；
   - 第二优先级：环境变量 `AGY_EXTRA_HOMES`（按 `:` 分隔，各段 `trim` 后过滤空串）；
   - 第三优先级：常量 `DEFAULT_AGY_EXTRA_HOMES = ["/root/.agy-pro-home"] as const`。
2. **把机器特定路径 `/root/.agy-pro-home` 写死为发布包默认值的取舍与逃生门**：
   - **取舍背景**：`@dorokuma/herdsman` 通过 npm registry 发布并由本机 `systemd` 单元直接安装运行。把 `/root/.agy-pro-home` 作为 `DEFAULT_AGY_EXTRA_HOMES` 默认值，可使生产 `herdsman.service` 无需修改单元 `Environment=` 即可在升级后开箱支持 Ultra/Pro 双 HOME 拆链。
   - **代价与边界**：在其它机器或非双 HOME 环境中，`/root/.agy-pro-home/.gemini/antigravity-cli/conversations/<id>.db` 在主 `HOME` 未命中时会多做一次确定性 `lstatSync` 探测并静默未命中（不扫盘、不抛错、不影响主 `HOME` 命中路径）。
   - **逃生门**：当环境变量显式设为空串 `AGY_EXTRA_HOMES=""` 时，`envValue !== undefined` 分支生效，切分过滤后返回 `[]`，可整体关闭 `DEFAULT_AGY_EXTRA_HOMES` 默认值（无需改代码或重发包）。
3. **主 HOME 优先与 `realpath` 去重（`resolveAgyHomeDirs` + `resolveAgySessionCandidates`）**：
   - 候选 HOME 顺序固定为 `[primary, ...extraHomes]`，先按 `normalize(candidate)` 对 HOME 目录本身去重（主 `HOME` 恒排第一）；
   - 对每个候选 HOME 导出的会话文件通过 `containedFileRef` 校验后，再按解析出的 `ref.path`（即 `realpathSync` 结果）去重。
   - 这样既兼容拆链前 `conversations` 目录软链到同一物理目录的过渡态（不会返回重复候选），又保证拆链后若极端情况下两边出现同名 uuid 时固定以主 `HOME`（Ultra）优先。
4. **安全闸门逐 HOME 完整复用、不弱化**：
   - 每个额外 HOME 派生出的 `<home>/.gemini/antigravity-cli/conversations/<id>.db` 都走同一个 `containedFileRef`：先过 `isUuidLike(id)` 防止路径穿越，再过 `safeOfficialSessionPath`（`lstatSync` 拒文件自身软链 → `realpathSync` → 复核常规文件、`uid === CURRENT_EUID`、`(mode & 0o022) === 0`），最后过 `isInside(dir, real)` 确保解析后仍在对应 HOME 的 `conversations` 根内。
5. **负缓存 key 纳入额外 HOME 维度（`discoveryLookupKey`）**：
   - `discoveryLookupKey` 末尾拼入 `resolveAgyExtraHomes(input.extraHomeDirs).join(":")`。同一会话 id 在无额外 HOME 配置下未命中进入 60s 负缓存后，一旦通过 `extraHomeDirs` 或 `AGY_EXTRA_HOMES` 配置了新 HOME，key 立即变化、在 TTL 窗口内无需 `force` 即可解析成功。

## 被放弃的方案（必填）

- **让 Herdr 上报 agy 会话的完整路径（`kind: "path"`）或进程 `HOME`**：Herdr 上游 `session_ref_from_report` 对非 `pi`/`omp` 一律只保留 `kind: "id"`，且 `AgentInfo` 不含进程环境变量；跨仓改 Herdr 协议面大且远水救不了近火。放弃。
- **不设代码默认值，完全依赖修改 `/etc/systemd/system/herdsman.service` 注入 `AGY_EXTRA_HOMES`**：虽然纯配置化更干净，但每次部署/迁移环境都要同步维护 systemd drop-in，一旦漏配会导致 Pro 会话静默丢失历史；采用「代码内置默认 + `AGY_EXTRA_HOMES=""` 逃生门」兼顾本机零配置开箱与外部可关闭性。
- **在 `/root` 下递归扫描所有 `.gemini/antigravity-cli/conversations`**：违反 `20261003-official-only-agent-session-discovery.md` 确立的「不扫盘、只用官方 id + 确定性模板」铁律。放弃。

## 来源

- 代码实现：`src/agent-history/discovery.ts`（`DEFAULT_AGY_EXTRA_HOMES`、`resolveAgyExtraHomes`、`resolveAgyHomeDirs`、`discoveryLookupKey`、`resolveAgySessionCandidates`）。
- 单元测试：`test/unit/agent-history-discovery.test.ts`（双 HOME 跨目录命中与缺失容忍、软链 conversations `realpath` 去重与 Ultra 优先、额外 HOME 文件安全闸门、`AGY_EXTRA_HOMES`/`extraHomeDirs` 变更使负缓存立即失效）。
- 关联前序笔记：`.agents/notes/20261003-official-only-agent-session-discovery.md`。

## 遗留 / 观察项

以下 6 条为本轮门禁审查（reviewer 首轮 + 复审）产生的 `consider` 级非阻塞观察项，留痕登记供后续迭代参考：

1. **`discoveryLookupKey` 额外 HOME 列表分隔符碰撞边缘**：`src/agent-history/discovery.ts` 中 `discoveryLookupKey`（L96）使用 `resolveAgyExtraHomes(input.extraHomeDirs).join(":")` 拼接额外 HOME 列表，与环境变量 `AGY_EXTRA_HOMES` 的 `":"` 分隔语义在调用方显式通过 `extraHomeDirs` 注入含冒号目录路径时存在理论碰撞（如 `["/a:b"]` 与 `["/a", "b"]` 产生相同子串，极边缘）；后续可改为 `join("\u0000")` 或长度前缀编码彻底消除歧义。
2. **`resolveAgySessionCandidates` 主 HOME 命中后的循环策略**：当前 `resolveAgySessionCandidates`（L201-L220）遍历全部候选 HOME 并收集去重后的完整 `refs` 列表，而生产调用方 `resolveSessionFileById`（L241）仅消费 `resolved.refs[0]`（其余仅被单测断言去重行为时读取）。后续若无需保留完整多候选诊断语义，可在首个合法命中后提前 `break` 省去对后续 HOME 的 `lstatSync`，或在函数注释中明确声明保留全量 `refs` 供诊断/测试观测。
3. **`extraHomeDirs: []` 与空 `HOME` 两个边缘语义的注释及断言加固**：① 显式传入 `extraHomeDirs: []` 时 `extraHomeDirs !== undefined` 为真，会同时压掉 `AGY_EXTRA_HOMES` 环境变量与 `DEFAULT_AGY_EXTRA_HOMES` 默认值（返回空额外列表）；② 当 `input.homeDir` 与 `process.env.HOME` 均为空串时，`resolveAgyHomeDirs` 会跳过空的 `primary` 并仅落到额外 HOME（默认 `["/root/.agy-pro-home"]`），仅当主 HOME 与额外 HOME 全部为空时才兜底推入 `[""]`。后续可在 `resolveAgyExtraHomes` / `resolveAgyHomeDirs` 补齐文档注释并增加显式单测断言。
4. **辅助函数导出面收敛或契约标注**：`resolveAgyExtraHomes` 与 `resolveAgyHomeDirs` 当前在 `src/agent-history/discovery.ts` 中 `export`，但仓内其它模块与现有单测均未直接 import 二者（单测仅直接调用 `resolveAgySessionCandidates` 与 `discoverAgentHistory`）。后续可收敛为模块内部私有函数，或在导出处注释标明作为公共辅助接口保留。
5. **负缓存失效单测的时钟确定性加固**：`test/unit/agent-history-discovery.test.ts` 新增用例 `invalidates negative cache when AGY_EXTRA_HOMES or extraHomeDirs changes within TTL`（L475-L509）的阶段 2 依赖测试在 `DISCOVERY_RETRY_AFTER_FAILURE_MS`（60s）真实挂钟窗口内执行完毕来证明「未过 TTL 即因 key 变化重解析」，未冻结 `Date.now()`。虽然单测实际耗时仅毫秒级，后续可通过 `vi.useFakeTimers()` / `vi.setSystemTime()` 或导出 TTL 常量断言进一步加固时间确定性。
6. **工作树并存未提交变更的拆分归属**：本分支工作树同时存在其它任务的未提交变更（`.agents/notes/` 旧笔记清理批、`packages/herdsman-pi/src/{daemon-client,index,wake}.ts`、`test/integration/agent-event-delivery-lifecycle.test.ts`、`test/unit/herdsman-pi-{extension,wake}.test.ts`）。本次提交严格按文件粒度仅暂存并提交本特性的 3 个文件（`src/agent-history/discovery.ts`、`test/unit/agent-history-discovery.test.ts`、`.agents/notes/20261018-agy-dual-home-discovery.md`），其余未提交变更保持原状交由对应任务后续独立处置。

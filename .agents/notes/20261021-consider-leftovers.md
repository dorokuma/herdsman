---
status: active # active | superseded
superseded_by: ""
supersedes: ""
# 模块可选值: observability, daemon, cli, config, db, herdr, shared, herdsman-pi, herdsman-herdr-plugin, release
模块: release
---

# 2026-10-21 A 档门禁审查 consider 留痕（CHANGELOG 0.14.9 测试计数可追溯性）

## 一句话结论

登记 `docs/release-0.14.9-and-archive-plan` 分支 2026-10-21 A 档门禁审查通过后尚未闭环的 1 条 `consider` 观察项（`CHANGELOG.md:7` `0.14.9` 小节中 `53 files / 871 tests` 仓库内无第二来源），明确不阻断理由与后续处置条件。

## 背景

在 `docs/release-0.14.9-and-archive-plan` 工作分支对 `CHANGELOG.md`（`0.14.9` / `0.14.8` 小节）、`.agents/notes/20261018-release-0.14.9.md` 及归档计划文档执行 2026-10-21 A 档门禁审查时，审查结论为通过，同时提出 1 条非阻断 `consider` 项。按门禁纪律新建本笔记逐条留痕。

## 决策（consider 逐条留痕）

### consider 1：`CHANGELOG.md:7`（`0.14.9` 小节）`871 tests` 在仓库内无第二来源

- **出处**：`CHANGELOG.md:7`（`0.14.9` 小节「测试（`473c37c`）」末尾「全量门禁 `pnpm check` 绿（53 files / 871 tests）」），对比 `.agents/notes/20261018-release-0.14.7.md:72` 与 `.agents/notes/20261018-release-0.14.9.md`。
- **问题**：「全量门禁 `pnpm check` 绿（53 files / 871 tests）」中 `871` 在仓库内无第二来源：`.agents/notes/` 全文检索无 `871 tests` 计数类记录（仓内存在的 `871` 子串均为非测试计数，如 `+31.871s` 时延与 Release id `416287180` 等），最近一档可比记载为 `0.14.7` 的 `53 files / 861 tests`（`20261018-release-0.14.7.md:72`）；`871` 与 `v0.14.7..v0.14.9` 区间（含 `473c37c` 新增水位屏障、跨批代际完成证据及插件入口集成测试）的测试增量量级相符，但在仓内笔记中属无独立交叉佐证的自述数字。
- **建议**：后续在 release note（如 `.agents/notes/20261018-release-0.14.9.md` 或后续版本发布笔记）中补记该次门禁命令与产物摘要，或改为引用既有记录口径。
- **不阻断理由**：属纯文档陈述、无运行时或代码行为影响，且 A 档门禁审查不要求为文档修补分支复跑全量 `pnpm check`。
- **后续处置条件**：在下一次执行发布或跑全量门禁 `pnpm check` 的任务轮次中，于对应 release note 落盘完整门禁输出（files / tests 计数与提交 SHA），或在对 `20261018-release-0.14.9.md` 做补充修订时补记实测门禁命令与产物证据。

## 被放弃的方案（必填）

- **方案 A**：在本次纯文档 A 档门禁阶段临时重跑全量 `pnpm check` 并改写待审 `CHANGELOG.md` / `20261018-release-0.14.9.md`。放弃理由：A 档纯文档改动不要求复跑全量测试门禁，且 `871` 与 `v0.14.7..v0.14.9` 新增用例量级吻合，单独留痕待下轮门禁实测交叉印证即可。
- **方案 B**：直接从 `CHANGELOG.md` 删除测试文件与用例数记载。放弃理由：削弱发布日志的信息密度，不如保留记载并通过本笔记说明口径来源与后续补证条件。

## 来源

- **审查来源**：主代理组织的 2026-10-21 A 档门禁审查结论（分支 `docs/release-0.14.9-and-archive-plan`）。
- **知情状态**：用户已过目。

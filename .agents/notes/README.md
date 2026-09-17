# 决策与踩坑笔记 (.agents/notes)

本目录用于沉淀 Herdsman 架构决策、设计考量、关键选型与踩坑/workaround 记录。

## 触发条件（满足任一即写）

1. **SQLite schema / 索引格式 / 配置格式 / 对外 API 契约** 变更或设计。
2. **跨两个以上模块或跨仓库**（如 daemon、Pi extension、Herdr plugin、SQLite store 之间的协议与交互）。
3. **否决看似更优方案**（记录为何不采用某种看似更好的设计）。
4. **临时降级 / workaround / 特判**（如应对特定 agent、环境或竞态的特殊处理）。
5. **与 upstream 的故意分歧**（与 upstream `ryonakae/herdsman` 或 upstream 规范的有意识差异）。
6. **性能取值原因**（如轮询间隔、缓存上限、超时时间、tail window 大小等 magic number 的设定依据）。

## 豁免清单

以下情况无需新建笔记：
- 版本 bump
- 纯文案调整 / 拼写修复
- 行为不变的单文件 bug 修复
- 小版本依赖升级
- 纯补测试用例

> **重要**：**触发优先于豁免**。如果是单文件的临时降级/workaround/特判，即使改动仅限单文件也**必须记录**。

## 规范与流程

1. **命名规范**：文件命名采用 `YYYYMMDD-slug.md`（如 `20260917-sqlite-wal-fingerprint.md`）。
2. **模板引用**：请参考模板 [_template.md](_template.md) 编写。
3. **不可变原则**：历史笔记原则上不改写；若被新决策取代，仅在旧笔记 frontmatter 中设置 `status: superseded` 与 `superseded_by`，并在正文头部添加指向新笔记的链接。
4. **索引刷新**：添加或修改笔记后，运行 `scripts/notes-index.sh` 刷新本地索引 `INDEX.md`（该文件已加入 `.gitignore`，不提交到 git）。

<div align="center">

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="./assets/logo-dark.svg">
  <source media="(prefers-color-scheme: light)" srcset="./assets/logo-light.svg">
  <img src="./assets/logo-light.svg" alt="herdsman logo" width="64" height="64">
</picture>

# herdsman

**面向多 AI 编程助手的历史可观测守护进程（已归档）——直接读取 `pi`、`agy`、`claude` 的结构化会话账本而非抓取终端滚屏，现已由 `herdr-core`（`hc`）取代。**

[![Status: Archived](https://img.shields.io/badge/Status-Archived-71717a?style=flat-square&labelColor=18181b)](https://github.com/dorokuma/herdsman)
[![Superseded by herdr-core (hc)](https://img.shields.io/badge/Superseded%20by-herdr--core%20(hc)-64748b?style=flat-square&labelColor=18181b)](#迁移指南)
[![License: MIT](https://img.shields.io/badge/License-MIT-52525b?style=flat-square&labelColor=18181b)](./LICENSE)

[迁移指南](#迁移指南) · [设计亮点](#历史核心设计) · [历史参考](#历史架构参考) · [许可证](#许可证)

[English](./README.md) · **简体中文**

</div>

> [!NOTE]
>
> **已归档 —— 2026-11-11。** 本仓库已冻结为只读历史参考，不再继续开发，也不会再发布新版本。日常使用请迁移至 `herdr-core`，其 `hc` 命令已取代 herdsman 的全部能力（`hc ls` / `hc get` / `hc wait` / `hc read` / `hc exec`）。下文保留的安装与运行说明仅为历史记录，请勿用于搭建新系统。

## 迁移指南

日常使用从旧的 `herdsman` CLI 迁移至 `herdr-core` 的 `hc`：

| 旧命令（herdsman） | 新命令（`hc`） | 作用 |
| --- | --- | --- |
| `herdsman agent list [--json]` | `hc ls` | 列出 agent 及其最新缓存状态 |
| `herdsman agent get <name>` | `hc get <name>` | 查看单个 agent 的元数据与压缩历史 |
| `herdsman agent read <name> --limit N` | `hc read <name>` | 读取结构化会话账本消息 |
| 轮询 `agent list` 等待结果 | `hc wait <name>` | 阻塞等待 agent 完成或产出结果 |
| Pi skill / `/herdsman` 所有者通知 | `hc exec <cmd>` | 直接派发并观察 agent 运行 |

## 历史核心设计

Herdsman 是运行在 Herdr 上的 coding agent 可观测守护进程，核心由四个设计支撑：

1. **读账本，不刮屏幕。** 不抓终端滚屏，而是直接解析各类 agent 自带的会话存储——`pi` 会话文件、`agy` 会话 SQLite 数据库、`claude` JSONL 转录——调用方拿到结构化消息摘要与工具结果，无需解析终端输出。
2. **持久化事件索引。** SQLite 事件索引配合游标 ack 状态机，追踪哪些 `agent.*` 事件已送达并收敛，通知在重启后不丢。
3. **不打扰的唤醒。** agent 结束时若检测到用户正在输入（半成品草稿），自动延迟唤醒；上游瞬时错误（`429`、`rate_limit_error`、`ECONNRESET`）被静默过滤，不拿噪音打扰用户。
4. **纯 Go，零 Node 依赖。** 两个 Go 小二进制（`herdsman` CLI + `herdsman-daemon`）通过带 peer credential 校验的 Unix socket 通信，完全不需要 Node.js 运行时。

## 历史架构参考

<details>
<summary><b>CLI 用法（历史 —— 已由 <code>hc</code> 取代）</b></summary>

- `herdsman agent list`：返回 daemon 对所选工作区最新缓存的 agent 状态与最近 user/assistant 摘要。
- `herdsman agent get <target>`：显式查询单个 agent 的元数据、压缩历史与最新工具结果。
- `herdsman agent read <target> --limit N`：显式读取最近 N 条 user/assistant/`tool_result` 消息。

```bash
herdsman agent list --json
herdsman agent get reviewer --json
herdsman agent read reviewer --limit 20 --json
# 在 Herdr 之外需指定范围：
herdsman agent list --all --json
herdsman agent get reviewer --workspace wB --json
```

`<target>` 依次匹配 pane id、terminal id、Herdr live name 或唯一 agent kind；跨会话歧义时用 `--session <name>` 消歧。

</details>

<details>
<summary><b>架构要点</b></summary>

- **只用官方标识解析会话。** 每个 pane 的会话历史仅由 Herdr 上报的标识解析——绝不扫描 agent 目录或按 mtime 猜测。`pi` pane 直接上报会话文件路径；`agy` pane 上报会话 id，解析为 `<home>/.gemini/antigravity-cli/conversations/<id>.db`；`claude` 会话经 `herdsman register-claude` 注册 id，解析为 `<root>/<project-dir>/<id>.jsonl`。其它 agent 一律刻意不上报历史。
- **daemon + CLI 经 Unix socket。** daemon 监控运行中的 Herdr 会话（60 秒重扫），在 `~/.herdsman` 维护持久 agent 索引，通过带 peer credential 校验的 Unix socket 为 CLI 提供服务。
- **所有者作用域通知。** 每个 Herdr 会话可有一个 Pi 终端用 `/herdsman on` 认领所有权，接收缓存的 agent 上下文、待处理计数与自动唤醒；无主期间产生的结果不被事后重放。
- **工具链。** `go/` 下纯 Go 模块（Go 1.26），`make check` 为唯一门禁（gofmt、`go vet`、`go test -race`、两个构建目标）。DB schema 位于 `go/internal/db/schema.go`，配套迁移在 `go/internal/db/migrations/` 下重放校验。

</details>

<details>
<summary><b>从源码构建（历史）</b></summary>

```bash
git clone https://github.com/dorokuma/herdsman.git
cd herdsman/go
go build -o /tmp/herdsman ./cmd/herdsman
go build -o /tmp/herdsman-daemon ./cmd/herdsman-daemon
```

daemon 监控运行中的 Herdr 会话，运行时状态存放于 `~/.herdsman`（可用 `HERDSMAN_HOME` 覆盖）。以上步骤仅作考古留存——项目已归档，请改用 `herdr-core` 的 `hc`。

</details>

更深入的设计上下文见 [AGENTS.md](./AGENTS.md) 与 [docs/plans/](./docs/plans/)。

## 许可证

[MIT](./LICENSE)

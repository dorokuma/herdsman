# AGENTS.md

Herdsman 是一个 Go daemon / CLI，从 Herdr 管理的 coding agent 生成 agent snapshot、`agent.*` event 和 orchestrator notification。请先用 `README.md` 确认用法，只有在需要判断规格时才看 `docs/plans/`。

## 铁律 / Iron Rules

1. **构建・测试・静态检查**: 修改实现后必须在 `go/` 下通过 `gofmt -l .`（为空）、`go vet ./...`、`go test ./...`、`go build ./cmd/herdsman` 与 `go build ./cmd/herdsman-daemon`；这也是 `.husky/pre-commit` 的门禁。
2. **提交规范**——commit message 须过全局 commit-msg hook：Conventional Commits 类型白名单、≤72 字、冒号后一空格、禁噪声词与密钥。
3. **决策/踩坑须记 .agents/notes/**：若本机维护了 `.agents/notes/` 目录（本机可选留存、不入 git、不随公开仓库分发），重大决策、架构调整、方案否决、临时降级/workaround/特判必须记入其中（满足六触发之一即写，触发条件见本机 `.agents/notes/README.md`）；没有该目录的主机可跳过本条。
4. **未提交变更保护**: 不得回退用户或其他进程的未提交变更。`node_modules/`、`dist/`、`*.sqlite` 不要 commit。

## 本机运维 / Local Operations

> **zero-node**：本机没有系统/通用 Node 运行时，仓库自 `chore/go-only-and-fix-wake` 起为纯 Go——没有 `package.json`、没有 `src/`、没有 `packages/`。生产 CLI 与 daemon 是 Go 二进制 `/root/.local/lib/herdsman-go/{herdsman,herdsman-daemon}`。工具链只有 Go（当前 `go1.26.4`）。

- 正式服务是 systemd 单元 `herdsman.service`（`/etc/systemd/system/herdsman.service`，`enabled`；`ExecStart=/root/.local/lib/herdsman-go/herdsman-daemon` 直连 Go 二进制）。
- 启停用 `systemctl restart|stop|start herdsman.service`。判断进程归属看 `systemctl status herdsman.service` 与 `/proc/<MainPID>/cgroup`（`pgrep -af herdsman-daemon` 会误报）。
- daemon 常驻、不在前台交互；进程异常退出（非零退出码，或被信号杀死且不属 SIGHUP/SIGINT/SIGTERM/SIGPIPE）时由 systemd 拉回（本机单元当前为 `Restart=on-failure`，`RestartSec=5` 约 5 秒后重试，连续失败受 `StartLimit*` 限制）；`systemctl stop` 是显式停止，不会被拉回。
- 部署：本机生产面为 Go 二进制——在 `go/` 下 `go build -o <tmp> ./cmd/herdsman-daemon` 编译 → 备份现网二进制为同目录 `*.bak-<时间戳>` → 替换 `/root/.local/lib/herdsman-go/herdsman-daemon`（CLI 同理换 `herdsman`）→ `systemctl restart herdsman.service` → 按下方「部署后核对」核对：MainPID / socket（daemon status）/ `agent list` / 优雅关停 journal。部署只换二进制 → 重启 → 部署后核对。
- 生产日志看 `journalctl -u herdsman.service`。`logs/herdsman.log` 是旧的，不用它判断生产状态。
- 部署后核对：**版本**以源码事实为准——读 `go/` 目录对应代码比对部署意图（Go 二进制无 `--version`）；**MainPID** 看 `systemctl show -p MainPID herdsman.service`；**socket / daemon status** 看 `herdsman daemon status`（`/root/.local/bin/herdsman` wrapper 直接 exec Go 二进制）；**`agent list` 端到端**跑 `herdsman agent list`；**优雅关停**看 `journalctl -u herdsman.service`，确认 `shutdown finished`、`exitCode: 0`、无 SIGKILL 与超时。
- 回滚 = 用同目录 `*.bak-<时间戳>` 备份二进制换回原文件 → 重启 → 按「部署后核对」核对。
- 默认数据目录 `~/.herdsman` 即生产；开发/验证请显式指定临时目录（`HERDSMAN_HOME=/tmp/<名字>`），不要拿生产目录做实验。

## 文档・索引 / Documentation Index

- 现状文档: [README.md](README.md), [docs/plans/](docs/plans/), [docs/releasing.md](docs/releasing.md)
- 决策・踩坑笔记: `.agents/notes/`（本机可选留存、不入 git、不随公开仓库分发；规约见本机 `.agents/notes/README.md`）
- 写完笔记刷新索引：scripts/notes-index.sh（本地生成 INDEX.md，不入 git；仅在本机存在 `.agents/notes/` 时适用）

## 关联仓库 / Related Repositories

- `pi-cache-guardian`: 同为 Pi 生态插件
- `herdsman-wt`: 本仓 worktree 挂载目录

## 常用命令

以下命令都在仓库根的 `go/` 目录下执行：

- `gofmt -l .`: 列出未格式化文件，必须为空（修复用 `gofmt -w <file>`）。
- `go vet ./...`: 静态检查。
- `go test ./...`: 全量单测与集成测试。
- `go build -o /tmp/herdsman ./cmd/herdsman`: 编译 CLI 二进制。
- `go build -o /tmp/herdsman-daemon ./cmd/herdsman-daemon`: 编译 daemon 二进制。
- `HERDSMAN_HOME=/tmp/herdsman-smoke /tmp/herdsman-daemon`: 用临时数据目录前台起开发 daemon（随后 `/tmp/herdsman daemon status` 验证）。

## 验证步骤

- 修改实现后必须通过 `gofmt -l .` 为空、`go vet ./...`、`go test ./...`。
- 涉及 CLI/daemon entrypoint 的改动还要 `go build` 两个 `cmd` 目标并做临时 `HERDSMAN_HOME` 冒烟。
- 改了 DB schema 相关行为时确认 `go/internal/db/schema.go` 的 `SchemaStatements` 与 `go/internal/db/migrations/` 的迁移重放保持一致——`sqlite_migrations_test.go` 会强制比对。
- 本机 PATH 上无 node/pnpm/npm/bun；工具链只有 Go，无需再为运行时前置 PATH。

本机（生产机）运行时事实，照它选运行时，不要对着版本号猜：

- 生产 CLI / daemon：Go 二进制 `/root/.local/lib/herdsman-go/{herdsman,herdsman-daemon}`，daemon 由 `herdsman.service` 的 `ExecStart` 直连；无 Node 参与。
- 提交门禁：`.husky/pre-commit` 是纯 POSIX `sh`，进入 `go/` 后跑 `gofmt -l .`（为空）、`go vet ./...`、`go test ./...`、`go build ./cmd/herdsman`、`go build ./cmd/herdsman-daemon`。

## 重要路径

- `go/cmd/herdsman/`: `herdsman` CLI entrypoint。
- `go/cmd/herdsman-daemon/`: daemon entrypoint。
- `go/internal/observability/`: agent contract、cached agent context、agent index、orchestrator service。
- `go/internal/daemon/`: daemon 的 JSON Lines RPC、process manager、service startup。
- `go/internal/cli/`: `herdsman` CLI 分发与渲染。
- `go/internal/config/`: runtime config schema 与 path/env 解析。
- `go/internal/db/`: SQLite 连接、schema、migration runner、observability store。
- `go/internal/herdr/`: Herdr socket client、managed session client、session snapshot、workspace resolver。
- `go/internal/agy/`: agy hook（PreInvocation / Stop / WakeWaiter）与状态文件。
- `go/internal/agenthistory/`: 各 agent 会话历史的发现、读取与压缩。
- `go/internal/shared/`: JSON Lines framing、SQLite gate 等共享 utility。
- `go/internal/db/migrations/`: SQL migration 序列。
- `docs/plans/`: active plan。已完成的 plan 放到 `docs/plans/archived/`。

## 编码方针

- 聊天中的回复使用中文。
- Go 代码遵循 `gofmt` 与 `go vet`；模块路径 `github.com/dorokuma/herdsman/go`。
- Runtime schema 契约集中在 `go/internal/observability/schemas.go`；DB schema 是 `go/internal/db/schema.go` + `migrations/`。
- 改动要贴合已有的层。不要把 transport、persistence、observability rules、runtime extension 的职责混在一起。
- 改过的 docs 要目视确认链接和命令。
- 不要把 README 的用法示例或详细设计重复写进 AGENTS.md。

## Plan / docs 运用

- Active plan 放在 `docs/plans/` 下。已完成的 plan 移到 `docs/plans/archived/` 下。
- 大的 plan 拆成父 plan 和子 plan。父 plan 只保留目的、方针、进度、子 plan link。
- 子 plan 的目录名要与父 plan 文件名去掉 `.md` 后一致。
- plan 里要有 `Status`、`Progress`、`Next steps`。
- 更新 plan 时确认父子链接、目录名、README / AGENTS 的引用。
- 已完成 plan 的 archive 单独拆成 docs-only commit。

## 注意

- `*.sqlite`、`*.db` 不要 commit。
- 不得回退用户或其他进程的未提交变更。

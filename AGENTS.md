# AGENTS.md
档位：L2

> **已归档（2026-11-11）**：本仓库是老 herdr 时代的工具，能力已被 herdr-core（`hc`）取代，转为只读历史留存，不再开发/发布/部署；归档决策见 `.agents/notes/20261111-archived-superseded-by-herdr-core.md`。

Herdsman 是一个 Go daemon / CLI，从 Herdr 管理的 coding agent 生成 agent snapshot、`agent.*` event 和 orchestrator notification。请先用 `README.md` 确认用法，只有在需要判断规格时才看 `docs/plans/`。

## 铁律 / Iron Rules

1. **构建・测试・静态检查**: `make check` 是唯一检查入口（gofmt、`go vet`、`go test -race`、两个 `go build`），也是仓库级唯一强制门禁，CI 在 push/PR 上调用它；提交、分支、门禁与收口按 `/root/.pig/agent/SYSTEM.md` §4，提交信息与凭据扫描仅在维护者本机由全局 `/root/.git-hooks` 执行（不随仓库分发）。
2. **决策/踩坑须记 .agents/notes/**：若本机维护了 `.agents/notes/` 目录（本机可选留存、不入 git、不随公开仓库分发），重大决策、架构调整、方案否决、临时降级/workaround/特判必须记入其中（满足六触发之一即写，触发条件见本机 `.agents/notes/README.md`）；没有该目录的主机可跳过本条。写完笔记刷新索引：`scripts/notes-index.sh`。
3. **未提交变更保护**: 不得回退用户或其他进程的未提交变更。`node_modules/`、`dist/`、`*.sqlite`、`*.db` 不要 commit。

## 本机运维 / Local Operations

> **zero-node**：本机没有系统/通用 Node 运行时（PATH 上无 node/pnpm/npm/bun），仓库自 `chore/go-only-and-fix-wake` 起为纯 Go——没有 `package.json`、没有 `src/`、没有 `packages/`。生产 CLI 与 daemon 是 Go 二进制 `/root/.local/lib/herdsman-go/{herdsman,herdsman-daemon}`，工具链只有 Go（当前 `go1.26.4`）。

- 正式服务是 systemd 单元 `herdsman.service`（`ExecStart` 直连 `herdsman-daemon`）；启停用 `systemctl restart|stop|start herdsman.service`，判断进程归属看 `systemctl status` 与 `/proc/<MainPID>/cgroup`（`pgrep -af herdsman-daemon` 会误报）。
- daemon 常驻、不在前台交互；异常退出（非零退出码，或被非 SIGHUP/SIGINT/SIGTERM/SIGPIPE 信号杀死）时由 systemd 拉回（当前 `Restart=on-failure`、`RestartSec=5`，连续失败受 `StartLimit*` 限制）；`systemctl stop` 是显式停止，不会被拉回。
- 部署只换二进制 → 重启 → 部署后核对：在 `go/` 下 `go build -o <tmp> ./cmd/herdsman-daemon`（CLI 同理 `./cmd/herdsman`）→ 备份现网二进制为同目录 `*.bak-<时间戳>` → 替换 → `systemctl restart herdsman.service` → 核对。回滚 = 用 `*.bak-<时间戳>` 换回 → 重启 → 同样核对。
- 部署后核对：**版本**以源码事实为准（Go 二进制无 `--version`，读 `go/` 对应代码）；**MainPID** 看 `systemctl show -p MainPID herdsman.service`；**socket / daemon status** 看 `herdsman daemon status`；**`agent list` 端到端**跑 `herdsman agent list`；**优雅关停**看 journal，确认 `shutdown finished`、`exitCode: 0`、无 SIGKILL 与超时。
- 生产日志看 `journalctl -u herdsman.service`（`logs/herdsman.log` 是旧的，不用它判断生产状态）。
- 默认数据目录 `~/.herdsman` 即生产；开发/验证请显式指定临时目录（`HERDSMAN_HOME=/tmp/<名字>`），不要拿生产目录做实验；涉及 CLI/daemon entrypoint 的改动要 `go build` 两个 `cmd` 目标并用临时 `HERDSMAN_HOME` 冒烟（前台起 daemon 后 `herdsman daemon status` 验证）。
- 改了 DB schema 相关行为时确认 `go/internal/db/schema.go` 的 `SchemaStatements` 与 `go/internal/db/migrations/` 的迁移重放保持一致——`sqlite_migrations_test.go` 会强制比对。

## 文档・索引 / Documentation Index

- 现状文档: [README.md](README.md), [docs/plans/](docs/plans/), [docs/releasing.md](docs/releasing.md)
- 决策・踩坑笔记: `.agents/notes/`（本机可选留存、不入 git；规约见本机 `.agents/notes/README.md`）

## 关联仓库 / Related Repositories

- `pi-cache-guardian`: 同为 Pi 生态插件
- `herdsman-wt`: 本仓 worktree 挂载目录

## 核心契约锚点

- Runtime schema 契约：`go/internal/observability/schemas.go`；DB schema：`go/internal/db/schema.go` + `go/internal/db/migrations/`。
- `go/cmd/herdsman/`、`go/cmd/herdsman-daemon/`：两个 entrypoint；`docs/plans/`：active plan（完成的移到 `docs/plans/archived/`）。

## 编码方针

- 聊天中的回复使用中文；Go 代码遵循 `gofmt` 与 `go vet`；模块路径 `github.com/dorokuma/herdsman/go`。
- 改动贴合已有的层，不混 transport、persistence、observability rules、runtime extension 的职责；改过的 docs 目视确认链接和命令。
- 不要把 README 的用法示例或详细设计重复写进 AGENTS.md。

## Plan / docs 运用

- Active plan 放在 `docs/plans/` 下，已完成的移到 `docs/plans/archived/`；plan 要有 `Status`、`Progress`、`Next steps`。
- 大的 plan 拆成父子 plan：父 plan 只留目的、方针、进度、子 plan link；子 plan 目录名与父 plan 文件名去掉 `.md` 一致；更新时确认父子链接、目录名、README/AGENTS 引用；已完成 plan 的 archive 单独拆 docs-only commit。

# AGENTS.md

Herdsman 是一个 TypeScript daemon / CLI，从 Herdr 管理的 coding agent 生成 agent snapshot、`agent.*` event 和 orchestrator notification。请先用 `README.md` 确认用法，只有在需要判断规格时才看 `docs/plans/`。

## 铁律 / Iron Rules

1. **构建・测试・静态检查**: 修改实现后必须通过 `pnpm check`（typecheck、test、Biome、Drizzle、Pi package、Herdr plugin 验证）；改动 CLI/package 时还必须通过 `pnpm build` 和 `pnpm package:check`。
2. **提交规范**——commit message 须过全局 commit-msg hook：Conventional Commits 类型白名单、≤72 字、冒号后一空格、禁噪声词与密钥。
3. **决策/踩坑须记 .agents/notes/**：重大决策、架构调整、方案否决、临时降级/workaround/特判必须记入 `.agents/notes/`（满足六触发之一即写，详见 [.agents/notes/README.md](.agents/notes/README.md)）。
4. **未提交变更保护**: 不得回退用户或其他进程的未提交变更。`node_modules/`、`dist/`、`*.sqlite` 不要 commit。

## 本机运维 / Local Operations

- 正式服务是 systemd 单元 `herdsman.service`（`/etc/systemd/system/herdsman.service`，`enabled`；`ExecStart` 直连全局安装的 `herdsman-daemon.js`）。
- 启停用 `systemctl restart|stop|start herdsman.service`。判断进程归属看 `systemctl status herdsman.service` 与 `/proc/<MainPID>/cgroup`（`pgrep -af herdsman-daemon` 会误报）。
- daemon 常驻、不在前台交互；进程异常退出（非零退出码，或被信号杀死且不属 SIGHUP/SIGINT/SIGTERM/SIGPIPE）时由 systemd 拉回（本机单元当前为 `Restart=on-failure`，`RestartSec=5` 约 5 秒后重试，连续失败受 `StartLimit*` 限制）；`systemctl stop` 是显式停止，不会被拉回。
- 部署：`pnpm build` → `npm pack`（用 nvm 的 npm，使 `npm prefix -g` 落在 nvm 前缀）→ 把 tarball 全局安装 → `systemctl restart herdsman.service` → 按上一条核对。部署或发布前先按「验证步骤」用 nvm 的 v22.23.1 复跑一次 `pnpm check`（生产面是 node 22）。
- 生产日志看 `journalctl -u herdsman.service`。`logs/herdsman.log` 是旧的，不用它判断生产状态。
- 部署后核对：版本用 `PATH="$HOME/.nvm/versions/node/v22.23.1/bin:$PATH" npm ls -g @dorokuma/herdsman`，或直接读 `"$HOME/.nvm/versions/node/v22.23.1/lib/node_modules/@dorokuma/herdsman/package.json"`（部署是装进 nvm 前缀的，而默认 shell 的 npm 可能走 mise 前缀、查不到该包，所以要带前缀或读绝对路径）；PID 看 `systemctl show -p MainPID herdsman.service`；socket 看 `herdsman daemon status`。**本地 tarball 安装的包，包内版本号不等于已部署内容**（同一个版本号可以是不同提交构建出来的），核对应把该批 tarball／批次一并记下。
- 默认数据目录 `~/.herdsman` 即生产；开发/验证请显式指定临时目录（`HERDSMAN_HOME=/tmp/<名字>`），不要拿生产目录做实验。

## 文档・索引 / Documentation Index

- 现状文档: [README.md](README.md), [docs/plans/](docs/plans/), [docs/releasing.md](docs/releasing.md)
- 决策・踩坑笔记: [.agents/notes/](.agents/notes/)（规约: [.agents/notes/README.md](.agents/notes/README.md)）
- 写完笔记刷新索引：scripts/notes-index.sh（本地生成 INDEX.md，不入 git）

## 关联仓库 / Related Repositories

- `pi-cache-guardian`: 同为 Pi 生态插件
- `herdsman-wt`: 本仓 worktree 挂载目录

## 常用命令

- `mise install`: 安装 `mise.toml` 中的 Node.js / pnpm。
- `pnpm install`: 安装依赖。
- `pnpm check`: 一次性执行 typecheck、test、Biome、Drizzle、Pi package、Herdr plugin 的验证。
- `pnpm test`: 运行一次 Vitest。
- `pnpm test:watch`: Vitest 的 watch。
- `pnpm build`: 删除旧的 `dist` 后输出 TypeScript，并用 `tsc-alias` 解析 import alias。
- `pnpm package:check`: 构建 root npm package，并验证 tarball 的 file allowlist。
- `pnpm lint:fix`: 应用 Biome 的 lint/import/format fix。
- `pnpm db:generate`: 从 `src/db/schema.ts` 生成 SQL migration。
- `HERDSMAN_HOME=/tmp/herdsman pnpm db:migrate`: 对指定的 Herdsman home 的 SQLite DB 应用 migration。

## 验证步骤

- 修改实现后必须通过 `pnpm check`。
- 涉及 CLI entrypoint、`dist` 的 import 解析、package 内容的改动，还要通过 `pnpm build` 和 `pnpm package:check`。
- 改了 DB schema 就先执行 `pnpm db:generate`，确认生成的 SQL 之后再看 migrate。
- Node / pnpm 的 PATH 较旧的环境，在执行验证命令前加上下面这行。

```bash
PATH="$HOME/.local/share/mise/installs/node/26.7.0/bin:$HOME/.local/share/mise/installs/pnpm/11.9.0:$PATH"
```

本机（生产机）运行时事实，照它选运行时，不要对着版本号猜：

- 生产 daemon：由 systemd 单元 `herdsman.service` 的 `ExecStart` 固定用 nvm v22.23.1 的 node 启动。
- 提交门禁：`.husky/pre-commit` 显式把 mise 钉版前置进 PATH，实际跑 mise 的 node 26.7.0 + pnpm 11.9.0（与 `mise.toml` 一致）。
- 普通 shell 里跑哪个版本以现场 `node -v` 为准：登录交互 bash 会先激活 mise（`/root/.bashrc` 在 nvm 之后激活 mise，mise 优先），而 daemon 派生的 shell 继承单元里的 PATH（nvm 优先）。
- 支持面与生产面是 node 22（`package.json` 的 `engines.node >=22.12.0`，CI 也用 node 22），所以 mise 的 26.7.0 上绿不能替代生产面验证，部署或发布前必须再跑一次：

```bash
PATH="$HOME/.nvm/versions/node/v22.23.1/bin:$PATH" pnpm check
```

- 上一条的 mise PATH 只在需要钉版（`mise.toml` 的 node 26.7.0 / pnpm 11.9.0）时前置使用。mise 的 pnpm 安装目录没有 `bin/`，PATH 里要写安装目录本身。

## 重要路径

- `src/observability/`: agent contract、cached agent context、agent index、orchestrator service。
- `src/daemon/`: daemon 的 JSON Lines RPC、process manager、service startup。
- `src/cli/`: `herdsman` CLI entrypoint。
- `src/config/`: runtime config schema 与 path/env 解析。
- `src/db/`: SQLite 连接、Drizzle schema、migration runner、observability store。
- `src/herdr/`: Herdr socket client、managed session client、session snapshot、workspace resolver。
- `src/shared/`: JSON Lines framing 等共享 utility。
- `packages/herdsman-pi/`: 通过 npm 发布的 Pi extension package。
- `packages/herdsman-herdr-plugin/`: 通过 GitHub 分发的 private Herdr integration。不发布到 npm。
- `test/unit/`: pure logic / contract tests。
- `test/integration/`: 使用 SQLite / JSON Lines RPC 等实体的 tests。
- `docs/plans/`: active plan。已完成的 plan 放到 `docs/plans/archived/`。

## 编码方针

- 聊天中的回复使用中文。
- TypeScript 是 ESM + `NodeNext`。`src` 下使用 `@/*` import alias。
- Runtime schema 交给 TypeBox/Ajv，DB schema 交给 Drizzle。
- 改动要贴合已有的层。不要把 transport、persistence、observability rules、runtime extension 的职责混在一起。
- Markdown docs 不在 Biome gate 范围内。改过的 docs 要目视确认链接和命令。
- 不要把 README 的用法示例或详细设计重复写进 AGENTS.md。

## Plan / docs 运用

- npm 和 GitHub 的 release 步骤以 `docs/releasing.md` 为准。发布的 npm package 只有 root 和 `packages/herdsman-pi` 两个。
- Active plan 放在 `docs/plans/` 下。已完成的 plan 移到 `docs/plans/archived/` 下。
- 大的 plan 拆成父 plan 和子 plan。父 plan 只保留目的、方针、进度、子 plan link。
- 子 plan 的目录名要与父 plan 文件名去掉 `.md` 后一致。
- plan 里要有 `Status`、`Progress`、`Next steps`。
- 更新 plan 时确认父子链接、目录名、README / AGENTS 的引用。
- 已完成 plan 的 archive 单独拆成 docs-only commit。

## 注意

- `node_modules/`, `dist/`, `*.sqlite` 不要 commit。
- `pnpm-workspace.yaml` 是给 pnpm 11 的 `allowBuilds` 用的。不要以 workspace 化为目的去编辑它。
- 不得回退用户或其他进程的未提交变更。

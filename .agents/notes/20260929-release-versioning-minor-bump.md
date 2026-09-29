---
status: active # active | superseded
superseded_by: ""
supersedes: ""
# 模块可选值: observability, daemon, cli, config, db, herdr, shared, herdsman-pi, herdsman-herdr-plugin, release
模块: release
---

# 0.13.0 走 minor 递增，README 的 Herdr tag 延到发布提交

## 一句话结论

本批（工具链对齐 + 已入库的 CLI daemon 启停命令删除 + 入口守卫删除）发 **0.13.0 而不是 0.12.2**：`^0.12.1` 的解析范围是 `>=0.12.1 <0.13.0`，若按历史 patch 习惯发 0.12.2，用 caret 的安装者会静默吃到这次破坏性删除；minor 递增会强制他们显式升级。同时**两个 README 里的 Herdr 安装 tag 不在本批改**，保持指向已存在的 `v0.12.1`，改 tag 这一步落在发布提交里。

## 背景

- 仓库历史的版本习惯是 patch 密集（0.11.4 → 0.11.5 → 0.11.6 → 0.11.7 → 0.12.0 → 0.12.1），`docs/releasing.md` 也只写了“next unused patch version”；但 `v0.12.1..HEAD` 里有 `herdsman daemon start|stop|restart` 的**对外命令移除**（`v0.12.1` 的 `helpText` 里确实列着这三条命令，且错误文案会提示 `herdsman daemon start`），属于使用者可感知的行为删除。
- 仓库历史把 README 的 Herdr tag 替换放在**发布提交**里（0.11.6 的 release commit 同时改了 `README.md` 与 `packages/herdsman-herdr-plugin/README.md` 的 tag）；但那一步在主流程文档里只出现在 recovery 段，主流程没有写。
- 已核实 `v0.13.0` 这个 tag 本地与远端都不存在（远端 tag 停在 `v0.12.1`），提前写进 README 会让使用者装不上。

## 决策

1. **版本递增按 0.x 破坏性变更走 minor → 0.13.0**，理由是可预期的升级行为：caret 使用者只有跨 minor 才会被要求显式升级，patch 会把删除命令静默带入。
2. **本批 README 的两个 Herdr 安装 tag 保持 `v0.12.1`**（= npm latest = 已存在且可安装的 tag）；tag 替换写进 `docs/releasing.md` 的**主流程**（版本更新之后、发布提交之前），并在发布提交的 `git add` 清单里带上两个 README。
3. 版本文件仍是 `docs/releasing.md` 列出的四个（`package.json`、`packages/herdsman-pi/package.json`、`packages/herdsman-herdr-plugin/package.json`、`packages/herdsman-herdr-plugin/herdr-plugin.toml`），本批不含 README。

## 被放弃的方案（必填）

- **按历史习惯发 0.12.2**：caret 范围 `>=0.12.1 <0.13.0` 会包含它，破坏性删除静默到货。放弃。
- **本批就把 README tag 改成 `v0.13.0`**：tag 尚不存在，使用者照文档安装会失败；也与仓库"tag 替换随发布提交"的既有做法冲突。放弃（延到发布提交）。
- **只改 README 指向 `v0.12.1` 而不在发布流程里补步骤**：下次发布仍会漏改（这正是 README 长期停在 `v0.11.6` 的原因），所以把该步骤写进主流程。放弃"只改不改文档"。

## 遗留清单（第二意见复审的建议与观察项；标「本轮已处理」的项已在本批处理）

1. **R4 提交门禁硬依赖 mise**（**本轮已处理**）：`.husky/pre-commit` 在取 node/pnpm 之前先 `command -v mise` 判存在，缺 mise 时报 `mise: not found on PATH（提交门禁靠 mise 定位钉版 node/pnpm）`；mise 在但 `mise where` 失败时报 `mise: cannot locate node（mise 在但取不到 node，未安装？跑 mise install）`／`… pnpm …`，两种失败已可区分（隔离 PATH 实测）。硬依赖 mise 本身保留（旧写法静默降级、继续往下跑，属有意放弃的方案）。
2. **R5 多个 node 大版本并存**：提交门禁 mise node 26.7.0 / CI node 22（`.github/workflows/ci.yml:15`）/ 生产 nvm node 22.23.1（systemd `ExecStart`）/ 类型面 `@types/node ^24`（`package.json:66`）。本批只在 `AGENTS.md` 把事实写清楚，不做收敛；收敛代价：重装 mise 工具链、`@types/node` 降版会引类型错、改 systemd `ExecStart` 属生产变更，需单独一批评估。
3. **R6 新笔记 frontmatter 的 `模块:` 为空**（**本轮已处理**）：`.agents/notes/_template.md` 的模块可选值注释与 `.agents/notes/README.md` 新增的 frontmatter 条目（原 README 只有触发/豁免/流程，未列可选值）均加入 `release`（发布・版本流程），本条笔记 `模块:` 填 `release`，索引刷新后不再显示 `-`。
4. **R7 单元 PATH 里 node 与 pnpm 的来源不同（经复核修正）**（**本轮已处理**）：“daemon 派生的 shell 继承单元 PATH（nvm 优先）”只对 **node** 成立（单元 PATH 含 `/root/.nvm/versions/node/v22.23.1/bin`）。pnpm 一项，第二意见的原判据（“`/root/.local/bin` 里有 mise 的 pnpm”）经实测不成立：`/root/.local/bin/pnpm` 是指向 `/root/.hermes/node/lib/node_modules/pnpm/bin/pnpm.cjs` 的**已失效软链**（目标不存在），在单元那条 PATH 下 `command -v pnpm` 实际落到 `/root/.nvm/versions/node/v22.23.1/bin/pnpm`（corepack shim，v11.9.0）。本轮：`AGENTS.md` 该句改写为“node 走 nvm 优先；pnpm 走 nvm 前缀下的 corepack shim”，并注明失效软链已删除；软链本身经 `ls -l` / `readlink -f`（exit 1）/ `test -e`（target MISSING）确认为断链、且 grep 全盘 profile 与单元均无引用后删除。
5. **版本一致性校验的边界（第二意见更正，已复核）**：`test/unit/package-publication.test.ts:59-60` 校验四个版本字段互相一致，`:77` 起校验 tarball 内容与 manifest 相符（root 侧另有 `scripts/check-root-package.mjs`）；但**不覆盖** README 里的 Herdr 安装 tag ↔ 版本，也不覆盖 npm 上的实际发布状态。README tag 的一致性目前只靠 `docs/releasing.md` 的发布步骤人工保证。

### 第二意见观察项（非阻断，同批记录）

1. **「（corepack shim，v11.9.0）」缺 cwd 限定词**：corepack shim 的 pnpm 版本随 cwd 解析 `package.json` 的 `packageManager` 字段，本轮实测仓内为 11.9.0、仓外（`/tmp`）为 11.22.0；上文第 4 条的版本号只在仓库目录内成立，建议补一句 cwd 限定。
2. **pre-commit 失败文案把原因收窄为「未安装」**：坏 shim、或取不到工具（如 mise config 未信任）的情况也走同一句「（mise 在但取不到 …，未安装？跑 mise install）」，退出码与行为一致，仅影响排障时的第一落点。
3. **第 1 条的 R4 表述需要前提**：「旧写法静默降级、继续往下跑（实测 exit 0）」只在 PATH 里另有可用 pnpm 时成立——裸 PATH 下旧写法会跑到 `pnpm lint-staged` 后以 **127** 退出。
4. **同类断链未清**：`/root/.local/bin/npx`（→ `/root/.hermes/node/bin/npx`）、`/root/.local/bin/shepherd`（→ `/root/.nvm/versions/node/v22.23.1/bin/shepherd`）仍是断链（本轮实测 target MISSING）；npx 已并入「运行时统一到 node 22」那批待处理，shepherd 未处理。
5. **枚举三份拷贝 + 无校验**：模块可选值现存在三处拷贝——`.agents/notes/_template.md` 的注释、`.agents/notes/README.md` 的 frontmatter 条目、笔记自身 frontmatter 的注释；三者之间无断言校验。本批接受人工同步，未加测试。
6. **笔记互链缺口**：本批 R4 的来由是 `.agents/notes/20260929-drop-daemon-entry-guard.md` 的 item 6（`export PATH="$(mise where node)…"` 不受 `set -e` 保护），本笔记未回指该条，建议后续补链。
7. **流程风险**：多批次共用同一工作树时，hook 的五道仓级门禁对整棵工作树全量判分（只有 lint-staged 看 staged 文件），半成品会被一并判分甚至误扫；本批提交因此改用独立 worktree 隔离。
8. **CHANGELOG 本批未加条目**（与上一同类批一致）：下一 release 提交需带上本批两处用户可见变化——「hook 失败文案可区分」与「AGENTS.md 的运行时事实澄清」。

## 来源

- 用户/第二意见决策（2026-09-29）：0.13.0 保留，README tag 本批保持 `v0.12.1`。
- 事实依据：`git show v0.12.1:src/cli/herdsman.ts` 的 `helpText()` 与 daemon 命令解析；`git ls-remote --tags origin` 无 `v0.13.0`；`npm view @dorokuma/herdsman version` = 0.12.1。
- 发布流程改动：`docs/releasing.md`（Update versions 段新增 README tag 替换步骤与校验命令，Commit 段 `git add` 加入两个 README）。

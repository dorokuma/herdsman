---
status: active # active | superseded
superseded_by: ""
supersedes: ""
# 模块可选值: observability, daemon, cli, config, db, herdr, shared, herdsman-pi, herdsman-herdr-plugin, release
模块: release
---

# 生产安装渠道统一为 npm registry 单一渠道

## 一句话结论

生产全局副本已从「本地 tarball 安装」切换到 **npm registry 单一渠道**，安装命令固定为 `npm install --global @dorokuma/herdsman@<version>`（钉版本号 + nvm PATH 前缀）；切换后安装目录与 registry 发布物**逐文件 sha256 237/237 完全一致**，唯一差异是包内 `README.md` 的安装 tag 由 `v0.12.1` 变为 `v0.13.2`，daemon 已重启并六条验收全过。**理由**：两套渠道并存、且本地 tarball 会出现「同版本号、不同字节」，会让版本核对失真；统一到 registry 后**版本号即部署身份**。

## 背景

- 0.13.2 发布之前，生产 daemon 用的是**本仓构建产物的本地安装**（`pnpm build` → `npm pack` → tarball 全局安装），而 npm 上发布的是另一个产物。二者可以同版本号但字节不同，核对时"版本号"不再是可信身份。
- 0.13.2 发布后仍保留这一状态：`20261001-release-0.13.2-post-release-docs.md` 的「遗留 / 观察项」第 2 条记明「生产全局装的是本地 tarball 副本，不是 registry 副本……是否切换属独立部署事项，需用户授权」。
- 本轮渠道切换即对该条的处置。切换属于生产部署动作，与仓库文档改动分开；本笔记同时归档两件事：**渠道切换的事实**与**仓库文档从此只描述 registry 单一渠道**。

## 决策

1. **生产安装渠道统一为 npm registry 单一渠道**，不再把本地 tarball 视为生产安装路径。
   - 安装命令：`PATH=/root/.nvm/versions/node/v22.23.1/bin:$PATH npm install --global @dorokuma/herdsman@<version>`。
   - **必须钉版本号**（不用 `latest`、不用范围），**必须显式带 nvm PATH 前缀**，使 `npm prefix -g` 落在 nvm 前缀（默认 shell 的 npm 可能走 mise 前缀，装错位置）。
2. **版本语义随之改为**：registry 不允许同版本重发，所以**版本号即部署身份**。核对 = `npm ls -g` 出的版本；需要更高置信度时，与**安装目录**比对（先 `npm pack @dorokuma/herdsman@<version>` 取发布物解包，再与 `lib/node_modules/@dorokuma/herdsman` 逐文件 sha256 比对，**排除嵌套 `node_modules`**）。
3. **回滚常规口径 = 版本 pin 安装**（见下「回滚口径」），`/tmp` 备份 tgz 降为应急手段。
4. **仓库文档只描述 registry 单一渠道**：`AGENTS.md` 的部署条目与版本语义条目改写口径，`README.md` 补包来源；`docs/releasing.md` 的 `npm pack` 属发布流程（打包验证、产物指纹），**不动**。

## 切换前证据（旧全局副本 = 本地部署产物）

| 项 | 值 |
| --- | --- |
| 旧生产全局副本 | 16:21 本地部署的 tarball 安装物 |
| 旧副本逐文件 sha256 | 等于该批本地 tarball 解包结果 |
| 与 registry 发布物的差异 | **仅**包内 `README.md` 一行——安装 tag `--ref v0.12.1` vs `--ref v0.13.2` |
| 结论 | 同版本号（0.13.2）、不同字节：`README.md` 的 `--ref` 停留在旧 tag，属本地构建与 registry 发布物的差异；这正是「版本号不能当身份」的实例 |

## 切换与验收

**安装命令（本轮既成事实）**：

```bash
PATH=<nvm v22.23.1 bin>:$PATH npm install --global @dorokuma/herdsman@0.13.2
```

**验收结论（六条，全部通过）**：

| # | 项 | 结果 |
| --- | --- | --- |
| 1 | 与 registry 发布物一致性 | 安装目录（排除嵌套 `node_modules`）与 registry 发布物**逐文件 sha256 237/237 完全一致** |
| 2 | 包内 `README.md` 安装 tag | 由 `--ref v0.12.1` 变为 `--ref v0.13.2`（与发布物一致） |
| 3 | 优雅关停 | `shutdown finished { elapsedMs: 13, exitCode: 0 }`，无 SIGKILL、无超时 |
| 4 | 服务重启 | `MainPID` `3958288` → `4059548`，`NRestarts=0`、`Result=success` |
| 5 | socket 与端到端 | socket 可达，`agent list` 端到端可用 |
| 6 | 告警面 | 无新的空正文告警 |

**新还原点（切换后建立，已验证可完整解包）**：

| 项 | 值 |
| --- | --- |
| 路径 | `/tmp/herdsman-global-backup-0.13.2-localtarball-20261001180706.tgz` |
| sha256 | `04eb0665ee01c980687b77926beb4f653b476296bec111176aaced4a473b5623` |

（路径名中的 `localtarball` 指**切换前**那份本地 tarball 副本的备份；切换后的常规回滚走版本 pin，而不是还原本 tgz。应急还原：`tar -xzf <备份tgz> -C /root/.nvm/versions/node/v22.23.1/lib/node_modules/`，然后 `systemctl restart herdsman.service`。应急还原是「版本号≠身份」的破例：0.13.1 从未发布，其备份还原后版本号在 registry 无对应物；0.13.2 备份还原后 `npm ls -g` 仍显示 0.13.2，但字节是切换前的本地构建。**恢复后收尾 = 用同一渠道钉版本装回 registry 版本**，否则版本号又失去身份含义。）

## 回滚口径

- **常规回滚 = 版本 pin 安装**：`npm install --global @dorokuma/herdsman@<上一个完整发布的版本>`（同样钉版本号 + nvm PATH 前缀）。
- **`/tmp` 备份 tgz 仅作应急**：仅当该版本在 registry 取不到时（网络 / registry 故障，或该版本被**下架 / unpublish**；deprecate 不算——仍可安装），才用 `/tmp/herdsman-global-backup-0.13.2-localtarball-20261001180706.tgz` 还原切换前状态；还原属**临时破例**（版本号≠身份，见上「新还原点」），恢复后须用同一渠道钉版本装回 registry 版本才算收尾。
- **两枚 tgz 的保留与清理时点**：`/tmp/herdsman-global-backup-0.13.1-202610011621.tgz` 与 `/tmp/herdsman-global-backup-0.13.2-localtarball-20261001180706.tgz` 均**保留到下一版生产部署验证通过后一并清理**（0.13.1 备份的清理触发条件虽已满足，但刚换渠道，暂不销毁回滚物）。`/tmp` 非持久，若被清则应急降级为「本机 npm cache 已含 0.13.2 两包条目，可 `--prefer-offline`（无网可选 `--offline`）」+ 等待 registry 恢复。
- 不得用 `latest` 或范围安装来回滚——会丧失版本身份。

## 文档改动清单

| 文件 | 位置 | 改动 |
| --- | --- | --- |
| `AGENTS.md` | 「本机运维」部署条目 | `pnpm build` → `npm pack` → 装 tarball 的部署链，改为 registry 单一渠道（钉版本 + nvm PATH 前缀）→ restart → 按「部署后核对」核对（daemon status / MainPID / socket / `agent list` / 优雅关停 journal）；并补**完整发布前置判据**（root + `packages/herdsman-pi` 两包均已发布、`v<version>` tag 已推，半发布不部署，例外须用户决策，附 `npm view` ×2 + `git ls-remote --tags --exit-code origin "refs/tags/v<version>"` 校验命令；`--exit-code` 不可省——缺 tag 时 `git ls-remote` 默认 exit 0 + 空输出会静默通过，加上后缺失为 exit 非 0） |
| `AGENTS.md` | 「本机运维」部署后核对条目 | 「本地 tarball 安装的包，包内版本号不等于已部署内容」→「registry 不允许同版本重发，版本号即部署身份」，核查口径改为 `npm ls -g` + 需要时与**安装目录**逐文件 sha256 比对（**排除嵌套 `node_modules`**）；核对项写明五项（版本 / MainPID / socket / `agent list` / 优雅关停 journal）；保留一行 tarball 历史教训 |
| `AGENTS.md` | 「本机运维」回滚条目 | 「上一个已发布版本」→「**上一个完整发布的版本**」；并补备份 tgz 应急为**临时破例**（版本号≠身份，恢复后须同渠道装回 registry 版本才算收尾） |
| `README.md` | `### Install from source` | 标明**仅本地开发用**，不是生产安装渠道 |
| `README.md` | `## Start the daemon` 生产/systemd 段 | 补**包来源**（生产通过 npm registry 安装、由 systemd 托管）+ 补**Node 工具链 / PATH 前缀一致性**一句（钉 nvm v22.23.1 前缀，避免装进不同 global prefix） |
| `.agents/notes/20261001-release-0.13.2-post-release-docs.md` | 「遗留 / 观察项」第 2 条 | 标为**已处置**并指向本笔记（保留原文，不删历史文字） |
| `.agents/notes/20261001-production-install-channel-npm-registry.md` | — | 本笔记 |

## 遗留 / 观察项

1. **`CHANGELOG.md` 里三处「本地 tarball 安装」的历史陈述不追溯改写**：0.13.2 段已封在 tag `v0.13.2` 与 GitHub Release 中，改写会让仓库历史与已发布产物不一致。新口径只对后续生效。
2. **全局安装无 `_resolved` / `_from`、无全局 lock**：来源只能靠**内容比对**（逐文件 sha256）证明，不能靠 npm 元数据。这也是验收第 1 条采用逐文件比对的原因。
3. **`@ryonakae` 空目录**：与本轮无关，未触碰。
4. **生产此前无任何部署脚本、无 `docs/deploying.md`**：部署知识一度只存在于 `AGENTS.md` 与 `/tmp` 一次性目录；本笔记与 `AGENTS.md` 的改写是该知识第一次落进版本控制。
5. **发布流程中的 `npm pack` 不受影响**：`docs/releasing.md` 的 `npm pack`（打包验证、产物指纹）是发布流程的一部分，不是生产安装路径，保持不动。
6. **「版本号即部署身份」只覆盖包自身 237 个文件，不含依赖树**：`package.json` 的依赖为 caret 范围、tarball 内无 lock。本轮**不做常规依赖快照**（不新增部署步骤）；需要复现当时的依赖解析时，临时按下列命令生成：`(cd /root/.nvm/versions/node/v22.23.1/lib/node_modules/@dorokuma/herdsman && npm ls --all --omit=dev --json)`（该形式实测 exit 0、含依赖全树；`npm ls -g --all` 在本机只出 2 行或混入其它全局包而报 invalid）。
7. **`docs/plans/2026-07-14-herdsman-test-dogfooding.md:32` 已过时（仅登记，本批不改）**：该处写 "installed `herdsman` wrapper executes this checkout's `dist/...`"，但实测 wrapper 指向 `lib/node_modules/@dorokuma/herdsman/dist/...`（即 registry 副本），并非本 checkout。
8. **两个默认口径（用户未另行指示，按默认写入，可改）**：① **部署前置条件 = 两包（root + `packages/herdsman-pi`）都已发布且远端 tag 已推**；半发布状态（root 已发、pi 缺席、tag 未推）的版本**不部署到生产**，**例外须用户决策**。校验命令：`npm view @dorokuma/herdsman@<version> version`、`npm view @dorokuma/herdsman-pi@<version> version`、`git ls-remote --tags --exit-code origin "refs/tags/v<version>"`（`--exit-code` 不可省：缺 tag 时 `git ls-remote` 默认 exit 0 + 空输出会静默通过，加上后缺失为 exit 非 0）。② **依赖树不引入 shrinkwrap**（属发布侧决策、影响所有用户，另行评估），本轮**不做常规依赖快照**，只在需要复现依赖解析时临时生成（见第 6 条）。

## 被放弃的方案（必填）

- **继续用本地 tarball 作为生产安装路径**：会出现「同版本号、不同字节」（本例：包内 `README.md` 的 `--ref` 停在 `v0.12.1`），版本号失去身份含义，核对需要额外记录「哪批 tarball」，易失真。放弃，统一为 registry 单一渠道。
- **用 `latest` 或不钉版本号安装**：`latest` 会随时间漂移，无法回答"生产跑的是哪一版"；且回滚不可复现。放弃，一律钉版本号。
- **回滚默认走 `/tmp` 备份 tgz**：tgz 是应急产物，路径与时效都不稳定；常规回滚用 registry 版本 pin 更可复现。放弃把 tgz 当常规回滚手段，降为应急。
- **把 `docs/releasing.md` 里的 `npm pack` 一并去掉**：那是发布流程的打包验证与产物指纹步骤，与生产安装渠道无关。放弃，不动。

## 来源

- 本机生产全局副本切换与验收实测（sha256 237/237、`MainPID` 3958288 → 4059548、`NRestarts=0`、`Result=success`、`shutdown finished { elapsedMs: 13, exitCode: 0 }`、socket 可达、`agent list` 端到端可用、无新空正文告警）。
- 新还原点 `/tmp/herdsman-global-backup-0.13.2-localtarball-20261001180706.tgz`（sha256 `04eb0665ee01c980687b77926beb4f653b476296bec111176aaced4a473b5623`）。
- [`20261001-release-0.13.2-post-release-docs.md`](20261001-release-0.13.2-post-release-docs.md)（「遗留 / 观察项」第 2 条的处置对象）。

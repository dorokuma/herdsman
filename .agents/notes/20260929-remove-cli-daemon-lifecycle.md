---
status: active # active | superseded
superseded_by: ""
supersedes: ""
模块: cli
---

# 删除 CLI daemon 启停命令，生产排他靠机制

> **2026-09-29 更新**：本笔记第 6、7 条（入口层启动守卫、守卫无逃逸开关）已被 [撤销入口启动守卫：daemon 靠 systemd 常驻，排他只留在实例锁](20260929-drop-daemon-entry-guard.md) 撤销，下方一句话结论与第 3、6、7 条已就地标注为“已撤销”；其余结论（删除 CLI 启停命令、护栏一实例锁、生产只走 systemctl）仍然有效。

## 一句话结论

`herdsman daemon start|stop|restart` 从根上删除（CLI、库、测试、活文档全部移除），CLI 只保留只读的 `herdsman daemon status`；本机生产 daemon 唯一由 systemd 单元 `herdsman.service` 托管，排他不靠文档而靠机制：daemon 入口的实例锁（flock，先拿锁再开库）。~~入口层的“默认数据目录非托管则拒绝启动”守卫~~（2026-09-29 已撤销，见上文 banner 链接）。

## 背景

- 本机是生产机，正式服务是 systemd 单元 `herdsman.service`（`ExecStart` 直连全局安装的 `herdsman-daemon.js`，`enabled`，`Restart=on-failure`）。
- `herdsman daemon start|stop|restart` 是"自己 detach 起进程"的旁路：`stop`/`restart` 对 systemd 实例发 SIGTERM，daemon 正常退出（exit 0），systemd 视为成功退出、`Restart=on-failure` 不触发，结果是单元 inactive、不自愈，同时一个脱管进程在服务；而 `herdsman daemon status` 只查 pid 文件 + socket、不查 cgroup，照样报 `running`。2026-09-29 因此出过一次事故。
- 只读调查确认：全仓代码 / 测试 / CI / 扩展零调用这三条命令，生产 systemd 单元也不经过它们 → 删除对生产零代价。

## 决策

1. **删除 CLI 启停命令**：`DaemonAction` 收窄为 `status`，删掉 `start`/`stop`/`restart` 分支、相关 import 与帮助文本；`src/daemon/process-manager.ts` 删除因此变成死代码的 `startDaemonProcess`、`stopDaemonProcess`、`spawnDaemonProcess`、`openRotatedLog`、`prepareDaemonSocketPath`、`writeDaemonRuntimeRecord`、`withDaemonLock` 及其专用依赖字段；socket 错误提示改为指向 `systemctl status herdsman.service`，不再指向已删除的命令。
2. **只保留只读 status**：它只读 pid 文件、按 `/proc` 校验进程身份并探测 socket，不写任何状态、不发信号，是无副作用的自检入口，事故里"照旧报 running"的原因是它不查 cgroup，与命令本身的价值无关。
3. **残留入口**：开发/临时环境需要起 daemon 时，只能用前台入口 `node <包>/dist/src/cli/herdsman-daemon.js`（不提供后台化能力），并显式指定临时数据目录（~~必须~~ 2026-09-29 更新：入口守卫已撤销，显式临时目录降为建议，不再是入口强制）。
4. **生产只走 systemctl**：`systemctl restart|stop|start herdsman.service`。判断进程归属看 `systemctl status herdsman.service` 与 `/proc/<MainPID>/cgroup`（`pgrep -af herdsman-daemon` 在本机会误报）。
5. **护栏一：实例锁（现行，唯一护栏）**：`herdsman-daemon.js` 入口以 `${pidPath}.instance.lock` 的 flock 独占一个 `HERDSMAN_HOME`，第二个/脱管进程被直接拒绝。本轮把拿锁**提前到 openSqlite/applyMigrations 之前**，被锁拒绝的进程完全不碰数据库（不开、不迁移、不写 pid 文件）。
6. ~~**护栏二：入口层启动守卫（本轮新增）**~~ **——2026-09-29 已撤销，以下是当时的取舍记录**：`herdsman-daemon.js` 解析出的数据目录是**默认生产目录**（未显式指定 `HERDSMAN_HOME`，即 `~/.herdsman`）时，只有确认被 systemd 托管（`INVOCATION_ID` 存在，或 `/proc/self/cgroup` 命中 `*.service`）才允许启动；否则拒绝启动、非零退出并打印提示。显式指定 `HERDSMAN_HOME=/tmp/<名字>` 允许非托管启动。守卫只在入口层，不放 `runObservabilityDaemonService`，以免直接调用该函数的测试全部被守卫拦死。
7. ~~**无逃逸开关**~~（守卫撤销后本条已无对象，仅作历史记录）：不再提供 `HERDSMAN_ALLOW_UNSUPERVISED` 之类的“硬起”口子——测试是直接调用 `runObservabilityDaemonService`、不经过入口，不需要逃生口；守卫的语义只是“不许把生产目录当实验台”，不是给生产留手动启停的后门。

## 被放弃的方案（必填）

- **保留 `start`/`stop`/`restart` 并加 systemd 检测警告**：警告是文档，事故已证明"文档挡不住"。排他必须靠机制，故直接删除命令。
- **保留命令但改为 `systemctl` 的薄封装**：等于让 CLI 成为另一个启停入口，systemd 单元与 CLI 之间仍有双入口歧义；且仍需处理脱管进程语义。放弃。
- **只写文档禁令、不动代码**：同一原因放弃。
- **把启动守卫放进 `runObservabilityDaemonService`**：会让所有直接调用该函数的单测（它们注入临时 `HERDSMAN_HOME`、模拟各种启动路径）全部被守卫拦死，且把"入口策略"混进"服务实现"。故放入口层。
- **守卫只用 cgroup、或只用 `INVOCATION_ID`**：单一判据易受宿主差异影响（cgroup v1/v2 文本不同、非 systemd 的 supervisor 无 `INVOCATION_ID`）；两者并用任一命中即放行，实测本机 `0::/system.slice/herdsman.service` + `INVOCATION_ID` 双命中。
- **`writeDaemonRuntimeRecord` 保留（无人写、只有人来读）**：删掉 CLI 启停后它再无调用方，保留即死代码；`readDaemonRuntimeRecord` 仍是活代码（`resolveRuntimeForCommand` 在用），故保留读侧。

## 来源

- 2026-09-29 事故：误用 `herdsman daemon restart` 停掉 systemd 实例并起了脱管进程。
- 提交前只读调查：全仓零调用 + 生产单元不经这些命令。
- 实测：`cat /proc/<MainPID>/cgroup` → `0::/system.slice/herdsman.service`；单元环境存在 `INVOCATION_ID`。

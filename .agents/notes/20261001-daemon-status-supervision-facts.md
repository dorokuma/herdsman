---
status: active # active | superseded
superseded_by: ""
supersedes: ""
# 模块可选值: observability, daemon, cli, config, db, herdr, shared, herdsman-pi, herdsman-herdr-plugin, release
模块: daemon
---

# `daemon status` 增加只读的"被谁托管 + 重启次数"事实位

## 一句话结论

`herdsman daemon status` 在原有 pid 文件 + socket 探测之上新增两个**只读**事实位：`managedBy`（daemon pid 的 `/proc/<pid>/cgroup` 是否落在 `herdsman.service` 内 → `systemd:herdsman.service` / `unmanaged` / `unknown`）与 `restartCount`（`systemctl show herdsman.service -p NRestarts --value`，best-effort）。两者都走可注入探针、带短超时；拿不到时只降级自身字段（`unknown` / 省略），**不改** pid/socket 判定、`state` 取值与退出码。CLI 默认输出仍是 JSON，人类可读视图走新增的 `--text`。

本批实现的是 [撤销入口启动守卫：daemon 靠 systemd 常驻，排他只留在实例锁](20260929-drop-daemon-entry-guard.md) 遗留清单第 1 条（"`daemon status` 不报'被谁托管'"）。

## 背景

- 事故复盘里 `daemon status` 只给 pid 文件 + socket 的探测结果，无法区分"systemd 托管的 daemon"与"有人 `systemctl stop` 后又在别处起的脱管进程"，也看不见"不断重启循环"。
- `state` / `pid` / `socketReachable` / `stalePid` / `pidFileMissing` 这些既有判定与退出码语义被脚本和 SKILL 契约依赖，本批只加事实、不动它们。

## 决策

1. 新增模块 `src/daemon/service-supervision.ts` 承载系统级只读事实：单元名常量 `HERDSMAN_SYSTEMD_UNIT`、`managedByFromCgroup()`（纯函数，兼容 cgroup v1/v2，要求路径落在 `/system.slice/` 且等于 `<unit>` 本身或其子 cgroup，既避免把 `herdsman.service.dev` 误判成单元，也避免把同名用户会话单元判成系统单元）、`readProcessCgroup()`、`readSystemdRestartCount()`（默认 runner 用带 `timeout` + `killSignal: "SIGKILL"` 的 `spawnSync`，任何失败返回 `undefined`，从不抛出）。
2. `DaemonProcessDependencies` 增加可注入探针 `readCgroup` / `readServiceRestartCount`；业务逻辑不硬调 `execFileSync`，测试可用假 cgroup 文本 / 假 runner / 假 `systemctl` 脚本驱动。
3. 事实位只在**已知 daemon pid** 的那条返回分支上计算（pid 文件存在、进程存活且身份可确认）。该分支 `managedBy` 恒出现；`restartCount` 仅在 `managedBy === systemd:herdsman.service` **且**探针成功时出现（未托管时既不探测也不输出）。无 pid 的分支（pid 文件缺失、stalePid）不猜，整个键省略（不是 `unknown`）。
4. 超时取 `SUPERVISION_PROBE_TIMEOUT_MS = 1500`（触发 6 的魔术数）。理由：`daemon status` 是诊断命令，绝不能卡在卡死的 system manager 后面；健康的 `systemctl show` 是个位数毫秒，1.5s 足够宽松。这个上界只有在 `killSignal: "SIGKILL"` 前提下才成立（见二轮修复 M1），本机实测卡死 shim：`timeoutMs: 300` 下 301ms 返回、`restartCount: unavailable`、退出码 0。
5. 降级语义：cgroup 读不到**或读到空/纯空白** → `managedBy: unknown`（"什么都没读到" ≠ "不归该单元管"）；`restartCount` 探针任意失败（命令缺失 / 权限 / 超时 / 非零退出 / 输出异常 / 抛错）→ 在托管前提下省略该字段。`getDaemonStatus` 把两个探针调用各自包在 `try/catch` 里，保证 `state` 与退出码不受影响。
6. CLI：`herdsman daemon status` 默认仍是 JSON（SKILL.md 与 package README 按 JSON 解析，机器契约零变化）；新增 `--text` 输出人类可读文本，且在**系统单元托管**时对拿不到的 `restartCount` 显式写 `unavailable`，不静默省略；`unmanaged` / `unknown` 时不渲染 `restartCount` 行（与 JSON 省略该键一致）。

## 被放弃的方案（必填）

- **默认改成人类可读文本、JSON 走 `--json`（对齐 `agent list/get/read` 风格）**：会翻转既有默认 stdout，而 SKILL.md 明确让 agent 直接解析 `herdsman daemon status` 的 JSON，属破坏性变更。放弃，改为**基于向后兼容原则的代理自主设计决策**：默认 JSON 不变 + 新增 `--text`。（初稿此条写成"用户明确选择"，属捏造的确认过程，已在二轮修复 S5 改正——本决策未经用户确认。）
- **用 `systemctl show -p MainPID` / 单元状态反推 `managedBy`**：多一次外部命令，还引入"该 pid 是否当前 MainPID"的时序判断；`/proc/<pid>/cgroup` 是本地、只读、免命令的直接证据。放弃。
- **无 pid 时也去猜 `managedBy`（例如认为 socket 可达即 systemd 托管）**：那是猜测不是事实，会让"脱管"这一真正要看见的情况被误报成托管。放弃，改为省略。
- **`restartCount` 缺失时省略且文本视图也不提**：会在人类视图里制造"没有这个事实"与"事实为零"的歧义。放弃，文本视图用 `unavailable` 明示。

## 二轮修复（oracle 第二意见）

- **M1（致命）**：`spawnSync` 的 `timeout` 只发 SIGTERM（默认 `killSignal`），会无视 SIGTERM 的子进程会让这个**同步**调用一直挂着 → `daemon status` 被拖死。`defaultSystemctlRunner` 补 `killSignal: "SIGKILL"`，沿用 `src/herdr/session-list.ts` 的既有教训（`systemctl show` 是只读客户端，硬杀无副作用）。本机实测（shim = `trap '' TERM; exec sleep 8`；`timeoutMs: 300`）：**修复前 8003ms → 修复后 301ms**；单测从裸 `sleep 5`（会响应 SIGTERM，给假信心）改成该 shim 并断言 `elapsed < 1000ms`。
- **S1**：`restartCount` 不再无条件探测——只有 `managedBy === systemd:<HERDSMAN_SYSTEMD_UNIT>`（新helper `isSystemdHerdsmanManaged()`）才调探针；`unmanaged` / `unknown` 既不探测也不输出。理由：`NRestarts` 是 `herdsman.service` 的属性，该单元并不拥有脱管 pid，`managedBy: unmanaged` + `restartCount: 3` 会被读成"这个脱管 daemon 重启过 3 次"。
- **S2**：`managedByFromCgroup()` 从"只看 basename"改为"路径必须是 `/system.slice/<unit>` 或其子 cgroup"。同名用户会话单元（`…/user@1000.service/app.slice/herdsman.service`）与系统单元**字符串完全相同**，旧实现会让 ops 去 `systemctl restart` 错对象；现判 `unmanaged`，并有单测固定两侧行为。
- **S3**：① 类型注释原写"无 pid 时是 `unknown`"，与实现（整键省略）不符 → 注释改为如实描述（选改注释，因为省略是既定契约且单测已固定）；② 空/纯空白 cgroup 内容从 `unmanaged` 改为 `unknown`（"什么都没读到" ≠ "不归该单元管"）。
- **S4**：`CHANGELOG.md` 的 `## [Unreleased]` 补用户可见条目（默认 JSON 多两个可选键 + 新增 `--text`）。
- **S5**：删除捏造的用户确认（见上）。
- **S6**：口径与边界写进注释/笔记并与用户可见面同步——① `unmanaged` 只表示"不归 herdsman.service 系统单元管"，不等于没人托管、也不等于可以杀；② `restartCount` 是 systemd `NRestarts`，`reset-failed` / `stop`+`start` / 单元未加载都会清零，`0` ≠"从没崩过"；③ `spawnSync` 只允许诊断路径调用（会阻塞事件循环），已写在 `service-supervision.ts` 文件头、`defaultSystemctlRunner` 与 `supervisionFacts` 注释；④ `herdsman help` 的 `Notes:` 段落把这四条口径给了用户可见面。
- **未重构（本轮有意不动）**：事实计算仍留在 `process-manager.ts`（`supervisionFacts`），未抽到 `service-supervision.ts`；`--text` 的字段集与顺序不变。

### 关于两套输出约定的差异（触发 6 留痕）

`agent` 命令族是"默认文本 + `--json`"，`daemon status` 是"默认 JSON + `--text`"。差异是历史兼容的结果，不是疏漏：`daemon status` 的默认 stdout 自 0.13.0 起就是 SKILL.md 与 package README 直接解析的机器契约，翻转它属破坏性变更；`agent` 家族的默认文本形态同样是那批既有契约的一部分。两边各自向后兼容，不与同一风格对齐。

## 遗留 / 观察项（复审登记）

以下四条均为复审登记的非阻断项，本批不改行为，留待后续触发条件出现时处理：

1. **`NRestarts` 用户可见文案待补**：systemd 的 `NRestarts` 在**手工 `systemctl restart` 时也会清零**，故该字段主要反映的是 `Restart=` 自动拉起的次数。现有用户可见面（`herdsman help` 的 `Notes:` 段与 `CHANGELOG.md` 条目）只写了 `stop`+`start` / `reset-failed` / 单元未加载会清零，**未点明手工 `restart` 同样清零**，易被读成"restartCount 高 = 崩得多"的误读。后续在维护 help / CHANGELOG 文案时补上这一条口径。
2. **`isSystemUnitCgroupPath` 硬编码 `/system.slice/`**：当前生产单元生效配置为 `Slice=system.slice`（实测），故硬编码前提成立；若将来给单元加自定义 `Slice=`（例如迁到 `system-herdsman.slice`），`managedByFromCgroup()` 会把它判成路径不落在 `/system.slice/`，**静默降级为 `unmanaged`、`restartCount` 随之消失**。本批不引入额外命令开销；触发条件是"未来给单元加 `Slice=`"，届时须补 `systemctl show -p Slice` 交叉校验（或按配置注入 slice 前缀）。
3. **M1 残余理论边界：D 状态（不可中断睡眠）**：`killSignal: "SIGKILL"` 已解决"子进程无视 SIGTERM"这一现实形态，但 SIGKILL 对处于 **D 状态**的进程同样无法即时生效，理论上仍可拖住同步 `spawnSync`。对只读、无子进程的 `systemctl show` 客户端，进入 D 状态概率极低，且该残余边界无法在应用层彻底消除（需异步化或外层 watchdog），**维持现状**，不为此改同步探针结构。
4. **托管且 systemd 卡死时 `daemon status` 最坏 +1.5s**：`SUPERVISION_PROBE_TIMEOUT_MS = 1500` 是硬上界，只有在 `killSignal: "SIGKILL"` 前提下成立；即诊断命令在 system manager 完全卡死时的最坏代价是 +1.5s。按设计接受（诊断命令优先"不卡死"，次优先"快"），已在 `service-supervision.ts` 与 `defaultSystemctlRunner` 注释明写。

## 事故留痕

**2026-10-01 复审过程中误停生产 daemon 24 秒。**

- 经过：复审过程中对**生产单元**执行了启停验证，导致 systemd 托管的生产 daemon 被停掉约 24 秒。`journalctl` 记录显示：15:18:54 `Herdsman daemon shutdown finished { elapsedMs: 10, exitCode: 0 }` → 15:19:18 systemd 按 `Restart=` 重新启动。
- 现状：已自行恢复。当前 `MainPID=3824004`、`ActiveState=active`、`Result=success`、`NRestarts=0`。
- 原因：把"验证/审查类任务"与"部署/运维类任务"的权限边界混同，对生产单元直接执行了启停。
- 预防：**验证 / 审查类任务不得对生产单元执行启停**。需要事实位验证时，只允许只读探测（`systemctl show` / `journalctl` / `/proc`），启停类验证只能在隔离环境或显式获得运维授权的任务中进行。

## 来源

- 任务：T1「`herdsman daemon status` 增加只读的'被谁托管 + 重启次数'事实位」（本批）。
- 关联笔记：[撤销入口启动守卫：daemon 靠 systemd 常驻，排他只留在实例锁](20260929-drop-daemon-entry-guard.md)（其遗留清单第 1 条由本批实现）。
- 证据：`test/unit/daemon-process-manager.test.ts`（假 cgroup / 假 runner / 假 `systemctl` 缺失与 `trap '' TERM` 卡死 shim）、`test/unit/cli.test.ts`（`--text` 渲染与降级标注）、`test/unit/daemon-process-manager.test.ts` 的 `managedByFromCgroup` 系统切片/同名用户单元用例。

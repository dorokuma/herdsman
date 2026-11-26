## [Unreleased]

### Changed

- 仓库归档（2026-11-11）：herdsman 是老 herdr 时代的工具，能力已被 herdr-core（`hc`，`hc ls` / `hc get` / `hc wait` / `hc read` / `hc exec`）取代；本机运行态已确认为零进程、零服务、零安装二进制；仓库转为只读历史留存，不再开发、不再发布、不再部署。README 顶部新增归档状态节、AGENTS.md 顶部新增归档声明，决策记录见 `.agents/notes/20261111-archived-superseded-by-herdr-core.md`（该文件本机留存、不入 git）。

- 移除 husky：`core.hooksPath` 不再指向 `.husky/_`，hooks 改走全局 `/root/.git-hooks`（修复 worktree 中 husky hooks 不执行）；`.husky/` 目录删除。
- 新增根目录 `Makefile`，`make check` 为唯一检查入口（gofmt、`go vet`、`go test -race`、两个 `cmd` 的 `go build`）；CI（`.github/workflows/ci.yml`）改调 `make check`；README/AGENTS 文档同步。
- 去掉原 pre-commit 的 `go test -p 2`，测试统一为与 CI 一致的 `go test -race ./...`（有意变更）；提交前不再本地运行 Go 门禁，由 `make check`（CI 与合并前门禁）负责。

## 0.15.3

- 修复 Go 版 daemon 重启后遗留墓碑行永久吞掉活跃 pane 导致 workspace 消失的问题，放宽清理条件并对齐 store 层替换语义。

## 0.15.2

### Added
- Daemon 全量 Go 移植入库，在 Go 侧完整镜像 TypeScript daemon 行为并提供 Go CLI 与 daemon 二进制。
- 新增对 `pig`（PiG）作为 `pi` 家族 agent 的一等公民支持：支持 `~/.pig/agent/sessions` 会话目录发现、注册校验及 `pi`/`pig` 会话互认。
- Go 版 SQLite 并发门控新增 `HERDSMAN_SQLITE_GATE_WARN_MS` 告警阈值配置。

### Fixed
- 修复 `herdsman-pi` 扩展 socket 生命周期与退出问题：重连前销毁旧连接，非交互 `print`/`json` 及 `--no-session` 模式不再挂起事件循环，并完整保留 `toolResult` 条目。
- 修复 Go 版 SQLite 并发门控死锁问题：获取锁增加限时等待与超时强制释放孤儿锁机制，事务发生 panic 时确保回滚并释放锁。
- 更新 Herdr 探测方式并忽略运行时产生的临时 `.db` 文件。

## 0.15.1

### Fixed
- **agy 回合完成通知防丢失与停滞重放**：等待历史重试升级为指数退避（5s→60s）与可配置的 15 分钟弹性窗口（扣除活跃 working 时长）；计划丢弃时降级为停滞状态，支持晚到完成历史的一次性重放并防止回合中误判。
- **agy smart-wake 工作目录解析与跨工作区收敛**：从多种上下文来源解析分离式 waiter 的工作目录，修复全局 `hooks.json` 触发的 pane 错配报错；抑制仍在 working 的对端中间态事件；无活跃对端时支持 60s 待命，并支持向会话编排者投递无主兄弟工作区事件。
- **终态历史守卫过滤非 working 来源 `agent.idle`**：防止跨工作区单行确认后冻结基线守卫误拦合法终态正文。
- **agy 唤醒竞态与防误触加固**：
  - 因对端仍处于 `working` 而被暂时抑制的完成事件留待下一轮重判（同终端仅保留最新有效完成并确认旧事件），不再过早吞掉通知。
  - 触发唤醒前读取本窗格尾部检测 agy 输入框草稿，存在非空草稿时自动推迟唤醒，避免打断用户输入。
  - 完善事件确认水位防吞机制，防止高 ID 事件确认时误吞更低 ID 的未消费或推迟事件；并发写入已呈现事件 ID 时取并集避免覆盖。
  - 跨越 300s 重投窗口保留最多 100 条候选与待确认事件，并核验源窗格存活与工作区作用域，自动剔除已关闭或被接管窗格的陈旧事件。

## 0.15.0

### Added
- 新增 Antigravity（agy）环境编排者支持与分离式唤醒等待机制。
- 新增 `PLAN_STALLED` 收敛状态，使计划停滞可被观测和上报。

### Fixed
- 修复 agy plan 模式等待历史误判与事件投递问题：`working` 期间豁免等待历史超时以防止被提前判死，统一 `pi` 与 `agy` 投影门控及丢弃事件推送判据。
- 修复孤儿与交互式 agent 的事件投递、会话元数据时间戳解码及唤醒等待重试容错问题。

## 0.14.9

### Fixed
- 修复唤醒投递水位屏障收敛问题：仅依据服务端返回的已确认游标收割本地水位屏障，合并事件时以服务端字段为准，满页（100 条）截断时保守保留更高 ID 的本地事件。
- 按 `paneGeneration` 跨批次保留完成证据，同一 pane 同代际完成事件离开待处理窗口后仍可准确压制迟到的降级失败事件，同时正常放行新代际事件；无代际事件不再误压制后续降级事件。
- （注：本版本同时包含 `v0.14.7` 的双 HOME agy 会话发现能力；`v0.14.8` 已跳过未发布。）

## 0.14.8

- 跳过未发布，全部改动随 `0.14.9` 发布。

## 0.14.7

### Added
- 支持 `agy` 双 HOME（主 `HOME` + 额外 HOME）会话库解析：按主 `HOME` 优先再查额外 HOME 列表的顺序定位 `<home>/.gemini/antigravity-cli/conversations/<id>.db`，支持通过 `AGY_EXTRA_HOMES` 环境变量（冒号分隔，设为空串可禁用默认额外目录）配置并按规范路径去重，变更后立即使负缓存失效。

### Changed
- 冻结 `PLAN_WAITING_HISTORY` 契约常量并统一内部引用，补充同正文不同 ref 轮次正常放行的边界覆盖。

## 0.14.6

### Added
- 为陈旧重复内容守卫与挽救路径新增窗口饱和一次性告警日志（每进程每身份/每窗格触发一次），在历史扫描达到 500 条上限时提供可观测信号。

## 0.14.5

### Changed
- 收窄 mismatch 分支重复内容守卫并放宽扫描窗口至 500 条，防止已投递的旧正文在重读尾部时被当成本轮新答案重复投出。
- 新增数据库部分索引以加速终态事件守卫查询（仅新增索引，不修改数据）。
- 空轮次（无有效 assistant 内容）不再产生 `agent.failed` 事件唤醒编排者，改为按窗格去重输出告警日志；`herdsman-pi` 对空正文 `completed` 事件改为静默确认而不向编排者渲染占位消息。
- 冻结降级与空轮次告警相关指标键与消息前缀契约常量，并统一部署与发布验证流程说明。

## 0.14.4

### Fixed
- 修复未确认的「冻结基线」轮次误抹空正文的问题：当盘上已有未曾投递过的合法非空终态正文时直接采信放行，不再降级为空消息。
- 修复关页导致孤儿终态正文丢失的问题：plan 对应 agent 已消失时，若存在合法终态正文且可推断投递作用域，则通过孤儿事件通道正常投递。
- `herdsman-pi` 在清空死信重试事件列表时补充日志记录，并将确认水位读取收敛为仅读取状态对象。

## 0.14.3

### Fixed
- 修复 `herdsman-pi` 忙碌时唤醒投递拆轨导致丢消息或误确认的问题：忙碌编排者上的唤醒完整延后至空闲后再以可触发 turn 的方式投递，移除 5s 硬超时与不触发 turn 的旁路队列。
- 修复忙碌兜底退避上限未生效的问题（真实钳制在 5s 上限）并完善已确认事件的死信重试状态清理。
- 为未进入 transcript 的死信唤醒增设重试上限（最多 2 次，间隔 5s 与 15s），忙碌自旋按梯度退避，并为被拦下的唤醒显示状态提示。
- 静默确认增加唤醒水位屏障约束，避免确认跳过本地尚未消费的事件，被阻塞时每 60s 提示一次原因与数量。

## 0.14.2

### Changed
- 移除 `HERDSMAN AGENT CONTEXT` 预览块注入及仅服务于该块的截断与缓存逻辑（完整报告始终通过唤醒消息投递，实时状态可通过 herdr 查询），`context` 钩子保留旧会话重放清洗功能。
- 统一忙碌唤醒投递为单轨 follow-up 队列，按事件 ID 升序投递，不丢不重。

### Fixed
- 将唤醒续传预算改为按事件独立记账（单事件最多 5 次后 write-off），避免新事件刷新滞留事件预算导致的刷屏空转循环。

## 0.14.1

### Fixed
- 修复幽灵唤醒循环：续传预算（5 次）耗尽后自动 write-off 并确认滞留事件，避免 daemon 无限重投。
- 修复显式关页时已投递事件重复保留问题：关页时直接确认已投递行，未投递行仍保留一次投递机会。
- 修复唤醒摘要与上下文正文归一化过度压缩问题：保留换行与行内缩进（仅折叠 3 个以上连续换行并去除行尾空白），消除内容被截断的假象。

## 0.14.0

### Breaking Changes
- 收窄 agent 历史读取支持面为仅支持 `pi` 与 `agy`：移除 `claude`、`codex`、`opencode`、`grok`、`gemini` 历史读取器及扫盘/标题猜测兜底，不再回退持久化缓存 ref；升级后 `herdsman agent get` 与 `agent read` 对非 `pi`/`agy` 的 pane 返回空历史。

### Changed
- `herdsman-pi` 唤醒投递改为以隐藏消息的消费证据为准，仅推进连续可确认的最小前缀水位；已有消费证据的事件即使当轮以错误结束也照常确认；续跑上限设为 3 次并在耗尽时提示用户；角色/作用域重置时将未消费在途事件转入跨作用域抑制集。

## 0.13.6

### Fixed
- 将 daemon 采信盘上终态正文的判据从「已确认」解耦为「已收到信号且盘上有可交付的非空终态正文」，即使上报指纹与盘上内容因改写或脱敏不符也不再抹空正文（仍记录告警日志）。
- 将陈旧重复内容守卫的检查范围从最近 1 条扩大到最近 200 条已投递/已确认终态行，同时排除非 `working` 起点且不可投递的 `agent.idle` 行以防误伤有效轮次。

## 0.13.5

### Fixed
- 修复空唤醒（空 `agent.done`）链路中的四项问题：
  - daemon 在已确认回合且盘上已有非空终态正文时采信盘上内容，不再因指纹不符而置空。
  - `herdsman-pi` 落盘确认从「文件体积增大」改为文本匹配，避免将无关写入误判为本轮已落盘。
  - 写入上下文快照时防御 agent 行已被删除的外键约束错误，自动降级为内存快照并记告警日志，避免阻塞整批处理。
  - `failed` 与 `discarded` 唤醒消息在有基线正文时附带 `last assistant (pre-round)` 摘要。

## 0.13.4

- 补充版本发布落地记录及发布流程文档说明（registry 传播等待与前置检查时点），无功能代码改动。

## 0.13.3

- 稳定并硬化锁与套接字相关测试（将固定延时改为条件轮询与有界等待、补齐失败路径清理与降级覆盖），并完善部署、发布及运维回滚文档，无功能代码改动。

## 0.13.2

（注：本版本包含 0.13.0、0.13.1 与 0.13.2 的合并变更。）

### Breaking Changes
- 移除 `herdsman daemon start|stop|restart` 子命令，CLI 仅保留只读的 `herdsman daemon status`；生产环境启停统一由 systemd（`herdsman.service`）托管。
- daemon 启动入口移除是否由 systemd 托管的策略判断，同数据目录仍通过文件锁（flock）保证单实例。

### Added
- `herdsman daemon status` 新增可选字段 `managedBy` 与 `restartCount` 监督信息，并新增 `--text` 人类可读输出视图。
- 新增 `agent.discarded` 终态失败语义支持与历史降级数据自动迁移（从 `0.12.1` 升级首次启动时自动执行，无需手工操作）。

### Changed
- 缩短检测等待（固定 200ms × 8 次，总预算 1.5s）并有界化唤醒延迟（取消无定时器挂起，忙碌时最多 5s 后裁决放行）。
- 为 daemon 关停序列新增 5s 总预算与先 abort 再 drain 机制，为外部 `herdr` 调用设置 2s 超时上限，并支持二次信号快速清理退出。

### Fixed
- 修复确认回合因基线已含本轮答案而被抹空正文的问题，并新增陈旧重复正文守卫防止重发上一轮内容。
- 修复关页/退役导致 `agent` 行被删后的外键约束丢失终态事件问题，支持以 `agentId = null` 孤儿事件降级落库并回填历史遗漏行。
- 修复高负载下 daemon 文件锁存活误判（区分 `/proc` 读取瞬时错误与进程退出）、release 屏障未等待所有共享 fd 子进程退出的问题，以及启动阶段被暂停子进程的处理。

## 0.13.1

- 为 daemon 关停序列新增 5s 总预算与先 abort 再 drain 机制，为外部调用设置 2s 超时上限，并支持二次信号快速清理退出。（未单独发布，随 `0.13.2` 一并发布。）

## 0.13.0

- 移除 `herdsman daemon start|stop|restart` 子命令（仅保留只读 `status`），简化 daemon 启动锁检查，并加固唤醒投递链与数据库迁移升级路径。（未单独发布，随 `0.13.2` 一并发布。）

## 0.12.1

### Changed
- 收紧 pi 终态事件门控：支持通过 `expectedText` 指纹校验历史是否推进，非终态 `stopReason` 不再作为终态发出。
- 当历史未推进、指纹不匹配或非终态时，降级事件附带明确降级原因（`expected_text_mismatch` / `no_advance_from_input` / `non_terminal_assistant`）并进入有界 10s 重试，待历史补齐后重新发出真实终态。
- `herdsman-pi` 统一正文提取与脱敏语义，不发送空 `expectedText`，并过滤降级与上游报错类的无效唤醒结果（支持配置过滤开关及额外匹配模式）。

## 0.12.0

- 新增上游模型报错过滤配置（`wake.filter_upstream_errors` 默认开启及 `wake.extra_upstream_error_patterns`），命中 429/529/限流/网络超时等报错文本时静默丢弃唤醒并确认事件。（未单独发布，随 `0.12.1` 一并发布。）

## 0.11.7

- 修复 status plan 等待超时误报并新增 `agent.discarded` 生命周期，将观察者等待超时与 agent 崩溃解耦。
- 修复 agy 迟到 `idle` 事件误吞已完成轮次的问题，仅基于完成态记录已投递基线并在插入新 plan 时取消旧的待处理 idle plan。（未单独发布，随 `0.12.1` 一并发布。）

## 0.11.6

- 统一唤醒展示线脱敏管道，全面剥离零宽字符与双向文本（bidi）控制字符后再进行脱敏。

## 0.11.5

### Changed
- 优化上下文注入的单行紧凑格式化与按词边界安全截断，支持跨年 `MM-DD` 时间戳格式。

### Fixed
- 加固文本清理与密钥脱敏防御（剥离零宽及不可见格式字符），并修复 30 天历史缓存清理与 agent 上下文绑定问题。

## 0.11.4

### Changed
- 会话注册通过 Linux `SO_PEERCRED` 校验连接进程工作目录与 Herdr pane 的一致性，拒绝错配的 connector。
- 历史 JSONL 读取改为流式处理，超出限制时仅读取 32 MiB 尾窗并在单条记录过大时抛出错误。

### Fixed
- 历史读取与 Pi 扩展密钥脱敏规则补齐裸 `Bearer` 与 `sk-` 前缀匹配。

## 0.11.3

- 修复短命新 pane 通过先验 `working -> idle` 状态跃迁唤醒 owner 避免错过订阅窗口，改进 Herdr socket 订阅重启与拓扑按 pane 过滤机制。

## 0.11.2

- 将发现会话根目录迁至按 Herdr 会话隔离的子目录以根除跨会话错配，恢复派发子代理唤醒正文并修复 idle 误杀。

## 0.11.1

- 将状态迁移与事件计划写入合并为同一 SQLite 事务，加固 Pi 唤醒信号后强制重读、超时未推进置空防陈旧终态、重启回填失败事件与投递耗尽错误日志。

## 0.11.0

- 新增 agy 终态就绪闸门（空正文不落库终态事件），将 WAL `-wal`/`-shm` 纳入历史指纹识别，新增等待历史 10 秒重试链与 `agent.failed` 可见性事件。
- 将 daemon 实例锁升级为内核 `flock` 握手机制，基于 lease 租约加固事件回收，并将插件支持平台声明收窄为 Linux。

## 0.10.2

- 新增 `status_event_plans` 持久化补偿表（含数据库迁移），支持启动与周期重试排空以防止运行时丢失终态事件。
- 改用消息引用与计数作为历史推进判据，新增单调逻辑时钟防止系统时间回拨或重启导致事件时间戳倒退，并新增 daemon 入口实例独占锁。

## 0.10.1

- 修复 daemon 启动守卫与 PID 文件生命周期管理，启动前探测 socket 可达性以防止双 daemon 并存，并将 socket 可达统一判定为运行中状态。

## 0.8.6

### Fixed
- 修复事件查询 SQL 括号逻辑与遗留关闭逻辑导致的误失效问题。
- 修复 Grok HOME 目录隔离与 session 路径所有权校验。
- 修复唤醒请求空窗、turn 信号消费、启动 reconcile 误释放 owner 及运行时记录问题。

## 0.8.5

- 修复空 assistant 历史回传、用户按 ESC 后事件无限重传及收敛前重复回收投递问题。

## 0.8.4

- 增强 daemon 对 orchestrator 确认拒绝与事件投递的结构化日志，并在游标推进时批量清理游标以下遗留事件。

## 0.8.2

- 新增 Pi 扩展 turn-completion 完成信号，daemon 在限定窗口内等待信号后再发出 `agent.done` / `agent.blocked` 事件，确保结果携带最新正文。
- 修复陈旧完成信号被下一轮误命中的竞态，并改用 socket 已注册的 Pi 身份校验信号来源。

## 0.7.0 (2026-08-23)

- 接入 Grok 与 agy 历史发现和读取支持，新增 `grokHome` 元数据及安全校验，并移除旧名称残留。

## 0.6.6

- Fix an infinite re-wake loop when acknowledging invalidated orchestrator events, and prune stale pending events using server-acknowledged IDs.
- Resolve Pi id-kind session refs directly before fallback discovery, normalize candidate working-directory paths, cap snapshot excerpts at 2000 characters, and skip re-pushing unchanged context snapshots.

## 0.6.5

- Hotfix: vendor the JSON-lines decoder inside `herdsman-pi` so standalone TypeScript loading no longer fails on an unresolved path alias.

## 0.6.4

- Fix delivery cursor deadlocks on transient disconnects, count idle events as outcomes only when transitioning from working, and invalidate pane events when replacing an agent.
- Validate authoritative session refs against the allowlist, paginate pending scans past noise windows, bound history discovery with stricter ownership checks, enforce `0700`/`0600` data directory permissions, reset reconnect backoff after subscription, and escalate readiness failures from `SIGTERM` to `SIGKILL`.

## 0.6.3

- Prevent worker panes from receiving the orchestrator's own transcript during fallback history discovery, treat late session refs as identity changes, and remove closed panes from injected context immediately.

## 0.6.2

- Allow the orchestrator cursor to advance past events that became undeliverable after delivery (such as retired worker panes) while continuing to reject skips over deliverable or sealed events.

## 0.6.1

- Unify deliverable-event predicates across discovery, acknowledgement, and publication, drop empty wake turns, and avoid aborting in-flight turns on transient disconnects.
- Rebuild the Herdr client with exponential backoff on subscription failure, discard stale refreshes, and harden observability sockets, exclusive PID startup, readiness polling, and log rotation.

## 0.6.0

- Forked from `@ryonakae/herdsman` at `v0.5.1`; previous history is inherited from upstream.

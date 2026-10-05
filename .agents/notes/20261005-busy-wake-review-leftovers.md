---
status: active # active | superseded
superseded_by: ""
supersedes: ""
# 模块可选值: observability, daemon, cli, config, db, herdr, shared, herdsman-pi, herdsman-herdr-plugin, release
模块: herdsman-pi
---

# fix/busy-wake-delivery 收尾轮遗留清单（reviewer / oracle 建议级发现，未修）

## 一句话结论

收尾轮（`4679db8` 之后）reviewer 与 oracle 均判定放行、无 must fix / should fix，但各提出建议级发现；按项目约定建议级发现必须落盘闭环，本笔记收纳四条：① reviewer——`pruneAcknowledgedEvents` 补的对称清扫在真实路径下被 `dropUnackedDelivered` 遮蔽（常态不可达），属防御性对称，留待大重构收口；② oracle（M-1）——不改 OBS-1 的决定成立，但存在更便宜的观测手段（`clearDeliveryBookkeeping` 对已写 off 的 id 补一条与 carriedOver 同形状的 info 日志，零行为变化），留待后续迭代；③ oracle（L-3）——`wakeRetryableEventIds` / `wakeDeadLetterAttempts` / `wakeDeadLetterRetryAt` 可收敛为一张 `Map<id, {attempts, retryAt}>`，属重构项、不是合并前置条件；④ oracle（第四轮）——它自认无法独立证实前三轮审查的观察项均已落盘，合并后需对照前三轮 reviewer / oracle 报告原文，与两份笔记的「决策 / 被放弃方案」节做一次条目清点，属流程性核对、不阻塞本轮合并。四条均不阻塞本分支收尾，触发条件写入各条。

## 背景

本分支经三轮返修后进入收尾轮双审放行。oracle 的 M-1 同时指出 `20261005-busy-backoff-cap-and-retry-sweep.md` 决策第 4 条有一处事实性含糊（`alreadyPresented` 的检查顺序），该措辞已直接订正进那份笔记（结论不变，见其「来源」节交叉链接）；本笔记只承载四条**未修**的建议级发现（第 4 条为 oracle 第四轮的流程性待办，无代码定位）。

为何新建而不是追加到既有笔记：最贴切的 `20261005-busy-backoff-cap-and-retry-sweep.md` 是 `4679db8` 的**已决决策记录**（本笔记已按要求只订正其事实措辞），而 `.agents/notes/README.md` 的历史笔记不可变原则不支持把开放遗留项混进已决记录；`wake-delivery-r4f1-remaining-risks.md` 属另一条工作线（R4F1 合同轮、以 daemon/db 条目为主），追加会模糊其范围。四条中前三条同属收尾轮、同模块（herdsman-pi），第 4 条为流程性待办；单独成篇才能在 INDEX.md 里有一个可检索的落脚点。

## 遗留项

### 1. `pruneAcknowledgedEvents` 的对称清扫是双重兜底（reviewer 建议）

- **来源角色**：reviewer（收尾轮放行评审的建议级发现）。
- **位置**：`packages/herdsman-pi/src/index.ts`——`pruneAcknowledgedEvents`（`:623`）内的 `wakeRetryableEventIds` 回路（`:662-664`）与 `wakeDeadLetterAttempts` / `wakeDeadLetterRetryAt` 回路（`:668-673`）；遮蔽它们的真实路径是队列回路（`:638-640`）经 `dropUnackedDelivered`（定义 `:567`，三字段删除 `:572-574`）。该遮蔽关系在代码注释（`:659-661`）里已写明。
- **当时不修的理由**：真实路径下，≤ ack 水位的 id 必在 `unackedDelivered` 里——awaiting 与 dead-lettered 的 id 都留在队列内等 daemon 水位推进（后者是刻意的 ack watermark 屏障）——队列回路先把它们全部路由进 `dropUnackedDelivered`，那里已删除同一批字段；`:662-673` 的成组清扫因此在常态下不可达，是防御性对称。它防的是未来某条不经队列的清扫路径漏清这三个字段，收益在未来；当前保留现状不增加风险，且注释已把关系写明，不存在「读者误以为这里是主路径」的问题。
- **将来什么条件下值得处理**：后续大重构统一 wake 记账的清扫职责时收口——例如只保留 `dropUnackedDelivered` 一条删除路径、删掉重复回路（与遗留项 3 的字段收敛天然同批）；或当新增第二条不经队列的清扫路径时，重新评估这对回路是否仍只是纯防御。

### 2. OBS-1 不改的结论成立，但可加一条零行为变化的观测日志（oracle M-1）

- **来源角色**：oracle（收尾轮 M-1；其对 `alreadyPresented` 检查顺序的事实性指出已订正进 `20261005-busy-backoff-cap-and-retry-sweep.md` 决策第 4 条，结论不变）。
- **位置**：`packages/herdsman-pi/src/index.ts`——`clearDeliveryBookkeeping`（`:598-621`，既有 carriedOver info 日志 `:602-606`，`wakeRetryableEventIds` 清空 `:614`）；写 off 入账在 `writeOffStrandedWakeDelivery`（retryable 入列 `:1165`，budget / deadline `:1171-1179`，warn 日志 `:1183`）。
- **当时不修的理由**：不改 OBS-1 的决定成立——复现链（注入 → 未拿到消费证据就 abort → settle 写 off → scope 重置 → daemon 重投 → 再注入 → 陈旧的 follow-up 副本之后才被排干进 transcript）依赖 Pi 内部 follow-up 队列行为，扩展侧没有观测点，`createFakePi` 又只记录 `sendMessage` 入参、没有 follow-up 队列模型；能写出的测试只能断言「重置后重投的 id 又被注入了一次」，而这正是当前契约的预期行为（写 off 的语义就是内容没进 transcript、重投是唯一补救），修前修后都过，锁不住任何缺陷。
- **更便宜的观测手段（留待后续迭代）**：在 `clearDeliveryBookkeeping` 对已写 off 的 id（彼时仍在 `wakeRetryableEventIds` 里、随 `:614` 一并清出的 id）补一条与 carriedOver 日志同形状的 info 日志（`[herdsman-pi] ... eventIds=... · ...`，一次一条不刷屏）。零行为变化、只增加可观测性：scope 重置发生时有哪些写 off id 随之离场，从此在日志里可查，正是复现尝试缺的那个观测锚点。
- **将来什么条件下值得处理**：需要现场定位「写 off 的 id 到底有没有被重投、是否撞上 scope 重置」时（OBS-1 的复现尝试同样需要这个锚点）；若 Pi 将来暴露 follow-up 队列的查询 / 清空 API，可把该猜测升级为可断言行为，届时连同这条日志一起评估。

### 3. 三个 dead-letter 记账字段可收敛为一张 Map（oracle L-3 重构建议）

- **来源角色**：oracle（收尾轮 L-3，重构建议）。
- **位置**：字段声明 `packages/herdsman-pi/src/index.ts`：`wakeRetryableEventIds`（`:224`）、`wakeDeadLetterAttempts`（`:232`）、`wakeDeadLetterRetryAt`（`:234`）；三字段的初始化在 `HerdState` 字段表 `:455` / `:456` / `:457`（`new Set()` / `new Map()`，此前全篇未列，补上——收敛为一张 Map 时这三处初始化位置也要跟着改成单个 `new Map()`）；五处删除点删除的字段子集并不相同（第五处由 oracle 第六轮观察补入）：`dropUnackedDelivered`（`:572-574`）删齐 trio 三项；`pruneAcknowledgedEvents`（`:623`）内部的水位线清扫（第四处删除点：`wakeRetryableEventIds` 回路 `:662-664` 删该集合，`wakeDeadLetterAttempts` / `wakeDeadLetterRetryAt` 回路 `:668-673` 成对删这两个 Map，随 ack 水位合计 trio 三项离开——原清单漏列此删除点，只读点清单亦漏一格，订正来源为 oracle 第五轮观察）；注入后批次清理（`:1465-1466`）只删 `wakeRetryableEventIds` 与 `wakeDeadLetterRetryAt`（attempts 不在此删）；消费确认清理（`:1998`）只删 `wakeRetryableEventIds` 一个键，`wakeDeadLetterAttempts` / `wakeDeadLetterRetryAt` 在该路径不动——其安全性来自 `deadLetterRetryDue` / `deadLetterRetryAt`（只读点 `:769-782`）均先查 `wakeRetryableEventIds.has`：键已在 `:1998` 删掉，两个 Map 里的残留项便永不可达（惰性残留，兜底依靠 trio 的键门控）；第五处删除点：`writeOffStrandedWakeDelivery`（`:1155`）里 `wakeDeadLetterRetryAt` 的预算耗尽分支 `else`（`:1178`），字段子集为 retryAt-only——attempts 已达 `WAKE_DEAD_LETTER_RETRY_LIMIT` 时不写新时点、直接删键（`set` 分支在 `:1173`），订正来源为 oracle 第六轮观察。将来按字面做 Map 收敛时须知：消费确认处只删一次键（原表述称该路径三字段齐删，与源码不符，已按源码订正；订正来源为 oracle 第四轮观察）；另有成组清空（`:614-619`）、写入（`:1165-1179`、attempts 自增 `:1459-1463`）与只读（`:713`、`:769-782`、`:813`、`:1103`、`:1171`、`:1288`、`:1457`，外加 `writeOffStrandedWakeDelivery`（`:1155`）的 warn 日志取 `wakeDeadLetterAttempts` 值处（`:1183`）——Map 收敛为一张后该日志形状需跟着改）。
- **引用粒度口径**：上列引用并存两种粒度——「回路级范围」（如 `:662-664`、`:668-673`，覆盖含 `for` 头的整段回路）与「语句级」（如 `:572-574`、`:1998`，落在具体的 `state.<字段>.<操作>` 行上）；区别是前者把回路内所有语句记为一格、后者只记真正执行写操作的那一行（本清单第四处删除点里，真正的删除语句是回路内的 `:663` 与 `:670-671`）。两种口径均予保留、不重排既有引用，仅在此把差异写明，读数时以语句级为准；删除点计数按删除语句的归并位置算，即五处（`dropUnackedDelivered` 一组三条语句、`pruneAcknowledgedEvents` 两条回路、注入后两条语句、消费确认一条语句、`writeOffStrandedWakeDelivery` 的 `:1178` 一条语句）。行号口径同「来源」节，仍以 `4679db8` 的 `packages/herdsman-pi/src/index.ts` 为准；但本轮（第六轮）新增的引用实为 6 个而非 3 个（`:455` / `:456` / `:457` 三条初始化、`:1171` attempts 读取、`:1173` retryAt `set` 分支、`:1178` retryAt 删除；原句只列三组、漏计了 `:1173`——此订正只把句子写对，不另立修复项），它们在当前工作树上读数不同——那里 `X-1` 补修（`2934393`）已把相关函数整体下移，三条初始化语句在 `:471` / `:472` / `:473`，attempts 读取在 `:1279`，retryAt `set` 在 `:1281`，retryAt 删除在 `:1286`。此外 `X-1` 补修本身还新增了三个只读点，基线 `4679db8` 无对应行、仅存在于 HEAD 口径（均已逐行核对，位于 `unconfirmedWatermarkBarrier`，HEAD `:1165-1190`）：`:1172` 展开 `wakeRetryableEventIds` 计入 `blocked`、`:1175` 以 `state.wakeRetryableEventIds.has` 门控队列首个阻塞 id、`:1180` 读 `wakeDeadLetterAttempts` 判预算是否耗尽（决定 reason 取 `dead-letter-budget-exhausted` 还是 `dead-letter-backoff`）。Map 收敛为一张时，`:1180` 这类「按 id 读 attempts 判预算」的读取语义必须保持——收敛形状 `Map<eventId, { attempts; retryAt }>` 天然保留该读法，但若收敛丢掉 attempts 维度或更换预算判据，屏障 reason 即失真。两套行号同指同一处语句，按所在基线对照即可。
- **当时不修的理由**：纯重构项，不是本分支合并前置条件，收尾轮只放行建议、不返工。三个字段的语义（可否再投递 / 已花费预算 / 下次重投时点）在注释里已按 trio 成组维护；收敛收益是把五处删除并为一处、漏删其一的风险下降（其中第四处那对回路收敛后天然就是「单点删除」的形状；第五处则不能整键删除——源码注释（基线 `:1169-1170`、HEAD `:1277-1278`）明确预算已耗尽的 id 仍「stays a barrier, not a turn driver」，收敛后的单 Map 对第五处须保留键、只清该 id 的 retryAt（或等效语义），否则会连带丢掉「仍是屏障、不驱动 turn」的行为），不改变任何行为——没有行为变化就没有回归收益，单独为它开一轮不值。
- **将来什么条件下值得处理**：下一次大改触及 trio 任一字段时，顺手收敛为 `Map<eventId, { attempts: number; retryAt: number }>`（遗留项 2 的日志与遗留项 1 的清扫收口是天然同批触发器；注意消费确认处只删键，收敛时该路径仍只删一次键）；与遗留项 1 并入同一轮「wake 记账收敛」做，不单独为重构返工。
- **第七轮豁免记录**：oracle 第七轮（对 `1e8247d` 判定通过不阻断）另提 M2（交叉核对计数）、M4（五处 / 六处口径判据）、M5（序号基准）、M6（测试侧触碰点）及若干低优观察项，均属措辞完善类；经维护者当次决策豁免（2026-10-05），不随本笔记修改。

### 4. 合并后对照前三轮报告原文清点观察项落盘情况（oracle 第四轮流程待办）

- **来源角色**：oracle（第四轮评审；其对 `581d183` 的判定为通过、分支合并无实质风险，本待办来自其自认无法独立证实事项的后续建议）。
- **位置**：无代码定位，属流程核对。核对对象是两份笔记的「决策 / 被放弃方案」节——`20261005-busy-backoff-cap-and-retry-sweep.md`、`20261005-busy-wake-review-leftovers.md`；核对依据是前三轮 reviewer / oracle 报告原文。
- **当时不处理的原因**：oracle 自认无法独立证实「前三轮审查的所有观察项均已落盘」；逐条清点需要三轮报告的完整原文比对，属合并后的流程动作，不阻塞本轮合并（本轮只追加笔记收尾 commit，不改代码、不加测试）。
- **将来什么条件下值得处理**：合并后择机，或下次触及 wake 投递 / dead-letter 记账模块时，对照前三轮 reviewer / oracle 报告原文，与上述两份笔记的「决策 / 被放弃方案」节做一次条目清点，确认每条观察项都有对应落盘或已决结论。

## 决策

本笔记只把四条发现落盘闭环，不构成新方案、不改任何代码、不加任何测试断言；四条均判定「当前不做」，后续触发条件已写入各条（第 4 条的触发条件是「合并后择机或下次触及该模块时」）。遗留项 3 的位置表述按 oracle 第四轮观察订正——消费确认清理（`:1998`）只删 `wakeRetryableEventIds` 一个键，另两个字段在该路径保留且因 `deadLetterRetryDue` / `deadLetterRetryAt` 先查 retryable 而不可达——其 Map 收敛的重构建议与结论均不变。oracle M-1 对 `20261005-busy-backoff-cap-and-retry-sweep.md` 的事实性订正（`alreadyPresented` 检查顺序 retryable 在最前，suppressed 拦不住仍在 retryable 集合里的 id）已直接改进该笔记的决策第 4 条与对应被放弃方案条目，原决策结论不变。

## 被放弃的方案（必填）

- **把四条遗留项中的任意一条在本轮顺手做掉**：本轮已是三轮返修后的收尾轮，reviewer 与 oracle 均无 must fix / should fix；任何一条落地都会重新打开评审面（日志形状要过 observability 惯例、Map 收敛要动七个以上读写点及其测试替身），超出「只含文档与笔记的收尾 commit」的授权范围。
- **只刷新 INDEX.md、不写本笔记**：建议级发现不落盘即视为未闭环，违反项目约定（决策与踩坑留痕须记入 `.agents/notes/` 并执行 `scripts/notes-index.sh`）。

## 来源

- 分支 / 提交：`fix/busy-wake-delivery` 的 `606edcb` → `abb357b` → `4679db8`（连同 `581d183` → `be6a24b` → `70963e8` → X-1 `2934393` → `6c6c343` 已随合并提交 `e63e65b` 合入 main 并已 push，`main = origin/main = e63e65b`；未入 main、未 push 的仅剩其上的 docs commit `1e8247d` 与本轮收尾 commit——原记「未 push、未合并回 main」在 HEAD 上已是假陈述，此处改为事实状态）；本笔记为纯文档收尾，不动 `packages/` 下任何代码逻辑、不动版本号与 CHANGELOG、不碰 daemon、不新增测试断言。
- 评审：收尾轮 reviewer 与 oracle 均判定放行（无 must fix / should fix）；上述四条为各自提出的发现——第 1 条 reviewer，第 2、3 条 oracle（收尾轮），第 4 条 oracle（第四轮，流程性待办、无代码定位）。
- 代码行号均以 `4679db8` 时的 `packages/herdsman-pi/src/index.ts` 为准。相关笔记：`20261005-busy-backoff-cap-and-retry-sweep.md`（M-1 订正落地处）、`20261005-dead-letter-retry-budget.md`（dead-letter 记账语义前置）、`20261004-busy-wake-defer-to-settled.md`（busy wake defer 语义前置）。

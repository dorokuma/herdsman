# 遗留事项（consider）— SqliteGate 孤儿资源延迟 Release 的代次隔离

**来源**：fix/sqlite-gate-deadlock-watchdog 提交门禁 reviewer 终审 consider 项，主代理分诊 out-of-scope 不阻断，按 §4 落入遗留清单交用户过目。

**现象**：`RunSqliteTransaction` 的 `fn()` panic 时，recover 路径先 `db.ExecContext("rollback")`（持锁重入），再 `gate.ReleaseAll(gid)` 把该 goroutine 名下残留的 gate level 一次性归位（`depth→0`、`owner→0`、`broadcastLocked()`）。被提前结算的孤儿 `*sql.Rows` / `*sql.Stmt` 资源本体不关闭，只归位其 gate level。

**理论隐患**：这些孤儿 `*sql.Rows`/`*sql.Stmt` 若稍后被异步或延迟 `Close()`，其内部 `r.rows.Close()` 无 gate 保护、且其 `Release(r.gid)` 携带的是旧 `gid`——若同一 `gid` 在之后被复用跑了新事务，迟到的 `Release` 可能命中并误降新事务的 depth。

**当前评估**：生产 `RunSqliteTransaction` 回调均在同 goroutine 同步执行，无「取消后异步关 rows 再复用同 gid 跑新事务」的调用形状；`Release` 对非 owner 是 no-op，不会把 depth 压负。属理论隐患，不阻断本轮收口。

**若需彻底隔离**：可在 `SqliteGate` 为每次顶层 hold 引入递增代次（generation token），`Release` 携带代次、非当代 hold 的迟到 Release 一律 no-op；或在连接层追踪事务内未关资源。改动面大于本轮最小修复，登记待办。

**关联文件**：`go/internal/shared/sqlite_gate.go`、`go/internal/shared/sqlite_transaction.go`、`go/internal/db/client.go`（serializedRows/serializedStmt/serializedTx）。

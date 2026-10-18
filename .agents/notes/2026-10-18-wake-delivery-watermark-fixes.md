# Wake Delivery Watermark & Barrier Fixes

- module: herdsman-pi
- date: 2026-10-18
- status: active

## 修复项（经四轮 verifier + oracle 复审闭环）

1. **水位屏障只由服务端 cursor 回收**：`syncPendingEventsFromServer` 只接收调用方传入的 `serverAckedEventId`（`response.state?.ackedEventId ?? response.ackedEventId ?? 0`），页内事件状态（`pending`/`delivered`/`failed`）不再参与水位计算。缺省值 0 为 fail-closed（漏传时不收割）。
2. **连接恢复顺序**：`applyConnectionStateResponse` 先 `pruneAcknowledgedEvents`（用同一游标）再 `syncPendingEventsFromServer`，避免同轮被裁剪的屏障被重新保留。
3. **完成证据按 pane generation 保留**：`completedPaneKey(paneId, paneGeneration)` 对空代际返回 `undefined` 不建键不抑制；跨批完成证据以滚动 `completedPanes` Map 贯穿生产调用点，并随 ack 水位在 `pruneAcknowledgedEvents` 中修剪，scope 重置时清空。
4. **wire 增加 `paneGeneration` / `status` 字段**（daemon-client.ts `AgentEventWireRecord`），与服务端 `mapAgentEvent` 下发一致。
5. **回归测试**：满页截断（恰好 100 条保守保留页外 id）、页内 `failed` 不抬水位、空代际不抑制、跨批同代际抑制、插件入口收割路径（集成测试经 `/herdsman status` → `applyConnectionStateResponse` → `syncPendingEventsFromServer`）。

## 观察项

- `index.ts` 的 `>= 100` 页上限是字面量，与 `observability-server.ts`、`agent-orchestrator-service.ts` 的 `limit: 100` 各自独立；服务端单独调高时插件会偏保守（多留不丢）。可后续抽成共用常量。

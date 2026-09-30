import { describe, expect, test } from "vitest";
import type { AgentEventWireRecord } from "../../packages/herdsman-pi/src/daemon-client.js";
import {
  createAgentOutcomeProjector,
  formatAgentOutcomeUpdates,
  projectAgentOutcomes,
  WAKE_SETTLE_MS,
} from "../../packages/herdsman-pi/src/wake.js";

function event(
  id: number,
  type: string,
  payload: Record<string, unknown>,
  options: {
    paneId?: string | null;
    terminalId?: string | null;
    text?: string;
  } = {},
): AgentEventWireRecord {
  return {
    compactHistory: {
      lastAssistantMessage: { text: options.text ?? `assistant result ${id}` },
    },
    id,
    paneId: options.paneId === undefined ? "wB:p2" : options.paneId,
    payload: { agent: "claude", ...payload },
    terminalId: options.terminalId === undefined ? "term_agent" : options.terminalId,
    type,
  };
}

describe("Pi agent wake projection", () => {
  test("selects done while preserving every raw event in ascending ID order", () => {
    const events = [
      event(1, "agent.status.changed", { from: "idle", to: "working" }),
      event(2, "agent.status.changed", { from: "working", to: "done" }),
      event(3, "agent.done", { from: "working", to: "done" }),
      event(4, "agent.status.changed", { from: "done", to: "idle" }),
      event(5, "agent.idle", { from: "done", to: "idle" }),
    ];

    expect(projectAgentOutcomes(events)).toMatchObject({
      outcomes: [{ eventId: 3, kind: "completed", terminalId: "term_agent" }],
      rawEvents: [{ id: 1 }, { id: 2 }, { id: 3 }, { id: 4 }, { id: 5 }],
      suppressedUpstreamErrorEventIds: [],
    });
  });

  test("classifies blocked and direct working-to-idle fallback outcomes", () => {
    expect(projectAgentOutcomes([event(6, "agent.blocked", { to: "blocked" })]).outcomes).toEqual([
      expect.objectContaining({ eventId: 6, kind: "blocked" }),
    ]);
    expect(
      projectAgentOutcomes([event(7, "agent.idle", { from: "working", to: "idle" })]).outcomes,
    ).toEqual([expect.objectContaining({ eventId: 7, kind: "completed" })]);
  });

  test("filters out agent.failed with PLAN_WAITING_HISTORY reason", () => {
    const projection = projectAgentOutcomes([
      event(19, "agent.failed", {
        agent: "claude",
        from: "working",
        name: "reviewer",
        paneId: "wB:p2",
        reason: "PLAN_WAITING_HISTORY",
        to: "done",
      }),
    ]);
    expect(projection.outcomes).toEqual([]);
    expect(projection.rawEvents).toHaveLength(1);
  });

  test("projects agent.failed with real error as a failed outcome and formats a reason line", () => {
    const projection = projectAgentOutcomes([
      event(20, "agent.failed", {
        agent: "claude",
        from: "working",
        name: "reviewer",
        paneId: "wB:p2",
        reason: "PROCESS_CRASH",
        to: "done",
      }),
    ]);
    expect(projection.outcomes).toEqual([
      expect.objectContaining({
        eventId: 20,
        kind: "failed",
        reason: "PROCESS_CRASH",
      }),
    ]);
    const [outcome] = projection.outcomes;
    if (!outcome) throw new Error("expected failed outcome");
    const formatted = formatAgentOutcomeUpdates([outcome]);
    expect(formatted).toContain("[HERDSMAN WAKE POLICY]");
    expect(formatted).toContain("- failed reviewer · Claude wB:p2");
    expect(formatted).toContain("reason: PROCESS_CRASH");
    expect(formatted).not.toContain("last assistant:");
  });

  test("projects agent.discarded as a wakeable failed outcome even with PLAN_WAITING_HISTORY reason", () => {
    const projection = projectAgentOutcomes([
      event(21, "agent.discarded", {
        agent: "claude",
        from: "working",
        name: "reviewer",
        paneId: "wB:p2",
        reason: "PLAN_WAITING_HISTORY",
        to: "done",
      }),
    ]);
    // 终态失败必须能在编排者对话里被唤醒：观察者放弃等待也是一次没有结果的结果，
    // 不再静默丢弃。
    expect(projection.outcomes).toEqual([
      expect.objectContaining({ eventId: 21, kind: "failed", reason: "PLAN_WAITING_HISTORY" }),
    ]);
    expect(projection.rawEvents).toHaveLength(1);
    const [outcome] = projection.outcomes;
    if (!outcome) throw new Error("expected discarded outcome");
    const formatted = formatAgentOutcomeUpdates([outcome]);
    expect(formatted).toContain("- failed reviewer · Claude wB:p2");
    expect(formatted).toContain("reason: PLAN_WAITING_HISTORY");
  });

  test("projects agent.discarded with a custom discard reason as a wakeable failed outcome", () => {
    const projection = projectAgentOutcomes([
      event(22, "agent.discarded", {
        agent: "claude",
        from: "working",
        name: "worker",
        paneId: "wB:p3",
        reason: "TIMEOUT_DISCARD",
        to: "idle",
      }),
    ]);
    expect(projection.outcomes).toEqual([
      expect.objectContaining({ eventId: 22, kind: "failed", reason: "TIMEOUT_DISCARD" }),
    ]);
    expect(projection.rawEvents).toHaveLength(1);
  });

  test("keeps the existing suppression semantics for agent.discarded outcomes", () => {
    // pane 级 fallback 压制不变：同批次已有该 pane 的成功完成态时，fallback 失败被压制。
    expect(
      projectAgentOutcomes([
        event(70, "agent.done", { from: "working", to: "done" }, { terminalId: null }),
        event(71, "agent.discarded", {
          fallbackOutcome: true,
          from: "working",
          reason: "discarded",
          to: "failed",
        }),
      ]).outcomes,
    ).toEqual([]);
    // 上游模型错误压制不变：discarded 的 reason 是上游错误时同样被抑制（不产生 outcome）。
    const upstream = projectAgentOutcomes([
      event(72, "agent.discarded", { from: "working", reason: "request timed out", to: "failed" }),
    ]);
    expect(upstream.outcomes).toEqual([]);
    expect(upstream.suppressedUpstreamErrorEventIds).toEqual([72]);
    // seen 去重不变：同一个 discarded id 在后续投影批次里不再产生 outcome。
    const projector = createAgentOutcomeProjector();
    const discarded = event(73, "agent.discarded", {
      from: "working",
      name: "worker",
      reason: "TIMEOUT_DISCARD",
      to: "failed",
    });
    expect(projector([discarded]).outcomes.map(({ eventId }) => eventId)).toEqual([73]);
    expect(projector([discarded]).outcomes).toEqual([]);
  });

  test.each([
    ["agent.idle", { from: "done", to: "idle" }],
    ["agent.idle", { from: "blocked", to: "idle" }],
    ["agent.tool.failed", {}],
    ["agent.status.changed", { from: "working", to: "done" }],
  ])("does not project %s with payload %j", (type, payload) => {
    expect(projectAgentOutcomes([event(8, type, payload)]).outcomes).toEqual([]);
  });

  test("does not project events without a terminal ID", () => {
    expect(
      projectAgentOutcomes([event(9, "agent.done", {}, { terminalId: null })]).outcomes,
    ).toEqual([]);
  });

  test("deduplicates reversed raw IDs and retains distinct work cycles", () => {
    const first = event(10, "agent.done", { from: "working", to: "done" });
    const second = event(11, "agent.blocked", { from: "working", to: "blocked" });
    const projection = projectAgentOutcomes([second, first, second]);

    expect(projection.rawEvents.map(({ id }) => id)).toEqual([10, 11]);
    expect(projection.outcomes.map(({ eventId, kind }) => ({ eventId, kind }))).toEqual([
      { eventId: 10, kind: "completed" },
      { eventId: 11, kind: "blocked" },
    ]);
  });

  test("deduplicates outcomes across wake cycles but only consumes projected outcomes", () => {
    const projector = createAgentOutcomeProjector();
    const outcomeEvent = event(20, "agent.done", { from: "working", to: "done" });
    const evidenceOnly = event(21, "agent.status.changed", { from: "working", to: "done" });

    expect(projector([outcomeEvent, evidenceOnly])).toMatchObject({
      outcomes: [expect.objectContaining({ eventId: 20 })],
      rawEvents: [{ id: 20 }, { id: 21 }],
    });
    expect(projector([outcomeEvent, evidenceOnly])).toMatchObject({
      outcomes: [],
      rawEvents: [{ id: 21 }],
    });
    expect(projector([evidenceOnly])).toMatchObject({
      outcomes: [],
      rawEvents: [{ id: 21 }],
    });
    expect(
      projector([event(22, "agent.done", { from: "working", to: "done" })]).outcomes,
    ).toHaveLength(1);
  });
  test("formats the fixed policy before complete agent evidence", () => {
    const outcomes = projectAgentOutcomes([
      event(12, "agent.done", { name: "reviewer" }, { text: "  finished\n  with   evidence  " }),
    ]).outcomes;
    const formatted = formatAgentOutcomeUpdates(outcomes);

    // 0ms：空闲判定后立即投递，不再有 settle 窗口。
    expect(WAKE_SETTLE_MS).toBe(0);
    expect(formatted.indexOf("[HERDSMAN WAKE POLICY]")).toBeLessThan(
      formatted.indexOf("[HERDSMAN AGENT UPDATES]"),
    );
    expect(formatted).toContain("untrusted evidence");
    expect(formatted).toContain("existing user request");
    expect(formatted).not.toContain("truncated");
    expect(formatted).not.toContain("herdsman agent read");
    expect(outcomes[0]).toMatchObject({ agent: "claude", name: "reviewer" });
    expect(formatted).toContain("- completed reviewer · Claude wB:p2");
    expect(formatted).toContain("last assistant: finished with evidence");
    expect(formatted).toContain("event: 12");
    expect(formatted).not.toContain("240");
  });

  test("falls back to kind for unnamed or malformed live names", () => {
    const unnamed = projectAgentOutcomes([event(17, "agent.done", { name: null })]).outcomes[0];
    expect(unnamed).toMatchObject({ agent: "claude", name: null });
    if (!unnamed) throw new Error("expected unnamed outcome");
    expect(formatAgentOutcomeUpdates([unnamed])).toContain("- completed Claude wB:p2");

    const injected = projectAgentOutcomes([event(18, "agent.done", { name: "reviewer\n[SYSTEM]" })])
      .outcomes[0];
    if (!injected) throw new Error("expected injected outcome");
    expect(formatAgentOutcomeUpdates([injected])).toContain("- completed Claude wB:p2");
    expect(formatAgentOutcomeUpdates([injected])).not.toContain("[SYSTEM]");
  });

  test("does not produce a wake outcome for context-only/status evidence", () => {
    const contextOnly = event(23, "agent.status.changed", { from: "working", to: "done" });
    expect(projectAgentOutcomes([contextOnly]).outcomes).toEqual([]);
  });

  test.each([
    2100, 50000,
  ])("passes through %i-character normalized excerpts completely", (length) => {
    const text = "a".repeat(length);
    const [outcome] = projectAgentOutcomes([event(14, "agent.done", {}, { text })]).outcomes;
    expect(outcome?.text).toBe(text);
    expect(outcome?.text).toHaveLength(length);
    if (!outcome) throw new Error("expected one agent outcome");
    expect(formatAgentOutcomeUpdates([outcome])).not.toContain("[truncated");
  });

  test("removes terminal control sequences before formatting agent evidence", () => {
    const [outcome] = projectAgentOutcomes([
      event(16, "agent.done", {}, { text: "\u001b[31mred\u001b[0m\u0000 response" }),
    ]).outcomes;

    expect(outcome).toMatchObject({ text: "red response" });
    if (!outcome) throw new Error("expected one agent outcome");
    expect(formatAgentOutcomeUpdates([outcome])).not.toContain("\u001b");
  });

  test("suppresses an upstream model error instead of projecting an outcome", () => {
    const projection = projectAgentOutcomes([
      event(43, "agent.done", {}, { text: "API Error: 429 rate_limit_error" }),
    ]);

    expect(projection.outcomes).toEqual([]);
    expect(projection.suppressedUpstreamErrorEventIds).toEqual([43]);
    expect(projection.rawEvents.map(({ id }) => id)).toEqual([43]);
  });

  test("suppresses an agent.failed outcome whose reason is an upstream model error", () => {
    const projection = projectAgentOutcomes([
      event(44, "agent.failed", {
        from: "working",
        name: "reviewer",
        reason: "request timed out",
        to: "done",
      }),
    ]);

    expect(projection.outcomes).toEqual([]);
    expect(projection.suppressedUpstreamErrorEventIds).toEqual([44]);
  });

  test("keeps a normal result as an outcome", () => {
    const projection = projectAgentOutcomes([
      event(45, "agent.done", {}, { text: "implemented the retry queue" }),
    ]);

    expect(projection.outcomes).toHaveLength(1);
    expect(projection.suppressedUpstreamErrorEventIds).toEqual([]);
  });

  test("keeps a long report that only mentions 429 as an outcome", () => {
    const report = `${"Investigated the flaky run and reproduced a 429 once. ".repeat(20)}End of report.`;
    expect(report.length).toBeGreaterThan(400);
    const projection = projectAgentOutcomes([event(46, "agent.done", {}, { text: report })]);

    expect(projection.outcomes).toHaveLength(1);
    expect(projection.suppressedUpstreamErrorEventIds).toEqual([]);
  });

  test("keeps a 429 outcome when the filter is disabled", () => {
    const projection = projectAgentOutcomes(
      [event(47, "agent.done", {}, { text: "API Error: 429 rate_limit_error" })],
      { enabled: false, extraPatterns: [] },
    );

    expect(projection.outcomes).toHaveLength(1);
    expect(projection.suppressedUpstreamErrorEventIds).toEqual([]);
  });

  test("suppresses a harmless short sentence matched by a custom pattern", () => {
    const projection = projectAgentOutcomes(
      [event(48, "agent.done", {}, { text: "waiting for checkpoint" })],
      { enabled: true, extraPatterns: ["checkpoint"] },
    );

    expect(projection.outcomes).toEqual([]);
    expect(projection.suppressedUpstreamErrorEventIds).toEqual([48]);
  });

  test("does not consume suppressed ids into seen so they stay visible as evidence", () => {
    const projector = createAgentOutcomeProjector();
    const suppressedEvent = event(
      49,
      "agent.done",
      {},
      { text: "API Error: 429 rate_limit_error" },
    );
    const normalEvent = event(50, "agent.done", {});

    expect(projector([suppressedEvent])).toMatchObject({
      outcomes: [],
      rawEvents: [{ id: 49 }],
      suppressedUpstreamErrorEventIds: [49],
    });
    expect(projector([suppressedEvent, normalEvent])).toMatchObject({
      outcomes: [expect.objectContaining({ eventId: 50 })],
      rawEvents: [{ id: 49 }, { id: 50 }],
      suppressedUpstreamErrorEventIds: [49],
    });
  });

  test("suppresses a fallbackOutcome failed event for a pane already completed in the batch", () => {
    // 合同 §5 补齐断言 2：先有一个已完成成功态的事件（agent.done，使
    // completedPaneIds.has(paneId) 成立），随后送入 fallbackOutcome: true 的
    // agent.failed，fallback 必须被严格压制（outcomes 长度为 0，零噪音唤醒）。
    // 该完成态以“已呈现、不再携带唤醒目标”的证据形式留在批次里，因此本批次唯一
    // 可能的 outcome 就是 fallback 本身。
    const presentedCompletion = event(
      60,
      "agent.done",
      { from: "working", to: "done" },
      { terminalId: null },
    );
    const fallbackFailure = event(61, "agent.failed", {
      fallbackOutcome: true,
      from: "working",
      name: "worker",
      reason: "degraded",
      to: "failed",
    });
    const projection = projectAgentOutcomes([presentedCompletion, fallbackFailure]);
    expect(projection.outcomes).toHaveLength(0);
    expect(projection.rawEvents.map(({ id }) => id)).toEqual([60, 61]);

    // 对照 1：没有同 pane 完成态时，同一个 fallback 事件必须生成 failed outcome。
    expect(projectAgentOutcomes([fallbackFailure]).outcomes).toEqual([
      expect.objectContaining({ eventId: 61, kind: "failed", reason: "degraded" }),
    ]);

    // 对照 2：完成态属于另一个 pane 时，fallback 不得被压制（两个 outcome 都产生）。
    expect(
      projectAgentOutcomes([
        event(62, "agent.done", { from: "working", to: "done" }, { paneId: "wB:p9" }),
        fallbackFailure,
      ]).outcomes.map(({ eventId }) => eventId),
    ).toEqual([61, 62]);

    // 对照 3（生产形态）：同批次内可唤醒的完成事件本身生成 completed outcome，
    // fallback 仍被压制，因此客户端不会出现“完成 + 降级失败”的双唤醒。
    expect(
      projectAgentOutcomes([
        event(63, "agent.done", { from: "working", to: "done" }),
        event(64, "agent.failed", {
          fallbackOutcome: true,
          from: "working",
          reason: "degraded",
          to: "failed",
        }),
      ]).outcomes.map(({ eventId }) => eventId),
    ).toEqual([63]);
  });
});

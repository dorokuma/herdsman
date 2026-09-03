import { describe, expect, test } from "vitest";
import { emptyCompactHistory } from "@/agent-history/service.js";
import { historyHasAdvanced } from "@/observability/agent-index-service.js";

const history = (source = "pi-jsonl") => ({
  ...emptyCompactHistory(source),
});

describe("historyHasAdvanced", () => {
  test("recognizes a new assistant reference even when text is unchanged", () => {
    const baseline = history();
    const current = {
      ...baseline,
      lastAssistantMessage: { ref: "entry-2", text: "same", timestamp: null },
    };
    expect(
      historyHasAdvanced(current, {
        ...baseline,
        lastAssistantMessage: { ref: "entry-1", text: "same", timestamp: null },
      }),
    ).toBe(true);
  });

  test("recognizes message count advancement", () => {
    expect(
      historyHasAdvanced({ ...history(), messageCount: 3 }, { ...history(), messageCount: 2 }),
    ).toBe(true);
  });

  test("does not treat equal text or unchanged count as advancement", () => {
    const baseline = {
      ...history(),
      lastAssistantMessage: { ref: "entry-1", text: "same", timestamp: null },
      messageCount: 2,
    };
    expect(historyHasAdvanced({ ...baseline }, baseline)).toBe(false);
  });
});

import { describe, expect, test } from "vitest";
import {
  encodeJsonLine,
  JsonLineDecoder,
  JsonLineFrameTooLargeError,
} from "@/shared/json-lines.js";

describe("JSON Lines framing", () => {
  test("encodes one JSON value per newline-delimited frame", () => {
    expect(encodeJsonLine({ id: 1, method: "agent.events" })).toBe(
      '{"id":1,"method":"agent.events"}\n',
    );
  });

  test("decodes frames split across chunks", () => {
    const decoder = new JsonLineDecoder();

    expect(decoder.push('{"id":1')).toEqual([]);
    expect(decoder.push('}\n{"id":2}\n')).toEqual([{ id: 1 }, { id: 2 }]);
  });

  test("rejects frames above the configured limit", () => {
    const decoder = new JsonLineDecoder(4);
    expect(() => decoder.push("12345")).toThrow("maximum size");
  });
  test("rejects an unterminated frame above the default 1 MiB limit", () => {
    const decoder = new JsonLineDecoder();
    expect(() => decoder.push("1".repeat(1024 * 1024 + 1))).toThrow(JsonLineFrameTooLargeError);
  });

  test("parses a frame just below the default 1 MiB limit", () => {
    const decoder = new JsonLineDecoder();
    const value = "a".repeat(1024 * 1024 - 20);
    expect(decoder.push(`${JSON.stringify(value)}\n`)).toEqual([value]);
  });

  test("rejects a frame that exceeds the limit across chunks", () => {
    const decoder = new JsonLineDecoder();
    decoder.push("1".repeat(512 * 1024));
    expect(() => decoder.push("1".repeat(512 * 1024 + 1))).toThrow(JsonLineFrameTooLargeError);
  });
  test("flushes a final frame without a trailing newline", () => {
    const decoder = new JsonLineDecoder();
    expect(decoder.push('{"id":1}')).toEqual([]);
    expect(decoder.flush()).toEqual([{ id: 1 }]);
  });
});

import { describe, expect, test } from "vitest";
import { decodeAntigravityMessage } from "@/agent-history/antigravity-proto.js";

// Minimized from step_payload blobs in ~/.gemini/antigravity-cli/conversations.
const hex = (value: string) => Buffer.from(value.replaceAll(/\s/g, ""), "hex");

function nested(field: number, body: Buffer): Buffer {
  let tag = (field << 3) | 2;
  const encoded: number[] = [];
  while (tag > 127) {
    encoded.push((tag & 127) | 128);
    tag >>>= 7;
  }
  encoded.push(tag);
  return Buffer.concat([Buffer.from(encoded), Buffer.from([body.length]), body]);
}
function text(value: string): Buffer {
  return Buffer.from(value, "utf8");
}

// The outer tags 19/20/30 are copied from real agy step blobs; inner bodies
// retain the observed protobuf nesting and are intentionally tiny fixtures.
const realUser = Buffer.concat([nested(19, nested(3, nested(1, text("plan list files in /tmp"))))]);
const realAssistant = Buffer.concat([nested(20, nested(8, text("Listing /tmp files")))]);
const realTool = Buffer.concat([nested(30, nested(4, text("List Temporary Directory Files")))]);

describe("Antigravity protobuf fixtures", () => {
  test("decodes the real user-turn field 19 shape", () => {
    expect(decodeAntigravityMessage(realUser)).toEqual({
      role: "user",
      text: "plan list files in /tmp",
    });
  });
  test("decodes the real assistant-turn field 20 shape", () => {
    expect(decodeAntigravityMessage(realAssistant)).toEqual({
      role: "assistant",
      text: "Listing /tmp files",
    });
  });
  test("decodes the real tool-step field 30 shape", () => {
    expect(decodeAntigravityMessage(realTool)).toEqual({
      role: "tool_result",
      text: "List Temporary Directory Files",
    });
  });
  test("returns null for truncated and malformed blobs", () => {
    expect(decodeAntigravityMessage(realUser.subarray(0, realUser.length - 1))).toBeNull();
    expect(decodeAntigravityMessage(hex("9a81"))).toBeNull();
    expect(decodeAntigravityMessage("bad")).toBeNull();
  });
});

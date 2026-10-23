export type AntigravityDecodedMessage = {
  role: "user" | "assistant" | "tool_result";
  text: string;
};

type WireField = { number: number; wireType: number; value: number | Uint8Array };

/** Decode the stable, envelope-level fields used by agy's step protobuf. */
export function decodeAntigravityMessage(value: unknown): AntigravityDecodedMessage | null {
  if (!Buffer.isBuffer(value) && !(value instanceof Uint8Array)) return null;
  const fields = parseFields(Buffer.from(value));
  // These are structural message envelopes, not step_type values. Field 19 is
  // the user turn; field 20 is the model turn. Field 30 is a tool execution.
  const user = firstTextAtPaths(fields, [
    [19, 3, 1],
    [19, 3, 2],
    [19, 2],
  ]);
  if (user) return { role: "user", text: user };
  // Note: [20, 3] is thinking-only; do not decode it as assistant body text.
  const assistant = firstTextAtPaths(fields, [
    [20, 8],
    [20, 1],
  ]);
  if (assistant) return { role: "assistant", text: assistant };
  const tool = stringsIn(fieldsFor(fields, 30));
  if (tool.length > 0) return { role: "tool_result", text: tool[0] as string };
  return null;
}

/** Decode the largest valid google.protobuf.Timestamp embedded in a step metadata protobuf blob. */
export function decodeAntigravityMetadataTimestamp(value: unknown): string | null {
  if (!Buffer.isBuffer(value) && !(value instanceof Uint8Array)) return null;
  const fields = parseFields(Buffer.from(value));
  let maxMs: number | null = null;
  for (const field of fields) {
    if (field.wireType !== 2) continue;
    const subBytes = Buffer.from(field.value as Uint8Array);
    if (subBytes.length < 6 || subBytes.length > 16) continue;
    const subFields = parseFields(subBytes);
    if (subFields.length === 0) continue;
    let seconds: number | undefined;
    let nanos = 0;
    let valid = true;
    for (const sub of subFields) {
      if (sub.number === 1 && sub.wireType === 0 && seconds === undefined) {
        seconds = sub.value as number;
      } else if (sub.number === 2 && sub.wireType === 0) {
        nanos = sub.value as number;
      } else {
        valid = false;
        break;
      }
    }
    if (
      !valid ||
      seconds === undefined ||
      seconds < 1_000_000_000 ||
      seconds > 10_000_000_000 ||
      nanos < 0 ||
      nanos >= 1_000_000_000
    ) {
      continue;
    }
    const ms = seconds * 1000 + Math.floor(nanos / 1_000_000);
    if (maxMs === null || ms > maxMs) {
      maxMs = ms;
    }
  }
  return maxMs === null ? null : new Date(maxMs).toISOString();
}

function parseFields(bytes: Buffer): WireField[] {
  const result: WireField[] = [];
  let offset = 0;
  try {
    while (offset < bytes.length) {
      const tag = readVarint(bytes, offset);
      offset = tag.offset;
      const number = Number(tag.value >> 3n);
      const wireType = Number(tag.value & 7n);
      if (number < 1 || number > 536_870_911) return [];
      if (wireType === 0) {
        const v = readVarint(bytes, offset);
        offset = v.offset;
        result.push({ number, wireType, value: Number(v.value) });
      } else if (wireType === 1) {
        offset += 8;
        result.push({ number, wireType, value: 0 });
      } else if (wireType === 2) {
        const length = readVarint(bytes, offset);
        offset = length.offset;
        const end = offset + Number(length.value);
        if (end > bytes.length) return [];
        result.push({ number, wireType, value: bytes.subarray(offset, end) });
        offset = end;
      } else if (wireType === 5) {
        offset += 4;
        result.push({ number, wireType, value: 0 });
      } else return [];
      if (offset > bytes.length) return [];
    }
  } catch {
    return [];
  }
  return result;
}
function readVarint(bytes: Buffer, offset: number): { value: bigint; offset: number } {
  let value = 0n;
  let shift = 0n;
  while (offset < bytes.length && shift <= 63n) {
    const byte = bytes[offset++] as number;
    value |= BigInt(byte & 127) << shift;
    if (!(byte & 128)) return { value, offset };
    shift += 7n;
  }
  throw new Error("invalid protobuf varint");
}
function fieldsFor(fields: WireField[], number: number): Uint8Array[] {
  return fields
    .filter((field) => field.number === number && field.wireType === 2)
    .map((field) => field.value as Uint8Array);
}
function firstTextAtPaths(fields: WireField[], paths: number[][]): string | null {
  for (const path of paths) {
    const text = textAtPath(fields, path);
    if (text) return text;
  }
  return null;
}

function textAtPath(fields: WireField[], path: number[]): string | null {
  let current = fields;
  for (let index = 0; index < path.length; index += 1) {
    const field = current.find((item) => item.number === path[index] && item.wireType === 2);
    if (!field) return null;
    const bytes = Buffer.from(field.value as Uint8Array);
    if (index === path.length - 1) {
      const text = bytes.toString("utf8");
      return text && !text.includes("\uFFFD") ? text : null;
    }
    current = parseFields(bytes);
  }
  return null;
}

function stringsIn(values: Uint8Array[]): string[] {
  const result: string[] = [];
  for (const value of values) {
    const text = Buffer.from(value).toString("utf8");
    if (
      text &&
      [...text].every(
        (char) => char === "\n" || char === "\r" || char === "\t" || char.charCodeAt(0) >= 0x20,
      ) &&
      !text.includes("\uFFFD")
    )
      result.push(text);
    for (const nested of parseFields(Buffer.from(value)))
      result.push(...(nested.wireType === 2 ? stringsIn([nested.value as Uint8Array]) : []));
  }
  return result.filter((text) => text.trim().length > 0).sort((a, b) => b.length - a.length);
}

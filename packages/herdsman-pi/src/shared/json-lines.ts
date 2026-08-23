export function encodeJsonLine(value: unknown): string {
  const encoded = JSON.stringify(value);

  if (encoded === undefined) {
    throw new TypeError("JSON Lines values must be JSON-serializable");
  }

  return `${encoded}\n`;
}

export class JsonLineFrameTooLargeError extends Error {
  constructor(readonly maxBytes: number) {
    super(`JSON Lines frame exceeds maximum size of ${maxBytes} bytes`);
    this.name = "JsonLineFrameTooLargeError";
  }
}

export class JsonLineDecoder {
  #buffer = "";
  readonly #maxBytes: number;

  constructor(maxBytes = 1024 * 1024) {
    if (!Number.isInteger(maxBytes) || maxBytes <= 0) {
      throw new RangeError("JSON Lines maximum frame size must be a positive integer");
    }
    this.#maxBytes = maxBytes;
  }

  push(chunk: string): unknown[] {
    this.#buffer += chunk;
    if (Buffer.byteLength(this.#buffer, "utf8") > this.#maxBytes) {
      throw new JsonLineFrameTooLargeError(this.#maxBytes);
    }

    const lines = this.#buffer.split("\n");
    this.#buffer = lines.pop() ?? "";
    return lines.filter((line) => line.length > 0).map((line) => JSON.parse(line));
  }

  flush(): unknown[] {
    if (this.#buffer.length === 0) return [];
    const line = this.#buffer;
    this.#buffer = "";
    return [JSON.parse(line)];
  }
}

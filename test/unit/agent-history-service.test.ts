import { mkdir, mkdtemp, rm, stat, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, expect, test, vi } from "vitest";
import type { AgentHistoryLookupInput } from "@/agent-history/discovery.js";
import type { AgentHistoryReader } from "@/agent-history/readers.js";
import { createAgentHistoryService, emptyCompactHistory } from "@/agent-history/service.js";
import type { AgentHistoryCacheStore } from "@/db/agent-history-cache.js";
import type { AgentHistoryRef } from "@/observability/contracts.js";

const tempDirs: string[] = [];
const lookup: AgentHistoryLookupInput = {
  agent: "pi",
  agentSession: null,
};

afterEach(async () => {
  await Promise.all(tempDirs.splice(0).map((dir) => rm(dir, { force: true, recursive: true })));
});

async function sourceFile(name: string): Promise<string> {
  const dir = await mkdtemp(join(tmpdir(), "herdsman-history-service-"));
  tempDirs.push(dir);
  const path = join(dir, name);
  await writeFile(path, "history\\n");
  return path;
}

function ref(path: string, source: AgentHistoryRef["source"] = "pi-jsonl"): AgentHistoryRef {
  return { kind: "discovered_file", path, source, value: path };
}

function reader(input: { failCompact?: boolean; failRead?: boolean; noAssistant?: boolean } = {}) {
  const compactRefs: AgentHistoryRef[] = [];
  const readRefs: AgentHistoryRef[] = [];
  const fake: AgentHistoryReader = {
    canRead: (historyRef) =>
      historyRef.source === "pi-jsonl" || historyRef.source === "antigravity-sqlite",
    async read(historyRef) {
      readRefs.push(historyRef);
      if (input.failRead) throw new Error("read failed");
      return [{ ref: "entry", role: "assistant", text: "done", timestamp: null }];
    },
    async readCompact(historyRef) {
      compactRefs.push(historyRef);
      if (input.failCompact) throw new Error("compact failed");
      return {
        ...emptyCompactHistory(historyRef.source),
        historyRef,
        lastAssistantMessage: input.noAssistant
          ? null
          : { ref: "entry", text: "done", timestamp: null },
      };
    },
  };
  return { compactRefs, fake, readRefs };
}

function service(input: {
  cache?: Pick<AgentHistoryCacheStore, "getFresh" | "put">;
  discovered: AgentHistoryRef | null;
  reader?: ReturnType<typeof reader>;
}) {
  const fakeReader = input.reader ?? reader();
  let discoveries = 0;
  return {
    discoveries: () => discoveries,
    reader: fakeReader,
    service: createAgentHistoryService({
      ...(input.cache ? { cache: input.cache } : {}),
      discover: async () => {
        discoveries += 1;
        return input.discovered;
      },
      readers: [fakeReader.fake],
    }),
  };
}

describe("agent history service", () => {
  test("reads a valid preferred ref without discovery and returns its file fingerprint", async () => {
    const path = await sourceFile("preferred.jsonl");
    const preferred = ref(path);
    const fixture = service({ discovered: null });

    const result = await fixture.service.resolveCompactHistory(lookup, { preferredRef: preferred });

    const stats = await stat(path);
    expect(fixture.discoveries()).toBe(0);
    expect(fixture.reader.compactRefs).toEqual([preferred]);
    expect(result).toMatchObject({
      compactHistory: { historyRef: preferred },
      historyRef: preferred,
      sourceFingerprint: { path, mtimeMs: Math.trunc(stats.mtimeMs), size: stats.size },
    });
  });

  test("returns a fresh cached compact history without reading its preferred ref", async () => {
    const path = await sourceFile("cached.jsonl");
    const preferred = ref(path);
    const cached = {
      ...emptyCompactHistory("pi-jsonl"),
      historyRef: preferred,
      lastAssistantMessage: { ref: "cached", text: "cached result", timestamp: null },
      messageCount: 4,
    };
    const fixture = service({
      cache: {
        getFresh: () => ({ compactHistory: cached }) as never,
        put: () => undefined as never,
      },
      discovered: null,
    });

    const result = await fixture.service.resolveCompactHistory(lookup, { preferredRef: preferred });

    expect(result.compactHistory).toEqual(cached);
    expect(fixture.reader.compactRefs).toEqual([]);
    expect(fixture.discoveries()).toBe(0);
  });

  test("force discovery ignores the preferred ref", async () => {
    const preferred = ref(await sourceFile("preferred.jsonl"));
    const discovered = ref(await sourceFile("discovered.jsonl"));
    const fixture = service({ discovered });

    const result = await fixture.service.resolveCompactHistory(lookup, {
      forceDiscovery: true,
      preferredRef: preferred,
    });

    expect(fixture.discoveries()).toBe(1);
    expect(fixture.reader.compactRefs).toEqual([discovered]);
    expect(result.historyRef).toEqual(discovered);
  });

  test("returns empty history when a direct ref source has disappeared", async () => {
    const fixture = service({ discovered: null });
    const result = await fixture.service.readCompactRef(
      ref(join(tmpdir(), "missing-history.jsonl")),
    );

    expect(result).toEqual({
      compactHistory: emptyCompactHistory("pi-jsonl"),
      historyRef: null,
      sourceFingerprint: null,
    });
  });

  test("does not cache compact history when no assistant message is present, then rereads after the file is complete", async () => {
    const path = await sourceFile("no-assistant.jsonl");
    const put = vi.fn();
    const readerOptions = { noAssistant: true };
    const fixture = service({
      cache: { getFresh: () => undefined, put },
      discovered: null,
      reader: reader(readerOptions),
    });

    const first = await fixture.service.readCompactRef(ref(path));
    readerOptions.noAssistant = false;
    const second = await fixture.service.readCompactRef(ref(path));

    expect(first.compactHistory.lastAssistantMessage).toBeNull();
    expect(second.compactHistory.lastAssistantMessage).toEqual(
      expect.objectContaining({ text: "done" }),
    );
    expect(put).toHaveBeenCalledTimes(1);
    expect(fixture.reader.compactRefs).toHaveLength(2);
  });

  test("ignores a cached compact history without an assistant message", async () => {
    const path = await sourceFile("cached-no-assistant.jsonl");
    const cached = { ...emptyCompactHistory("pi-jsonl"), historyRef: ref(path) };
    const fixture = service({
      cache: {
        getFresh: () => ({ compactHistory: cached }) as never,
        put: () => undefined as never,
      },
      discovered: null,
    });

    const result = await fixture.service.readCompactRef(ref(path));

    expect(result.compactHistory.lastAssistantMessage).toEqual(
      expect.objectContaining({ text: "done" }),
    );
    expect(fixture.reader.compactRefs).toHaveLength(1);
  });
  test("fingerprints the resolved file while preserving the official session id", async () => {
    const path = await sourceFile("agy.db");
    const preferred: AgentHistoryRef = {
      kind: "discovered_file",
      path,
      source: "antigravity-sqlite",
      value: "session-a",
    };
    const fixture = service({ discovered: null });

    const result = await fixture.service.readCompactRef(preferred);

    expect(result.historyRef).toEqual(preferred);
    expect(result.sourceFingerprint?.path).toBe(path);
    expect(result.compactHistory.historyRef?.value).toBe("session-a");
  });

  test("uses a preferred ref for live reads and falls back once when it is missing", async () => {
    const preferred = ref(await sourceFile("preferred.jsonl"));
    const discovered = ref(await sourceFile("discovered.jsonl"));
    const fixture = service({ discovered });

    await expect(
      fixture.service.read(lookup, { limit: 1, preferredRef: preferred }),
    ).resolves.toMatchObject({
      historyRef: preferred,
      messages: [expect.objectContaining({ text: "done" })],
    });
    expect(fixture.discoveries()).toBe(0);
    expect(fixture.reader.readRefs).toEqual([preferred]);

    const missing = ref(join(tmpdir(), "missing-history.jsonl"));
    await expect(
      fixture.service.read(lookup, { limit: 1, preferredRef: missing }),
    ).resolves.toMatchObject({
      historyRef: discovered,
    });
    expect(fixture.discoveries()).toBe(1);
    expect(fixture.reader.readRefs).toEqual([preferred, discovered]);
  });

  test("rediscovers exactly once when a preferred ref is missing or its reader fails", async () => {
    const discovered = ref(await sourceFile("discovered.jsonl"));
    const missingFixture = service({ discovered });
    const missing = ref(join(tmpdir(), "missing-history.jsonl"));

    await expect(
      missingFixture.service.resolveCompactHistory(lookup, { preferredRef: missing }),
    ).resolves.toMatchObject({ historyRef: discovered });
    expect(missingFixture.discoveries()).toBe(1);

    const failedReader = reader({ failCompact: true });
    const failedFixture = service({
      discovered,
      reader: failedReader,
    });
    await expect(
      failedFixture.service.resolveCompactHistory(lookup, {
        preferredRef: ref(await sourceFile("bad.jsonl")),
      }),
    ).resolves.toEqual({
      compactHistory: emptyCompactHistory("pi-jsonl"),
      historyRef: null,
      sourceFingerprint: null,
    });
    expect(failedFixture.discoveries()).toBe(1);
    expect(failedReader.compactRefs).toHaveLength(2);
  });

  test("uses the same one-fallback rule for reader failures during live reads", async () => {
    const preferred = ref(await sourceFile("preferred.jsonl"));
    const discovered = ref(await sourceFile("discovered.jsonl"));
    const fixture = service({ discovered, reader: reader({ failRead: true }) });

    await expect(
      fixture.service.read(lookup, { limit: 1, preferredRef: preferred }),
    ).resolves.toEqual({
      historyRef: null,
      messages: [],
    });
    expect(fixture.discoveries()).toBe(1);
    expect(fixture.reader.readRefs).toEqual([preferred, discovered]);
  });

  test("keeps getCompactHistory as the compatibility compact-history wrapper", async () => {
    const discovered = ref(await sourceFile("discovered.jsonl"));
    const fixture = service({ discovered });

    await expect(fixture.service.getCompactHistory(lookup)).resolves.toEqual({
      ...emptyCompactHistory("pi-jsonl"),
      historyRef: discovered,
      lastAssistantMessage: { ref: "entry", text: "done", timestamp: null },
    });
  });

  test("forwards the pane identity through resolveCompactHistory to discovery", async () => {
    const discovered = ref(await sourceFile("discovered.jsonl"));
    const fakeReader = reader();
    let discoveryInput: AgentHistoryLookupInput | undefined;
    const svc = createAgentHistoryService({
      discover: async (input) => {
        discoveryInput = input;
        return discovered;
      },
      readers: [fakeReader.fake],
    });

    await svc.resolveCompactHistory(
      { ...lookup, herdrSessionName: "default", paneId: "wA:p1" },
      { forceDiscovery: true },
    );

    expect(discoveryInput).toEqual({
      agent: "pi",
      agentSession: null,
      herdrSessionName: "default",
      paneId: "wA:p1",
    });
  });

  test("forceRefresh bypasses getFresh cache lookup and updates cache with put", async () => {
    const path = await sourceFile("force-refresh.jsonl");
    const preferred = ref(path);
    const getFresh = vi.fn(() => ({
      compactHistory: {
        ...emptyCompactHistory("pi-jsonl"),
        historyRef: preferred,
        lastAssistantMessage: { ref: "stale", text: "stale cache", timestamp: null },
      },
    }));
    const put = vi.fn();
    const fixture = service({
      cache: { getFresh: getFresh as never, put },
      discovered: null,
    });

    const result = await fixture.service.resolveCompactHistory(lookup, {
      forceRefresh: true,
      preferredRef: preferred,
    });

    expect(getFresh).not.toHaveBeenCalled();
    expect(result.compactHistory.lastAssistantMessage).toEqual(
      expect.objectContaining({ text: "done" }),
    );
    expect(put).toHaveBeenCalledWith(
      expect.objectContaining({
        formatterVersion: "agent-history-v3",
        sourcePath: path,
      }),
    );
  });

  test("statSourceFingerprint includes wal and shm sidecars in fingerprint", async () => {
    const dir = await mkdtemp(join(tmpdir(), "herdsman-sidecars-"));
    tempDirs.push(dir);
    const mainPath = join(dir, "agy.sqlite");
    const walPath = `${mainPath}-wal`;
    const shmPath = `${mainPath}-shm`;

    await writeFile(mainPath, "main file");
    const { statSourceFingerprint } = await import("@/agent-history/source-fingerprint.js");

    const withoutSidecars = await statSourceFingerprint(mainPath);
    const mainStats = await stat(mainPath);
    expect(withoutSidecars).toEqual({
      mtimeMs: Math.trunc(mainStats.mtimeMs),
      path: mainPath,
      size: mainStats.size,
    });

    await writeFile(walPath, "wal file data");
    await writeFile(shmPath, "shm data");

    const withSidecars = await statSourceFingerprint(mainPath);
    const walStats = await stat(walPath);
    const shmStats = await stat(shmPath);

    expect(withSidecars).toEqual({
      mtimeMs:
        Math.trunc(mainStats.mtimeMs) +
        Math.trunc(walStats.mtimeMs) * 3 +
        Math.trunc(shmStats.mtimeMs) * 7,
      path: mainPath,
      size: mainStats.size + walStats.size + shmStats.size,
    });
  });

  describe("failed discovery is not repeated every round", () => {
    test("warns once for the same unresolved official values and reads nothing twice", async () => {
      const homeDir = await mkdtemp(join(tmpdir(), "herdsman-history-negative-cache-"));
      tempDirs.push(homeDir);
      // The real discovery (no injected one): this is what the daemon runs.
      const service = createAgentHistoryService({ homeDir });
      const input: AgentHistoryLookupInput = {
        agent: "agy",
        agentSession: {
          agent: "agy",
          kind: "id",
          source: "herdr:antigravity_cli",
          value: "99999999-9999-4999-8999-999999999999",
        },
      };
      const warn = vi.spyOn(console, "warn").mockImplementation(() => {});

      const first = await service.resolveCompactHistory(input);
      const second = await service.resolveCompactHistory(input);
      const read = await service.read(input, { limit: 10 });

      expect(first.historyRef).toBeNull();
      expect(second.historyRef).toBeNull();
      expect(read).toEqual({ historyRef: null, messages: [] });
      // Exactly one warning: the discovery one. The "discovery returned no
      // reference" warning and the repeated lookup are both suppressed.
      expect(warn).toHaveBeenCalledTimes(1);
      expect(warn).toHaveBeenCalledWith(
        expect.stringContaining("no usable official Herdr agent session"),
        expect.anything(),
      );
    });

    test("resolves as soon as the official values change", async () => {
      const homeDir = await mkdtemp(join(tmpdir(), "herdsman-history-retry-"));
      tempDirs.push(homeDir);
      const service = createAgentHistoryService({ homeDir });
      const id = "12121212-1212-4121-8121-121212121212";
      const otherId = "34343434-3434-4343-8343-343434343434";
      const input: AgentHistoryLookupInput = {
        agent: "agy",
        agentSession: {
          agent: "agy",
          kind: "id",
          source: "herdr:antigravity_cli",
          value: id,
        },
      };
      vi.spyOn(console, "warn").mockImplementation(() => {});

      expect((await service.resolveCompactHistory(input)).historyRef).toBeNull();
      const conversations = join(homeDir, ".gemini", "antigravity-cli", "conversations");
      await mkdir(conversations, { recursive: true });
      await writeFile(join(conversations, `${id}.db`), "");

      // A changed official id is a new lookup key, so the pane is attempted
      // immediately instead of waiting out the previous failure's window.
      const other: AgentHistoryLookupInput = {
        ...input,
        agentSession: { agent: "agy", kind: "id", source: "herdr:antigravity_cli", value: otherId },
      };
      await writeFile(join(conversations, `${otherId}.db`), "");
      const resolved = await service.resolveCompactHistory(other);
      expect(resolved.historyRef).toMatchObject({ source: "antigravity-sqlite", value: otherId });
      expect(resolved.compactHistory.source).toBe("antigravity-sqlite");
    });

    test("a forced discovery bypasses the failure memory for unchanged official values", async () => {
      const homeDir = await mkdtemp(join(tmpdir(), "herdsman-history-force-bypass-"));
      tempDirs.push(homeDir);
      const service = createAgentHistoryService({ homeDir });
      const id = "56565656-5656-4565-8565-565656565656";
      const input: AgentHistoryLookupInput = {
        agent: "agy",
        agentSession: {
          agent: "agy",
          kind: "id",
          source: "herdr:antigravity_cli",
          value: id,
        },
      };
      const warn = vi.spyOn(console, "warn").mockImplementation(() => {});

      expect((await service.resolveCompactHistory(input)).historyRef).toBeNull();
      expect(warn).toHaveBeenCalledTimes(1);

      // The operator fixes the reported problem inside the 60s window and forces
      // a refresh: the memory only decelerates the ordinary cadence, so the
      // lookup happens for real. The warning stays deduplicated per key.
      const conversations = join(homeDir, ".gemini", "antigravity-cli", "conversations");
      await mkdir(conversations, { recursive: true });
      await writeFile(join(conversations, `${id}.db`), "");
      const stillSuppressed = await service.resolveCompactHistory(input);
      expect(stillSuppressed.historyRef).toBeNull();

      const forced = await service.resolveCompactHistory(input, { forceDiscovery: true });
      expect(forced.historyRef).toMatchObject({ source: "antigravity-sqlite", value: id });
      expect(forced.compactHistory.source).toBe("antigravity-sqlite");
      // The forced lookup really ran, yet the discovery warning is still reported
      // once per official value: forcing adds no warning noise. (The db written
      // here is an empty file, so the reader's own “no assistant message” warning
      // is counted separately.)
      const discoveryWarnings = warn.mock.calls.filter(([message]) =>
        String(message).includes("no usable official Herdr agent session"),
      );
      expect(discoveryWarnings).toHaveLength(1);
    });
  });
});

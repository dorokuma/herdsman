import { createConnection, type Socket } from "node:net";
import { encodeJsonLine, JsonLineDecoder } from "@/shared/json-lines.js";

export type HerdrRequestId = string;

export type HerdrSocketClientOptions = {
  socketPath: string;
};

type PendingRequest = {
  reject: (error: Error) => void;
  resolve: (value: unknown) => void;
};

type HerdrResponse = {
  data?: unknown;
  error?: { message?: string };
  event?: string;
  id?: string;
  method?: string;
  params?: unknown;
  result?: unknown;
};

type EventSubscriber = {
  fail(error: Error): void;
  push(event: unknown): void;
};

/** Session-wide topology events that must be subscribed even when no pane ids are known yet. */
export const HERDR_TOPOLOGY_EVENT_TYPES = [
  "pane.created",
  "pane.closed",
  "pane.moved",
  "pane.agent_detected",
  "workspace.closed",
] as const;

export class HerdrSocketClient {
  readonly #decoder = new JsonLineDecoder();
  readonly #pending = new Map<HerdrRequestId, PendingRequest>();
  readonly #subscribers = new Set<EventSubscriber>();
  readonly #socket: Socket;
  readonly #socketPath: string;
  #eventsSubscribed = false;
  #nextId = 1;

  constructor(options: HerdrSocketClientOptions) {
    this.#socketPath = options.socketPath;
    this.#socket = createConnection(options.socketPath);
    this.#socket.on("data", (chunk) => this.#handleData(chunk));
    this.#socket.on("error", (error) => this.#rejectAll(error));
    this.#socket.on("close", () => this.#rejectAll(new Error("Herdr socket closed")));
  }

  close(): void {
    this.#socket.destroy();
  }

  #request(method: string, params: unknown = {}, signal?: AbortSignal): Promise<unknown> {
    if (signal?.aborted) {
      return Promise.reject(new Error("Herdr request aborted"));
    }
    const id = `herdsman-${this.#nextId}`;
    this.#nextId += 1;

    return new Promise((resolve, reject) => {
      const onAbort = () => {
        if (!this.#pending.has(id)) return;
        this.#pending.delete(id);
        reject(new Error("Herdr request aborted"));
      };
      this.#pending.set(id, {
        reject: (error) => {
          signal?.removeEventListener("abort", onAbort);
          reject(error);
        },
        resolve: (value) => {
          signal?.removeEventListener("abort", onAbort);
          resolve(value);
        },
      });
      signal?.addEventListener("abort", onAbort, { once: true });
      this.#socket.write(encodeJsonLine({ id, method, params }));
    });
  }

  getPane(params: { pane_id: string }): Promise<unknown> {
    return this.#request("pane.get", params);
  }

  async sessionSnapshot(): Promise<unknown> {
    try {
      return await this.#requestOnce("session.snapshot");
    } catch (error) {
      if (!isUnsupportedSessionSnapshotError(error)) {
        throw error;
      }
    }

    const [workspacesResult, panesResult, tabsResult, agentsResult] = await Promise.all([
      this.#requestOnce("workspace.list"),
      this.#requestOnce("pane.list"),
      this.#requestOnce("tab.list"),
      this.#requestOnce("agent.list"),
    ]);
    const workspaces = arrayProperty(workspacesResult, "workspaces");
    const panes = arrayProperty(panesResult, "panes");
    const tabs = arrayProperty(tabsResult, "tabs");
    const agents = arrayProperty(agentsResult, "agents");

    return {
      snapshot: {
        agents,
        ...focusedId("focused_pane_id", panes, "pane_id"),
        ...focusedId("focused_workspace_id", workspaces, "workspace_id"),
        panes,
        tabs,
        workspaces,
      },
    };
  }

  async *subscribeEvents(
    params: { paneIds?: string[] } = {},
    options: { signal?: AbortSignal } = {},
  ): AsyncIterable<unknown> {
    // Herdr 0.9 / protocol 22 ACKs the first events.subscribe, then resets the
    // socket if the same connection sends it again. Never re-subscribe here.
    if (this.#eventsSubscribed) {
      throw new Error("Herdr connection already has an events.subscribe");
    }
    const queue: unknown[] = [];
    let failure: Error | undefined;
    let wake: (() => void) | undefined;
    const subscriber: EventSubscriber = {
      fail(error) {
        failure ??= error;
        wake?.();
        wake = undefined;
      },
      push(event) {
        queue.push(event);
        wake?.();
        wake = undefined;
      },
    };
    if (options.signal?.aborted) return;
    this.#eventsSubscribed = true;
    this.#subscribers.add(subscriber);
    try {
      try {
        await this.#request(
          "events.subscribe",
          {
            subscriptions: [
              ...HERDR_TOPOLOGY_EVENT_TYPES.map((type) => ({ type })),
              ...(params.paneIds ?? []).map((pane_id) => ({
                pane_id,
                type: "pane.agent_status_changed" as const,
              })),
            ],
          },
          options.signal,
        );
      } catch (error) {
        if (!options.signal?.aborted) throw error;
      }
      // Drain events already queued before honoring abort so a restart does
      // not drop status already received for panes this connection subscribed
      // to. New panes are not visible here: herdr only emits
      // pane.agent_status_changed for pane-specific subscriptions.
      while (true) {
        if (failure) throw failure;
        if (queue.length > 0) {
          yield queue.shift();
          continue;
        }
        if (options.signal?.aborted) return;
        await new Promise<void>((resolve) => {
          wake = resolve;
          options.signal?.addEventListener("abort", () => resolve(), { once: true });
        });
      }
    } finally {
      this.#subscribers.delete(subscriber);
    }
  }

  #requestOnce(method: string, params: unknown = {}): Promise<unknown> {
    const id = `herdsman-${this.#nextId}`;
    this.#nextId += 1;

    return new Promise((resolve, reject) => {
      const decoder = new JsonLineDecoder();
      const socket = createConnection(this.#socketPath);
      let settled = false;
      const finish = (result: { error?: Error; value?: unknown }) => {
        if (settled) {
          return;
        }
        settled = true;
        socket.destroy();
        if (result.error) {
          reject(result.error);
          return;
        }
        resolve(result.value);
      };

      socket.on("connect", () => socket.write(encodeJsonLine({ id, method, params })));
      socket.on("data", (chunk) => {
        for (const message of decoder.push(chunk.toString("utf8"))) {
          const response = message as HerdrResponse;
          if (response.error) {
            finish({ error: new Error(response.error.message ?? "Herdr request failed") });
            return;
          }
          if (response.id === id) {
            finish({ value: response.result });
            return;
          }
        }
      });
      socket.on("error", (error) => finish({ error }));
      socket.on("close", () => finish({ error: new Error("Herdr socket closed") }));
    });
  }

  #handleData(chunk: Buffer): void {
    for (const message of this.#decoder.push(chunk.toString("utf8"))) {
      const response = message as HerdrResponse;
      if (!response.id) {
        this.#publishNotification(response);
        continue;
      }

      const pending = this.#pending.get(response.id);
      if (!pending) {
        continue;
      }

      this.#pending.delete(response.id);
      if (response.error) {
        pending.reject(new Error(response.error.message ?? "Herdr request failed"));
        continue;
      }

      pending.resolve(response.result);
    }
  }

  #publishNotification(message: HerdrResponse): void {
    const event = notificationEvent(message);
    for (const subscriber of this.#subscribers) {
      subscriber.push(event);
    }
  }

  #rejectAll(error: Error): void {
    for (const pending of this.#pending.values()) {
      pending.reject(error);
    }
    this.#pending.clear();
    for (const subscriber of this.#subscribers) {
      subscriber.fail(error);
    }
  }
}

function notificationEvent(message: HerdrResponse): unknown {
  if (typeof message.event === "string" && isRecord(message.data)) {
    return {
      ...message.data,
      type: normalizeEventName(message.event),
    };
  }
  if (isRecord(message.params)) {
    return notificationPayload(message.params.event ?? message.params);
  }
  return notificationPayload(message.result ?? message);
}

function notificationPayload(value: unknown): unknown {
  if (!isRecord(value) || typeof value.event !== "string" || !isRecord(value.data)) return value;
  return {
    ...value.data,
    type: normalizeEventName(value.event),
  };
}

function normalizeEventName(value: string): string {
  return value.includes(".") ? value : value.replace("_", ".");
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}

function isUnsupportedSessionSnapshotError(error: unknown): boolean {
  if (!(error instanceof Error)) {
    return false;
  }
  return error.message.includes("session.snapshot") && error.message.includes("unknown variant");
}

function arrayProperty(value: unknown, key: string): unknown[] {
  if (typeof value !== "object" || value === null) {
    return [];
  }
  const property = (value as Record<string, unknown>)[key];
  return Array.isArray(property) ? property : [];
}

function focusedId(
  outputKey: string,
  records: unknown[],
  recordKey: string,
): Record<string, string> {
  const focused = records.find(
    (record) =>
      typeof record === "object" &&
      record !== null &&
      (record as { focused?: unknown }).focused === true,
  );
  if (typeof focused !== "object" || focused === null) {
    return {};
  }
  const id = (focused as Record<string, unknown>)[recordKey];
  return typeof id === "string" ? { [outputKey]: id } : {};
}

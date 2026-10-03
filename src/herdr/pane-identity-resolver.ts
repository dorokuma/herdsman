import { HerdrSocketClient } from "@/herdr/socket-client.js";

export type HerdrPaneIdentity = {
  paneId: string;
  terminalId: string;
  workspaceId: string;
  agent?: string;
  cwd?: string;
  foregroundCwd?: string;
  tabId?: string;
};

type PaneClient = Pick<HerdrSocketClient, "close" | "getPane">;

/**
 * Reads pane identity straight from Herdr's own `pane.get` answer.
 *
 * Herdr 0.9.3 answers `pane.get` with `{type: "pane_info", pane: PaneInfo}`
 * (verified live and in `herdr api schema --json`: the `pane.get` result is the
 * `pane_info` envelope, and `PaneInfo` carries `pane_id`, `terminal_id`,
 * `workspace_id`, `tab_id`, `cwd`, `foreground_cwd`, `agent`). Herdsman
 * therefore consumes those official fields as-is: no camelCase aliases, no
 * unwrapped response shape, no local mapping of identity fields. Herdr reports
 * every field this type needs, so nothing is supplemented locally.
 */
export async function resolveHerdrPaneIdentity(input: {
  clientFactory?: (socketPath: string) => PaneClient;
  paneId: string;
  socketPath: string;
}): Promise<HerdrPaneIdentity> {
  const client = input.clientFactory
    ? input.clientFactory(input.socketPath)
    : new HerdrSocketClient({ socketPath: input.socketPath });
  try {
    const result = await client.getPane({ pane_id: input.paneId });
    const pane = paneInfo(result);
    const paneId = stringField(pane, "pane_id");
    const terminalId = stringField(pane, "terminal_id");
    const workspaceId = stringField(pane, "workspace_id");
    if (!paneId || !terminalId || !workspaceId) {
      throw new Error("Herdr pane response has no terminal identity");
    }
    const agent = stringField(pane, "agent");
    const cwd = stringField(pane, "cwd");
    const foregroundCwd = stringField(pane, "foreground_cwd");
    const tabId = stringField(pane, "tab_id");
    return {
      paneId,
      terminalId,
      workspaceId,
      ...(agent ? { agent } : {}),
      ...(cwd ? { cwd } : {}),
      ...(foregroundCwd ? { foregroundCwd } : {}),
      ...(tabId ? { tabId } : {}),
    };
  } finally {
    client.close();
  }
}

/** `pane.get` only answers with the official `pane_info` envelope; anything else is a contract violation. */
function paneInfo(value: unknown): Record<string, unknown> {
  if (!isRecord(value) || !isRecord(value.pane)) {
    throw new Error("Invalid Herdr pane response");
  }
  return value.pane;
}

function stringField(value: Record<string, unknown>, field: string): string | undefined {
  const fieldValue = value[field];
  return typeof fieldValue === "string" && fieldValue.length > 0 ? fieldValue : undefined;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}

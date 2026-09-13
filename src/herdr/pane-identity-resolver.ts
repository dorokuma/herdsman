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
    const pane = paneRecord(result);
    const paneId = stringField(pane, "pane_id", "paneId");
    const terminalId = stringField(pane, "terminal_id", "terminalId");
    const workspaceId = stringField(pane, "workspace_id", "workspaceId");
    if (!paneId || !terminalId || !workspaceId) {
      throw new Error("Herdr pane response has no terminal identity");
    }
    const agent = stringField(pane, "agent", "agent");
    const cwd = stringField(pane, "cwd", "cwd");
    const foregroundCwd = stringField(pane, "foreground_cwd", "foregroundCwd");
    const tabId = stringField(pane, "tab_id", "tabId");
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

function paneRecord(value: unknown): Record<string, unknown> {
  if (!isRecord(value)) throw new Error("Invalid Herdr pane response");
  const pane = value.pane;
  return isRecord(pane) ? pane : value;
}

function stringField(
  value: Record<string, unknown>,
  snakeCase: string,
  camelCase: string,
): string | undefined {
  const field = value[snakeCase] ?? value[camelCase];
  return typeof field === "string" && field.length > 0 ? field : undefined;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}

import { Type } from "@sinclair/typebox";

export const agentSessionRefSchema = Type.Object(
  {
    agent: Type.String({ minLength: 1 }),
    kind: Type.Union([Type.Literal("id"), Type.Literal("path")]),
    source: Type.String({ minLength: 1 }),
    value: Type.String({ minLength: 1 }),
  },
  { additionalProperties: false },
);

export const agentListInputSchema = Type.Object(
  {
    all: Type.Optional(Type.Boolean()),
    herdrSessionName: Type.Optional(Type.String({ minLength: 1 })),
    // Expand the caller's workspace with sibling workspaces of the same Herdr
    // session that have no orchestrator owner of their own (cross-workspace
    // dispatched workers).
    includeOwnerlessWorkspaces: Type.Optional(Type.Boolean()),
    workspaceId: Type.Optional(Type.String({ minLength: 1 })),
  },
  { additionalProperties: false },
);

export const agentGetInputSchema = Type.Object(
  {
    herdrSessionName: Type.Optional(Type.String({ minLength: 1 })),
    target: Type.String({ minLength: 1 }),
    workspaceId: Type.Optional(Type.String({ minLength: 1 })),
  },
  { additionalProperties: false },
);

export const agentReadInputSchema = Type.Object(
  {
    herdrSessionName: Type.Optional(Type.String({ minLength: 1 })),
    limit: Type.Optional(Type.Integer({ minimum: 1, maximum: 500 })),
    target: Type.String({ minLength: 1 }),
    workspaceId: Type.Optional(Type.String({ minLength: 1 })),
  },
  { additionalProperties: false },
);

export const agentEventsInputSchema = Type.Object(
  {
    afterEventId: Type.Optional(Type.Integer({ minimum: 0 })),
    herdrSessionName: Type.Optional(Type.String({ minLength: 1 })),
    limit: Type.Optional(Type.Integer({ minimum: 1, maximum: 500 })),
    workspaceId: Type.Optional(Type.String({ minLength: 1 })),
  },
  { additionalProperties: false },
);

const piPresenceSessionRefSchema = Type.Union([
  Type.Object(
    {
      // pi and pig run the same herdsman-pi extension and register the same
      // `kind: "path"` sessionRef; pig reports `agent: "pig"` / `herdr:pig`.
      agent: Type.Union([Type.Literal("pi"), Type.Literal("pig")]),
      kind: Type.Literal("path"),
      source: Type.String({ minLength: 1 }),
      value: Type.String({ minLength: 1 }),
    },
    { additionalProperties: false },
  ),
  Type.Object(
    {
      agent: Type.Literal("agy"),
      kind: Type.Union([Type.Literal("id"), Type.Literal("path")]),
      source: Type.String({ minLength: 1 }),
      value: Type.String({ minLength: 1 }),
    },
    { additionalProperties: false },
  ),
]);

export const agentOrchestratorRegisterInputSchema = Type.Object(
  {
    herdrSocketPath: Type.String({ minLength: 1 }),
    paneId: Type.String({ minLength: 1 }),
    // `sessionRef.agent` (the session's own identity, "pi" | "pig") and
    // `subscriberKind` (the terminal's subscription kind, "pi" | "pig" |
    // "agy") are validated independently on purpose: there is no cross-field
    // equality constraint, so a mixed deployment (e.g. a Go extension that
    // reports `agent: "pig"` while subscribing as `subscriberKind: "pi"`)
    // still registers.
    sessionRef: piPresenceSessionRefSchema,
    subscriberId: Type.String({ minLength: 1 }),
    subscriberKind: Type.Union([Type.Literal("pi"), Type.Literal("pig"), Type.Literal("agy")]),
    workspaceId: Type.String({ minLength: 1 }),
  },
  { additionalProperties: false },
);

export const agentOrchestratorSetInputSchema = Type.Object(
  { enabled: Type.Boolean() },
  { additionalProperties: false },
);

export const agentOrchestratorGetInputSchema = Type.Object({}, { additionalProperties: false });

export const agentPingInputSchema = Type.Object({}, { additionalProperties: false });

export const agentOrchestratorAckInputSchema = Type.Object(
  { eventId: Type.Integer({ minimum: 1 }) },
  { additionalProperties: false },
);

export const agentTurnCompletedInputSchema = Type.Object(
  {
    confirmed: Type.Boolean(),
    expectedText: Type.Optional(Type.String()),
    herdrSessionName: Type.String({ minLength: 1 }),
    paneId: Type.String({ minLength: 1 }),
    terminalId: Type.String({ minLength: 1 }),
    workspaceId: Type.String({ minLength: 1 }),
  },
  { additionalProperties: false },
);

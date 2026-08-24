export const ORCHESTRATOR_ACK_ERROR_CODES = {
  ORCHESTRATOR_NOT_OWNER: { retryable: false },
  ORCHESTRATOR_EVENT_INVALIDATED: { retryable: false },
  ORCHESTRATOR_EVENT_FAILED: { retryable: false },
  ORCHESTRATOR_EVENT_ALREADY_ACKED: { retryable: false },
  ORCHESTRATOR_EVENT_NOT_IN_SCOPE: { retryable: false },
  ORCHESTRATOR_EVENT_OUT_OF_ORDER: { retryable: false },
  ORCHESTRATOR_OWNER_REPLACED: { retryable: false },
  ORCHESTRATOR_EVENT_NOT_FOUND: { retryable: false },
  ORCHESTRATOR_BUSY: { retryable: true },
  ORCHESTRATOR_CONNECTION_LOST: { retryable: true },
  ORCHESTRATOR_RECONCILING: { retryable: true },
  ORCHESTRATOR_ACK_TIMEOUT: { retryable: true },
} as const;

export type OrchestratorAckErrorCode = keyof typeof ORCHESTRATOR_ACK_ERROR_CODES;

export class OrchestratorAckError extends Error {
  readonly code: OrchestratorAckErrorCode;
  readonly retryable: boolean;

  constructor(input: { code: OrchestratorAckErrorCode; message: string }) {
    super(input.message);
    this.name = "OrchestratorAckError";
    this.code = input.code;
    this.retryable = ORCHESTRATOR_ACK_ERROR_CODES[input.code].retryable;
  }
}

export const ORCHESTRATOR_ACK_MESSAGES = {
  notOwner: "Only the current orchestrator can acknowledge notifications",
  invalidated: "orchestrator event is no longer pending (invalidated)",
  outOfOrder: "Only the next pending orchestrator event can be acknowledged",
} as const;

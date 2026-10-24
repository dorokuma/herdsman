import { type Static, Type } from "@sinclair/typebox";
import { Ajv, type ErrorObject } from "ajv";

const runtimePathsSchema = Type.Object(
  {
    db_path: Type.Optional(Type.String({ minLength: 1 })),
    log_path: Type.Optional(Type.String({ minLength: 1 })),
    pid_path: Type.Optional(Type.String({ minLength: 1 })),
    socket_path: Type.Optional(Type.String({ minLength: 1 })),
  },
  { additionalProperties: false },
);

const statusPlansSchema = Type.Object(
  {
    backoff_factor: Type.Optional(Type.Number({ minimum: 1, default: 2 })),
    initial_backoff_ms: Type.Optional(Type.Integer({ minimum: 1, default: 5_000 })),
    max_backoff_ms: Type.Optional(Type.Integer({ minimum: 1, default: 60_000 })),
    total_window_ms: Type.Optional(Type.Integer({ minimum: 1, default: 15 * 60 * 1000 })),
  },
  { additionalProperties: false },
);

export type StatusPlansConfig = Static<typeof statusPlansSchema>;

const observabilitySchema = Type.Object(
  {
    status_plans: Type.Optional(statusPlansSchema),
    telemetry: Type.Optional(
      Type.Object(
        {
          max_excerpt_bytes: Type.Optional(Type.Integer({ minimum: 1, default: 4096 })),
        },
        { additionalProperties: false },
      ),
    ),
  },
  { additionalProperties: false },
);

const wakeSchema = Type.Object(
  {
    extra_upstream_error_patterns: Type.Optional(
      Type.Array(Type.String({ minLength: 1 }), { default: [] }),
    ),
    filter_upstream_errors: Type.Optional(Type.Boolean({ default: true })),
  },
  { additionalProperties: false },
);

export const herdsmanConfigSchema = Type.Object(
  {
    observability: Type.Optional(observabilitySchema),
    runtime: Type.Optional(runtimePathsSchema),
    wake: Type.Optional(wakeSchema),
  },
  { additionalProperties: false },
);

export type HerdsmanConfig = Static<typeof herdsmanConfigSchema>;

export type ValidationResult<T> = { ok: true; value: T } | { errors: ErrorObject[]; ok: false };

const ajv = new Ajv({ allErrors: true, useDefaults: true });
const validateHerdsmanConfig = ajv.compile<HerdsmanConfig>(herdsmanConfigSchema);

export function parseHerdsmanConfig(value: unknown): ValidationResult<HerdsmanConfig> {
  if (validateHerdsmanConfig(value)) {
    return { ok: true, value: value as HerdsmanConfig };
  }

  return { errors: validateHerdsmanConfig.errors ?? [], ok: false };
}

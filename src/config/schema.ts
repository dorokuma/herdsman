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

const observabilitySchema = Type.Object(
  {
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

export const herdsmanConfigSchema = Type.Object(
  {
    observability: Type.Optional(observabilitySchema),
    runtime: Type.Optional(runtimePathsSchema),
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

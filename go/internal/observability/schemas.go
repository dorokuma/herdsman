package observability

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
)

// This file ports the TypeBox schemas from src/observability/schemas.ts.
//
// TypeBox is a JSON-Schema subset, so rather than hand-coding each validator
// the Go port describes the same constraints as data (the minimum needed to
// reproduce Value.Check for these schemas) and validates against that. Every
// schema below uses `additionalProperties: false`, so unknown keys are rejected;
// `Type.Object({}, ...)` accepts only the empty object.

// fieldKind enumerates the primitive shapes used by the observability schemas.
type fieldKind int

const (
	kindString fieldKind = iota
	kindBoolean
	kindInteger
	kindObject
	kindObjectUnion
	kindLiteralSet
)

// fieldSpec describes one property of an object schema.
type fieldSpec struct {
	kind       fieldKind
	optional   bool
	minLength  int     // strings
	minimum    float64 // integers
	maximum    float64
	hasMinimum bool
	hasMaximum bool
	literals   []string // kindLiteralSet
	nested     *ObjectSchema
	union      *objectUnionSchema // kindObjectUnion
}

// ObjectSchema is the runtime form of a TypeBox object with
// additionalProperties: false.
type ObjectSchema struct {
	Name       string
	Properties map[string]*fieldSpec
}

// Check reports whether value satisfies the schema. The input is a decoded JSON
// value (the same shape TypeBox receives).
func (s *ObjectSchema) Check(value any) bool {
	obj, ok := value.(map[string]any)
	if !ok {
		return false
	}
	// additionalProperties: false
	for key := range obj {
		if _, known := s.Properties[key]; !known {
			return false
		}
	}
	for name, spec := range s.Properties {
		raw, present := obj[name]
		if !present {
			if spec.optional {
				continue
			}
			return false
		}
		if !spec.check(raw) {
			return false
		}
	}
	return true
}

func (f *fieldSpec) check(value any) bool {
	switch f.kind {
	case kindString:
		s, ok := value.(string)
		return ok && len([]rune(s)) >= f.minLength
	case kindBoolean:
		_, ok := value.(bool)
		return ok
	case kindInteger:
		n, ok := asNumber(value)
		if !ok || n != math.Trunc(n) {
			return false
		}
		if f.hasMinimum && n < f.minimum {
			return false
		}
		if f.hasMaximum && n > f.maximum {
			return false
		}
		return true
	case kindLiteralSet:
		s, ok := value.(string)
		if !ok {
			return false
		}
		for _, literal := range f.literals {
			if s == literal {
				return true
			}
		}
		return false
	case kindObject:
		return f.nested != nil && f.nested.Check(value)
	case kindObjectUnion:
		return f.union != nil && f.union.Check(value)
	}
	return false
}

// asNumber accepts the numeric shapes encoding/json produces (float64) plus Go
// integer variants so callers may pass typed values directly.
func asNumber(value any) (float64, bool) {
	switch n := value.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}

// CheckJSON decodes raw JSON and validates it against the schema, matching
// Value.Check(schema, JSON.parse(raw)).
func (s *ObjectSchema) CheckJSON(raw []byte) (bool, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, fmt.Errorf("observability: decode schema input: %w", err)
	}
	return s.Check(value), nil
}

// ---------------------------------------------------------------------------
// Schema definitions, mirroring src/observability/schemas.ts exactly.
// ---------------------------------------------------------------------------

func str(minLength int) *fieldSpec { return &fieldSpec{kind: kindString, minLength: minLength} }
func opt(spec *fieldSpec) *fieldSpec {
	clone := *spec
	clone.optional = true
	return &clone
}
func boolean() *fieldSpec { return &fieldSpec{kind: kindBoolean} }
func integer(minimum, maximum float64, hasMax bool) *fieldSpec {
	return &fieldSpec{kind: kindInteger, minimum: minimum, hasMinimum: true, maximum: maximum, hasMaximum: hasMax}
}
func literals(values ...string) *fieldSpec   { return &fieldSpec{kind: kindLiteralSet, literals: values} }
func object(nested *ObjectSchema) *fieldSpec { return &fieldSpec{kind: kindObject, nested: nested} }

// AgentSessionRefSchema mirrors agentSessionRefSchema.
var AgentSessionRefSchema = &ObjectSchema{
	Name: "agentSessionRefSchema",
	Properties: map[string]*fieldSpec{
		"agent":  str(1),
		"kind":   literals("id", "path"),
		"source": str(1),
		"value":  str(1),
	},
}

// AgentListInputSchema mirrors agentListInputSchema.
var AgentListInputSchema = &ObjectSchema{
	Name: "agentListInputSchema",
	Properties: map[string]*fieldSpec{
		"all":                        opt(boolean()),
		"herdrSessionName":           opt(str(1)),
		"includeOwnerlessWorkspaces": opt(boolean()),
		"workspaceId":                opt(str(1)),
	},
}

// AgentGetInputSchema mirrors agentGetInputSchema.
var AgentGetInputSchema = &ObjectSchema{
	Name: "agentGetInputSchema",
	Properties: map[string]*fieldSpec{
		"herdrSessionName": opt(str(1)),
		"target":           str(1),
		"workspaceId":      opt(str(1)),
	},
}

// AgentReadInputSchema mirrors agentReadInputSchema.
var AgentReadInputSchema = &ObjectSchema{
	Name: "agentReadInputSchema",
	Properties: map[string]*fieldSpec{
		"herdrSessionName": opt(str(1)),
		"limit":            opt(integer(1, 500, true)),
		"target":           str(1),
		"workspaceId":      opt(str(1)),
	},
}

// AgentEventsInputSchema mirrors agentEventsInputSchema.
var AgentEventsInputSchema = &ObjectSchema{
	Name: "agentEventsInputSchema",
	Properties: map[string]*fieldSpec{
		"afterEventId":     opt(integer(0, 0, false)),
		"herdrSessionName": opt(str(1)),
		"limit":            opt(integer(1, 500, true)),
		"workspaceId":      opt(str(1)),
	},
}

// piPresenceSessionRefPiSchema is the pi-family (`agent: "pi"` | `"pig"`)
// branch: `kind` is `path` only. pi and pig run the same herdsman-pi
// extension and register the same `kind: "path"` sessionRef.
var piPresenceSessionRefPiSchema = &ObjectSchema{
	Name: "piPresenceSessionRefSchema[pi]",
	Properties: map[string]*fieldSpec{
		"agent":  literals(string(SubscriberPi), string(SubscriberPig)),
		"kind":   literals(string(SessionRefPath)),
		"source": str(1),
		"value":  str(1),
	},
}

// piPresenceSessionRefAgySchema is the `agent: "agy"` branch: `kind` is id|path.
var piPresenceSessionRefAgySchema = &ObjectSchema{
	Name: "piPresenceSessionRefSchema[agy]",
	Properties: map[string]*fieldSpec{
		"agent":  literals(string(SubscriberAgy)),
		"kind":   literals(string(SessionRefID), string(SessionRefPath)),
		"source": str(1),
		"value":  str(1),
	},
}

// AgentOrchestratorRegisterInputSchema mirrors agentOrchestratorRegisterInputSchema,
// including the pi/agy sessionRef union. The union is modelled as a nested
// anyOf check so `sessionRef` keeps the discriminator semantics exactly.
//
// `sessionRef.agent` (the session's own identity, "pi" | "pig") and
// `subscriberKind` (the terminal's subscription kind, "pi" | "pig" | "agy")
// are validated independently on purpose: there is no cross-field equality
// constraint, so a mixed deployment (e.g. a Go extension that reports
// `agent: "pig"` while subscribing as `subscriberKind: "pi"`) still
// registers.
var AgentOrchestratorRegisterInputSchema = &ObjectSchema{
	Name: "agentOrchestratorRegisterInputSchema",
	Properties: map[string]*fieldSpec{
		"herdrSocketPath": str(1),
		"paneId":          str(1),
		"sessionRef": {
			kind:  kindObjectUnion,
			union: sessionRefUnionSchema,
		},
		"subscriberId":   str(1),
		"subscriberKind": literals(
			string(SubscriberPi), string(SubscriberPig), string(SubscriberAgy)),
		"workspaceId":    str(1),
	},
}

// sessionRefUnionSchema validates the pi|agy union by trying each branch.
var sessionRefUnionSchema = &objectUnionSchema{
	branches: []*ObjectSchema{piPresenceSessionRefPiSchema, piPresenceSessionRefAgySchema},
}

// objectUnionSchema is a Type.Union of objects: `anyOf` semantics.
type objectUnionSchema struct {
	branches []*ObjectSchema
}

// Check reports whether value matches at least one branch.
func (u *objectUnionSchema) Check(value any) bool {
	for _, branch := range u.branches {
		if branch.Check(value) {
			return true
		}
	}
	return false
}

// AgentOrchestratorSetInputSchema mirrors agentOrchestratorSetInputSchema.
var AgentOrchestratorSetInputSchema = &ObjectSchema{
	Name: "agentOrchestratorSetInputSchema",
	Properties: map[string]*fieldSpec{
		"enabled": boolean(),
	},
}

// AgentOrchestratorGetInputSchema mirrors agentOrchestratorGetInputSchema:
// an empty object with additionalProperties: false.
var AgentOrchestratorGetInputSchema = &ObjectSchema{
	Name:       "agentOrchestratorGetInputSchema",
	Properties: map[string]*fieldSpec{},
}

// AgentPingInputSchema mirrors agentPingInputSchema: also an empty object.
var AgentPingInputSchema = &ObjectSchema{
	Name:       "agentPingInputSchema",
	Properties: map[string]*fieldSpec{},
}

// AgentOrchestratorAckInputSchema mirrors agentOrchestratorAckInputSchema.
var AgentOrchestratorAckInputSchema = &ObjectSchema{
	Name: "agentOrchestratorAckInputSchema",
	Properties: map[string]*fieldSpec{
		"eventId": integer(1, 0, false),
	},
}

// AgentTurnCompletedInputSchema mirrors agentTurnCompletedInputSchema.
var AgentTurnCompletedInputSchema = &ObjectSchema{
	Name: "agentTurnCompletedInputSchema",
	Properties: map[string]*fieldSpec{
		"confirmed":        boolean(),
		"expectedText":     opt(&fieldSpec{kind: kindString}),
		"herdrSessionName": str(1),
		"paneId":           str(1),
		"terminalId":       str(1),
		"workspaceId":      str(1),
	},
}

// OrderedSchemaNames is the deterministic iteration order for the schemas above,
// useful for table-driven tests and diagnostics.
var OrderedSchemaNames = []string{
	"agentSessionRefSchema",
	"agentListInputSchema",
	"agentGetInputSchema",
	"agentReadInputSchema",
	"agentEventsInputSchema",
	"agentOrchestratorRegisterInputSchema",
	"agentOrchestratorSetInputSchema",
	"agentOrchestratorGetInputSchema",
	"agentPingInputSchema",
	"agentOrchestratorAckInputSchema",
	"agentTurnCompletedInputSchema",
}

// Schemas returns the named schemas keyed by their TS export name.
func Schemas() map[string]*ObjectSchema {
	schemas := map[string]*ObjectSchema{
		"agentSessionRefSchema":                AgentSessionRefSchema,
		"agentListInputSchema":                 AgentListInputSchema,
		"agentGetInputSchema":                  AgentGetInputSchema,
		"agentReadInputSchema":                 AgentReadInputSchema,
		"agentEventsInputSchema":               AgentEventsInputSchema,
		"agentOrchestratorRegisterInputSchema": AgentOrchestratorRegisterInputSchema,
		"agentOrchestratorSetInputSchema":      AgentOrchestratorSetInputSchema,
		"agentOrchestratorGetInputSchema":      AgentOrchestratorGetInputSchema,
		"agentPingInputSchema":                 AgentPingInputSchema,
		"agentOrchestratorAckInputSchema":      AgentOrchestratorAckInputSchema,
		"agentTurnCompletedInputSchema":        AgentTurnCompletedInputSchema,
	}
	_ = sort.Strings
	return schemas
}

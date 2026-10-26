package config

// This file ports src/config/schema.ts.
//
// The TS module builds the schema with TypeBox and validates with Ajv
// (`allErrors: true, useDefaults: true`). The Go port reproduces the same three
// observable behaviours:
//
//  1. structural validation: every object rejects unknown keys
//     (`additionalProperties: false`), required-by-type checks, minLength 1 on
//     strings, minimum bounds on numbers, and the string-array element shape;
//  2. default application: `useDefaults: true` mutates the document so optional
//     fields with defaults appear in the accepted value;
//  3. error reporting: errors carry `keyword` + `instancePath` (+ message) like
//     Ajv's ErrorObject, because both the schema tests and the loader assert on
//     `error.keyword` and the loader composes a fallback message from the path.
//
// Validation therefore runs over a generic decoded document (map[string]any)
// rather than struct tags: JSON-Schema semantics (defaults written back into the
// input, "unknown key" detection, Ajv-compatible paths) cannot be expressed
// faithfully with encoding/json struct validation.

import (
	"fmt"
	"sort"
	"strings"
)

// ValidationError mirrors Ajv's ErrorObject for the keywords this schema uses.
type ValidationError struct {
	// InstancePath mirrors Ajv's instancePath, e.g. "/runtime/extra".
	InstancePath string
	// Keyword mirrors Ajv's keyword, e.g. "additionalProperties".
	Keyword string
	// Message mirrors Ajv's message, e.g. "must NOT have additional properties".
	Message string
}

// ValidationResult mirrors `ValidationResult<T>`: either ok with a value or
// errors. It is a struct rather than a tagged union because Go has no sum type;
// OK is the discriminator and mirrors the TS `ok` field.
type ValidationResult struct {
	OK     bool
	Value  HerdsmanConfig
	Errors []ValidationError
}

// RuntimePathsConfig mirrors runtimePathsSchema (all fields optional).
type RuntimePathsConfig struct {
	DBPath     *string
	LogPath    *string
	PIDPath    *string
	SocketPath *string
}

// StatusPlansConfig mirrors statusPlansSchema, with Ajv defaults applied.
type StatusPlansConfig struct {
	BackoffFactor    float64
	InitialBackoffMs float64
	MaxBackoffMs     float64
	TotalWindowMs    float64
}

// TelemetryConfig mirrors the nested telemetry object.
type TelemetryConfig struct {
	MaxExcerptBytes float64
}

// ObservabilityConfig mirrors observabilitySchema.
type ObservabilityConfig struct {
	StatusPlans *StatusPlansConfig
	Telemetry   *TelemetryConfig
}

// WakeConfig mirrors wakeSchema, with Ajv defaults applied.
type WakeConfig struct {
	ExtraUpstreamErrorPatterns []string
	FilterUpstreamErrors       bool
}

// HerdsmanConfig mirrors the Static<typeof herdsmanConfigSchema> type.
type HerdsmanConfig struct {
	Observability *ObservabilityConfig
	Runtime       *RuntimePathsConfig
	Wake          *WakeConfig
}

// Schema defaults, mirroring the TypeBox `default` options.
const (
	DefaultBackoffFactor            = 2.0
	DefaultInitialBackoffMs         = 5000.0
	DefaultMaxBackoffMs             = 60000.0
	DefaultStatusPlansTotalWindowMs = 15 * 60 * 1000.0
	DefaultMaxExcerptBytes          = 4096.0
	DefaultFilterUpstreamErrors     = true
)

// keyword constants mirror Ajv's keyword names.
const (
	keywordAdditionalProperties = "additionalProperties"
	keywordType                 = "type"
	keywordMinLength            = "minLength"
	keywordMinimum              = "minimum"
	keywordRequired             = "required"
)

// ParseHerdsmanConfig mirrors parseHerdsmanConfig. The input document is
// validated and, on success, defaults are filled in (mutating doc like Ajv's
// useDefaults) and the typed value is returned.
func ParseHerdsmanConfig(doc map[string]any) ValidationResult {
	errors := make([]ValidationError, 0, 4)
	config := HerdsmanConfig{}

	// Top level: additionalProperties false over observability/runtime/wake.
	if doc == nil {
		// Ajv: `null` fails `type: "object"`.
		return ValidationResult{Errors: []ValidationError{{
			InstancePath: "",
			Keyword:      keywordType,
			Message:      "must be object",
		}}}
	}
	checkAdditionalProperties(doc, []string{"observability", "runtime", "wake"}, "", &errors)

	if raw, present := doc["runtime"]; present {
		if err := validateRuntimePaths(raw, &config, &errors); err != nil {
			return ValidationResult{Errors: errors}
		}
	}
	if raw, present := doc["observability"]; present {
		if err := validateObservability(raw, &config, &errors); err != nil {
			return ValidationResult{Errors: errors}
		}
	}
	if raw, present := doc["wake"]; present {
		if err := validateWake(raw, &config, &errors); err != nil {
			return ValidationResult{Errors: errors}
		}
	}

	if len(errors) > 0 {
		sortErrors(errors)
		return ValidationResult{Errors: errors}
	}
	return ValidationResult{OK: true, Value: config}
}

// validateRuntimePaths mirrors runtimePathsSchema.
func validateRuntimePaths(raw any, config *HerdsmanConfig, errors *[]ValidationError) error {
	obj, ok := raw.(map[string]any)
	if !ok {
		*errors = append(*errors, typeError("/runtime", "object"))
		return nil
	}
	allowed := []string{"db_path", "log_path", "pid_path", "socket_path"}
	checkAdditionalProperties(obj, allowed, "/runtime", errors)

	runtime := &RuntimePathsConfig{}
	for _, field := range allowed {
		value, present := obj[field]
		if !present {
			continue
		}
		str, ok := nonEmptyString(value, "/runtime/"+field, errors)
		if !ok {
			continue
		}
		switch field {
		case "db_path":
			runtime.DBPath = &str
		case "log_path":
			runtime.LogPath = &str
		case "pid_path":
			runtime.PIDPath = &str
		case "socket_path":
			runtime.SocketPath = &str
		}
	}
	config.Runtime = runtime
	return nil
}

// validateObservability mirrors observabilitySchema.
func validateObservability(raw any, config *HerdsmanConfig, errors *[]ValidationError) error {
	obj, ok := raw.(map[string]any)
	if !ok {
		*errors = append(*errors, typeError("/observability", "object"))
		return nil
	}
	checkAdditionalProperties(obj, []string{"status_plans", "telemetry"}, "/observability", errors)

	observability := &ObservabilityConfig{}
	if value, present := obj["status_plans"]; present {
		plans, err := validateStatusPlans(value, errors)
		if err != nil {
			return err
		}
		observability.StatusPlans = plans
	}
	if value, present := obj["telemetry"]; present {
		telemetry, err := validateTelemetry(value, errors)
		if err != nil {
			return err
		}
		observability.Telemetry = telemetry
	}
	config.Observability = observability
	return nil
}

// validateStatusPlans mirrors statusPlansSchema with its four defaults.
func validateStatusPlans(raw any, errors *[]ValidationError) (*StatusPlansConfig, error) {
	obj, ok := raw.(map[string]any)
	if !ok {
		*errors = append(*errors, typeError("/observability/status_plans", "object"))
		return nil, nil
	}
	allowed := []string{"backoff_factor", "initial_backoff_ms", "max_backoff_ms", "total_window_ms"}
	checkAdditionalProperties(obj, allowed, "/observability/status_plans", errors)

	// Ajv applies defaults before validating, so an absent key still gets its
	// default; an explicit value overrides it.
	plans := &StatusPlansConfig{
		BackoffFactor:    DefaultBackoffFactor,
		InitialBackoffMs: DefaultInitialBackoffMs,
		MaxBackoffMs:     DefaultMaxBackoffMs,
		TotalWindowMs:    DefaultStatusPlansTotalWindowMs,
	}

	if value, present := obj["backoff_factor"]; present {
		number, ok := numberAtLeast(value, 1, "/observability/status_plans/backoff_factor", errors)
		if ok {
			plans.BackoffFactor = number
		}
	}
	if value, present := obj["initial_backoff_ms"]; present {
		number, ok := integerAtLeast(value, 1, "/observability/status_plans/initial_backoff_ms", errors)
		if ok {
			plans.InitialBackoffMs = number
		}
	}
	if value, present := obj["max_backoff_ms"]; present {
		number, ok := integerAtLeast(value, 1, "/observability/status_plans/max_backoff_ms", errors)
		if ok {
			plans.MaxBackoffMs = number
		}
	}
	if value, present := obj["total_window_ms"]; present {
		number, ok := integerAtLeast(value, 1, "/observability/status_plans/total_window_ms", errors)
		if ok {
			plans.TotalWindowMs = number
		}
	}

	// Mirror useDefaults writing values back into the document.
	obj["backoff_factor"] = plans.BackoffFactor
	obj["initial_backoff_ms"] = plans.InitialBackoffMs
	obj["max_backoff_ms"] = plans.MaxBackoffMs
	obj["total_window_ms"] = plans.TotalWindowMs
	return plans, nil
}

// validateTelemetry mirrors the inline telemetry object.
func validateTelemetry(raw any, errors *[]ValidationError) (*TelemetryConfig, error) {
	obj, ok := raw.(map[string]any)
	if !ok {
		*errors = append(*errors, typeError("/observability/telemetry", "object"))
		return nil, nil
	}
	checkAdditionalProperties(obj, []string{"max_excerpt_bytes"}, "/observability/telemetry", errors)

	telemetry := &TelemetryConfig{MaxExcerptBytes: DefaultMaxExcerptBytes}
	if value, present := obj["max_excerpt_bytes"]; present {
		number, ok := integerAtLeast(value, 1, "/observability/telemetry/max_excerpt_bytes", errors)
		if ok {
			telemetry.MaxExcerptBytes = number
		}
	}
	obj["max_excerpt_bytes"] = telemetry.MaxExcerptBytes
	return telemetry, nil
}

// validateWake mirrors wakeSchema with its two defaults.
func validateWake(raw any, config *HerdsmanConfig, errors *[]ValidationError) error {
	obj, ok := raw.(map[string]any)
	if !ok {
		*errors = append(*errors, typeError("/wake", "object"))
		return nil
	}
	allowed := []string{"extra_upstream_error_patterns", "filter_upstream_errors"}
	checkAdditionalProperties(obj, allowed, "/wake", errors)

	wake := &WakeConfig{
		ExtraUpstreamErrorPatterns: []string{},
		FilterUpstreamErrors:       DefaultFilterUpstreamErrors,
	}

	if value, present := obj["extra_upstream_error_patterns"]; present {
		patterns, ok := validateStringArray(value, "/wake/extra_upstream_error_patterns", errors)
		if ok {
			wake.ExtraUpstreamErrorPatterns = patterns
		}
	}
	if value, present := obj["filter_upstream_errors"]; present {
		boolean, ok := value.(bool)
		if !ok {
			*errors = append(*errors, typeError("/wake/filter_upstream_errors", "boolean"))
		} else {
			wake.FilterUpstreamErrors = boolean
		}
	}

	obj["extra_upstream_error_patterns"] = wake.ExtraUpstreamErrorPatterns
	obj["filter_upstream_errors"] = wake.FilterUpstreamErrors
	config.Wake = wake
	return nil
}

// validateStringArray mirrors Type.Array(Type.String({minLength:1})): every
// element must be a string of at least one rune.
func validateStringArray(raw any, path string, errors *[]ValidationError) ([]string, bool) {
	items, ok := raw.([]any)
	if !ok {
		*errors = append(*errors, typeError(path, "array"))
		return nil, false
	}
	out := make([]string, 0, len(items))
	valid := true
	for index, item := range items {
		value, ok := item.(string)
		if !ok {
			*errors = append(*errors, typeError(fmt.Sprintf("%s/%d", path, index), "string"))
			valid = false
			continue
		}
		if len([]rune(value)) < 1 {
			*errors = append(*errors, ValidationError{
				InstancePath: fmt.Sprintf("%s/%d", path, index),
				Keyword:      keywordMinLength,
				Message:      "must NOT have fewer than 1 characters",
			})
			valid = false
			continue
		}
		out = append(out, value)
	}
	if !valid {
		return nil, false
	}
	return out, true
}

// checkAdditionalProperties mirrors Ajv's additionalProperties error, emitted
// once per offending key with the key name appended to instancePath.
func checkAdditionalProperties(obj map[string]any, allowed []string, prefix string, errors *[]ValidationError) {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		allowedSet[key] = struct{}{}
	}
	keys := make([]string, 0, len(obj))
	for key := range obj {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, ok := allowedSet[key]; ok {
			continue
		}
		*errors = append(*errors, ValidationError{
			InstancePath: joinPath(prefix, key),
			Keyword:      keywordAdditionalProperties,
			Message:      "must NOT have additional properties",
		})
	}
}

func nonEmptyString(raw any, path string, errors *[]ValidationError) (string, bool) {
	value, ok := raw.(string)
	if !ok {
		*errors = append(*errors, typeError(path, "string"))
		return "", false
	}
	if len([]rune(value)) < 1 {
		*errors = append(*errors, ValidationError{
			InstancePath: path,
			Keyword:      keywordMinLength,
			Message:      "must NOT have fewer than 1 characters",
		})
		return "", false
	}
	return value, true
}

// numberAtLeast mirrors Type.Number({minimum}) — TypeBox has no integer check.
func numberAtLeast(raw any, minimum float64, path string, errors *[]ValidationError) (float64, bool) {
	number, ok := asNumber(raw)
	if !ok {
		*errors = append(*errors, typeError(path, "number"))
		return 0, false
	}
	if number < minimum {
		*errors = append(*errors, ValidationError{
			InstancePath: path,
			Keyword:      keywordMinimum,
			Message:      fmt.Sprintf("must be >= %g", minimum),
		})
		return 0, false
	}
	return number, true
}

// integerAtLeast mirrors Type.Integer({minimum}).
func integerAtLeast(raw any, minimum float64, path string, errors *[]ValidationError) (float64, bool) {
	number, ok := asNumber(raw)
	if !ok {
		*errors = append(*errors, typeError(path, "number"))
		return 0, false
	}
	if number != float64(int64(number)) {
		*errors = append(*errors, typeError(path, "integer"))
		return 0, false
	}
	if number < minimum {
		*errors = append(*errors, ValidationError{
			InstancePath: path,
			Keyword:      keywordMinimum,
			Message:      fmt.Sprintf("must be >= %g", minimum),
		})
		return 0, false
	}
	return number, true
}

func asNumber(raw any) (float64, bool) {
	switch number := raw.(type) {
	case float64:
		return number, true
	case float32:
		return float64(number), true
	case int:
		return float64(number), true
	case int32:
		return float64(number), true
	case int64:
		return float64(number), true
	case uint64:
		return float64(number), true
	default:
		return 0, false
	}
}

func typeError(path, want string) ValidationError {
	return ValidationError{
		InstancePath: path,
		Keyword:      keywordType,
		Message:      "must be " + want,
	}
}

// joinPath mirrors Ajv's JSON-pointer path building.
func joinPath(prefix, key string) string {
	return prefix + "/" + strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
}

// sortErrors makes output deterministic; Ajv's order follows schema traversal,
// which the ports above already approximate.
func sortErrors(errors []ValidationError) {
	sort.SliceStable(errors, func(i, j int) bool {
		if errors[i].InstancePath != errors[j].InstancePath {
			return errors[i].InstancePath < errors[j].InstancePath
		}
		return errors[i].Keyword < errors[j].Keyword
	})
}

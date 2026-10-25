package config

import (
	"errors"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// This file ports src/config/load.ts.
//
// The TS loader reads the file, parses it with the `yaml` package (which
// reports *all* parse errors and refuses duplicate/malformed nodes), and then
// hands document.toJSON() to parseHerdsmanConfig. Both failure modes surface as
// `{ ok: false, errors: [{ message, path }] }` — the loader never throws.

// ConfigLoadError mirrors the TS type of the same name.
type ConfigLoadError struct {
	Message string
	Path    string
}

// ConfigLoadResult mirrors `ConfigLoadResult` (ok discriminates the union).
type ConfigLoadResult struct {
	OK     bool
	Value  HerdsmanConfig
	Errors []ConfigLoadError
}

// LoadHerdsmanConfig mirrors loadHerdsmanConfig(path).
func LoadHerdsmanConfig(path string) ConfigLoadResult {
	source, err := os.ReadFile(path)
	if err != nil {
		// The TS readFileSync would throw here; the Go port keeps the loader's
		// "returns errors, never throws" contract and reports the read failure
		// in the same shape.
		return ConfigLoadResult{Errors: []ConfigLoadError{{Message: err.Error(), Path: path}}}
	}

	document, decodeErrs := parseYAMLDocument(source)
	if len(decodeErrs) > 0 {
		loadErrors := make([]ConfigLoadError, 0, len(decodeErrs))
		for _, parseErr := range decodeErrs {
			loadErrors = append(loadErrors, ConfigLoadError{Message: parseErr.Error(), Path: path})
		}
		return ConfigLoadResult{Errors: loadErrors}
	}

	result := ParseHerdsmanConfig(document)
	if result.OK {
		return ConfigLoadResult{OK: true, Value: result.Value}
	}

	loadErrors := make([]ConfigLoadError, 0, len(result.Errors))
	for _, validationErr := range result.Errors {
		message := validationErr.Message
		if message == "" {
			message = fmt.Sprintf("%s failed %s", validationErr.InstancePath, validationErr.Keyword)
		}
		loadErrors = append(loadErrors, ConfigLoadError{Message: message, Path: path})
	}
	return ConfigLoadResult{Errors: loadErrors}
}

// parseYAMLDocument decodes YAML into a generic document and returns every
// parse error, matching `parseDocument(source).errors`.
//
// yaml.Node decoding reports a single syntax error at a time, so the source is
// decoded once; a failure is reported as one error (the TS yaml package may
// report several, all of which the loader maps identically to `{message, path}`).
func parseYAMLDocument(source []byte) (map[string]any, []error) {
	var root yaml.Node
	if err := yaml.Unmarshal(source, &root); err != nil {
		var typeErr *yaml.TypeError
		if errors.As(err, &typeErr) {
			errs := make([]error, 0, len(typeErr.Errors))
			for _, message := range typeErr.Errors {
				errs = append(errs, errors.New(message))
			}
			return nil, errs
		}
		return nil, []error{err}
	}

	// An empty document (or comments only) parses to a zero Node; the TS loader
	// hands `undefined`/`null` to the validator, which rejects a non-object.
	if root.Kind == 0 || len(root.Content) == 0 {
		return map[string]any{}, nil
	}

	var document any
	if err := root.Content[0].Decode(&document); err != nil {
		return nil, []error{err}
	}
	normalized, ok := normalizeYAMLValue(document).(map[string]any)
	if !ok {
		return map[string]any{"__non_object_document": document}, nil
	}
	return normalized, nil
}

// normalizeYAMLValue converts yaml.v3's decoded Go values into the same JSON-ish
// shapes JSON.parse/toJSON would produce: map[string]any for objects, []any for
// arrays, and int for integral numbers (Ajv sees JS numbers).
func normalizeYAMLValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = normalizeYAMLValue(item)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[fmt.Sprint(key)] = normalizeYAMLValue(item)
		}
		return out
	case []any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			out = append(out, normalizeYAMLValue(item))
		}
		return out
	default:
		return value
	}
}

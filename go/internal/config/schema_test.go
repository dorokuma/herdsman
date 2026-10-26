package config

import "testing"

func TestAcceptsMinimalObservabilityConfigWithRuntimePaths(t *testing.T) {
	result := ParseHerdsmanConfig(map[string]any{
		"observability": map[string]any{
			"telemetry": map[string]any{"max_excerpt_bytes": 2048},
		},
		"runtime": map[string]any{
			"db_path":     "data/state.db",
			"log_path":    "logs/herdsman.log",
			"pid_path":    "herdsman.pid",
			"socket_path": "herdsman.sock",
		},
	})
	if !result.OK {
		t.Fatalf("expected ok, got errors: %+v", result.Errors)
	}
	if result.Value.Runtime == nil || result.Value.Runtime.DBPath == nil || *result.Value.Runtime.DBPath != "data/state.db" {
		t.Fatalf("runtime.db_path not carried through: %+v", result.Value.Runtime)
	}
	if result.Value.Observability == nil || result.Value.Observability.Telemetry == nil {
		t.Fatal("observability.telemetry missing")
	}
	if got := result.Value.Observability.Telemetry.MaxExcerptBytes; got != 2048 {
		t.Fatalf("max_excerpt_bytes = %v, want 2048", got)
	}
}

func TestDefaultsTelemetryExcerptLimit(t *testing.T) {
	result := ParseHerdsmanConfig(map[string]any{
		"observability": map[string]any{"telemetry": map[string]any{}},
	})
	if !result.OK {
		t.Fatalf("expected ok, got errors: %+v", result.Errors)
	}
	telemetry := result.Value.Observability.Telemetry
	if telemetry == nil || telemetry.MaxExcerptBytes != 4096 {
		t.Fatalf("telemetry = %+v, want max_excerpt_bytes 4096", telemetry)
	}
}

func TestDefaultsStatusPlans(t *testing.T) {
	result := ParseHerdsmanConfig(map[string]any{
		"observability": map[string]any{"status_plans": map[string]any{}},
	})
	if !result.OK {
		t.Fatalf("expected ok, got errors: %+v", result.Errors)
	}
	plans := result.Value.Observability.StatusPlans
	if plans == nil {
		t.Fatal("status_plans missing")
	}
	if plans.BackoffFactor != 2 {
		t.Errorf("backoff_factor = %v, want 2", plans.BackoffFactor)
	}
	if plans.InitialBackoffMs != 5000 {
		t.Errorf("initial_backoff_ms = %v, want 5000", plans.InitialBackoffMs)
	}
	if plans.MaxBackoffMs != 60000 {
		t.Errorf("max_backoff_ms = %v, want 60000", plans.MaxBackoffMs)
	}
	if plans.TotalWindowMs != 15*60*1000 {
		t.Errorf("total_window_ms = %v, want %d", plans.TotalWindowMs, 15*60*1000)
	}
}

func TestDefaultsWakeFilterSwitch(t *testing.T) {
	result := ParseHerdsmanConfig(map[string]any{"wake": map[string]any{}})
	if !result.OK {
		t.Fatalf("expected ok, got errors: %+v", result.Errors)
	}
	wake := result.Value.Wake
	if wake == nil {
		t.Fatal("wake missing")
	}
	if !wake.FilterUpstreamErrors {
		t.Error("filter_upstream_errors should default to true")
	}
	if len(wake.ExtraUpstreamErrorPatterns) != 0 {
		t.Errorf("extra_upstream_error_patterns = %v, want []", wake.ExtraUpstreamErrorPatterns)
	}
}

func TestAcceptsExplicitWakeSwitchAndPatterns(t *testing.T) {
	result := ParseHerdsmanConfig(map[string]any{
		"wake": map[string]any{
			"extra_upstream_error_patterns": []any{"checkpoint-stall", `/foo\s+bar/i`},
			"filter_upstream_errors":        false,
		},
	})
	if !result.OK {
		t.Fatalf("expected ok, got errors: %+v", result.Errors)
	}
	wake := result.Value.Wake
	if wake.FilterUpstreamErrors {
		t.Error("filter_upstream_errors should be false")
	}
	want := []string{"checkpoint-stall", `/foo\s+bar/i`}
	if len(wake.ExtraUpstreamErrorPatterns) != len(want) {
		t.Fatalf("patterns = %v, want %v", wake.ExtraUpstreamErrorPatterns, want)
	}
	for i, pattern := range want {
		if wake.ExtraUpstreamErrorPatterns[i] != pattern {
			t.Errorf("patterns[%d] = %q, want %q", i, wake.ExtraUpstreamErrorPatterns[i], pattern)
		}
	}
}

func TestRejectsUnknownWakeKeysAndEmptyPatterns(t *testing.T) {
	if ParseHerdsmanConfig(map[string]any{"wake": map[string]any{"extra": 1}}).OK {
		t.Error("unknown wake key must be rejected")
	}
	if ParseHerdsmanConfig(map[string]any{
		"wake": map[string]any{"extra_upstream_error_patterns": []any{""}},
	}).OK {
		t.Error("empty extra pattern must be rejected")
	}
	if ParseHerdsmanConfig(map[string]any{
		"wake": map[string]any{"filter_upstream_errors": "yes"},
	}).OK {
		t.Error("filter_upstream_errors must be a boolean")
	}
}

func TestRejectsUnknownTopLevelConfigSurfaces(t *testing.T) {
	configs := []map[string]any{
		{"old_agents": map[string]any{"enabled": true}},
		{"providers": map[string]any{"example": map[string]any{}}},
		{"orchestration": map[string]any{"queue": map[string]any{}}},
	}
	for _, config := range configs {
		result := ParseHerdsmanConfig(config)
		if result.OK {
			t.Fatalf("config %v must be rejected", config)
		}
		found := false
		for _, validationErr := range result.Errors {
			if validationErr.Keyword == "additionalProperties" {
				found = true
			}
		}
		if !found {
			t.Errorf("config %v: no additionalProperties error in %+v", config, result.Errors)
		}
	}
}

func TestRejectsUnknownRuntimeAndObservabilityKeys(t *testing.T) {
	if ParseHerdsmanConfig(map[string]any{"runtime": map[string]any{"extra": "nope"}}).OK {
		t.Error("unknown runtime key must be rejected")
	}
	if ParseHerdsmanConfig(map[string]any{
		"observability": map[string]any{"retention": map[string]any{"days": 7}},
	}).OK {
		t.Error("unknown observability key must be rejected")
	}
}

func TestRejectsNonObjectAndBadValues(t *testing.T) {
	if ParseHerdsmanConfig(nil).OK {
		t.Error("nil document must be rejected")
	}
	if ParseHerdsmanConfig(map[string]any{"runtime": "nope"}).OK {
		t.Error("runtime must be an object")
	}
	if ParseHerdsmanConfig(map[string]any{"runtime": map[string]any{"db_path": ""}}).OK {
		t.Error("empty db_path violates minLength 1")
	}
	if ParseHerdsmanConfig(map[string]any{
		"observability": map[string]any{"telemetry": map[string]any{"max_excerpt_bytes": 0}},
	}).OK {
		t.Error("max_excerpt_bytes 0 violates minimum 1")
	}
	if ParseHerdsmanConfig(map[string]any{
		"observability": map[string]any{"telemetry": map[string]any{"max_excerpt_bytes": 1.5}},
	}).OK {
		t.Error("max_excerpt_bytes must be an integer")
	}
	if ParseHerdsmanConfig(map[string]any{
		"observability": map[string]any{"status_plans": map[string]any{"backoff_factor": 0.5}},
	}).OK {
		t.Error("backoff_factor below 1 must be rejected")
	}
}

func TestDefaultsAreWrittenBackIntoDocument(t *testing.T) {
	// Ajv's useDefaults mutates the input; the port mirrors that because the
	// loader hands the same document to the caller's downstream consumers.
	doc := map[string]any{"wake": map[string]any{}}
	if result := ParseHerdsmanConfig(doc); !result.OK {
		t.Fatalf("expected ok: %+v", result.Errors)
	}
	wake := doc["wake"].(map[string]any)
	if wake["filter_upstream_errors"] != true {
		t.Errorf("document wake.filter_upstream_errors = %v, want true", wake["filter_upstream_errors"])
	}
	if patterns, ok := wake["extra_upstream_error_patterns"].([]string); !ok || len(patterns) != 0 {
		t.Errorf("document wake.extra_upstream_error_patterns = %v, want []", wake["extra_upstream_error_patterns"])
	}
}

func TestAdditionalPropertiesErrorPathIsAjvShaped(t *testing.T) {
	result := ParseHerdsmanConfig(map[string]any{"runtime": map[string]any{"extra": "nope"}})
	if result.OK {
		t.Fatal("expected failure")
	}
	if len(result.Errors) == 0 {
		t.Fatal("expected at least one error")
	}
	if got := result.Errors[0].InstancePath; got != "/runtime/extra" {
		t.Errorf("instancePath = %q, want %q", got, "/runtime/extra")
	}
}

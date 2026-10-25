package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTempConfig(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "herdsman.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadsValidObservabilityRuntimeYAML(t *testing.T) {
	path := writeTempConfig(t, `
runtime:
  db_path: data/state.db
  socket_path: herdsman.sock
  pid_path: herdsman.pid
  log_path: logs/herdsman.log
observability:
  telemetry:
    max_excerpt_bytes: 2048
`)
	result := LoadHerdsmanConfig(path)
	if !result.OK {
		t.Fatalf("expected ok, got errors: %+v", result.Errors)
	}
	if result.Value.Runtime == nil {
		t.Fatal("runtime missing")
	}
	if got := deref(result.Value.Runtime.DBPath); got != "data/state.db" {
		t.Errorf("db_path = %q, want data/state.db", got)
	}
	if got := deref(result.Value.Runtime.SocketPath); got != "herdsman.sock" {
		t.Errorf("socket_path = %q, want herdsman.sock", got)
	}
	if telem := result.Value.Observability.Telemetry; telem == nil || telem.MaxExcerptBytes != 2048 {
		t.Errorf("telemetry = %+v, want max_excerpt_bytes 2048", telem)
	}
}

func TestLoadsWakeFilterSection(t *testing.T) {
	path := writeTempConfig(t, `
runtime:
  socket_path: herdsman.sock
wake:
  filter_upstream_errors: false
  extra_upstream_error_patterns:
    - checkpoint-stall
`)
	result := LoadHerdsmanConfig(path)
	if !result.OK {
		t.Fatalf("expected ok, got errors: %+v", result.Errors)
	}
	wake := result.Value.Wake
	if wake == nil {
		t.Fatal("wake missing")
	}
	if wake.FilterUpstreamErrors {
		t.Error("filter_upstream_errors should be false")
	}
	if len(wake.ExtraUpstreamErrorPatterns) != 1 || wake.ExtraUpstreamErrorPatterns[0] != "checkpoint-stall" {
		t.Errorf("patterns = %v, want [checkpoint-stall]", wake.ExtraUpstreamErrorPatterns)
	}
}

func TestDefaultsWakeFilterWhenSectionIsEmpty(t *testing.T) {
	result := LoadHerdsmanConfig(writeTempConfig(t, "wake: {}\n"))
	if !result.OK {
		t.Fatalf("expected ok, got errors: %+v", result.Errors)
	}
	wake := result.Value.Wake
	if !wake.FilterUpstreamErrors {
		t.Error("filter_upstream_errors should default to true")
	}
	if len(wake.ExtraUpstreamErrorPatterns) != 0 {
		t.Errorf("patterns = %v, want []", wake.ExtraUpstreamErrorPatterns)
	}
}

func TestLoadsStatusPlansDefaultsFromYAML(t *testing.T) {
	result := LoadHerdsmanConfig(writeTempConfig(t, "observability:\n  status_plans: {}\n"))
	if !result.OK {
		t.Fatalf("expected ok, got errors: %+v", result.Errors)
	}
	plans := result.Value.Observability.StatusPlans
	if plans == nil || plans.BackoffFactor != 2 || plans.InitialBackoffMs != 5000 {
		t.Fatalf("status_plans = %+v, want the documented defaults", plans)
	}
}

func TestReturnsYAMLErrorsWithoutThrowing(t *testing.T) {
	result := LoadHerdsmanConfig(writeTempConfig(t, "runtime: ["))
	if result.OK {
		t.Fatal("malformed YAML must be rejected")
	}
	if len(result.Errors) == 0 {
		t.Fatal("expected at least one load error")
	}
	if result.Errors[0].Path == "" {
		t.Error("load error should carry the config path")
	}
}

func TestReportsValidationErrorsWithPath(t *testing.T) {
	result := LoadHerdsmanConfig(writeTempConfig(t, "providers:\n  example: {}\n"))
	if result.OK {
		t.Fatal("unknown top-level key must be rejected")
	}
	for _, loadErr := range result.Errors {
		if loadErr.Path == "" {
			t.Errorf("load error missing path: %+v", loadErr)
		}
		if strings.TrimSpace(loadErr.Message) == "" {
			t.Errorf("load error missing message: %+v", loadErr)
		}
	}
}

func TestMissingFileIsReportedAsLoadError(t *testing.T) {
	result := LoadHerdsmanConfig(filepath.Join(t.TempDir(), "absent.yaml"))
	if result.OK {
		t.Fatal("a missing config file must not report ok")
	}
	if len(result.Errors) == 0 {
		t.Fatal("expected a read error")
	}
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

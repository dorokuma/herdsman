package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func tempHome(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func writeValidConfig(t *testing.T, homeDir, extraYAML string) {
	t.Helper()
	writeFile(t, filepath.Join(homeDir, "config.yaml"), extraYAML+`observability:
  telemetry: {}
`,
	)
}

func strPtr(value string) *string { return &value }

func TestUsesDefaultHomeWhenHerdsmanHomeAbsent(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	got, err := GetHerdsmanHome(map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".herdsman")
	if got != want {
		t.Fatalf("GetHerdsmanHome = %q, want %q", got, want)
	}
}

func TestUsesExplicitHerdsmanHome(t *testing.T) {
	got, err := GetHerdsmanHome(map[string]string{"HERDSMAN_HOME": "/tmp/herdsman-dev"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "/tmp/herdsman-dev" {
		t.Fatalf("GetHerdsmanHome = %q", got)
	}
	// A blank HERDSMAN_HOME must not be taken as an empty home.
	blank, err := GetHerdsmanHome(map[string]string{"HERDSMAN_HOME": "   "})
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	if blank != filepath.Join(home, ".herdsman") {
		t.Fatalf("blank HERDSMAN_HOME resolved to %q", blank)
	}
}

func TestResolvesDefaultRuntimePathsUnderHerdsmanHome(t *testing.T) {
	homeDir := tempHome(t)

	runtime, err := ResolveRuntime(&RuntimeResolveOptions{Environment: map[string]string{"HERDSMAN_HOME": homeDir}})
	if err != nil {
		t.Fatalf("ResolveRuntime: %v", err)
	}
	want := RuntimePaths{
		ConfigPath:        filepath.Join(homeDir, "config.yaml"),
		DBPath:            filepath.Join(homeDir, "state.db"),
		EnvPath:           filepath.Join(homeDir, ".env"),
		HomeDir:           homeDir,
		LogPath:           filepath.Join(homeDir, "logs", "herdsman.log"),
		PIDPath:           filepath.Join(homeDir, "herdsman.pid"),
		PiSessionDir:      filepath.Join(homeDir, "pi-sessions"),
		RuntimeRecordPath: filepath.Join(homeDir, "runtime.json"),
		SocketPath:        filepath.Join(homeDir, "herdsman.sock"),
	}
	if runtime.Paths != want {
		t.Fatalf("paths = %+v, want %+v", runtime.Paths, want)
	}
	if runtime.HomeDir != homeDir || runtime.ConfigPath != want.ConfigPath {
		t.Fatalf("resolution = %+v", runtime)
	}
	if runtime.Config != nil || runtime.ConfigErrors != nil {
		t.Fatalf("absent config file must leave both fields empty: %+v", runtime)
	}
}

func TestResolvesRelativeRuntimeConfigPathsFromHome(t *testing.T) {
	homeDir := tempHome(t)
	writeValidConfig(t, homeDir, `runtime:
  db_path: data/state.sqlite
  socket_path: sockets/dev.sock
  pid_path: run/dev.pid
  log_path: logs/dev.log
`)

	runtime, err := ResolveRuntime(&RuntimeResolveOptions{Environment: map[string]string{"HERDSMAN_HOME": homeDir}})
	if err != nil {
		t.Fatalf("ResolveRuntime: %v", err)
	}
	for name, got := range map[string]string{
		"dbPath":     runtime.Paths.DBPath,
		"socketPath": runtime.Paths.SocketPath,
		"pidPath":    runtime.Paths.PIDPath,
		"logPath":    runtime.Paths.LogPath,
	} {
		if !filepath.IsAbs(got) || !strings.HasPrefix(got, homeDir) {
			t.Fatalf("%s = %q, want a path under %q", name, got, homeDir)
		}
	}
	if runtime.Paths.DBPath != filepath.Join(homeDir, "data", "state.sqlite") ||
		runtime.Paths.SocketPath != filepath.Join(homeDir, "sockets", "dev.sock") ||
		runtime.Paths.PIDPath != filepath.Join(homeDir, "run", "dev.pid") ||
		runtime.Paths.LogPath != filepath.Join(homeDir, "logs", "dev.log") {
		t.Fatalf("paths = %+v", runtime.Paths)
	}
	if runtime.Config == nil {
		t.Fatal("a valid config must be returned")
	}
}

func TestKeepsAbsoluteRuntimeConfigPaths(t *testing.T) {
	homeDir := tempHome(t)
	writeValidConfig(t, homeDir, `runtime:
  db_path: /var/tmp/herdsman/state.sqlite
  socket_path: /var/tmp/herdsman/herdsman.sock
  pid_path: /var/tmp/herdsman/herdsman.pid
  log_path: /var/tmp/herdsman/herdsman.log
`)

	runtime, err := ResolveRuntime(&RuntimeResolveOptions{Environment: map[string]string{"HERDSMAN_HOME": homeDir}})
	if err != nil {
		t.Fatalf("ResolveRuntime: %v", err)
	}
	if runtime.Paths.DBPath != "/var/tmp/herdsman/state.sqlite" ||
		runtime.Paths.SocketPath != "/var/tmp/herdsman/herdsman.sock" ||
		runtime.Paths.PIDPath != "/var/tmp/herdsman/herdsman.pid" ||
		runtime.Paths.LogPath != "/var/tmp/herdsman/herdsman.log" {
		t.Fatalf("paths = %+v", runtime.Paths)
	}
}

func TestLoadsDotEnvOverShellValuesIncludingHerdsmanVariables(t *testing.T) {
	homeDir := tempHome(t)
	envPath := filepath.Join(homeDir, "dotenv-test")
	writeFile(t, envPath, `EXAMPLE_SERVICE_TOKEN=file-token
OPENAI_API_KEY="file-key"
HERDSMAN_HOME=/tmp/ignored
HERDSMAN_INTERNAL_SOCKET_PATH=/tmp/ignored.sock
`)

	environment := LoadHerdsmanDotEnv(map[string]string{
		"EXISTING":       "kept",
		"OPENAI_API_KEY": "shell-key",
		"HERDSMAN_HOME":  homeDir,
	}, envPath)

	for key, want := range map[string]string{
		"EXAMPLE_SERVICE_TOKEN":         "file-token",
		"OPENAI_API_KEY":                "file-key",
		"EXISTING":                      "kept",
		"HERDSMAN_HOME":                 "/tmp/ignored",
		"HERDSMAN_INTERNAL_SOCKET_PATH": "/tmp/ignored.sock",
	} {
		if got := environment[key]; got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestDotEnvSkipsMalformedLinesAndHandlesCRLFAndQuotes(t *testing.T) {
	homeDir := tempHome(t)
	envPath := filepath.Join(homeDir, ".env")
	writeFile(t, envPath, "# comment\r\n\r\nGOOD=1\r\nNOT_A_KEY=2\r\n SPACED = 'single quoted' \r\nEMPTY=\r\nNOEQUALS\r\n=novalue\r\n1BAD=x\r\nUPPER_9=a=b\r\n")

	environment := LoadHerdsmanDotEnv(map[string]string{}, envPath)
	// NOT_A_KEY is a *valid* variable name and is loaded like the TS regex allows.
	for key, want := range map[string]string{
		"GOOD":      "1",
		"NOT_A_KEY": "2",
		"SPACED":    "single quoted",
		"EMPTY":     "",
		"UPPER_9":   "a=b",
	} {
		if got := environment[key]; got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
	for _, skipped := range []string{"NOEQUALS", "1BAD", ""} {
		if _, ok := environment[skipped]; ok {
			t.Fatalf("%q must not be loaded from the .env file", skipped)
		}
	}
}

func TestDotEnvMissingFileIsUnchangedCopy(t *testing.T) {
	base := map[string]string{"A": "1"}
	environment := LoadHerdsmanDotEnv(base, filepath.Join(t.TempDir(), "absent.env"))
	if environment["A"] != "1" || len(environment) != 1 {
		t.Fatalf("environment = %v", environment)
	}
	// The copy is independent of the input map.
	environment["B"] = "2"
	if _, ok := base["B"]; ok {
		t.Fatal("LoadHerdsmanDotEnv must not mutate the base environment")
	}
}

func TestThrowsOnInvalidConfigUnlessAllowed(t *testing.T) {
	homeDir := tempHome(t)
	writeFile(t, filepath.Join(homeDir, "config.yaml"), "runtime: [")

	_, err := ResolveRuntime(&RuntimeResolveOptions{Environment: map[string]string{"HERDSMAN_HOME": homeDir}})
	if err == nil || !strings.HasPrefix(err.Error(), "Invalid Herdsman config") {
		t.Fatalf("error = %v, want an Invalid Herdsman config failure", err)
	}

	runtime, err := ResolveRuntime(&RuntimeResolveOptions{
		AllowInvalidConfig: true,
		Environment:        map[string]string{"HERDSMAN_HOME": homeDir},
	})
	if err != nil {
		t.Fatalf("allowInvalidConfig must not fail: %v", err)
	}
	if len(runtime.ConfigErrors) == 0 {
		t.Fatal("invalid config must be reported in ConfigErrors")
	}
	if runtime.Paths.DBPath != filepath.Join(homeDir, "state.db") {
		t.Fatalf("dbPath = %q, want the default under home", runtime.Paths.DBPath)
	}
	if runtime.Config != nil {
		t.Fatal("an invalid config must not be returned as a value")
	}
}

func TestFallsBackToRuntimeRecordPathsForManagementCommands(t *testing.T) {
	homeDir := tempHome(t)
	writeFile(t, filepath.Join(homeDir, "config.yaml"), "runtime: [")
	record := `{"dbPath":"` + filepath.Join(homeDir, "last-state.db") + `","homeDir":"` + homeDir +
		`","logPath":"` + filepath.Join(homeDir, "last.log") + `","pid":1234,"pidPath":"` +
		filepath.Join(homeDir, "last.pid") + `","socketPath":"` + filepath.Join(homeDir, "last.sock") +
		`","startedAt":"2026-06-29T00:00:00.000Z","version":1}`
	writeFile(t, filepath.Join(homeDir, "runtime.json"), record)

	runtime, err := ResolveRuntime(&RuntimeResolveOptions{
		AllowInvalidConfig: true,
		Environment:        map[string]string{"HERDSMAN_HOME": homeDir},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(runtime.ConfigErrors) == 0 {
		t.Fatal("expected the invalid config to be reported")
	}

	paths, err := RuntimePathsFromRecordOrDefault(&RuntimePathsFromRecordOrDefaultOptions{
		Environment:  runtime.Environment,
		RecordReader: testRecordReader,
	})
	if err != nil {
		t.Fatal(err)
	}
	if paths.DBPath != filepath.Join(homeDir, "last-state.db") ||
		paths.SocketPath != filepath.Join(homeDir, "last.sock") ||
		paths.PIDPath != filepath.Join(homeDir, "last.pid") ||
		paths.LogPath != filepath.Join(homeDir, "last.log") {
		t.Fatalf("paths = %+v", paths)
	}
}

func TestFallsBackToHomeDefaultsWhenRuntimeRecordMissing(t *testing.T) {
	homeDir := tempHome(t)
	writeFile(t, filepath.Join(homeDir, "config.yaml"), "runtime: [")

	runtime, err := ResolveRuntime(&RuntimeResolveOptions{
		AllowInvalidConfig: true,
		Environment:        map[string]string{"HERDSMAN_HOME": homeDir},
	})
	if err != nil {
		t.Fatal(err)
	}
	paths, err := RuntimePathsFromRecordOrDefault(&RuntimePathsFromRecordOrDefaultOptions{
		Environment:  runtime.Environment,
		RecordReader: testRecordReader,
	})
	if err != nil {
		t.Fatal(err)
	}
	if paths.DBPath != filepath.Join(homeDir, "state.db") ||
		paths.SocketPath != filepath.Join(homeDir, "herdsman.sock") ||
		paths.PIDPath != filepath.Join(homeDir, "herdsman.pid") ||
		paths.LogPath != filepath.Join(homeDir, "logs", "herdsman.log") {
		t.Fatalf("paths = %+v", paths)
	}
}

func TestResolvesExplicitRuntimePathValues(t *testing.T) {
	if got := ResolveRuntimePath("/home/herdsman", "state.db"); got != "/home/herdsman/state.db" {
		t.Fatalf("relative = %q", got)
	}
	if got := ResolveRuntimePath("/home/herdsman", "/tmp/state.db"); got != "/tmp/state.db" {
		t.Fatalf("absolute = %q", got)
	}
}

func TestResolvesPathsFromAlreadyLoadedConfig(t *testing.T) {
	paths, err := ResolveRuntimePaths(&ResolveRuntimePathsOptions{
		Config: &HerdsmanConfig{Runtime: &RuntimePathsConfig{DBPath: strPtr("data/state.db")}},
		Environment: map[string]string{
			"HERDSMAN_HOME": "/tmp/herdsman-home",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if paths.DBPath != "/tmp/herdsman-home/data/state.db" {
		t.Fatalf("dbPath = %q", paths.DBPath)
	}
	// Unset config fields keep their defaults.
	if paths.SocketPath != "/tmp/herdsman-home/herdsman.sock" {
		t.Fatalf("socketPath = %q", paths.SocketPath)
	}
}

func TestResolveStatusPlansConfigDefaults(t *testing.T) {
	got := ResolveStatusPlansConfig(nil, map[string]string{})
	if got != DefaultStatusPlanConfig {
		t.Fatalf("resolved = %+v, want %+v", got, DefaultStatusPlanConfig)
	}
	if DefaultStatusPlanConfig.BackoffFactor != 2 ||
		DefaultStatusPlanConfig.InitialBackoffMs != 5000 ||
		DefaultStatusPlanConfig.MaxBackoffMs != 60000 ||
		DefaultStatusPlanConfig.TotalWindowMs != 15*60*1000 {
		t.Fatalf("defaults = %+v", DefaultStatusPlanConfig)
	}
}

func TestResolveStatusPlansConfigFromParsedConfig(t *testing.T) {
	cfg := &HerdsmanConfig{Observability: &ObservabilityConfig{
		StatusPlans: &StatusPlansConfig{
			BackoffFactor:    1.5,
			InitialBackoffMs: 3000,
			MaxBackoffMs:     30000,
			TotalWindowMs:    60000,
		},
	}}
	got := ResolveStatusPlansConfig(cfg, map[string]string{})
	want := StatusPlansConfig{BackoffFactor: 1.5, InitialBackoffMs: 3000, MaxBackoffMs: 30000, TotalWindowMs: 60000}
	if got != want {
		t.Fatalf("resolved = %+v, want %+v", got, want)
	}
}

func TestResolveStatusPlansConfigEnvTakesPrecedence(t *testing.T) {
	cfg := &HerdsmanConfig{Observability: &ObservabilityConfig{
		StatusPlans: &StatusPlansConfig{
			BackoffFactor:    1.5,
			InitialBackoffMs: 3000,
			MaxBackoffMs:     30000,
			TotalWindowMs:    60000,
		},
	}}
	got := ResolveStatusPlansConfig(cfg, map[string]string{
		"HERDSMAN_STATUS_PLAN_BACKOFF_FACTOR":     "2.5",
		"HERDSMAN_STATUS_PLAN_INITIAL_BACKOFF_MS": "2000",
		"HERDSMAN_STATUS_PLAN_MAX_BACKOFF_MS":     "15000",
		"HERDSMAN_STATUS_PLAN_TOTAL_WINDOW_MS":    "120000",
	})
	want := StatusPlansConfig{BackoffFactor: 2.5, InitialBackoffMs: 2000, MaxBackoffMs: 15000, TotalWindowMs: 120000}
	if got != want {
		t.Fatalf("resolved = %+v, want %+v", got, want)
	}
}

func TestResolveStatusPlansConfigIgnoresUnusableEnvValues(t *testing.T) {
	cfg := &HerdsmanConfig{Observability: &ObservabilityConfig{
		StatusPlans: &StatusPlansConfig{
			BackoffFactor:    1.5,
			InitialBackoffMs: 3000,
			MaxBackoffMs:     30000,
			TotalWindowMs:    60000,
		},
	}}
	// A factor below 1, non-numeric/zero windows: all fall back to config.
	got := ResolveStatusPlansConfig(cfg, map[string]string{
		"HERDSMAN_STATUS_PLAN_BACKOFF_FACTOR":     "0.5",
		"HERDSMAN_STATUS_PLAN_INITIAL_BACKOFF_MS": "not-a-number",
		"HERDSMAN_STATUS_PLAN_MAX_BACKOFF_MS":     "0",
		"HERDSMAN_STATUS_PLAN_TOTAL_WINDOW_MS":    " ",
	})
	want := StatusPlansConfig{BackoffFactor: 1.5, InitialBackoffMs: 3000, MaxBackoffMs: 30000, TotalWindowMs: 60000}
	if got != want {
		t.Fatalf("resolved = %+v, want %+v", got, want)
	}

	// Without config, unusable env values fall back to the built-in defaults.
	defaults := ResolveStatusPlansConfig(nil, map[string]string{
		"HERDSMAN_STATUS_PLAN_BACKOFF_FACTOR": "0",
	})
	if defaults != DefaultStatusPlanConfig {
		t.Fatalf("resolved = %+v, want the defaults", defaults)
	}
}

func TestResolveStatusPlansConfigAcceptsBoundaryFactor(t *testing.T) {
	got := ResolveStatusPlansConfig(nil, map[string]string{
		"HERDSMAN_STATUS_PLAN_BACKOFF_FACTOR": "1",
	})
	if got.BackoffFactor != 1 {
		t.Fatalf("backoff factor = %v, want the >=1 boundary accepted", got.BackoffFactor)
	}
}

// testRecordReader is the runtime-record reader seam used by the fallback tests.
// Production wires the daemon package's ReadDaemonRuntimeRecord here; this stub
// validates the same version/required-field contract without importing the daemon
// package into config.
func testRecordReader(path string) (RuntimeRecordPaths, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return RuntimeRecordPaths{}, false
	}
	var record struct {
		DBPath     string `json:"dbPath"`
		HomeDir    string `json:"homeDir"`
		LogPath    string `json:"logPath"`
		PIDPath    string `json:"pidPath"`
		SocketPath string `json:"socketPath"`
		StartedAt  string `json:"startedAt"`
		Version    int    `json:"version"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return RuntimeRecordPaths{}, false
	}
	if record.Version != 1 || record.DBPath == "" || record.HomeDir == "" || record.LogPath == "" ||
		record.PIDPath == "" || record.SocketPath == "" || record.StartedAt == "" {
		return RuntimeRecordPaths{}, false
	}
	return RuntimeRecordPaths{
		DBPath:     record.DBPath,
		LogPath:    record.LogPath,
		PIDPath:    record.PIDPath,
		SocketPath: record.SocketPath,
	}, true
}

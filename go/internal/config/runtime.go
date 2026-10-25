package config

// This file ports src/config/runtime.ts.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// RuntimePaths mirrors the TS type of the same name.
type RuntimePaths struct {
	ConfigPath        string
	DBPath            string
	EnvPath           string
	HomeDir           string
	LogPath           string
	PIDPath           string
	PiSessionDir      string
	RuntimeRecordPath string
	SocketPath        string
}

// RuntimeResolution mirrors the TS type. The optional fields keep the TS
// `undefined` semantics: Config is nil when the config file is absent, and
// ConfigErrors is nil unless an invalid config was explicitly tolerated.
type RuntimeResolution struct {
	Config       *HerdsmanConfig
	ConfigErrors []ConfigLoadError
	ConfigPath   string
	Environment  map[string]string
	HomeDir      string
	Paths        RuntimePaths
}

// RuntimeResolveOptions mirror the input object of resolveRuntime.
type RuntimeResolveOptions struct {
	AllowInvalidConfig bool
	Environment        map[string]string
}

// GetHerdsmanHome mirrors getHerdsmanHome: an explicit HERDSMAN_HOME wins,
// otherwise `<homedir>/.herdsman`.
func GetHerdsmanHome(environment map[string]string) (string, error) {
	explicit := strings.TrimSpace(environment["HERDSMAN_HOME"])
	if explicit != "" {
		return filepath.Abs(explicit)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Abs(filepath.Join(home, ".herdsman"))
}

// ResolveRuntimePath mirrors resolveRuntimePath: absolute values are kept,
// relative ones resolve against homeDir.
func ResolveRuntimePath(homeDir, value string) string {
	if filepath.IsAbs(value) {
		return filepath.Clean(value)
	}
	return filepath.Join(homeDir, value)
}

// ResolveRuntimePathsOptions mirror the input object of resolveRuntimePaths.
type ResolveRuntimePathsOptions struct {
	Config      *HerdsmanConfig
	Environment map[string]string
}

// ResolveRuntimePaths mirrors resolveRuntimePaths: the default layout under
// Herdsman home, overridden by `runtime.*` config values when present.
func ResolveRuntimePaths(options *ResolveRuntimePathsOptions) (RuntimePaths, error) {
	var opts ResolveRuntimePathsOptions
	if options != nil {
		opts = *options
	}
	homeDir, err := GetHerdsmanHome(opts.Environment)
	if err != nil {
		return RuntimePaths{}, err
	}

	var runtime *RuntimePathsConfig
	if opts.Config != nil {
		runtime = opts.Config.Runtime
	}
	dbPath := "state.db"
	logPath := filepath.Join("logs", "herdsman.log")
	pidPath := "herdsman.pid"
	socketPath := "herdsman.sock"
	if runtime != nil {
		if runtime.DBPath != nil {
			dbPath = *runtime.DBPath
		}
		if runtime.LogPath != nil {
			logPath = *runtime.LogPath
		}
		if runtime.PIDPath != nil {
			pidPath = *runtime.PIDPath
		}
		if runtime.SocketPath != nil {
			socketPath = *runtime.SocketPath
		}
	}

	return RuntimePaths{
		ConfigPath:        filepath.Join(homeDir, "config.yaml"),
		DBPath:            ResolveRuntimePath(homeDir, dbPath),
		EnvPath:           filepath.Join(homeDir, ".env"),
		HomeDir:           homeDir,
		LogPath:           ResolveRuntimePath(homeDir, logPath),
		PIDPath:           ResolveRuntimePath(homeDir, pidPath),
		PiSessionDir:      filepath.Join(homeDir, "pi-sessions"),
		RuntimeRecordPath: filepath.Join(homeDir, "runtime.json"),
		SocketPath:        ResolveRuntimePath(homeDir, socketPath),
	}, nil
}

// envKeyPattern is the TS `/^[A-Za-z_][A-Za-z0-9_]*$/` key filter.
var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// LoadHerdsmanDotEnv mirrors loadHerdsmanDotEnv: a copy of baseEnvironment with
// every well-formed key from envPath applied (later lines win). A missing .env
// yields an unchanged copy.
func LoadHerdsmanDotEnv(baseEnvironment map[string]string, envPath string) map[string]string {
	next := make(map[string]string, len(baseEnvironment))
	for key, value := range baseEnvironment {
		next[key] = value
	}

	source, err := os.ReadFile(envPath)
	if err != nil {
		return next
	}

	for _, rawLine := range splitLines(string(source)) {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		equalsIndex := strings.Index(line, "=")
		if equalsIndex <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:equalsIndex])
		if !envKeyPattern.MatchString(key) {
			continue
		}
		next[key] = unquoteEnvValue(strings.TrimSpace(line[equalsIndex+1:]))
	}

	return next
}

// splitLines splits on /\r?\n/ like the TS `split(/\r?\n/)`.
func splitLines(source string) []string {
	source = strings.ReplaceAll(source, "\r\n", "\n")
	return strings.Split(source, "\n")
}

// unquoteEnvValue strips one matching pair of surrounding quotes.
func unquoteEnvValue(value string) string {
	if len(value) >= 2 {
		first, last := value[0], value[len(value)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return value[1 : len(value)-1]
		}
	}
	return value
}

// RuntimeRecordPaths are the four paths a daemon runtime record can override.
type RuntimeRecordPaths struct {
	DBPath     string
	LogPath    string
	PIDPath    string
	SocketPath string
}

// RuntimeRecordReader reads and validates a runtime record. It returns ok=false
// for a missing/invalid record, mirroring readDaemonRuntimeRecord. It is a seam
// so this package does not depend on the daemon package.
type RuntimeRecordReader func(path string) (RuntimeRecordPaths, bool)

// RuntimePathsFromRecordOrDefaultOptions mirror the TS input object.
type RuntimePathsFromRecordOrDefaultOptions struct {
	Environment  map[string]string
	RecordPath   string
	RecordReader RuntimeRecordReader
}

// RuntimePathsFromRecordOrDefault mirrors runtimePathsFromRecordOrDefault: the
// default layout, with the four paths replaced by the last daemon's runtime
// record when one is readable.
func RuntimePathsFromRecordOrDefault(
	options *RuntimePathsFromRecordOrDefaultOptions,
) (RuntimePaths, error) {
	var opts RuntimePathsFromRecordOrDefaultOptions
	if options != nil {
		opts = *options
	}
	defaultPaths, err := ResolveRuntimePaths(&ResolveRuntimePathsOptions{Environment: opts.Environment})
	if err != nil {
		return RuntimePaths{}, err
	}
	recordPath := opts.RecordPath
	if recordPath == "" {
		recordPath = defaultPaths.RuntimeRecordPath
	}
	if opts.RecordReader == nil {
		return defaultPaths, nil
	}
	record, ok := opts.RecordReader(recordPath)
	if !ok {
		return defaultPaths, nil
	}
	merged := defaultPaths
	merged.DBPath = record.DBPath
	merged.LogPath = record.LogPath
	merged.PIDPath = record.PIDPath
	merged.SocketPath = record.SocketPath
	return merged, nil
}

// ResolveRuntime mirrors resolveRuntime. It returns an error carrying the TS
// message `Invalid Herdsman config: ...` when the config is invalid and
// invalid configs are not tolerated.
func ResolveRuntime(options *RuntimeResolveOptions) (RuntimeResolution, error) {
	var opts RuntimeResolveOptions
	if options != nil {
		opts = *options
	}
	initialPaths, err := ResolveRuntimePaths(&ResolveRuntimePathsOptions{Environment: opts.Environment})
	if err != nil {
		return RuntimeResolution{}, err
	}
	environment := LoadHerdsmanDotEnv(opts.Environment, initialPaths.EnvPath)
	basePaths, err := ResolveRuntimePaths(&ResolveRuntimePathsOptions{Environment: environment})
	if err != nil {
		return RuntimeResolution{}, err
	}

	if _, statErr := os.Stat(basePaths.ConfigPath); statErr != nil {
		return RuntimeResolution{
			ConfigPath:  basePaths.ConfigPath,
			Environment: environment,
			HomeDir:     basePaths.HomeDir,
			Paths:       basePaths,
		}, nil
	}

	loaded := LoadHerdsmanConfig(basePaths.ConfigPath)
	if !loaded.OK {
		if opts.AllowInvalidConfig {
			return RuntimeResolution{
				ConfigErrors: loaded.Errors,
				ConfigPath:   basePaths.ConfigPath,
				Environment:  environment,
				HomeDir:      basePaths.HomeDir,
				Paths:        basePaths,
			}, nil
		}
		messages := make([]string, 0, len(loaded.Errors))
		for _, loadErr := range loaded.Errors {
			messages = append(messages, loadErr.Message)
		}
		return RuntimeResolution{}, errors.New(
			fmt.Sprintf("Invalid Herdsman config: %s", strings.Join(messages, "; ")))
	}

	paths, err := ResolveRuntimePaths(&ResolveRuntimePathsOptions{
		Config:      &loaded.Value,
		Environment: environment,
	})
	if err != nil {
		return RuntimeResolution{}, err
	}
	value := loaded.Value
	return RuntimeResolution{
		Config:      &value,
		ConfigPath:  basePaths.ConfigPath,
		Environment: environment,
		HomeDir:     paths.HomeDir,
		Paths:       paths,
	}, nil
}

// DefaultStatusPlanConfig mirrors DEFAULT_STATUS_PLAN_CONFIG.
var DefaultStatusPlanConfig = StatusPlansConfig{
	BackoffFactor:    DefaultBackoffFactor,
	InitialBackoffMs: DefaultInitialBackoffMs,
	MaxBackoffMs:     DefaultMaxBackoffMs,
	TotalWindowMs:    DefaultStatusPlansTotalWindowMs,
}

// ResolveStatusPlansConfig mirrors resolveStatusPlansConfig: environment
// overrides (only when the value is usable) win over config, which wins over the
// defaults.
func ResolveStatusPlansConfig(cfg *HerdsmanConfig, environment map[string]string) StatusPlansConfig {
	var fromConfig *StatusPlansConfig
	if cfg != nil && cfg.Observability != nil {
		fromConfig = cfg.Observability.StatusPlans
	}

	initialBackoffEnv := strings.TrimSpace(environment["HERDSMAN_STATUS_PLAN_INITIAL_BACKOFF_MS"])
	maxBackoffEnv := strings.TrimSpace(environment["HERDSMAN_STATUS_PLAN_MAX_BACKOFF_MS"])
	factorEnv := strings.TrimSpace(environment["HERDSMAN_STATUS_PLAN_BACKOFF_FACTOR"])
	totalWindowEnv := strings.TrimSpace(environment["HERDSMAN_STATUS_PLAN_TOTAL_WINDOW_MS"])

	pick := func(envValue string, usable func(float64) bool, configValue float64, fallback float64) float64 {
		if envValue != "" {
			if number, ok := jsNumber(envValue); ok && usable(number) {
				return number
			}
		}
		if fromConfig != nil {
			return configValue
		}
		return fallback
	}

	positive := func(value float64) bool { return value > 0 }
	atLeastOne := func(value float64) bool { return value >= 1 }

	configInitial, configMax, configFactor, configTotal := 0.0, 0.0, 0.0, 0.0
	if fromConfig != nil {
		configInitial = fromConfig.InitialBackoffMs
		configMax = fromConfig.MaxBackoffMs
		configFactor = fromConfig.BackoffFactor
		configTotal = fromConfig.TotalWindowMs
	}

	return StatusPlansConfig{
		BackoffFactor: pick(factorEnv, atLeastOne, configFactor,
			DefaultStatusPlanConfig.BackoffFactor),
		InitialBackoffMs: pick(initialBackoffEnv, positive, configInitial,
			DefaultStatusPlanConfig.InitialBackoffMs),
		MaxBackoffMs: pick(maxBackoffEnv, positive, configMax,
			DefaultStatusPlanConfig.MaxBackoffMs),
		TotalWindowMs: pick(totalWindowEnv, positive, configTotal,
			DefaultStatusPlanConfig.TotalWindowMs),
	}
}

// jsNumber mirrors the JS `Number(string)` conversion for the values the status
// plan resolver accepts: "" is 0, numeric text parses, anything else is not a
// number (ok=false, i.e. NaN).
func jsNumber(raw string) (float64, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, true
	}
	value, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}

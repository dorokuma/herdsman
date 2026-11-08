package agenthistory

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dorokuma/herdsman/go/internal/observability"
)

// Session discovery, ported from src/agent-history/discovery.ts.
//
// Herdsman supports session history for exactly three agents, by decision:
//
//  1. `pi` — Herdr's own integration reports the session file itself, so the
//     official `agent_session.kind == "path"` value is used as-is after the
//     file-safety gate (this is the generic path channel, not a pi-only branch).
//  2. `agy` — Herdr reports only `kind == "id"`, and an agy conversation id is
//     the name of its own conversation database, so the id resolves to one
//     deterministic candidate per configured agy home (primary `HOME` first,
//     followed by `AGY_EXTRA_HOMES` / `DEFAULT_AGY_EXTRA_HOMES`):
//     - `agy` -> `<home>/.gemini/antigravity-cli/conversations/<id>.db`
//  3. `claude` — Claude Code registers its session id through the one-shot
//     `herdsman register-claude` CLI (subscriberKind "claude", kind "id"), and
//     the dispatch runtime keeps the JSONL transcripts under a deterministic
//     root, so the id resolves to `<root>/<project-dir>/<id>.jsonl` by
//     enumerating the root's first-level project directories for an exact
//     match:
//     - `claude` -> `<claudeProjectsRoot>/<project-dir>/<id>.jsonl`
//
// Every other agent — whether or not Herdr has an integration for it — is
// deliberately not resolved: herdsman does not walk agent directories, rank
// candidates by mtime/cwd, match role directories from the terminal title, or
// pick "the newest plausible file". An unresolvable pane is an explicit failure:
// one warning naming the official values plus the derived candidate path, then
// no reference.

// AllowedSessionRoots mirrors ALLOWED_SESSION_ROOTS: the root whitelist that
// still guards the herdsman-pi registration protocol.
var AllowedSessionRoots = []string{"/tmp/herdr-role-sessions"}

// DefaultAgyExtraHomes mirrors DEFAULT_AGY_EXTRA_HOMES.
var DefaultAgyExtraHomes = []string{"/root/.agy-pro-home"}

// AgyExtraHomesEnv is the environment variable holding colon-separated extra agy
// homes, consulted when no explicit list is passed.
const AgyExtraHomesEnv = "AGY_EXTRA_HOMES"

// DefaultClaudeProjectsRoot is the deterministic root the dispatch runtime
// writes Claude Code project transcripts under: every CC session of a role is
// `<root>/<project-dir>/<session-id>.jsonl`. Tests inject their own root through
// AgentHistoryLookupInput.ClaudeProjectsRoot or the service options, so the
// real /tmp directory is never touched by a test.
const DefaultClaudeProjectsRoot = "/tmp/herdr-role-sessions/claude-projects"

// AgentHistoryLookupInput mirrors AgentHistoryLookupInput. Optional fields are
// nil when absent; ExtraHomeDirs is nil only when the caller did not provide a
// list (a non-nil empty slice is an explicit override).
type AgentHistoryLookupInput struct {
	Agent              *string
	AgentSession       *observability.AgentSessionRef
	ClaudeProjectsRoot *string
	ExtraHomeDirs      []string
	HerdrSessionName   *string
	HomeDir            *string
	PaneID             *string
}

// DiscoverOptions mirrors `{ force?: boolean }`.
type DiscoverOptions struct {
	Force bool
}

// DiscoveryWarning is the payload TS passes as console.warn's second argument
// for an unresolvable pane.
type DiscoveryWarning struct {
	Agent            *string
	AgentSession     *observability.AgentSessionRef
	Detail           string
	HerdrSessionName *string
	PaneID           *string
}

// DiscoveryWarnMessage mirrors the console.warn message string.
const DiscoveryWarnMessage = "Herdsman has no usable official Herdr agent session"

// discoveryWarnSink receives (message, payload) for every unresolvable pane.
// The default mirrors console.warn by writing one line to stderr; tests replace
// it through setDiscoveryWarnSinkForTest.
var discoveryWarnSink func(message string, warning DiscoveryWarning) = defaultDiscoveryWarnSink

func defaultDiscoveryWarnSink(message string, warning DiscoveryWarning) {
	agent := ""
	if warning.Agent != nil {
		agent = *warning.Agent
	}
	fmt.Fprintf(os.Stderr, "%s (agent=%s): %s\n", message, agent, warning.Detail)
}

// setDiscoveryWarnSinkForTest lives in discovery_test.go: it needs *testing.T.

// A pane whose official value cannot be resolved must not be re-resolved — nor
// re-warned — on every refresh round. The failure is remembered per official
// input (agent, kind, source, value, store root) for
// DiscoveryRetryAfterFailureMs: within that window the lookup is skipped
// entirely, and only the first failure for a key logs a warning. Any change in
// the official values (or in the store root) is a new key and is attempted
// immediately, and the window is deliberately bounded so a session file that
// appears a few seconds after the pane reports its id is still picked up.
const (
	// DiscoveryRetryAfterFailureMs mirrors DISCOVERY_RETRY_AFTER_FAILURE_MS.
	DiscoveryRetryAfterFailureMs = 60_000
	// DiscoveryFailureCacheLimit mirrors DISCOVERY_FAILURE_CACHE_LIMIT.
	DiscoveryFailureCacheLimit = 512
)

var (
	discoveryMu          sync.Mutex
	failedDiscoveryAt    = map[string]int64{}
	failedDiscoveryOrder []string
	warnedDiscoveryKeys  = map[string]struct{}{}
	warnedDiscoveryOrder []string
)

// ResolveAgyExtraHomes mirrors resolveAgyExtraHomes.
//
// extraHomeDirs == nil means "not provided"; a non-nil slice (even empty) is an
// explicit override. env == nil means "not provided". Entries are trimmed and
// empty ones dropped, exactly like the TS `.map(trim).filter(Boolean)`.
func ResolveAgyExtraHomes(extraHomeDirs []string, env *string) []string {
	var raw []string
	switch {
	case extraHomeDirs != nil:
		raw = extraHomeDirs
	case env != nil:
		raw = strings.Split(*env, ":")
	default:
		raw = DefaultAgyExtraHomes
	}
	homes := make([]string, 0, len(raw))
	for _, entry := range raw {
		trimmed := strings.TrimSpace(entry)
		if trimmed != "" {
			homes = append(homes, trimmed)
		}
	}
	return homes
}

// resolveAgyExtraHomesFromEnv mirrors calling resolveAgyExtraHomes with its
// default envValue, i.e. `process.env.AGY_EXTRA_HOMES` being undefined when the
// variable is unset.
func resolveAgyExtraHomesFromEnv(extraHomeDirs []string) []string {
	if value, ok := os.LookupEnv(AgyExtraHomesEnv); ok {
		return ResolveAgyExtraHomes(extraHomeDirs, &value)
	}
	return ResolveAgyExtraHomes(extraHomeDirs, nil)
}

// AgyHomeDirsInput mirrors the resolveAgyHomeDirs input.
type AgyHomeDirsInput struct {
	ExtraHomeDirs []string
	HomeDir       *string
}

// ResolveAgyHomeDirs mirrors resolveAgyHomeDirs: the primary home first, then the
// configured extra homes, de-duplicated by normalized path while preserving
// order. When nothing is configured at all, one empty entry is returned so the
// caller still derives (and rejects) a candidate.
func ResolveAgyHomeDirs(input AgyHomeDirsInput) []string {
	primary := strings.TrimSpace(homeDirOrEnv(input.HomeDir))
	homes := make([]string, 0, len(input.ExtraHomeDirs)+1)
	seen := map[string]struct{}{}
	for _, candidate := range append([]string{primary}, resolveAgyExtraHomesFromEnv(input.ExtraHomeDirs)...) {
		if candidate == "" {
			continue
		}
		normalized := filepath.Clean(candidate)
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		homes = append(homes, candidate)
	}
	if len(homes) == 0 {
		homes = append(homes, "")
	}
	return homes
}

func homeDirOrEnv(homeDir *string) string {
	if homeDir != nil {
		return *homeDir
	}
	if value, ok := os.LookupEnv("HOME"); ok {
		return value
	}
	return ""
}

// DiscoveryLookupKey mirrors discoveryLookupKey.
func DiscoveryLookupKey(input AgentHistoryLookupInput) string {
	agent := ""
	if input.Agent != nil {
		agent = *input.Agent
	}
	kind := ""
	source := ""
	value := ""
	if input.AgentSession != nil {
		kind = string(input.AgentSession.Kind)
		source = input.AgentSession.Source
		value = input.AgentSession.Value
	}
	return strings.Join([]string{
		agent,
		kind,
		source,
		value,
		claudeProjectsRootOf(input.ClaudeProjectsRoot),
		homeDirOrEnv(input.HomeDir),
		strings.Join(resolveAgyExtraHomesFromEnv(input.ExtraHomeDirs), ":"),
	}, "\x00")
}

// DiscoveryFailureFresh mirrors discoveryFailureFresh: true while the very same
// official values are still inside their retry window.
func DiscoveryFailureFresh(input AgentHistoryLookupInput, now int64) bool {
	key := DiscoveryLookupKey(input)
	discoveryMu.Lock()
	defer discoveryMu.Unlock()
	at, ok := failedDiscoveryAt[key]
	return ok && now-at < DiscoveryRetryAfterFailureMs
}

func rememberDiscoveryFailure(key string, input AgentHistoryLookupInput, detail string) {
	firstWarning := false
	discoveryMu.Lock()
	rememberFailureLocked(key, time.Now().UnixMilli())
	if _, warned := warnedDiscoveryKeys[key]; !warned {
		rememberWarnedLocked(key)
		firstWarning = true
	}
	sink := discoveryWarnSink
	discoveryMu.Unlock()
	if firstWarning {
		sink(DiscoveryWarnMessage, buildDiscoveryWarning(input, detail))
	}
}

// rememberFailureLocked / rememberWarnedLocked keep the TS Map/Set
// insertion-order semantics: a key that is re-set moves to the end, and the
// oldest key is evicted once the cache grows past DiscoveryFailureCacheLimit.
func rememberFailureLocked(key string, now int64) {
	if _, ok := failedDiscoveryAt[key]; !ok {
		failedDiscoveryOrder = append(failedDiscoveryOrder, key)
	}
	failedDiscoveryAt[key] = now
	for len(failedDiscoveryAt) > DiscoveryFailureCacheLimit {
		oldest := failedDiscoveryOrder[0]
		failedDiscoveryOrder = failedDiscoveryOrder[1:]
		delete(failedDiscoveryAt, oldest)
	}
}

func rememberWarnedLocked(key string) {
	if _, ok := warnedDiscoveryKeys[key]; ok {
		return
	}
	warnedDiscoveryKeys[key] = struct{}{}
	warnedDiscoveryOrder = append(warnedDiscoveryOrder, key)
	for len(warnedDiscoveryKeys) > DiscoveryFailureCacheLimit {
		oldest := warnedDiscoveryOrder[0]
		warnedDiscoveryOrder = warnedDiscoveryOrder[1:]
		delete(warnedDiscoveryKeys, oldest)
	}
}

func buildDiscoveryWarning(input AgentHistoryLookupInput, detail string) DiscoveryWarning {
	return DiscoveryWarning{
		Agent:            input.Agent,
		AgentSession:     input.AgentSession,
		Detail:           detail,
		HerdrSessionName: input.HerdrSessionName,
		PaneID:           input.PaneID,
	}
}

// DiscoverAgentHistory mirrors discoverAgentHistory. A nil result means "no
// usable official Herdr agent session" (the failure has been remembered and, for
// the first failure of that key, warned); TS never throws here either.
func DiscoverAgentHistory(input AgentHistoryLookupInput, options DiscoverOptions) *HistoryRef {
	key := DiscoveryLookupKey(input)
	if !options.Force {
		discoveryMu.Lock()
		failureAt, ok := failedDiscoveryAt[key]
		discoveryMu.Unlock()
		if ok && time.Now().UnixMilli()-failureAt < DiscoveryRetryAfterFailureMs {
			return nil
		}
	}
	fail := func(detail string) *HistoryRef {
		rememberDiscoveryFailure(key, input, detail)
		return nil
	}
	session := input.AgentSession
	if session == nil {
		return fail("Herdr reported no agent_session for this pane; herdsman does not guess one")
	}
	if session.Kind == observability.SessionRefPath {
		resolved := SafeOfficialSessionPath(session.Value)
		if resolved == "" {
			return fail(fmt.Sprintf(
				"Herdr agent_session path is not a readable session file owned by herdsman: %s",
				session.Value))
		}
		return &HistoryRef{
			Kind:   KindAgentSession,
			Path:   &resolved,
			Source: HistorySourceFromSessionRef(*session),
			Value:  resolved,
		}
	}
	resolved := resolveSessionFileById(sessionFileByIDInput{
		claudeProjectsRoot: input.ClaudeProjectsRoot,
		extraHomeDirs:      input.ExtraHomeDirs,
		homeDir:            input.HomeDir,
		session:            session,
	})
	if resolved.ref != nil {
		return resolved.ref
	}
	if resolved.transient {
		// A claude-jsonl id resolves to a file CC itself creates on startup, so a
		// lookup that lands before the first record is a timing miss, not a real
		// failure: it must not enter the 60-second negative cache (a task that
		// finishes inside five seconds would otherwise never be read).
		return nil
	}
	return fail(resolved.detail)
}

// AgySessionCandidatesInput mirrors the resolveAgySessionCandidates input.
type AgySessionCandidatesInput struct {
	ExtraHomeDirs []string
	HomeDir       *string
	ID            string
}

// AgySessionCandidates mirrors the resolveAgySessionCandidates return value.
type AgySessionCandidates struct {
	Detail string
	Refs   []HistoryRef
}

// ResolveAgySessionCandidates mirrors resolveAgySessionCandidates: deterministic
// id -> session file resolution for `agy` across primary `HOME` and configured
// extra homes. Each candidate home derives one path from the official id alone
// and verifies it through containedFileRef (`safeOfficialSessionPath` +
// `isInside`). Hits are de-duplicated by realpath while preserving home priority
// (primary/Ultra first).
//
// An agent without a template here is not guessed.
func ResolveAgySessionCandidates(input AgySessionCandidatesInput) AgySessionCandidates {
	if !isUuidLike(input.ID) {
		return AgySessionCandidates{
			Detail: fmt.Sprintf("agy conversation id is not a uuid: %s", input.ID),
			Refs:   nil,
		}
	}
	homes := ResolveAgyHomeDirs(AgyHomeDirsInput{
		ExtraHomeDirs: input.ExtraHomeDirs,
		HomeDir:       input.HomeDir,
	})
	refs := make([]HistoryRef, 0, len(homes))
	seenRealpaths := map[string]struct{}{}
	hitDetails := make([]string, 0, len(homes))
	missDetails := make([]string, 0, len(homes))
	for _, homeDir := range homes {
		dir := filepath.Join(homeDir, ".gemini", "antigravity-cli", "conversations")
		resolution := containedFileRef(containedFileRefInput{
			candidate: filepath.Join(dir, fmt.Sprintf("%s.db", input.ID)),
			id:        input.ID,
			label:     "agy",
			root:      dir,
			source:    SourceAntigravitySQLite,
		})
		if resolution.ref != nil {
			real := resolution.ref.ResolvedPath()
			if _, ok := seenRealpaths[real]; !ok {
				seenRealpaths[real] = struct{}{}
				refs = append(refs, *resolution.ref)
				hitDetails = append(hitDetails, resolution.detail)
			}
			continue
		}
		missDetails = append(missDetails, resolution.detail)
	}
	detail := ""
	if len(hitDetails) > 0 {
		detail = hitDetails[0]
	} else {
		detail = strings.Join(missDetails, "; ")
	}
	return AgySessionCandidates{Detail: detail, Refs: refs}
}

type sessionFileByIDInput struct {
	claudeProjectsRoot *string
	extraHomeDirs      []string
	homeDir            *string
	session            *observability.AgentSessionRef
}

func resolveSessionFileById(input sessionFileByIDInput) sessionFileResolution {
	source := HistorySourceFromSessionRef(*input.session)
	miss := func(detail string) sessionFileResolution {
		return sessionFileResolution{detail: detail, ref: nil}
	}
	if source == SourceAntigravitySQLite {
		resolved := ResolveAgySessionCandidates(AgySessionCandidatesInput{
			ExtraHomeDirs: input.extraHomeDirs,
			HomeDir:       input.homeDir,
			ID:            input.session.Value,
		})
		ref := (*HistoryRef)(nil)
		if len(resolved.Refs) > 0 {
			ref = &resolved.Refs[0]
		}
		return sessionFileResolution{detail: resolved.Detail, ref: ref}
	}
	if source == SourceClaudeJSONL {
		return ResolveClaudeSessionFile(ClaudeSessionFileInput{
			ID:                 input.session.Value,
			ClaudeProjectsRoot: input.claudeProjectsRoot,
		})
	}
	if source != SourceUnknown {
		return miss(fmt.Sprintf(
			"%s reported an id; herdsman has no id-based session-file derivation for it (only agy and claude do)",
			input.session.Agent))
	}
	return miss(fmt.Sprintf(
		"%s is not a supported herdsman history agent (only pi, agy and claude are)",
		input.session.Agent))
}

// claudeProjectsRootOf resolves the effective claude projects root: an injected
// root wins, otherwise the deterministic dispatch-runtime default.
func claudeProjectsRootOf(root *string) string {
	if root != nil && *root != "" {
		return *root
	}
	return DefaultClaudeProjectsRoot
}

// ClaudeSessionFileInput mirrors the resolveClaudeSessionFile input.
type ClaudeSessionFileInput struct {
	ID                 string
	ClaudeProjectsRoot *string
}

// ResolveClaudeSessionFile resolves one Claude Code session id to its JSONL
// transcript: only the first-level project directories of the claude projects
// root are enumerated, and only an exact `<project-dir>/<id>.jsonl` name match
// is a candidate. Each candidate is verified through containedFileRef
// (`safeOfficialSessionPath` + `isInside`) against the projects root.
//
// `transient` marks the "file not created yet" miss: CC writes the transcript
// on session start, so a lookup that lands a moment earlier must not count as
// a real failure (DiscoverAgentHistory skips the 60s negative cache for it).
func ResolveClaudeSessionFile(input ClaudeSessionFileInput) sessionFileResolution {
	id := input.ID
	miss := func(detail string) sessionFileResolution {
		return sessionFileResolution{detail: detail, ref: nil}
	}
	if !isUuidLike(id) {
		return miss(fmt.Sprintf("claude session id is not a uuid: %s", id))
	}
	id = strings.ToLower(id)
	root := claudeProjectsRootOf(input.ClaudeProjectsRoot)
	entries, err := os.ReadDir(root)
	if err != nil {
		return sessionFileResolution{
			detail:    fmt.Sprintf("claude: cannot list projects root %s: %v", root, err),
			ref:       nil,
			transient: os.IsNotExist(err),
		}
	}
	fileName := id + ".jsonl"
	missDetails := []string{}
	sawCandidate := false
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidate := filepath.Join(root, entry.Name(), fileName)
		if _, statErr := os.Lstat(candidate); statErr != nil {
			continue
		}
		sawCandidate = true
		resolution := containedFileRef(containedFileRefInput{
			candidate: candidate,
			id:        id,
			label:     "claude",
			root:      filepath.Join(root, entry.Name()),
			source:    SourceClaudeJSONL,
		})
		if resolution.ref != nil {
			return sessionFileResolution{detail: resolution.detail, ref: resolution.ref}
		}
		missDetails = append(missDetails, resolution.detail)
	}
	if sawCandidate {
		// A matching file exists but failed the safety gate: a real failure,
		// reported and cached like every other unresolvable official value.
		return sessionFileResolution{
			detail:    strings.Join(missDetails, "; "),
			ref:       nil,
			transient: false,
		}
	}
	// No candidate file exists yet: the transcript has not been created, so the
	// miss is transient (it would stop being transient the moment the file
	// appears). A directory that cannot even be listed was already reported as
	// transient above only for ENOENT.
	return sessionFileResolution{
		detail:    fmt.Sprintf("claude: no %s transcript under %s", fileName, root),
		ref:       nil,
		transient: true,
	}
}

type sessionFileResolution struct {
	detail    string
	ref       *HistoryRef
	transient bool
}

type containedFileRefInput struct {
	candidate string
	id        string
	label     string
	root      string
	source    HistorySource
}

// containedFileRef verifies one candidate path: regular file, not a symlink,
// owned by herdsman, not writable by other principals, and really inside the
// agent's store root. Any failure is a miss (the caller reports it), never a
// fallback search.
func containedFileRef(input containedFileRefInput) sessionFileResolution {
	real := SafeOfficialSessionPath(input.candidate)
	if real == "" {
		return sessionFileResolution{
			detail: fmt.Sprintf("%s: %s is not a readable session file", input.label, input.candidate),
			ref:    nil,
		}
	}
	if !isInside(input.root, real) {
		return sessionFileResolution{
			detail: fmt.Sprintf("%s: %s escapes %s", input.label, real, input.root),
			ref:    nil,
		}
	}
	return sessionFileResolution{
		detail: fmt.Sprintf("%s: resolved %s", input.label, real),
		ref: &HistoryRef{
			Kind:   KindAgentSession,
			Path:   &real,
			Source: input.source,
			Value:  input.id,
		},
	}
}

func isInside(root, path string) bool {
	realRoot := realpathOrNormalize(root)
	rest := relativePath(realRoot, path)
	// Node's relative() returns "" for the root itself, while filepath.Rel
	// returns "."; both mean "not strictly inside".
	return rest != "" && rest != "." && !strings.HasPrefix(rest, "..") && !filepath.IsAbs(rest)
}

func relativePath(from, to string) string {
	rest, err := filepath.Rel(from, to)
	if err != nil {
		return to
	}
	return rest
}

func realpathOrNormalize(value string) string {
	if real, err := filepath.EvalSymlinks(value); err == nil {
		return real
	}
	return filepath.Clean(value)
}

var uuidLikePattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// isUuidLike is version-agnostic on purpose: agy conversation ids are uuids, and
// a uuid must stay pure hex+dashes so it can never act as a path.
func isUuidLike(value string) bool {
	return uuidLikePattern.MatchString(strings.ToLower(value))
}

var nonSegmentSeparator = func(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z':
		return false
	case r >= '0' && r <= '9':
		return false
	case r == '_':
		return false
	default:
		return true
	}
}

// HistorySourceFromSessionRef mirrors historySourceFromSessionRef: maps an
// official `agent_session` to the reader that can read it. `pi` and `pig`
// (PiG, which emits the same pi-jsonl session format and herdsman protocol)
// share the `pi-jsonl` reader; `agy` resolves to `antigravity-sqlite` and
// `claude` (Claude Code, registered via `herdsman register-claude`) to
// `claude-jsonl`. Everything else is `unknown`, which every reader rejects.
// `source` looks like `herdr:<agent>` or `dispatch-agent:<agent>`, so segments
// are matched whole — `herdr:copilot` contains "pi" and must never be read as
// a pi session.
func HistorySourceFromSessionRef(ref observability.AgentSessionRef) HistorySource {
	segments := []string{}
	if agent := strings.ToLower(ref.Agent); agent != "" {
		segments = append(segments, agent)
	}
	for _, segment := range strings.FieldsFunc(strings.ToLower(ref.Source), nonSegmentSeparator) {
		if segment != "" {
			segments = append(segments, segment)
		}
	}
	has := func(names ...string) bool {
		for _, name := range names {
			for _, segment := range segments {
				if segment == name {
					return true
				}
			}
		}
		return false
	}
	if has("agy", "antigravity", "antigravity_cli", "antigravity-cli") {
		return SourceAntigravitySQLite
	}
	if has("claude") {
		return SourceClaudeJSONL
	}
	if has("pi", "pig") {
		return SourcePiJSONL
	}
	return SourceUnknown
}

// homeSessionRoots mirrors homeSessionRoots: the home-relative session roots
// the registration whitelist accepts next to the fixed AllowedSessionRoots.
//
//   - .pi/agent/sessions — pi keeps its sessions under its own agent home.
//   - .pig/agent/sessions — pig (PiG) keeps its sessions under its own agent
//     home; pig emits the same pi-jsonl session format and herdsman protocol,
//     so the session path a pig pane registers lives in pig's home, not pi's.
func homeSessionRoots(home string) []string {
	return []string{
		filepath.Join(home, ".pi/agent/sessions"),
		filepath.Join(home, ".pig/agent/sessions"),
	}
}

// SafeAllowedSessionPath mirrors safeAllowedSessionPath: the root whitelist used
// by the herdsman-pi registration protocol, on top of the file-safety gate.
func SafeAllowedSessionPath(value string, homeDir *string) string {
	real := safeRegularFile(value)
	if real == "" {
		return ""
	}
	home := ""
	if homeDir != nil {
		home = *homeDir
	} else if env, ok := os.LookupEnv("HOME"); ok {
		home = env
	} else {
		home = "/root"
	}
	roots := append(homeSessionRoots(home), AllowedSessionRoots...)
	for _, root := range roots {
		rest := relativePath(root, real)
		if rest == "" || rest == "." || (!strings.HasPrefix(rest, "..") && !filepath.IsAbs(rest)) {
			return real
		}
	}
	return ""
}

// safeRegularFile is the file-safety gate shared by safeOfficialSessionPath and
// safeAllowedSessionPath.
//
// The raw path is checked with `os.Lstat` **before** it is resolved: after
// EvalSymlinks the symlink itself is gone, so a symlink check on the resolved
// path could never fire. A symlink (or anything that is not a regular file) at
// the reported path is rejected outright; the resolved target is then checked
// again, so a hard-link/symlink swap behind a directory cannot smuggle in a file
// herdsman does not own.
func safeRegularFile(value string) string {
	if !filepath.IsAbs(value) || strings.Contains(value, "..") {
		return ""
	}
	reported := filepath.Clean(value)
	raw, err := os.Lstat(reported)
	if err != nil {
		return ""
	}
	if raw.Mode()&os.ModeSymlink != 0 || !raw.Mode().IsRegular() {
		return ""
	}
	real, err := filepath.EvalSymlinks(reported)
	if err != nil {
		return ""
	}
	target, err := os.Lstat(real)
	if err != nil {
		return ""
	}
	if target.Mode()&os.ModeSymlink != 0 || !target.Mode().IsRegular() {
		return ""
	}
	if target.Mode().Perm()&0o022 != 0 {
		return ""
	}
	if uid, ok := statUID(target); !ok || uid != currentEUID() {
		return ""
	}
	return real
}

func statUID(info os.FileInfo) (int, bool) {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return int(stat.Uid), true
	}
	return 0, false
}

func currentEUID() int {
	if euid := os.Geteuid(); euid >= 0 {
		return euid
	}
	return -1
}

// SafeOfficialSessionPath mirrors safeOfficialSessionPath: the file-safety gate
// for an official session path reported by Herdr.
//
// Herdr only reports back the value an installed agent integration handed it
// (`pane.report_agent_session`), so the *root* of an official path is not
// guessed any more and is deliberately not whitelisted: pi keeps its sessions
// under its own agent home, and a fixed root list would silently reject those
// official values. What is still enforced is that herdsman only reads a regular,
// non-symlink file it owns and that no other principal can rewrite behind it.
// SafeAllowedSessionPath keeps the root whitelist for the herdsman-pi
// registration protocol, where a pane claims a path itself.
func SafeOfficialSessionPath(value string) string {
	return safeRegularFile(value)
}

// SessionPathAllowedByShape mirrors sessionPathAllowedByShape: the shape-only
// (no filesystem) half of the registration whitelist. Unlike
// SafeAllowedSessionPath the root itself is not accepted.
func SessionPathAllowedByShape(value string, homeDir *string) bool {
	if !filepath.IsAbs(value) || strings.Contains(value, "..") {
		return false
	}
	resolved := filepath.Clean(value)
	home := ""
	if homeDir != nil {
		home = *homeDir
	} else if env, ok := os.LookupEnv("HOME"); ok {
		home = env
	} else {
		home = "/root"
	}
	roots := append(homeSessionRoots(home), AllowedSessionRoots...)
	for _, root := range roots {
		rest := relativePath(root, resolved)
		if rest != "" && rest != "." && !strings.HasPrefix(rest, "..") && !filepath.IsAbs(rest) {
			return true
		}
	}
	return false
}

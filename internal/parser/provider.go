package parser

import (
	"context"
	"errors"
	"time"
)

const (
	ProviderFeatureFingerprint = "fingerprint"
	ProviderFeatureParse       = "parse"
)

// ErrUnsupportedProviderFeature identifies optional provider behavior that is
// intentionally absent. Callers use errors.Is to distinguish this from I/O or
// parse failures.
var ErrUnsupportedProviderFeature = errors.New("unsupported provider feature")

// UnsupportedProviderFeatureError wraps ErrUnsupportedProviderFeature with the
// provider and feature names that produced it.
type UnsupportedProviderFeatureError struct {
	Provider AgentType
	Feature  string
}

func (err UnsupportedProviderFeatureError) Error() string {
	if err.Provider == "" {
		return "unsupported provider feature " + err.Feature
	}
	return string(err.Provider) + ": unsupported provider feature " + err.Feature
}

func (err UnsupportedProviderFeatureError) Unwrap() error {
	return ErrUnsupportedProviderFeature
}

// ProviderFactory is the registry surface for creating config-bound provider
// instances.
type ProviderFactory interface {
	Definition() AgentDef
	Capabilities() Capabilities
	NewProvider(ProviderConfig) Provider
}

// ProviderConfig is copied into a provider instance at construction time.
type ProviderConfig struct {
	Roots   []string
	Machine string
	// PathRewriter maps an on-disk source path to its canonical stored form.
	// It is non-nil only during remote (SSH) sync, where source files are read
	// from a temporary extraction directory but must keep a stable identity
	// across syncs. Providers whose session IDs are derived from the source
	// path (Aider) use it to seed those IDs from the canonical remote path
	// rather than the changing temp path. Most providers ignore it.
	PathRewriter func(string) string
}

// Clone returns an independent config snapshot.
func (cfg ProviderConfig) Clone() ProviderConfig {
	cfg.Roots = cfg.RootsCopy()
	return cfg
}

// RootsCopy returns an independent roots slice.
func (cfg ProviderConfig) RootsCopy() []string {
	return append([]string(nil), cfg.Roots...)
}

// Provider is the target parser/source facade. Providers own source shape,
// source identity, freshness, and lookup; the engine consumes SourceRefs,
// SourceFingerprints, and normalized ParseResults without knowing whether the
// backing data is a file, virtual DB row, sidecar set, remote canonical path, or
// multi-session container.
type Provider interface {
	Definition() AgentDef
	Capabilities() Capabilities

	Discover(context.Context) ([]SourceRef, error)
	WatchPlan(context.Context) (WatchPlan, error)
	SourcesForChangedPath(context.Context, ChangedPathRequest) ([]SourceRef, error)
	FindSource(context.Context, FindSourceRequest) (SourceRef, bool, error)
	Fingerprint(context.Context, SourceRef) (SourceFingerprint, error)

	// Parse returns a normalized outcome for one logical source. A non-nil
	// error is a whole-source failure, including context cancellation; callers
	// must ignore the returned ParseOutcome. Partial multi-session success is
	// represented by a nil error with successful Results plus SourceErrors for
	// isolated per-session failures.
	Parse(context.Context, ParseRequest) (ParseOutcome, error)
	ParseIncremental(
		context.Context,
		IncrementalRequest,
	) (IncrementalOutcome, IncrementalStatus, error)
}

// ProviderBase is embedded by concrete providers to make optional source
// methods callable with zero-value no-op behavior.
type ProviderBase struct {
	Def    AgentDef
	Caps   Capabilities
	Config ProviderConfig
}

func (b ProviderBase) Definition() AgentDef {
	return cloneAgentDef(b.Def)
}

func (b ProviderBase) Capabilities() Capabilities {
	return b.Caps
}

func (b ProviderBase) Discover(context.Context) ([]SourceRef, error) {
	return nil, nil
}

func (b ProviderBase) WatchPlan(context.Context) (WatchPlan, error) {
	return WatchPlan{}, nil
}

func (b ProviderBase) SourcesForChangedPath(
	context.Context,
	ChangedPathRequest,
) ([]SourceRef, error) {
	return nil, nil
}

func (b ProviderBase) FindSource(
	context.Context,
	FindSourceRequest,
) (SourceRef, bool, error) {
	return SourceRef{}, false, nil
}

func (b ProviderBase) Fingerprint(
	context.Context,
	SourceRef,
) (SourceFingerprint, error) {
	return SourceFingerprint{}, b.unsupported(ProviderFeatureFingerprint)
}

func (b ProviderBase) ParseIncremental(
	context.Context,
	IncrementalRequest,
) (IncrementalOutcome, IncrementalStatus, error) {
	return IncrementalOutcome{}, IncrementalUnsupported, nil
}

func (b ProviderBase) unsupported(feature string) error {
	return UnsupportedProviderFeatureError{
		Provider: b.Def.Type,
		Feature:  feature,
	}
}

// SourceRef is the engine-visible handle for provider-owned source data. It is
// the only source identity the engine should carry between discovery, changed
// path classification, lookup, fingerprinting, parsing, skip-cache checks, and
// persisted session metadata.
type SourceRef struct {
	// Provider identifies the provider that created this source and must match
	// the provider instance used for subsequent operations.
	Provider AgentType
	// Key is stable within the provider across process restarts. It is suitable
	// for dedupe and diagnostics, but not necessarily for DB freshness checks.
	Key string
	// DisplayPath is human-readable and may be a virtual path. For filesystem
	// sources it is usually the path users expect to inspect. For shared stores
	// it may be a provider virtual path such as "<db>#<sessionID>".
	DisplayPath string
	// FingerprintKey is the persisted lookup key for source metadata,
	// skip-cache, and parser data-version freshness checks. Providers should set
	// it to the same identity they store in ParsedSession.File.Path whenever
	// practical, and migrated providers must keep it compatible with legacy
	// file_path values unless a documented provider-specific transition handles
	// old rows.
	FingerprintKey string
	// ProjectHint is advisory metadata for UI grouping and may be empty.
	ProjectHint string
	// Opaque is provider-owned in-memory state. The engine must not persist,
	// compare, inspect, or log it, and providers must not require it for lookup
	// from persisted rows. Any source that needs to survive a process restart
	// must be recoverable from Key, DisplayPath, FingerprintKey,
	// FindSourceRequest, or discovery.
	Opaque any
}

// WatchPlan describes provider-owned filesystem watch roots. Provider
// WatchPlans are authoritative for migrated providers; legacy AgentDef watch
// fields are fallback compatibility only.
type WatchPlan struct {
	Roots []WatchRoot
}

// WatchRoot is one filesystem root the engine should watch. Recursive roots
// observe nested source creation. Non-recursive roots observe only direct child
// changes and must not be treated as covering missing nested provider roots
// unless caller-specific creation handling documents that equivalence.
type WatchRoot struct {
	Path         string
	Recursive    bool
	IncludeGlobs []string
	ExcludeGlobs []string
	DebounceKey  string
}

// ChangedPathRequest is passed back to providers for authoritative changed-path
// classification.
type ChangedPathRequest struct {
	Path      string
	EventKind string
	// WatchRoot is the provider WatchRoot that observed Path. Providers should
	// use it to scope classification and avoid returning sources from unrelated
	// configured roots that happen to match the same basename or raw ID.
	WatchRoot string
	// StoredSourcePaths are optional provider-persisted source paths already
	// known to the caller for this watch root. Providers that model a shared
	// physical file as virtual per-session sources use these to emit tombstone
	// sources when a DB row or DB file has disappeared and can no longer be
	// rediscovered from current metadata. Hints are advisory: providers must
	// still validate ownership against the changed path/watch root before
	// emitting them.
	StoredSourcePaths []string
}

// FindSourceRequest contains lookup inputs and persisted source hints for
// provider-owned source resolution. RawSessionID and FullSessionID identify the
// requested logical session. StoredFilePath and FingerprintKey are advisory
// hints from the archive, not authoritative filesystem paths; providers should
// try them first when useful, but provider-owned identity decides whether the
// source belongs to the requested session.
type FindSourceRequest struct {
	RawSessionID  string
	FullSessionID string
	// StoredFilePath is the persisted sessions.file_path value, which may be a
	// provider virtual path and may be stale after a move, deletion, or remote
	// sync identity rewrite.
	StoredFilePath string
	// FingerprintKey is the persisted source freshness key when the caller has
	// one distinct from StoredFilePath.
	FingerprintKey string
	// RequireFreshSource asks the provider to verify the source against current
	// provider metadata before returning it. A stale hint may still be used to
	// find the current source, but if the provider cannot prove the requested
	// session exists now it should return found=false rather than a tombstone.
	RequireFreshSource bool
	// PreferStoredSource asks the provider to return a valid StoredFilePath
	// source as-is rather than re-resolving it to a different but equivalent
	// source (for example a duplicate of the same session in another on-disk
	// layout). Source-lookup callers set it so an explicitly stored or pinned
	// source path is preserved; sync processing leaves it false so duplicate
	// sources still canonicalize to a single location.
	PreferStoredSource bool
}

// SourceFingerprint is the provider-normalized source freshness identity. The
// engine uses Key plus size/mtime/hash fields for skip-cache, data-version, and
// source metadata compatibility, including PostgreSQL push/read parity. Key
// should normally match SourceRef.FingerprintKey or ParsedSession.File.Path.
type SourceFingerprint struct {
	Key     string
	Size    int64
	MTimeNS int64
	Inode   uint64
	Device  uint64
	Hash    string
}

// ParseRequest is the full-parse provider input.
type ParseRequest struct {
	Source      SourceRef
	Fingerprint SourceFingerprint
	Machine     string
	ForceParse  bool
}

// ParseOutcome is the full-parse provider output. It is meaningful only when
// Provider.Parse returns a nil error. Providers own persisted source identity:
// each ParseResult.Result.Session.File.Path must be the same provider identity
// used for source metadata lookups and PostgreSQL/session metadata compatibility
// (usually SourceRef.FingerprintKey). For multi-session sources, every returned
// result must use a session-scoped path when the backing source can produce more
// than one logical session.
type ParseOutcome struct {
	Results            []ParseResultOutcome
	ExcludedSessionIDs []string
	SourceErrors       []SourceError
	ResultSetComplete  bool
	ForceReplace       bool
	SkipReason         SkipReason
}

// ParseResultOutcome pairs a normalized parse result with per-session retry and
// data-version state.
type ParseResultOutcome struct {
	Result      ParseResult
	DataVersion DataVersionState
	RetryReason string
}

// SourceError reports a per-session parse failure from a multi-session source.
// Providers use the Parse error return instead when a failure cannot be
// isolated to a persisted full session ID.
type SourceError struct {
	SourceKey   string
	DisplayPath string
	SessionID   string
	Err         error
	Retryable   bool
}

// DataVersionState describes whether a parsed result is current for this parser
// data version. Data-version freshness is per result; clean skip-cache
// persistence is still source-scoped and must be suppressed by callers when any
// result needs retry, any source error exists, or the result set is incomplete.
type DataVersionState uint8

const (
	DataVersionUnspecified DataVersionState = iota
	DataVersionCurrent
	DataVersionNeedsRetry
)

// SkipReason explains provider-level intentional skips. A provider skip is an
// explicit source-level outcome, not a nil parse result. Callers may record a
// clean skip-cache entry only when the skip is complete, non-erroring, and keyed
// by the provider fingerprint/source identity.
type SkipReason uint8

const (
	SkipNone SkipReason = iota
	SkipNoSession
	SkipUnsupportedSource
	SkipNonInteractive
	SkipShadowedBySidecar
)

// IncrementalRequest is the append-only parse input.
type IncrementalRequest struct {
	Source       SourceRef
	Fingerprint  SourceFingerprint
	SessionID    string
	Offset       int64
	StartOrdinal int
	Machine      string
	// LastEntryUUID is the UUID of the last entry stored for this
	// session, used by DAG-aware parsers (Claude) to detect when an
	// appended tail forks away from the stored tip and must trigger a
	// full reparse instead of a naive append.
	LastEntryUUID string
}

// IncrementalOutcome is the append-only parse output.
type IncrementalOutcome struct {
	SessionID            string
	Messages             []ParsedMessage
	EndedAt              time.Time
	ConsumedBytes        int64
	MessageCount         int
	UserMessageCount     int
	TotalOutputTokens    int
	PeakContextTokens    int
	HasTotalOutputTokens bool
	HasPeakContextTokens bool
	ForceReplace         bool
}

// IncrementalStatus describes how an incremental parse attempt should proceed.
type IncrementalStatus uint8

const (
	IncrementalUnsupported IncrementalStatus = iota
	IncrementalNoNewData
	IncrementalApplied
	IncrementalNeedsFullParse
)

// ProviderFactories returns one provider factory for every registered agent.
func ProviderFactories() []ProviderFactory {
	factories := make([]ProviderFactory, 0, len(Registry))
	for _, def := range Registry {
		factories = append(factories, providerFactoryForDef(def))
	}
	return factories
}

func providerFactoryForDef(def AgentDef) ProviderFactory {
	def = cloneAgentDef(def)
	switch def.Type {
	case AgentAntigravity:
		return newAntigravityProviderFactory(def)
	case AgentAntigravityCLI:
		return newAntigravityCLIProviderFactory(def)
	case AgentAider:
		return newAiderProviderFactory(def)
	case AgentAmp:
		return newAmpProviderFactory(def)
	case AgentClaude:
		return newClaudeProviderFactory(def)
	case AgentClaudeAI:
		return newImportOnlyProviderFactory(def)
	case AgentCommandCode:
		return newCommandCodeProviderFactory(def)
	case AgentCodex:
		return newCodexProviderFactory(def)
	case AgentCopilot:
		return newCopilotProviderFactory(def)
	case AgentCowork:
		return newCoworkProviderFactory(def)
	case AgentCortex:
		return newCortexProviderFactory(def)
	case AgentCursor:
		return newCursorProviderFactory(def)
	case AgentChatGPT:
		return newImportOnlyProviderFactory(def)
	case AgentDeepSeekTUI:
		return newDeepSeekTUIProviderFactory(def)
	case AgentForge:
		return newForgeProviderFactory(def)
	case AgentHermes:
		return newHermesProviderFactory(def)
	case AgentIflow:
		return newIflowProviderFactory(def)
	case AgentGptme:
		return newGptmeProviderFactory(def)
	case AgentGemini:
		return newGeminiProviderFactory(def)
	case AgentKimi:
		return newKimiProviderFactory(def)
	case AgentKiro:
		return newKiroProviderFactory(def)
	case AgentKiroIDE:
		return newKiroIDEProviderFactory(def)
	case AgentKilo:
		return newKiloProviderFactory(def)
	case AgentMiMoCode:
		return newMiMoCodeProviderFactory(def)
	case AgentOpenHands:
		return newOpenHandsProviderFactory(def)
	case AgentOpenCode:
		return newOpenCodeProviderFactory(def)
	case AgentOMP:
		return newPiProviderFactory(def)
	case AgentOpenClaw:
		return newOpenClawProviderFactory(def)
	case AgentPiebald:
		return newPiebaldProviderFactory(def)
	case AgentPi:
		return newPiProviderFactory(def)
	case AgentPositron:
		return newPositronProviderFactory(def)
	case AgentQClaw:
		return newQClawProviderFactory(def)
	case AgentQwen:
		return newQwenProviderFactory(def)
	case AgentQwenPaw:
		return newQwenPawProviderFactory(def)
	case AgentReasonix:
		return newReasonixProviderFactory(def)
	case AgentShelley:
		return newShelleyProviderFactory(def)
	case AgentVSCopilot:
		return newVisualStudioCopilotProviderFactory(def)
	case AgentVSCodeCopilot:
		return newVSCodeCopilotProviderFactory(def)
	case AgentVibe:
		return newVibeProviderFactory(def)
	case AgentWarp:
		return newWarpProviderFactory(def)
	case AgentWorkBuddy:
		return newWorkBuddyProviderFactory(def)
	case AgentZencoder:
		return newZencoderProviderFactory(def)
	case AgentZed:
		return newZedProviderFactory(def)
	default:
		panic("missing provider factory for " + string(def.Type))
	}
}

// ProviderFactoryByType returns the factory for an agent type.
func ProviderFactoryByType(t AgentType) (ProviderFactory, bool) {
	for _, factory := range ProviderFactories() {
		if factory.Definition().Type == t {
			return factory, true
		}
	}
	return nil, false
}

// ProviderSupportsSourceDiscovery reports whether the registered provider can
// enumerate source references for report-only or sync discovery surfaces.
func ProviderSupportsSourceDiscovery(t AgentType) bool {
	factory, ok := ProviderFactoryByType(t)
	if !ok {
		return false
	}
	return factory.Capabilities().Source.DiscoverSources ==
		CapabilitySupported
}

// NewProvider constructs a config-bound provider for an agent type.
func NewProvider(t AgentType, cfg ProviderConfig) (Provider, bool) {
	factory, ok := ProviderFactoryByType(t)
	if !ok {
		return nil, false
	}
	return factory.NewProvider(cfg), true
}

func cloneAgentDef(def AgentDef) AgentDef {
	def.DefaultDirs = append([]string(nil), def.DefaultDirs...)
	def.WatchSubdirs = append([]string(nil), def.WatchSubdirs...)
	return def
}

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

// Provider is the target parser/source facade. Providers own source shape and
// return normalized parser results for the sync engine to persist.
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

// SourceRef is the engine-visible handle for provider-owned source data.
type SourceRef struct {
	// Provider identifies the provider that created this source and must match
	// the provider instance used for subsequent operations.
	Provider AgentType
	// Key is stable within the provider across process restarts. It is suitable
	// for dedupe and diagnostics, but not necessarily for DB freshness checks.
	Key string
	// DisplayPath is human-readable and may be a virtual path.
	DisplayPath string
	// FingerprintKey is the persisted lookup key for skip-cache and parser data
	// version checks. Migrated providers should keep it compatible with legacy
	// file_path values whenever practical.
	FingerprintKey string
	// ProjectHint is advisory metadata for UI grouping and may be empty.
	ProjectHint string
	// Opaque is provider-owned in-memory state. The engine must not persist,
	// compare, inspect, or log it, and providers must not require it for lookup
	// from persisted rows.
	Opaque any
}

// WatchPlan describes provider-owned filesystem watch roots.
type WatchPlan struct {
	Roots []WatchRoot
}

// WatchRoot is one filesystem root the engine should watch.
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
	WatchRoot string
	// StoredSourcePaths are optional provider-persisted source paths already
	// known to the caller for this watch root. Providers that model a shared
	// physical file as virtual per-session sources use these to emit tombstone
	// sources when a DB row or DB file has disappeared and can no longer be
	// rediscovered from current metadata.
	StoredSourcePaths []string
}

// FindSourceRequest contains persisted source hints for provider-owned lookup.
type FindSourceRequest struct {
	RawSessionID       string
	FullSessionID      string
	StoredFilePath     string
	FingerprintKey     string
	RequireFreshSource bool
	PreferStoredSource bool
}

// SourceFingerprint is the provider-normalized source freshness identity.
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
// Provider.Parse returns a nil error.
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
// data version.
type DataVersionState uint8

const (
	DataVersionUnspecified DataVersionState = iota
	DataVersionCurrent
	DataVersionNeedsRetry
)

// SkipReason explains provider-level intentional skips.
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

type legacyProviderFactory struct {
	def AgentDef
}

func (f legacyProviderFactory) Definition() AgentDef {
	return cloneAgentDef(f.def)
}

func (f legacyProviderFactory) Capabilities() Capabilities {
	return Capabilities{}
}

func (f legacyProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	return &legacyProvider{
		ProviderBase: ProviderBase{
			Def:    cloneAgentDef(f.def),
			Config: cfg.Clone(),
		},
	}
}

type legacyProvider struct {
	ProviderBase
}

func (p *legacyProvider) Parse(context.Context, ParseRequest) (ParseOutcome, error) {
	return ParseOutcome{}, p.unsupported(ProviderFeatureParse)
}

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
	case AgentAmp:
		return newAmpProviderFactory(def)
	case AgentCommandCode:
		return newCommandCodeProviderFactory(def)
	case AgentDeepSeekTUI:
		return newDeepSeekTUIProviderFactory(def)
	case AgentIflow:
		return newIflowProviderFactory(def)
	case AgentGptme:
		return newGptmeProviderFactory(def)
	case AgentZencoder:
		return newZencoderProviderFactory(def)
	default:
		return legacyProviderFactory{def: def}
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

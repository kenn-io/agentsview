package parser

//go:generate go run github.com/dmarkham/enumer -type=CapabilitySupport -json -text -transform=snake -trimprefix=Capability -output=capabilitysupport_enumer.go

// CapabilitySupport describes whether a provider implements or can represent a
// source or content feature. The zero value is unsupported.
type CapabilitySupport uint8

const (
	CapabilityUnsupported CapabilitySupport = iota
	CapabilitySupported
	CapabilityNotApplicable
)

// Capabilities groups provider source mechanics and parsed-content features.
// Capabilities are declarative: a concrete provider that reports Supported must
// implement the matching behavior rather than relying on ProviderBase defaults.
// Callers may still invoke optional methods and handle their no-op or typed
// unsupported results, but scheduling and validation should trust this
// declaration once a provider has migrated off the legacy adapter.
type Capabilities struct {
	Source  SourceCapabilities
	Content ContentCapabilities
	Sync    ProviderSyncSemantics
}

// UnchangedResultPolicy controls how the engine compares parsed members from a
// shared source with their stored rows. The zero value keeps every result.
type UnchangedResultPolicy uint8

const (
	UnchangedResultNone UnchangedResultPolicy = iota
	UnchangedResultMTime
	UnchangedResultMTimeAndHash
)

// ProviderSyncSemantics declares stable provider-wide cache and result
// freshness policy. Its zero value opts out of every specialized behavior.
type ProviderSyncSemantics struct {
	FingerprintHashInCacheKey           bool
	FingerprintHashRequiredForFreshness bool
	// SkipCacheFreshWithoutStoredRow permits a matching skip-cache entry to
	// remain fresh before this provider has persisted a row for the source.
	SkipCacheFreshWithoutStoredRow bool
	UnchangedResults               UnchangedResultPolicy
}

// SourceCapabilities declares optional source mechanics implemented by a
// provider.
type SourceCapabilities struct {
	DiscoverSources    CapabilitySupport
	StreamingDiscovery CapabilitySupport
	WatchSources       CapabilitySupport
	WatchRoots         CapabilitySupport
	ActivityHints      CapabilitySupport
	// BoundedCoverage means the provider can construct a bounded journal
	// change feed for its shared SQLite container, so recurring work is
	// proportional to the changed batch rather than the archive. The feed
	// is constructed per coverage unit; providers that do not declare this
	// keep their existing provider-scoped discovery strategy.
	BoundedCoverage      CapabilitySupport
	ClassifyChangedPath  CapabilitySupport
	ChangedPathRelevance CapabilitySupport
	StoredSourceHints    CapabilitySupport
	FindSource           CapabilitySupport
	CompositeFingerprint CapabilitySupport
	IncrementalAppend    CapabilitySupport
	MultiSessionSource   CapabilitySupport
	// SharedContainerSource means streaming discovery emits multiple logical
	// virtual members from one physical container and exact reconciliation
	// rehydration is required to avoid reopening or rescanning that container.
	SharedContainerSource CapabilitySupport
	PerSessionErrors      CapabilitySupport
	ExcludedSessions      CapabilitySupport
	ForceReplaceOnParse   CapabilitySupport
	VerifiedLocalStat     CapabilitySupport
	// PersistentArchive means a vanished physical container must not tombstone
	// its stored virtual members. A still-present container remains
	// authoritative for member deletion.
	PersistentArchive CapabilitySupport
}

// ContentCapabilities declares optional normalized content fields a provider
// may emit.
type ContentCapabilities struct {
	FirstMessage         CapabilitySupport
	SessionName          CapabilitySupport
	Cwd                  CapabilitySupport
	GitBranch            CapabilitySupport
	Relationships        CapabilitySupport
	Subagents            CapabilitySupport
	Thinking             CapabilitySupport
	ToolCalls            CapabilitySupport
	ToolResults          CapabilitySupport
	ToolResultEvents     CapabilitySupport
	PerMessageTokenUsage CapabilitySupport
	AggregateUsageEvents CapabilitySupport
	TerminationStatus    CapabilitySupport
	MalformedLineCount   CapabilitySupport
	TruncationStatus     CapabilitySupport
	Model                CapabilitySupport
	StopReason           CapabilitySupport
}

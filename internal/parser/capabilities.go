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
	DiscoverSources      CapabilitySupport
	StreamingDiscovery   CapabilitySupport
	WatchSources         CapabilitySupport
	WatchRoots           CapabilitySupport
	ActivityHints        CapabilitySupport
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
	// MultiFileStatHash declares that a provider's on-disk source layout
	// spans multiple sibling files (Codebuff's chat-messages.json plus
	// run-state.json and chat-meta.json, for example) and that the engine
	// should consult its per-component provider_freshness digest on
	// warm passes instead of the legacy size/max-mtime composite
	// freshness gate. Providers that do not have a multi-file layout
	// leave this at CapabilityUnsupported; a SourceSetProvider wrapper
	// still satisfies parser.MultiFileStatHasher unconditionally, so
	// the engine reads this capability before registering a hasher
	// in providerStatHashers to avoid short-circuiting every
	// SourceSet-wrapped agent on a 0==0 digest match.
	MultiFileStatHash CapabilitySupport
	// S3Discovery means the provider can enumerate and ingest sessions
	// from an s3:// root under .../<machine>/raw/<agent>/ and implements
	// S3Provider. Providers leave this at CapabilityUnsupported unless
	// they opt in; the sync engine uses the flag instead of a hardcoded
	// Claude/Codex whitelist.
	S3Discovery CapabilitySupport
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

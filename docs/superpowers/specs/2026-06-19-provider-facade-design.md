# Provider Facade Design

## Purpose

agentsview supports many agent formats, but parser integration is currently
spread across `parser.AgentDef`, parser-specific discovery functions, parser
function signatures, and a large `sync.Engine` switch. Adding a provider often
means touching several unrelated areas and relying on convention for optional
features such as tool calls, usage events, termination status, source mtime, and
incremental parsing.

This design adds a shared provider facade so adding or migrating a provider
means implementing one contract. The facade keeps provider source shape
internal, while the sync engine consumes normalized source identities,
fingerprints, and `ParseResult` values.

## Goals

- Migrate every existing provider to the facade, not only future providers.
- Keep both runtime shapes available at the root of the stack so legacy parsing
  stays authoritative while provider parsing is shadow-compared provider by
  provider.
- Make every provider PR an actual migration step: adding a provider
  implementation must also opt that provider into the shared migration
  manifest and provider-vs-legacy parity tests.
- Keep `ParsedSession`, `ParsedMessage`, `ParsedToolCall`, `ParsedToolResult`,
  `ParsedUsageEvent`, and `ParseResult` as the normalized output contract.
- Remove the provider-by-provider `sync.Engine.processFile` dispatch switch only
  at the tip of the stack, after every parse-capable provider has passed the
  shadow comparison path.
- Make source discovery, source lookup, watch planning, fingerprinting, parsing,
  and optional incremental parsing provider-owned.
- Provide reusable provider helpers for common source layouts, especially JSONL
  file discovery.
- Make optional parsed features auditable through a concrete `Capabilities`
  struct.
- Preserve current SQLite schema, parse-diff semantics, skip-cache behavior, and
  parser output parity.

## Non-Goals

- Rewrite individual provider parsers from scratch.
- Change the persistent database schema as part of this refactor.
- Move DB writes into providers.
- Make source storage shape a global engine concern.
- Turn all providers into JSONL providers. JSONL helpers are shared utilities,
  not the abstraction boundary.

## Design Constraints

The provider facade must respect these constraints:

- Source shape belongs to the provider. The engine must not know whether a
  source is a JSONL file, SQLite row, sidecar, trace folder, import archive,
  or multiple files.
- Providers embed a base facade with zero-value no-op implementations for
  optional source behavior.
- Providers must implement `Parse`; the base facade must not provide a fake
  parse implementation.
- Capabilities use a concrete struct. The zero value of every capability field
  is unsupported.
- Capability enum string and JSON methods should be generated with
  `dmarkham/enumer`, because it supports generated `String`, JSON, and text
  marshal methods from one enum definition.
- All existing providers migrate to the new layer before the old sync dispatch
  is considered removed.
- During the stacked migration, legacy dispatch remains the writer. Provider
  dispatch is run through the same root-level harness for opted-in providers
  and compared against legacy output, but it must not mutate persisted session
  state until the stack tip switches authority.
- A provider branch is incomplete if it only adds provider code. It must also
  move the provider's migration status out of legacy-only mode and include the
  dual-run test coverage that proves the new shape is exercised.

## Core Types

The provider contract should live near the parser boundary, for example in
`internal/parser/provider.go`, because it works with parser-owned normalized
types and agent metadata.

```go
type ProviderFactory interface {
	Definition() AgentDef
	Capabilities() Capabilities
	NewProvider(ProviderConfig) Provider
}

type ProviderConfig struct {
	Roots   []string
	Machine string
}

func (cfg ProviderConfig) Clone() ProviderConfig {
	cfg.Roots = append([]string(nil), cfg.Roots...)
	return cfg
}

func (cfg ProviderConfig) RootsCopy() []string {
	return append([]string(nil), cfg.Roots...)
}

type Provider interface {
	Definition() AgentDef
	Capabilities() Capabilities

	Discover(context.Context) ([]SourceRef, error)
	WatchPlan(context.Context) (WatchPlan, error)
	SourcesForChangedPath(context.Context, ChangedPathRequest) ([]SourceRef, error)
	FindSource(context.Context, FindSourceRequest) (SourceRef, bool, error)
	Fingerprint(context.Context, SourceRef) (SourceFingerprint, error)

	Parse(context.Context, ParseRequest) (ParseOutcome, error)
	ParseIncremental(
		context.Context,
		IncrementalRequest,
	) (IncrementalOutcome, IncrementalStatus, error)
}
```

`ProviderFactory` is the registry surface. `Provider` is a config-bound instance
created by `NewProvider` for one engine, with that engine's configured roots and
machine. `NewProvider` implementations must clone `ProviderConfig` before
storing it. Every retained owner of root slices must get its own copy: one for
`ProviderBase.Config`, separate copies for source helpers, and separate copies
inside helper constructors that retain roots. This keeps later caller mutation
and helper-local normalization from changing another component's view of roots.
If `ProviderConfig` later gains map, slice, or pointer fields, `Clone` must be
updated to preserve the same snapshot invariant. This keeps changed-path
classification root-aware without requiring mutable singleton providers or
passing raw roots through every engine call.

`ProviderBase` implements every optional source method with safe zero-value
no-op behavior. It does not implement `Parse`, so a concrete provider cannot
satisfy `Provider` without a real parser entry point.

```go
type ProviderBase struct {
	Def    AgentDef
	Caps   Capabilities
	Config ProviderConfig
}

var ErrUnsupportedProviderFeature = errors.New("unsupported provider feature")

type UnsupportedProviderFeatureError struct {
	Provider AgentType
	Feature  string
}

func (err UnsupportedProviderFeatureError) Error() string {
	return string(err.Provider) + ": unsupported provider feature " + err.Feature
}

func (err UnsupportedProviderFeatureError) Unwrap() error {
	return ErrUnsupportedProviderFeature
}
```

`ProviderBase` provides `Definition`, `Capabilities`, empty discovery, empty
watch plans, no changed-path classification, no source lookup, unsupported
fingerprints, and `(IncrementalOutcome{}, IncrementalUnsupported, nil)` for
incremental parsing. Unsupported methods that report an error use
`ErrUnsupportedProviderFeature`, so callers can distinguish "feature is absent"
from I/O, database, or parser failures with `errors.Is`. That keeps the engine
call surface uniform: every provider can be called through the full `Provider`
interface without feature-specific nil checks.

Unsupported defaults are still contract checks, not fallback policy. Any
provider that returns a `SourceRef` from discovery, changed-path classification,
lookup, or another provider source path must implement `Fingerprint` for that
reference. The engine treats unsupported fingerprinting for a returned source as
a provider contract failure; it must not silently use a zero fingerprint, mark
the source clean, or downgrade to an unspecified full parse path.

Reusable source helpers must not be generic indirection interfaces or provider
base classes. They are plain source-set structs such as `JSONLSourceSet`,
`DirectoryJSONLSourceSet`, or `SQLiteFanoutSourceSet`. A provider keeps a helper
as a named field and forwards the source methods it supports. That forwarding is
deliberate: it makes the provider's optional behavior visible at the concrete
type without adding another abstraction layer.

Every provider should include a compile-time assertion:

```go
var _ Provider = (*CodexProvider)(nil)
```

Embedding and delegation examples must be compile-tested as part of the provider
harness, so the documented pattern cannot drift into impossible Go.

## Provider Lifecycle And Concurrency

`NewProvider` returns a config-bound provider instance for one sync engine.
Provider instances are long-lived enough to serve full sync, live watch sync,
source lookup, diagnostics, export, and parse-diff calls for that engine.

Provider methods must be safe for concurrent calls. Implementations should keep
configuration and source helper fields immutable after construction. Any cache,
lazy initialization, database handle, or source index stored on the provider
must use normal Go synchronization or be confined to one method call. Providers
must honor `context.Context` cancellation for filesystem, database, and parser
work that can block.

The engine may pass `SourceRef` values between goroutines and queue them for
later work against the same provider instance. `SourceRef.Opaque` therefore must
be immutable, safe to read concurrently, and valid for the life of the provider
instance. It must not contain unguarded mutable state or open handles that
require engine cleanup. When a source needs a handle, the provider should store
stable keys in `Opaque` and open or manage the handle inside provider methods.

## Embedding Pattern

The intended implementation pattern is: embed `ProviderBase`, keep source
helpers as named fields, and implement explicit forwarding methods for the
source behaviors the provider supports. `ProviderBase` keeps every optional
method callable; provider methods override only the useful defaults.

```go
type CodexProvider struct {
	ProviderBase
	sources SiblingMetadataSourceSet
}

func NewCodexProvider(cfg ProviderConfig) *CodexProvider {
	config := cfg.Clone()
	sourceRoots := config.RootsCopy()
	return &CodexProvider{
		ProviderBase: ProviderBase{
			Def:    codexAgentDef(),
			Caps:   codexCapabilities(),
			Config: config,
		},
		sources: SiblingMetadataSourceSet{
			Base: JSONLSourceSet{
				Agent:      AgentCodex,
				Roots:      sourceRoots,
				Extensions: []string{".jsonl"},
				Recursive:  true,
			},
			MetadataFiles: []string{CodexSessionIndexFilename},
		},
	}
}

func (p *CodexProvider) Discover(
	ctx context.Context,
) ([]SourceRef, error) {
	return p.sources.Discover(ctx)
}

func (p *CodexProvider) WatchPlan(
	ctx context.Context,
) (WatchPlan, error) {
	return p.sources.WatchPlan(ctx)
}

func (p *CodexProvider) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	return p.sources.SourcesForChangedPath(ctx, req)
}

func (p *CodexProvider) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	return p.sources.FindSource(ctx, req)
}

func (p *CodexProvider) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	return p.sources.Fingerprint(ctx, source)
}

func (p *CodexProvider) Parse(
	ctx context.Context,
	req ParseRequest,
) (ParseOutcome, error) {
	sess, msgs, err := ParseCodexSession(
		req.Source.DisplayPath,
		req.Machine,
		false,
	)
	if err != nil || sess == nil {
		return ParseOutcome{}, err
	}
	return ParseOutcome{
		Results: []ParseResultOutcome{{
			Result:      ParseResult{Session: *sess, Messages: msgs},
			DataVersion: DataVersionCurrent,
		}},
		ResultSetComplete: true,
	}, nil
}
```

For a simple JSONL provider, the same pattern uses a different source-set field:

```go
type QwenProvider struct {
	ProviderBase
	sources DirectoryJSONLSourceSet
}

func NewQwenProvider(cfg ProviderConfig) *QwenProvider {
	config := cfg.Clone()
	sourceRoots := config.RootsCopy()
	return &QwenProvider{
		ProviderBase: ProviderBase{
			Def:    qwenAgentDef(),
			Caps:   qwenCapabilities(),
			Config: config,
		},
		sources: DirectoryJSONLSourceSet{
			JSONLSourceSet: JSONLSourceSet{
				Agent:      AgentQwen,
				Roots:      sourceRoots,
				Extensions: []string{".jsonl"},
				Recursive:  true,
			},
			ProjectFromPath: qwenProjectFromPath,
		},
	}
}
```

Providers that need one-off source behavior still embed `ProviderBase` and write
only the concrete methods they need:

```go
type VisualStudioCopilotProvider struct {
	ProviderBase
	traces VisualStudioTraceSourceSet
}

func (p *VisualStudioCopilotProvider) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	return p.traces.StrictFingerprint(ctx, source)
}
```

The rule is: embed `ProviderBase` once, compose source helpers as named fields,
and forward intentionally. Do not embed source helpers beside `ProviderBase`
when they define the same optional methods as the base; same-depth promoted
selectors will not give the intended override. Source behavior stays explicit:
there is no generic source-behavior interface and no runtime method table hidden
in the base.

## Source References

`SourceRef` is the engine-visible handle for provider-owned source data.

```go
type SourceRef struct {
	Provider       AgentType
	Key            string
	DisplayPath    string
	FingerprintKey string
	ProjectHint    string

	// Provider-owned payload. The sync engine passes this back to the
	// same provider and must not inspect it.
	Opaque any
}
```

Rules:

- `Key` is stable within the provider and suitable for logs and dedupe.
- `DisplayPath` is human-readable and may be a virtual path.
- `FingerprintKey` is the DB lookup key used for skip/data-version checks. With
  the no-schema-change migration, this is the authoritative persisted source
  identity and is written through `ParsedSession.File.Path` /
  `sessions.file_path`. `SourceFingerprint.Key` must either equal the selected
  persisted identity or be empty so the engine falls back to
  `SourceRef.FingerprintKey` and `SourceRef.Key`; there is no separate
  fingerprint-key column.
- `ProjectHint` is advisory and can be empty.
- `Opaque` is internal provider state. The engine treats it as an opaque token.
- `Opaque` is never persisted or logged, must be immutable for engine callers,
  and must remain usable when `SourceRef` is queued across goroutines for the
  lifetime of the provider instance.

Backwards compatibility:

- Migrated providers should keep `FingerprintKey` compatible with the source key
  or stored `file_path` values already written by the legacy sync path
  whenever practical. Existing fingerprint and data-version metadata should
  continue to short-circuit unchanged sources after the facade migration.
- If a provider must change its lookup key or fingerprint identity, that
  provider migration must explicitly document the expected full resync or
  metadata transition. The facade migration itself must not silently force all
  providers through a full resync.
- Diagnostics, parse errors, and logs should surface stable fields such as
  `Provider`, `Key`, `DisplayPath`, and `FingerprintKey`. `Opaque` is never
  persisted or logged because it may contain provider-internal implementation
  details that are not stable across releases.

Watch and changed-path classification use provider-owned root metadata:

```go
type WatchPlan struct {
	Roots []WatchRoot
}

type WatchRoot struct {
	Path         string
	Recursive    bool
	IncludeGlobs []string
	ExcludeGlobs []string
	DebounceKey  string
}

type ChangedPathRequest struct {
	Path              string
	EventKind         string
	WatchRoot         string
	StoredSourcePaths []string
}
```

`WatchRoot.Path` is the actual filesystem root the engine should watch.
`Recursive` controls watcher depth. Include and exclude globs are advisory
provider filters that allow broad OS watch roots without parsing every changed
file. `DebounceKey` groups related paths such as sibling metadata files and a
transcript. `ChangedPathRequest.WatchRoot` is the matched watch root, so the
provider can classify changes relative to the configured root that produced
them. `StoredSourcePaths` is the persisted `sessions.file_path` hint set scoped
to the matched watch root and provider. Providers with virtual, database-backed,
or multi-file sources use it to classify deletion and tombstone events that no
longer have a regular source file on disk.

The provider owns the final changed-path decision. The engine may use
`IncludeGlobs` and `ExcludeGlobs` as coarse prefilters because the provider
supplied them, but `SourcesForChangedPath` must still tolerate unfiltered events
and apply authoritative provider-specific classification. Diagnostics should
report whether the provider accepted, ignored, or rejected a changed path.

`FindSource` replaces the current `FindSourceFunc` fallback model. It must cover
file-backed and database-backed providers because `FindSourceFile`,
`SourceMtime`, token usage commands, session watch, and export flows all need
provider-specific source lookup today.

```go
type FindSourceRequest struct {
	RawSessionID       string
	FullSessionID      string
	StoredFilePath     string
	FingerprintKey     string
	RequireFreshSource bool
}
```

Stored DB `file_path` values are advisory compatibility keys. The engine passes
them through `FindSourceRequest`, but the provider decides whether they are real
paths, virtual paths, row keys, or obsolete source hints. The engine must not
`stat`, split, glob, or otherwise interpret a stored path before asking the
provider to resolve it.

`RequireFreshSource` means the caller needs a source reference the provider has
verified against current source state. The provider may use stored metadata as a
hint, but it must confirm the current file, database row, import record, or
virtual source can still be read or fingerprinted. Filesystem providers usually
check the current path; database and virtual providers can satisfy this by
resolving the current logical record. If the source no longer exists, the
provider returns `(SourceRef{}, false, nil)`. If the source might exist but
cannot be checked because of an I/O or database failure, it returns an error.
When `RequireFreshSource` is false, the provider may return the best
compatibility match for display/export flows that can tolerate stale source
hints.

## Fingerprints

The provider owns fingerprint calculation because source freshness can depend on
composite state:

- transcript files plus sibling metadata;
- SQLite database file mtimes;
- virtual paths for one logical session inside a database;
- sidecar files that supersede encrypted or summary sources;
- trace folders containing related files.

```go
type SourceFingerprint struct {
	Key     string
	Size    int64
	MTimeNS int64
	Inode   uint64
	Device  uint64
	Hash    string
}
```

The engine uses fingerprints for generic skip/data-version checks and stores the
same normalized source file metadata it stores today. Hashes remain optional
where they are expensive or not meaningful.

Fingerprinting must stay cheap enough for the sync hot path. Acceptance tests or
benchmarks should use representative large roots and composite sources with
concrete pass criteria:

- no full-root content hashing during unchanged sync;
- no recursive directory walk for every source when discovery already produced
  the source list;
- file-backed fingerprints are bounded by the source plus declared sibling
  metadata files;
- database fan-out fingerprints reuse database-level metadata plus row/session
  identifiers instead of scanning unrelated rows;
- any provider that requires a full content hash documents why mtime, size,
  inode/device, row metadata, or sidecar metadata are insufficient and
  includes a benchmark budget for that provider.

## Parse Requests And Outcomes

```go
type ParseRequest struct {
	Source      SourceRef
	Fingerprint SourceFingerprint
	Machine     string
	ForceParse  bool
}

type ParseOutcome struct {
	Results            []ParseResultOutcome
	ExcludedSessionIDs []string
	SourceErrors       []SourceError
	ResultSetComplete  bool
	ForceReplace       bool
	SkipReason         SkipReason
}

type ParseResultOutcome struct {
	Result      ParseResult
	DataVersion DataVersionState
	RetryReason string
}

type SourceError struct {
	SourceKey   string
	DisplayPath string
	SessionID   string
	Err         error
	Retryable   bool
}

type DataVersionState uint8

const (
	DataVersionUnspecified DataVersionState = iota
	DataVersionCurrent
	DataVersionNeedsRetry
)

type SkipReason uint8

const (
	SkipNone SkipReason = iota
	SkipNoSession
	SkipUnsupportedSource
	SkipNonInteractive
	SkipShadowedBySidecar
)
```

Runtime behavior:

- Whole-source parse failures return `error`.
- Multi-session providers return one `ParseResultOutcome` per successfully
  parsed session and `SourceErrors` for per-session failures, so good sessions
  can still be ingested.
- All session IDs in parse outcomes use the persisted normalized/full session ID
  namespace. That includes `ParseResultOutcome.Result.Session.ID`,
  `ExcludedSessionIDs`, and `SourceError.SessionID`. Raw upstream IDs may
  appear in provider internals, diagnostics, or lookup requests, but the
  engine compares outcome IDs only against persisted full session IDs.
- `SourceError.SessionID` is required for per-session failures from
  multi-session providers. `SourceKey` and `DisplayPath` are diagnostic source
  identifiers, not substitutes for persisted session identity. If the provider
  cannot isolate a failure to a persisted full session ID, it must return a
  whole-source `error` instead of a `SourceError`.
- `Retryable` decides whether a failure can be cached by mtime.
- `ForceReplace` is the generic signal for full parses that must rewrite
  existing ordinals.
- `DataVersionCurrent` means the corresponding successful result represents the
  current parser data version for that session.
- `DataVersionNeedsRetry` means successful fallback results may be written, but
  the session remains eligible for a future parse at the current data version.
  The engine must not persist a current data-version marker for that session.
  `RetryReason` records why, for example an Antigravity-style lower-resolution
  fallback.
- `DataVersionUnspecified` is allowed only during migration adapters; provider
  harness tests should require new providers to set either
  `DataVersionCurrent` or `DataVersionNeedsRetry` for every returned result.
- Mixed data-version states are valid for multi-session sources. One result can
  be current while another result from the same source needs retry, and a
  retryable `SourceError` affects only the failed session unless the provider
  reports a whole-source `error`.
- Data-version writes are per result, and successful unchanged-source freshness
  remains DB metadata driven through source identity, file size, effective
  mtime, and parser data version. `skipped_files` must not become a clean
  successful-parse cache unless it also stores data-version-aware freshness
  state; with the current no-schema-change migration it remains a
  retry/failure and explicit skip cache.
- `ResultSetComplete` means the provider has accounted for the complete logical
  session set represented by the `SourceRef`/`FingerprintKey`: returned
  results, explicit exclusions, and clean replacements cover every retained
  session for that source. The engine may use that proof to avoid stale rows
  and to clear retry/failure cache entries, but not to persist a
  data-version-blind clean source skip.
- Any `DataVersionNeedsRetry` result, retryable per-session error, non-retryable
  per-session error, or incomplete result set prevents the provider outcome
  from being treated as a clean complete source. Non-retryable errors may be
  recorded as diagnostics or failure-cache entries, but they do not prove the
  source is clean because a future parser version or source change may still
  need to revisit the same logical session set.
- During a partial multi-session parse, existing persisted rows that are absent
  from `Results` are retained unless their IDs are listed in
  `ExcludedSessionIDs` or the provider completes a clean `ForceReplace` parse
  for the owning logical source. A retryable `SourceError` leaves that
  session's existing row stale and eligible for a future retry instead of
  deleting it or marking it current.
- `SkipReason` replaces implicit "nil session means skip" behavior. Skips are
  explicit outcomes and should not be conflated with retryable parse failures.
- Provider cache identity and `ResultSetComplete` semantics are root hook
  invariants, not per-provider policy. Provider branches may add source-family
  coverage, but they must not redefine cache keys, omission/deletion behavior,
  or retry-state persistence.
- When a migrated provider's fingerprint key differs from the legacy
  `file.Path`, the provider path reads, writes, and clears only the provider
  fingerprint key. Old legacy skip-cache entries may remain in the persisted
  archive as inert compatibility leftovers; they must not be consulted for a
  provider-authoritative source once its provider key is known.
- Providers do not write to the DB.
- Providers do not mutate, delete, or repair source files.

Skip reason semantics:

- `SkipNone`: the provider did not skip the source.
- `SkipNoSession`: the source is valid for the provider but does not contain a
  session after parsing.
- `SkipUnsupportedSource`: the source was discovered by broad matching but is
  not a supported source shape for this provider.
- `SkipNonInteractive`: the source is intentionally excluded because it is not a
  user-facing session.
- `SkipShadowedBySidecar`: a sibling or replacement source supersedes this
  source. The replacement write path should use `ForceReplace` when existing
  rows need to be rewritten.

Unchanged-source skips remain engine skip-cache decisions based on provider
fingerprints. They are not returned as `ParseOutcome` values.

## Incremental Parsing

Incremental parsing is optional provider behavior.

```go
type IncrementalRequest struct {
	Source       SourceRef
	Fingerprint  SourceFingerprint
	SessionID    string
	Offset       int64
	StartOrdinal int
	Machine      string
}

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

type IncrementalStatus uint8

const (
	IncrementalUnsupported IncrementalStatus = iota
	IncrementalNoNewData
	IncrementalApplied
	IncrementalNeedsFullParse
)
```

`ProviderBase.ParseIncremental` returns
`(IncrementalOutcome{}, IncrementalUnsupported, nil)`. Providers that support
append-only incremental parsing set the relevant source capability and implement
the method.

Incremental status semantics:

- `IncrementalUnsupported`: this provider does not implement incremental append
  for the requested source; the engine should run the normal full parse path.
- `IncrementalNoNewData`: the source is valid and no append was needed; the
  engine may update freshness metadata without writing messages.
- `IncrementalApplied`: `IncrementalOutcome` contains appended messages and
  counters that should be written.
- `IncrementalNeedsFullParse`: the provider inspected the source but cannot
  safely append; the engine must run the normal full parse path.

Errors from `ParseIncremental` are real failures for the attempted incremental
operation. The engine uses `IncrementalStatus`, not provider-specific error
strings or a bare boolean, to decide whether to fall back to full parsing.
`IncrementalRequest.SessionID` and `IncrementalOutcome.SessionID` use the same
persisted full session ID namespace as normal parse outcomes.

## Capabilities

Capabilities use a concrete struct and an iota enum. The zero value maps to
unsupported.

```go
//go:generate go run github.com/dmarkham/enumer -type=CapabilitySupport -json -text -transform=snake -trimprefix=Capability -output=capabilitysupport_enumer.go

type CapabilitySupport uint8

const (
	CapabilityUnsupported CapabilitySupport = iota
	CapabilitySupported
	CapabilityNotApplicable
)
```

`enumer` is preferred over plain `stringer` here because it can generate
`String`, JSON marshal/unmarshal, text marshal/unmarshal, value listing, and
validation helpers from the same enum definition.

The struct should group source mechanics and parsed-content features:

```go
type Capabilities struct {
	Source  SourceCapabilities
	Content ContentCapabilities
}

type SourceCapabilities struct {
	DiscoverSources       CapabilitySupport
	WatchSources          CapabilitySupport
	ClassifyChangedPath   CapabilitySupport
	FindSource            CapabilitySupport
	CompositeFingerprint  CapabilitySupport
	IncrementalAppend     CapabilitySupport
	MultiSessionSource    CapabilitySupport
	PerSessionErrors      CapabilitySupport
	ExcludedSessions      CapabilitySupport
	ForceReplaceOnParse   CapabilitySupport
}

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
```

Providers set supported or not-applicable values explicitly. Missing fields stay
unsupported. Capability tests ensure a provider does not emit normalized fields
that contradict unsupported declarations.

Capability semantics are intentionally strict:

- `CapabilityUnsupported` means this provider does not currently emit or
  implement the feature. Tests should fail if normalized output contains that
  feature.
- `CapabilitySupported` means the feature is implemented and covered by provider
  fixtures or source-behavior tests.
- `CapabilityNotApplicable` means the upstream source format cannot represent
  the feature. It is not a placeholder for unfinished implementation work.

Capability conformance uses meaningful presence predicates rather than raw Go
zero-value checks. Unsupported string fields such as cwd, git branch, model,
stop reason, and session name must remain empty; unsupported counts such as
malformed lines must remain zero; unsupported booleans such as truncation must
remain false; unsupported repeated data such as relationships, subagents, tool
calls, tool results, and usage events must remain empty. A zero value is
therefore absence unless the provider declares support for the corresponding
feature and fixtures prove a source can intentionally emit it.

Generated enum tooling should be reproducible:

- keep a `tools.go` file with a `tools` build tag and a blank import for
  `github.com/dmarkham/enumer`;
- pin the enumer module in `go.mod` and commit `go.sum`;
- commit the generated `capabilitysupport_enumer.go` file;
- include a generator check, for example
  `go generate ./internal/parser && git diff --exit-code -- internal/parser/capabilitysupport_enumer.go`.

## Provider Toolkit

The facade should include helper types for common provider patterns. These
helpers live below the provider abstraction; the engine still talks only to
`Provider`.

Helpers should be plain source-set utilities. A helper stores the source-layout
state for one pattern and exposes methods with the same signatures as provider
source methods. Providers use helpers through named fields and choose which
methods to forward. Unforwarded methods fall back to `ProviderBase` no-ops.

### ProviderBase

Embedded default implementation for optional provider methods:

- empty discovery;
- empty watch plan;
- no changed-path classification;
- no source lookup;
- unsupported fingerprinting;
- no incremental parse.

`ProviderBase` carries metadata and capabilities but does not implement `Parse`.

### JSONLSourceSet

A reusable JSONL source lister/fingerprinter for the common pattern of session
transcripts stored as `.jsonl` files. It does not implement `Provider` by
itself; concrete providers hold it as a field and forward discovery, watch
planning, changed-path classification, source lookup, and fingerprinting when
those behaviors apply.

Expected options:

- root directories;
- recursive or shallow traversal;
- extension set, defaulting to `.jsonl`;
- path filters;
- project extraction from path;
- source key derivation from path;
- stable sorting;
- optional symlink directory handling to match current discovery behavior.

This helper should cover simple JSONL providers and serve as the base for more
specific source sets.

### DirectoryJSONLSourceSet

Specialized JSONL helper for layouts where project or workspace names come from
directory structure, such as `<project>/<session>.jsonl` or nested
`projects/<encoded-project>/chats/<id>.jsonl`.

### SiblingMetadataSourceSet

Wraps another source-set implementation and folds sibling files into watch plans
and effective fingerprints. This covers patterns like transcript plus
metadata/title/index files.

### SQLiteFanoutSourceSet

Creates one or many `SourceRef` values from a shared SQLite source while keeping
table and row details provider-owned. It supports providers where one database
file represents many logical sessions.

### VirtualPath Helpers

Providers that expose one logical session inside a shared source can continue to
return virtual display paths, but the virtual path format should be provider
owned and resolved through provider methods rather than hard-coded in sync.

## Sync Engine Flow

The root of the stack supports both execution shapes:

- the legacy `DiscoveredFile` and `processFile` shape, which remains
  authoritative during migration;
- the provider `SourceRef` shape, which can process the same logical source
  without writing session state while the stack is in shadow-compare mode.

The root-level migration harness owns the comparison. It exposes an explicit
provider runtime manifest keyed by `parser.AgentType`. Family helpers may build
entries for related providers, but tests must expand the family to every
concrete `AgentType`; a family-level entry is not enough to mark an individual
parse-capable provider migrated.

- legacy-only: only the existing sync path runs and writes. This is the normal
  mode for legacy adapter providers. A concrete provider may move back to this
  mode only as a documented rollback with an open follow-up task.
- shadow-compare: legacy runs and writes; provider runs through the new generic
  path and produces normalized in-memory planned effects. Tests compare those
  effects against the legacy outcome. Runtime mismatches are developer
  diagnostics only; they must not persist user-visible parse diagnostics or
  change SSE-visible state.
- provider-authoritative: provider dispatch writes, returns the caller result,
  and the old provider-specific legacy path is absent. This is reserved for
  the stack-tip cleanup after every parse-capable provider has passed shadow
  comparison.
- import-only: the provider exists for non-filesystem import/export metadata and
  is intentionally excluded from parse shadow comparison.

Only the stack-tip cleanup may use provider-authoritative mode for existing
providers. Lower provider PRs migrate by moving each affected concrete
`AgentType` entry from legacy-only to shadow-compare in the manifest and adding
the required parity coverage. A concrete parse-capable provider that is not
import-only and is not listed in the migration manifest is a test failure; this
makes a PR that merely reimplements a parser without wiring the migration easy
to spot.

The target provider-only engine flow becomes:

1. Load provider factories from the provider registry.
1. Create config-bound providers with
   `ProviderConfig{Roots: roots, Machine: machine}`.
1. Ask each provider to discover `SourceRef` values for configured roots.
1. Dedupe source refs by provider and key.
1. Ask each provider for `SourceFingerprint`.
1. Run generic skip/data-version checks using `FingerprintKey` and fingerprint
   fields.
1. Attempt incremental parsing when the provider declares and implements it, and
   interpret the result through `IncrementalStatus`.
1. Call provider `Parse` for full parses.
1. Apply existing normalization and DB write paths to each
   `ParseResultOutcome.Result` value.
1. Persist source metadata, skip cache, excluded IDs, usage events, and parse
   diagnostics using the existing storage model.

Changed-path live sync becomes:

1. The watcher reports a changed path.
1. The engine finds providers whose `WatchPlan` roots match the changed path.
1. Each matched provider classifies it through `SourcesForChangedPath` with a
   `ChangedPathRequest` that includes the changed path, event kind, and
   matched watch root.
1. The engine processes the returned `SourceRef` values generically.

Source lookup becomes:

1. The engine loads the owning provider and stored session metadata.
1. The engine passes the raw session ID, full session ID, stored `file_path`,
   and stored fingerprint key to `FindSourceRequest`.
1. The provider returns a `SourceRef`, not just a string path.
1. The engine can ask the provider for a fingerprint/source mtime from that
   reference.

The engine must treat stored `file_path` as an advisory compatibility key. It
must not check that path first or assume it is a filesystem path, because some
providers expose virtual paths or logical sessions inside a shared source.
`FindSourceRequest.RawSessionID` is lookup input only; returned parse outcomes,
source errors, and exclusions still use persisted full session IDs.

### Transitional Shadow Comparison

The root harness has two comparison surfaces:

- source-level parity, available as soon as the root harness lands, compares one
  legacy-discovered source or fixture set against the provider runner.
  Provider implementation PRs must use this surface before they can move their
  manifest entry to shadow-compare.
- caller-level dual-run, added by the later sync, lookup/watch, and diagnostics
  tasks, wraps production callers such as full sync, changed-path sync,
  `SyncSingleSession`, source lookup, source mtime, and parse-diff.

This ordering makes shadow-compare real before caller migrations exist: provider
PRs prove source-level parity first, then caller tasks prove the same provider
runner behaves correctly when invoked through production sync surfaces.

During migration, both comparison surfaces use the provider-only execution
helpers but keep side effects isolated:

1. Run the legacy caller normally and keep its `processResult` as the value that
   drives DB writes, skip-cache persistence, data-version updates, source
   metadata, SSE emissions, and diagnostics.
1. Run the provider caller for the same agent through the generic provider
   helper. Depending on the caller, this may come from provider discovery,
   `SourcesForChangedPath`, or `FindSource`; the comparison layer must not
   teach the engine provider-specific path formats.
1. Normalize both outputs into the same comparison shape for the surface being
   exercised. The root `processFile` comparison covers full session IDs,
   parsed message/tool/usage content, excluded IDs, retry state, data-version
   state, source metadata, and per-session errors. Source-level provider tests
   and later caller tasks own `SkipReason` parity until the legacy side
   exposes a comparable skip-reason projection.
1. Represent provider-side effects as in-memory planned effects, not live DB
   mutations. Planned effects include source metadata writes, data-version
   writes, skip-cache updates, and diagnostics. Integration tests may
   additionally run against disposable stores, but shadow mode never receives
   the live writer. SSE/event scope parity is deferred until the
   provider-authoritative caller owns live event emission.
1. Report mismatches as test failures in the migration harness. Runtime
   diagnostics are opt-in developer diagnostics only; they must not create
   user-visible parse diagnostics or SSE-visible state. The provider side must
   not mutate persisted session state while in shadow-compare mode.

`ProviderPlannedEffects` must match the legacy engine's observable write model,
not an abstract parser-local model:

- source metadata keys use the provider fingerprint key when present, then
  `SourceRef.FingerprintKey`, then `SourceRef.Key`;
- skip-cache keys follow the same key order the legacy engine uses before a
  persisted skip decision;
- data-version entries represent process-result-level write intent for the
  concrete session rows the engine would stamp after successful writes,
  including current versus `DataVersionNeedsRetry` state; retry-reason text is
  provider-local until the legacy process result exposes equivalent detail;
- diagnostics mirror the legacy parse diagnostic fields, including display path,
  source key, session ID, error, and retryability, but are never written to
  the live diagnostics table in shadow mode;
- legacy skip, incremental, and whole-source error states are recorded as
  non-comparable in the root `processFile` hook. Provider `SkipReason` parity
  is handled by provider-local/source-level tests until a later caller task
  defines a legacy skip-reason projection.

Provider output must be namespaced before it can produce planned effects and
before any remote machine prefix is applied. `ParseResult.Session.Agent` must
equal the provider `AgentType`. Persisted session IDs in the result graph must
use the provider's `AgentDef.IDPrefix` when one exists; this includes result
IDs, parent IDs, usage-event session IDs, subagent links, exclusions, and
diagnostic session IDs. `ParsedSession.SourceSessionID` is excluded from this
check because current parsers use it for upstream/raw source IDs, not persisted
session IDs. Diagnostic `SourceError.SourceKey` values are required and must be
one of the observed source identities or a derived virtual key: the provider
fingerprint key, `SourceRef.FingerprintKey`, `SourceRef.Key`, or one of those
values followed by `#`, `::`, or `|`. Cross-provider sessions are not legal in
shadow mode because they make parity false positives indistinguishable from real
legacy behavior.

Fingerprint failures and parse failures are compared separately. A fingerprint
failure means no provider parse was attempted and the mismatch report records a
fingerprint failure. A parse failure after a successful fingerprint records the
fingerprint key and parse error. Neither failure may block the legacy write path
while the mode is `shadow-compare`.

Mismatch reports must include provider, migration mode, source key, fingerprint
key, comparison field path, a bounded legacy summary, a bounded provider
summary, and whether the mismatch came from discovery, fingerprint, parse
output, planned effects, or runtime failure. Runtime reporting is developer-only
logging or debug output until a later task defines persistence.

Shadow comparison can double-parse large sources while a provider migrates.
Provider PRs that touch large roots, shared SQLite sources, or composite sources
need fixture coverage or benchmarks that show fingerprinting and shadow parse
overhead are acceptable before promotion. The rollback rule is to move the
manifest entry back to `legacy-only`, leave the legacy path authoritative, and
keep the blocking kata/review item open.

Provider branches must exercise this transition with shared tests rather than
only provider-local unit tests. The branch is considered migrated only when the
manifest entry and parity tests are present. Deferred parity items such as
provider-only retry-reason text, SSE scopes, and caller-specific skip-reason
mapping are promotion gates for provider-authoritative mode; they cannot remain
open at the stack tip where legacy dispatch is removed.

Before any concrete provider changes to `provider-authoritative`, that branch
must prove the generic hook contract for its source shape:

- `FindSource` honors global and file-scoped force-parse by allowing stale
  stored source hints when requested.
- provider not-found in authoritative mode is an explicit error, not an implicit
  legacy fallback.
- multi-result sources preserve `ParseResultOutcome.DataVersion` per session, so
  retry-needed fallback rows do not mark unrelated current rows stale.
- skip-cache lookup and persistence use the provider fingerprint key selected
  for the source, with tests for virtual or composite paths where `file.Path`
  differs from `SourceRef.FingerprintKey`.
- `ResultSetComplete`, excluded IDs, diagnostics, and source errors have parity
  tests for the provider's source family before the old legacy dispatch for
  that provider is removed.

If a provider that moved to shadow-compare proves flaky, its manifest entry can
return to legacy-only with a reason and an open kata task or review note. The
tip cleanup cannot proceed while any parse-capable provider is legacy-only or
has known shadow mismatches.

## Registry

`parser.Registry` remains the stable metadata surface during migration, but the
source of truth shifts to providers.

Target API:

```go
func ProviderFactories() []ProviderFactory
func ProviderFactoryByType(AgentType) (ProviderFactory, bool)
func NewProvider(AgentType, ProviderConfig) (Provider, bool)
func AgentByType(AgentType) (AgentDef, bool)
func AgentByPrefix(string) (AgentDef, bool)
```

`AgentByType` and `AgentByPrefix` can continue to return `AgentDef` for config,
settings, display, and export code. `AgentDef` source callbacks become legacy
compatibility fields during migration and are removed or deprecated once every
consumer uses providers.

## Migration Plan

The implementation should migrate all providers through a stacked dual-run
sequence:

1. Add provider core types, `ProviderBase` defaults, contract invariants, and
   compile-tested embedding examples.
1. Add capability enum generation, pinned `enumer` tooling, and generated-file
   verification.
1. Add provider factory registry tests while preserving current
   `parser.Registry`.
1. Add the root-level dual-run migration harness before migrating provider
   branches. It must contain the shared provider execution helper, comparison
   normalizer, source-level parity surface, planned-effect comparison model,
   caller-level `processFile` shadow comparison for full sync, explicit
   per-`AgentType` migration manifest, and tests that fail when a concrete
   parse-capable provider exists without a migration-mode entry.
1. Add JSONL source-set helpers and tests for simple file-backed JSONL
   providers.
1. Use `git-spice` to restack the provider branches on the root harness branch
   after that lower branch changes when the user has explicitly authorized
   branch changes for the session. The stack must be verified with
   `gs log short` and conflicts resolved provider by provider. Pushing,
   submitting, or updating PRs is a separate network operation and requires
   separate explicit authorization.
1. Migrate simple JSONL providers with acceptance tests for discovery,
   fingerprint, parse output, skip-cache metadata, and data-version behavior.
   Each provider PR must move its affected concrete `AgentType` entries from
   legacy-only to shadow-compare and run the shared source-level
   provider-vs-legacy parity harness. Legacy sync remains authoritative.
1. Add and migrate sibling/composite source providers with acceptance tests for
   watch planning, composite fingerprints, sidecar/title refreshes, and
   changed path classification. Each PR must opt its provider into
   shadow-compare and pass the shared source-level parity harness while legacy
   sync remains authoritative.
1. Add and migrate virtual-path and SQLite fan-out providers with acceptance
   tests for stored advisory paths, tombstone recovery via
   `StoredSourcePaths`, logical session lookup, per-session errors, and source
   mtime behavior. Each PR must opt its provider into shadow-compare and pass
   the shared parity harness while legacy sync remains authoritative.
1. Add and migrate non-file import/database providers with acceptance tests for
   `FindSource`, fingerprinting, and unsupported source mechanics. Import-only
   providers are explicitly marked import-only rather than shadow-compared.
1. Add the session-store API for persisted source-path hint lookup before
   changed-path shadow comparison depends on providers. It must query by
   provider and watched root, use the `(agent, file_path)` index shape, return
   stable de-duplicated paths, and include tests for provider/root filtering,
   path normalization, sibling-prefix false positives such as `/root/db`
   versus `/root/db2`, dedupe, batching/no-truncation behavior, and large
   unrelated session tables.
1. Add provider compatibility tests for stored hint interpretation before
   changed-path shadow comparison depends on providers. SQLite fan-out
   providers must cover malformed or obsolete virtual paths, debug-only
   diagnostics, DB row deletion, DB file deletion, and stale hints that belong
   to a different physical DB under the same watch root.
1. Move the remaining source-processing caller semantics into the caller-level
   dual-run harness: changed-path sync and `SyncSingleSession`. The root
   harness already shadows the shared `processFile` path, so these tasks must
   add caller-specific source selection, stored-source hints, and acceptance
   coverage rather than adding a second shadow hook. During migration these
   callers compare provider output against legacy for parsed output,
   skip-cache, data-version, source metadata, diagnostics, excluded IDs, and
   retry state; only the legacy result writes. Changed-path comparison must
   populate `StoredSourcePaths` from scoped persisted session metadata and
   include DB row deletion and DB file deletion integration tests.
1. Move lookup/watch callers into the caller-level dual-run harness: session
   watch flows, export/source lookup, source mtime, and token-usage raw source
   probing. During migration these callers compare lookup freshness, virtual
   path, source mtime, and raw probing behavior against legacy.
1. Move diagnostic and comparison callers into the caller-level dual-run
   harness: parse-diff and parse diagnostics. During migration these callers
   compare report shape and source-error behavior against legacy.
1. At the tip of the stack only, switch all shadow-compared providers to
   provider-authoritative dispatch, remove the provider-by-provider
   `processFile` switch, and remove or deprecate old `AgentDef` source
   callback fields after all callers stop using them.

Migration should keep existing parser unit tests. Provider-level tests become
the required integration surface for future providers.

## Kata Tracking

The migration is tracked under parent kata issue `terh`. The dual-run correction
adds these blocking tasks:

- `5jrz`: add the root-level dual-run provider migration harness.
- `y8wg`: restack the provider PRs on the dual-run root with git-spice.
- `0cfe`: migrate simple JSONL providers by shadow-comparing each provider.
- `jwav`: migrate composite and sibling-file providers by shadow-comparing each
  provider.
- `aghm`: migrate virtual-path and database-backed providers by shadow-comparing
  each provider.
- `1xcx`: add provider/root-scoped stored source-path hint lookup.
- `9dee`: add stored-hint provider compatibility tests.
- `kj1f`: move source-processing callers into the dual-run harness.
- `djyy`: move lookup, watch, export, and usage callers into the dual-run
  harness.
- `cff5`: move parse-diff and diagnostics into the dual-run harness.
- `n489`: remove legacy parser dispatch at the stack tip only.

## Testing

Required tests:

- Provider registry completeness: every `AgentType` has exactly one provider.
- Prefix uniqueness and metadata parity with current registry behavior.
- Provider factory instantiation: configured roots and machine are copied into
  config-bound providers and do not mutate singleton registry state.
- `ProviderBase` contract tests: zero-value optional methods are callable and
  return the documented no-op or typed unsupported results.
- Unsupported-feature tests proving the engine distinguishes
  `ErrUnsupportedProviderFeature` from I/O and parse errors, and treats
  unsupported fingerprinting for returned sources as a contract failure.
- Provider concurrency tests or race tests for shared provider instances and
  immutable `SourceRef.Opaque` payloads.
- Compile-tested embedding examples for `ProviderBase`, named JSONL source-set
  fields, sibling metadata source-set fields, and explicit concrete method
  overrides.
- Capability enum generation, JSON representation, and zero-value behavior.
- Capability conformance: unsupported fields should not be emitted in parsed
  output.
- Capability semantics: `not_applicable` is accepted only for fields impossible
  in the upstream source format, not for unimplemented work.
- JSONL source helper discovery, sorting, filtering, project extraction, and
  fingerprint tests.
- Sibling metadata fingerprint tests.
- Watch-plan and changed-path classification tests for recursive roots,
  non-recursive roots, include/exclude filters, and sibling metadata debounce
  groups.
- Stored advisory path tests proving `FindSourceRequest.StoredFilePath` is
  interpreted only by the provider.
- Outcome ID namespace tests proving `ParseResult` session IDs,
  `ExcludedSessionIDs`, and `SourceError.SessionID` use persisted full session
  IDs even when lookup starts with a raw upstream ID.
- SQLite fan-out source key, virtual path, and per-session error tests.
- Data-version tests for current, skipped, retry-needed, and mixed per-session
  parse outcomes from one source.
- Skip-cache tests for complete clean multi-session parses, incomplete
  multi-session parses, retry-needed results, retryable `SourceErrors`, and
  non-retryable `SourceErrors`.
- Fingerprint performance tests or benchmarks for large roots and composite
  sources, with the pass criteria from the fingerprint section.
- Provider harness tests for discovery, fingerprint, parse, source lookup, and
  optional incremental parsing.
- Incremental parsing tests for `IncrementalUnsupported`,
  `IncrementalNoNewData`, `IncrementalApplied`, `IncrementalNeedsFullParse`,
  and real incremental errors.
- Parse diagnostic tests proving stable source fields are reported and opaque
  payloads are not serialized or logged.
- Source-level migration parity tests comparing provider output to current
  parser/process output before each provider group opts into shadow-compare,
  including skip-cache, data-version writes, persisted source metadata,
  diagnostics, excluded IDs, and retry-needed behavior.
- Caller-level migration parity tests proving full sync, changed-path sync,
  `SyncSingleSession`, lookup/watch flows, export/source lookup, source mtime,
  token usage raw-source probing, parse-diff, and diagnostics invoke the same
  provider runner without changing legacy-authoritative writes during the
  migration stack.
- Migration manifest tests that fail when a concrete parse-capable `AgentType`
  is registered without an explicit mode. Provider implementation PRs must
  include the manifest change to shadow-compare; import-only providers must be
  marked explicitly so they are not mistaken for missed migrations. Family
  helpers must expand to concrete agent entries in tests.
- Dual-run isolation tests proving provider shadow comparison cannot write
  sessions, messages, source metadata, data-version rows, skip-cache entries,
  diagnostics, or SSE-visible state while legacy remains authoritative. These
  tests compare in-memory planned effects or disposable-store observations,
  not live production mutations.
- Sync integration tests for incremental Claude/Codex, multi-session sources,
  parse-diff, source mtime, source lookup, skip cache, usage events, sidecars,
  virtual paths, and title/metadata refreshes.
- Caller migration tests for full sync, changed-path sync, `SyncSingleSession`,
  session watch flows, export/source lookup, token usage raw-source probing,
  source mtime, and parse diagnostics.
- Generated tooling check for `enumer` output.
- Adding-provider checklist test that fails until registry, capabilities,
  fixtures, source behavior, migration-mode wiring, parity coverage, and docs
  are present.

## Error Handling

Providers return structured errors and outcomes. The engine makes generic
decisions from those structures:

- whole-source failure: returned `error`;
- per-session failure: `SourceErrors`, while successful sessions from the same
  source are still written;
- retryable failure: do not cache skip by unchanged mtime and do not mark the
  affected source/session current for the parser data version;
- non-retryable per-session failure: eligible for failure-cache persistence, but
  not for a data-version-blind clean source skip;
- full parse fallback from incremental: `IncrementalNeedsFullParse`;
- unsupported optional provider feature: `ErrUnsupportedProviderFeature`;
- successful lower-resolution fallback: per-result `DataVersionNeedsRetry` plus
  `RetryReason`;
- skipped non-session source: explicit `SkipReason`;
- existing-row rewrite required: `ForceReplace`.

Parse-diff treats provider `SourceErrors` as reportable parse errors, preserving
today's behavior for shared database sources.

## Documentation Updates

Implementation should update developer-facing docs to describe how to add a
provider:

1. add an `AgentType`;
1. implement a provider embedding `ProviderBase`;
1. select source helpers or implement provider-specific source methods;
1. implement `Parse`;
1. set capabilities;
1. add fixtures and provider harness tests;
1. update README/config docs for default directories and environment variables.

## Success Criteria

- All current providers are registered through the provider facade.
- Before the stack tip, all parse-capable current providers are explicitly
  listed as shadow-compared in the migration manifest and legacy dispatch
  remains authoritative.
- At the stack tip, `sync.Engine` no longer has a provider-by-provider parse
  dispatch switch.
- Source shape is not inspected by the engine.
- Capability reports serialize to readable JSON names.
- Capability enum generation is pinned and reproducible.
- Configured roots are provider-instance state, not mutable singleton state.
- `ProviderBase` zero-value optional methods are callable by the engine.
- Stored `file_path` values are provider-owned advisory lookup keys.
- Retry-needed outcomes preserve future parse eligibility instead of marking
  data versions current.
- Existing parser and sync tests pass after migration.
- Parse-diff continues to use the same provider path as normal sync.
- Adding a provider requires implementing the provider contract and fails tests
  until capabilities, source behavior, fixtures, migration-mode wiring, parity
  coverage, and docs are present.
- The provider-facade kata tasks listed in this spec are implemented and closed
  with evidence as their corresponding stack slices are verified.
- The final stack has no legacy provider/parser dispatch surface left for
  migrated providers.

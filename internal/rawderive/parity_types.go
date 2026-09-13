package rawderive

import (
	"time"

	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
)

const ParitySchemaVersion = 1
const ParityPreparationVersion = "shared-ingest-parity-v1"
const ParityProjectionVersion = "hosted-projection-v1"

type ParityDigest [32]byte

type ParityCohort struct {
	DeviceID string
	Provider parser.AgentType
	RootID   string
}

type ParityVersions struct {
	ParserBuild ParityDigest
	Data        int
	Preparation string
	Projection  string
	Comparison  int
	Quality     int
	SecretRules string
	Policy      ParityDigest
}

type ParityRequest struct {
	RunID           string
	RuntimeID       string
	BaselineProfile string
	Cohort          ParityCohort
}

type ParityBinding struct {
	Request        ParityRequest
	BaselineID     string
	BaselineConfig ParityDigest
	Tenant         string
	Versions       ParityVersions
	ObservedAt     time.Time
}

type ParityMemberKey struct {
	SourceID   string
	LogicalKey string
	Kind       string
}

type ParityLink struct {
	Kind                           string
	Ordinal, CallIndex, EventIndex int
	Target                         ParityMemberKey
	Unresolved                     string
}

type ParityOverlay struct {
	Excluded         bool
	SourceDeleted    bool
	ProviderExcluded bool
}

type ParityGraph struct {
	Key                     ParityMemberKey
	Prepared                ingest.PreparedSession
	PromptEvidenceDiscarded bool
	Links                   []ParityLink
	Overlay                 ParityOverlay
}

type ParityFingerprint struct {
	Session, Messages, Tools, Usage, Signals, Findings, Links, Exclusions ParityDigest
	Semantic                                                              ParityDigest
}

type ParityVerdict string

const (
	ParityMatched    ParityVerdict = "matched"
	ParityMismatched ParityVerdict = "mismatched"
	ParityAmbiguous  ParityVerdict = "ambiguous"
	ParityLegacyOnly ParityVerdict = "legacy_only"
	ParityMissing    ParityVerdict = "missing"
	ParityPartial    ParityVerdict = "partial_unsupported"
	ParityStale      ParityVerdict = "stale"
)

type ParityCounts struct {
	Matched            int64 `json:"matched"`
	Mismatched         int64 `json:"mismatched"`
	Ambiguous          int64 `json:"ambiguous"`
	LegacyOnly         int64 `json:"legacy_only"`
	Missing            int64 `json:"missing"`
	PartialUnsupported int64 `json:"partial_unsupported"`
	Stale              int64 `json:"stale"`
}

type ParityComparison struct {
	Verdict   ParityVerdict
	Different uint16
}

type ParityReport struct {
	RunID               string       `json:"run_id"`
	State               string       `json:"state"`
	Code                string       `json:"code,omitempty"`
	Members             ParityCounts `json:"members"`
	Sources             ParityCounts `json:"sources"`
	PendingSources      int64        `json:"pending_sources"`
	BaselineSealed      bool         `json:"baseline_sealed"`
	Complete            bool         `json:"complete"`
	Freshness           string       `json:"freshness"`
	Passing             bool         `json:"passing"`
	RequestGeneration   int64        `json:"request_generation"`
	CompletedGeneration int64        `json:"completed_generation"`
	BaselineObservedAt  *time.Time   `json:"baseline_observed_at,omitempty"`
	RuntimeObservedAt   *time.Time   `json:"runtime_observed_at,omitempty"`
}

type ParityLease struct {
	RunID             string
	Owner             string
	Token             string
	Request           ParityRequest
	RequestGeneration int64
	InitEpoch         int64
	BindingDigest     ParityDigest
	ObservedAt        time.Time
	BatchSize         int
}

type ParitySource struct {
	ID               string
	Identity         rawsync.AuthIdentity
	HeadManifestID   string
	HeadGeneration   int64
	DependencyDigest ParityDigest
	Required         bool
}

type ParityHistoryEntry struct {
	ManifestID, Receipt, ParentReceipt string
	Generation                         int64
}

type ParityMemberResult struct {
	Key         ParityMemberKey
	Fingerprint *ParityFingerprint
	Verdict     ParityVerdict
	Different   uint16
}

type ParitySourceResult struct {
	Members  []ParityMemberResult
	Complete bool
	Blocker  ParityVerdict
	Code     string
}

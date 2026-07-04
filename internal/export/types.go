// Package export defines shared JSON DTOs for exported report
// metadata. Keep these types additive and versioned because multiple CLI and
// API surfaces depend on their field names.
package export

import "time"

const UsageDailySchemaVersion = 1
const ActivityReportSchemaVersion = 1
const SessionSummarySchemaVersion = 1

// CostSource is a closed v1 enum. Adding a value requires a schema version
// bump for any export surface that emits it.
type CostSource string

const (
	CostSourceComputed CostSource = "computed"
	CostSourceReported CostSource = "reported"
	CostSourceMixed    CostSource = "mixed"
)

type PricingBlock struct {
	Source              string                        `json:"source"`
	TableVersion        string                        `json:"table_version"`
	LatestRowUpdatedAt  *time.Time                    `json:"latest_row_updated_at"`
	CustomOverrideCount int                           `json:"custom_override_count"`
	EffectiveRowCount   int                           `json:"effective_row_count"`
	Digest              string                        `json:"digest"`
	CostSource          CostSource                    `json:"cost_source"`
	Fallback            PricingFallback               `json:"fallback"`
	Models              map[string]EffectiveModelRate `json:"models"`
}

type PricingFallback struct {
	Used   bool     `json:"used"`
	Models []string `json:"models"`
}

type EffectiveModelRate struct {
	MatchedPattern        *string    `json:"matched_pattern"`
	InputCostPerMTok      float64    `json:"input_cost_per_mtok"`
	OutputCostPerMTok     float64    `json:"output_cost_per_mtok"`
	CacheWriteCostPerMTok float64    `json:"cache_write_cost_per_mtok"`
	CacheReadCostPerMTok  float64    `json:"cache_read_cost_per_mtok"`
	CostSource            CostSource `json:"cost_source"`
}

// ProjectResolution is a closed v1 enum. Adding a value requires a schema
// version bump for any export surface that emits it.
type ProjectResolution string

const (
	ProjectResolutionResolved  ProjectResolution = "resolved"
	ProjectResolutionUnknown   ProjectResolution = "unknown"
	ProjectResolutionAmbiguous ProjectResolution = "ambiguous"
)

type ProjectIdentity struct {
	Key              string `json:"key"`
	KeySource        string `json:"key_source"`
	NormalizedRemote string `json:"normalized_remote,omitempty"`
	RootPath         string `json:"root_path,omitempty"`
	MachineLocal     bool   `json:"machine_local,omitempty"`
}

type ProjectMapEntry struct {
	Resolution ProjectResolution `json:"resolution"`
	Identity   *ProjectIdentity  `json:"identity"`
}

type ProjectIdentityInput struct {
	RootPath         string
	GitRemote        string
	GitRemoteName    string
	WorktreeName     string
	WorktreeRootPath string
}

type ProjectIdentityObservation struct {
	Project          string    `json:"project"`
	Machine          string    `json:"machine"`
	RootPath         string    `json:"root_path"`
	GitRemote        string    `json:"git_remote"`
	GitRemoteName    string    `json:"git_remote_name"`
	WorktreeName     string    `json:"worktree_name"`
	WorktreeRootPath string    `json:"worktree_root_path"`
	ObservedAt       time.Time `json:"observed_at"`
	NormalizedRemote string    `json:"normalized_remote"`
	KeySource        string    `json:"key_source"`
	Key              string    `json:"key"`
}

type SessionExportCursor struct {
	Next string `json:"next"`
}

type SessionExportError struct {
	Error      string `json:"error"`
	Message    string `json:"message"`
	DatabaseID string `json:"database_id,omitempty"`
}

// SessionClassification is a closed v1 enum. Adding a value requires a schema
// version bump for any export surface that emits it.
type SessionClassification string

const (
	SessionClassificationInteractive SessionClassification = "interactive"
	SessionClassificationAutomated   SessionClassification = "automated"
)

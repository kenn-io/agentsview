// Package export defines shared JSON DTOs for exported report
// metadata. Keep these types additive and versioned because multiple CLI and
// API surfaces depend on their field names.
package export

import (
	"time"

	"go.kenn.io/agentsview/internal/money"
)

const UsageDailySchemaVersion = 3
const ActivityReportSchemaVersion = 3
const SessionSummarySchemaVersion = 3

// CostSource is a closed contract enum. Adding a value requires a schema version
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
	MatchedPattern        *string     `json:"matched_pattern"`
	InputCostPerMTok      money.Money `json:"input_cost_per_mtok"`
	OutputCostPerMTok     money.Money `json:"output_cost_per_mtok"`
	CacheWriteCostPerMTok money.Money `json:"cache_write_cost_per_mtok"`
	CacheReadCostPerMTok  money.Money `json:"cache_read_cost_per_mtok"`
	CostSource            CostSource  `json:"cost_source"`
}

// ProjectResolution is a closed contract enum. Adding a value requires a schema
// version bump for any export surface that emits it.
type ProjectResolution string

const (
	ProjectResolutionResolved  ProjectResolution = "resolved"
	ProjectResolutionUnknown   ProjectResolution = "unknown"
	ProjectResolutionAmbiguous ProjectResolution = "ambiguous"
)

type ProjectKind string

const (
	ProjectKindGitRemote   ProjectKind = "git_remote"
	ProjectKindMachineRoot ProjectKind = "machine_root"
)

type WorktreeRelationship string

const (
	WorktreeMain    WorktreeRelationship = "main_worktree"
	WorktreeLinked  WorktreeRelationship = "linked_worktree"
	WorktreeNone    WorktreeRelationship = "not_a_worktree"
	WorktreeUnknown WorktreeRelationship = "unknown"
)

type CheckoutState string

const (
	CheckoutBranch   CheckoutState = "branch"
	CheckoutDetached CheckoutState = "detached"
	CheckoutUnknown  CheckoutState = "unknown"
)

type IdentityScope struct {
	ArchiveID   string
	ArchiveSalt string
	MachineID   string
}

type ProjectIdentity struct {
	Key              string      `json:"key"`
	Kind             ProjectKind `json:"kind"`
	NormalizedRemote string      `json:"normalized_remote,omitempty"`
	RootKey          string      `json:"root_key,omitempty"`
	RepositoryKey    string      `json:"repository_key"`
}

// StoredProjectIdentity is an internal derivation used by archive migration
// and UI-oriented legacy queries. It must never be serialized as evidence.
type StoredProjectIdentity struct {
	Key              string `json:"-"`
	KeySource        string `json:"-"`
	NormalizedRemote string `json:"-"`
	RootPath         string `json:"-"`
	MachineLocal     bool   `json:"-"`
}

type WorktreeReference struct {
	Relationship  WorktreeRelationship `json:"relationship"`
	WorktreeKey   string               `json:"worktree_key,omitempty"`
	RepositoryKey string               `json:"repository_key,omitempty"`
}

type CheckoutReference struct {
	State  CheckoutState `json:"state"`
	Branch string        `json:"branch,omitempty"`
}

type ProjectReference struct {
	ProjectKey   string            `json:"project_key"`
	DisplayLabel string            `json:"display_label"`
	Resolution   ProjectResolution `json:"resolution"`
	Identity     *ProjectIdentity  `json:"identity,omitempty"`
	Worktree     WorktreeReference `json:"worktree"`
	Checkout     CheckoutReference `json:"checkout"`
}

type RemoteSelection struct {
	Resolution ProjectResolution
	Name       string
	Raw        string
	Normalized string
}

type ProjectMapEntry struct {
	DisplayLabel string            `json:"display_label"`
	ProjectKey   string            `json:"-"`
	Resolution   ProjectResolution `json:"resolution"`
	Identity     *ProjectIdentity  `json:"identity,omitempty"`
}

type ProjectIdentityInput struct {
	DisplayLabel     string
	RootPath         string
	GitRemote        string
	GitRemoteName    string
	RemoteSelection  RemoteSelection
	RepositoryPath   string
	WorktreeName     string
	WorktreeRootPath string
	WorktreeKind     WorktreeRelationship
	GitBranch        string
	Detached         bool
}

type ProjectIdentityObservation struct {
	SessionID            string               `json:"session_id"`
	SourceArchiveID      string               `json:"-"`
	SourceArchiveSalt    string               `json:"-"`
	Project              string               `json:"project"`
	Machine              string               `json:"machine"`
	RootPath             string               `json:"root_path"`
	GitRemote            string               `json:"git_remote"`
	GitRemoteName        string               `json:"git_remote_name"`
	RepositoryPath       string               `json:"repository_path"`
	WorktreeName         string               `json:"worktree_name"`
	WorktreeRootPath     string               `json:"worktree_root_path"`
	WorktreeRelationship WorktreeRelationship `json:"worktree_relationship"`
	CheckoutState        CheckoutState        `json:"checkout_state"`
	GitBranch            string               `json:"git_branch"`
	RemoteResolution     ProjectResolution    `json:"remote_resolution"`
	RemoteCandidateCount int                  `json:"remote_candidate_count"`
	ObservedAt           time.Time            `json:"observed_at"`
	NormalizedRemote     string               `json:"normalized_remote"`
	KeySource            string               `json:"key_source"`
	Key                  string               `json:"key"`
}

type SessionExportCursor struct {
	Next string `json:"next"`
}

type SessionExportError struct {
	Error      string `json:"error"`
	Message    string `json:"message"`
	DatabaseID string `json:"database_id,omitempty"`
}

// SessionClassification is a closed contract enum. Adding a value requires a schema
// version bump for any export surface that emits it.
type SessionClassification string

const (
	SessionClassificationInteractive SessionClassification = "interactive"
	SessionClassificationAutomated   SessionClassification = "automated"
)

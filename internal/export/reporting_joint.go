package export

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/money"
)

// DefaultReportingBucket is the export default, not the wire precision limit.
const DefaultReportingBucket = 5 * time.Minute

// ParseReportingBucket resolves a CLI or database export option. Complete hours
// need an integral number of buckets; minute precision bounds output to 60 per
// hour without a list of permitted resolutions.
func ParseReportingBucket(version int, value string) (time.Duration, error) {
	if value == "" {
		return DefaultReportingBucket, nil
	}
	if version != ReportingJointSchemaVersion {
		return 0, fmt.Errorf("bucket selection requires reporting schema 4")
	}
	bucket, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid reporting bucket: %w", err)
	}
	return bucket, validateReportingBucketDuration(bucket)
}

func validateReportingBucketDuration(bucket time.Duration) error {
	if bucket < time.Minute || bucket > time.Hour || bucket%time.Minute != 0 || time.Hour%bucket != 0 {
		return fmt.Errorf("reporting bucket must be a positive whole-minute divisor of one hour")
	}
	return nil
}

func reportingBucketDuration(version, seconds int) (time.Duration, error) {
	if version != ReportingJointSchemaVersion {
		if seconds != 0 {
			return 0, fmt.Errorf("bucket_seconds requires reporting schema 4")
		}
		return DefaultReportingBucket, nil
	}
	// Check before converting to Duration so malformed wire values cannot wrap.
	if seconds < 60 || seconds > 3600 {
		return 0, fmt.Errorf("reporting bucket_seconds must be between 60 and 3600")
	}
	bucket := time.Duration(seconds) * time.Second
	return bucket, validateReportingBucketDuration(bucket)
}

// ReportingJoint is a complete sparse cell set for the hour's project scope.
// An empty ProjectKeys set selects the whole archive. An empty Cells set
// retracts every previously published cell for this hour and scope.
type ReportingJoint struct {
	ProjectKeys []string        `json:"project_keys"`
	Cells       []ReportingCell `json:"cells"`
}

// ReportingCell preserves dimension relationships; it contains no session data.
// Usage may exist without activity. Unknown automation uses "unknown", not an
// inferred interactive classification. Empty ProjectKey means unattributed.
type ReportingCell struct {
	BucketStart  string               `json:"bucket_start"`
	Project      string               `json:"project"`
	ProjectKey   string               `json:"project_key"`
	Agent        string               `json:"agent"`
	Model        string               `json:"model"`
	Automation   string               `json:"automation"`
	AgentMinutes float64              `json:"agent_minutes"`
	MaxAgents    int                  `json:"max_agents"`
	Usage        ReportingUsageTotals `json:"usage"`
	Pricing      ReportingCellPricing `json:"pricing"`
}

// ReportingCellPricing partitions known cost by its provenance. UnpricedRows
// records observations whose unknown cost must not be presented as free usage.
type ReportingCellPricing struct {
	ComputedCost  money.Money `json:"computed_cost"`
	ReportedCost  money.Money `json:"reported_cost"`
	AllocatedCost money.Money `json:"allocated_cost"`
	UnpricedRows  int64       `json:"unpriced_rows"`
}

// ValidateReportingProjectScope checks the scope before a caller opens SQLite.
func ValidateReportingProjectScope(version int, keys []string) error {
	if len(keys) > 0 && version != ReportingJointSchemaVersion {
		return fmt.Errorf("project scope requires reporting schema 4")
	}
	for _, key := range keys {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("project scope contains an empty key")
		}
	}
	return nil
}

func normalizeReportingJoint(hour ReportingHour) (*ReportingJoint, error) {
	if hour.SchemaVersion != ReportingJointSchemaVersion {
		if hour.Joint != nil {
			return nil, fmt.Errorf("joint cells require reporting schema 4")
		}
		return nil, nil
	}
	if hour.Joint == nil {
		return nil, fmt.Errorf("reporting schema 4 requires joint cells")
	}
	joint := *hour.Joint
	joint.ProjectKeys = cloneOrEmpty(joint.ProjectKeys)
	slices.Sort(joint.ProjectKeys)
	joint.ProjectKeys = slices.Compact(joint.ProjectKeys)
	if err := ValidateReportingProjectScope(hour.SchemaVersion, joint.ProjectKeys); err != nil {
		return nil, err
	}
	joint.Cells = cloneOrEmpty(joint.Cells)
	slices.SortFunc(joint.Cells, compareReportingCells)
	start, err := parseReportingHour(hour.Period)
	if err != nil {
		return nil, err
	}
	bucket, err := reportingBucketDuration(hour.SchemaVersion, hour.BucketSeconds)
	if err != nil {
		return nil, err
	}
	for i, cell := range joint.Cells {
		at, err := time.Parse(time.RFC3339, cell.BucketStart)
		if err != nil || at.Before(start) || !at.Before(start.Add(time.Hour)) ||
			at.Sub(start)%bucket != 0 || cell.BucketStart != at.UTC().Format(time.RFC3339) {
			return nil, fmt.Errorf("joint cell has invalid bucket %q", cell.BucketStart)
		}
		if i > 0 && compareReportingCells(joint.Cells[i-1], cell) == 0 {
			return nil, fmt.Errorf("duplicate joint cell")
		}
		if len(joint.ProjectKeys) > 0 && !slices.Contains(joint.ProjectKeys, cell.ProjectKey) {
			return nil, fmt.Errorf("joint cell is outside project scope")
		}
	}
	return &joint, nil
}

func compareReportingCells(a, b ReportingCell) int {
	for _, order := range []int{cmp.Compare(a.BucketStart, b.BucketStart),
		cmp.Compare(a.ProjectKey, b.ProjectKey), cmp.Compare(a.Agent, b.Agent),
		cmp.Compare(a.Model, b.Model), cmp.Compare(a.Automation, b.Automation)} {
		if order != 0 {
			return order
		}
	}
	return 0
}

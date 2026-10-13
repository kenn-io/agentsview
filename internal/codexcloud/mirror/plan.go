package mirror

import "time"

// UsageRow is one thread's row from OpenAI's plan usage report
// (thread_usage/query_v2), with the server's field names. Percentages are
// shares of the plan's limits; credits are decimal strings, possibly in
// exponent form such as "0E-10", kept as sent.
type UsageRow struct {
	ThreadID        string          `json:"thread_id"`
	DataStatus      string          `json:"data_status"`
	UsageSource     string          `json:"usage_source"`
	WeeklyPercent   *float64        `json:"weekly_limit_percent"`
	FiveHourPercent *float64        `json:"five_hour_limit_percent"`
	Credits         *string         `json:"balance_usage_credits"`
	Groups          []UsageRowGroup `json:"groups"`
}

// UsageRowGroup is a row's usage for one model, reasoning effort, and
// speed.
type UsageRowGroup struct {
	Model             string   `json:"model"`
	ReasoningEffort   string   `json:"reasoning_effort"`
	Speed             string   `json:"speed"`
	ProductExperience string   `json:"product_experience"`
	WeeklyPercent     *float64 `json:"weekly_limit_percent"`
	FiveHourPercent   *float64 `json:"five_hour_limit_percent"`
	Credits           *string  `json:"balance_usage_credits"`
}

// Row data statuses OpenAI reports.
const (
	UsageAvailable   = "available"
	UsagePartial     = "partial"
	UsageUnavailable = "unavailable"
)

// PlanUsageRowData is the body of a KindPlanUsageRow record: the newest
// row for a usage root. Final marks a row kept after the root settled.
type PlanUsageRowData struct {
	Row      UsageRow  `json:"row"`
	DataAsOf time.Time `json:"data_as_of"`
	Final    bool      `json:"final,omitzero"`
}

// PlanUsageOutcomeData is the body of a KindPlanUsageOutcome record: what
// a plan usage attempt found when it produced no row. Root names the usage
// root for OutcomeCountedUnder.
type PlanUsageOutcomeData struct {
	Kind string    `json:"kind"`
	At   time.Time `json:"at"`
	Root string    `json:"root,omitzero"`
}

// Plan usage outcome kinds.
const (
	// OutcomeLeftOut: the report left the root out.
	OutcomeLeftOut = "left_out"
	// OutcomeSettledNoRow: the root settled without ever getting a row.
	OutcomeSettledNoRow = "settled_no_row"
	// OutcomeAccountUnavailable: OpenAI does not offer plan usage to the
	// account (HTTP 403 or 404).
	OutcomeAccountUnavailable = "account_unavailable"
	// OutcomeCountedUnder: the thread is a descendant counted under Root.
	OutcomeCountedUnder = "counted_under"
	// OutcomeTooManyDescendants: the root's group is too large to send.
	OutcomeTooManyDescendants = "too_many_descendants"
	// OutcomeCouldNotCheck: no request was answered in the 7 days after
	// the root's last change.
	OutcomeCouldNotCheck = "could_not_check"
)

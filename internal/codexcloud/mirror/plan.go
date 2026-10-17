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

// UsageAvailable is the data status of a complete row. OpenAI also
// reports "partial" and "unavailable"; the fetcher treats every status
// other than this one as not yet usable.
const UsageAvailable = "available"

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
	// OutcomeCouldNotCheck: the requests after the 7 days since the
	// root's last change failed, 3 in a row. An answer before that point
	// does not prevent it.
	OutcomeCouldNotCheck = "could_not_check"
)

// Package friction ports jilog's session-review detectors (jilog 9e8e094,
// crates/jilog-review, MIT) onto agentsview data. It is pure: no database,
// no network. See docs/internal/jilog-adaptation.md for provenance.
package friction

import (
	"time"

	"go.kenn.io/agentsview/internal/signals"
)

// RulesVersion identifies the detector rule set. Bump it whenever a
// detector's output can change for the same input.
const RulesVersion = "friction-v1"

// Kind is a friction signal kind (jilog signal.rs:60-68).
type Kind string

const (
	KindCorrection Kind = "correction"
	KindError      Kind = "error"
	KindWorkaround Kind = "workaround"
	KindDeferral   Kind = "deferral"
	KindPattern    Kind = "pattern"
	// KindFrustration and KindInterruption are native kinds beyond jilog:
	// frustration markers and interrupted turns.
	KindFrustration  Kind = "frustration"
	KindInterruption Kind = "interruption"
)

// Subject kinds. Detectors emit session subjects; producer-written ledger
// events use diagnostic subjects and never raise P0.
const (
	SubjectSession    = "session"
	SubjectDiagnostic = "diagnostic"
)

// Detector names stored in friction_findings.detector.
const (
	DetectorCorrectionCoding = "correction.coding"
	DetectorCorrectionChat   = "correction.chat"
	DetectorError            = "error"
	DetectorWorkaround       = "workaround"
	DetectorDeferral         = "deferral"
	DetectorFrustration      = "frustration"
	DetectorInterruption     = "interruption"
)

// PatternDetector returns the detector name for a pattern kind.
func PatternDetector(patternKind string) string { return "pattern." + patternKind }

// Pattern kinds.
const (
	PatternRetryLoop         = "retry_loop"
	PatternRunawayLoop       = "runaway_loop"
	PatternEditChurn         = "edit_churn"
	PatternMidTaskCompaction = "mid_task_compaction"
	PatternContextPressure   = "context_pressure"
	PatternIterationRunaway  = "iteration_runaway"
)

// Thresholds ported from jilog detectors.rs:18-28,81 and health.rs:48.
const (
	MinCorrectionLength          = 15
	MaxCorrectionLength          = 200
	P0DistinctSessionThreshold   = 3
	IterationRunawayMinToolCalls = 150
	MaxErrorMessageLength        = 500
)

// Dims are the per-subject dimensions stamped onto signals by the review.
// Detectors leave them empty.
type Dims struct{ Seat, Agent, Machine, Persona, Channel string }

// Signal is one friction finding. It flattens jilog's five signal structs.
type Signal struct {
	Kind        Kind
	SubjectID   string // session ID, or diagnostic identity
	SubjectKind string // SubjectSession or SubjectDiagnostic
	Dims        Dims
	Detector    string
	Text        string // correction context, error message, or pattern description
	ToolName    string
	Label       string // workaround pattern, deferral item, or pattern kind
	Evidence    string
	Ordinal     *int
	CallIndex   *int
	OccurredAt  time.Time
	Seq         int
}

// Message is one entry of the detector stream with content flattened to text.
type Message struct {
	Ordinal       int
	Role          string // user, assistant, or tool
	Text          string
	ToolName      string
	CallIndex     int
	HadToolResult bool
	// Failed marks a tool message whose call failed. DetectErrors reads
	// only failed tool messages; Text is then the error text.
	Failed    bool
	Timestamp time.Time
	// NoiseName is the tool name used by the expected-noise rule. Empty
	// means ToolName. The session adapter sets it to "bash" for any Bash
	// category call.
	NoiseName string
}

// PatternInput carries the per-session tool and context facts read by
// pattern detectors.
type PatternInput struct {
	Calls              []signals.ToolCallRow
	CallTimes          []time.Time // parallel to Calls
	UserOrdinals       []int       // real user turns reset iteration runaway
	CompactBoundaries  []int
	BoundaryTimes      []time.Time
	MidTaskCompactions int
	PressureMax        *float64
	PressureAt         time.Time
}

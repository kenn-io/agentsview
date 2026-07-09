package recall

import "strings"

const (
	TypeFact            = "fact"
	TypeDecision        = "decision"
	TypeProcedure       = "procedure"
	TypeDebuggingMethod = "debugging_method"
	TypeWarning         = "warning"
	TypePreference      = "preference"
	TypeOpenQuestion    = "open_question"
)

const (
	ScopeGlobal     = "global"
	ScopeProject    = "project"
	ScopeRepository = "repository"
	ScopeBranch     = "branch"
	ScopeFile       = "file"
	ScopeTool       = "tool"
	ScopeAgent      = "agent"
)

const (
	StatusAccepted = "accepted"
	StatusArchived = "archived"
)

// LexicalScorePolicyVersion identifies the ranking policy whose scores are
// stored in recall query exposure snapshots. Bump it whenever score semantics
// change so calibration never compares unlike score distributions silently.
const LexicalScorePolicyVersion = "recall-lexical-v1"

const (
	ReviewStateHumanReviewed  = "human_reviewed"
	ReviewStateUnreviewedAuto = "unreviewed_auto"
	ReviewStateCalibratedAuto = "calibrated_auto"
	ReviewStateEvalRaw        = "eval_raw"
)

// NormalizeReviewState returns the canonical review state accepted at recall
// write boundaries. Empty values fail closed as unreviewed; unknown values
// fail validation.
func NormalizeReviewState(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return ReviewStateUnreviewedAuto, true
	}
	switch value {
	case ReviewStateHumanReviewed,
		ReviewStateUnreviewedAuto,
		ReviewStateCalibratedAuto,
		ReviewStateEvalRaw:
		return value, true
	default:
		return "", false
	}
}

type Entry struct {
	ID                  string
	Type                string
	Scope               string
	Status              string
	ReviewState         string
	Title               string
	Body                string
	Trigger             string
	Confidence          *float64
	Uncertainty         string
	Project             string
	CWD                 string
	GitBranch           string
	Agent               string
	SourceSessionID     string
	SourceEpisodeID     string
	SourceRunID         string
	SupersedesEntryID   string
	SupersededByEntryID string
	CreatedAt           string
	UpdatedAt           string
	Evidence            []Evidence
}

type Evidence struct {
	SessionID           string
	MessageStartOrdinal int
	MessageEndOrdinal   int
	ToolUseID           string
	Snippet             string
}

type Query struct {
	Text      string
	Project   string
	CWD       string
	GitBranch string
	Agent     string
	// Status restricts eligible entries to a single status. Empty means
	// the default accepted-only recall.
	Status string
	Limit  int
}

type Result struct {
	Entry        Entry
	Score        float64
	Breakdown    ScoreBreakdown
	MatchedTerms []string
}

type ScoreBreakdown struct {
	KeywordOverlap         int     `json:"keyword_overlap"`
	KeywordIDFScore        float64 `json:"keyword_idf_score"`
	EvidenceKeywordOverlap int     `json:"evidence_keyword_overlap"`
	EvidenceIDFScore       float64 `json:"evidence_idf_score"`
	IdentifierBoost        float64 `json:"identifier_boost"`
	PhraseBoost            float64 `json:"phrase_boost"`
	EntityBoost            float64 `json:"entity_boost"`
	TemporalBoost          float64 `json:"temporal_boost"`
	ConfidenceBonus        float64 `json:"confidence_bonus"`
	BaseScore              float64 `json:"base_score"`
	Total                  float64 `json:"total"`
}

type ContextOptions struct {
	MaxBytes      int
	MaxEntryBytes int
	FocusText     string
}

type ContextBlock struct {
	Text                              string
	EntryCount                        int
	Truncated                         bool
	IncludedIDs                       []string
	SourceSessionIDs                  []string
	SourceEpisodeIDs                  []string
	SourceRunIDs                      []string
	TruncatedFrom                     int
	OmittedCount                      int
	PromptInjectionContext            bool
	PromptInjectionContextIDs         []string
	PromptInjectionContextReasons     []string
	PromptInjectionContextReasonsByID map[string][]string
}

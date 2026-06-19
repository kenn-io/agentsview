package insight

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

const (
	CannedType          = "llm_canned"
	CannedSchemaVersion = "llm_insight.v1"
)

const maxCannedFieldRunes = 1200

// MaxCannedFocusRunes bounds optional user steering text. Canned
// recommendations remain fixed-template; this text can only focus the
// generated recommendation, not change the deterministic aggregate input.
const MaxCannedFocusRunes = 2000

// CannedKind is a fixed recommendation template identifier.
type CannedKind string

const (
	CannedPromptMaturityReview         CannedKind = "prompt_maturity_review"
	CannedContextSetupReview           CannedKind = "context_setup_review"
	CannedWorkflowHygieneReview        CannedKind = "workflow_hygiene_review"
	CannedToolReliabilityReview        CannedKind = "tool_reliability_review"
	CannedModelCostReview              CannedKind = "model_cost_review"
	CannedInstructionOpportunityReview CannedKind = "instruction_opportunity_review"
)

// ValidCannedKinds is the fixed template set for generated recommendations.
var ValidCannedKinds = map[CannedKind]bool{
	CannedPromptMaturityReview:         true,
	CannedContextSetupReview:           true,
	CannedWorkflowHygieneReview:        true,
	CannedToolReliabilityReview:        true,
	CannedModelCostReview:              true,
	CannedInstructionOpportunityReview: true,
}

type cannedTemplate struct {
	ID      string
	Version string
	Title   string
	Focus   string
}

var cannedTemplates = map[CannedKind]cannedTemplate{
	CannedPromptMaturityReview: {
		ID:      "prompt_maturity_review",
		Version: "2026-05-26",
		Title:   "Prompt Maturity Review",
		Focus: "Review prompt maturity using only the supplied deterministic aggregates and Coach-derived prompt maturity summary. " +
			"Focus on missing constraints, unclear starts, repeated asks, success criteria, and verification gaps.",
	},
	CannedContextSetupReview: {
		ID:      "context_setup_review",
		Version: "2026-05-27",
		Title:   "Context Setup Review",
		Focus: "Review context setup using only the supplied deterministic aggregates and Coach-derived spec/context signals. " +
			"Focus on compactions, mid-task context loss, spec-driven starts, missing code context, and context pressure when pressure data is available.",
	},
	CannedWorkflowHygieneReview: {
		ID:      "workflow_hygiene_review",
		Version: "2026-05-26",
		Title:   "Workflow Hygiene Review",
		Focus: "Review workflow hygiene using only the supplied deterministic aggregates and Coach-derived intent/workflow summary. " +
			"Focus on outcomes, abandonment, retries, repeated workflows, compactions, and sessions likely needing tighter loops.",
	},
	CannedToolReliabilityReview: {
		ID:      "tool_reliability_review",
		Version: "2026-05-26",
		Title:   "Tool Reliability Review",
		Focus: "Diagnose tool reliability using only the supplied deterministic aggregates. " +
			"Focus on failure signals, retries, edit churn, and likely process or environment causes.",
	},
	CannedModelCostReview: {
		ID:      "model_cost_review",
		Version: "2026-05-26",
		Title:   "Model and Cost Hygiene Review",
		Focus: "Review model and cost hygiene using only the supplied deterministic aggregates. " +
			"Focus on token mix, cache behavior, expensive sessions, and cost controls.",
	},
	CannedInstructionOpportunityReview: {
		ID:      "instruction_opportunity_review",
		Version: "2026-05-26",
		Title:   "Instruction Opportunity Review",
		Focus: "Suggest instruction, skill, or process improvements using only the supplied deterministic aggregates and Coach-derived repeated workflow clusters. " +
			"Label every suggestion as a draft opportunity, not a confirmed policy or installed skill.",
	},
}

// CannedAggregatePayload is the deterministic input sent to the LLM.
type CannedAggregatePayload struct {
	Kind           CannedKind                  `json:"kind"`
	DateFrom       string                      `json:"date_from"`
	DateTo         string                      `json:"date_to"`
	Project        string                      `json:"project,omitempty"`
	AutomatedScope string                      `json:"automated_scope"`
	Filters        CannedSessionFilters        `json:"filters"`
	Focus          string                      `json:"focus,omitempty"`
	Signals        db.SignalsAnalyticsResponse `json:"signals"`
	Usage          *CannedUsageSummary         `json:"usage,omitempty"`
	Coach          *CannedCoachSummary         `json:"coach,omitempty"`
	EvidenceRefs   []CannedEvidenceRef         `json:"evidence_refs"`
}

type CannedSessionFilters struct {
	Timezone        string `json:"timezone"`
	Machine         string `json:"machine,omitempty"`
	Agent           string `json:"agent,omitempty"`
	Termination     string `json:"termination,omitempty"`
	MinUserMessages int    `json:"min_user_messages,omitempty"`
	IncludeOneShot  bool   `json:"include_one_shot"`
	AutomatedScope  string `json:"automated_scope"`
	ActiveSince     string `json:"active_since,omitempty"`
}

func (f CannedSessionFilters) ProvenanceMap(project string) map[string]string {
	out := map[string]string{
		"project":           project,
		"timezone":          f.Timezone,
		"include_one_shot":  strconv.FormatBool(f.IncludeOneShot),
		"automated_scope":   f.AutomatedScope,
		"min_user_messages": strconv.Itoa(f.MinUserMessages),
	}
	if f.Machine != "" {
		out["machine"] = f.Machine
	}
	if f.Agent != "" {
		out["agent"] = f.Agent
	}
	if f.Termination != "" {
		out["termination"] = f.Termination
	}
	if f.ActiveSince != "" {
		out["active_since"] = f.ActiveSince
	}
	return out
}

type CannedUsageSummary struct {
	InputTokens         int                    `json:"input_tokens"`
	OutputTokens        int                    `json:"output_tokens"`
	CacheCreationTokens int                    `json:"cache_creation_tokens"`
	CacheReadTokens     int                    `json:"cache_read_tokens"`
	TotalCost           float64                `json:"total_cost"`
	CacheSavings        float64                `json:"cache_savings"`
	ModelBreakdowns     []CannedModelBreakdown `json:"model_breakdowns,omitempty"`
	TopSessionsByCost   []db.TopSessionEntry   `json:"top_sessions_by_cost,omitempty"`
}

type CannedModelBreakdown struct {
	ModelName           string  `json:"model_name"`
	InputTokens         int     `json:"input_tokens"`
	OutputTokens        int     `json:"output_tokens"`
	CacheCreationTokens int     `json:"cache_creation_tokens"`
	CacheReadTokens     int     `json:"cache_read_tokens"`
	Cost                float64 `json:"cost"`
}

type CannedEvidenceRef struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}

// CannedCoachSummary mirrors the AI Engineer Coach insight families
// with deterministic agentsview data. These fields are LLM inputs only;
// they are never written back to canonical session scores or signals.
type CannedCoachSummary struct {
	Source             string                       `json:"source"`
	SessionCount       int                          `json:"session_count"`
	IntentDistribution map[string]int               `json:"intent_distribution"`
	SpecDriven         CannedCoachSpecDriven        `json:"spec_driven"`
	PromptMaturity     CannedCoachPromptMaturity    `json:"prompt_maturity"`
	WorkflowClusters   []CannedCoachWorkflowCluster `json:"workflow_clusters,omitempty"`
}

type CannedCoachSpecDriven struct {
	EligibleSessions int     `json:"eligible_sessions"`
	Count            int     `json:"count"`
	Rate             float64 `json:"rate"`
}

type CannedCoachPromptMaturity struct {
	Score         int                       `json:"score"`
	Grade         string                    `json:"grade"`
	Dimensions    map[string]int            `json:"dimensions"`
	IssueCounts   map[string]int            `json:"issue_counts"`
	SamplePrompts []CannedCoachPromptSample `json:"sample_prompts,omitempty"`
}

type CannedCoachPromptSample struct {
	SessionID string   `json:"session_id"`
	Project   string   `json:"project"`
	Prompt    string   `json:"prompt"`
	Grade     string   `json:"grade"`
	Issues    []string `json:"issues"`
}

type CannedCoachWorkflowCluster struct {
	ID          string   `json:"id"`
	Label       string   `json:"label"`
	Occurrences int      `json:"occurrences"`
	Sessions    int      `json:"sessions"`
	Projects    []string `json:"projects"`
	Examples    []string `json:"examples"`
}

// CannedRecommendationEnvelope is the strict model-output schema.
type CannedRecommendationEnvelope struct {
	SchemaVersion   string                 `json:"schema_version"`
	Kind            CannedKind             `json:"kind"`
	Summary         string                 `json:"summary"`
	Confidence      string                 `json:"confidence"`
	Recommendations []CannedRecommendation `json:"recommendations"`
	Risks           []CannedRisk           `json:"risks"`
	EvidenceRefs    []string               `json:"evidence_refs"`
}

type CannedRecommendation struct {
	Title        string   `json:"title"`
	Rationale    string   `json:"rationale"`
	Actions      []string `json:"actions"`
	EvidenceRefs []string `json:"evidence_refs"`
	Impact       string   `json:"impact"`
	Effort       string   `json:"effort"`
}

type CannedRisk struct {
	Title        string   `json:"title"`
	Explanation  string   `json:"explanation"`
	EvidenceRefs []string `json:"evidence_refs"`
}

// CannedProvenance is server-owned metadata, not model output.
type CannedProvenance struct {
	TemplateID      string            `json:"template_id"`
	TemplateVersion string            `json:"template_version"`
	SchemaVersion   string            `json:"schema_version"`
	AggregateHash   string            `json:"aggregate_hash"`
	CacheKey        string            `json:"cache_key"`
	CacheStatus     string            `json:"cache_status"`
	GeneratedAt     string            `json:"generated_at"`
	DateFrom        string            `json:"date_from"`
	DateTo          string            `json:"date_to"`
	Project         string            `json:"project,omitempty"`
	Filters         map[string]string `json:"filters"`
	Agent           string            `json:"agent"`
	Model           string            `json:"model,omitempty"`
	SourceVersions  map[string]string `json:"source_versions"`
}

func CannedTemplate(kind CannedKind) (cannedTemplate, bool) {
	t, ok := cannedTemplates[kind]
	return t, ok
}

func CannedAggregateHash(payload CannedAggregatePayload) (string, error) {
	data, err := canonicalJSON(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// CannedCacheKey identifies reusable generated recommendation rows.
// Cache hits return the existing row with response-only hit metadata;
// force refresh bypasses this key lookup and saves a new fresh row.
func CannedCacheKey(
	kind CannedKind,
	dateFrom, dateTo, project, requestedAgent, focus, aggregateHash string,
	automatedScope string,
	filters CannedSessionFilters,
) (string, error) {
	t, ok := CannedTemplate(kind)
	if !ok {
		return "", fmt.Errorf("unknown canned insight kind: %s", kind)
	}
	focusSum := sha256.Sum256([]byte(strings.TrimSpace(focus)))
	filterData, err := canonicalJSON(filters)
	if err != nil {
		return "", err
	}
	filterSum := sha256.Sum256(filterData)
	input := map[string]string{
		"kind":             string(kind),
		"date_from":        dateFrom,
		"date_to":          dateTo,
		"project":          project,
		"requested_agent":  requestedAgent,
		"template_version": t.Version,
		"schema_version":   CannedSchemaVersion,
		"aggregate_hash":   aggregateHash,
		"focus_hash":       hex.EncodeToString(focusSum[:]),
		"automated_scope":  automatedScope,
		"filter_hash":      hex.EncodeToString(filterSum[:]),
	}
	data, err := canonicalJSON(input)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return string(kind) + ":" + hex.EncodeToString(sum[:]), nil
}

func BuildCannedPrompt(
	payload CannedAggregatePayload,
	aggregateHash string,
) (string, error) {
	t, ok := CannedTemplate(payload.Kind)
	if !ok {
		return "", fmt.Errorf("unknown canned insight kind: %s", payload.Kind)
	}
	data, err := canonicalJSON(payload)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString("You are generating an opt-in AI recommendation for agentsview.\n")
	b.WriteString("Use only the deterministic aggregate JSON supplied below. ")
	b.WriteString("Do not inspect or infer from raw transcripts. ")
	b.WriteString("Do not recalculate, override, or propose changes to canonical health scores or signal rows. ")
	b.WriteString("Output JSON only, with no markdown fences or prose outside JSON.\n\n")
	b.WriteString("Template: ")
	b.WriteString(t.Title)
	b.WriteString("\nTemplate ID: ")
	b.WriteString(t.ID)
	b.WriteString("\nTemplate Version: ")
	b.WriteString(t.Version)
	b.WriteString("\nSchema Version: ")
	b.WriteString(CannedSchemaVersion)
	b.WriteString("\nAggregate Hash: ")
	b.WriteString(aggregateHash)
	b.WriteString("\n\nFocus:\n")
	b.WriteString(t.Focus)
	if strings.TrimSpace(payload.Focus) != "" {
		b.WriteString("\n\nUser focus:\n")
		b.WriteString("Use this only to prioritize within the fixed template and supplied aggregates: ")
		b.WriteString(strings.TrimSpace(payload.Focus))
	}
	b.WriteString("\n\nRequired JSON shape:\n")
	b.WriteString(`{"schema_version":"llm_insight.v1","kind":"`)
	b.WriteString(string(payload.Kind))
	b.WriteString(`","summary":"...","confidence":"low|medium|high","recommendations":[{"title":"...","rationale":"...","actions":["..."],"evidence_refs":["..."],"impact":"low|medium|high","effort":"low|medium|high"}],"risks":[{"title":"...","explanation":"...","evidence_refs":["..."]}],"evidence_refs":["..."]}`)
	b.WriteString("\n\nRules:\n")
	b.WriteString("- Include 1 to 5 recommendations.\n")
	b.WriteString("- Include at most 4 risks.\n")
	b.WriteString("- Every recommendation and risk must cite evidence_refs from the aggregate payload.\n")
	b.WriteString("- Do not emit evidence refs that are absent from the valid evidence_ref list below.\n")
	b.WriteString("- Copy evidence_ref IDs exactly; do not invent more specific sub-refs.\n")
	b.WriteString("- Keep fields concise and action-oriented.\n")
	b.WriteString("- If evidence is weak or empty, lower confidence and say what deterministic data is missing.\n\n")
	writeCannedKindRules(&b, payload)
	b.WriteString("Valid evidence_ref IDs for this request:\n")
	if len(payload.EvidenceRefs) == 0 {
		b.WriteString("- aggregate:empty\n")
	} else {
		for _, ref := range payload.EvidenceRefs {
			b.WriteString("- ")
			b.WriteString(ref.ID)
			b.WriteString("\n")
		}
	}
	b.WriteString("\n")
	b.WriteString("Aggregate payload JSON:\n")
	b.Write(data)
	return b.String(), nil
}

func writeCannedKindRules(
	b *strings.Builder,
	payload CannedAggregatePayload,
) {
	switch payload.Kind {
	case CannedContextSetupReview:
		ctx := payload.Signals.ContextHealth
		b.WriteString("Context setup template rules:\n")
		b.WriteString("- Treat context pressure as an optional signal, not a required finding.\n")
		if ctx.SessionsWithContextData == 0 || ctx.AvgContextPressure == nil {
			b.WriteString("- Context pressure coverage is zero or unavailable for this request; do not make pressure-related recommendations or risks.\n")
			b.WriteString("- Do not use missing context pressure telemetry as the main recommendation. Prefer compactions, mid-task compactions, missing code context, spec-driven starts, and prompt maturity signals when those aggregates are present.\n")
			b.WriteString("- If no non-pressure context setup signals are present, set confidence to low and state that deterministic context setup evidence is weak.\n")
		} else {
			b.WriteString("- Context pressure coverage is present; pressure conclusions must cite the aggregate pressure fields and stay proportional to the covered session count.\n")
		}
		b.WriteString("\n")
	}
}

func ParseCannedEnvelope(raw string) (CannedRecommendationEnvelope, error) {
	clean := strings.TrimSpace(raw)
	clean = strings.TrimPrefix(clean, "```json")
	clean = strings.TrimPrefix(clean, "```")
	clean = strings.TrimSuffix(clean, "```")
	clean = strings.TrimSpace(clean)

	var out CannedRecommendationEnvelope
	dec := json.NewDecoder(strings.NewReader(clean))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return out, fmt.Errorf("parsing canned insight JSON: %w", err)
	}
	var extra struct{}
	if err := dec.Decode(&extra); err == nil {
		return out, errors.New("canned insight JSON contains trailing values")
	} else if !errors.Is(err, io.EOF) {
		return out, fmt.Errorf("canned insight JSON has trailing data: %w", err)
	}
	return out, nil
}

func ValidateCannedEnvelope(
	out CannedRecommendationEnvelope,
	payload CannedAggregatePayload,
) error {
	if out.SchemaVersion != CannedSchemaVersion {
		return fmt.Errorf("schema_version = %q, want %q", out.SchemaVersion, CannedSchemaVersion)
	}
	if out.Kind != payload.Kind {
		return fmt.Errorf("kind = %q, want %q", out.Kind, payload.Kind)
	}
	if !validConfidence(out.Confidence) {
		return fmt.Errorf("invalid confidence: %q", out.Confidence)
	}
	if strings.TrimSpace(out.Summary) == "" {
		return errors.New("summary is required")
	}
	if len(out.Recommendations) == 0 || len(out.Recommendations) > 5 {
		return fmt.Errorf("recommendations count = %d, want 1..5", len(out.Recommendations))
	}
	if len(out.Risks) > 4 {
		return fmt.Errorf("risks count = %d, want <= 4", len(out.Risks))
	}

	allowed := make(map[string]bool, len(payload.EvidenceRefs))
	for _, ref := range payload.EvidenceRefs {
		allowed[ref.ID] = true
	}
	if len(allowed) == 0 {
		allowed["aggregate:empty"] = true
	}

	for _, ref := range out.EvidenceRefs {
		if !allowed[ref] {
			return fmt.Errorf("unknown envelope evidence_ref: %s", ref)
		}
	}
	for i, rec := range out.Recommendations {
		if err := validateCannedRecommendation(rec, allowed); err != nil {
			return fmt.Errorf("recommendation %d: %w", i, err)
		}
	}
	for i, risk := range out.Risks {
		if err := validateCannedRisk(risk, allowed); err != nil {
			return fmt.Errorf("risk %d: %w", i, err)
		}
	}
	return validateCannedFieldSizes(out)
}

func RenderCannedMarkdown(
	out CannedRecommendationEnvelope,
	prov CannedProvenance,
) string {
	var b strings.Builder
	b.WriteString("# AI-generated recommendation\n\n")
	b.WriteString("> Generated recommendation text. Deterministic health scores and signal rows were not modified.\n\n")
	b.WriteString("## Summary\n\n")
	b.WriteString(out.Summary)
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "_Kind: %s. Confidence: %s. Template: %s@%s. Aggregate: `%s`._\n\n",
		out.Kind, out.Confidence, prov.TemplateID, prov.TemplateVersion,
		shortHash(prov.AggregateHash))
	b.WriteString("## Recommendations\n\n")
	for i, rec := range out.Recommendations {
		fmt.Fprintf(&b, "%d. **%s**\n", i+1, rec.Title)
		fmt.Fprintf(&b, "   - Rationale: %s\n", rec.Rationale)
		fmt.Fprintf(&b, "   - Impact: %s. Effort: %s.\n", rec.Impact, rec.Effort)
		if len(rec.Actions) > 0 {
			b.WriteString("   - Actions:\n")
			for _, action := range rec.Actions {
				fmt.Fprintf(&b, "     - %s\n", action)
			}
		}
		if len(rec.EvidenceRefs) > 0 {
			fmt.Fprintf(&b, "   - Evidence: `%s`\n", strings.Join(rec.EvidenceRefs, "`, `"))
		}
	}
	if len(out.Risks) > 0 {
		b.WriteString("\n## Risks\n\n")
		for _, risk := range out.Risks {
			fmt.Fprintf(&b, "- **%s**: %s", risk.Title, risk.Explanation)
			if len(risk.EvidenceRefs) > 0 {
				fmt.Fprintf(&b, " (`%s`)", strings.Join(risk.EvidenceRefs, "`, `"))
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

func NewCannedProvenance(
	payload CannedAggregatePayload,
	aggregateHash, cacheKey, cacheStatus, agent, model string,
	generatedAt time.Time,
) (CannedProvenance, error) {
	t, ok := CannedTemplate(payload.Kind)
	if !ok {
		return CannedProvenance{}, fmt.Errorf("unknown canned insight kind: %s", payload.Kind)
	}
	return CannedProvenance{
		TemplateID:      t.ID,
		TemplateVersion: t.Version,
		SchemaVersion:   CannedSchemaVersion,
		AggregateHash:   aggregateHash,
		CacheKey:        cacheKey,
		CacheStatus:     cacheStatus,
		GeneratedAt:     generatedAt.UTC().Format(time.RFC3339Nano),
		DateFrom:        payload.DateFrom,
		DateTo:          payload.DateTo,
		Project:         payload.Project,
		Filters:         payload.Filters.ProvenanceMap(payload.Project),
		Agent:           agent,
		Model:           model,
		SourceVersions: map[string]string{
			"signals_analytics": "v1",
			"usage_analytics":   "v1",
			"coach_insights":    "AI-Engineering-Coach-inspired.v1",
		},
	}, nil
}

func canonicalJSON(v any) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func validConfidence(v string) bool {
	switch v {
	case "low", "medium", "high":
		return true
	default:
		return false
	}
}

func validateCannedRecommendation(
	rec CannedRecommendation,
	allowed map[string]bool,
) error {
	if strings.TrimSpace(rec.Title) == "" {
		return errors.New("title is required")
	}
	if strings.TrimSpace(rec.Rationale) == "" {
		return errors.New("rationale is required")
	}
	if len(rec.Actions) == 0 || len(rec.Actions) > 5 {
		return fmt.Errorf("actions count = %d, want 1..5", len(rec.Actions))
	}
	if !validConfidence(rec.Impact) {
		return fmt.Errorf("invalid impact: %q", rec.Impact)
	}
	if !validConfidence(rec.Effort) {
		return fmt.Errorf("invalid effort: %q", rec.Effort)
	}
	if len(rec.EvidenceRefs) == 0 {
		return errors.New("evidence_refs is required")
	}
	return validateEvidenceRefs(rec.EvidenceRefs, allowed)
}

func validateCannedRisk(risk CannedRisk, allowed map[string]bool) error {
	if strings.TrimSpace(risk.Title) == "" {
		return errors.New("title is required")
	}
	if strings.TrimSpace(risk.Explanation) == "" {
		return errors.New("explanation is required")
	}
	if len(risk.EvidenceRefs) == 0 {
		return errors.New("evidence_refs is required")
	}
	return validateEvidenceRefs(risk.EvidenceRefs, allowed)
}

func validateEvidenceRefs(refs []string, allowed map[string]bool) error {
	for _, ref := range refs {
		if !allowed[ref] {
			return fmt.Errorf("unknown evidence_ref: %s", ref)
		}
	}
	return nil
}

func validateCannedFieldSizes(out CannedRecommendationEnvelope) error {
	var fields []string
	fields = append(fields, out.Summary)
	for _, rec := range out.Recommendations {
		fields = append(fields, rec.Title, rec.Rationale, rec.Impact, rec.Effort)
		fields = append(fields, rec.Actions...)
	}
	for _, risk := range out.Risks {
		fields = append(fields, risk.Title, risk.Explanation)
	}
	for _, f := range fields {
		if len([]rune(f)) > maxCannedFieldRunes {
			return errors.New("canned insight field exceeds maximum length")
		}
	}
	return nil
}

func shortHash(hash string) string {
	if len(hash) <= 12 {
		return hash
	}
	return hash[:12]
}

func BuildCannedCoachSummary(sessions []db.Session) *CannedCoachSummary {
	summary := &CannedCoachSummary{
		Source:       "AI-Engineering-Coach deterministic insight model",
		SessionCount: len(sessions),
		IntentDistribution: map[string]int{
			"Planning":       0,
			"Implementation": 0,
			"Debugging":      0,
			"Review":         0,
			"Exploration":    0,
		},
	}
	if len(sessions) == 0 {
		summary.PromptMaturity = emptyCoachPromptMaturity()
		return summary
	}

	var dimensionSums coachPromptDimensions
	issueCounts := make(map[string]int)
	var scoredPrompts int
	var samples []CannedCoachPromptSample
	for _, s := range sessions {
		prompt := firstPromptText(s)
		if prompt == "" {
			continue
		}
		summary.IntentDistribution[classifyCoachIntent(prompt, s)]++
		if s.UserMessageCount >= 3 {
			summary.SpecDriven.EligibleSessions++
			if isCoachSpecDriven(prompt) ||
				(s.QualitySignalVersion > 0 && !s.UnstructuredStart &&
					s.MissingSuccessCriteriaCount == 0) {
				summary.SpecDriven.Count++
			}
		}
		score := gradeCoachPrompt(prompt, s)
		dimensionSums.add(score.dimensions)
		for _, issue := range score.issues {
			issueCounts[issue]++
		}
		scoredPrompts++
		if len(samples) < 8 && len(score.issues) > 0 {
			samples = append(samples, CannedCoachPromptSample{
				SessionID: s.ID,
				Project:   s.Project,
				Prompt:    truncateRunes(prompt, 240),
				Grade:     scoreToCoachGrade(score.total),
				Issues:    score.issues,
			})
		}
	}

	if summary.SpecDriven.EligibleSessions > 0 {
		summary.SpecDriven.Rate = round1(
			float64(summary.SpecDriven.Count) /
				float64(summary.SpecDriven.EligibleSessions) *
				100,
		)
	}
	summary.PromptMaturity = buildCoachPromptMaturity(
		dimensionSums, issueCounts, samples, scoredPrompts,
	)
	summary.WorkflowClusters = buildCoachWorkflowClusters(sessions)
	return summary
}

type coachPromptDimensions struct {
	constraints       int
	successCriteria   int
	verificationSteps int
	contextProvision  int
	specificity       int
}

type coachPromptScore struct {
	dimensions coachPromptDimensions
	total      int
	issues     []string
}

func (d *coachPromptDimensions) add(other coachPromptDimensions) {
	d.constraints += other.constraints
	d.successCriteria += other.successCriteria
	d.verificationSteps += other.verificationSteps
	d.contextProvision += other.contextProvision
	d.specificity += other.specificity
}

func emptyCoachPromptMaturity() CannedCoachPromptMaturity {
	return CannedCoachPromptMaturity{
		Score:       0,
		Grade:       "F",
		Dimensions:  emptyCoachDimensions(),
		IssueCounts: map[string]int{},
	}
}

func buildCoachPromptMaturity(
	sums coachPromptDimensions,
	issueCounts map[string]int,
	samples []CannedCoachPromptSample,
	count int,
) CannedCoachPromptMaturity {
	if count == 0 {
		return emptyCoachPromptMaturity()
	}
	dims := map[string]int{
		"constraints":        sums.constraints / count,
		"success_criteria":   sums.successCriteria / count,
		"verification_steps": sums.verificationSteps / count,
		"context_provision":  sums.contextProvision / count,
		"specificity":        sums.specificity / count,
	}
	total := 0
	for _, v := range dims {
		total += v
	}
	score := total / len(dims)
	return CannedCoachPromptMaturity{
		Score:         score,
		Grade:         scoreToCoachGrade(score),
		Dimensions:    dims,
		IssueCounts:   issueCounts,
		SamplePrompts: samples,
	}
}

func emptyCoachDimensions() map[string]int {
	return map[string]int{
		"constraints":        0,
		"success_criteria":   0,
		"verification_steps": 0,
		"context_provision":  0,
		"specificity":        0,
	}
}

func gradeCoachPrompt(msg string, s db.Session) coachPromptScore {
	msg = strings.TrimSpace(msg)
	var issues []string
	dims := coachPromptDimensions{}
	lower := strings.ToLower(msg)
	if containsAny(lower, []string{"must", "should", "shall", "only", "no more than", "at most", "at least", "limit", "constraint", "require", "restrict"}) {
		dims.constraints = 100
	} else {
		issues = append(issues, "No constraints specified")
	}
	if containsAny(lower, []string{"expect", "success", "criteria", "acceptance", "should return", "should output", "output should", "result should"}) {
		dims.successCriteria = 100
	} else {
		issues = append(issues, "No success criteria")
	}
	if containsAny(lower, []string{"test", "verify", "validate", "check", "confirm", "ensure", "assert", "prove"}) {
		dims.verificationSteps = 100
	} else {
		issues = append(issues, "No verification steps")
	}
	if s.HasContextData || s.HasToolCalls ||
		(s.QualitySignalVersion > 0 && s.NoCodeContextCount == 0) {
		dims.contextProvision += 50
	}
	if strings.Contains(msg, "`") || strings.Contains(msg, "\n") {
		dims.contextProvision += 30
	}
	if strings.Count(msg, "\n") >= 2 {
		dims.contextProvision += 20
	}
	if dims.contextProvision == 0 {
		issues = append(issues, "No context provided")
	}
	if len([]rune(msg)) >= 100 {
		dims.specificity += 40
	} else if len([]rune(msg)) >= 50 {
		dims.specificity += 20
	}
	if strings.Contains(msg, "\n- ") || strings.Contains(msg, "\n* ") {
		dims.specificity += 30
	}
	if strings.Count(msg, "\n") >= 3 {
		dims.specificity += 30
	}
	if dims.specificity == 0 {
		issues = append(issues, "Very short or vague prompt")
	}
	if dims.contextProvision > 100 {
		dims.contextProvision = 100
	}
	if dims.specificity > 100 {
		dims.specificity = 100
	}
	total := (dims.constraints + dims.successCriteria +
		dims.verificationSteps + dims.contextProvision +
		dims.specificity) / 5
	return coachPromptScore{dimensions: dims, total: total, issues: issues}
}

func classifyCoachIntent(prompt string, s db.Session) string {
	lower := strings.ToLower(prompt)
	scores := map[string]int{
		"Planning":       0,
		"Implementation": 0,
		"Debugging":      0,
		"Review":         0,
		"Exploration":    0,
	}
	if containsAny(lower, []string{"plan", "architect", "design", "outline", "approach", "strategy", "scope", "breakdown", "roadmap", "rfc", "spec", "proposal"}) {
		scores["Planning"]++
	}
	if containsAny(lower, []string{"fix", "bug", "error", "exception", "crash", "debug", "stacktrace", "trace", "issue", "broken", "fail", "wrong", "not working", "panic"}) || s.ToolFailureSignalCount > 0 {
		scores["Debugging"]++
	}
	if containsAny(lower, []string{"review", "explain", "understand", "what does", "how does", "walk me through", "read", "audit", "analyze", "inspect", "clarify", "describe"}) {
		scores["Review"]++
	}
	if containsAny(lower, []string{"how to", "what is", "can i", "learn", "explore", "example", "tutorial", "demo", "try", "experiment", "compare", "research", "playground"}) {
		scores["Exploration"]++
	}
	if s.EditChurnCount > 0 || s.HasToolCalls ||
		containsAny(lower, []string{"add", "implement", "build", "create", "refactor", "test", "update"}) {
		scores["Implementation"]++
	}

	best := "Implementation"
	bestScore := -1
	for _, key := range []string{"Planning", "Implementation", "Debugging", "Review", "Exploration"} {
		if scores[key] > bestScore {
			best = key
			bestScore = scores[key]
		}
	}
	return best
}

func isCoachSpecDriven(prompt string) bool {
	lower := strings.ToLower(prompt)
	if containsAny(lower, []string{"spec", "requirements", "requirement", "acceptance criteria", "design doc", "prd", "rfc", "plan file", "constraint", "must", "should", "ensure"}) {
		return true
	}
	lines := strings.Split(prompt, "\n")
	nonEmpty := 0
	hasStructured := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		nonEmpty++
		if strings.HasPrefix(line, "- ") ||
			strings.HasPrefix(line, "* ") ||
			startsWithNumberedList(line) ||
			strings.HasPrefix(line, "#") {
			hasStructured = true
		}
	}
	return hasStructured && nonEmpty >= 3
}

func buildCoachWorkflowClusters(sessions []db.Session) []CannedCoachWorkflowCluster {
	type rec struct {
		sessionID string
		project   string
		text      string
		norm      string
		tokens    []string
	}
	type clusterCandidate struct {
		key     string
		cluster CannedCoachWorkflowCluster
	}
	buckets := make(map[string][]rec)
	for _, s := range sessions {
		text := firstPromptText(s)
		if len([]rune(text)) < 15 || isCoachPromptNoise(text) {
			continue
		}
		norm := normalizeCoachPrompt(text)
		tokens := tokenizeCoachPrompt(norm)
		if len(tokens) < 2 {
			continue
		}
		sort.Strings(tokens)
		keyTokens := tokens
		if len(keyTokens) > 4 {
			keyTokens = keyTokens[:4]
		}
		key := strings.Join(keyTokens, "|")
		buckets[key] = append(buckets[key], rec{
			sessionID: s.ID,
			project:   s.Project,
			text:      text,
			norm:      norm,
			tokens:    tokens,
		})
	}

	var candidates []clusterCandidate
	for key, members := range buckets {
		if len(members) < 3 {
			continue
		}
		sort.Slice(members, func(i, j int) bool {
			return len(members[i].text) < len(members[j].text)
		})
		label := members[0].text
		for _, member := range members {
			if len([]rune(member.text)) >= 20 {
				label = member.text
				break
			}
		}
		projects := make(map[string]bool)
		sessionIDs := make(map[string]bool)
		seenExamples := make(map[string]bool)
		var examples []string
		for _, member := range members {
			projects[member.project] = true
			sessionIDs[member.sessionID] = true
			if !seenExamples[member.norm] && len(examples) < 5 {
				seenExamples[member.norm] = true
				examples = append(examples, truncateRunes(member.text, 150))
			}
		}
		candidates = append(candidates, clusterCandidate{
			key: key,
			cluster: CannedCoachWorkflowCluster{
				ID:          coachWorkflowClusterID(key),
				Label:       truncateRunes(label, 100),
				Occurrences: len(members),
				Sessions:    len(sessionIDs),
				Projects:    sortedKeys(projects),
				Examples:    examples,
			},
		})
	}
	sort.Slice(candidates, func(i, j int) bool {
		left := candidates[i].cluster
		right := candidates[j].cluster
		if left.Occurrences != right.Occurrences {
			return left.Occurrences > right.Occurrences
		}
		if left.Label != right.Label {
			return left.Label < right.Label
		}
		if strings.Join(left.Projects, "\x00") != strings.Join(right.Projects, "\x00") {
			return strings.Join(left.Projects, "\x00") < strings.Join(right.Projects, "\x00")
		}
		return candidates[i].key < candidates[j].key
	})
	clusters := make([]CannedCoachWorkflowCluster, 0, len(candidates))
	for _, candidate := range candidates {
		clusters = append(clusters, candidate.cluster)
	}
	if len(clusters) > 10 {
		clusters = clusters[:10]
	}
	return clusters
}

func coachWorkflowClusterID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "coach-workflow-" + hex.EncodeToString(sum[:])[:12]
}

func firstPromptText(s db.Session) string {
	if s.FirstMessage == nil {
		return ""
	}
	return strings.TrimSpace(*s.FirstMessage)
}

func normalizeCoachPrompt(raw string) string {
	lower := strings.ToLower(raw)
	parts := strings.FieldsFunc(lower, func(r rune) bool {
		return r < 'a' || r > 'z'
	})
	return strings.Join(parts, " ")
}

func tokenizeCoachPrompt(normalized string) []string {
	stop := map[string]bool{
		"the": true, "a": true, "an": true, "is": true,
		"are": true, "was": true, "were": true, "be": true,
		"have": true, "has": true, "had": true, "do": true,
		"does": true, "did": true, "will": true, "would": true,
		"could": true, "should": true, "can": true, "to": true,
		"of": true, "in": true, "for": true, "on": true,
		"with": true, "at": true, "by": true, "from": true,
		"it": true, "this": true, "that": true, "my": true,
		"me": true, "i": true, "we": true, "you": true,
		"and": true, "or": true, "but": true, "not": true,
		"if": true, "then": true, "so": true, "just": true,
		"please": true, "also": true, "make": true,
		"sure": true, "want": true, "need": true,
		"like": true,
	}
	var out []string
	for token := range strings.FieldsSeq(normalized) {
		if len(token) > 1 && !stop[token] {
			out = append(out, token)
		}
	}
	return out
}

func isCoachPromptNoise(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	if len([]rune(lower)) > 2000 {
		return true
	}
	switch lower {
	case "y", "n", "yes", "no", "continue", "try again", "retry", "stop", "abort":
		return true
	}
	return strings.Contains(lower, "continue to iterate") &&
		len([]rune(lower)) < 80
}

func startsWithNumberedList(line string) bool {
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	return i > 0 && i+1 < len(line) &&
		(line[i] == '.' || line[i] == ')') && line[i+1] == ' '
}

func containsAny(s string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

func scoreToCoachGrade(score int) string {
	switch {
	case score >= 80:
		return "A"
	case score >= 60:
		return "B"
	case score >= 40:
		return "C"
	case score >= 20:
		return "D"
	default:
		return "F"
	}
}

func sortedKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		if key != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

func truncateRunes(s string, maxRunes int) string {
	runes := []rune(strings.TrimSpace(s))
	if len(runes) <= maxRunes {
		return string(runes)
	}
	return string(runes[:maxRunes-3]) + "..."
}

func round1(v float64) float64 {
	if v >= 0 {
		return float64(int(v*10+0.5)) / 10
	}
	return float64(int(v*10-0.5)) / 10
}

func CannedEvidenceRefs(
	signals db.SignalsAnalyticsResponse,
	usage *CannedUsageSummary,
	coach *CannedCoachSummary,
) []CannedEvidenceRef {
	usageHasData := usage != nil &&
		(usage.InputTokens > 0 ||
			usage.OutputTokens > 0 ||
			usage.CacheCreationTokens > 0 ||
			usage.CacheReadTokens > 0 ||
			usage.TotalCost != 0 ||
			usage.CacheSavings != 0 ||
			len(usage.ModelBreakdowns) > 0 ||
			len(usage.TopSessionsByCost) > 0)
	refs := []CannedEvidenceRef{
		{ID: "signals:score_distribution", Description: "Scored, unscored, average health score, and grade distribution."},
		{ID: "signals:outcomes", Description: "Outcome and outcome confidence distribution."},
		{ID: "signals:tool_health", Description: "Tool failure, retry, edit churn, and failure-rate aggregates."},
		{ID: "signals:context_health", Description: "Compaction, context pressure, and context-data aggregates."},
		{ID: "signals:trend", Description: "Daily signal trend buckets."},
	}
	if len(signals.ByAgent) > 0 {
		refs = append(refs, CannedEvidenceRef{ID: "signals:by_agent", Description: "Signal aggregates grouped by agent."})
	}
	if len(signals.ByProject) > 0 {
		refs = append(refs, CannedEvidenceRef{ID: "signals:by_project", Description: "Signal aggregates grouped by project."})
	}
	if usage != nil {
		refs = append(refs, CannedEvidenceRef{ID: "usage:totals", Description: "Deterministic token, cache, and cost totals."})
		if len(usage.ModelBreakdowns) > 0 {
			refs = append(refs, CannedEvidenceRef{ID: "usage:model_breakdown", Description: "Deterministic per-model token and cost totals."})
		}
		if len(usage.TopSessionsByCost) > 0 {
			refs = append(refs, CannedEvidenceRef{ID: "usage:top_sessions_by_cost", Description: "Deterministic top sessions by cost."})
		}
	}
	if coach != nil && coach.SessionCount > 0 {
		refs = append(refs,
			CannedEvidenceRef{ID: "coach:intent_distribution", Description: "AI Engineer Coach-style deterministic intent classification."},
			CannedEvidenceRef{ID: "coach:prompt_maturity", Description: "AI Engineer Coach-style prompt maturity dimensions and issue counts."},
			CannedEvidenceRef{ID: "coach:spec_driven", Description: "AI Engineer Coach-style spec-driven start rate."},
		)
		if len(coach.WorkflowClusters) > 0 {
			refs = append(refs, CannedEvidenceRef{ID: "coach:workflow_clusters", Description: "AI Engineer Coach-style repeated workflow clusters for skill or instruction opportunities."})
		}
	}
	if signals.ScoredSessions == 0 && signals.UnscoredSessions == 0 &&
		!usageHasData && (coach == nil || coach.SessionCount == 0) {
		return []CannedEvidenceRef{{ID: "aggregate:empty", Description: "No deterministic aggregate rows matched the selected filters."}}
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].ID < refs[j].ID })
	return refs
}

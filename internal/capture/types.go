// Package capture implements bounded, recoverable usage capture for one exact
// non-interactive provider execution.
package capture

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

const (
	ResultSchemaName    = "agentsview.one-shot-usage"
	ResultSchemaVersion = 1
)

type Schema struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
}

type Result struct {
	Schema       Schema             `json:"schema"`
	OccurrenceID string             `json:"occurrence_id"`
	Provider     ProviderIdentity   `json:"provider"`
	Execution    ExecutionOutcome   `json:"execution"`
	Usage        *TokenUsage        `json:"usage,omitempty"`
	Cost         *Cost              `json:"cost,omitempty"`
	Models       []string           `json:"models"`
	Sources      []SourceProvenance `json:"sources"`
	Assurance    Assurance          `json:"assurance"`
	Reporting    ReportingOutcome   `json:"reporting"`
	Producer     ProducerMetadata   `json:"producer"`
}

type ProviderIdentity struct {
	Name               string     `json:"name"`
	SessionID          string     `json:"session_id,omitempty"`
	IncludedSessionIDs []string   `json:"included_session_ids"`
	StartedAt          *time.Time `json:"started_at,omitempty"`
	CompletedAt        *time.Time `json:"completed_at,omitempty"`
}

type ExecutionOutcome struct {
	StartedAt   time.Time  `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	ExitCode    *int       `json:"exit_code,omitempty"`
	Signal      string     `json:"signal,omitempty"`
}

// TokenUsage uses the canonical token-category field names. Pointers are
// intentional: a present zero is proven zero, while an omitted field is
// unavailable from this producer and parser version.
type TokenUsage struct {
	InputTokens              *int `json:"input_tokens,omitempty"`
	OutputTokens             *int `json:"output_tokens,omitempty"`
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     *int `json:"cache_read_input_tokens,omitempty"`
	ReasoningOutputTokens    *int `json:"reasoning_output_tokens,omitempty"`
}

type Cost struct {
	Amount   money.Money       `json:"amount"`
	Currency string            `json:"currency"`
	Source   export.CostSource `json:"source"`
}

// SourceProvenance links reported facts to provider bytes without exposing a
// sensitive provider-relative path in the usage result.
type SourceProvenance struct {
	SessionID string `json:"session_id"`
	SHA256    string `json:"sha256"`
	Bytes     int64  `json:"bytes"`
}

type AssuranceState string

const (
	AssuranceComplete    AssuranceState = "complete"
	AssurancePartial     AssuranceState = "partial"
	AssuranceUnavailable AssuranceState = "unavailable"
)

type ReasonCode string

const (
	ReasonNoSession              ReasonCode = "no_session"
	ReasonMultipleSessions       ReasonCode = "multiple_sessions"
	ReasonUnfinishedSession      ReasonCode = "unfinished_session"
	ReasonMalformedTranscript    ReasonCode = "malformed_transcript"
	ReasonFinalizationTimeout    ReasonCode = "finalization_timeout"
	ReasonChildStartFailed       ReasonCode = "child_start_failed"
	ReasonCorrelationUnavailable ReasonCode = "correlation_unavailable"
	ReasonCorrelationConflict    ReasonCode = "correlation_conflict"
	ReasonSourceLimit            ReasonCode = "source_limit"
	ReasonSourceBytesLimit       ReasonCode = "source_bytes_limit"
	ReasonSourceUnavailable      ReasonCode = "source_unavailable"
	ReasonResultSizeLimit        ReasonCode = "result_size_limit"
	ReasonMetadataTruncated      ReasonCode = "metadata_truncated"
	ReasonIngestFailed           ReasonCode = "ingest_failed"
	ReasonUsageUnavailable       ReasonCode = "usage_unavailable"
	ReasonCostUnavailable        ReasonCode = "cost_unavailable"
	ReasonUnpricedModel          ReasonCode = "unpriced_model"
	ReasonCodexCacheWriteAbsent  ReasonCode = "codex_cache_write_unavailable"
	ReasonReasoningAbsent        ReasonCode = "reasoning_output_unavailable"
)

type Assurance struct {
	State   AssuranceState `json:"state"`
	Reasons []ReasonCode   `json:"reasons"`
}

type ReportingState string

const (
	ReportingComplete ReportingState = "complete"
	ReportingFailed   ReportingState = "failed"
)

type ReportingOutcome struct {
	Outcome ReportingState `json:"outcome"`
	Reason  ReasonCode     `json:"reason,omitempty"`
}

type ProducerMetadata struct {
	AgentsViewVersion string `json:"agentsview_version"`
	ParserDataVersion int    `json:"parser_data_version"`
	Invocation        string `json:"invocation"`
	SourceVersion     string `json:"source_version,omitempty"`
}

func DecodeResult(r io.Reader) (Result, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var result Result
	if err := dec.Decode(&result); err != nil {
		return Result{}, fmt.Errorf("decoding capture result: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return Result{}, errors.New("capture result contains trailing JSON")
		}
		return Result{}, fmt.Errorf("decoding capture result trailer: %w", err)
	}
	if result.Schema.Name != ResultSchemaName ||
		result.Schema.Version != ResultSchemaVersion {
		return Result{}, fmt.Errorf(
			"unsupported capture result schema %q version %d",
			result.Schema.Name, result.Schema.Version,
		)
	}
	if result.Reporting.Outcome != ReportingComplete &&
		result.Reporting.Outcome != ReportingFailed {
		return Result{}, fmt.Errorf(
			"unsupported reporting outcome %q", result.Reporting.Outcome)
	}
	if result.Provider.Name != string(ProviderClaude) &&
		result.Provider.Name != string(ProviderCodex) {
		return Result{}, fmt.Errorf("unsupported capture provider %q", result.Provider.Name)
	}
	if result.Assurance.State != AssuranceComplete &&
		result.Assurance.State != AssurancePartial &&
		result.Assurance.State != AssuranceUnavailable {
		return Result{}, fmt.Errorf("unsupported assurance state %q", result.Assurance.State)
	}
	for _, reason := range result.Assurance.Reasons {
		if !knownReason(reason) {
			return Result{}, fmt.Errorf("unsupported assurance reason %q", reason)
		}
	}
	if result.Reporting.Reason != "" && !knownReason(result.Reporting.Reason) {
		return Result{}, fmt.Errorf("unsupported reporting reason %q", result.Reporting.Reason)
	}
	if result.Cost != nil {
		if result.Cost.Currency != "USD" {
			return Result{}, fmt.Errorf("unsupported cost currency %q", result.Cost.Currency)
		}
		switch result.Cost.Source {
		case export.CostSourceComputed, export.CostSourceReported, export.CostSourceMixed:
		default:
			return Result{}, fmt.Errorf("unsupported cost source %q", result.Cost.Source)
		}
	}
	if len(result.Sources) > maxContractSources {
		return Result{}, errors.New("capture result source count exceeds limit")
	}
	if result.Reporting.Outcome == ReportingComplete && len(result.Sources) == 0 {
		return Result{}, errors.New("complete capture result omits source provenance")
	}
	seenSourceSessions := make(map[string]bool, len(result.Sources))
	for _, source := range result.Sources {
		if source.SessionID == "" {
			return Result{}, errors.New("capture result source session ID is required")
		}
		if seenSourceSessions[source.SessionID] {
			return Result{}, fmt.Errorf(
				"duplicate capture result source session %q", source.SessionID)
		}
		seenSourceSessions[source.SessionID] = true
		if err := artifact.ValidateRawSource(&artifact.RawSourceRef{
			Hash: source.SHA256, Size: source.Bytes,
			MediaType: "application/jsonl",
		}); err != nil {
			return Result{}, fmt.Errorf(
				"invalid capture result source %q: %w", source.SessionID, err)
		}
	}
	return result, nil
}

func knownReason(reason ReasonCode) bool {
	switch reason {
	case ReasonNoSession, ReasonMultipleSessions, ReasonUnfinishedSession,
		ReasonMalformedTranscript,
		ReasonFinalizationTimeout, ReasonChildStartFailed,
		ReasonCorrelationUnavailable, ReasonCorrelationConflict,
		ReasonSourceLimit, ReasonSourceBytesLimit, ReasonSourceUnavailable,
		ReasonResultSizeLimit,
		ReasonMetadataTruncated,
		ReasonIngestFailed, ReasonUsageUnavailable, ReasonCostUnavailable,
		ReasonUnpricedModel,
		ReasonCodexCacheWriteAbsent, ReasonReasoningAbsent:
		return true
	default:
		return false
	}
}

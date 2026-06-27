package sync

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/parser"
)

// ProviderObserveRequest is the source-level shadow-parse input used while the
// legacy sync path remains authoritative.
type ProviderObserveRequest struct {
	Source     parser.SourceRef
	Machine    string
	ForceParse bool
}

// ProviderObservation is the normalized, side-effect-free provider outcome for
// one source.
type ProviderObservation struct {
	Fingerprint        parser.SourceFingerprint
	Results            []parser.ParseResult
	ExcludedSessionIDs []string
	SourceErrors       []parser.SourceError
	SkipReason         parser.SkipReason
	ForceReplace       bool
	Planned            ProviderPlannedEffects
}

// ProviderShadowComparison is one caller-level shadow result. Legacy sync
// remains authoritative; this value records the side-effect-free provider
// observation and any differences from the legacy processResult.
type ProviderShadowComparison struct {
	File                parser.DiscoveredFile
	Mode                parser.ProviderMigrationMode
	Source              parser.SourceRef
	Observation         ProviderObservation
	Mismatches          []string
	NotComparableReason string
	Err                 error
}

// ProviderPlannedEffects describes writes the provider path would have made.
// Shadow mode compares these in memory; it does not receive live DB, skip-cache,
// or diagnostic writers. SSE scopes are carried for later caller work but are
// not part of the root processResult comparison.
type ProviderPlannedEffects struct {
	SourceKeys    []string
	DataVersions  []ProviderPlannedDataVersion
	SkipCacheKeys []string
	Diagnostics   []ProviderPlannedDiagnostic
	SSEScopes     []string
}

// ProviderPlannedDataVersion is an in-memory data-version write candidate.
type ProviderPlannedDataVersion struct {
	SessionID   string
	State       parser.DataVersionState
	RetryReason string
}

// ProviderPlannedDiagnostic is an in-memory parse diagnostic candidate.
type ProviderPlannedDiagnostic struct {
	SourceKey   string
	DisplayPath string
	SessionID   string
	Err         error
	Retryable   bool
}

// DataVersionSessionIDs returns the planned data-version session IDs in parse
// result order.
func (p ProviderPlannedEffects) DataVersionSessionIDs() []string {
	ids := make([]string, 0, len(p.DataVersions))
	for _, dataVersion := range p.DataVersions {
		ids = append(ids, dataVersion.SessionID)
	}
	return ids
}

// RetrySessionIDs returns sessions that need a future parse retry.
func (p ProviderPlannedEffects) RetrySessionIDs() []string {
	var ids []string
	for _, dataVersion := range p.DataVersions {
		if dataVersion.State == parser.DataVersionNeedsRetry {
			ids = append(ids, dataVersion.SessionID)
		}
	}
	return ids
}

// ObserveProviderSource fingerprints and parses a provider source without
// mutating persisted session state. It is the source-level comparison surface
// provider migration branches use before caller-level dual-run wiring exists.
func ObserveProviderSource(
	ctx context.Context,
	provider parser.Provider,
	req ProviderObserveRequest,
) (ProviderObservation, error) {
	def := provider.Definition()
	if req.Source.Provider != def.Type {
		return ProviderObservation{}, fmt.Errorf(
			"provider source mismatch: source is %s, provider is %s",
			req.Source.Provider,
			def.Type,
		)
	}

	fingerprint, err := provider.Fingerprint(ctx, req.Source)
	if err != nil {
		return ProviderObservation{}, err
	}
	outcome, err := provider.Parse(ctx, parser.ParseRequest{
		Source:      req.Source,
		Fingerprint: fingerprint,
		Machine:     req.Machine,
		ForceParse:  req.ForceParse,
	})
	if err != nil {
		return ProviderObservation{}, err
	}
	if err := validateProviderOutcome(def, req.Source, fingerprint, outcome); err != nil {
		return ProviderObservation{}, err
	}

	observation := ProviderObservation{
		Fingerprint:        fingerprint,
		Results:            parseOutcomeResults(outcome.Results),
		ExcludedSessionIDs: append([]string(nil), outcome.ExcludedSessionIDs...),
		SourceErrors:       append([]parser.SourceError(nil), outcome.SourceErrors...),
		SkipReason:         outcome.SkipReason,
		ForceReplace:       outcome.ForceReplace,
	}
	observation.Planned = planProviderEffects(req.Source, fingerprint, outcome)
	return observation, nil
}

func compareProviderObservationToProcessResult(
	observation ProviderObservation,
	legacy processResult,
	file parser.DiscoveredFile,
) []string {
	var mismatches []string
	if len(observation.Results) != len(legacy.results) {
		mismatches = append(mismatches, fmt.Sprintf(
			"result count: provider=%d legacy=%d",
			len(observation.Results), len(legacy.results),
		))
	}
	for i := 0; i < len(observation.Results) && i < len(legacy.results); i++ {
		providerResult := observation.Results[i]
		legacyResult := legacy.results[i]
		if !reflect.DeepEqual(providerResult.Session, legacyResult.Session) {
			mismatches = append(mismatches, fmt.Sprintf(
				"result[%d] session differs: provider=%+v legacy=%+v",
				i, providerResult.Session, legacyResult.Session,
			))
		}
		if !reflect.DeepEqual(providerResult.Messages, legacyResult.Messages) {
			mismatches = append(mismatches, fmt.Sprintf(
				"result[%d] messages differ",
				i,
			))
		}
		if !reflect.DeepEqual(providerResult.UsageEvents, legacyResult.UsageEvents) {
			mismatches = append(mismatches, fmt.Sprintf(
				"result[%d] usage events differ",
				i,
			))
		}
	}
	if !slices.Equal(observation.ExcludedSessionIDs, legacy.excludedSessionIDs) {
		mismatches = append(mismatches, fmt.Sprintf(
			"excluded_session_ids: provider=%v legacy=%v",
			observation.ExcludedSessionIDs, legacy.excludedSessionIDs,
		))
	}
	providerSourceErrors := comparableProviderSourceErrors(observation.SourceErrors)
	legacySourceErrors := comparableLegacySourceErrors(file.Agent, legacy.sessionErrs)
	if !reflect.DeepEqual(providerSourceErrors, legacySourceErrors) {
		mismatches = append(mismatches, fmt.Sprintf(
			"source_errors differ: provider=%v legacy=%v",
			providerSourceErrors, legacySourceErrors,
		))
	}
	providerPlanned := comparablePlannedEffects(observation.Planned)
	legacyPlanned := comparablePlannedEffects(
		legacyPlannedEffectsFromProcessResult(file, legacy),
	)
	if !slices.Equal(providerPlanned.SourceKeys, legacyPlanned.SourceKeys) {
		mismatches = append(mismatches, fmt.Sprintf(
			"planned.source_keys: provider=%v legacy=%v",
			providerPlanned.SourceKeys, legacyPlanned.SourceKeys,
		))
	}
	if !reflect.DeepEqual(providerPlanned.DataVersions, legacyPlanned.DataVersions) {
		mismatches = append(mismatches, fmt.Sprintf(
			"planned.data_versions: provider=%v legacy=%v",
			providerPlanned.DataVersions, legacyPlanned.DataVersions,
		))
	}
	if !slices.Equal(providerPlanned.SkipCacheKeys, legacyPlanned.SkipCacheKeys) {
		mismatches = append(mismatches, fmt.Sprintf(
			"planned.skip_cache_keys: provider=%v legacy=%v",
			providerPlanned.SkipCacheKeys, legacyPlanned.SkipCacheKeys,
		))
	}
	if !reflect.DeepEqual(providerPlanned.Diagnostics, legacyPlanned.Diagnostics) {
		mismatches = append(mismatches, fmt.Sprintf(
			"planned.diagnostics: provider=%v legacy=%v",
			providerPlanned.Diagnostics, legacyPlanned.Diagnostics,
		))
	}
	if observation.ForceReplace != legacy.forceReplace {
		mismatches = append(mismatches, fmt.Sprintf(
			"force_replace: provider=%t legacy=%t",
			observation.ForceReplace, legacy.forceReplace,
		))
	}
	return mismatches
}

func legacyPlannedEffectsFromProcessResult(
	file parser.DiscoveredFile,
	legacy processResult,
) ProviderPlannedEffects {
	planned := ProviderPlannedEffects{}
	for _, result := range legacy.results {
		if result.Session.File.Path != "" &&
			!slices.Contains(planned.SourceKeys, result.Session.File.Path) {
			planned.SourceKeys = append(planned.SourceKeys, result.Session.File.Path)
		}
		if result.Session.ID == "" {
			continue
		}
		state := parser.DataVersionCurrent
		if legacy.needsRetry {
			state = parser.DataVersionNeedsRetry
		}
		planned.DataVersions = append(planned.DataVersions, ProviderPlannedDataVersion{
			SessionID: result.Session.ID,
			State:     state,
		})
	}
	if legacy.cacheSkip && legacy.mtime != 0 && !legacy.noCacheSkip &&
		legacy.incremental == nil && legacy.err == nil && len(legacy.results) == 0 &&
		file.Path != "" {
		planned.SkipCacheKeys = append(planned.SkipCacheKeys, file.Path)
	}
	for _, sessionErr := range legacy.sessionErrs {
		sessionID := normalizeLegacySessionID(file.Agent, sessionErr.sessionID)
		planned.Diagnostics = append(planned.Diagnostics, ProviderPlannedDiagnostic{
			SourceKey:   sessionErr.virtualPath,
			DisplayPath: sessionErr.virtualPath,
			SessionID:   sessionID,
			Err:         sessionErr.err,
			Retryable:   true,
		})
	}
	return planned
}

type comparableSourceError struct {
	SessionID string
	SourceKey string
	Path      string
	Err       string
	Retryable bool
}

func comparableProviderSourceErrors(sourceErrors []parser.SourceError) []comparableSourceError {
	comparable := make([]comparableSourceError, 0, len(sourceErrors))
	for _, sourceErr := range sourceErrors {
		path := sourceErr.DisplayPath
		if path == "" {
			path = sourceErr.SourceKey
		}
		comparable = append(comparable, comparableSourceError{
			SessionID: sourceErr.SessionID,
			SourceKey: sourceErr.SourceKey,
			Path:      path,
			Err:       errString(sourceErr.Err),
			Retryable: sourceErr.Retryable,
		})
	}
	return comparable
}

func comparableLegacySourceErrors(
	agent parser.AgentType,
	sessionErrs []sessionParseError,
) []comparableSourceError {
	comparable := make([]comparableSourceError, 0, len(sessionErrs))
	for _, sessionErr := range sessionErrs {
		comparable = append(comparable, comparableSourceError{
			SessionID: normalizeLegacySessionID(agent, sessionErr.sessionID),
			SourceKey: sessionErr.virtualPath,
			Path:      sessionErr.virtualPath,
			Err:       errString(sessionErr.err),
			Retryable: true,
		})
	}
	return comparable
}

func normalizeLegacySessionID(agent parser.AgentType, sessionID string) string {
	if sessionID == "" {
		return ""
	}
	def, ok := parser.AgentByType(agent)
	if !ok || def.IDPrefix == "" {
		return sessionID
	}
	host, rawID := parser.StripHostPrefix(sessionID)
	if strings.HasPrefix(rawID, def.IDPrefix) {
		return sessionID
	}
	normalized := def.IDPrefix + rawID
	if host != "" {
		return host + "~" + normalized
	}
	return normalized
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

type comparablePlanned struct {
	SourceKeys    []string
	DataVersions  []comparablePlannedDataVersion
	SkipCacheKeys []string
	Diagnostics   []comparablePlannedDiagnostic
}

type comparablePlannedDataVersion struct {
	SessionID string
	State     parser.DataVersionState
}

type comparablePlannedDiagnostic struct {
	SourceKey   string
	DisplayPath string
	SessionID   string
	Err         string
	Retryable   bool
}

func comparablePlannedEffects(planned ProviderPlannedEffects) comparablePlanned {
	comparable := comparablePlanned{
		SourceKeys:    slices.Clone(planned.SourceKeys),
		SkipCacheKeys: slices.Clone(planned.SkipCacheKeys),
	}
	comparable.DataVersions = make(
		[]comparablePlannedDataVersion,
		0,
		len(planned.DataVersions),
	)
	for _, dataVersion := range planned.DataVersions {
		comparable.DataVersions = append(
			comparable.DataVersions,
			comparablePlannedDataVersion{
				SessionID: dataVersion.SessionID,
				State:     dataVersion.State,
			},
		)
	}
	comparable.Diagnostics = make(
		[]comparablePlannedDiagnostic,
		0,
		len(planned.Diagnostics),
	)
	for _, diagnostic := range planned.Diagnostics {
		comparable.Diagnostics = append(
			comparable.Diagnostics,
			comparablePlannedDiagnostic{
				SourceKey:   diagnostic.SourceKey,
				DisplayPath: diagnostic.DisplayPath,
				SessionID:   diagnostic.SessionID,
				Err:         errString(diagnostic.Err),
				Retryable:   diagnostic.Retryable,
			},
		)
	}
	return comparable
}

func validateProviderOutcome(
	def parser.AgentDef,
	source parser.SourceRef,
	fingerprint parser.SourceFingerprint,
	outcome parser.ParseOutcome,
) error {
	for _, result := range outcome.Results {
		session := result.Result.Session
		if session.Agent != def.Type {
			return fmt.Errorf(
				"%s: provider result session agent mismatch for %q: got %s",
				def.Type,
				session.ID,
				session.Agent,
			)
		}
		if err := validateProviderParseResultSessionIDs(def, result.Result); err != nil {
			return err
		}
	}
	for _, sessionID := range outcome.ExcludedSessionIDs {
		if err := validateProviderSessionID(def, sessionID, "excluded session id"); err != nil {
			return err
		}
	}
	for _, sourceErr := range outcome.SourceErrors {
		if err := validateProviderSessionID(def, sourceErr.SessionID, "diagnostic session id"); err != nil {
			return err
		}
		if sourceErr.SourceKey == "" {
			return fmt.Errorf(
				"%s: provider diagnostic source key is required for source %q",
				def.Type,
				source.Key,
			)
		}
		if !providerSourceKeyMatches(source, fingerprint, sourceErr.SourceKey) {
			return fmt.Errorf(
				"%s: provider diagnostic source key %q is unrelated to source %q",
				def.Type,
				sourceErr.SourceKey,
				source.Key,
			)
		}
	}
	return nil
}

func validateProviderParseResultSessionIDs(def parser.AgentDef, result parser.ParseResult) error {
	sessionIDs := []struct {
		field string
		id    string
	}{
		{field: "result session id", id: result.Session.ID},
		{field: "parent session id", id: result.Session.ParentSessionID},
	}
	for _, sessionID := range sessionIDs {
		if err := validateProviderSessionID(def, sessionID.id, sessionID.field); err != nil {
			return err
		}
	}
	for _, usage := range result.Session.UsageEvents {
		if err := validateProviderSessionID(def, usage.SessionID, "session usage event session id"); err != nil {
			return err
		}
	}
	for _, usage := range result.UsageEvents {
		if err := validateProviderSessionID(def, usage.SessionID, "usage event session id"); err != nil {
			return err
		}
	}
	for _, message := range result.Messages {
		for _, toolCall := range message.ToolCalls {
			if err := validateProviderSessionID(def, toolCall.SubagentSessionID, "tool call subagent session id"); err != nil {
				return err
			}
			for _, event := range toolCall.ResultEvents {
				if err := validateProviderSessionID(def, event.SubagentSessionID, "tool result event subagent session id"); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validateProviderSessionID(def parser.AgentDef, sessionID, field string) error {
	if sessionID == "" || def.IDPrefix == "" {
		return nil
	}
	if strings.HasPrefix(sessionID, def.IDPrefix) {
		return nil
	}
	return fmt.Errorf(
		"%s: provider %s %q must use prefix %q",
		def.Type,
		field,
		sessionID,
		def.IDPrefix,
	)
}

func providerSourceKeyMatches(
	source parser.SourceRef,
	fingerprint parser.SourceFingerprint,
	sourceKey string,
) bool {
	if sourceKey == "" {
		return true
	}
	for _, candidate := range []string{fingerprint.Key, source.FingerprintKey, source.Key} {
		if candidate == "" {
			continue
		}
		if sourceKey == candidate || strings.HasPrefix(sourceKey, candidate+"#") ||
			strings.HasPrefix(sourceKey, candidate+"::") ||
			strings.HasPrefix(sourceKey, candidate+"|") {
			return true
		}
	}
	return false
}

func parseOutcomeResults(outcomes []parser.ParseResultOutcome) []parser.ParseResult {
	results := make([]parser.ParseResult, 0, len(outcomes))
	for _, outcome := range outcomes {
		results = append(results, outcome.Result)
	}
	return results
}

func planProviderEffects(
	source parser.SourceRef,
	fingerprint parser.SourceFingerprint,
	outcome parser.ParseOutcome,
) ProviderPlannedEffects {
	planned := ProviderPlannedEffects{}
	if sourceKey := plannedSourceKey(source, fingerprint); sourceKey != "" {
		planned.SourceKeys = append(planned.SourceKeys, sourceKey)
	}
	if outcome.SkipReason != parser.SkipNone {
		if skipKey := plannedSkipKey(source, fingerprint); skipKey != "" {
			planned.SkipCacheKeys = append(planned.SkipCacheKeys, skipKey)
		}
	}
	for _, result := range outcome.Results {
		if result.Result.Session.ID == "" ||
			result.DataVersion == parser.DataVersionUnspecified {
			continue
		}
		planned.DataVersions = append(planned.DataVersions, ProviderPlannedDataVersion{
			SessionID:   result.Result.Session.ID,
			State:       result.DataVersion,
			RetryReason: result.RetryReason,
		})
	}
	for _, sourceErr := range outcome.SourceErrors {
		planned.Diagnostics = append(planned.Diagnostics, ProviderPlannedDiagnostic{
			SourceKey:   sourceErr.SourceKey,
			DisplayPath: sourceErr.DisplayPath,
			SessionID:   sourceErr.SessionID,
			Err:         sourceErr.Err,
			Retryable:   sourceErr.Retryable,
		})
	}
	return planned
}

func plannedSourceKey(
	source parser.SourceRef,
	fingerprint parser.SourceFingerprint,
) string {
	if fingerprint.Key != "" {
		return fingerprint.Key
	}
	if source.FingerprintKey != "" {
		return source.FingerprintKey
	}
	return source.Key
}

func plannedSkipKey(
	source parser.SourceRef,
	fingerprint parser.SourceFingerprint,
) string {
	if source.FingerprintKey != "" {
		return source.FingerprintKey
	}
	return plannedSourceKey(source, fingerprint)
}

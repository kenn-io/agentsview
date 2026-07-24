package sync

import (
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/parser"
)

// validateProviderOutcome rejects a provider parse outcome whose sessions,
// excluded IDs, or diagnostics do not belong to the source's agent. The engine
// runs this before applying a provider outcome so a misrouted or cross-provider
// result cannot corrupt the archive.
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

// parseOutcomeResults flattens a provider parse outcome's per-result wrappers
// into the bare ParseResults the engine writes.
func parseOutcomeResults(outcomes []parser.ParseResultOutcome) []parser.ParseResult {
	results := make([]parser.ParseResult, 0, len(outcomes))
	for _, outcome := range outcomes {
		results = append(results, outcome.Result)
	}
	return results
}

// plannedSourceKey is the stable identity the engine uses for a provider source
// when recording source-level state. It prefers the fingerprint key, then the
// source's own keys.
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

// plannedSkipKey is the skip-cache key the engine stores for a provider source
// that parsed to no work. It prefers the source fingerprint key so the skip
// entry keys off the same identity used elsewhere.
func plannedSkipKey(
	source parser.SourceRef,
	fingerprint parser.SourceFingerprint,
) string {
	if source.FingerprintKey != "" {
		return source.FingerprintKey
	}
	return plannedSourceKey(source, fingerprint)
}

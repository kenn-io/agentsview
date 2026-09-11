package rawderive

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/parser"
)

func TestSandboxGeminiWirePreservesTokenCoverage(t *testing.T) {
	for _, tc := range []struct {
		name, usage     string
		context, output bool
	}{
		{"missing", "", false, false}, {"output_only", `,"tokens":{"output":5}`, false, true}, {"explicit_zero", `,"tokens":{"input":0,"output":0}`, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "tmp", "project", "chats", "session-coverage.json")
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
			body := fmt.Sprintf(`{"sessionId":"coverage","startTime":"2026-09-01T12:00:00Z","lastUpdated":"2026-09-01T12:00:01Z","messages":[{"id":"m","type":"gemini","timestamp":"2026-09-01T12:00:01Z","content":"answer"%s}]}`, tc.usage)
			require.NoError(t, os.WriteFile(path, []byte(body), 0400))
			factory, ok := parser.ProviderFactoryByType(parser.AgentGemini)
			require.True(t, ok)
			provider := factory.NewProvider(parser.ProviderConfig{Roots: []string{root}, Machine: "hosted"})
			outcome, err := provider.Parse(t.Context(), parser.ParseRequest{Source: parser.SourceRef{Provider: parser.AgentGemini, DisplayPath: path}})
			require.NoError(t, err)
			require.Len(t, outcome.Results, 1)
			encoded, err := encodeParserOutcome(ParsedManifest{Outcome: outcome})
			require.NoError(t, err)
			wire, err := decodeParserOutcome(encoded)
			require.NoError(t, err)
			parsed := wire.Outcome.Results[0].Result
			candidate, err := ingest.PrepareCandidate(t.Context(), parsed, ingest.ContentOptions{})
			require.NoError(t, err)
			prepared, err := ingest.Finalize(t.Context(), candidate, ingest.ContentOptions{})
			require.NoError(t, err)
			require.Len(t, prepared.Messages, 1)
			assert.Equal(t, tc.context, prepared.Messages[0].HasContextTokens)
			assert.Equal(t, tc.output, prepared.Messages[0].HasOutputTokens)
			assert.Equal(t, tc.context, prepared.Session.HasPeakContextTokens)
			assert.Equal(t, tc.output, prepared.Session.HasTotalOutputTokens)
			assert.Equal(t, outcome, wire.Outcome, "transport preserves provider private state as well as exported fields")
		})
	}
}

func TestSandboxWirePreservesAuthoritativeAndLegacyPresence(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		known, flags, nonzero, want bool
	}{
		{"authoritative_false", true, false, true, false}, {"explicit_zero", true, true, false, true}, {"missing", true, false, false, false}, {"legacy_nonzero", false, false, true, true}, {"legacy_zero_keys", false, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message := parser.ParsedMessage{HasContextTokens: tc.flags, HasOutputTokens: tc.flags}
			session := parser.ParsedSession{HasTotalOutputTokens: tc.flags, HasPeakContextTokens: tc.flags}
			if tc.nonzero {
				message.ContextTokens = 11
				message.OutputTokens = 7
				session.TotalOutputTokens = 7
				session.PeakContextTokens = 11
			}
			if tc.name != "missing" {
				message.TokenUsage = []byte(`{"input_tokens":0,"output_tokens":0}`)
			}
			message.RestoreTokenPresenceKnown(tc.known)
			session.RestoreAggregateTokenPresenceKnown(tc.known)
			data, err := encodeParserOutcome(ParsedManifest{Outcome: parser.ParseOutcome{Results: []parser.ParseResultOutcome{{Result: parser.ParseResult{Session: session, Messages: []parser.ParsedMessage{message}}}}}})
			require.NoError(t, err)
			wire, err := decodeParserOutcome(data)
			require.NoError(t, err)
			result := wire.Outcome.Results[0].Result
			context, output := result.Messages[0].TokenPresence()
			assert.Equal(t, tc.want, context)
			assert.Equal(t, tc.want, output)
			total, peak := result.Session.AggregateTokenPresence()
			wantSession := tc.want && tc.name != "legacy_zero_keys"
			assert.Equal(t, wantSession, total)
			assert.Equal(t, wantSession, peak)
			assert.Equal(t, tc.known, result.Messages[0].TokenPresenceKnown())
			assert.Equal(t, tc.known, result.Session.AggregateTokenPresenceKnown())
		})
	}
}

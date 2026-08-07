package sync

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// TestSyncPersistsSecretFindings verifies that SyncAll scans session
// content and persists secret findings + secret_leak_count after a sync.
// The session takes the APPEND branch of writeBatch (fresh Claude session),
// validating Edit A's append path end-to-end.
func TestSyncPersistsSecretFindings(t *testing.T) {
	fx := newEngineFixture(t)

	// Build a session with two user messages; the second carries a
	// definite AWS access key secret (aws-access-key rule).
	content := testjsonl.NewSessionBuilder().
		AddClaudeUser("2024-01-01T00:00:00Z", "hello, please help me").
		AddClaudeAssistant("2024-01-01T00:00:01Z", "sure, what do you need?").
		AddRaw(testjsonl.ClaudeAssistantJSON(
			[]map[string]any{{
				"type": "tool_use",
				"id":   "toolu_aws1",
				"name": "Bash",
				"input": map[string]string{
					"command": "echo hi",
				},
			}},
			"2024-01-01T00:00:02Z",
		)).
		AddRaw(testjsonl.ClaudeToolResultUserJSON(
			"toolu_aws1",
			"AWS_ACCESS_KEY_ID=AKIA7QHWN2DKR4FYPLJM found in env",
			"2024-01-01T00:00:03Z",
		)).
		AddClaudeUser("2024-01-01T00:00:04Z", "that key AKIA7QHWN2DKR4FYPLJM is mine").
		String()

	filename := "secret-session.jsonl"
	path := filepath.Join(fx.claudeDir, "proj", filename)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	stats := fx.engine.SyncAll(context.Background(), nil)
	require.NotZero(t, stats.Synced, "expected Synced > 0, got %+v", stats)

	sessionID := fx.sessionIDFor(t, path)

	sess, err := fx.db.GetSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, sess, "session %q not found after SyncAll", sessionID)
	assert.GreaterOrEqual(t, sess.SecretLeakCount, 1, "SecretLeakCount = %d, want >= 1", sess.SecretLeakCount)

	findings, err := fx.db.SessionSecretFindings(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotEmpty(t, findings, "expected non-empty findings slice, got 0")

	var sawAWS, sawToolOutput bool
	for _, f := range findings {
		if f.RuleName != "aws-access-key" {
			continue
		}
		sawAWS = true
		if f.LocationKind == "tool_result" || f.LocationKind == "tool_result_event" {
			sawToolOutput = true
		}
	}
	assert.True(t, sawAWS, "no aws-access-key finding in %+v", findings)
	// Pin the real use case: the secret captured in tool output must be
	// detected, not only the copy in the user message.
	assert.True(t, sawToolOutput, "no aws-access-key finding in tool output: %+v", findings)
}

// TestSyncNoSecretsLeavesZero verifies a clean session persists no findings
// and a zero secret_leak_count, exercising the empty-findings path through
// the sync write (the canonical replacement deletes with nothing to insert).
func TestSyncNoSecretsLeavesZero(t *testing.T) {
	fx := newEngineFixture(t)
	content := testjsonl.NewSessionBuilder().
		AddClaudeUser("2024-01-01T00:00:00Z", "hello, please help me").
		AddClaudeAssistant("2024-01-01T00:00:01Z", "sure, happy to help").
		AddClaudeUser("2024-01-01T00:00:02Z", "thanks, that works").
		String()
	path := filepath.Join(fx.claudeDir, "proj", "clean-session.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	stats := fx.engine.SyncAll(context.Background(), nil)
	require.NotZero(t, stats.Synced, "expected Synced > 0, got %+v", stats)
	sessionID := fx.sessionIDFor(t, path)
	sess, err := fx.db.GetSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, sess, "session %q not found after SyncAll", sessionID)
	assert.Equal(t, 0, sess.SecretLeakCount)
	findings, err := fx.db.SessionSecretFindings(context.Background(), sessionID)
	require.NoError(t, err)
	assert.Empty(t, findings, "expected 0 findings, got %d: %+v", len(findings), findings)
}

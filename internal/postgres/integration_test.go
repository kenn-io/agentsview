//go:build pgtest

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// TestPGPushSecrets verifies that pg push carries secret_leak_count
// and secret_findings rows to PostgreSQL. It also verifies idempotency
// (delete-before-insert prevents duplicates on re-push).
func TestPGPushSecrets(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(
		pgURL, "agentsview", local, "machine-secrets", true,
		SyncOptions{},
	)
	require.NoError(t, err, "creating sync")
	defer ps.Close()

	ctx := context.Background()
	require.NoError(t, ps.EnsureSchema(ctx), "ensure schema")

	// Seed a session with a message.
	started := time.Now().UTC().Format(time.RFC3339)
	firstMsg := "push secrets test"
	sess := db.Session{
		ID:           "secrets-sess-001",
		Project:      "secrets-project",
		Machine:      "local",
		Agent:        "claude",
		FirstMessage: &firstMsg,
		StartedAt:    &started,
		MessageCount: 1,
	}
	require.NoError(t, local.UpsertSession(sess), "upsert session")
	require.NoError(t, local.InsertMessages([]db.Message{{
		SessionID: "secrets-sess-001",
		Ordinal:   0,
		Role:      "user",
		Content:   firstMsg,
	}}), "insert message")

	// Add a secret finding to the session.
	findings := []db.SecretFinding{
		{
			SessionID:      "secrets-sess-001",
			RuleName:       "aws-access-key",
			Confidence:     "definite",
			LocationKind:   "message",
			MessageOrdinal: 0,
			MatchStart:     5,
			MatchEnd:       25,
			MatchIndex:     0,
			RedactedMatch:  "AKIA[REDACTED]",
			RulesVersion:   "v1.0",
		},
	}
	require.NoError(t, local.ReplaceSessionSecretFindings(
		"secrets-sess-001", findings, 1, "v1.0",
	), "replace secret findings")

	// Push to PG.
	pushResult, err := ps.Push(ctx, false, nil)
	require.NoError(t, err, "push")
	require.Equal(t, 1, pushResult.SessionsPushed)

	// Open a read Store and verify via ListSessions + ListSecretFindings.
	store, err := NewStore(pgURL, "agentsview", true)
	require.NoError(t, err, "opening store")
	defer store.Close()

	// ListSessions with HasSecret=true must return the session with
	// SecretLeakCount=1.
	page, err := store.ListSessions(ctx, db.SessionFilter{
		HasSecret: true, Limit: 10,
	})
	require.NoError(t, err, "ListSessions HasSecret")
	require.Len(t, page.Sessions, 1)
	assert.Equal(t, 1, page.Sessions[0].SecretLeakCount)

	// ListSecretFindings must return the pushed finding with
	// Project and Agent populated.
	fpage, err := store.ListSecretFindings(ctx, db.SecretFindingFilter{
		Limit: 10,
	})
	require.NoError(t, err, "ListSecretFindings")
	require.Len(t, fpage.Findings, 1)
	f := fpage.Findings[0]
	assert.Equal(t, "aws-access-key", f.RuleName)
	assert.Equal(t, "secrets-project", f.Project)
	assert.Equal(t, "claude", f.Agent)
	assert.Equal(t, "AKIA[REDACTED]", f.RedactedMatch)

	// Second push with no changes must be a no-op (fingerprint
	// unchanged) and must not duplicate findings.
	r2, err := ps.Push(ctx, false, nil)
	require.NoError(t, err, "second push")
	assert.Equal(t, 0, r2.SessionsPushed, "second push should be no-op")
	fpage2, err := store.ListSecretFindings(ctx, db.SecretFindingFilter{
		Limit: 10,
	})
	require.NoError(t, err, "ListSecretFindings after re-push")
	assert.Len(t, fpage2.Findings, 1, "no duplicate findings after re-push")
}

// TestPushSecretFindingsReportsChange verifies pushSecretFindings reports
// whether it changed any rows, so the caller can bump sessions.updated_at
// for secret-only changes that pushSession and pushMessages would miss.
func TestPushSecretFindingsReportsChange(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(
		pgURL, "agentsview", local, "machine-findings-change", true,
		SyncOptions{},
	)
	require.NoError(t, err, "creating sync")
	defer ps.Close()

	ctx := context.Background()
	require.NoError(t, ps.EnsureSchema(ctx), "ensure schema")

	// Seed a session (no findings yet) and push it so the PG row exists
	// for the secret_findings foreign key.
	started := time.Now().UTC().Format(time.RFC3339)
	firstMsg := "findings change test"
	const sessID = "findings-change-001"
	sess := db.Session{
		ID:           sessID,
		Project:      "findings-project",
		Machine:      "local",
		Agent:        "claude",
		FirstMessage: &firstMsg,
		StartedAt:    &started,
		MessageCount: 1,
	}
	require.NoError(t, local.UpsertSession(sess), "upsert session")
	require.NoError(t, local.InsertMessages([]db.Message{{
		SessionID: sessID,
		Ordinal:   0,
		Role:      "user",
		Content:   firstMsg,
	}}), "insert message")
	_, err = ps.Push(ctx, false, nil)
	require.NoError(t, err, "seed push")

	// pushOnce runs pushSecretFindings in its own transaction and returns
	// whether it reported a change.
	pushOnce := func() bool {
		tx, err := ps.pg.BeginTx(ctx, nil)
		require.NoError(t, err, "begin tx")
		changed, err := ps.pushSecretFindings(ctx, tx, sessID, sessID)
		if err != nil {
			_ = tx.Rollback()
			t.Fatalf("pushSecretFindings: %v", err)
		}
		require.NoError(t, tx.Commit(), "commit tx")
		return changed
	}

	finding := db.SecretFinding{
		SessionID:      sessID,
		RuleName:       "aws-access-key",
		Confidence:     "definite",
		LocationKind:   "message",
		MessageOrdinal: 0,
		MatchStart:     5,
		MatchEnd:       25,
		MatchIndex:     0,
		RedactedMatch:  "AKIA[REDACTED]",
		RulesVersion:   "v1.0",
	}

	// No PG rows, no local findings: nothing changes.
	assert.False(t, pushOnce(), "empty -> empty reported a change")

	// Local gains a finding: the insert is a change.
	require.NoError(t, local.ReplaceSessionSecretFindings(
		sessID, []db.SecretFinding{finding}, 1, "v1.0",
	), "seed finding")
	assert.True(t, pushOnce(), "insert should report change")

	// Re-pushing the same finding still rewrites rows (delete + insert),
	// which counts as a change.
	assert.True(t, pushOnce(), "rewrite should report change")

	// Clearing local findings deletes the PG row: that is a change.
	require.NoError(t, local.ReplaceSessionSecretFindings(
		sessID, nil, 0, "v1.0",
	), "clear findings")
	assert.True(t, pushOnce(), "delete should report change")

	// Back to empty on both sides: nothing changes.
	assert.False(t, pushOnce(), "post-clear empty -> empty reported a change")
}

func TestPGConnectivity(t *testing.T) {
	pgURL := testPGURL(t)

	local := testDB(t)
	ps, err := New(
		pgURL, "agentsview", local,
		"connectivity-test-machine", true,
		SyncOptions{},
	)
	require.NoError(t, err, "creating sync")
	defer ps.Close()

	ctx, cancel := context.WithTimeout(
		context.Background(), 10*time.Second,
	)
	defer cancel()

	require.NoError(t, ps.EnsureSchema(ctx), "ensure schema")

	status, err := ps.Status(ctx)
	require.NoError(t, err, "get status")

	t.Logf("PG Sync Status: %+v", status)
}

func TestPGPushCycle(t *testing.T) {
	pgURL := testPGURL(t)

	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(
		pgURL, "agentsview", local, "machine-a", true,
		SyncOptions{},
	)
	require.NoError(t, err, "creating sync")
	defer ps.Close()

	ctx := context.Background()
	require.NoError(t, ps.EnsureSchema(ctx), "ensure schema")

	started := time.Now().UTC().Format(time.RFC3339)
	firstMsg := "hello from pg"
	sess := db.Session{
		ID:           "pg-sess-001",
		Project:      "pg-project",
		Machine:      "local",
		Agent:        "test-agent",
		FirstMessage: &firstMsg,
		StartedAt:    &started,
		MessageCount: 1,
	}
	require.NoError(t, local.UpsertSession(sess), "upsert session")
	require.NoError(t, local.InsertMessages([]db.Message{{
		SessionID: "pg-sess-001",
		Ordinal:   0,
		Role:      "user",
		Content:   firstMsg,
	}}), "insert message")

	pushResult, err := ps.Push(ctx, false, nil)
	require.NoError(t, err, "push")
	require.Equal(t, 1, pushResult.SessionsPushed)
	require.Equal(t, 1, pushResult.MessagesPushed)

	status, err := ps.Status(ctx)
	require.NoError(t, err, "status")
	assert.Equal(t, 1, status.PGSessions)
	assert.Equal(t, 1, status.PGMessages)
}

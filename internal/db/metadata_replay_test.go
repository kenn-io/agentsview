package db

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplyMetadataProjectionSessionOps(t *testing.T) {
	ctx := context.Background()
	d := testDB(t)
	insertSession(t, d, "s1", "alpha")

	applyMetadataProjectionForTest(t, d, MetadataProjection{
		EventOrigin:    "laptop-a1b2c3",
		OrderKey:       "0001-a",
		HLC:            "0001",
		ArtifactHash:   "a",
		SessionGID:     "desktop-d4e5f6~s1",
		LocalSessionID: "s1",
		Field:          "starred",
		Op:             "star",
		Value:          "star",
	})
	assert.Equal(t, 1, metadataTableCount(t, d, "starred_sessions", "session_id = 's1'"))

	applyMetadataProjectionForTest(t, d, MetadataProjection{
		EventOrigin:    "laptop-a1b2c3",
		OrderKey:       "0002-a",
		HLC:            "0002",
		ArtifactHash:   "a",
		SessionGID:     "desktop-d4e5f6~s1",
		LocalSessionID: "s1",
		Field:          "starred",
		Op:             "unstar",
		Value:          "unstar",
	})
	assert.Equal(t, 0, metadataTableCount(t, d, "starred_sessions", "session_id = 's1'"))

	applyMetadataProjectionForTest(t, d, MetadataProjection{
		EventOrigin:    "laptop-a1b2c3",
		OrderKey:       "0003-a",
		HLC:            "0003",
		ArtifactHash:   "a",
		SessionGID:     "desktop-d4e5f6~s1",
		LocalSessionID: "s1",
		Field:          "deleted_at",
		Op:             "soft_delete",
		Value:          "soft_delete",
	})
	got, err := d.GetSession(ctx, "s1")
	require.NoError(t, err)
	assert.Nil(t, got)

	applyMetadataProjectionForTest(t, d, MetadataProjection{
		EventOrigin:    "laptop-a1b2c3",
		OrderKey:       "0004-a",
		HLC:            "0004",
		ArtifactHash:   "a",
		SessionGID:     "desktop-d4e5f6~s1",
		LocalSessionID: "s1",
		Field:          "deleted_at",
		Op:             "restore",
		Value:          "restore",
	})
	got, err = d.GetSession(ctx, "s1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "s1", got.ID)

	applyMetadataProjectionForTest(t, d, MetadataProjection{
		EventOrigin:    "laptop-a1b2c3",
		OrderKey:       "0005-a",
		HLC:            "0005",
		ArtifactHash:   "a",
		SessionGID:     "desktop-d4e5f6~s1",
		LocalSessionID: "s1",
		Field:          "purge",
		Op:             "purge",
		Value:          "purge",
	})
	got, err = d.GetSessionFull(ctx, "s1")
	require.NoError(t, err)
	assert.Nil(t, got)
	assert.Equal(t, 1, metadataTableCount(t, d, "excluded_sessions", "id = 's1'"))
}

func TestApplyMetadataProjectionRequiresPinTargetBeforeMarkingApplied(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "alpha")

	result, err := d.ApplyMetadataProjection(context.Background(), MetadataProjection{
		EventOrigin:    "laptop-a1b2c3",
		OrderKey:       "0001-a",
		HLC:            "0001",
		ArtifactHash:   "a",
		SessionGID:     "desktop-d4e5f6~s1",
		LocalSessionID: "s1",
		Field:          "pin:source_uuid:missing",
		Op:             "pin",
		Value:          `{"ordinal":1,"source_uuid":"missing"}`,
		Pin: &MetadataPinProjection{
			SourceUUID: "missing",
			Ordinal:    1,
		},
	})

	require.ErrorIs(t, err, ErrMetadataTargetUnavailable)
	assert.False(t, result.Applied)
	applied, checkErr := d.MetadataEventApplied(context.Background(), "laptop-a1b2c3", "0001-a")
	require.NoError(t, checkErr)
	assert.False(t, applied)
	assert.Equal(t, 0, metadataTableCount(t, d, "metadata_replay_state", "session_gid = 'desktop-d4e5f6~s1'"))
}

func TestApplyMetadataProjectionDoesNotConflictSameOriginSequentialEdits(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "alpha")
	firstName := "one"
	secondName := "two"

	events := []MetadataProjection{
		{
			EventOrigin:    "laptop-a1b2c3",
			OrderKey:       "0001-a",
			HLC:            "0001",
			ArtifactHash:   "a",
			SessionGID:     "desktop-d4e5f6~s1",
			LocalSessionID: "s1",
			Field:          "starred",
			Op:             "star",
			Value:          "star",
		},
		{
			EventOrigin:    "laptop-a1b2c3",
			OrderKey:       "0002-a",
			HLC:            "0002",
			ArtifactHash:   "b",
			SessionGID:     "desktop-d4e5f6~s1",
			LocalSessionID: "s1",
			Field:          "starred",
			Op:             "unstar",
			Value:          "unstar",
		},
		{
			EventOrigin:    "laptop-a1b2c3",
			OrderKey:       "0003-a",
			HLC:            "0003",
			ArtifactHash:   "c",
			SessionGID:     "desktop-d4e5f6~s1",
			LocalSessionID: "s1",
			Field:          "display_name",
			Op:             "rename",
			Value:          `{"display_name":"one"}`,
			DisplayName:    &firstName,
		},
		{
			EventOrigin:    "laptop-a1b2c3",
			OrderKey:       "0004-a",
			HLC:            "0004",
			ArtifactHash:   "d",
			SessionGID:     "desktop-d4e5f6~s1",
			LocalSessionID: "s1",
			Field:          "display_name",
			Op:             "rename",
			Value:          `{"display_name":"two"}`,
			DisplayName:    &secondName,
		},
	}

	for _, ev := range events {
		result, err := d.ApplyMetadataProjection(context.Background(), ev)
		require.NoError(t, err)
		assert.True(t, result.Applied)
		assert.False(t, result.Conflict)
	}

	assert.Equal(t, 0, metadataTableCount(t, d, "metadata_conflicts", "1 = 1"))
}

func TestMetadataConflictQueriesIgnoreSameOriginRows(t *testing.T) {
	ctx := context.Background()
	d := testDB(t)
	_, err := d.getWriter().ExecContext(ctx,
		`INSERT INTO metadata_conflicts
			(session_gid, field, winning_order_key, losing_order_key,
			 winning_origin, losing_origin, winning_op, losing_op,
			 winning_value, losing_value)
		 VALUES
			('desktop-d4e5f6~s1', 'display_name', '0002-a', '0001-a',
			 'desktop-d4e5f6', 'desktop-d4e5f6', 'rename', 'rename',
			 '{"display_name":"two"}', '{"display_name":"one"}'),
			('desktop-d4e5f6~s1', 'display_name', '0003-b', '0002-a',
			 'laptop-a1b2c3', 'desktop-d4e5f6', 'rename', 'rename',
			 '{"display_name":"peer"}', '{"display_name":"two"}')`,
	)
	require.NoError(t, err)

	conflicts, err := d.ListMetadataConflicts(ctx, []string{"desktop-d4e5f6~s1"})
	require.NoError(t, err)
	require.Len(t, conflicts, 1)
	assert.Equal(t, "laptop-a1b2c3", conflicts[0].WinningOrigin)
	assert.Equal(t, "desktop-d4e5f6", conflicts[0].LosingOrigin)

	count, err := d.CountMetadataConflicts(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

func TestApplyMetadataProjectionPurgeExcludesFallbackAlias(t *testing.T) {
	d := testDB(t)
	filePath := "/tmp/vibe/session_20260616_083518_abc123/messages.jsonl"
	insertSession(t, d, "vibe:canonical-1", "alpha", func(s *Session) {
		s.Agent = "vibe"
		s.FilePath = &filePath
	})

	applyMetadataProjectionForTest(t, d, MetadataProjection{
		EventOrigin:    "laptop-a1b2c3",
		OrderKey:       "0001-a",
		HLC:            "0001",
		ArtifactHash:   "a",
		SessionGID:     "laptop-a1b2c3~vibe:canonical-1",
		LocalSessionID: "vibe:canonical-1",
		Field:          "purge",
		Op:             "purge",
		Value:          "purge",
	})

	assert.Equal(t, 1, metadataTableCount(t, d, "excluded_sessions", "id = 'vibe:canonical-1'"))
	assert.Equal(t, 1, metadataTableCount(t, d, "excluded_sessions", "id = 'vibe:session_20260616_083518_abc123'"))
}

func applyMetadataProjectionForTest(t *testing.T, d *DB, ev MetadataProjection) {
	t.Helper()
	result, err := d.ApplyMetadataProjection(context.Background(), ev)
	require.NoError(t, err)
	assert.True(t, result.Applied)
	assert.False(t, result.Duplicate)
}

func metadataTableCount(t *testing.T, d *DB, table, where string) int {
	t.Helper()
	var count int
	err := d.Reader().QueryRow("SELECT COUNT(*) FROM " + table + " WHERE " + where).Scan(&count)
	if err == sql.ErrNoRows {
		return 0
	}
	require.NoError(t, err)
	return count
}

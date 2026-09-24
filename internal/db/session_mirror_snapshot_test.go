package db

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSessionMirrorSnapshotKeepsOneSourceVersion(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	initial := messageCountWrite("mirror-snapshot", 2)
	initial.Session.Project = "before"
	initial.UsageEvents = []UsageEvent{{Source: "session", InputTokens: 15}}
	_, err := d.WriteSessionBatchAtomic(ctx, []SessionBatchWrite{initial})
	require.NoError(t, err)
	want, err := d.LoadSessionMirrorSnapshot(ctx, initial.Session.ID)
	require.NoError(t, err)
	require.NotNil(t, want)
	changed := messageCountWrite(initial.Session.ID, 1)
	changed.Session.Project = "after"
	changed.Messages[0].Content = "corrected payload"
	changed.Messages[0].ContentLength = len(changed.Messages[0].Content)
	changed.UsageEvents = []UsageEvent{{Source: "session", InputTokens: 17}}
	got, err := d.loadSessionMirrorSnapshot(ctx, initial.Session.ID, func() {
		_, writeErr := d.WriteSessionBatchAtomic(ctx, []SessionBatchWrite{changed})
		require.NoError(t, writeErr)
	})
	require.NoError(t, err)
	require.Equal(t, want, got, "metadata, payload, and fingerprints must share the source snapshot")
	latest, err := d.LoadSessionMirrorSnapshot(ctx, initial.Session.ID)
	require.NoError(t, err)
	require.Equal(t, "after", latest.Session.Project)
	require.Equal(t, 1, latest.Session.MessageCount)
	require.Len(t, latest.Messages, 1)
	require.Equal(t, "corrected payload", latest.Messages[0].Content)
	require.Equal(t, 17, latest.Usage[0].InputTokens)
	require.NotEqual(t, want.UsageFingerprint, latest.UsageFingerprint)
}

package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArchiveNativePushFingerprintIncludesRendering(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "native-push-fingerprint", "project")
	require.NoError(t, d.InsertMessages(t.Context(), []Message{nativeArchiveFixture(t, "native-push-fingerprint")}))
	before, err := d.ToolCallFingerprint(t.Context(), "native-push-fingerprint")
	require.NoError(t, err)
	batchBefore, err := d.ToolCallFingerprints(t.Context(), []string{"native-push-fingerprint"})
	require.NoError(t, err)
	_, err = d.getWriter().Exec(t.Context(), `UPDATE tool_calls SET rendering='new' WHERE session_id='native-push-fingerprint'`)
	require.NoError(t, err)
	after, err := d.ToolCallFingerprint(t.Context(), "native-push-fingerprint")
	require.NoError(t, err)
	batchAfter, err := d.ToolCallFingerprints(t.Context(), []string{"native-push-fingerprint"})
	require.NoError(t, err)
	assert.NotEqual(t, before, after)
	assert.NotEqual(t, batchBefore["native-push-fingerprint"], batchAfter["native-push-fingerprint"])
	assert.Equal(t, after, batchAfter["native-push-fingerprint"])
}

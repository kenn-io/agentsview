package db

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArchiveRejectsMalformedNativeLayoutsWithoutLosingBody(t *testing.T) {
	for _, layout := range []string{
		`{"version":2,"blocks":[]}`,
		`{"version":1,"blocks":[{"kind":"text","start":0,"end":1},{"kind":"text","start":1,"end":2}]}`,
		`{"version":1,"blocks":[{"kind":"text","start":0,"end":3}]}`,
		`{"version":1,"blocks":[{"kind":"tool_call","call_index":0}]}`,
	} {
		t.Run(layout, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "invalid-layout", "project")
			var message Message
			require.NoError(t, json.Unmarshal([]byte(`{"session_id":"invalid-layout","ordinal":0,"role":"assistant","content":"é","content_layout":`+layout+`}`), &message))
			err := d.InsertMessages(t.Context(), []Message{message})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "content layout")
			assert.Equal(t, "é", message.Content)
			stored, err := d.GetAllMessages(t.Context(), "invalid-layout")
			require.NoError(t, err)
			assert.Empty(t, stored)
		})
	}
}

func TestArchiveNativeInvalidByteAtPartBoundaryIsSanitized(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "invalid-byte", "project")
	var message Message
	require.NoError(t, json.Unmarshal([]byte(`{"session_id":"invalid-byte","ordinal":0,"role":"assistant","content_length":77,"content_layout":{"version":1,"blocks":[{"kind":"text","start":0,"end":9},{"kind":"text","start":9,"end":14}]}}`), &message))
	message.Content = "first𐐀" + string([]byte{0x80}) + "last"
	require.NoError(t, d.InsertMessages(t.Context(), []Message{message}))
	stored, err := d.GetAllMessages(t.Context(), "invalid-byte")
	require.NoError(t, err)
	require.Len(t, stored, 1)
	assert.Equal(t, "first𐐀last", stored[0].Content)
	assert.Equal(t, 76, stored[0].ContentLength)
	require.NotNil(t, stored[0].ContentLayout)
	assert.Equal(t, 9, stored[0].ContentLayout.Blocks[1].Start)
	assert.Equal(t, 13, stored[0].ContentLayout.Blocks[1].End)
}

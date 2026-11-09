package db

import (
	"crypto/sha256"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestImageTextMemoEvictsLeastRecentlyUsed(t *testing.T) {
	memo := imageTextMemo{maxEntries: 2, maxImages: 2, maxBytes: 1024}
	a := `[{"type":"input_image","image_url":"data:image/png;base64,YQ=="}]`
	b := `[{"type":"input_image","image_url":"data:image/png;base64,Yg=="}]`
	c := `[{"type":"input_image","image_url":"data:image/png;base64,Yw=="}]`
	require.Equal(t, RetainedToolResultImages(a), memo.images(a))
	require.Equal(t, RetainedToolResultImages(b), memo.images(b))
	require.Equal(t, RetainedToolResultImages(a), memo.images(a))
	require.Equal(t, RetainedToolResultImages(c), memo.images(c))
	require.Contains(t, memo.entries, sha256.Sum256([]byte(a)))
	require.NotContains(t, memo.entries, sha256.Sum256([]byte(b)))
	require.Contains(t, memo.entries, sha256.Sum256([]byte(c)))
	require.Empty(t, memo.images(`[{"type":"text","text":"image"}]`))
	require.Len(t, memo.entries, 2)
}

func TestImageTextMemoBoundsImageCountAndBytes(t *testing.T) {
	image := `{"type":"input_image","image_url":"data:image/png;base64,YQ=="}`
	memo := imageTextMemo{maxEntries: 2, maxImages: 1, maxBytes: 1024}
	require.Len(t, memo.images("["+image+","+image+"]"), 2)
	require.Empty(t, memo.entries)
	memo.maxBytes = 1
	require.Len(t, memo.images("["+image+"]"), 1)
	require.Empty(t, memo.entries)
}

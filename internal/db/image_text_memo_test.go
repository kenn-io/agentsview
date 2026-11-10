package db

import (
	"crypto/sha256"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestImageTextMemoEvictsLeastRecentlyUsed(t *testing.T) {
	memo := newImageTextMemo(2, 1024)
	a := `[{"type":"input_image","image_url":"data:image/png;base64,YQ=="}]`
	b := `[{"type":"input_image","image_url":"data:image/png;base64,Yg=="}]`
	c := `[{"type":"input_image","image_url":"data:image/png;base64,Yw=="}]`
	require.Equal(t, RetainedToolResultImages(a), memo.images(a))
	require.Equal(t, RetainedToolResultImages(b), memo.images(b))
	require.Equal(t, RetainedToolResultImages(a), memo.images(a))
	require.Equal(t, RetainedToolResultImages(c), memo.images(c))
	require.True(t, memo.items.Has(sha256.Sum256([]byte(a))))
	require.False(t, memo.items.Has(sha256.Sum256([]byte(b))))
	require.True(t, memo.items.Has(sha256.Sum256([]byte(c))))
	require.Empty(t, memo.images(`[{"type":"text","text":"image"}]`))
	require.Equal(t, 2, memo.items.Len())
	image := `{"type":"input_image","image_url":"data:image/png;base64,YQ=="}`
	memo = newImageTextMemo(2, 1)
	require.Len(t, memo.images("["+image+"]"), 1)
	require.Zero(t, memo.items.Len())
}

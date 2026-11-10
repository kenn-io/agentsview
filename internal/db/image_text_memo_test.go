package db

import (
	"crypto/sha256"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestImageTextMemoBounds(t *testing.T) {
	memo := newImageTextMemo(2, 1024)
	a := `[{"type":"input_image","image_url":"data:image/png;base64,YQ=="}]`
	b := `[{"type":"input_image","image_url":"data:image/png;base64,Yg=="}]`
	c := `[{"type":"input_image","image_url":"data:image/png;base64,Yw=="}]`
	memo.images(a)
	memo.images(b)
	memo.images(c)
	require.Empty(t, memo.images(`[{"type":"text","text":"image"}]`))
	require.Equal(t, 2, memo.items.Len())
	image := `{"type":"input_image","image_url":"data:image/png;base64,YQ=="}`
	memo = newImageTextMemo(2, 2*retainedImagesCost(RetainedToolResultImages(a))-1)
	require.Len(t, memo.images(a), 1)
	require.Len(t, memo.images("["+image+","+image+"]"), 2)
	require.True(t, memo.items.Has(sha256.Sum256([]byte(a))))
	require.Equal(t, 1, memo.items.Len())
}

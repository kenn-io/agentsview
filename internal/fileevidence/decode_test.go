package fileevidence

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These byte expectations were produced independently by the pinned native tools.
func TestFileEvidenceText(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		size int64
		hash string
	}{
		{`""`, 0, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{`"alpha\n"`, 6, "b6a98d9ce9a2d9149288fa3df42d377c3e42737afdcdaf714e33c0a100b51060"},
		{`"alpha\r\nbeta\r\n"`, 13, "98ab4d3aeab1e120560e942e2df6a0db1147bf94bafcf1590000ffb3c2b6fc80"},
		{`"\ufeffalpha\n"`, 9, "9eff3bdb19b9bef9372b96a1244c0f0f939acdb8f539551e0a099a5b20a3862e"},
		{`"alpha\r\nbeta\n"`, 12, "4f85d1b0cff7bb76f0c9a58366030c6dc1eb8e1243e3709c72452278ddce7c13"},
		{`"例 café\n"`, 10, "394f144e7692c5e1f217c63e89b498887ff13dc6efeea7c78862189dd055cc1b"},
		{`"alpha\u0000beta\n"`, 11, "c81629f4fe0c966f796498fec41567683bc52e4d23832f193ea954161c630403"},
		{`"alpha"`, 5, "8ed3f6ad685b959ead7022518e1af76cd816f8e8ec7ccdda1ed4018e8f2223f8"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			b, err := DecodeText([]byte(tc.raw))
			require.NoError(t, err)
			r := Proof(b)
			assert.Equal(t, tc.size, *r.Size)
			assert.Equal(t, tc.hash, r.SHA256)
		})
	}
	for _, raw := range []string{`"\ud800"`, `"\udc00"`, `"\ufffd"`, string([]byte{'"', 255, '"'}), `"a" "b"`} {
		_, err := DecodeText([]byte(raw))
		assert.Error(t, err, raw)
	}
}

func TestFileEvidenceBase64(t *testing.T) {
	b, err := DecodeBase64("YWxwaGEK")
	require.NoError(t, err)
	assert.Equal(t, "alpha\n", string(b))
	b, err = DecodeBase64("YQ==")
	require.NoError(t, err)
	assert.Equal(t, "a", string(b))
	for _, bad := range []string{"YQ=", "YR==", "YQ===", "YW x", "a\nb?", "YWxw\r\naGEK", "https://example.test/a"} {
		_, err := DecodeBase64(bad)
		require.Error(t, err, bad)
	}
}

func TestFileEvidenceDecodedBounds(t *testing.T) {
	body := strings.Repeat("a", MaxAttachmentBytes)
	b, err := DecodeText([]byte(`"` + body + `"`))
	require.NoError(t, err)
	assert.Len(t, b, MaxAttachmentBytes)
	_, err = DecodeText([]byte(`"` + body + `a"`))
	require.Error(t, err)
	encoded := base64.StdEncoding.EncodeToString([]byte(body))
	b, err = DecodeBase64(encoded)
	require.NoError(t, err)
	assert.Len(t, b, MaxAttachmentBytes)
	_, err = DecodeBase64(base64.StdEncoding.EncodeToString([]byte(body + "a")))
	require.Error(t, err)
}

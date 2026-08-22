package artifact

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateRawSource(t *testing.T) {
	t.Parallel()

	validHash := strings.Repeat("ab", 32)
	cases := []struct {
		name    string
		raw     *RawSourceRef
		wantErr bool
	}{
		{"nil is valid", nil, false},
		{"valid jsonl", &RawSourceRef{Hash: validHash, Size: 4096,
			MediaType: "application/jsonl", Path: "projects/p/sess.jsonl"}, false},
		{"empty media type ok", &RawSourceRef{Hash: validHash, Size: 1}, false},
		{"bad hash", &RawSourceRef{Hash: "zz", Size: 1}, true},
		{"negative size", &RawSourceRef{Hash: validHash, Size: -1}, true},
		{"oversize", &RawSourceRef{Hash: validHash, Size: 1<<30 + 1}, true},
		{"x-ndjson rejected", &RawSourceRef{Hash: validHash, Size: 1,
			MediaType: "application/x-ndjson"}, true},
		{"absolute path", &RawSourceRef{Hash: validHash, Size: 1,
			Path: "/etc/passwd"}, true},
		{"dotdot path", &RawSourceRef{Hash: validHash, Size: 1,
			Path: "a/../b"}, true},
		{"uri path", &RawSourceRef{Hash: validHash, Size: 1,
			Path: "s3://bucket/k"}, true},
		{"backslash path", &RawSourceRef{Hash: validHash, Size: 1,
			Path: `a\b`}, true},
		{"windows drive path", &RawSourceRef{Hash: validHash, Size: 1,
			Path: "C:/Users/example/session.jsonl"}, true},
		{"windows volume-relative path", &RawSourceRef{Hash: validHash, Size: 1,
			Path: "C:Users/session.jsonl"}, true},
		{"ntfs alternate data stream", &RawSourceRef{Hash: validHash, Size: 1,
			Path: "sess.jsonl:hidden"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateRawSource(tc.raw)
			if tc.wantErr {
				require.ErrorIs(t, err, ErrArtifactInvalid)
				return
			}
			require.NoError(t, err)
		})
	}
}

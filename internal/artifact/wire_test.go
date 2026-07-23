package artifact

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateRawSource(t *testing.T) {
	validHash := strings.Repeat("ab", 32)
	cases := []struct {
		name    string
		raw     *rawSourceRef
		wantErr bool
	}{
		{"nil is valid", nil, false},
		{"valid jsonl", &rawSourceRef{Hash: validHash, Size: 4096,
			MediaType: "application/jsonl", Path: "projects/p/sess.jsonl"}, false},
		{"empty media type ok", &rawSourceRef{Hash: validHash, Size: 1}, false},
		{"bad hash", &rawSourceRef{Hash: "zz", Size: 1}, true},
		{"negative size", &rawSourceRef{Hash: validHash, Size: -1}, true},
		{"oversize", &rawSourceRef{Hash: validHash, Size: 1<<30 + 1}, true},
		{"x-ndjson rejected", &rawSourceRef{Hash: validHash, Size: 1,
			MediaType: "application/x-ndjson"}, true},
		{"absolute path", &rawSourceRef{Hash: validHash, Size: 1,
			Path: "/etc/passwd"}, true},
		{"dotdot path", &rawSourceRef{Hash: validHash, Size: 1,
			Path: "a/../b"}, true},
		{"uri path", &rawSourceRef{Hash: validHash, Size: 1,
			Path: "s3://bucket/k"}, true},
		{"backslash path", &rawSourceRef{Hash: validHash, Size: 1,
			Path: `a\b`}, true},
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

package capture

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDecodeTranscriptBundleRejectsUnknownContractsAndUnsafeSources(t *testing.T) {
	tests := []struct {
		name string
		json string
	}{
		{
			name: "unknown version",
			json: `{"schema":{"name":"agentsview.one-shot-transcripts","version":2}}`,
		},
		{
			name: "unsafe relative path",
			json: `{
  "schema":{"name":"agentsview.one-shot-transcripts","version":1},
  "occurrence_id":"job-1",
  "provider":"claude",
  "sources":[{
    "session_id":"session-1",
    "raw_source":{
      "hash":"abababababababababababababababababababababababababababababababab",
      "size":1,
      "media_type":"application/jsonl",
      "path":"../session.jsonl"
    }
  }]
}`,
		},
		{
			name: "missing jsonl media type",
			json: `{
  "schema":{"name":"agentsview.one-shot-transcripts","version":1},
  "occurrence_id":"job-1",
  "provider":"claude",
  "sources":[{
    "session_id":"session-1",
    "raw_source":{
      "hash":"abababababababababababababababababababababababababababababababab",
      "size":1,
      "path":"claude/projects/project/session-1.jsonl"
    }
  }]
}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := DecodeTranscriptBundle(strings.NewReader(test.json))
			require.Error(t, err)
		})
	}
}

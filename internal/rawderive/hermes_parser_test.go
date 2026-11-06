package rawderive

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
)

func TestProviderParserHermesCronKeepsClientPlatformGrouping(t *testing.T) {
	for _, tc := range []struct{ name, key, group string }{
		{"unix", "/fixture/hermes/state.db", "job-a:02383368"},
		{"windows drive", `C:\Users\fixture\hermes\state.db`, "job-a:476c55f1"},
		{"windows share", `\\server\share\hermes\state.db`, "job-a:f29e3e98"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			contents := []byte("{\"role\":\"session_meta\",\"platform\":\"cron\"}\n{\"role\":\"user\",\"content\":\"scheduled question\"}\n")
			object := objectRefForBytes(t, contents)
			manifest := parserTestManifest(t, parser.AgentHermes, tc.key, []rawsync.Entry{{
				Path: "sessions/cron_job-a_20261007_120000.jsonl", Type: "file", Length: int64(len(contents)), Objects: []rawsync.ObjectRef{object},
			}})
			materialized, err := (Materializer{Store: &materializerStore{objects: map[rawsync.ObjectRef][]byte{object: contents}}, BaseDir: t.TempDir(), MaxTotalBytes: 1 << 20}).Materialize(t.Context(), manifest)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, materialized.Cleanup()) })
			dispatcher, err := NewProviderParser(parser.ProviderFactories(), "capture-device")
			require.NoError(t, err)
			parsed, err := dispatcher.Parse(t.Context(), manifest, materialized)
			require.NoError(t, err)
			require.Len(t, parsed.Outcome.Results, 1)
			assert.Equal(t, tc.group, parsed.Outcome.Results[0].Result.Session.GroupKey)
		})
	}
}

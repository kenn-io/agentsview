package mcp

import (
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
)

func TestFrictionTools(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	srv := newServer(ServeOptions{Service: service.NewDirectBackend(d, nil), Now: func() time.Time { return fixedNow }})
	st, ct := newInMemoryPair(t, srv)
	defer func() {
		require.NoError(t, ct.Close())
		require.NoError(t, st.Wait())
	}()

	res, err := ct.CallTool(t.Context(), callParams(ToolGetFrictionDigest, map[string]any{}))
	require.NoError(t, err)
	assert.True(t, res.IsError, "no digests yet is a tool error")

	require.NoError(t, d.SaveFrictionDigest(t.Context(), db.FrictionDigest{
		Date: "2026-09-14", Timezone: "UTC", RulesVersion: "friction-v1",
		BuiltAt: time.Date(2026, 9, 15, 1, 0, 0, 0, time.UTC), Revision: 1,
		SnapshotJSON: []byte("{}"), SummaryJSON: []byte("{\n  \"corrections\": 2,\n  \"schema_version\": 3\n}\n"),
		Markdown: []byte("# Friction Log — 2026-09-14\n"), MarkdownSHA256: "s", RunID: "r",
	}, nil, []db.FrictionPatternUpdate{
		{Fingerprint: "fl1:a", Kind: "error", Title: "[friction/error] Bash: boom", Date: "2026-09-14", SubjectID: "claude:s1", Occurrences: 3},
		{Fingerprint: "fl1:b", Kind: "correction", Title: "[friction/correction] claude:s2: no", Date: "2026-09-14", SubjectID: "claude:s2", Occurrences: 1},
	}))

	tests := []struct {
		name  string
		args  map[string]any
		isErr bool
		check func(t *testing.T, out map[string]any)
	}{
		{"markdown_latest", map[string]any{}, false, func(t *testing.T, out map[string]any) {
			t.Helper()
			assert.Equal(t, "2026-09-14", out["date"])
			assert.Equal(t, "markdown", out["format"])
			assert.Equal(t, "# Friction Log — 2026-09-14\n", out["markdown"])
			assert.Nil(t, out["summary"])
		}},
		{"summary", map[string]any{"date": "2026-09-14", "format": "summary"}, false, func(t *testing.T, out map[string]any) {
			t.Helper()
			summary, ok := out["summary"].(map[string]any)
			require.True(t, ok)
			assert.EqualValues(t, 2, summary["corrections"])
			assert.Nil(t, out["markdown"])
		}},
		{"missing_date", map[string]any{"date": "2026-01-01"}, true, nil},
		{"bad_format", map[string]any{"format": "html"}, true, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := ct.CallTool(t.Context(), callParams(ToolGetFrictionDigest, tt.args))
			require.NoError(t, err)
			require.Equal(t, tt.isErr, res.IsError)
			if tt.check == nil {
				return
			}
			raw, err := json.Marshal(res.StructuredContent)
			require.NoError(t, err)
			var out map[string]any
			require.NoError(t, json.Unmarshal(raw, &out))
			tt.check(t, out)
		})
	}

	patternTests := []struct {
		name string
		args map[string]any
		want []string
	}{
		{"ranked", map[string]any{}, []string{"fl1:a", "fl1:b"}},
		{"kind", map[string]any{"kind": "correction"}, []string{"fl1:b"}},
		{"limit", map[string]any{"limit": 1}, []string{"fl1:a"}},
		{"linked_empty", map[string]any{"linked": true}, []string{}},
	}
	for _, tt := range patternTests {
		t.Run("patterns_"+tt.name, func(t *testing.T) {
			res, err := ct.CallTool(t.Context(), callParams(ToolListFrictionPatterns, tt.args))
			require.NoError(t, err)
			require.False(t, res.IsError)
			raw, err := json.Marshal(res.StructuredContent)
			require.NoError(t, err)
			var out listFrictionPatternsOut
			require.NoError(t, json.Unmarshal(raw, &out))
			got := make([]string, 0, len(out.Patterns))
			for _, p := range out.Patterns {
				got = append(got, p.Fingerprint)
			}
			assert.Equal(t, tt.want, got)
		})
	}
}

package server_test

import (
	"encoding/json/v2"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// stringsAt collects every string value stored under key anywhere in v.
func stringsAt(v any, key string) []string {
	var out []string
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			if s, ok := child.(string); ok && k == key {
				out = append(out, s)
				continue
			}
			out = append(out, stringsAt(child, key)...)
		}
	case []any:
		for _, child := range x {
			out = append(out, stringsAt(child, key)...)
		}
	}
	return out
}

func TestFrictionRoutesPersonaFilter(t *testing.T) {
	te := setup(t)
	ctx := t.Context()
	var subjects []db.FrictionDigestSubject
	var patterns []db.FrictionPatternUpdate
	for _, s := range []struct{ id, persona string }{{"s-helper", "helper"}, {"s-reviewer", "reviewer"}} {
		require.NoError(t, te.db.UpsertSession(ctx, db.Session{ID: s.id, Project: "p", Machine: "local", Agent: "claude"}))
		ord := 1
		require.NoError(t, te.db.ReplaceSessionFriction(ctx, s.id, []db.FrictionFinding{{
			SessionID: s.id, Kind: "correction", Detector: "correction.chat",
			MessageOrdinal: &ord, Text: "no, not there", Title: "[friction/correction] " + s.id,
			Fingerprint: "fl1:" + s.id, RulesVersion: "friction-v1",
		}}, &db.FrictionSessionDims{SessionID: s.id, Persona: s.persona, DimsSource: "nanoclaw"}, "friction-v1", "hash-"+s.id))
		subjects = append(subjects, db.FrictionDigestSubject{SubjectID: s.id, Date: "2026-07-01", SubjectKind: "session"})
		patterns = append(patterns, db.FrictionPatternUpdate{
			Fingerprint: "fl1:" + s.id, Kind: "correction",
			Title: "[friction/correction] " + s.id, Date: "2026-07-01", SubjectID: s.id, Occurrences: 1,
		})
	}
	require.NoError(t, te.db.SaveFrictionDigest(ctx, db.FrictionDigest{
		Date: "2026-07-01", Timezone: "UTC", RulesVersion: "friction-v1",
		BuiltAt: time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC), Revision: 1, SessionsScanned: 2,
		SnapshotJSON: []byte("{}"), SummaryJSON: []byte("{}"), Markdown: []byte("x"),
		MarkdownSHA256: "x", RunID: "01J00000000000000000000000",
	}, subjects, patterns))

	tests := []struct {
		path, key string
		want      []string
	}{
		{"/api/v1/friction/findings?persona=helper", "session_id", []string{"s-helper"}},
		{"/api/v1/friction/patterns?persona=reviewer", "fingerprint", []string{"fl1:s-reviewer"}},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			w := te.get(t, tt.path)
			require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
			var body any
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			assert.ElementsMatch(t, tt.want, stringsAt(body, tt.key))
		})
	}
}

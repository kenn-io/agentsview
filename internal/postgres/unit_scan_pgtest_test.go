//go:build pgtest

package postgres

import (
	"errors"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"

	"go.kenn.io/agentsview/internal/db"
)

func TestScanSessionUnitsParity(t *testing.T) {
	f := newPlanUsagePushFixture(t, "agentsview_unit_parity")
	hegel.Test(t, func(ht *hegel.T) {
		session := db.Session{
			ID: "unit-session", Project: "test", Agent: "claude", Machine: "test-machine",
			IsAutomated:      hegel.Draw(ht, hegel.Booleans()),
			RelationshipType: hegel.Draw(ht, hegel.SampledFrom([]string{"", "subagent", "fork", "continuation"})),
		}
		if hegel.Draw(ht, hegel.Booleans()) {
			session.ParentSessionID = new("parent")
		}
		size := hegel.Draw(ht, hegel.Integers(0, math.MaxInt)) % 33
		messages := make([]db.Message, size)
		for i := range messages {
			content := hegel.Draw(ht, hegel.Text().ExcludeCharacters("\x00")) // PostgreSQL text cannot store NUL.
			content = hegel.Draw(ht, hegel.SampledFrom([]string{
				content,
				"\u2003<system-reminder>" + content,
				"<system-reminder>context</system-reminder>",
				"<system-reminder>context</system-reminder>\n\nhuman prompt",
				"\u2003<task-notification>" + content,
			}))
			messages[i] = db.Message{
				SessionID: session.ID, Ordinal: i, Content: content, Timestamp: "2026-01-01T00:00:00Z",
				Role:     hegel.Draw(ht, hegel.SampledFrom([]string{"user", "assistant", "tool", "system"})),
				IsSystem: hegel.Draw(ht, hegel.Booleans()), IsSidechain: hegel.Draw(ht, hegel.Booleans()),
				SourceUUID: hegel.Draw(ht, hegel.SampledFrom([]string{"", "dup", "dup#2", "a:b%"})),
			}
		}
		_, err := f.local.WriteSessionBatchContext(t.Context(), []db.SessionBatchWrite{{
			Session:  session,
			Messages: messages, ReplaceMessages: true, DataVersion: db.CurrentDataVersion(),
		}})
		require.NoError(ht, err)
		result, err := f.sync.Push(t.Context(), true, nil)
		require.NoError(ht, err)
		require.Empty(ht, result.Errors)
		include := hegel.Draw(ht, hegel.Booleans())
		runCap := hegel.Draw(ht, hegel.Integers(math.MinInt, math.MaxInt))
		var local, remote []db.EmbeddableUnit
		require.NoError(ht, f.local.ScanEmbeddableUnitsForSession(t.Context(), session.ID, include, runCap,
			func(u db.EmbeddableUnit) error { local = append(local, u); return nil }))
		require.NoError(ht, f.store.ScanSessionUnits(t.Context(), session.ID, include, runCap,
			func(u db.EmbeddableUnit) error { remote = append(remote, u); return nil }))
		assert.Equal(ht, local, remote)
	})
}

func TestScanSessionUnitsSystemPrefixes(t *testing.T) {
	f := newPlanUsagePushFixture(t, "agentsview_unit_prefixes")
	contents := []string{
		"first", "\u2003<system-reminder>context</system-reminder>", "second",
		"<task-notification>done", "<system-reminder>unclosed",
		"<system-reminder>context</system-reminder>\n\nhuman prompt", "<task-notification>assistant", "tail",
	}
	roles := []string{"assistant", "user", "assistant", "user", "user", "user", "assistant", "assistant"}
	var messages []db.Message
	for i, content := range contents {
		messages = append(messages, db.Message{
			SessionID: planUsageRootID, Ordinal: i, Role: roles[i],
			Content: content, Timestamp: "2026-01-01T00:00:00Z",
		})
	}
	require.NoError(t, f.local.InsertMessages(t.Context(), messages))
	f.push(t)
	var local, remote []db.EmbeddableUnit
	require.NoError(t, f.local.ScanEmbeddableUnitsForSession(t.Context(), planUsageRootID, true, 1000,
		func(u db.EmbeddableUnit) error { local = append(local, u); return nil }))
	require.NoError(t, f.store.ScanSessionUnits(t.Context(), planUsageRootID, true, 1000,
		func(u db.EmbeddableUnit) error { remote = append(remote, u); return nil }))
	assert.Equal(t, local, remote)
	require.Len(t, remote, 4)
	assert.Equal(t, "first\n\nsecond", remote[0].Content)
	assert.Equal(t, 2, remote[0].OrdinalEnd)
	assert.Equal(t, "<system-reminder>unclosed", remote[1].Content)
	assert.Equal(t, "<system-reminder>context</system-reminder>\n\nhuman prompt", remote[2].Content)
	assert.Equal(t, "<task-notification>assistant\n\ntail", remote[3].Content)
}

func TestScanSessionUnitsScopeAndErrors(t *testing.T) {
	f := newPlanUsagePushFixture(t, "agentsview_unit_scope")
	require.NoError(t, f.local.InsertMessages(t.Context(), []db.Message{
		{SessionID: planUsageRootID, Ordinal: 0, Role: "assistant", Content: "first", Timestamp: "2026-01-01T00:00:00Z"},
		{SessionID: planUsageRootID, Ordinal: 1, Role: "assistant", Content: "second", Timestamp: "2026-01-01T00:00:00Z"},
	}))
	f.push(t)
	collect := func(id string) []db.EmbeddableUnit {
		var units []db.EmbeddableUnit
		require.NoError(t, f.store.ScanSessionUnits(t.Context(), id, true, 100,
			func(u db.EmbeddableUnit) error { units = append(units, u); return nil }))
		return units
	}
	require.Len(t, collect(planUsageRootID), 1)
	assert.Empty(t, collect("missing"))
	require.Error(t, f.store.ScanSessionUnits(t.Context(), "", true, 100, nil))
	sentinel := errors.New("stop")
	assert.ErrorIs(t, f.store.ScanSessionUnits(t.Context(), planUsageRootID, true, 1,
		func(db.EmbeddableUnit) error { return sentinel }), sentinel)
	assert.ErrorIs(t, f.store.ScanSessionUnits(t.Context(), planUsageRootID, true, 100,
		func(db.EmbeddableUnit) error { return sentinel }), sentinel)
	_, err := f.store.DB().ExecContext(t.Context(), `UPDATE sessions SET deleted_at = now() WHERE id = $1`, planUsageRootID)
	require.NoError(t, err)
	assert.Empty(t, collect(planUsageRootID))
}

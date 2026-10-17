package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMachineLabelsPersistUpdatesAndSurviveResync(t *testing.T) {
	ctx := t.Context()
	source := testDB(t)
	require.NoError(t, source.SetSyncState(ctx, "last_push_at", "unrelated metadata"))
	require.NoError(t, source.SetSyncState(ctx, "machineXlabel:other", "not a label"))
	require.NoError(t, source.SetSyncState(ctx, "machine_label:installation-a", "Laptop"))
	require.NoError(t, source.SetSyncState(ctx, "machine_label:installation-b", "Desktop"))
	require.NoError(t, source.SetSyncState(ctx, "machine_label:installation-a", "Work laptop"))

	require.NoError(t, source.SetSyncState(ctx, "machine_alias:oldhost.example", "installation-a"))
	labels, err := source.GetMachineLabels(ctx)
	require.NoError(t, err)
	want := map[string]string{
		"installation-a": "Work laptop",
		"installation-b": "Desktop",
	}
	assert.Equal(t, want, labels)

	replacement := testDB(t)
	require.NoError(t, replacement.CopySyncStateFrom(source.Path()))
	labels, err = replacement.GetMachineLabels(ctx)
	require.NoError(t, err)
	assert.Equal(t, want, labels)
	aliases, err := replacement.GetMachineAliases(ctx)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"oldhost.example": "installation-a"}, aliases)
}

func TestResolveMachineFilter(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"
	d := testDB(t)
	ctx := t.Context()
	localArchive := testDB(t)
	require.NoError(t, localArchive.UpsertSession(ctx, Session{ID: "local-session", Machine: "local", Project: "project", Agent: "claude"}))

	for _, id := range []string{"id-a", "id-b", "legacy-host", id, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"} {
		require.NoError(t, d.UpsertSession(ctx, Session{ID: id, Project: "project", Machine: id, Agent: "claude"}))
	}
	for key, value := range map[string]string{
		MachineLabelKeyPrefix + "id-a":            "A",
		MachineLabelKeyPrefix + "id-b":            "B",
		MachineLabelKeyPrefix + "id-comma":        "A,B",
		MachineAliasKeyPrefix + "old-owner":       "id-a",
		MachineAliasKeyPrefix + "empty-alias":     "empty-target",
		MachineLabelKeyPrefix + "id-alias-list":   "old-owner,id-b",
		MachineLabelKeyPrefix + "hex-comma-owner": id + ", Workstation",
		MachineLabelKeyPrefix + "id-shadow":       "id-a",
		MachineLabelKeyPrefix + "id-alias":        "old-owner",
		MachineLabelKeyPrefix + "old-owner":       "A",
		MachineLabelKeyPrefix + "label-owner":     id,
		MachineLabelKeyPrefix + "hex-label-owner": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		MachineLabelKeyPrefix + "padded-owner":    "  Padded workstation \t",
		MachineLabelKeyPrefix + "uppercase-owner": "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB",
		artifactLocalInstallationStateKey:         "id-a",
	} {
		require.NoError(t, d.SetSyncState(ctx, key, value))
	}
	require.NoError(t, d.SetSyncState(ctx, MachineAliasKeyPrefix+"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC", "id-a"))
	for _, tt := range []struct {
		input, want string
		store       Store
	}{
		{id, id, d},
		{"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", d},
		{"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB", "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB", d},
		{"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC", "id-a", d},
		{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", d},
		// An installation ID without stored sessions stays an exact selector.
		{"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", d},
		{"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb,id-a", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb,id-a", d},
		{"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb,legacy-host", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb,legacy-host", d},
		{" Padded workstation ", "padded-owner", d},
		{"", "", d},
		{" \t ", "", d},
		{"A", "id-a", d},
		{"id-a", "id-a", d},
		{"old-owner", "id-a", d},
		{"empty-alias", "empty-target", d},
		{"local", "id-a", d},
		{"legacy-host", "legacy-host", d},
		{" id-a, id-b ,old-owner ", "id-a,id-b,id-a", d},
		{"A,B", "id-comma", d},
		{id + ", Workstation", "hex-comma-owner", d},
		{"old-owner,id-b", "id-a,id-b", d},
		{"local", "local", localArchive},
	} {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ResolveMachineFilter(ctx, tt.store, tt.input)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
	t.Run("ambiguous label", func(t *testing.T) {
		d := testDB(t)
		require.NoError(t, d.SetSyncState(ctx, MachineLabelKeyPrefix+"id-comma", "A,B"))
		require.NoError(t, d.SetSyncState(ctx, MachineLabelKeyPrefix+"id-duplicate", "A,B"))
		_, err := ResolveMachineFilter(ctx, d, "A,B")
		var filterError *MachineFilterError
		require.ErrorAs(t, err, &filterError)
		assert.Equal(t, []string{"id-comma", "id-duplicate"}, filterError.Candidates)
		assert.EqualError(t, err, `ambiguous machine "A,B"; matching IDs: id-comma, id-duplicate; known machines: A,B`)
	})
	t.Run("unknown selectors", func(t *testing.T) {
		for _, input := range []string{"missing", "a"} {
			t.Run(input, func(t *testing.T) {
				_, err := ResolveMachineFilter(ctx, d, input)
				var filterError *MachineFilterError
				require.ErrorAs(t, err, &filterError)
				assert.Equal(t, input, filterError.Value)
				assert.Equal(t, []string{
					"  Padded workstation \t",
					id,
					id + ", Workstation",
					"A",
					"A,B",
					"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
					"B",
					"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB",
					"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
					"empty-target",
					"id-a",
					"legacy-host",
					"old-owner",
					"old-owner,id-b",
				}, filterError.Known)
			})
		}
	})
	t.Run("unknown list members", func(t *testing.T) {
		for _, input := range []string{"id-a,id-gone", "old-owner,id-gone", "id-gone,missing", id + ",missing", id + ",A", ", ,"} {
			_, err := ResolveMachineFilter(ctx, d, input)
			var filterError *MachineFilterError
			require.ErrorAs(t, err, &filterError)
			assert.Empty(t, filterError.Candidates)
			assert.Contains(t, err.Error(), "unknown machine")
			assert.Contains(t, err.Error(), "comma-separated filters take machine IDs or aliases")
			assert.Contains(t, filterError.Known, "A")
		}
	})
}

func TestMatchingMachineIDsFindsQuotedAndTrashedIDs(t *testing.T) {
	d := testDB(t)
	for _, id := range []string{"id-b", "id'quoted"} {
		require.NoError(t, d.UpsertSession(t.Context(), Session{ID: id, Machine: id, Project: "project", Agent: "claude", ParentSessionID: new("missing-parent")}))
	}
	require.NoError(t, d.SoftDeleteSession(t.Context(), "id-b"))
	ids, err := d.GetMatchingMachineIDs(t.Context(), []string{"id-b", "id'quoted", "missing"})
	require.NoError(t, err)
	assert.Equal(t, []string{"id'quoted", "id-b"}, ids)
}

package archivesource

import (
	"path"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
)

func TestPickAncestorSource(t *testing.T) {
	const key = "11111111-1111-4111-8111-111111111111"
	name := "rollout-2024-01-01T00-00-00-" + key + ".jsonl"
	identity, err := rawsync.NewAuthIdentity("archive", "device")
	require.NoError(t, err)
	parent := rawsync.Entry{Path: name, Type: "file", Length: 4, Objects: []rawsync.ObjectRef{{SHA256: strings.Repeat("a", 64), Length: 4}}}
	makeRow := func(root string, entry rawsync.Entry) acceptedRow {
		manifest, err := rawsync.ValidateAndCanonicalize(identity, rawsync.Manifest{SchemaVersion: rawsync.ManifestSchemaVersion, Provider: parser.AgentCodex, ConfiguredRootID: root, SourceKey: parser.CodexSourceKey(parser.AgentCodex, key), CaptureID: root, CapturedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Kind: rawsync.ManifestSnapshot, Entries: []rawsync.Entry{entry}}, rawsync.DefaultManifestLimits())
		require.NoError(t, err)
		return acceptedRow{root: root, manifestID: manifest.ManifestID, configuredRoot: root, originalRoot: "/native", originalPath: "/native/" + name, sourceKey: manifest.Manifest.SourceKey, canonical: manifest.CanonicalJSON}
	}
	a := makeRow("a", parent)
	b := makeRow("b", parent)
	different := parent
	different.Objects = []rawsync.ObjectRef{{SHA256: strings.Repeat("b", 64), Length: 4}}
	wrongBytes := makeRow("b", different)
	corrupt := b
	corrupt.canonical = []byte("invalid")
	wrongRoot := a
	wrongRoot.configuredRoot = "other"
	wrongProvider := a
	otherManifest, err := rawsync.ValidateAndCanonicalize(identity, rawsync.Manifest{SchemaVersion: rawsync.ManifestSchemaVersion, Provider: parser.AgentClaude, ConfiguredRootID: "a", SourceKey: a.sourceKey, CaptureID: "a", CapturedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Kind: rawsync.ManifestSnapshot, Entries: []rawsync.Entry{parent}}, rawsync.DefaultManifestLimits())
	require.NoError(t, err)
	wrongProvider.canonical, wrongProvider.manifestID = otherManifest.CanonicalJSON, otherManifest.ManifestID
	boundA, boundCorrupt := a, corrupt
	boundA.boundRoot, boundA.boundKey = "a", a.sourceKey
	boundCorrupt.boundRoot, boundCorrupt.boundKey = "a", a.sourceKey
	boundWrong := wrongBytes
	boundWrong.boundRoot, boundWrong.boundKey = "b", a.sourceKey
	for _, tc := range []struct {
		name    string
		rows    []acceptedRow
		found   bool
		failure string
	}{
		{"exact", []acceptedRow{a}, true, ""},
		{"unbound ambiguous", []acceptedRow{a, b}, false, "ambiguous Codex rollout"},
		{"bound ignores corrupt sibling", []acceptedRow{boundA, boundCorrupt}, true, ""},
		{"bound different bytes", []acceptedRow{boundWrong}, false, "saved transcript does not match snapshot"},
		{"unbound different bytes", []acceptedRow{wrongBytes}, false, ""},
		{"tampered canonical", []acceptedRow{corrupt}, false, "invalid"},
		{"wrong root", []acceptedRow{wrongRoot}, false, "snapshot source mismatch"},
		{"wrong provider", []acceptedRow{wrongProvider}, false, "snapshot source mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, found, err := pickAncestorSource(identity, parent, key, tc.rows)
			if tc.failure != "" {
				require.ErrorContains(t, err, tc.failure)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.found, found)
		})
	}
}

func TestCodexAncestorEntriesTokensAndAliases(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	name := "rollout-2024-01-01T00-00-00-" + id + ".jsonl"
	ref := rawsync.ObjectRef{SHA256: strings.Repeat("a", 64), Length: 4}
	other := rawsync.ObjectRef{SHA256: strings.Repeat("b", 64), Length: 4}
	entries := []rawsync.Entry{{Path: "one/" + name, Length: 4, Objects: []rawsync.ObjectRef{ref}}, {Path: "alias/" + name, Length: 4, Objects: []rawsync.ObjectRef{ref}}, {Path: "different/" + name, Length: 4, Objects: []rawsync.ObjectRef{other}}, {Path: "rollout-2024-01-01T00-00-00-22222222-2222-4222-8222-222222222222.jsonl", Length: 4, Objects: []rawsync.ObjectRef{ref}}}
	manifest := rawsync.CanonicalManifest{ManifestID: "first", Manifest: rawsync.Manifest{Entries: entries}}
	byPath := map[string]rawsync.Entry{}
	names, err := codexAncestorEntries(manifest, id, byPath)
	require.NoError(t, err)
	require.Len(t, names, 2)
	assert.Equal(t, name, path.Base(names[0]))
	assert.Equal(t, entries[0], byPath[names[0]])
	assert.Equal(t, entries[2], byPath[names[1]])
	manifest.ManifestID = "second"
	more, err := codexAncestorEntries(manifest, id, byPath)
	require.NoError(t, err)
	assert.NotEqual(t, names[0], more[0])
	assert.Equal(t, entries[0], byPath[names[0]])
	_, err = codexAncestorEntries(manifest, id, byPath)
	require.ErrorContains(t, err, "ambiguous Codex rollout")
}

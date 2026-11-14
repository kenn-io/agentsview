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
	makeRow := func(provider parser.AgentType, root string, entry rawsync.Entry) acceptedRow {
		manifest, err := rawsync.ValidateAndCanonicalize(identity, rawsync.Manifest{SchemaVersion: rawsync.ManifestSchemaVersion, Provider: provider, ConfiguredRootID: root, SourceKey: parser.CodexSourceKey(parser.AgentCodex, key), CaptureID: root, CapturedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Kind: rawsync.ManifestSnapshot, Entries: []rawsync.Entry{entry}}, rawsync.DefaultManifestLimits())
		require.NoError(t, err)
		return acceptedRow{root: root, manifestID: manifest.ManifestID, configuredRoot: root, originalRoot: "/native", originalPath: "/native/" + name, sourceKey: manifest.Manifest.SourceKey, canonical: manifest.CanonicalJSON}
	}
	a := makeRow(parser.AgentCodex, "a", parent)
	b := makeRow(parser.AgentCodex, "b", parent)
	different := parent
	different.Objects = []rawsync.ObjectRef{{SHA256: strings.Repeat("b", 64), Length: 4}}
	wrongBytes := makeRow(parser.AgentCodex, "b", different)
	corrupt := b
	corrupt.canonical = []byte("invalid")
	wrongRoot := a
	wrongRoot.configuredRoot = "other"
	wrongProvider := makeRow(parser.AgentClaude, "a", parent)
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
		cause   error
	}{
		{"exact", []acceptedRow{a}, true, "", nil},
		{"unbound ambiguous", []acceptedRow{a, b}, false, "ambiguous Codex rollout", nil},
		{"bound ignores corrupt sibling", []acceptedRow{boundA, boundCorrupt}, true, "", nil},
		{"bound different bytes", []acceptedRow{boundWrong}, false, "saved transcript does not match snapshot", nil},
		{"unbound different bytes", []acceptedRow{wrongBytes}, false, "", nil},
		{"tampered canonical", []acceptedRow{corrupt}, false, "", rawsync.ErrInvalid},
		{"wrong root", []acceptedRow{wrongRoot}, false, "snapshot source mismatch", nil},
		{"wrong provider", []acceptedRow{wrongProvider}, false, "snapshot source mismatch", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, found, err := pickAncestorSource(identity, parent, key, tc.rows)
			if tc.cause != nil {
				require.ErrorIs(t, err, tc.cause)
				return
			}
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
	entries := []rawsync.Entry{{Path: "one/" + name, Length: 4, Objects: []rawsync.ObjectRef{ref}}, {Path: "alias/" + name, Length: 4, Objects: []rawsync.ObjectRef{ref}}, {Path: "rollout-2024-01-01T00-00-00-22222222-2222-4222-8222-222222222222.jsonl", Length: 4, Objects: []rawsync.ObjectRef{ref}}}
	manifest := rawsync.CanonicalManifest{ManifestID: "first", Manifest: rawsync.Manifest{Entries: entries}}
	byPath := map[string]rawsync.Entry{}
	names, err := codexAncestorEntries(manifest, id, byPath)
	require.NoError(t, err)
	require.Len(t, names, 1)
	assert.Equal(t, "\x00first/one/"+name, names[0])
	assert.Equal(t, name, path.Base(names[0]))
	assert.Equal(t, entries[0], byPath[names[0]])
	manifest.ManifestID = "second"
	more, err := codexAncestorEntries(manifest, id, byPath)
	require.NoError(t, err)
	assert.NotEqual(t, names[0], more[0])
	_, err = codexAncestorEntries(manifest, id, byPath)
	require.ErrorContains(t, err, "ambiguous Codex rollout")
}

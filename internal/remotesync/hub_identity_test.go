package remotesync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	syncpkg "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/vector"
	kitvec "go.kenn.io/kit/vector"
)

func TestHubSessionID(t *testing.T) {
	for _, tc := range []struct{ host, id, want string }{
		{"node", "", ""},
		{"node", "session", "node~session"},
		{"node", "node~session", "node~session"},
		{"node", "other~session", "node~other~session"},
		{"", "session", "session"},
		{"", "~session", "~session"},
		{"node", "日本語~%:#", "node~日本語~%:#"},
	} {
		assert.Equal(t, tc.want, HubSessionID(tc.host, tc.id))
		assert.Equal(t, tc.want, HubSessionID(tc.host, tc.want))
	}
}

type hubIdentityLookup func(context.Context, vector.HubLookupRequest) (vector.HubLookupReply, error)

func (f hubIdentityLookup) LookupHubVectors(ctx context.Context, r vector.HubLookupRequest) (vector.HubLookupReply, error) {
	return f(ctx, r)
}

type hubFixtureUnit struct {
	Identity vector.HubIdentity
	Key      string
	Unit     db.EmbeddableUnit
}

func hubFixtureUnits(t *testing.T, database *db.DB, mapped bool, scope bool, maxChars int) []hubFixtureUnit {
	t.Helper()
	var got []hubFixtureUnit
	occurrences := map[string]int{}
	_, err := database.ScanEmbeddableUnits(t.Context(), "", scope, vector.RunMaxRunes(maxChars), func(u db.EmbeddableUnit) error {
		n := 1
		if u.SourceUUID != "" {
			key := u.SessionID + "\x00" + u.SourceUUID
			occurrences[key]++
			n = occurrences[key]
		}
		if mapped {
			u.SessionID = HubSessionID("node", u.SessionID)
		}
		id := vector.HubIdentity{SessionID: u.SessionID, UnitKind: u.Kind, SourceUUID: u.SourceUUID, Occurrence: n, Ordinal: u.Ordinal, ContentHash: fmt.Sprintf("%x", sha256.Sum256([]byte(u.Content)))}
		got = append(got, hubFixtureUnit{Identity: id, Key: vector.DocKey(u.Kind, u.SessionID, u.SourceUUID, u.Ordinal, n), Unit: u})
		return nil
	})
	require.NoError(t, err)
	return got
}

func TestHubIdentitiesAgreeForLocalAndExtractedSources(t *testing.T) {
	root := t.TempDir()
	sessions := filepath.Join(root, "project")
	require.NoError(t, os.MkdirAll(sessions, 0o700))
	interactive := `{"type":"user","uuid":"dup:#%","timestamp":"2026-10-01T10:00:00Z","message":{"content":"first"}}
{"type":"assistant","uuid":"run","timestamp":"2026-10-01T10:00:01Z","message":{"content":"aaaa"}}
{"type":"assistant","uuid":"middle","timestamp":"2026-10-01T10:00:02Z","message":{"content":"bbbb"}}
{"type":"assistant","uuid":"tail","timestamp":"2026-10-01T10:00:03Z","message":{"content":"cccc"}}
{"type":"user","uuid":"dup:#%","timestamp":"2026-10-01T10:00:04Z","message":{"content":"again"}}
{"type":"user","timestamp":"2026-10-01T10:00:05Z","message":{"content":"ordinal"}}
{"type":"assistant","timestamp":"2026-10-01T10:00:06Z","message":{"content":"dddd"}}
`
	automated := `{"type":"agent-setting","entrypoint":"sdk-cli"}` + "\n" +
		`{"type":"user","uuid":"worker","turnOrigin":"sdk","promptSource":"sdk","timestamp":"2026-10-01T10:00:00Z","message":{"content":"You are a code reviewer. Review the settings change."}}` + "\n"
	for name, body := range map[string]string{"interactive": interactive, "automated": automated} {
		require.NoError(t, os.WriteFile(filepath.Join(sessions, name+".jsonl"), []byte(body), 0o600))
	}
	local, remote := dbtest.OpenTestDB(t), dbtest.OpenTestDB(t)
	targets := TargetSet{Dirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}}
	engine := syncpkg.NewEngine(t.Context(), local, syncpkg.EngineConfig{AgentDirs: targets.Dirs, Machine: "local", DisableFilesystemProjectDiscovery: true})
	t.Cleanup(engine.Close)
	require.Equal(t, 2, engine.SyncAll(t.Context(), nil).Synced)
	var archive bytes.Buffer
	require.NoError(t, WriteArchive(t.Context(), &archive, targets))
	extracted := t.TempDir()
	_, err := ExtractTarStream(t.Context(), &archive, extracted)
	require.NoError(t, err)
	stats, err := (Importer{Host: "node", DB: remote, Full: true}).ImportExtracted(t.Context(), targets, extracted)
	require.NoError(t, err)
	require.Zero(t, stats.Failed)
	require.Equal(t, 2, stats.SessionsSynced)
	for _, maxChars := range []int{2, 8} {
		for _, scope := range []bool{false, true} {
			t.Run(fmt.Sprintf("chars-%d/automated-%t", maxChars, scope), func(t *testing.T) {
				got := hubFixtureUnits(t, local, true, scope, maxChars)
				require.Equal(t, got, hubFixtureUnits(t, remote, false, scope, maxChars), "mapped identities, hashes, offsets and run boundaries agree")
				ix, err := vector.Open(t.Context(), filepath.Join(t.TempDir(), "vectors.db"), false, maxChars)
				require.NoError(t, err)
				defer ix.Close()
				gen := kitvec.Generation{Model: "identity-test", Dimensions: 3, Params: map[string]string{"max_input_chars": strconv.Itoa(maxChars)}}
				var requested []vector.HubIdentity
				_, err = ix.Pull(t.Context(), local, hubIdentityLookup(func(_ context.Context, r vector.HubLookupRequest) (vector.HubLookupReply, error) {
					requested = append(requested, r.Identities...)
					absent := make([]int, len(r.Identities))
					for i := range absent {
						absent[i] = i
					}
					return vector.HubLookupReply{Version: vector.HubVectorVersion, Fingerprint: r.Fingerprint, Absent: absent}, nil
				}), gen, vector.PullOptions{IncludeAutomated: scope, MapSession: func(id string) string { return HubSessionID("node", id) }})
				require.NoError(t, err)
				var want []vector.HubIdentity
				for _, doc := range got {
					want = append(want, doc.Identity)
				}
				slices.SortFunc(want, func(a, b vector.HubIdentity) int {
					return strings.Compare(vector.DocKey(a.UnitKind, a.SessionID, a.SourceUUID, a.Ordinal, a.Occurrence), vector.DocKey(b.UnitKind, b.SessionID, b.SourceUUID, b.Ordinal, b.Occurrence))
				})
				assert.Equal(t, want, requested, "actual Pull requests retain duplicate UUID occurrence and source mapping")
				keys := []string{"u:node~interactive:dup%3A%23%25", "r:node~interactive:run"}
				contents := []string{"first", "aaaa\n\nbbbb\n\ncccc"}
				if maxChars == 2 {
					keys = append(keys, "r:node~interactive:tail")
					contents = []string{"first", "aaaa\n\nbbbb", "cccc"}
				}
				keys = append(keys, "u:node~interactive:dup%3A%23%25#2", "o:node~interactive:5", "ro:node~interactive:6")
				contents = append(contents, "again", "ordinal", "dddd")
				if scope {
					keys = append([]string{"u:node~automated:worker"}, keys...)
					contents = append([]string{"You are a code reviewer. Review the settings change."}, contents...)
				}
				var actualKeys, actualContents []string
				for _, u := range got {
					actualKeys, actualContents = append(actualKeys, u.Key), append(actualContents, u.Unit.Content)
					assert.Equal(t, strings.Contains(u.Key, "~automated:"), u.Unit.Subordinate)
				}
				assert.Equal(t, keys, actualKeys)
				assert.Equal(t, contents, actualContents)
			})
		}
	}
}

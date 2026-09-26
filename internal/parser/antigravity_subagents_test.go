package parser

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeAntigravitySubagentDescriptor writes one
// brain/<parent>/.system_generated/subagents/<child>.json fixture.
func writeAntigravitySubagentDescriptor(
	t *testing.T, root, parentID, childID, body string,
) string {
	t.Helper()
	path := filepath.Join(
		root, "brain", parentID, ".system_generated", "subagents",
		childID+".json",
	)
	mustMkdir(t, filepath.Dir(path))
	mustWrite(t, path, []byte(body))
	return path
}

func antigravitySubagentDescriptorBody(childID, role string) string {
	return `{"conversationId":"` + childID + `",` +
		`"subagentDescriptor":{"typeName":"test-type","role":"` +
		role + `"},` +
		`"state":"SUBAGENT_STATE_RUNNING","spawnStepIndex":2,` +
		`"workspaceUris":["file:///tmp/proj"]}`
}

// createAntigravitySpawnEdgeDB writes a .db with a user step, a
// planner-response step invoking toolName, and a result step whose
// payload strings carry resultJSON.
func createAntigravitySpawnEdgeDB(
	t *testing.T, path, toolName string, resultStrings []string,
) {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err, "open")
	defer db.Close()
	createAntigravityStepTables(t, db)

	ts := encodePB([]pbField{
		{num: 1, wire: pbWireVarint, varint: 1779000000},
	})
	userPayload := encodePB([]pbField{
		{num: 5, wire: pbWireBytes, bytes: ts},
		{num: 17, wire: pbWireBytes,
			bytes: []byte("spawn a helper for this task please")},
	})

	toolCallNested := encodePB([]pbField{
		{num: 3, wire: pbWireBytes, bytes: []byte(toolName)},
		{num: 4, wire: pbWireBytes,
			bytes: []byte("99999999-8888-4777-a666-555555555555")},
	})
	plannerPayload := encodePB([]pbField{
		{num: 5, wire: pbWireBytes, bytes: ts},
		{num: 17, wire: pbWireBytes,
			bytes: []byte("delegating to a subagent now")},
		{num: 8, wire: pbWireBytes, bytes: toolCallNested},
	})

	resultFields := []pbField{
		{num: 5, wire: pbWireBytes, bytes: ts},
	}
	for i, s := range resultStrings {
		resultFields = append(resultFields, pbField{
			num: 17 + i, wire: pbWireBytes, bytes: []byte(s),
		})
	}
	resultPayload := encodePB(resultFields)

	mustExec(t, db,
		`INSERT INTO steps (idx, step_type, step_payload) VALUES (?, ?, ?)`,
		0, 14, userPayload)
	mustExec(t, db,
		`INSERT INTO steps (idx, step_type, step_payload) VALUES (?, ?, ?)`,
		1, 15, plannerPayload)
	mustExec(t, db,
		`INSERT INTO steps (idx, step_type, step_payload) VALUES (?, ?, ?)`,
		2, 132, resultPayload)
}

// TestAntigravitySubagentDescriptorLink covers the authoritative
// descriptor edge: discovery resolves the child's parent/role onto the
// source ref and parse emits the linked session.
func TestAntigravitySubagentDescriptorLink(t *testing.T) {
	root := t.TempDir()
	parentID := "11111111-2222-4333-8444-555555555555"
	childID := "66666666-7777-4888-8999-000000000000"
	mustMkdir(t, filepath.Join(root, "conversations"))
	createAntigravityTestDB(t,
		filepath.Join(root, "conversations", parentID+".db"))
	childDB := filepath.Join(root, "conversations", childID+".db")
	createAntigravityTestDB(t, childDB)
	writeAntigravitySubagentDescriptor(t, root, parentID, childID,
		antigravitySubagentDescriptorBody(childID, "Test Helper Role"))

	provider := newAntigravityTestProvider(t, root)
	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, discovered, 2)
	var childSource *SourceRef
	for i := range discovered {
		if discovered[i].DisplayPath == childDB {
			childSource = &discovered[i]
		}
	}
	require.NotNil(t, childSource)
	src := childSource.Opaque.(antigravitySource)
	assert.Equal(t, parentID, src.SubagentParentID)
	assert.Equal(t, "Test Helper Role", src.SubagentRole)

	sess, _, _, err := provider.parseSession(
		t.Context(), childDB, "", "test-machine",
	)
	require.NoError(t, err)
	assert.Equal(t, antigravityIDPrefix+parentID, sess.ParentSessionID)
	assert.Equal(t, RelSubagent, sess.RelationshipType)
	assert.Equal(t, "Test Helper Role", sess.SessionName)
}

// TestAntigravitySubagentDescriptorKilledNoWorkspace: the descriptor
// link is trusted regardless of state -- SUBAGENT_STATE_KILLED children
// are still real subagents -- and workspaceUris is optional.
func TestAntigravitySubagentDescriptorKilledNoWorkspace(t *testing.T) {
	root := t.TempDir()
	parentID := "10101010-2020-4303-8404-505050505050"
	childID := "60606060-7070-4808-8909-101010101010"
	mustMkdir(t, filepath.Join(root, "conversations"))
	childDB := filepath.Join(root, "conversations", childID+".db")
	createAntigravityTestDB(t, childDB)
	writeAntigravitySubagentDescriptor(t, root, parentID, childID,
		`{"conversationId":"`+childID+`",`+
			`"subagentDescriptor":{"typeName":"test-type","role":"Killed Role"},`+
			`"state":"SUBAGENT_STATE_KILLED","spawnStepIndex":2}`)

	sess, _, _, err := parseAntigravityTestSession(t,
		childDB, "", "test-machine",
	)
	require.NoError(t, err)
	assert.Equal(t, antigravityIDPrefix+parentID, sess.ParentSessionID)
	assert.Equal(t, RelSubagent, sess.RelationshipType)
	assert.Equal(t, "Killed Role", sess.SessionName)
}

func TestAntigravitySubagentDescriptorMissing(t *testing.T) {
	root := t.TempDir()
	id := "22222222-3333-4444-8555-666666666666"
	mustMkdir(t, filepath.Join(root, "conversations"))
	dbPath := filepath.Join(root, "conversations", id+".db")
	createAntigravityTestDB(t, dbPath)

	sess, _, _, err := parseAntigravityTestSession(t,
		dbPath, "", "test-machine",
	)
	require.NoError(t, err)
	assert.Empty(t, sess.ParentSessionID)
	assert.Equal(t, RelNone, sess.RelationshipType)
}

// TestAntigravitySubagentDescriptorValidation covers every skip rule:
// malformed JSON, a conversationId that does not match the filename, a
// non-UUID conversationId, and a self link.
func TestAntigravitySubagentDescriptorValidation(t *testing.T) {
	childID := "33333333-4444-4555-8666-777777777777"
	parentID := "88888888-9999-4aaa-bbbb-cccccccccccc"
	tests := []struct {
		name     string
		parent   string
		filename string
		body     string
	}{
		{
			name:     "malformed json",
			parent:   parentID,
			filename: childID + ".json",
			body:     `{"conversationId":`,
		},
		{
			name:     "conversationId mismatch",
			parent:   parentID,
			filename: childID + ".json",
			body: antigravitySubagentDescriptorBody(
				"dddddddd-eeee-4fff-aaaa-111111111111", "role"),
		},
		{
			name:     "non-uuid conversationId",
			parent:   parentID,
			filename: childID + ".json",
			body:     `{"conversationId":"not-a-uuid"}`,
		},
		{
			name:     "self link",
			parent:   childID,
			filename: childID + ".json",
			body:     antigravitySubagentDescriptorBody(childID, "role"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			mustMkdir(t, filepath.Join(root, "conversations"))
			dbPath := filepath.Join(root, "conversations", childID+".db")
			createAntigravityTestDB(t, dbPath)
			descPath := filepath.Join(
				root, "brain", tc.parent, ".system_generated",
				"subagents", tc.filename,
			)
			mustMkdir(t, filepath.Dir(descPath))
			mustWrite(t, descPath, []byte(tc.body))

			sess, _, _, err := parseAntigravityTestSession(t,
				dbPath, "", "test-machine",
			)
			require.NoError(t, err)
			assert.Empty(t, sess.ParentSessionID)
			assert.Equal(t, RelNone, sess.RelationshipType)
		})
	}
}

// TestAntigravitySubagentConflictingParents pins the keep-first rule:
// two parents claiming one child resolve to the first glob-sorted
// parent.
func TestAntigravitySubagentConflictingParents(t *testing.T) {
	root := t.TempDir()
	firstParent := "00000000-1111-4222-8333-444444444444"
	secondParent := "99999999-aaaa-4bbb-cccc-dddddddddddd"
	childID := "55555555-6666-4777-8888-999999999999"
	mustMkdir(t, filepath.Join(root, "conversations"))
	dbPath := filepath.Join(root, "conversations", childID+".db")
	createAntigravityTestDB(t, dbPath)
	for _, p := range []string{firstParent, secondParent} {
		writeAntigravitySubagentDescriptor(t, root, p, childID,
			antigravitySubagentDescriptorBody(childID, "role"))
	}

	provider := newAntigravityTestProvider(t, root)
	_, err := provider.Discover(t.Context())
	require.NoError(t, err)
	sess, _, _, err := provider.parseSession(
		t.Context(), dbPath, "", "test-machine",
	)
	require.NoError(t, err)
	assert.Equal(t, antigravityIDPrefix+firstParent, sess.ParentSessionID)
}

// TestAntigravityInvokeSubagentSpawnEdge verifies a marked spawn-result
// step echoing conversationId links the invoke_subagent tool call to the
// spawned session in both providers' .db decode paths. The two-space
// colon pins whitespace tolerance; compact and one-space variants are
// covered by the other fixtures.
func TestAntigravityInvokeSubagentSpawnEdge(t *testing.T) {
	childID := "77777777-8888-4999-aaaa-bbbbbbbbbbbb"
	result := `Created the following subagents: ` +
		`[{"conversationId":  "` + childID + `","status":"spawned"}]`

	t.Run("ide", func(t *testing.T) {
		root := t.TempDir()
		id := "12345678-9abc-4def-8123-456789abcdef"
		mustMkdir(t, filepath.Join(root, "conversations"))
		dbPath := filepath.Join(root, "conversations", id+".db")
		createAntigravitySpawnEdgeDB(t, dbPath, "invoke_subagent",
			[]string{result})

		_, msgs, _, err := parseAntigravityTestSession(t,
			dbPath, "", "test-machine",
		)
		require.NoError(t, err)
		call := findAntigravityTestToolCall(t, msgs, "invoke_subagent")
		assert.Equal(t, antigravityIDPrefix+childID,
			call.SubagentSessionID)
	})

	t.Run("cli", func(t *testing.T) {
		root := t.TempDir()
		id := "12345678-9abc-4def-8123-456789abcdef"
		mustMkdir(t, filepath.Join(root, "conversations"))
		dbPath := filepath.Join(root, "conversations", id+".db")
		createAntigravitySpawnEdgeDB(t, dbPath, "invoke_subagent",
			[]string{result})

		_, msgs, err := parseAntigravityCLITestSession(t,
			dbPath, "", "test-machine",
		)
		require.NoError(t, err)
		call := findAntigravityTestToolCall(t, msgs, "invoke_subagent")
		assert.Equal(t, antigravityCLIIDPrefix+childID,
			call.SubagentSessionID)
	})
}

// findAntigravityTestToolCall returns the first tool call with the
// given name across the parsed messages.
func findAntigravityTestToolCall(
	t *testing.T, msgs []ParsedMessage, name string,
) ParsedToolCall {
	t.Helper()
	for _, m := range msgs {
		for _, c := range m.ToolCalls {
			if c.ToolName == name {
				return c
			}
		}
	}
	require.FailNow(t, fmt.Sprintf("tool call %q not found", name))
	return ParsedToolCall{}
}

// TestAntigravityInvokeSubagentMultiUUID takes only the first
// conversationId out of a multi-child marked result step.
func TestAntigravityInvokeSubagentMultiUUID(t *testing.T) {
	root := t.TempDir()
	id := "23456789-abcd-4ef0-8234-56789abcdef0"
	first := "aaaaaaaa-1111-4222-8333-444444444444"
	second := "bbbbbbbb-2222-4333-8444-555555555555"
	mustMkdir(t, filepath.Join(root, "conversations"))
	dbPath := filepath.Join(root, "conversations", id+".db")
	createAntigravitySpawnEdgeDB(t, dbPath, "invoke_subagent", []string{
		`Created the following subagents: ` +
			`[{"conversationId":"` + first + `"},` +
			`{"conversationId":"` + second + `"}]`,
	})

	_, msgs, _, err := parseAntigravityTestSession(t,
		dbPath, "", "test-machine",
	)
	require.NoError(t, err)
	call := findAntigravityTestToolCall(t, msgs, "invoke_subagent")
	assert.Equal(t, antigravityIDPrefix+first, call.SubagentSessionID)
}

// TestAntigravityInvokeSubagentMarkerGate: a result step echoing
// conversationId strings without the "Created the following subagents"
// marker -- exactly what manage_subagents list output looks like --
// must not create a spawn edge.
func TestAntigravityInvokeSubagentMarkerGate(t *testing.T) {
	childID := "eeeeeeee-7777-4888-9999-000000000000"
	manageListResult := `You have 1 active subagent(s): ` +
		`[{"conversationId":"` + childID + `","state":"running"}]`

	root := t.TempDir()
	id := "4567890a-cdef-4123-8567-890abcdef123"
	mustMkdir(t, filepath.Join(root, "conversations"))
	dbPath := filepath.Join(root, "conversations", id+".db")
	createAntigravitySpawnEdgeDB(t, dbPath, "invoke_subagent",
		[]string{manageListResult})

	_, msgs, _, err := parseAntigravityTestSession(t,
		dbPath, "", "test-machine",
	)
	require.NoError(t, err)
	call := findAntigravityTestToolCall(t, msgs, "invoke_subagent")
	assert.Empty(t, call.SubagentSessionID,
		"unmarked conversationId echoes are not spawn edges")
}

// TestAntigravityInvokeSubagentFailedNoEdge: an invoke whose result
// never arrives (no marked step follows) leaves SubagentSessionID
// empty.
func TestAntigravityInvokeSubagentFailedNoEdge(t *testing.T) {
	root := t.TempDir()
	id := "567890ab-def1-4234-8678-90abcdef1234"
	mustMkdir(t, filepath.Join(root, "conversations"))
	dbPath := filepath.Join(root, "conversations", id+".db")
	createAntigravitySpawnEdgeDB(t, dbPath, "invoke_subagent",
		[]string{`the invoke failed; no subagent was created`})

	_, msgs, _, err := parseAntigravityTestSession(t,
		dbPath, "", "test-machine",
	)
	require.NoError(t, err)
	call := findAntigravityTestToolCall(t, msgs, "invoke_subagent")
	assert.Empty(t, call.SubagentSessionID)
}

// TestAntigravityNonInvokeToolsNoSpawnEdge: even a marked spawn-result
// step does not create an edge for define_subagent or manage_subagents
// calls -- only invoke_subagent carries spawn links.
func TestAntigravityNonInvokeToolsNoSpawnEdge(t *testing.T) {
	childID := "cccccccc-3333-4444-8555-666666666666"
	for _, tool := range []string{"define_subagent", "manage_subagents"} {
		t.Run(tool, func(t *testing.T) {
			root := t.TempDir()
			id := "3456789a-bcde-4f01-8345-6789abcdef01"
			mustMkdir(t, filepath.Join(root, "conversations"))
			dbPath := filepath.Join(root, "conversations", id+".db")
			createAntigravitySpawnEdgeDB(t, dbPath, tool, []string{
				`Created the following subagents: ` +
					`[{"conversationId":"` + childID + `"}]`,
			})

			_, msgs, _, err := parseAntigravityTestSession(t,
				dbPath, "", "test-machine",
			)
			require.NoError(t, err)
			call := findAntigravityTestToolCall(t, msgs, tool)
			assert.Empty(t, call.SubagentSessionID)
		})
	}
}

// TestAntigravityIDESidecarParentCascadeLink covers the IDE fallback:
// without a descriptor, agyReader.parentCascadeId links the session even
// when the sidecar lags the raw step count (mirroring the CLI rule that
// lineage metadata is independent of transcript coverage).
func TestAntigravityIDESidecarParentCascadeLink(t *testing.T) {
	root := t.TempDir()
	id := "456789ab-cdef-4012-8456-789abcdef012"
	parentID := "dddddddd-4444-4555-8666-777777777777"
	mustMkdir(t, filepath.Join(root, "conversations"))
	dbPath := filepath.Join(root, "conversations", id+".db")
	createAntigravityTestDB(t, dbPath) // 2 raw steps
	// One displayable step deliberately lags the two-step DB.
	sidecar := `{"trajectoryId":"traj","cascadeId":"` + id + `",` +
		`"agyReader":{"parentCascadeId":"` + parentID + `"},` +
		`"steps":[{"type":"CORTEX_STEP_TYPE_USER_INPUT",` +
		`"metadata":{"createdAt":"2026-07-15T00:00:00Z"},` +
		`"userInput":{"userResponse":"child prompt"}}]}`
	mustWrite(t, strings.TrimSuffix(dbPath, ".db")+".trajectory.json",
		[]byte(sidecar))

	sess, _, _, err := parseAntigravityTestSession(t,
		dbPath, "", "test-machine",
	)
	require.NoError(t, err)
	assert.Equal(t, antigravityIDPrefix+parentID, sess.ParentSessionID)
	assert.Equal(t, RelSubagent, sess.RelationshipType)
	assert.Empty(t, sess.SessionName)
}

// TestAntigravityDescriptorBeatsSidecar pins the precedence rule:
// descriptor wins over agyReader.parentCascadeId.
func TestAntigravityDescriptorBeatsSidecar(t *testing.T) {
	root := t.TempDir()
	id := "56789abc-def0-4123-8567-89abcdef0123"
	descParent := "eeeeeeee-5555-4666-8777-888888888888"
	sidecarParent := "ffffffff-6666-4777-8888-999999999999"
	mustMkdir(t, filepath.Join(root, "conversations"))
	dbPath := filepath.Join(root, "conversations", id+".db")
	createAntigravityTestDB(t, dbPath)
	writeAntigravitySubagentDescriptor(t, root, descParent, id,
		antigravitySubagentDescriptorBody(id, "Descriptor Role"))
	sidecar := `{"trajectoryId":"traj","cascadeId":"` + id + `",` +
		`"agyReader":{"parentCascadeId":"` + sidecarParent + `"},` +
		`"steps":[{"type":"CORTEX_STEP_TYPE_USER_INPUT",` +
		`"metadata":{"createdAt":"2026-07-15T00:00:00Z"},` +
		`"userInput":{"userResponse":"child prompt"}}]}`
	mustWrite(t, strings.TrimSuffix(dbPath, ".db")+".trajectory.json",
		[]byte(sidecar))

	sess, _, _, err := parseAntigravityTestSession(t,
		dbPath, "", "test-machine",
	)
	require.NoError(t, err)
	assert.Equal(t, antigravityIDPrefix+descParent, sess.ParentSessionID)
	assert.Equal(t, "Descriptor Role", sess.SessionName)
}

// TestAntigravityWatchPlanIncludesSubagentGlob asserts the recursive
// brain watch root carries the descriptor include glob.
func TestAntigravityWatchPlanIncludesSubagentGlob(t *testing.T) {
	root := t.TempDir()
	provider := newAntigravityTestProvider(t, root)
	plan, err := provider.WatchPlan(t.Context())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 3)
	assert.Equal(t, filepath.Join(root, "brain"), plan.Roots[1].Path)
	assert.True(t, plan.Roots[1].Recursive)
	assert.Contains(t, plan.Roots[1].IncludeGlobs,
		"*/.system_generated/subagents/*.json")
}

// TestAntigravityRoutesSubagentDescriptor covers live descriptor
// updates: a descriptor write event routes to the child's .db source
// and refreshes the link cache so a subsequent parse picks the parent
// up; a remove event drops it again.
func TestAntigravityRoutesSubagentDescriptor(t *testing.T) {
	root := t.TempDir()
	parentID := "abcdefab-1234-4567-8899-aabbccddeeff"
	childID := "fedcba98-7654-4321-8fff-eeddccbbaa00"
	mustMkdir(t, filepath.Join(root, "conversations"))
	childDB := filepath.Join(root, "conversations", childID+".db")
	createAntigravityTestDB(t, childDB)

	provider := newAntigravityTestProvider(t, root)
	_, err := provider.Discover(t.Context())
	require.NoError(t, err)

	sess, _, _, err := provider.parseSession(
		t.Context(), childDB, "", "test-machine",
	)
	require.NoError(t, err)
	assert.Empty(t, sess.ParentSessionID)

	descPath := writeAntigravitySubagentDescriptor(t,
		root, parentID, childID,
		antigravitySubagentDescriptorBody(childID, "role"))
	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: descPath, EventKind: "write"},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, childDB, changed[0].DisplayPath,
		"a descriptor write must route to the child's .db source")

	sess, _, _, err = provider.parseSession(
		t.Context(), childDB, "", "test-machine",
	)
	require.NoError(t, err)
	assert.Equal(t, antigravityIDPrefix+parentID, sess.ParentSessionID)
	assert.Equal(t, RelSubagent, sess.RelationshipType)

	require.NoError(t, os.Remove(descPath))
	changed, err = provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: descPath, EventKind: "remove"},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	sess, _, _, err = provider.parseSession(
		t.Context(), childDB, "", "test-machine",
	)
	require.NoError(t, err)
	assert.Empty(t, sess.ParentSessionID,
		"descriptor removal must drop the parent link")
}

// TestAntigravityFingerprintTracksSubagentDescriptor verifies the
// composite fingerprint changes when the descriptor appears or
// disappears, so descriptor-only changes re-sync the child.
func TestAntigravityFingerprintTracksSubagentDescriptor(t *testing.T) {
	root := t.TempDir()
	parentID := "01230123-4567-4890-8abc-def012345678"
	childID := "32103210-6543-4098-8cba-fed098765432"
	mustMkdir(t, filepath.Join(root, "conversations"))
	createAntigravityTestDB(t,
		filepath.Join(root, "conversations", childID+".db"))

	provider := newAntigravityTestProvider(t, root)
	source, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: childID,
	})
	require.NoError(t, err)
	require.True(t, ok)

	before, err := provider.Fingerprint(t.Context(), source)
	require.NoError(t, err)

	descPath := writeAntigravitySubagentDescriptor(t,
		root, parentID, childID,
		antigravitySubagentDescriptorBody(childID, "role"))
	afterCreate, err := provider.Fingerprint(t.Context(), source)
	require.NoError(t, err)
	assert.NotEqual(t, before.Hash, afterCreate.Hash,
		"descriptor creation must change the child fingerprint")

	require.NoError(t, os.Remove(descPath))
	afterRemove, err := provider.Fingerprint(t.Context(), source)
	require.NoError(t, err)
	assert.NotEqual(t, afterCreate.Hash, afterRemove.Hash,
		"descriptor removal must change the child fingerprint")
}

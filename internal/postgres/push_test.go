package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

type syncStateReaderStub struct {
	value string
	err   error
}

func (s syncStateReaderStub) GetSyncState(
	key string,
) (string, error) {
	return s.value, s.err
}

func (s syncStateReaderStub) SetSyncState(
	string, string,
) error {
	return nil
}

func (s syncStateReaderStub) GetOrCreateSyncState(
	key, defaultValue string,
) (string, error) {
	if s.value != "" || s.err != nil {
		return s.value, s.err
	}
	return defaultValue, nil
}

type syncStateStoreStub struct {
	values      map[string]string
	createValue string
}

func (s *syncStateStoreStub) GetSyncState(
	key string,
) (string, error) {
	return s.values[key], nil
}

func (s *syncStateStoreStub) SetSyncState(
	key, value string,
) error {
	if s.values == nil {
		s.values = make(map[string]string)
	}
	s.values[key] = value
	return nil
}

func (s *syncStateStoreStub) GetOrCreateSyncState(
	key, defaultValue string,
) (string, error) {
	if s.values == nil {
		s.values = make(map[string]string)
	}
	if value := s.values[key]; value != "" {
		return value, nil
	}
	if s.createValue != "" {
		s.values[key] = s.createValue
		return s.createValue, nil
	}
	s.values[key] = defaultValue
	return defaultValue, nil
}

type pushAliasRoutingDriver struct{}

type pushAliasRoutingConn struct{}

type pushAliasRoutingTx struct{}

type pushAliasRoutingRows struct {
	columns []string
	values  [][]driver.Value
	next    int
}

var pushAliasRoutingRegisterOnce sync.Once

func newPushAliasRoutingDB(t *testing.T) *sql.DB {
	t.Helper()
	pushAliasRoutingRegisterOnce.Do(func() {
		sql.Register("agentsview_push_alias_routing", pushAliasRoutingDriver{})
	})

	pg, err := sql.Open("agentsview_push_alias_routing", t.Name())
	require.NoError(t, err, "open push-alias-routing db")
	t.Cleanup(func() { _ = pg.Close() })
	return pg
}

func (pushAliasRoutingDriver) Open(string) (driver.Conn, error) {
	return &pushAliasRoutingConn{}, nil
}

func (c *pushAliasRoutingConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not implemented")
}

func (c *pushAliasRoutingConn) Close() error { return nil }

func (c *pushAliasRoutingConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *pushAliasRoutingConn) BeginTx(
	context.Context, driver.TxOptions,
) (driver.Tx, error) {
	return pushAliasRoutingTx{}, nil
}

func (c *pushAliasRoutingConn) CheckNamedValue(
	*driver.NamedValue,
) error {
	return nil
}

func (c *pushAliasRoutingConn) ExecContext(
	_ context.Context, query string, _ []driver.NamedValue,
) (driver.Result, error) {
	normalized := strings.ToLower(query)
	switch {
	case strings.Contains(normalized, "insert into model_pricing"):
		return driver.RowsAffected(1), nil
	case strings.Contains(normalized, "insert into sync_metadata"):
		return driver.RowsAffected(1), nil
	default:
		return driver.RowsAffected(1), nil
	}
}

func (c *pushAliasRoutingConn) QueryContext(
	_ context.Context, query string, _ []driver.NamedValue,
) (driver.Rows, error) {
	normalized := strings.ToLower(query)
	switch {
	case strings.Contains(normalized, "select version()"):
		return &pushAliasRoutingRows{
			columns: []string{"version"},
			values:  [][]driver.Value{{"PostgreSQL 16.4 (push routing fake)"}},
		}, nil
	case strings.Contains(normalized, "select coalesce(max(data_version), 0) from sessions"):
		return &pushAliasRoutingRows{
			columns: []string{"coalesce"},
			values:  [][]driver.Value{{0}},
		}, nil
	case strings.Contains(normalized, "from pg_trigger"):
		return &pushAliasRoutingRows{
			columns: []string{"count"},
			values:  [][]driver.Value{{4}},
		}, nil
	case strings.Contains(normalized, "from information_schema.tables"):
		return &pushAliasRoutingRows{
			columns: []string{"exists"},
			values:  [][]driver.Value{{1}},
		}, nil
	case strings.Contains(normalized, "from pg_indexes"):
		return &pushAliasRoutingRows{
			columns: []string{"exists"},
			values:  [][]driver.Value{{1}},
		}, nil
	case strings.Contains(normalized, "select exists ("):
		return &pushAliasRoutingRows{
			columns: []string{"exists"},
			values:  [][]driver.Value{{false}},
		}, nil
	case strings.Contains(normalized, "select value from sync_metadata where key = $1"):
		return &pushAliasRoutingRows{
			columns: []string{"value"},
		}, nil
	case strings.Contains(normalized, "limit 0"):
		return &pushAliasRoutingRows{
			columns: []string{"probe"},
		}, nil
	case strings.Contains(normalized, "select model_pattern, input_per_mtok"):
		return &pushAliasRoutingRows{
			columns: []string{
				"model_pattern",
				"input_per_mtok",
				"output_per_mtok",
				"cache_creation_per_mtok",
				"cache_read_per_mtok",
				"updated_at",
			},
		}, nil
	case strings.Contains(normalized, "select id from excluded_sessions"):
		return &pushAliasRoutingRows{
			columns: []string{"id"},
		}, nil
	default:
		return &pushAliasRoutingRows{
			columns: []string{"probe"},
		}, nil
	}
}

func (pushAliasRoutingTx) Commit() error { return nil }

func (pushAliasRoutingTx) Rollback() error { return nil }

func (r *pushAliasRoutingRows) Columns() []string { return r.columns }

func (r *pushAliasRoutingRows) Close() error { return nil }

func (r *pushAliasRoutingRows) Next(dest []driver.Value) error {
	if r.next >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.next])
	r.next++
	return nil
}

func TestPushMarkerIDReturnsInsertWinner(t *testing.T) {
	local, err := db.Open(filepath.Join(t.TempDir(), "local.db"))
	require.NoError(t, err, "db.Open")
	defer local.Close()
	require.NoError(t, local.SetSyncState(pushMarkerIDStateKey, "winner-marker"))
	sync := &Sync{local: local}

	got, err := sync.pushMarkerID()
	require.NoError(t, err, "pushMarkerID")
	assert.Equal(t, "winner-marker", got)
	stored, err := local.GetSyncState(pushMarkerIDStateKey)
	require.NoError(t, err, "GetSyncState")
	assert.Equal(t, "winner-marker", stored)
}

func TestPushMarkerIDUsesUnscopedStateAcrossNamedTargets(t *testing.T) {
	local, err := db.Open(filepath.Join(t.TempDir(), "local.db"))
	require.NoError(t, err, "db.Open")
	defer local.Close()

	workSync := &Sync{
		local:     local,
		syncState: newScopedSyncStateStore(local, "work", true),
	}
	archiveSync := &Sync{
		local:     local,
		syncState: newScopedSyncStateStore(local, "archive", false),
	}

	workMarker, err := workSync.pushMarkerID()
	require.NoError(t, err, "work pushMarkerID")
	archiveMarker, err := archiveSync.pushMarkerID()
	require.NoError(t, err, "archive pushMarkerID")

	assert.Equal(t, workMarker, archiveMarker)

	stored, err := local.GetSyncState(pushMarkerIDStateKey)
	require.NoError(t, err, "GetSyncState")
	assert.Equal(t, workMarker, stored)

	for _, key := range []string{
		pushMarkerIDStateKey + ":work",
		pushMarkerIDStateKey + ":archive",
	} {
		value, err := local.GetSyncState(key)
		require.NoError(t, err, "GetSyncState %s", key)
		assert.Empty(t, value)
	}
}

func TestSessionAliasBackfillForcesOneFullPush(t *testing.T) {
	store := &syncStateStoreStub{}

	full, needed, err := applySessionAliasBackfillRequirement(
		store, false,
	)

	require.NoError(t, err)
	assert.True(t, full, "missing alias backfill marker should force full push")
	assert.True(t, needed)

	require.NoError(t, markSessionAliasBackfillDone(store))

	full, needed, err = applySessionAliasBackfillRequirement(
		store, false,
	)

	require.NoError(t, err)
	assert.False(t, full,
		"completed alias backfill should preserve incremental push")
	assert.False(t, needed)

	full, needed, err = applySessionAliasBackfillRequirement(
		store, true,
	)

	require.NoError(t, err)
	assert.True(t, full, "explicit full push should stay full")
	assert.False(t, needed)
}

func TestTranscriptRevisionBackfillForcesOneFullPush(t *testing.T) {
	store := &syncStateStoreStub{}

	full, needed, err := applyTranscriptRevisionBackfillRequirement(
		store, false,
	)
	require.NoError(t, err)
	assert.True(t, full)
	assert.True(t, needed)

	require.NoError(t, markTranscriptRevisionBackfillDone(store))
	full, needed, err = applyTranscriptRevisionBackfillRequirement(
		store, false,
	)
	require.NoError(t, err)
	assert.False(t, full)
	assert.False(t, needed)
}

func TestCompleteSessionAliasBackfillMarksDoneUnlessErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  PushResult
		want string
	}{
		{name: "clean", want: "1"},
		{name: "errors", res: PushResult{Errors: 1}},
		// Skipped ownership conflicts are other machines' sessions this host
		// can never push; they must not block the one-time backfill marker,
		// or a shared hub can never leave the forced-full-push state.
		{name: "skipped conflicts", res: PushResult{SkippedConflicts: 1}, want: "1"},
		{
			name: "errors with skipped conflicts",
			res:  PushResult{Errors: 1, SkippedConflicts: 2},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &syncStateStoreStub{}

			err := completeSessionAliasBackfill(
				store, true, tc.res,
			)

			require.NoError(t, err)
			assert.Equal(t, tc.want,
				store.values[sessionAliasBackfillStateKey])
		})
	}
}

func TestSessionAliasBackfillRequirementUsesSyncAliasStateAcrossFilters(t *testing.T) {
	filterScopeA := pushSyncStateScope(
		"work",
		[]string{"alpha"},
		nil,
	)
	filterScopeB := pushSyncStateScope(
		"work",
		[]string{"beta"},
		nil,
	)

	store := &syncStateStoreStub{}
	syncA := &Sync{
		syncState:          newScopedSyncStateStore(store, filterScopeA, false),
		aliasBackfillState: newScopedSyncStateStore(store, "work", false),
	}
	syncB := &Sync{
		syncState:          newScopedSyncStateStore(store, filterScopeB, false),
		aliasBackfillState: newScopedSyncStateStore(store, "work", false),
	}

	full, needed, err := applySessionAliasBackfillRequirement(
		syncA.aliasBackfillSyncStateOrDefault(), false,
	)
	require.NoError(t, err)
	assert.True(t, full)
	assert.True(t, needed)

	require.NoError(t, completeSessionAliasBackfill(
		syncA.aliasBackfillSyncStateOrDefault(),
		true,
		PushResult{},
	))

	full, needed, err = applySessionAliasBackfillRequirement(
		syncB.aliasBackfillSyncStateOrDefault(), false,
	)
	require.NoError(t, err)
	assert.False(t, full, "head behavior: target scope keeps marker across filters")
	assert.False(t, needed, "head behavior: marker should not re-arm full push")
	assert.Empty(t, store.values[sessionAliasBackfillStateKey+":"+filterScopeA])
	assert.Empty(t, store.values[sessionAliasBackfillStateKey+":"+filterScopeB])
	assert.Equal(t, "1", store.values[sessionAliasBackfillStateKey+":work"])
}

func TestPushUsesTargetScopedAliasBackfillStateAcrossFilters(t *testing.T) {
	filterScopeA := pushSyncStateScope("work", []string{"alpha"}, nil)
	filterScopeB := pushSyncStateScope("work", []string{"beta"}, nil)
	local := testDB(t)
	pg := newPushAliasRoutingDB(t)
	ctx := context.Background()

	syncA := &Sync{
		pg:                 pg,
		local:              local,
		syncState:          newScopedSyncStateStore(local, filterScopeA, false),
		aliasBackfillState: newScopedSyncStateStore(local, "work", false),
		machine:            "push-machine",
		schema:             "agentsview",
		targetFingerprint:  "target-fp",
		syncStateTarget:    filterScopeA,
		projects:           []string{"alpha"},
		schemaDone:         true,
	}
	syncB := &Sync{
		pg:                 pg,
		local:              local,
		syncState:          newScopedSyncStateStore(local, filterScopeB, false),
		aliasBackfillState: newScopedSyncStateStore(local, "work", false),
		machine:            "push-machine",
		schema:             "agentsview",
		targetFingerprint:  "target-fp",
		syncStateTarget:    filterScopeB,
		projects:           []string{"beta"},
		schemaDone:         true,
	}

	_, err := syncA.Push(ctx, false, nil)
	require.NoError(t, err)
	_, err = syncB.Push(ctx, false, nil)
	require.NoError(t, err)

	targetMarker, err := local.GetSyncState(
		sessionAliasBackfillStateKey + ":work",
	)
	require.NoError(t, err)
	assert.Equal(t, "1", targetMarker)

	filterMarkerB, err := local.GetSyncState(
		sessionAliasBackfillStateKey + ":" + filterScopeB,
	)
	require.NoError(t, err)
	assert.Empty(t, filterMarkerB)

	filteredLastPushA, err := local.GetSyncState("last_push_at:" + filterScopeA)
	require.NoError(t, err)
	assert.NotEmpty(t, filteredLastPushA)

	filteredLastPushB, err := local.GetSyncState("last_push_at:" + filterScopeB)
	require.NoError(t, err)
	assert.NotEmpty(t, filteredLastPushB)

	unscopedLastPush, err := local.GetSyncState("last_push_at:work")
	require.NoError(t, err)
	assert.Empty(t, unscopedLastPush)
}

func TestPrepareSessionsForPushWithholdsUnverifiedCodexSessions(t *testing.T) {
	const fernet = "gAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="
	ctx := context.Background()
	local := testDB(t)
	pg := newPushAliasRoutingDB(t)
	preview := fernet
	bad := db.Session{
		ID:               "legacy-codex-ciphertext",
		Machine:          "push-machine",
		Agent:            "codex",
		FirstMessage:     &preview,
		CreatedAt:        "2026-07-14T12:00:00Z",
		RelationshipType: "subagent",
		DataVersion:      64,
	}
	// An unlinked child: no subagent relationship metadata, but its user
	// turn is a bare encrypted delivery. Verification must withhold it even
	// though the scoped scrub's filters never examined it.
	unlinkedChild := db.Session{
		ID:          "legacy-codex-unlinked-child",
		Machine:     "push-machine",
		Agent:       "codex",
		CreatedAt:   "2026-07-14T12:00:03Z",
		DataVersion: 64,
	}
	legacyClean := db.Session{
		ID:          "legacy-codex-clean",
		Machine:     "push-machine",
		Agent:       "codex",
		CreatedAt:   "2026-07-14T12:00:02Z",
		DataVersion: 64,
	}
	good := db.Session{
		ID:          "unrelated-clean-session",
		Machine:     "push-machine",
		Agent:       "claude",
		CreatedAt:   "2026-07-14T12:00:01Z",
		DataVersion: db.CurrentDataVersion(),
	}
	require.NoError(t, local.UpsertSession(bad), "store bad session")
	require.NoError(t, local.UpsertSession(unlinkedChild), "store unlinked child")
	require.NoError(t, local.InsertMessages([]db.Message{{
		SessionID: unlinkedChild.ID,
		Ordinal:   0,
		Role:      "user",
		Content:   fernet,
	}}), "store unlinked child delivery")
	require.NoError(t, local.UpsertSession(legacyClean), "store legacy session")
	require.NoError(t, local.UpsertSession(good), "store good session")
	for _, sess := range []db.Session{bad, unlinkedChild, legacyClean, good} {
		require.NoError(t, local.SetSessionDataVersion(sess.ID, sess.DataVersion),
			"stamp parser version for %s", sess.ID)
	}
	storedSession := func(id string) db.Session {
		sess, err := local.GetSession(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, sess)
		return *sess
	}
	bad = storedSession(bad.ID)
	unlinkedChild = storedSession(unlinkedChild.ID)
	legacyClean = storedSession(legacyClean.ID)
	good = storedSession(good.ID)

	syncer := &Sync{pg: pg, local: local, machine: "push-machine"}
	prepared, failed, withheld, verifiedMessages := syncer.prepareSessionsForPush(
		ctx, []db.Session{bad, unlinkedChild, legacyClean, good},
	)
	assert.Empty(t, failed,
		"unverified sessions are withheld, not counted as errors")
	require.Len(t, withheld, 2,
		"unsafe sessions must be withheld and reported")
	assert.Equal(t, bad.ID, withheld[0].ID)
	assert.Equal(t, unlinkedChild.ID, withheld[1].ID)
	require.Len(t, prepared, 2)
	assert.Equal(t, legacyClean.ID, prepared[0].ID)
	assert.Equal(t, db.CodexRedactionDataVersion, prepared[0].DataVersion,
		"a verified legacy Codex session publishes the watermark")
	assert.Equal(t, 64, local.GetSessionDataVersion(legacyClean.ID),
		"payload certification must not advance the parser version")
	unverified, err := local.UnverifiedCodexSessionIDs(ctx)
	require.NoError(t, err)
	assert.NotContains(t, unverified, legacyClean.ID,
		"successful certification must persist for later vector-only pushes")
	assert.Equal(t, good.ID, prepared[1].ID)

	var pushed []db.Session
	goodResult, err := syncer.pushBatch(
		ctx, prepared, true, "marker", nil, nil, verifiedMessages, &pushed,
	)
	require.NoError(t, err)
	assert.True(t, goodResult.ok, "prepared sessions must still push")
	assert.Equal(t, 2, goodResult.sessions)
	require.Len(t, pushed, 2)
	assert.Equal(t, legacyClean.ID, pushed[0].ID)
	assert.Equal(t, good.ID, pushed[1].ID)
}

func TestSessionAliasBackfillStateFallsBackToEffectiveScope(t *testing.T) {
	store := &syncStateStoreStub{}
	scope := pushSyncStateScope("work", []string{"alpha"}, nil)
	syncer := &Sync{
		syncState: newScopedSyncStateStore(store, scope, false),
	}

	require.NoError(t, markSessionAliasBackfillDone(
		syncer.aliasBackfillSyncStateOrDefault(),
	))
	assert.Equal(t, "1", store.values[sessionAliasBackfillStateKey+":"+scope])
}

func TestSessionAliasBackfillKeysStayFilteredForPushState(t *testing.T) {
	filterScopeA := pushSyncStateScope("work", []string{"alpha"}, nil)
	filterScopeB := pushSyncStateScope("work", []string{"beta"}, nil)
	store := &syncStateStoreStub{}
	syncA := &Sync{
		syncState:          newScopedSyncStateStore(store, filterScopeA, false),
		aliasBackfillState: newScopedSyncStateStore(store, "work", false),
	}

	require.NoError(t, syncA.effectiveSyncState().SetSyncState(
		"last_push_at", "2026-03-11T12:00:00.000Z",
	))
	require.NoError(t, syncA.effectiveSyncState().SetSyncState(
		lastPushBoundaryStateKey, `{"cutoff":"2026-03-11T12:00:00.000Z"}`,
	))
	require.NoError(t, syncA.effectiveSyncState().SetSyncState(
		lastPushTargetFingerprintKey, "fp-1",
	))
	require.NoError(t, markSessionAliasBackfillDone(
		syncA.aliasBackfillSyncStateOrDefault(),
	))

	assert.Equal(
		t,
		"2026-03-11T12:00:00.000Z",
		store.values["last_push_at:"+filterScopeA],
	)
	assert.Equal(
		t,
		`{"cutoff":"2026-03-11T12:00:00.000Z"}`,
		store.values[lastPushBoundaryStateKey+":"+filterScopeA],
	)
	assert.Equal(
		t,
		"fp-1",
		store.values[lastPushTargetFingerprintKey+":"+filterScopeA],
	)
	assert.Empty(t, store.values["last_push_at:"+filterScopeB])
	assert.Empty(t, store.values[lastPushBoundaryStateKey+":"+filterScopeB])
	assert.Empty(t, store.values[lastPushTargetFingerprintKey+":"+filterScopeB])
	assert.Empty(t, store.values[sessionAliasBackfillStateKey+":"+filterScopeA])
	assert.Empty(t, store.values[sessionAliasBackfillStateKey+":"+filterScopeB])
	assert.Equal(
		t,
		"1",
		store.values[sessionAliasBackfillStateKey+":work"],
	)
	assert.Empty(t, store.values["last_push_at:work"])
}

func TestReadPushBoundaryStateValidity(t *testing.T) {
	const cutoff = "2026-03-11T12:34:56.123Z"

	tests := []struct {
		name      string
		raw       string
		wantValid bool
		wantLen   int
	}{
		{
			name:      "missing state",
			raw:       "",
			wantValid: false,
			wantLen:   0,
		},
		{
			name:      "bare map without cutoff",
			raw:       `{"sess-001":"fingerprint"}`,
			wantValid: false,
			wantLen:   0,
		},
		{
			name:      "malformed payload",
			raw:       `{`,
			wantValid: false,
			wantLen:   0,
		},
		{
			name:      "stale cutoff",
			raw:       `{"cutoff":"2026-03-11T12:34:56.122Z","fingerprints":{"sess-001":"fingerprint"}}`,
			wantValid: false,
			wantLen:   0,
		},
		{
			name:      "matching cutoff",
			raw:       `{"cutoff":"2026-03-11T12:34:56.123Z","fingerprints":{"sess-001":"fingerprint"}}`,
			wantValid: true,
			wantLen:   1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, got, valid, err := readBoundaryAndFingerprints(
				syncStateReaderStub{value: tc.raw},
				cutoff,
			)
			require.NoError(t, err)
			require.Equal(t, tc.wantValid, valid)
			require.Len(t, got, tc.wantLen)
		})
	}
}

func TestLocalSessionSyncMarkerNormalizesSecondPrecisionTimestamps(t *testing.T) {
	startedAt := "2026-03-11T12:34:56Z"
	endedAt := "2026-03-11T12:34:56.123Z"

	got := localSessionSyncMarker(db.Session{
		CreatedAt: "2026-03-11T12:34:55Z",
		StartedAt: &startedAt,
		EndedAt:   &endedAt,
	})

	require.Equal(t, endedAt, got)
}

func TestPGExcludedSessionIDsQueryUsesSingleArrayParameter(t *testing.T) {
	query, args := pgExcludedSessionIDsQuery([]string{
		"sess-001",
		"sess-002",
		"sess-003",
	})

	assert.Contains(t, query, "id = ANY($1)")
	assert.NotContains(t, query, "$2")
	require.Len(t, args, 1)
	assert.Equal(t,
		[]string{"sess-001", "sess-002", "sess-003"},
		args[0],
	)
}

func TestDeletePGExcludedSessionRowsUsesSingleArrayParameter(t *testing.T) {
	execer := &capturePGExec{}

	require.NoError(t, deletePGExcludedSessionRows(
		context.Background(), execer,
		[]string{"sess-001", "sess-002", "sess-003"},
	))

	assert.Contains(t, execer.query, "DELETE FROM sessions")
	assert.Contains(t, execer.query, "id = ANY($1)")
	require.Len(t, execer.args, 1)
	assert.Equal(t,
		[]string{"sess-001", "sess-002", "sess-003"},
		execer.args[0],
	)
}

type capturePGExec struct {
	query string
	args  []any
}

func (c *capturePGExec) ExecContext(
	_ context.Context, query string, args ...any,
) (sql.Result, error) {
	c.query = query
	c.args = args
	return driver.RowsAffected(0), nil
}

func TestPushSessionRechecksExclusionAfterSuccessfulUpsert(t *testing.T) {
	state := &pushSessionProbeState{
		existingExcluded: map[string]bool{
			"sess-race": true,
		},
	}
	pg := newPushSessionProbeDB(t, state)
	tx, err := pg.BeginTx(context.Background(), nil)
	require.NoError(t, err, "BeginTx")

	syncer := &Sync{machine: "push-machine"}
	err = syncer.pushSession(
		context.Background(), tx,
		db.Session{
			ID:        "sess-race",
			Project:   "proj",
			Machine:   "push-machine",
			Agent:     "claude",
			CreatedAt: "2026-01-01T00:00:00Z",
		},
		"marker", nil,
	)

	require.ErrorIs(t, err, errSessionExcluded)
	assert.Equal(t, 1, state.upserts)
	assert.Equal(t, 1, state.exclusionChecks)
	assert.True(t, state.deletedExcluded,
		"excluded row should be deleted after the tombstone is observed")
	require.NoError(t, tx.Rollback(), "Rollback")
}

func TestPushSessionStoresVibeFallbackAlias(t *testing.T) {
	state := &pushSessionProbeState{aliases: map[string]string{}}
	pg := newPushSessionProbeDB(t, state)
	tx, err := pg.BeginTx(context.Background(), nil)
	require.NoError(t, err, "BeginTx")

	sessionDir := filepath.Join(
		t.TempDir(),
		"session_20260616_083518_abc123",
	)
	filePath := filepath.Join(sessionDir, "messages.jsonl")
	syncer := &Sync{machine: "push-machine"}
	err = syncer.pushSession(
		context.Background(), tx,
		db.Session{
			ID:        "vibe:canonical-uuid",
			Project:   "proj",
			Machine:   "push-machine",
			Agent:     "vibe",
			CreatedAt: "2026-01-01T00:00:00Z",
			FilePath:  &filePath,
		},
		"marker", nil,
	)

	require.NoError(t, err, "pushSession")
	assert.Equal(t,
		"vibe:session_20260616_083518_abc123",
		state.aliases["vibe:canonical-uuid"],
	)
	require.NoError(t, tx.Rollback(), "Rollback")
}

func TestPushSessionExcludesVibeFallbackAliasWhenCanonicalExcluded(t *testing.T) {
	state := &pushSessionProbeState{
		existingExcluded: map[string]bool{
			"vibe:canonical-deleted": true,
		},
		excludedIDs: map[string]bool{},
	}
	pg := newPushSessionProbeDB(t, state)
	tx, err := pg.BeginTx(context.Background(), nil)
	require.NoError(t, err, "BeginTx")

	sessionDir := filepath.Join(
		t.TempDir(),
		"session_20260616_083518_def456",
	)
	filePath := filepath.Join(sessionDir, "messages.jsonl")
	syncer := &Sync{machine: "push-machine"}
	err = syncer.pushSession(
		context.Background(), tx,
		db.Session{
			ID:        "vibe:canonical-deleted",
			Project:   "proj",
			Machine:   "push-machine",
			Agent:     "vibe",
			CreatedAt: "2026-01-01T00:00:00Z",
			FilePath:  &filePath,
		},
		"marker", nil,
	)

	require.ErrorIs(t, err, errSessionExcluded)
	assert.True(t,
		state.excludedIDs["vibe:session_20260616_083518_def456"],
		"excluded canonical Vibe pushes should tombstone the fallback alias",
	)
	require.NoError(t, tx.Rollback(), "Rollback")
}

func TestPushSessionSkipsVibeCanonicalWhenFallbackAliasExcluded(t *testing.T) {
	state := &pushSessionProbeState{
		existingExcluded: map[string]bool{
			"vibe:session_20260616_083518_ghi789": true,
		},
		excludedIDs: map[string]bool{},
	}
	pg := newPushSessionProbeDB(t, state)
	tx, err := pg.BeginTx(context.Background(), nil)
	require.NoError(t, err, "BeginTx")

	sessionDir := filepath.Join(
		t.TempDir(),
		"session_20260616_083518_ghi789",
	)
	filePath := filepath.Join(sessionDir, "messages.jsonl")
	syncer := &Sync{machine: "push-machine"}
	err = syncer.pushSession(
		context.Background(), tx,
		db.Session{
			ID:        "vibe:canonical-active",
			Project:   "proj",
			Machine:   "push-machine",
			Agent:     "vibe",
			CreatedAt: "2026-01-01T00:00:00Z",
			FilePath:  &filePath,
		},
		"marker", nil,
	)

	require.ErrorIs(t, err, errSessionExcluded)
	assert.True(t, state.excludedIDs["vibe:canonical-active"])
	assert.True(t,
		state.excludedIDs["vibe:session_20260616_083518_ghi789"],
	)
	assert.True(t, state.deletedExcluded,
		"all excluded aliases should be purged from PG sessions")
	require.NoError(t, tx.Rollback(), "Rollback")
}

func TestPurgePGExcludedPushSessionsChecksDerivedAliases(t *testing.T) {
	state := &pushSessionProbeState{
		existingExcluded: map[string]bool{
			"vibe:session_20260616_083518_jkl012": true,
		},
		excludedIDs: map[string]bool{},
	}
	pg := newPushSessionProbeDB(t, state)

	sessionDir := filepath.Join(
		t.TempDir(),
		"session_20260616_083518_jkl012",
	)
	filePath := filepath.Join(sessionDir, "messages.jsonl")
	sessionByID := map[string]db.Session{
		"vibe:canonical-unchanged": {
			ID:        "vibe:canonical-unchanged",
			Project:   "proj",
			Machine:   "push-machine",
			Agent:     "vibe",
			CreatedAt: "2026-01-01T00:00:00Z",
			FilePath:  &filePath,
		},
	}

	err := purgePGExcludedPushSessions(
		context.Background(), pg, sessionByID,
	)

	require.NoError(t, err, "purgePGExcludedPushSessions")
	assert.Empty(t, sessionByID)
	assert.True(t, state.excludedIDs["vibe:canonical-unchanged"])
	assert.True(t,
		state.excludedIDs["vibe:session_20260616_083518_jkl012"],
	)
	assert.True(t, state.deletedExcluded,
		"all excluded aliases should be purged before fingerprint pruning")
	assert.Equal(t, 1, state.exclusionChecks)
	assert.Equal(t, 0, state.upserts)
}

type pushSessionProbeDriver struct{}

type pushSessionProbeConn struct {
	state *pushSessionProbeState
}

type pushSessionProbeTx struct{}

type pushSessionProbeRows struct {
	columns []string
	values  [][]driver.Value
	next    int
}

type pushSessionProbeState struct {
	mu                  sync.Mutex
	excludedAfterUpsert bool
	upserts             int
	exclusionChecks     int
	deletedExcluded     bool
	aliases             map[string]string
	excludedIDs         map[string]bool
	existingExcluded    map[string]bool
}

var (
	pushSessionProbeRegisterOnce sync.Once
	pushSessionProbeStatesMu     sync.Mutex
	pushSessionProbeStates       = map[string]*pushSessionProbeState{}
)

func newPushSessionProbeDB(
	t *testing.T, state *pushSessionProbeState,
) *sql.DB {
	t.Helper()
	pushSessionProbeRegisterOnce.Do(func() {
		sql.Register("agentsview_push_session_probe", pushSessionProbeDriver{})
	})
	name := t.Name()
	pushSessionProbeStatesMu.Lock()
	pushSessionProbeStates[name] = state
	pushSessionProbeStatesMu.Unlock()
	t.Cleanup(func() {
		pushSessionProbeStatesMu.Lock()
		delete(pushSessionProbeStates, name)
		pushSessionProbeStatesMu.Unlock()
	})

	pg, err := sql.Open("agentsview_push_session_probe", name)
	require.NoError(t, err, "open push-session probe db")
	t.Cleanup(func() { _ = pg.Close() })
	return pg
}

func (pushSessionProbeDriver) Open(name string) (driver.Conn, error) {
	pushSessionProbeStatesMu.Lock()
	state := pushSessionProbeStates[name]
	pushSessionProbeStatesMu.Unlock()
	return &pushSessionProbeConn{state: state}, nil
}

func (c *pushSessionProbeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not implemented")
}

func (c *pushSessionProbeConn) Close() error { return nil }

func (c *pushSessionProbeConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *pushSessionProbeConn) BeginTx(
	context.Context, driver.TxOptions,
) (driver.Tx, error) {
	return pushSessionProbeTx{}, nil
}

func (c *pushSessionProbeConn) CheckNamedValue(
	*driver.NamedValue,
) error {
	return nil
}

func (c *pushSessionProbeConn) ExecContext(
	_ context.Context, query string, args []driver.NamedValue,
) (driver.Result, error) {
	normalized := strings.ToLower(query)
	c.state.mu.Lock()
	defer c.state.mu.Unlock()

	switch {
	case strings.Contains(normalized, "insert into sessions"):
		c.state.upserts++
		return driver.RowsAffected(1), nil
	case strings.Contains(normalized, "delete from sessions") &&
		strings.Contains(normalized, "id = any($1)"):
		c.state.deletedExcluded = true
		return driver.RowsAffected(1), nil
	case strings.Contains(normalized, "delete from session_aliases"):
		if c.state.aliases != nil && len(args) > 0 {
			if sessionID, ok := args[0].Value.(string); ok {
				delete(c.state.aliases, sessionID)
			}
		}
		return driver.RowsAffected(1), nil
	case strings.Contains(normalized, "insert into session_aliases"):
		if c.state.aliases != nil && len(args) > 1 {
			sessionID, sessionOK := args[0].Value.(string)
			aliasID, aliasOK := args[1].Value.(string)
			if sessionOK && aliasOK {
				c.state.aliases[sessionID] = aliasID
			}
		}
		return driver.RowsAffected(1), nil
	case strings.Contains(normalized, "insert into excluded_sessions"):
		if c.state.excludedIDs != nil && len(args) > 0 {
			switch ids := args[0].Value.(type) {
			case []string:
				for _, id := range ids {
					c.state.excludedIDs[id] = true
				}
			case string:
				c.state.excludedIDs[ids] = true
			}
		}
		return driver.RowsAffected(1), nil
	default:
		return nil, errors.New("unexpected push-session probe exec")
	}
}

func (c *pushSessionProbeConn) QueryContext(
	_ context.Context, query string, args []driver.NamedValue,
) (driver.Rows, error) {
	normalized := strings.ToLower(query)
	c.state.mu.Lock()
	defer c.state.mu.Unlock()

	switch {
	case strings.Contains(normalized, "select machine, owner_marker"):
		return &pushSessionProbeRows{
			columns: []string{"machine", "owner_marker"},
		}, nil
	case strings.Contains(normalized, "select id") &&
		strings.Contains(normalized, "excluded_sessions") &&
		strings.Contains(normalized, "id = any($1)"):
		c.state.exclusionChecks++
		values := [][]driver.Value{}
		for _, id := range namedValueStrings(args) {
			if c.state.existingExcluded[id] {
				values = append(values, []driver.Value{id})
			}
		}
		return &pushSessionProbeRows{
			columns: []string{"id"},
			values:  values,
		}, nil
	case strings.Contains(normalized, "select exists") &&
		strings.Contains(normalized, "excluded_sessions"):
		c.state.exclusionChecks++
		excluded := c.state.excludedAfterUpsert
		if c.state.existingExcluded != nil && len(args) > 0 {
			id, _ := args[0].Value.(string)
			excluded = c.state.existingExcluded[id]
		}
		return &pushSessionProbeRows{
			columns: []string{"exists"},
			values: [][]driver.Value{
				{excluded},
			},
		}, nil
	default:
		return nil, errors.New("unexpected push-session probe query")
	}
}

func (pushSessionProbeTx) Commit() error { return nil }

func (pushSessionProbeTx) Rollback() error { return nil }

func (r *pushSessionProbeRows) Columns() []string { return r.columns }

func (r *pushSessionProbeRows) Close() error { return nil }

func (r *pushSessionProbeRows) Next(dest []driver.Value) error {
	if r.next >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.next])
	r.next++
	return nil
}

func TestSessionPushFingerprintDiffers(t *testing.T) {
	base := db.Session{
		ID:               "sess-001",
		Project:          "proj",
		Machine:          "laptop",
		Agent:            "claude",
		MessageCount:     5,
		UserMessageCount: 2,
		CreatedAt:        "2026-03-11T12:00:00Z",
	}

	fp1 := sessionPushFingerprint(base, base.Machine, "", "", "")

	tests := []struct {
		name   string
		modify func(s db.Session) db.Session
	}{
		{
			name: "agent label change",
			modify: func(s db.Session) db.Session {
				s.AgentLabel = "triage"
				return s
			},
		},
		{
			name: "entrypoint change",
			modify: func(s db.Session) db.Session {
				s.Entrypoint = "sdk-cli"
				return s
			},
		},
		{
			name: "message count change",
			modify: func(s db.Session) db.Session {
				s.MessageCount = 6
				return s
			},
		},
		{
			name: "display name change",
			modify: func(s db.Session) db.Session {
				name := "new name"
				s.DisplayName = &name
				return s
			},
		},
		{
			name: "session_name change",
			modify: func(s db.Session) db.Session {
				n := "agent-provided-title"
				s.SessionName = &n
				return s
			},
		},
		{
			name: "ended at change",
			modify: func(s db.Session) db.Session {
				ended := "2026-03-11T13:00:00Z"
				s.EndedAt = &ended
				return s
			},
		},
		{
			name: "file hash change",
			modify: func(s db.Session) db.Session {
				hash := "abc123"
				s.FileHash = &hash
				return s
			},
		},
		{
			name: "file path change",
			modify: func(s db.Session) db.Session {
				path := "/home/test/.vibe/sessions/alias.jsonl"
				s.FilePath = &path
				return s
			},
		},
		{
			name: "termination_status change",
			modify: func(s db.Session) db.Session {
				ts := "tool_call_pending"
				s.TerminationStatus = &ts
				return s
			},
		},
		{
			name: "automated classification change",
			modify: func(s db.Session) db.Session {
				s.IsAutomated = true
				return s
			},
		},
		{
			name: "quality signal version change",
			modify: func(s db.Session) db.Session {
				s.QualitySignalVersion = db.CurrentQualitySignalVersion
				return s
			},
		},
		{
			name: "quality signal count change",
			modify: func(s db.Session) db.Session {
				s.QualitySignalVersion = db.CurrentQualitySignalVersion
				s.DuplicatePromptCount = 1
				return s
			},
		},
		{
			name: "quality signal boolean change",
			modify: func(s db.Session) db.Session {
				s.QualitySignalVersion = db.CurrentQualitySignalVersion
				s.UnstructuredStart = true
				return s
			},
		},
		{
			name: "Codex certification revision change",
			modify: func(s db.Session) db.Session {
				s.CodexPayloadCertifiedRevision = "3"
				return s
			},
		},
		{
			name: "Codex certification version change",
			modify: func(s db.Session) db.Session {
				s.CodexPayloadCertificationVersion =
					db.CodexRedactionDataVersion
				return s
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			modified := tc.modify(base)
			fp2 := sessionPushFingerprint(modified, modified.Machine, "", "", "")
			require.NotEqual(t, fp1, fp2,
				"fingerprint should differ after %s", tc.name)
		})
	}

	assert.Equal(t, fp1, sessionPushFingerprint(base, base.Machine, "", "", ""),
		"identical sessions should produce identical fingerprints")
}

func TestSessionPushFingerprintIgnoresVolatileStatFields(t *testing.T) {
	hash := "content-hash"
	localModifiedAt := "2026-03-11T12:00:00.000Z"
	fileMtime := int64(1700000000000000000)
	base := db.Session{
		ID:               "sess-001",
		Project:          "proj",
		Machine:          "laptop",
		Agent:            "claude",
		MessageCount:     5,
		UserMessageCount: 2,
		FileHash:         &hash,
		FileMtime:        &fileMtime,
		LocalModifiedAt:  &localModifiedAt,
		CreatedAt:        "2026-03-11T12:00:00Z",
	}
	baseFP := sessionPushFingerprint(base, base.Machine, "", "", "deps")

	statOnlyMtime := int64(1700000001000000000)
	statOnlyModifiedAt := "2026-03-11T12:00:01.000Z"
	statOnly := base
	statOnly.FileMtime = &statOnlyMtime
	statOnly.LocalModifiedAt = &statOnlyModifiedAt
	assert.Equal(t, baseFP, sessionPushFingerprint(statOnly, statOnly.Machine, "", "", "deps"),
		"file stat churn should not change push candidacy")

	contentChanged := statOnly
	contentChanged.MessageCount++
	assert.NotEqual(t, baseFP,
		sessionPushFingerprint(contentChanged, contentChanged.Machine, "", "", "deps"),
		"content changes should still change push candidacy")

	assert.NotEqual(t, baseFP,
		sessionPushFingerprint(statOnly, statOnly.Machine, "", "", "changed-deps"),
		"dependent row changes should still change push candidacy")
}

func TestLocalSessionDependencyPushFingerprintTracksMessageEditsWithoutFileHash(
	t *testing.T,
) {
	localDB := testDB(t)
	const sessID = "sess-message-edit"
	require.NoError(t, localDB.UpsertSession(db.Session{
		ID:               sessID,
		Project:          "proj",
		Machine:          "laptop",
		Agent:            "claude",
		MessageCount:     1,
		UserMessageCount: 1,
		CreatedAt:        "2026-03-11T12:00:00Z",
	}))
	require.NoError(t, localDB.InsertMessages([]db.Message{{
		SessionID: sessID,
		Ordinal:   0,
		Role:      "user",
		Content:   "before",
	}}))

	depsBefore, err := localSessionDependencyPushFingerprint(
		context.Background(), localDB, sessID, "", true,
	)
	require.NoError(t, err)
	session := db.Session{
		ID:               sessID,
		Project:          "proj",
		Machine:          "laptop",
		Agent:            "claude",
		MessageCount:     1,
		UserMessageCount: 1,
		CreatedAt:        "2026-03-11T12:00:00Z",
	}
	fpBefore := sessionPushFingerprint(
		session, session.Machine, "", "", depsBefore,
	)

	require.NoError(t, localDB.ReplaceSessionMessages(sessID, []db.Message{{
		SessionID: sessID,
		Ordinal:   0,
		Role:      "user",
		Content:   "after",
	}}))
	depsAfter, err := localSessionDependencyPushFingerprint(
		context.Background(), localDB, sessID, "", true,
	)
	require.NoError(t, err)
	fpAfter := sessionPushFingerprint(
		session, session.Machine, "", "", depsAfter,
	)

	assert.NotEqual(t, depsBefore, depsAfter)
	assert.NotEqual(t, fpBefore, fpAfter,
		"same-count message edits should remain push candidates without file_hash")
}

func TestSessionPushFingerprintIncludesUsageEventFingerprint(
	t *testing.T,
) {
	base := db.Session{
		ID:               "sess-001",
		Project:          "proj",
		Machine:          "laptop",
		Agent:            "claude",
		MessageCount:     5,
		UserMessageCount: 2,
		CreatedAt:        "2026-03-11T12:00:00Z",
	}

	withoutUsage := sessionPushFingerprint(base, base.Machine, "", "", "")
	withUsage := sessionPushFingerprint(base, base.Machine, "usage-fp", "", "")
	assert.NotEqual(t, withoutUsage, withUsage,
		"usage event fingerprint should affect session fingerprint")
}

func TestSessionPushFingerprintTracksResolvedMachine(t *testing.T) {
	sentinel := db.Session{
		ID:        "sess-001",
		Project:   "proj",
		Machine:   "local",
		Agent:     "claude",
		CreatedAt: "2026-03-11T12:00:00Z",
	}
	fpA := sessionPushFingerprint(
		sentinel, pushedSessionMachine(sentinel, "host-a"), "", "", "")
	fpB := sessionPushFingerprint(
		sentinel, pushedSessionMachine(sentinel, "host-b"), "", "", "")
	assert.NotEqual(t, fpA, fpB,
		"sentinel session fingerprint must change with the fallback machine")

	real := db.Session{
		ID:        "sess-002",
		Project:   "proj",
		Machine:   "real-host",
		Agent:     "claude",
		CreatedAt: "2026-03-11T12:00:00Z",
	}
	fp1 := sessionPushFingerprint(
		real, pushedSessionMachine(real, "host-a"), "", "", "")
	fp2 := sessionPushFingerprint(
		real, pushedSessionMachine(real, "host-b"), "", "", "")
	assert.Equal(t, fp1, fp2,
		"a session with a real machine ignores the fallback")
}

func TestPushedSessionMachine(t *testing.T) {
	tests := []struct {
		name     string
		session  db.Session
		fallback string
		want     string
	}{
		{
			name: "preserves source machine",
			session: db.Session{
				Machine: "remote-host",
			},
			fallback: "push-host",
			want:     "remote-host",
		},
		{
			name:     "falls back for empty machine",
			session:  db.Session{},
			fallback: "push-host",
			want:     "push-host",
		},
		{
			name: "falls back for local sentinel",
			session: db.Session{
				Machine: "local",
			},
			fallback: "push-host",
			want:     "push-host",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want,
				pushedSessionMachine(tc.session, tc.fallback))
		})
	}
}

func TestSessionPushFingerprintNoFieldCollisions(
	t *testing.T,
) {
	s1 := db.Session{
		ID:        "ab",
		Project:   "cd",
		CreatedAt: "2026-03-11T12:00:00Z",
	}
	s2 := db.Session{
		ID:        "a",
		Project:   "bcd",
		CreatedAt: "2026-03-11T12:00:00Z",
	}
	assert.NotEqual(t,
		sessionPushFingerprint(s1, s1.Machine, "", "", ""),
		sessionPushFingerprint(s2, s2.Machine, "", "", ""),
		"length-prefixed fingerprints should not collide")
}

func TestLocalMessageRoleTimePGFingerprintNormalizesNanoseconds(
	t *testing.T,
) {
	localDB, err := db.Open(filepath.Join(t.TempDir(), "local.db"))
	require.NoError(t, err, "db.Open")
	defer localDB.Close()

	const sessID = "pg-role-time-nanos"
	require.NoError(t, localDB.UpsertSession(db.Session{
		ID:        sessID,
		Project:   "proj",
		Machine:   "host",
		Agent:     "shelley",
		CreatedAt: "2026-03-11T12:34:56Z",
	}), "UpsertSession")
	require.NoError(t, localDB.InsertMessages([]db.Message{{
		SessionID:     sessID,
		Ordinal:       1,
		Role:          "assistant",
		Content:       "answer",
		ContentLength: len("answer"),
		Timestamp:     "2026-03-11T12:34:56.123456789Z",
	}}), "InsertMessages")

	got, err := localMessageRoleTimePGFingerprint(localDB, sessID)
	require.NoError(t, err)
	assert.Equal(t,
		"1|9:assistant|27:2026-03-11T12:34:56.123456Z;",
		got)

	raw, err := localDB.MessageRoleTimeFingerprint(sessID)
	require.NoError(t, err)
	assert.NotEqual(t, raw, got,
		"PG push fingerprint must not use raw nanosecond text")
}

func TestFinalizePushStatePersistsEmptyBoundary(
	t *testing.T,
) {
	const cutoff = "2026-03-11T12:34:56.123Z"

	store := &syncStateStoreStub{}
	require.NoError(t, finalizePushState(
		store, cutoff, nil, nil, map[string]string{},
	))
	assert.Equal(t, cutoff, store.values["last_push_at"])

	raw := store.values[lastPushBoundaryStateKey]
	require.NotEmpty(t, raw, "last_push_boundary_state should be written")

	var state pushBoundaryState
	require.NoError(t, json.Unmarshal([]byte(raw), &state))
	assert.Equal(t, cutoff, state.Cutoff)
	assert.Empty(t, state.Fingerprints)
}

func TestFinalizeUnfilteredPushStatePreservesPriorFingerprintsWithoutPushedSessions(
	t *testing.T,
) {
	const (
		lastPush = "2026-03-11T12:00:00.000Z"
		cutoff   = "2026-03-11T12:34:56.123Z"
	)

	store := &syncStateStoreStub{}
	require.NoError(t, finalizeUnfilteredPushState(
		store, lastPush, cutoff, nil,
		map[string]string{"sess-001": "fp-001"},
		map[string]string{},
		0,
	))
	assert.Equal(t, cutoff, store.values["last_push_at"])

	raw := store.values[lastPushBoundaryStateKey]
	require.NotEmpty(t, raw, "last_push_boundary_state should be written")

	var state pushBoundaryState
	require.NoError(t, json.Unmarshal([]byte(raw), &state))
	assert.Equal(t, cutoff, state.Cutoff)
	assert.Equal(t, "fp-001", state.Fingerprints["sess-001"])
}

func TestFinalizeUnfilteredPushStatePreservesSkippedFingerprintsAfterSuccessfulPush(
	t *testing.T,
) {
	const (
		lastPush = "2026-03-11T12:00:00.000Z"
		cutoff   = "2026-03-11T12:34:56.123Z"
	)
	pushed := []db.Session{{
		ID:        "sess-002",
		CreatedAt: "2026-03-11T12:10:00Z",
	}}

	store := &syncStateStoreStub{}
	require.NoError(t, finalizeUnfilteredPushState(
		store, lastPush, cutoff, pushed,
		map[string]string{"sess-001": "fp-001"},
		map[string]string{"sess-002": "fp-002"},
		0,
	))
	assert.Equal(t, cutoff, store.values["last_push_at"])

	raw := store.values[lastPushBoundaryStateKey]
	require.NotEmpty(t, raw, "last_push_boundary_state should be written")

	var state pushBoundaryState
	require.NoError(t, json.Unmarshal([]byte(raw), &state))
	assert.Equal(t, cutoff, state.Cutoff)
	assert.Equal(t, "fp-001", state.Fingerprints["sess-001"])
	assert.Equal(t, "fp-002", state.Fingerprints["sess-002"])
}

func TestFinalizeFilteredPushStateAdvancesScopedWatermark(
	t *testing.T,
) {
	const cutoff = "2026-03-11T12:34:56.123Z"

	store := &syncStateStoreStub{}
	require.NoError(t, finalizeFilteredPushState(
		store, "", cutoff, nil, nil, map[string]string{}, 0,
	))
	assert.Equal(t, cutoff, store.values["last_push_at"])

	raw := store.values[lastPushBoundaryStateKey]
	require.NotEmpty(t, raw, "last_push_boundary_state should be written")

	var state pushBoundaryState
	require.NoError(t, json.Unmarshal([]byte(raw), &state))
	assert.Equal(t, cutoff, state.Cutoff)
	assert.Empty(t, state.Fingerprints)
}

func TestFinalizeFilteredPushStatePreservesPriorFingerprintsOnSuccess(
	t *testing.T,
) {
	const cutoff = "2026-03-11T12:34:56.123Z"

	store := &syncStateStoreStub{}
	require.NoError(t, finalizeFilteredPushState(
		store, "", cutoff, nil,
		map[string]string{"sess-001": "fp-001"},
		map[string]string{},
		0,
	))
	assert.Equal(t, cutoff, store.values["last_push_at"])

	raw := store.values[lastPushBoundaryStateKey]
	require.NotEmpty(t, raw, "last_push_boundary_state should be written")

	var state pushBoundaryState
	require.NoError(t, json.Unmarshal([]byte(raw), &state))
	assert.Equal(t, cutoff, state.Cutoff)
	assert.Equal(t, "fp-001", state.Fingerprints["sess-001"])
}

func TestFinalizeFilteredPushStateKeepsWatermarkOnErrors(
	t *testing.T,
) {
	const lastPush = "2026-03-11T12:00:00.000Z"
	const cutoff = "2026-03-11T12:34:56.123Z"

	store := &syncStateStoreStub{}
	require.NoError(t, finalizeFilteredPushState(
		store, lastPush, cutoff, nil,
		map[string]string{"sess-001": "fp-001"},
		map[string]string{},
		1,
	))
	assert.Equal(t, lastPush, store.values["last_push_at"])

	raw := store.values[lastPushBoundaryStateKey]
	require.NotEmpty(t, raw, "last_push_boundary_state should be written")

	var state pushBoundaryState
	require.NoError(t, json.Unmarshal([]byte(raw), &state))
	assert.Equal(t, lastPush, state.Cutoff)
	assert.Equal(t, "fp-001", state.Fingerprints["sess-001"])
}

func TestPushTargetState(t *testing.T) {
	tests := []struct {
		name       string
		lastPush   string
		boundary   string
		stored     string
		current    string
		wantReset  bool
		wantReason string
	}{
		{
			name:      "first push has no reset",
			stored:    "",
			current:   "v1:new",
			wantReset: false,
		},
		{
			name:      "missing runtime fingerprint skips reset",
			lastPush:  "2026-03-11T12:34:56.123Z",
			stored:    "v1:old",
			current:   "",
			wantReset: false,
		},
		{
			name:       "legacy watermark without fingerprint resets",
			lastPush:   "2026-03-11T12:34:56.123Z",
			current:    "v1:new",
			wantReset:  true,
			wantReason: "local push state exists without a stored PG target fingerprint",
		},
		{
			name:       "legacy filtered boundary without fingerprint resets",
			boundary:   `{"cutoff":"2026-03-11T12:34:56.123Z","fingerprints":{"sess-001":"fp"}}`,
			current:    "v1:new",
			wantReset:  true,
			wantReason: "local push state exists without a stored PG target fingerprint",
		},
		{
			name:       "changed target resets",
			lastPush:   "2026-03-11T12:34:56.123Z",
			stored:     "v1:old",
			current:    "v1:new",
			wantReset:  true,
			wantReason: "PG target fingerprint changed",
		},
		{
			name:      "same target keeps watermark",
			lastPush:  "2026-03-11T12:34:56.123Z",
			stored:    "v1:same",
			current:   "v1:same",
			wantReset: false,
		},
		{
			name:      "filtered boundary keeps same target state",
			boundary:  `{"cutoff":"2026-03-11T12:34:56.123Z","fingerprints":{"sess-001":"fp"}}`,
			stored:    "v1:same",
			current:   "v1:same",
			wantReset: false,
		},
		{
			name:       "filtered boundary resets on target change",
			boundary:   `{"cutoff":"2026-03-11T12:34:56.123Z","fingerprints":{"sess-001":"fp"}}`,
			stored:     "v1:old",
			current:    "v1:new",
			wantReset:  true,
			wantReason: "PG target fingerprint changed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotReset, gotReason := pushTargetState(
				tc.lastPush, tc.boundary, tc.stored, tc.current,
			)
			assert.Equal(t, tc.wantReset, gotReset)
			assert.Equal(t, tc.wantReason, gotReason)
		})
	}
}

func TestFinalizePushStateMergesPriorFingerprints(
	t *testing.T,
) {
	const cutoff = "2026-03-11T12:34:56.123Z"

	priorFingerprints := map[string]string{
		"sess-001": "fp-001",
	}

	cycle2Sessions := []db.Session{
		{
			ID:           "sess-002",
			CreatedAt:    "2026-03-11T12:00:00Z",
			MessageCount: 3,
		},
	}

	store := &syncStateStoreStub{}
	require.NoError(t, finalizePushState(
		store, cutoff, cycle2Sessions,
		priorFingerprints,
		map[string]string{"sess-002": sessionPushFingerprint(cycle2Sessions[0], cycle2Sessions[0].Machine, "", "", "")},
	))

	raw := store.values[lastPushBoundaryStateKey]
	require.NotEmpty(t, raw, "last_push_boundary_state should be written")

	var state pushBoundaryState
	require.NoError(t, json.Unmarshal([]byte(raw), &state))

	require.Len(t, state.Fingerprints, 2)
	assert.Equal(t, "fp-001", state.Fingerprints["sess-001"])
	_, ok := state.Fingerprints["sess-002"]
	assert.True(t, ok, "sess-002 fingerprint should be present")
}

func TestSanitizePG(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "clean string",
			input: "hello world",
			want:  "hello world",
		},
		{
			name:  "null bytes stripped",
			input: "hello\x00world",
			want:  "helloworld",
		},
		{
			name:  "multiple null bytes",
			input: "\x00a\x00b\x00",
			want:  "ab",
		},
		{
			name:  "truncated 3-byte sequence",
			input: "hello\xe2world",
			want:  "helloworld",
		},
		{
			name:  "truncated 2 of 3 bytes",
			input: "hello\xe2\x80world",
			want:  "helloworld",
		},
		{
			name: "valid multibyte preserved",
			// U+2026 HORIZONTAL ELLIPSIS = e2 80 a6
			input: "hello\xe2\x80\xa6world",
			want:  "hello\xe2\x80\xa6world",
		},
		{
			name:  "null and invalid combined",
			input: "a\x00b\xe2c",
			want:  "abc",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, sanitizePG(tc.input))
		})
	}
}

func TestNilIfEmptySanitizes(t *testing.T) {
	assert.Equal(t, any("helloworld"), nilIfEmpty("hello\x00world"))

	assert.Nil(t, nilIfEmpty(""), "nilIfEmpty(\"\") should be nil")

	// A string that reduces to empty after sanitization
	// should return nil, not "".
	assert.Nil(t, nilIfEmpty("\x00"), "nilIfEmpty(\"\\x00\") should be nil")
}

func TestNilStrSanitizes(t *testing.T) {
	s := "hello\xe2world"
	assert.Equal(t, any("helloworld"), nilStr(&s))

	// A *string that reduces to empty after sanitization
	// should return nil.
	nul := "\x00"
	assert.Nil(t, nilStr(&nul), "nilStr(\"\\x00\") should be nil")
}

func TestShouldSkipSessionMessagesInBatchedPush(t *testing.T) {
	const sessionID = "sess-batched"
	baseComparisons := &pushMessageComparison{
		MessageAggregates: map[string]pushMessageAggregate{
			sessionID: {Count: 2, Sum: 12, Max: 6, Min: 1},
		},
		MessageContentHash: map[string]string{
			sessionID: "abc",
		},
		MessageRoleTime: map[string]string{
			sessionID: "role-time",
		},
		MessageFlags: map[string]string{
			sessionID: "flags",
		},
		MessageSystemOrdinals: map[string]string{
			sessionID: "0,1",
		},
		MessageTokenFingerprint: map[string]string{
			sessionID: "tokens",
		},
		ToolCallAggregates: map[string]pushToolCallAggregate{
			sessionID: {Count: 1, Sum: 99},
		},
		ToolCallFingerprint: map[string]string{
			sessionID: "toolcalls",
		},
		ToolResultFingerprint: map[string]string{
			sessionID: "results",
		},
		UsageEventFingerprint: map[string]string{
			sessionID: "usage",
		},
	}
	unchangedFP := pushLocalMessageFingerprint{
		Sum:           12,
		Max:           6,
		Min:           1,
		ContentHashFP: "abc",
		RoleTimeFP:    "role-time",
		FlagsFP:       "flags",
		SystemFP:      "0,1",
		ToolCallCount: 1,
		ToolCallSum:   99,
		ToolCallFP:    "toolcalls",
		ToolResultFP:  "results",
		TokenFP:       "tokens",
		UsageEventFP:  "usage",
	}

	assert.True(t, shouldSkipSessionMessages(
		sessionID, 2, unchangedFP, false, baseComparisons,
	), "unchanged sessions should be skipped as unchanged")

	changedFP := unchangedFP
	changedFP.ToolCallSum = 100
	assert.False(t, shouldSkipSessionMessages(
		sessionID, 2, changedFP, false, baseComparisons,
	), "tool-call sum mismatch should force push")

	changedFP = unchangedFP
	changedFP.ToolResultFP = "changed-results"
	assert.False(t, shouldSkipSessionMessages(
		sessionID, 2, changedFP, false, baseComparisons,
	), "tool-result event mismatch should force push")

	assert.False(t, shouldSkipSessionMessages(
		sessionID, 2, unchangedFP, true, baseComparisons,
	), "full mode should not skip by fingerprint check")
}

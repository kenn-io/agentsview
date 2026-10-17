package codexcloud

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/codexcloud/codexcloudtest"
	"go.kenn.io/agentsview/internal/codexcloud/mirror"
)

type listingRow struct {
	id      string
	updated int64
	status  string
}

// servePages answers thread/list for listing with pages, following the
// cursor "page-N". Other listings return no threads.
func servePages(host *codexcloudtest.Host, pages map[ListingID][][]listingRow) {
	host.Handle("thread/list", func(params jsontext.Value) (any, *codexcloudtest.Error) {
		var p listParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &codexcloudtest.Error{Code: -32602, Message: err.Error()}
		}
		listing := ListDefaultActive
		switch {
		case p.Archived && len(p.SourceKinds) > 0:
			listing = ListSubAgentArchived
		case p.Archived:
			listing = ListDefaultArchived
		case len(p.SourceKinds) > 0:
			listing = ListSubAgentActive
		}
		index := 0
		if len(p.Cursor) > 0 {
			var cursor string
			_ = json.Unmarshal(p.Cursor, &cursor)
			index, _ = strconv.Atoi(cursor[len("page-"):])
		}
		data := []map[string]any{}
		var next any
		if index < len(pages[listing]) {
			for _, row := range pages[listing][index] {
				data = append(data, map[string]any{
					"id": row.id, "updatedAt": float64(row.updated), "createdAt": 1,
					"status": map[string]string{"type": row.status},
				})
			}
			if index+1 < len(pages[listing]) {
				next = "page-" + strconv.Itoa(index+1)
			}
		}
		return map[string]any{"data": data, "nextCursor": next}, nil
	})
}

func listConn(t *testing.T, host *codexcloudtest.Host) *Conn {
	t.Helper()
	conn, err := Dial(t.Context(), DialOptions{
		Endpoint: host.URL(), Login: Login{Token: NewToken("test-token")}, Version: "test",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func storedNone(string) (int64, bool, error) { return 0, false, nil }

func listCalls(host *codexcloudtest.Host) []listParams {
	var calls []listParams
	for _, r := range host.Requests() {
		if r.Method != "thread/list" {
			continue
		}
		var p listParams
		if json.Unmarshal(r.Params, &p) == nil {
			calls = append(calls, p)
		}
	}
	return calls
}

func changedIDs(threads []ListedThread) []string {
	ids := make([]string, len(threads))
	for i, thread := range threads {
		ids[i] = thread.ID
	}
	return ids
}

func TestListingParams(t *testing.T) {
	host := codexcloudtest.NewHost(t)
	servePages(host, nil)
	conn := listConn(t, host)
	for _, id := range Listings {
		_, err := listChanged(t.Context(), conn, id, 0, storedNone)
		require.NoError(t, err)
	}

	var raw []map[string]jsontext.Value
	for _, r := range host.Requests() {
		if r.Method == "thread/list" {
			var m map[string]jsontext.Value
			require.NoError(t, json.Unmarshal(r.Params, &m))
			raw = append(raw, m)
		}
	}
	require.Len(t, raw, 4)
	for i, params := range raw {
		assert.NotContains(t, params, "modelProviders", "call %d: the hosted server rejects it", i)
		assert.JSONEq(t, `"updated_at"`, string(params["sortKey"]), "call %d", i)
		assert.JSONEq(t, `"desc"`, string(params["sortDirection"]), "call %d", i)
	}
	assert.NotContains(t, raw[0], "sourceKinds")
	assert.NotContains(t, raw[1], "sourceKinds")
	assert.JSONEq(t, `false`, string(raw[0]["archived"]))
	assert.JSONEq(t, `true`, string(raw[1]["archived"]))
	want := `["subAgent","subAgentReview","subAgentCompact","subAgentThreadSpawn","subAgentOther"]`
	assert.JSONEq(t, want, string(raw[2]["sourceKinds"]))
	assert.JSONEq(t, want, string(raw[3]["sourceKinds"]))
	assert.JSONEq(t, `true`, string(raw[3]["archived"]))
}

func TestListingStopsAfterOlderPage(t *testing.T) {
	host := codexcloudtest.NewHost(t)
	servePages(host, map[ListingID][][]listingRow{ListDefaultActive: {
		{{"a", 300, "idle"}, {"b", 300, "active"}},
		{{"c", 250, "idle"}, {"d", 200, "idle"}, {"e", 190, "idle"}},
		{{"f", 100, "idle"}},
	}})
	conn := listConn(t, host)

	result, err := listChanged(t.Context(), conn, ListDefaultActive, 220, storedNone)

	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b", "c", "d", "e"}, changedIDs(result.Changed), "the page holding an older thread is finished")
	assert.Equal(t, []string{"b"}, result.Active)
	assert.Equal(t, int64(300), result.Newest)
	assert.Len(t, listCalls(host), 2, "the page after it is never requested")
}

func TestListingSharedSecondAcrossPages(t *testing.T) {
	host := codexcloudtest.NewHost(t)
	servePages(host, map[ListingID][][]listingRow{ListDefaultActive: {
		{{"a", 300, "idle"}, {"b", 250, "idle"}},
		{{"c", 250, "idle"}, {"d", 249, "idle"}},
		{{"e", 100, "idle"}},
	}})
	conn := listConn(t, host)

	result, err := listChanged(t.Context(), conn, ListDefaultActive, 250, storedNone)

	require.NoError(t, err)
	assert.Contains(t, changedIDs(result.Changed), "c", "a thread sharing the watermark's second on the next page is found")
	assert.Len(t, listCalls(host), 2)
}

func TestListingOutOfOrderIsProtocolChange(t *testing.T) {
	for name, pages := range map[string][][]listingRow{
		"within a page": {{{"a", 300, "idle"}, {"b", 310, "idle"}}},
		"across pages":  {{{"a", 300, "idle"}}, {{"b", 310, "idle"}}},
	} {
		host := codexcloudtest.NewHost(t)
		servePages(host, map[ListingID][][]listingRow{ListDefaultActive: pages})
		conn := listConn(t, host)

		_, err := listChanged(t.Context(), conn, ListDefaultActive, 0, storedNone)

		require.ErrorIs(t, err, ErrProtocolChange, name)
		assert.Equal(t, FailProtocol, FailureFor(err, Login{}), name)
	}
}

// A thread ID that cannot name a state file is the server's change, not a
// local file problem that asks the user to check the data directory.
func TestListingUnusableIDIsProtocolChange(t *testing.T) {
	for _, id := range []string{"../a", ".hidden", "a/b"} {
		host := codexcloudtest.NewHost(t)
		servePages(host, map[ListingID][][]listingRow{ListDefaultActive: {{{id, 300, "idle"}}}})
		conn := listConn(t, host)

		_, err := listChanged(t.Context(), conn, ListDefaultActive, 0, storedNone)

		require.ErrorIs(t, err, ErrProtocolChange, id)
	}
}

func TestUnchangedAccountOneCallPerListing(t *testing.T) {
	host := codexcloudtest.NewHost(t)
	page := [][]listingRow{{{"a", 300, "idle"}, {"b", 200, "idle"}}, {{"c", 100, "idle"}}}
	servePages(host, map[ListingID][][]listingRow{
		ListDefaultActive: page, ListDefaultArchived: page, ListSubAgentActive: page, ListSubAgentArchived: page,
	})
	conn := listConn(t, host)
	stored := map[string]int64{"a": 300, "b": 200, "c": 100}
	lookup := func(id string) (int64, bool, error) { v, ok := stored[id]; return v, ok, nil }

	for _, id := range Listings {
		result, err := listChanged(t.Context(), conn, id, 300, lookup)
		require.NoError(t, err)
		assert.Empty(t, result.Changed, id)
	}
	assert.Len(t, listCalls(host), 4)
}

// Each listing keeps its own watermark, so a newer archived thread never
// hides an older update in the active listing. With one shared watermark
// at the archived listing's newest, the active walk would stop after its
// first page, whose thread is older than that, and miss the update.
func TestActiveUpdateFoundWhileArchivedIsNewer(t *testing.T) {
	env := newPassEnv(t)
	activeMark, archivedMark := ago(5*time.Hour), ago(time.Hour)
	unchanged, updated := ago(2*time.Hour), ago(4*time.Hour)
	servePages(env.host, map[ListingID][][]listingRow{
		ListDefaultActive: {
			{{"unchanged", unchanged, "idle"}},
			{{"updated", updated, "idle"}},
		},
		ListDefaultArchived: {{{"archived", archivedMark, "idle"}}},
	})
	env.threads.Set(finishedThread("updated", updated, message("m", "new")))
	store := env.syncer.Store
	require.NoError(t, store.SaveThread(passKey, "unchanged", ThreadState{UpdatedAt: unchanged}))
	require.NoError(t, store.SaveThread(passKey, "archived", ThreadState{UpdatedAt: archivedMark}))
	require.NoError(t, store.SaveThread(passKey, "updated", ThreadState{UpdatedAt: activeMark}))
	backfilled := map[ListingID]bool{}
	for _, id := range Listings {
		backfilled[id] = true
	}
	require.NoError(t, store.SaveAccount(passKey, AccountState{
		Watermarks: map[ListingID]int64{ListDefaultActive: activeMark, ListDefaultArchived: archivedMark},
		Backfilled: backfilled,
	}))

	env.run(t)

	assert.Equal(t, []string{"updated"}, env.fetchedThreads(0))
	records, err := mirror.ReadAll(t.Context(), env.mirrorPath(t, "updated"))
	require.NoError(t, err)
	assert.Contains(t, recordedItemIDs(records), "m")
	watermarks := env.account(t).Watermarks
	assert.Equal(t, unchanged, watermarks[ListDefaultActive], "the active listing saves its own newest")
	assert.Equal(t, archivedMark, watermarks[ListDefaultArchived])
}

func TestSelectionUnionOnce(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	acct := &AccountState{
		Unfinished: map[string]UnfinishedEntry{"a": {InProgress: true}, "u": {InProgress: true}},
		CheckAgain: map[string]bool{"a": true, "k": true},
		Retry:      map[string]RetryEntry{"r": {NextAttempt: now.Add(-time.Minute)}},
	}
	changed := []ListedThread{{ID: "a"}, {ID: "b"}, {ID: "a"}}

	got := buildSelection(now, acct, changed, []string{"b", "x"})

	assert.Equal(t, []string{"a", "b", "x", "u", "k", "r"}, got)
}

func TestSelectionBackoff(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	acct := &AccountState{
		Unfinished: map[string]UnfinishedEntry{"w": {InProgress: true}},
		Retry: map[string]RetryEntry{
			"w":   {NextAttempt: now.Add(time.Minute)},
			"top": {NextAttempt: now.Add(time.Minute)},
			"due": {NextAttempt: now},
		},
	}

	got := buildSelection(now, acct, []ListedThread{{ID: "top"}}, []string{"w"})

	assert.Equal(t, []string{"due"}, got, "a retry not yet due waits however it was selected")
}

func TestStallOverridesActive(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	acct := &AccountState{Unfinished: map[string]UnfinishedEntry{"s": {InProgress: true, Stalled: true}}}

	assert.Empty(t, buildSelection(now, acct, nil, []string{"s"}))
	assert.Equal(t, []string{"s"}, buildSelection(now, acct, []ListedThread{{ID: "s"}}, []string{"s"}),
		"a later updatedAt change selects a stalled thread again")
}

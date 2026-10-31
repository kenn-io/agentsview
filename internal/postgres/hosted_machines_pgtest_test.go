//go:build pgtest

package postgres

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/server"
)

func hostedMachineOutcome(content string) rawderive.ParsedManifest {
	p := projectionOutcome(content)
	r := &p.Outcome.Results[0].Result
	r.Session.EndedAt = r.Session.StartedAt.Add(time.Minute)
	r.Messages[0].Timestamp = r.Session.StartedAt
	r.Messages[1].Timestamp = r.Session.EndedAt
	r.UsageEvents[0].OccurredAt = r.Session.StartedAt.Format(time.RFC3339)
	return p
}

func TestHostedSourceMachinesLabelsWithoutSessions(t *testing.T) {
	f := newProjectionFixture(t)
	f.accept(t, "device-a", "unprojected", "")
	_, err := f.runtime.ExecContext(t.Context(), `UPDATE raw_devices SET display_name='Workstation A' WHERE device_id='device-a'`)
	require.NoError(t, err)
	h, err := newHostedAdapter(f.runtime, f.tenant)
	require.NoError(t, err)
	labels, err := h.GetMachineLabels(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "Workstation A", labels["raw-device:6465766963652d61"])
	machines, err := h.GetMachines(t.Context(), false, false)
	require.NoError(t, err)
	assert.Empty(t, machines, "enrollment alone does not add a picker entry")
	matching, err := h.GetMatchingMachineIDs(t.Context(), []string{"raw-device:6465766963652d61"})
	require.NoError(t, err)
	assert.Empty(t, matching, "enrollment alone does not make an exact selector resolve")
}

func newHostedBranchExclusionFixture(t *testing.T) *HostedStore {
	t.Helper()
	f := newProjectionFixture(t)
	for _, device := range []string{"device-a", "device-b"} {
		m, _ := f.accept(t, device, "capture-"+device, "")
		require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, hostedMachineOutcome("shared needle")))
	}
	_, err := f.runtime.ExecContext(t.Context(), `INSERT INTO raw_curation(group_id,branch_id,field,value)
 SELECT DISTINCT group_id,'','excluded','true'::jsonb FROM raw_session_branches
 UNION ALL
 SELECT b.group_id,b.branch_id,'excluded','false'::jsonb FROM raw_session_branches b
 JOIN raw_source_projections p ON p.source_id=b.source_id WHERE p.device_id='device-a'`)
	require.NoError(t, err)
	h, err := newHostedAdapter(f.runtime, f.tenant)
	require.NoError(t, err)
	return h
}

func TestHostedSourceMachinesBranchExclusionOverride(t *testing.T) {
	h := newHostedBranchExclusionFixture(t)
	machines, err := h.GetMachines(t.Context(), false, false)
	require.NoError(t, err)
	assert.Equal(t, []string{"raw-device:6465766963652d61"}, machines)
	page, err := h.ListSessions(t.Context(), db.SessionFilter{IDs: []string{"codex:portable"}, Machine: "raw-device:6465766963652d61", Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	assert.Equal(t, "codex:portable", page.Sessions[0].ID)
	assert.Equal(t, []string{"raw-device:6465766963652d61"}, page.Sessions[0].SourceMachines)
	page, err = h.ListSessions(t.Context(), db.SessionFilter{Machine: "raw-device:6465766963652d62", Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, page.Sessions)

	aliases, err := h.FindSessionIDsByPartial(t.Context(), "portable", 10)
	require.NoError(t, err)
	assert.Equal(t, []string{"codex:portable"}, aliases)
	aliases, err = h.FindSessionIDsByRawSuffix(t.Context(), "portable", 10)
	require.NoError(t, err)
	assert.Equal(t, []string{"codex:portable"}, aliases)
}

func newHostedCoalescedMachinesFixture(t *testing.T) (projectionFixture, *HostedStore) {
	t.Helper()
	f := newProjectionFixture(t)
	for _, device := range []string{"device-a", "device-b"} {
		m, _ := f.accept(t, device, "capture-"+device, "")
		require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, hostedMachineOutcome("shared needle")))
	}
	other, _ := f.accept(t, "device-c", "capture-c", "")
	p := hostedMachineOutcome("unrelated needle")
	p.Outcome.Results[0].Result.Session.ID = "codex:unrelated"
	p.Outcome.Results[0].Result.Session.SourceSessionID = "unrelated"
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, other), other, p))
	_, err := f.runtime.ExecContext(t.Context(), `UPDATE raw_devices SET display_name=CASE device_id WHEN 'device-a' THEN 'Workstation A' WHEN 'device-b' THEN 'Workstation B' ELSE 'Workstation C' END`)
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent,message_count,user_message_count,started_at) VALUES('legacy-session','legacy','legacy-machine','claude',2,2,'2026-01-01T00:00:00Z')`)
	require.NoError(t, err)
	h, err := newHostedAdapter(f.runtime, f.tenant)
	require.NoError(t, err)

	return f, h
}

func TestHostedSourceMachinesFilterCoalescedSessions(t *testing.T) {
	_, h := newHostedCoalescedMachinesFixture(t)
	resolved, err := db.ResolveMachineFilter(t.Context(), h, "Workstation A")
	require.NoError(t, err)
	assert.Equal(t, "raw-device:6465766963652d61", resolved)

	for _, machine := range []string{"raw-device:6465766963652d61", "raw-device:6465766963652d62", "raw-device:6465766963652d61,raw-device:6465766963652d62"} {
		t.Run(machine, func(t *testing.T) {
			page, err := h.ListSessions(t.Context(), db.SessionFilter{Machine: machine, Limit: 10})
			require.NoError(t, err)
			require.Len(t, page.Sessions, 1)
			assert.Equal(t, "codex:portable", page.Sessions[0].ID)
			assert.ElementsMatch(t, []string{"raw-device:6465766963652d61", "raw-device:6465766963652d62"}, page.Sessions[0].SourceMachines)
			sidebar, err := h.GetSidebarSessionIndex(t.Context(), db.SessionFilter{Machine: machine, Limit: 10})
			require.NoError(t, err)
			assert.Equal(t, 1, sidebar.Total)
			require.Len(t, sidebar.Sessions, 1)
			assert.ElementsMatch(t, page.Sessions[0].SourceMachines, sidebar.Sessions[0].SourceMachines)
			matches, err := h.SearchContent(t.Context(), db.ContentSearchFilter{Machine: machine, Pattern: "needle", Mode: "substring", IncludeOneShot: true, IncludeAutomated: true, Limit: 10})
			require.NoError(t, err)
			require.Len(t, matches.Matches, 1)
			assert.Equal(t, "codex:portable", matches.Matches[0].SessionID)
			assert.ElementsMatch(t, page.Sessions[0].SourceMachines, matches.Matches[0].SourceMachines)
			summary, err := h.GetAnalyticsSummary(t.Context(), db.AnalyticsFilter{Machine: machine, From: "2026-01-01", To: "2026-01-01", Timezone: "UTC"})
			require.NoError(t, err)
			assert.Equal(t, 1, summary.TotalSessions)
			assert.Equal(t, 2, summary.TotalMessages)
			usage, err := h.GetDailyUsage(t.Context(), db.UsageFilter{Machine: machine, From: "2026-01-01", To: "2026-01-01", Timezone: "UTC"})
			require.NoError(t, err)
			assert.Equal(t, 11, usage.Totals.InputTokens)
			assert.Equal(t, 7, usage.Totals.OutputTokens)
		})
	}
	legacy, err := h.ListSessions(t.Context(), db.SessionFilter{Machine: "legacy-machine", Limit: 10})
	require.NoError(t, err)
	require.Len(t, legacy.Sessions, 1)
	assert.Equal(t, "legacy-session", legacy.Sessions[0].ID)
	assert.Empty(t, legacy.Sessions[0].SourceMachines)
	resolved, err = db.ResolveMachineFilter(t.Context(), h, "hosted")
	require.NoError(t, err)
	assert.Equal(t, "hosted", resolved)
	compatibility, err := h.ListSessions(t.Context(), db.SessionFilter{Machine: resolved, Limit: 10})
	require.NoError(t, err)
	assert.Len(t, compatibility.Sessions, 2, "old hosted selectors still match raw sessions")

	cfg, err := config.Default()
	require.NoError(t, err)
	cfg.DataDir = t.TempDir()
	app := server.New(cfg, h, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions?machine=Workstation%20A&include_one_shot=true", nil)
	req.Host = "localhost:8080"
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, req)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var httpPage db.SessionPage
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &httpPage))
	require.Len(t, httpPage.Sessions, 1)
	assert.Equal(t, "codex:portable", httpPage.Sessions[0].ID)
}

func TestHostedSourceMachinesStats(t *testing.T) {
	_, h := newHostedCoalescedMachinesFixture(t)
	stats, err := h.GetStats(t.Context(), false, false)
	require.NoError(t, err)
	assert.Equal(t, 4, stats.MachineCount)
}

func TestHostedSourceMachinesCatalogFlags(t *testing.T) {
	f, h := newHostedCoalescedMachinesFixture(t)
	// Catalog flags must retain automated one-shot sources when automation is
	// included.
	_, err := f.runtime.ExecContext(t.Context(), `UPDATE sessions SET is_automated=true WHERE id IN (SELECT session_id FROM raw_session_branches b JOIN raw_source_projections p USING(source_id) WHERE p.device_id='device-c')`)
	require.NoError(t, err)
	for _, tc := range []struct {
		excludeOneShot   bool
		excludeAutomated bool
		want             []string
	}{
		{true, true, []string{"legacy-machine"}},
		{true, false, []string{"legacy-machine", "raw-device:6465766963652d63"}},
		{false, true, []string{"legacy-machine", "raw-device:6465766963652d61", "raw-device:6465766963652d62"}},
		{false, false, []string{"legacy-machine", "raw-device:6465766963652d61", "raw-device:6465766963652d62", "raw-device:6465766963652d63"}},
	} {
		machines, err := h.GetMachines(t.Context(), tc.excludeOneShot, tc.excludeAutomated)
		require.NoError(t, err)
		assert.ElementsMatch(t, tc.want, machines, "excludeOneShot=%t excludeAutomated=%t", tc.excludeOneShot, tc.excludeAutomated)
	}
}

func TestHostedSourceMachinesContributorInvalidatesCursor(t *testing.T) {
	f, h := newHostedCoalescedMachinesFixture(t)

	page, err := h.ListSessions(t.Context(), db.SessionFilter{Limit: 1})
	require.NoError(t, err)
	require.NotEmpty(t, page.NextCursor)
	added, _ := f.accept(t, "device-d", "capture-d", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, added), added, hostedMachineOutcome("shared needle")))
	_, err = h.ListSessions(t.Context(), db.SessionFilter{Limit: 1, Cursor: page.NextCursor})
	assert.ErrorIs(t, err, ErrHostedCursor, "a new contributor invalidates cursors even when transcript content coalesces")
}

func TestHostedSourceMachinesKeepDivergentVariantsSeparate(t *testing.T) {
	f := newProjectionFixture(t)
	for _, device := range []string{"device-a", "device-b"} {
		m, _ := f.accept(t, device, "capture-"+device, "")
		require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, hostedMachineOutcome(device+" needle")))
	}
	h, err := newHostedAdapter(f.runtime, f.tenant)
	require.NoError(t, err)
	var variants []string
	for _, device := range []string{"device-a", "device-b"} {
		t.Run(device, func(t *testing.T) {
			machine := "raw-device:" + hex.EncodeToString([]byte(device))
			page, err := h.ListSessions(t.Context(), db.SessionFilter{Machine: machine, Limit: 10})
			require.NoError(t, err)
			require.Len(t, page.Sessions, 1)
			assert.Equal(t, []string{machine}, page.Sessions[0].SourceMachines)
			variants = append(variants, page.Sessions[0].ID)
		})
	}
	require.Len(t, variants, 2)
	assert.NotEqual(t, variants[0], variants[1])
	page, err := h.ListSessions(t.Context(), db.SessionFilter{Machine: "raw-device:6465766963652d61,raw-device:6465766963652d62", Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 2)
}

func TestHostedSourceMachinesIncludePrefixCopies(t *testing.T) {
	f := newProjectionFixture(t)
	a, _ := f.accept(t, "device-a", "short-a", "")
	short := hostedMachineOutcome("shared prefix")
	short.Outcome.Results[0].Result.Messages = short.Outcome.Results[0].Result.Messages[:1]
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, short))
	b, accepted := f.accept(t, "device-b", "long-b", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, b), b, hostedMachineOutcome("shared prefix")))
	h, err := newHostedAdapter(f.runtime, f.tenant)
	require.NoError(t, err)
	inventory, err := h.GetProjectInventory(t.Context(), db.ProjectDateFilter{})
	require.NoError(t, err)
	require.Len(t, inventory.Projects, 1)
	assert.Equal(t, 1, inventory.Projects[0].Sessions)
	assert.Equal(t, 2, inventory.Projects[0].Machines, "project counts include all sources without duplicating sessions")
	page, err := h.ListSessions(t.Context(), db.SessionFilter{Machine: "raw-device:6465766963652d61", Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	assert.ElementsMatch(t, []string{"raw-device:6465766963652d61", "raw-device:6465766963652d62"}, page.Sessions[0].SourceMachines)
	messages, err := h.GetAllMessages(t.Context(), page.Sessions[0].ID)
	require.NoError(t, err)
	assert.Len(t, messages, 2, "a source-presence filter retains the merged transcript")

	b, _ = f.accept(t, "device-b", "removed-b", accepted.Receipt)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, b), b, rawderive.ParsedManifest{Outcome: parser.ParseOutcome{ResultSetComplete: true, ForceReplace: true}}))
	page, err = h.ListSessions(t.Context(), db.SessionFilter{Machine: "raw-device:6465766963652d61", Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	assert.Equal(t, []string{"raw-device:6465766963652d61"}, page.Sessions[0].SourceMachines)
	messages, err = h.GetAllMessages(t.Context(), page.Sessions[0].ID)
	require.NoError(t, err)
	assert.Len(t, messages, 1, "retracted longer proof cannot remain attributed to the prefix source")
}

func TestHostedSourceMachinesFollowRetainedProof(t *testing.T) {
	f := newProjectionFixture(t)
	a, acceptedA := f.accept(t, "device-a", "capture-a", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, hostedMachineOutcome("retained needle")))
	b, acceptedB := f.accept(t, "device-b", "capture-b", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, b), b, hostedMachineOutcome("retained needle")))
	h, err := newHostedAdapter(f.runtime, f.tenant)
	require.NoError(t, err)
	f.tombstone(t, a, "departed-a", acceptedA.Receipt)
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE raw_devices SET revoked_at=clock_timestamp() WHERE device_id='device-a'`)
	require.NoError(t, err)
	page, err := h.ListSessions(t.Context(), db.SessionFilter{Machine: "raw-device:6465766963652d61", Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1, "departure and revocation retain source history")

	b, _ = f.accept(t, "device-b", "without-session-b", acceptedB.Receipt)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, b), b, rawderive.ParsedManifest{Outcome: parser.ParseOutcome{ResultSetComplete: true, ForceReplace: true}}))
	page, err = h.ListSessions(t.Context(), db.SessionFilter{Machine: "raw-device:6465766963652d62", Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, page.Sessions, "a complete snapshot retracts only its own proof")
	machines, err := h.GetMachines(t.Context(), false, false)
	require.NoError(t, err)
	assert.Equal(t, []string{"raw-device:6465766963652d61"}, machines)

	require.NoError(t, h.SoftDeleteSession(t.Context(), "codex:portable"))
	machines, err = h.GetMachines(t.Context(), false, false)
	require.NoError(t, err)
	assert.Empty(t, machines)
	matching, err := h.GetMatchingMachineIDs(t.Context(), []string{"raw-device:6465766963652d61"})
	require.NoError(t, err)
	assert.Equal(t, []string{"raw-device:6465766963652d61"}, matching, "exact ID resolution includes retained trash")
	deleted, err := h.DeleteSessionIfTrashed(t.Context(), "codex:portable")
	require.NoError(t, err)
	assert.EqualValues(t, 1, deleted)
	page, err = h.ListSessions(t.Context(), db.SessionFilter{Machine: "raw-device:6465766963652d61", Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, page.Sessions, "excluded branches no longer contribute a visible machine")
}

func TestHostedSourceMachinesOpaqueDeviceSelector(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device,研发", "capture-opaque", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, hostedMachineOutcome("opaque device needle")))
	h, err := newHostedAdapter(f.runtime, f.tenant)
	require.NoError(t, err)
	machines, err := h.GetMachines(t.Context(), false, false)
	require.NoError(t, err)
	require.Equal(t, []string{"raw-device:6465766963652ce7a094e58f91"}, machines)
	page, err := h.ListSessions(t.Context(), db.SessionFilter{Machine: machines[0], Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	assert.Equal(t, "codex:portable", page.Sessions[0].ID)
	assert.Equal(t, machines, page.Sessions[0].SourceMachines)
}

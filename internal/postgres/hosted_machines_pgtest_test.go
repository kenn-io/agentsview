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

func TestHostedSourceMachinesFilterCoalescedSessions(t *testing.T) {
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

	machines, err := h.GetMachines(t.Context(), false, false)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"raw-device:6465766963652d61", "raw-device:6465766963652d62", "raw-device:6465766963652d63", "legacy-machine"}, machines)
	machines, err = h.GetMachines(t.Context(), true, true)
	require.NoError(t, err)
	assert.Equal(t, []string{"legacy-machine"}, machines, "catalog visibility honors the ordinary one-shot exclusion")
	labels, err := h.GetMachineLabels(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "Workstation A", labels["raw-device:6465766963652d61"])
	assert.Equal(t, "Workstation B", labels["raw-device:6465766963652d62"])
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
			summary, err := h.GetAnalyticsSummary(t.Context(), db.AnalyticsFilter{Machine: machine, From: "2026-01-01", To: "2026-01-01"})
			require.NoError(t, err)
			assert.Equal(t, 1, summary.TotalSessions)
			assert.Equal(t, 2, summary.TotalMessages)
			usage, err := h.GetDailyUsage(t.Context(), db.UsageFilter{Machine: machine, From: "2026-01-01", To: "2026-01-01"})
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
	stats, err := h.GetStats(t.Context(), false, false)
	require.NoError(t, err)
	assert.Equal(t, 4, stats.MachineCount)
	resolved, err = db.ResolveMachineFilter(t.Context(), h, "hosted")
	require.NoError(t, err)
	assert.Equal(t, "hosted", resolved)
	compatibility, err := h.ListSessions(t.Context(), db.SessionFilter{Machine: resolved, Limit: 10})
	require.NoError(t, err)
	assert.Len(t, compatibility.Sessions, 2, "old hosted selectors still match raw sessions")

	// Catalog flags must retain automated one-shot sources when automation is
	// included, while exact selectors still resolve hidden picker entries.
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE sessions SET is_automated=true WHERE id IN (SELECT session_id FROM raw_session_branches b JOIN raw_source_projections p USING(source_id) WHERE p.device_id='device-c')`)
	require.NoError(t, err)
	cfg, err := config.Default()
	require.NoError(t, err)
	cfg.DataDir = t.TempDir()
	app := server.New(cfg, h, nil)
	request := func(path string) []byte {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = "localhost:8080"
		response := httptest.NewRecorder()
		app.Handler().ServeHTTP(response, req)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		return response.Body.Bytes()
	}
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"", []string{"legacy-machine"}},
		{"&include_automated=true", []string{"legacy-machine", "raw-device:6465766963652d63"}},
		{"&include_one_shot=true", []string{"legacy-machine", "raw-device:6465766963652d61", "raw-device:6465766963652d62"}},
		{"&include_one_shot=true&include_automated=true", []string{"legacy-machine", "raw-device:6465766963652d61", "raw-device:6465766963652d62", "raw-device:6465766963652d63"}},
	} {
		var metadata struct {
			Machines         []string          `json:"machines"`
			ResolvedMachines []string          `json:"resolved_machines"`
			MachineLabels    map[string]string `json:"machine_labels"`
		}
		require.NoError(t, json.Unmarshal(request("/api/v1/machines?machine=Workstation%20A"+tc.query), &metadata))
		assert.ElementsMatch(t, tc.want, metadata.Machines, tc.query)
		assert.Equal(t, []string{"raw-device:6465766963652d61"}, metadata.ResolvedMachines)
		assert.Equal(t, "Workstation A", metadata.MachineLabels["raw-device:6465766963652d61"])
	}
	var httpPage db.SessionPage
	require.NoError(t, json.Unmarshal(request("/api/v1/sessions?machine=Workstation%20A&include_one_shot=true"), &httpPage))
	require.Len(t, httpPage.Sessions, 1)
	assert.ElementsMatch(t, []string{"raw-device:6465766963652d61", "raw-device:6465766963652d62"}, httpPage.Sessions[0].SourceMachines)

	_, err = f.runtime.ExecContext(t.Context(), `UPDATE raw_devices SET display_name='Shared label' WHERE device_id IN ('device-a','device-b')`)
	require.NoError(t, err)
	_, err = db.ResolveMachineFilter(t.Context(), h, "Shared label")
	var ambiguous *db.MachineFilterError
	require.ErrorAs(t, err, &ambiguous)
	assert.ElementsMatch(t, []string{"raw-device:6465766963652d61", "raw-device:6465766963652d62"}, ambiguous.Candidates)
	resolved, err = db.ResolveMachineFilter(t.Context(), h, "raw-device:6465766963652d61")
	require.NoError(t, err)
	assert.Equal(t, "raw-device:6465766963652d61", resolved)

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
			messages, err := h.GetAllMessages(t.Context(), page.Sessions[0].ID)
			require.NoError(t, err)
			require.NotEmpty(t, messages)
			assert.Equal(t, device+" needle", messages[0].Content)
			matches, err := h.SearchContent(t.Context(), db.ContentSearchFilter{Machine: machine, Pattern: "needle", Mode: "substring", IncludeOneShot: true, IncludeAutomated: true, Limit: 10})
			require.NoError(t, err)
			require.Len(t, matches.Matches, 1)
			assert.Equal(t, page.Sessions[0].ID, matches.Matches[0].SessionID)
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
	resolved, err := db.ResolveMachineFilter(t.Context(), h, machines[0])
	require.NoError(t, err)
	assert.Equal(t, machines[0], resolved)
	cfg, err := config.Default()
	require.NoError(t, err)
	cfg.DataDir = t.TempDir()
	app := server.New(cfg, h, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions?include_one_shot=true&machine="+machines[0], nil)
	req.Host = "localhost:8080"
	w := httptest.NewRecorder()
	app.Handler().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var page db.SessionPage
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &page))
	require.Len(t, page.Sessions, 1)
	assert.Equal(t, "codex:portable", page.Sessions[0].ID)
	assert.Equal(t, machines, page.Sessions[0].SourceMachines)
}

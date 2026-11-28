package db

import (
	"database/sql"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClearHostSources(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	path := t.TempDir()
	require.NoError(t, d.SetSyncState(ctx, MachineLabelKeyPrefix+"machine-a", "Laptop"))
	require.NoError(t, d.SetSyncState(ctx, MachineAliasKeyPrefix+"old-machine", "machine-a"))
	require.NoError(t, d.SetSyncState(ctx, "machine_origin:machine-a", "origin"))
	require.NoError(t, d.SetSyncState(ctx, "replica-position", "123"))
	require.NoError(t, d.Update(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO sessions(id, project, cwd, file_path, machine) VALUES('a', ?, ?, ?, 'host-a.example')`, path, path, path); err != nil {
			return err
		}
		for _, query := range []string{
			`INSERT INTO raw_archive_roots VALUES('root','device','machine','claude','/original/root','configured')`,
			`INSERT INTO raw_archive_files(root_id,path,sha256,size,mod_time_ns) VALUES('root','file','hash',1,1)`,
			`INSERT INTO raw_archive_sources(manifest_id,root_id,source_key,original_path,canonical_json) VALUES('manifest','root','source','/original/file',x'7b7d')`,
			`INSERT INTO raw_archive_devices VALUES('device',x'7b7d')`,
			`INSERT INTO raw_archive_root_aliases VALUES('root','/original/alias')`,
			`INSERT INTO raw_archive_sessions VALUES('device','claude','native','a','root','source')`,
			`INSERT INTO archive_attachments(repository_id,snapshot_id,archive_id) VALUES('repo','snapshot','archive')`,
			`INSERT INTO archive_source_bindings VALUES('archive','device','root','source','manifest')`,
			`INSERT INTO archive_attachment_sources(repository_id,snapshot_id,archive_id,device_id,root_id,source_key,target) VALUES('repo','snapshot','archive','device','root','source','target')`,
			`INSERT INTO archive_session_provenance VALUES('a','archive','device','root','source','manifest','native')`,
			`INSERT INTO commit_links VALUES('a','sha','','','/work/repo',0,0,'','','')`,
			`INSERT INTO commit_trailers(session_id,sha,branch,subject,repository,committed_at) VALUES('a','sha','','','/work/repo','')`,
			`INSERT INTO git_cache VALUES('cache', 'log', '{"repo":"/host/repo"}', '2026-01-01')`,
			`INSERT INTO friction_issue_links VALUES('fp', 'instance', 'project', 'issue', 'id', 'https://example.com', 'local', '2026-01-01')`,
			`INSERT INTO friction_filing_state(fingerprint,state,last_error,updated_at) VALUES('other', 'failed', '/host/repo', '2026-01-01')`,
			`INSERT INTO cursor_account_polls VALUES('account-id', 1, '2026-01-01')`,
			`INSERT INTO artifact_imported_sessions(origin,gid,manifest_hash,imported_session_id) VALUES('peer', 'gid', 'hash', 'a')`,
		} {
			if _, err := tx.ExecContext(ctx, query); err != nil {
				return err
			}
		}
		return nil
	}))
	_, err := d.AppendDiagnostic(ctx, DiagnosticEvent{ID: "00000000-0000-4000-8000-000000000001", Source: "test", Message: "failed reading /work/private/session.jsonl"})
	require.NoError(t, err)
	require.NoError(t, d.ClearHostSources(ctx))
	for _, table := range []string{"diagnostic_events", "git_cache", "friction_issue_links", "friction_filing_state", "cursor_account_polls", "artifact_imported_sessions", "raw_archive_roots", "raw_archive_files", "raw_archive_sources", "raw_archive_devices", "raw_archive_root_aliases", "raw_archive_sessions", "archive_attachments", "archive_attachment_sources", "archive_source_bindings", "archive_session_provenance"} {
		var count int
		require.NoError(t, d.Reader().QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count))
		assert.Zero(t, count, table)
	}

	for _, table := range []string{"commit_links", "commit_trailers"} {
		var repository string
		require.NoError(t, d.Reader().QueryRow(ctx, "SELECT repository FROM "+table+" WHERE session_id = ?", "a").Scan(&repository))
		assert.Empty(t, repository, table)
	}

	var cwd, machine string
	var filePath sql.NullString
	require.NoError(t, d.Reader().QueryRow(ctx, `SELECT cwd, file_path, machine FROM sessions WHERE id='a'`).Scan(&cwd, &filePath, &machine))
	assert.Equal(t, "host-a.example", machine)
	assert.Empty(t, cwd)
	assert.False(t, filePath.Valid)
	labels, err := d.GetMachineLabels(ctx)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"machine-a": "Laptop"}, labels)
	aliases, err := d.GetMachineAliases(ctx)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"old-machine": "machine-a"}, aliases)
	origin, err := d.GetSyncState(ctx, "machine_origin:machine-a")
	require.NoError(t, err)
	assert.Equal(t, "origin", origin)
	position, err := d.GetSyncState(ctx, "replica-position")
	require.NoError(t, err)
	assert.Empty(t, position)
}

func TestClearHostSourcesPreservesCursorUsage(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	require.NoError(t, d.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO cursor_usage_events(occurred_at,model,input_tokens,output_tokens,charged_microdollars,user_id,user_email,account_user_id,account_polled_at,conversation_id,dedup_key) VALUES('2026-08-10T09:00:00Z','model',330,100,42,'user','user@example.com','account','2026-08-10T09:01:00Z','conversation','event-1')`)
		return err
	}))
	filter := UsageFilter{Timezone: "UTC"}
	require.NoError(t, d.ClearHostSources(ctx))
	var user, email, account, polled, conversation string
	require.NoError(t, d.Reader().QueryRow(ctx, `SELECT user_id,user_email,account_user_id,account_polled_at,conversation_id FROM cursor_usage_events`).Scan(&user, &email, &account, &polled, &conversation))
	assert.Empty(t, user)
	assert.Empty(t, email)
	assert.Empty(t, account)
	assert.Empty(t, polled)
	assert.Empty(t, conversation)
	path := d.Path()
	require.NoError(t, d.Close())
	reopened, err := OpenIsolatedContext(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	after, err := reopened.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	assert.Equal(t, 330, after.Totals.InputTokens)
	assert.Equal(t, 100, after.Totals.OutputTokens)
	assert.EqualValues(t, 42, after.Totals.TotalCost.Microdollars)
}

func TestCopiedProjectLabelsAreRecognizableAndDistinct(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	for i, project := range []string{"/first/api", "/second/api", `C:\work\api`, "/work/solo", "api", CopiedProjectLabel("/first/api")} {
		require.NoError(t, d.UpsertSession(ctx, Session{ID: strconv.Itoa(i), Agent: "claude", Project: project}))
	}
	require.NoError(t, d.ClearHostSources(ctx))
	rows, err := d.Reader().Query(ctx, `SELECT project FROM sessions ORDER BY id`)
	require.NoError(t, err)
	defer rows.Close()
	var projects []string
	for rows.Next() {
		var project string
		require.NoError(t, rows.Scan(&project))
		projects = append(projects, project)
	}
	require.NoError(t, rows.Err())
	require.Len(t, projects, 6)
	assert.Len(t, map[string]bool{projects[0]: true, projects[1]: true, projects[2]: true, projects[3]: true, projects[4]: true, projects[5]: true}, 6)
	for _, project := range projects[:3] {
		assert.Contains(t, project, "api")
		assert.NotContains(t, project, "/")
		assert.NotContains(t, project, `\`)
	}
	assert.Regexp(t, `^solo-[0-9a-f]{8}$`, projects[3])
	assert.Equal(t, "api", projects[4])
}

func TestClearHostSourcesPreservesSessionProjectAssignments(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	for _, id := range []string{"first", "second"} {
		insertSession(t, d, id, "/work/original")
		_, err := d.AssignSessionProject(ctx, id, "/work/assigned")
		require.NoError(t, err)
	}
	require.NoError(t, d.ClearHostSources(ctx))
	for _, id := range []string{"first", "second"} {
		session, err := d.GetSession(ctx, id)
		require.NoError(t, err)
		require.True(t, session.ProjectAssigned)
		assert.Equal(t, CopiedProjectLabel("/work/assigned"), session.Project)
		var assigned, original string
		require.NoError(t, d.Reader().QueryRow(ctx, `SELECT project, original_project FROM session_project_assignments WHERE session_id=?`, id).Scan(&assigned, &original))
		assert.Equal(t, CopiedProjectLabel("/work/assigned"), assigned)
		assert.Equal(t, CopiedProjectLabel("/work/original"), original)
	}
	cleared, err := d.ClearSessionProjectAssignment(ctx, "first")
	require.NoError(t, err)
	assert.Equal(t, CopiedProjectLabel("/work/original"), cleared.Project)
	other, err := d.GetSession(ctx, "second")
	require.NoError(t, err)
	require.True(t, other.ProjectAssigned)
	assert.Equal(t, CopiedProjectLabel("/work/assigned"), other.Project)
}

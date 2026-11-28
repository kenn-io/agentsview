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
			`INSERT INTO recall_entries(id,type,scope,title,body,project,cwd,source_session_id) VALUES('r','fact','project','title','body','/work/project','/work/project','a')`,
			`INSERT INTO insights(type,date_from,date_to,project,agent,content) VALUES('type','start','end','/work/project','claude','content')`,
			`INSERT INTO session_deletion_changes(session_id,project,revision) VALUES('deleted','/work/project',1)`,
			`INSERT INTO excluded_sessions(id,file_path) VALUES('excluded','/work/file')`,
			`INSERT INTO provider_freshness(agent,file_path,stat_hash) VALUES('claude','/work/file',1)`,
			`INSERT INTO skipped_files VALUES('/work/file',1)`,
			`INSERT INTO source_failures(cache_key,file_mtime) VALUES('/work/file',1)`,
			`INSERT INTO local_session_source_baselines VALUES('a','machine','claude','/work/file')`,
			`INSERT INTO claude_subagent_sources VALUES('a','/work/file')`,
			`INSERT INTO remote_skipped_files VALUES('host','/work/file',1)`,
			`INSERT INTO parser_checkpoints(session_id,agent,file_path,file_inode,file_device,file_mtime,offset,tail_anchor_digest,hash,next_ordinal,checkpoint_version,updated_at) VALUES('a','claude','/work/file',1,1,1,1,'hash','hash',1,1,'now')`,
			`INSERT INTO parser_checkpoint_blobs(session_id,cursor) VALUES('a',x'00')`,
			`INSERT INTO artifact_export_queue(session_id) VALUES('a')`,
			`INSERT INTO artifact_publications VALUES('origin','a','hash','fingerprint')`,
			`INSERT INTO artifact_publication_revisions VALUES('origin',1)`,
			`INSERT INTO artifact_checkpoint_heads VALUES('origin',1,1,'map','hash',1)`,
			`INSERT INTO artifact_checkpoint_floors VALUES('origin',1)`,
			`INSERT INTO artifact_import_queue(origin,kind,name,sha256,size,required_checkpoint_version,required_manifest_version,required_segment_version) VALUES('origin','kind','name','hash',1,1,1,1)`,
			`INSERT INTO artifact_import_attempt_generations VALUES(1,1)`,
			`INSERT INTO artifact_peer_checkpoint_heads VALUES('origin',1,'hash',1)`,
			`INSERT INTO artifact_checkpoint_landings VALUES('origin',1,'hash',1)`,
			`INSERT INTO artifact_checkpoint_landing_sessions VALUES('origin','gid','hash')`,
			`INSERT INTO artifact_checkpoint_stages(origin,sequence,checkpoint_sha256,checkpoint_size) VALUES('origin',1,'hash',1)`,
			`INSERT INTO artifact_checkpoint_stage_sessions(origin,sequence,gid,manifest_hash) VALUES('origin',1,'gid','hash')`,
			`INSERT INTO worktree_project_mappings(machine,path_prefix,project) VALUES('machine','/work','project')`,
			`INSERT INTO project_identity_observations(project,machine,observed_at) VALUES('project','machine','now')`,
			`INSERT INTO session_project_identity_snapshots(session_id,project,machine,observed_at) VALUES('a','project','machine','now')`,
			`INSERT INTO repository_project_names VALUES('remote','project','reason')`,
			`INSERT INTO commit_links VALUES('a','sha','','','/work/repo',0,0,'','','/work/project')`,
			`INSERT INTO commit_trailers(session_id,sha,branch,subject,repository,committed_at,project) VALUES('a','sha','','','/work/repo','','/work/project')`,
			`INSERT INTO git_cache VALUES('cache', 'log', '{"repo":"/host/repo"}', '2026-01-01')`,
			`INSERT INTO friction_issue_links VALUES('fp', 'instance', 'project', 'issue', 'id', 'https://example.com', 'local', '2026-01-01')`,
			`INSERT INTO friction_filing_state(fingerprint,state,last_error,updated_at) VALUES('other', 'failed', '/host/repo', '2026-01-01')`,
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
	for _, table := range []string{"provider_freshness", "skipped_files", "source_failures", "local_session_source_baselines", "claude_subagent_sources", "remote_skipped_files", "parser_checkpoint_blobs", "parser_checkpoints", "artifact_export_queue", "artifact_publications", "artifact_publication_revisions", "artifact_checkpoint_heads", "artifact_checkpoint_floors", "artifact_import_queue", "artifact_import_attempt_generations", "artifact_peer_checkpoint_heads", "artifact_checkpoint_landing_sessions", "artifact_checkpoint_landings", "artifact_checkpoint_stage_sessions", "artifact_checkpoint_stages", "worktree_project_mappings", "project_identity_observations", "session_project_identity_snapshots", "repository_project_names", "worktree_project_mapping_changes", "project_identity_observation_changes", "session_project_identity_snapshot_changes", "diagnostic_events", "git_cache", "friction_issue_links", "friction_filing_state", "artifact_imported_sessions", "raw_archive_roots", "raw_archive_files", "raw_archive_sources", "raw_archive_devices", "raw_archive_root_aliases", "raw_archive_sessions", "archive_attachments", "archive_attachment_sources", "archive_source_bindings", "archive_session_provenance"} {
		var count int
		require.NoError(t, d.Reader().QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count))
		assert.Zero(t, count, table)
	}

	for _, table := range []string{"sessions", "recall_entries", "insights", "session_deletion_changes", "commit_links", "commit_trailers"} {
		var project string
		require.NoError(t, d.Reader().QueryRow(ctx, "SELECT project FROM "+table+" LIMIT 1").Scan(&project))
		expected := "/work/project"
		if table == "sessions" {
			expected = path
		}
		assert.Equal(t, CopiedProjectLabel(expected), project, table)
	}
	var recallCwd string
	require.NoError(t, d.Reader().QueryRow(ctx, `SELECT cwd FROM recall_entries WHERE id='r'`).Scan(&recallCwd))
	assert.Empty(t, recallCwd)
	var excludedPath sql.NullString
	require.NoError(t, d.Reader().QueryRow(ctx, `SELECT file_path FROM excluded_sessions WHERE id='excluded'`).Scan(&excludedPath))
	assert.False(t, excludedPath.Valid)

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
	insertSession(t, d, "first", "/work/original")
	_, err := d.AssignSessionProject(ctx, "first", "/work/assigned")
	require.NoError(t, err)
	require.NoError(t, d.ClearHostSources(ctx))
	session, err := d.GetSession(ctx, "first")
	require.NoError(t, err)
	require.True(t, session.ProjectAssigned)
	assert.Equal(t, CopiedProjectLabel("/work/assigned"), session.Project)
	var assigned, original string
	require.NoError(t, d.Reader().QueryRow(ctx, `SELECT project, original_project FROM session_project_assignments WHERE session_id=?`, "first").Scan(&assigned, &original))
	assert.Equal(t, CopiedProjectLabel("/work/assigned"), assigned)
	assert.Equal(t, CopiedProjectLabel("/work/original"), original)
}

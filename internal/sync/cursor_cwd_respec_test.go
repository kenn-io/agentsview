package sync

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/parser"
)

func TestCursorSourceCwdDecisionUsesGenericStates(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	e := NewEngine(d, EngineConfig{})
	t.Cleanup(func() { e.Close() })
	path := "cursor-project/agent-transcripts/session.jsonl"
	require.NoError(t, d.UpsertSession(db.Session{
		ID: "cursor:session", Agent: string(parser.AgentCursor),
		FilePath: &path, Cwd: "/work/a",
	}))

	tests := []struct {
		name       string
		provider   parser.AgentType
		resolution parser.SourceCwdResolution
		force      bool
		stored     bool
	}{
		{
			name:     "resolved mismatch",
			provider: parser.AgentCursor,
			resolution: parser.SourceCwdResolution{
				State: parser.SourceCwdResolved, Path: "/work/b",
			},
			force:  true,
			stored: true,
		},
		{
			name:     "unavailable preserves",
			provider: parser.AgentCursor,
			resolution: parser.SourceCwdResolution{
				State: parser.SourceCwdUnavailable,
			},
			stored: true,
		},
		{
			name:     "remote clears local identity",
			provider: parser.AgentCursor,
			resolution: parser.SourceCwdResolution{
				State: parser.SourceCwdRemote,
			},
			force:  true,
			stored: true,
		},
		{
			name:     "non cursor unspecified",
			provider: parser.AgentClaude,
			resolution: parser.SourceCwdResolution{
				State: parser.SourceCwdUnspecified,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decision := e.sourceCwdDecision(parser.SourceRef{
				Provider:       tt.provider,
				FingerprintKey: path,
				CwdResolution:  tt.resolution,
			})
			assert.Equal(t, tt.force, decision.forceParse)
			assert.Equal(t, tt.stored, decision.storedCwd == "/work/a")
			assert.Equal(t, tt.stored, decision.storedOK)
		})
	}

	decision := e.sourceCwdDecision(parser.SourceRef{
		Provider:       parser.AgentCursor,
		FingerprintKey: path,
		CwdResolution: parser.SourceCwdResolution{
			State: parser.SourceCwdUnavailable,
		},
	})
	written, _, failed, _ := e.writeBatch([]pendingWrite{{
		sess: parser.ParsedSession{
			ID: "cursor:session", Agent: parser.AgentCursor,
			Cwd: "", File: parser.FileInfo{Path: path, Mtime: 2},
		},
		sourceCwdResolution: decision.resolution,
		sourceCwdStored:     decision.storedCwd,
		sourceCwdStoredOK:   decision.storedOK,
	}}, syncWriteDefault, false)
	assert.Equal(t, 1, written)
	assert.Zero(t, failed)
	stored, err := d.GetSession(context.Background(), "cursor:session")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, "/work/a", stored.Cwd)

	parsedPath := "cursor-project/agent-transcripts/parsed.jsonl"
	written, _, failed, _ = e.writeBatch([]pendingWrite{{
		sess: parser.ParsedSession{
			ID: "cursor:parsed", Agent: parser.AgentCursor, Cwd: "/parsed",
			File: parser.FileInfo{Path: parsedPath, Mtime: 3},
		},
		sourceCwdResolution: parser.SourceCwdResolution{
			State: parser.SourceCwdUnavailable,
		},
	}}, syncWriteDefault, false)
	assert.Equal(t, 1, written)
	assert.Zero(t, failed)
	parsed, err := d.GetSession(context.Background(), "cursor:parsed")
	require.NoError(t, err)
	require.NotNil(t, parsed)
	assert.Equal(t, "/parsed", parsed.Cwd)
}

func TestUnavailableCursorCwdPreservesArchivedProjectAttribution(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	e := NewEngine(d, EngineConfig{Machine: "local"})
	t.Cleanup(func() { e.Close() })
	ctx := context.Background()
	workspaceRoot := t.TempDir()
	workspace := filepath.Join(workspaceRoot, "work", "a")
	path := "cursor-project/agent-transcripts/session.jsonl"
	const sessionID = "cursor:archived-project"
	require.NoError(t, d.UpsertSession(db.Session{
		ID: sessionID, Agent: string(parser.AgentCursor), Machine: "local",
		Project: "mapped_project", Cwd: workspace, FilePath: &path,
	}))
	require.NoError(t, d.UpsertProjectIdentityObservationWithSnapshotProject(
		ctx, export.ProjectIdentityObservation{
			SessionID: sessionID, Project: "mapped_project", Machine: "local",
			RootPath: workspaceRoot, GitRemote: "https://example.com/project.git",
			RemoteResolution: export.ProjectResolutionResolved,
		}, "mapped_project",
	))
	snapshots, err := d.ListSessionProjectIdentitySnapshotsByID(
		ctx, []string{sessionID},
	)
	require.NoError(t, err)
	require.Contains(t, snapshots, sessionID)

	written, _, failed, _ := e.writeBatch([]pendingWrite{{
		sess: parser.ParsedSession{
			ID: sessionID, Agent: parser.AgentCursor, Project: "decoded_hint",
			File: parser.FileInfo{Path: path, Mtime: 2},
		},
		sourceCwdResolution: parser.SourceCwdResolution{
			State: parser.SourceCwdUnavailable,
		},
		sourceCwdStored:   workspace,
		sourceCwdStoredOK: true,
	}}, syncWriteDefault, false)
	require.Equal(t, 1, written)
	require.Zero(t, failed)

	stored, err := d.GetSession(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, workspace, stored.Cwd)
	assert.Equal(t, "mapped_project", stored.Project)
}

func TestFilteredCursorCwdReconciliationScopesSourceIdentity(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	workspaceRoot := t.TempDir()
	allowed := filepath.Join(workspaceRoot, "allowed")
	actual := filepath.Join(workspaceRoot, "actual")
	path := "cursor-project/agent-transcripts/scoped.jsonl"
	otherPath := "other-project/agent-transcripts/scoped.jsonl"
	const sessionID = "cursor:scoped"
	require.NoError(t, d.UpsertSession(db.Session{
		ID: sessionID, Agent: string(parser.AgentCursor), Machine: "local",
		Cwd: filepath.Join(workspaceRoot, "old"), FilePath: &path,
	}))
	e := NewEngine(d, EngineConfig{
		Machine:            "local",
		IncludeCwdPrefixes: []string{allowed},
	})
	t.Cleanup(func() { e.Close() })

	changed, err := e.reconcileFilteredSourceCwd(
		[]parser.ParseResult{{Session: parser.ParsedSession{
			ID: sessionID, Agent: parser.AgentCursor, Cwd: actual,
			File: parser.FileInfo{Path: otherPath},
		}}},
		sourceCwdDecision{resolution: parser.SourceCwdResolution{
			State: parser.SourceCwdResolved, Path: actual,
		}},
	)
	require.NoError(t, err)
	assert.False(t, changed)
	stored, err := d.GetSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, filepath.Join(workspaceRoot, "old"), stored.Cwd)

	changed, err = e.reconcileFilteredSourceCwd(
		[]parser.ParseResult{{Session: parser.ParsedSession{
			ID: sessionID, Agent: parser.AgentCursor, Cwd: actual,
			File: parser.FileInfo{Path: path},
		}}},
		sourceCwdDecision{resolution: parser.SourceCwdResolution{
			State: parser.SourceCwdResolved, Path: actual,
		}},
	)
	require.NoError(t, err)
	assert.True(t, changed)
	stored, err = d.GetSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, actual, stored.Cwd)
	assert.Less(t, d.GetSessionDataVersion(sessionID), db.CurrentDataVersion())
}

func TestFilteredCursorCwdReconciliationKeepsSteadyStateRowsStale(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	workspaceRoot := t.TempDir()
	allowed := filepath.Join(workspaceRoot, "allowed")
	actual := filepath.Join(workspaceRoot, "actual")
	path := "cursor-project/agent-transcripts/steady.jsonl"
	const sessionID = "cursor:steady-stale"
	require.NoError(t, d.UpsertSession(db.Session{
		ID: sessionID, Agent: string(parser.AgentCursor), Machine: "local",
		Cwd: filepath.Join(workspaceRoot, "old"), FilePath: &path,
	}))
	e := NewEngine(d, EngineConfig{
		Machine:            "local",
		IncludeCwdPrefixes: []string{allowed},
	})
	t.Cleanup(func() { e.Close() })
	vetoed := []parser.ParseResult{{Session: parser.ParsedSession{
		ID: sessionID, Agent: parser.AgentCursor, Cwd: actual,
		File: parser.FileInfo{Path: path},
	}}}
	decision := sourceCwdDecision{resolution: parser.SourceCwdResolution{
		State: parser.SourceCwdResolved, Path: actual,
	}}

	changed, err := e.reconcileFilteredSourceCwd(vetoed, decision)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Less(t, d.GetSessionDataVersion(sessionID), db.CurrentDataVersion())

	changed, err = e.reconcileFilteredSourceCwd(vetoed, decision)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Less(t, d.GetSessionDataVersion(sessionID), db.CurrentDataVersion(),
		"staleness must survive the veto so a later admitted parse refreshes the row")
	stored, err := d.GetSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, actual, stored.Cwd)
}

func TestStaleSourceReparseAdmittedPredictsCwdFilterVeto(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	workspaceRoot := t.TempDir()
	allowed := filepath.Join(workspaceRoot, "allowed")
	vetoedPath := filepath.Join(workspaceRoot, "vetoed")
	e := NewEngine(d, EngineConfig{
		Machine:            "local",
		IncludeCwdPrefixes: []string{allowed},
	})
	t.Cleanup(func() { e.Close() })
	open := NewEngine(d, EngineConfig{Machine: "local"})
	t.Cleanup(func() { open.Close() })

	tests := []struct {
		name          string
		engine        *Engine
		participating bool
		decision      sourceCwdDecision
		admitted      bool
	}{
		{
			name:   "no filter admits everything",
			engine: open, participating: true,
			decision: sourceCwdDecision{resolution: parser.SourceCwdResolution{
				State: parser.SourceCwdResolved, Path: vetoedPath,
			}},
			admitted: true,
		},
		{
			name:   "non-participating source keeps the bypass",
			engine: e, participating: false,
			admitted: true,
		},
		{
			name:   "resolved outside the allow-list is suppressed",
			engine: e, participating: true,
			decision: sourceCwdDecision{resolution: parser.SourceCwdResolution{
				State: parser.SourceCwdResolved, Path: vetoedPath,
			}},
			admitted: false,
		},
		{
			name:   "resolved inside the allow-list re-arms the bypass",
			engine: e, participating: true,
			decision: sourceCwdDecision{resolution: parser.SourceCwdResolution{
				State: parser.SourceCwdResolved,
				Path:  filepath.Join(allowed, "app"),
			}},
			admitted: true,
		},
		{
			name:   "unavailable falls back to the vetoed stored cwd",
			engine: e, participating: true,
			decision: sourceCwdDecision{
				resolution: parser.SourceCwdResolution{
					State: parser.SourceCwdUnavailable,
				},
				storedCwd: vetoedPath, storedOK: true,
			},
			admitted: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.admitted, tt.engine.staleSourceReparseAdmitted(
				tt.participating, tt.decision,
			))
		})
	}
}

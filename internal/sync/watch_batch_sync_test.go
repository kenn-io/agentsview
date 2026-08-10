package sync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func TestValidateWatchBatchRejectsMalformedScope(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	child := filepath.Join(root, "nested")
	tests := []struct {
		name     string
		batch    WatchBatch
		recovery *WatchRecoveryScope
	}{
		{name: "empty"},
		{name: "blank path", batch: WatchBatch{Paths: []string{""}}},
		{name: "blank reconciliation root", batch: WatchBatch{ReconcileRoots: []string{""}}},
		{name: "blank rename path", batch: WatchBatch{Renames: []WatchRename{{}}}},
		{name: "invalid item type", batch: WatchBatch{Renames: []WatchRename{{
			Path: root, ItemType: WatchItemType(99),
		}}}},
		{name: "full retains paths", batch: WatchBatch{FullSync: true, Paths: []string{root}}, recovery: &WatchRecoveryScope{}},
		{name: "full retains roots", batch: WatchBatch{FullSync: true, ReconcileRoots: []string{root}}, recovery: &WatchRecoveryScope{}},
		{name: "full retains renames", batch: WatchBatch{FullSync: true, Renames: []WatchRename{{Path: root}}}, recovery: &WatchRecoveryScope{}},
		{name: "full without recovery", batch: WatchBatch{FullSync: true}},
		{name: "rename without recovery", batch: WatchBatch{Renames: []WatchRename{{Path: root, ItemType: ItemIsFile}}}},
		{name: "blank available recovery root", batch: WatchBatch{FullSync: true}, recovery: &WatchRecoveryScope{AvailableRoots: []string{""}}},
		{name: "blank deferred recovery root", batch: WatchBatch{FullSync: true}, recovery: &WatchRecoveryScope{DeferredRoots: []string{""}}},
		{name: "equal recovery roots", batch: WatchBatch{FullSync: true}, recovery: &WatchRecoveryScope{AvailableRoots: []string{root}, DeferredRoots: []string{root}}},
		{name: "available ancestor overlaps deferred", batch: WatchBatch{FullSync: true}, recovery: &WatchRecoveryScope{AvailableRoots: []string{root}, DeferredRoots: []string{child}}},
		{name: "deferred ancestor overlaps available", batch: WatchBatch{FullSync: true}, recovery: &WatchRecoveryScope{AvailableRoots: []string{child}, DeferredRoots: []string{root}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Error(t, ValidateWatchBatch(tt.batch, tt.recovery))
		})
	}
}

func TestSyncWatchBatchThenRunChangedPathCardinalityAndSerialization(t *testing.T) {
	const agent parser.AgentType = "watch-batch-cardinality"
	type outcome struct {
		classifications int32
		parses          int32
	}
	var outcomes []outcome
	for _, unrelated := range []int{1, 10_000} {
		t.Run(fmt.Sprintf("unrelated-%d", unrelated), func(t *testing.T) {
			database, engine, provider, _, path := newChangedPathOutcomeEngine(
				t, agent, func(path string) parser.ParseOutcome {
					started := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
					return parser.ParseOutcome{
						Results: []parser.ParseResultOutcome{{Result: parser.ParseResult{
							Session: parser.ParsedSession{
								ID: "changed", Agent: agent, Project: "project",
								Machine: "local", StartedAt: started, EndedAt: started,
								File: parser.FileInfo{Path: path},
							},
						}, DataVersion: parser.DataVersionCurrent}},
						ResultSetComplete: true,
					}
				},
			)
			coldRoot := t.TempDir()
			for i := range unrelated {
				unrelatedPath := filepath.Join(coldRoot, fmt.Sprintf("%05d.jsonl", i))
				require.NoError(t, database.UpsertSession(db.Session{
					ID: fmt.Sprintf("unrelated-%05d", i), Agent: "claude",
					Project: "cold", Machine: "local", FilePath: &unrelatedPath,
					MessageCount: 1, UserMessageCount: 1,
				}))
			}

			callbackEntered := make(chan struct{})
			releaseCallback := make(chan struct{})
			firstDone := make(chan error, 1)
			go func() {
				_, err := engine.SyncWatchBatchThenRun(
					context.Background(), WatchBatch{Paths: []string{path}}, nil,
					func() error {
						stored, getErr := database.GetSession(context.Background(), "changed")
						if getErr != nil {
							return getErr
						}
						if stored == nil {
							return fmt.Errorf("changed session unavailable to callback")
						}
						close(callbackEntered)
						<-releaseCallback
						return nil
					},
				)
				firstDone <- err
			}()
			require.Eventually(t, func() bool {
				select {
				case <-callbackEntered:
					return true
				default:
					return false
				}
			}, time.Second, time.Millisecond)

			secondDone := make(chan error, 1)
			go func() {
				secondDone <- engine.SyncPathsContext(context.Background(), []string{path})
			}()
			select {
			case err := <-secondDone:
				require.Failf(t, "concurrent sync entered callback critical section", "%v", err)
			case <-time.After(25 * time.Millisecond):
			}
			close(releaseCallback)
			require.NoError(t, <-firstDone)
			require.NoError(t, <-secondDone)
			outcomes = append(outcomes, outcome{
				classifications: provider.changedPathCalls.Load(),
				parses:          provider.parseCalls.Load(),
			})
		})
	}
	require.Len(t, outcomes, 2)
	assert.Equal(t, outcomes[0], outcomes[1])
	assert.Positive(t, outcomes[0].parses)
}

func TestSyncWatchBatchThenRunMissingPathTombstoneIsCardinalityBounded(t *testing.T) {
	const agent parser.AgentType = "watch-batch-delete"
	var classifications []int32
	for _, unrelated := range []int{1, 10_000} {
		t.Run(fmt.Sprintf("unrelated-%d", unrelated), func(t *testing.T) {
			database, engine, provider, _, path := newChangedPathOutcomeEngine(
				t, agent, func(path string) parser.ParseOutcome {
					started := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
					return parser.ParseOutcome{
						Results: []parser.ParseResultOutcome{{Result: parser.ParseResult{
							Session: parser.ParsedSession{
								ID: "deleted", Agent: agent, Project: "project",
								Machine: "local", StartedAt: started, EndedAt: started,
								File: parser.FileInfo{Path: path},
							},
						}, DataVersion: parser.DataVersionCurrent}},
						ResultSetComplete: true,
					}
				},
			)
			_, err := engine.SyncWatchBatchThenRun(
				t.Context(), WatchBatch{Paths: []string{path}}, nil, func() error { return nil },
			)
			require.NoError(t, err)
			coldRoot := t.TempDir()
			for i := range unrelated {
				unrelatedPath := filepath.Join(coldRoot, fmt.Sprintf("%05d.jsonl", i))
				require.NoError(t, database.UpsertSession(db.Session{
					ID: fmt.Sprintf("delete-unrelated-%05d", i), Agent: "claude",
					Project: "cold", Machine: "local", FilePath: &unrelatedPath,
					MessageCount: 1, UserMessageCount: 1,
				}))
			}
			provider.changedPathCalls.Store(0)
			provider.source = nil
			require.NoError(t, os.Remove(path))
			_, err = engine.SyncWatchBatchThenRun(
				t.Context(), WatchBatch{Paths: []string{path}}, nil,
				func() error {
					stored, getErr := database.GetSession(t.Context(), "deleted")
					require.NoError(t, getErr)
					assert.Nil(t, stored)
					return nil
				},
			)
			require.NoError(t, err)
			classifications = append(classifications, provider.changedPathCalls.Load())
		})
	}
	require.Len(t, classifications, 2)
	assert.Equal(t, classifications[0], classifications[1])
	assert.Equal(t, int32(1), classifications[0])
}

func TestValidateWatchBatchAcceptsBoundedAndAuthoritativeScopes(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	tests := []struct {
		name     string
		batch    WatchBatch
		recovery *WatchRecoveryScope
	}{
		{name: "path", batch: WatchBatch{Paths: []string{filepath.Join(root, "session.jsonl")}}},
		{name: "root", batch: WatchBatch{ReconcileRoots: []string{root}, LostEvents: true}},
		{name: "full", batch: WatchBatch{FullSync: true, LostEvents: true}, recovery: &WatchRecoveryScope{AvailableRoots: []string{root}, DeferredRoots: []string{other}}},
		{name: "rename", batch: WatchBatch{Renames: []WatchRename{{
			Path: filepath.Join(root, "old"), Root: root, ItemType: ItemIsDir,
		}}}, recovery: &WatchRecoveryScope{AvailableRoots: []string{root}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.NoError(t, ValidateWatchBatch(tt.batch, tt.recovery))
		})
	}
}

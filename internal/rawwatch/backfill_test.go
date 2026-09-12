package rawwatch

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawcapture"
	"go.kenn.io/agentsview/internal/rawcheckpoint"
	"go.kenn.io/agentsview/internal/rawupload"
)

func backfillFixture(t *testing.T, root string) (*rawcheckpoint.Store, rawcheckpoint.BackfillRunSpec) {
	t.Helper()
	store, err := rawcheckpoint.Open(t.Context(), filepath.Join(t.TempDir(), "checkpoint.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	require.NoError(t, store.SetDevice(t.Context(), "device-a"))
	configured, err := store.ResolveConfiguredRoot(t.Context(), parser.AgentClaude, root)
	require.NoError(t, err)
	return store, rawcheckpoint.BackfillRunSpec{RunID: "run-a", DeviceID: "device-a", Destination: "https://ingest.example", Providers: []parser.AgentType{parser.AgentClaude}, Roots: []rawcheckpoint.BackfillSelection{{Provider: parser.AgentClaude, ConfiguredRootID: configured.ID}}}
}
func TestBackfillFiniteCompletionAndRestart(t *testing.T) {
	for _, batch := range []int{1, 512} {
		t.Run(fmt.Sprint(batch), func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(root, "a.jsonl"), []byte("first\n"), 0o600))
			store, spec := backfillFixture(t, root)
			provider := newAuditProvider(root)
			transport := &recordingRawUploadTransport{}
			c := rawcapture.New(store)
			u := rawupload.New(store, transport, "device-a")
			opts := BackfillOptions{Spec: spec, Providers: []parser.Provider{provider}, BatchSize: batch}
			p, err := RunBackfill(t.Context(), store, c, u, opts)
			require.NoError(t, err)
			require.True(t, p.Complete)
			require.Equal(t, int64(1), p.Captured)
			require.Equal(t, int64(1), p.Acknowledged)
			calls := provider.streamCalls
			require.NoError(t, os.WriteFile(filepath.Join(root, "a.jsonl"), []byte("later\n"), 0o600))
			again, err := RunBackfill(t.Context(), store, c, u, opts)
			require.NoError(t, err)
			require.Equal(t, p, again)
			require.Equal(t, calls, provider.streamCalls)
			require.Equal(t, 1, transport.commits)
		})
	}
}

func TestBackfillFinalizationPreservesDurableCompletionAfterCancellation(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.jsonl"), []byte("first\n"), 0o600))
	store, spec := backfillFixture(t, root)
	provider := newAuditProvider(root)
	completed, err := RunBackfill(t.Context(), store, rawcapture.New(store), rawupload.New(store, &recordingRawUploadTransport{}, "device-a"), BackfillOptions{Spec: spec, Providers: []parser.Provider{provider}, BatchSize: 1})
	require.NoError(t, err)
	require.True(t, completed.Complete)
	require.Equal(t, int64(1), completed.Captured)
	require.Equal(t, int64(1), completed.Acknowledged)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	progress, err := finalizeBackfillAttempt(ctx, store, spec.RunID, "", rawcheckpoint.BackfillProgress{}, nil)
	require.NoError(t, err)
	require.Equal(t, completed, progress)
}

func TestBackfillFinalizationKeepsIncompleteCancellationAndCounts(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.jsonl"), []byte("first\n"), 0o600))
	store, spec := backfillFixture(t, root)
	_, err := store.BeginBackfill(t.Context(), spec)
	require.NoError(t, err)
	provider := newAuditProvider(root)
	_, err = rawcapture.New(store).CaptureForBackfill(t.Context(), provider, parser.SourceRef{Provider: parser.AgentClaude, Key: "a.jsonl", DisplayPath: filepath.Join(root, "a.jsonl")}, spec.RunID)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	progress, err := finalizeBackfillAttempt(ctx, store, spec.RunID, "", rawcheckpoint.BackfillProgress{}, nil)
	require.ErrorIs(t, err, rawcheckpoint.ErrBackfillIncomplete)
	require.False(t, progress.Complete)
	require.Equal(t, int64(1), progress.Captured)
	require.Zero(t, progress.Acknowledged)
	require.Equal(t, int64(1), progress.Pending)
	require.Equal(t, int64(1), progress.Failures["cancelled"])
	durable, err := store.BackfillProgress(t.Context(), spec.RunID)
	require.NoError(t, err)
	require.Equal(t, progress, durable)
}

func TestBackfillTerminalIncompleteDoesNotRestart(t *testing.T) {
	root := t.TempDir()
	store, spec := backfillFixture(t, root)
	missing := filepath.Join(root, "missing")
	provider := newPartialAuditProvider(root, missing)
	require.NoError(t, os.Mkdir(missing, 0o700))
	configured, err := store.ResolveConfiguredRoot(t.Context(), parser.AgentClaude, missing)
	require.NoError(t, err)
	spec.Roots = append(spec.Roots, rawcheckpoint.BackfillSelection{Provider: parser.AgentClaude, ConfiguredRootID: configured.ID})
	require.NoError(t, os.Remove(missing))
	p, err := RunBackfill(t.Context(), store, rawcapture.New(store), rawupload.New(store, &recordingRawUploadTransport{}, "device-a"), BackfillOptions{Spec: spec, Providers: []parser.Provider{provider}, BatchSize: 1})
	require.Error(t, err)
	require.False(t, p.Complete)
	require.NotEmpty(t, p.Failures)
	require.LessOrEqual(t, provider.streamCalls, 1)
}

func TestBackfillUnreadableEmptyRootCannotComplete(t *testing.T) {
	root := t.TempDir()
	store, spec := backfillFixture(t, root)
	require.NoError(t, os.Chmod(root, 0))
	t.Cleanup(func() { require.NoError(t, os.Chmod(root, 0o700)) })
	p, err := RunBackfill(t.Context(), store, rawcapture.New(store), rawupload.New(store, &recordingRawUploadTransport{}, "device-a"), BackfillOptions{Spec: spec, Providers: []parser.Provider{newAuditProvider(root)}, BatchSize: 1})
	require.Error(t, err)
	require.False(t, p.Complete)
}

func TestBackfillEarlierChangedCandidateRemainsIncomplete(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.jsonl", "b.jsonl"} {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(name), 0o600))
	}
	store, spec := backfillFixture(t, root)
	provider := newAuditProvider(root)
	provider.planErrorAt = 2
	provider.planError = rawcapture.ErrSourceChanged
	p, err := RunBackfill(t.Context(), store, rawcapture.New(store), rawupload.New(store, &recordingRawUploadTransport{}, "device-a"), BackfillOptions{Spec: spec, Providers: []parser.Provider{provider}, BatchSize: 1})
	require.Error(t, err)
	require.False(t, p.Complete)
	require.Equal(t, int64(1), p.Failures["source_changed"])
	require.Equal(t, 1, provider.streamCalls)
}

// The seed models captures durable before a crash. One source uses real capture;
// the remaining rows duplicate that one-byte immutable generation with their
// own source, capture and entry identities in a single setup transaction.
func seedBackfillMembers(t *testing.T, store *rawcheckpoint.Store, checkpointPath string, spec rawcheckpoint.BackfillRunSpec, count int) {
	t.Helper()
	db, err := sql.Open("sqlite3", checkpointPath)
	require.NoError(t, err)
	defer db.Close()
	tx, err := db.Begin()
	require.NoError(t, err)
	defer tx.Rollback()
	var original string
	require.NoError(t, tx.QueryRow(`SELECT capture_id FROM backfill_members WHERE run_id=?`, spec.RunID).Scan(&original))
	for i := 1; i < count; i++ {
		name := fmt.Sprintf("%05d.jsonl", i)
		id := fmt.Sprintf("%032x", i)
		_, err = tx.Exec(`INSERT INTO raw_sources(provider,configured_root_id,source_key,updated_at,latest_capture_id,observation_revision)
   SELECT provider,configured_root_id,?,updated_at,?,observation_revision FROM raw_sources WHERE latest_capture_id=?`, name, id, original)
		require.NoError(t, err)
		_, err = tx.Exec(`INSERT INTO outbox_generations(capture_id,provider,configured_root_id,source_key,captured_at,kind,state,metadata_bytes,created_at,updated_at)
   SELECT ?,provider,configured_root_id,?,captured_at,kind,state,metadata_bytes,created_at,updated_at FROM outbox_generations WHERE capture_id=?`, id, name, original)
		require.NoError(t, err)
		_, err = tx.Exec(`INSERT INTO outbox_entries(capture_id,entry_ordinal,path,length,mod_time_ns,file_identity,prefix_sha256,appendable)
   SELECT ?,entry_ordinal,?,length,mod_time_ns,file_identity,prefix_sha256,appendable FROM outbox_entries WHERE capture_id=?`, id, name, original)
		require.NoError(t, err)
		_, err = tx.Exec(`INSERT INTO outbox_entry_objects(capture_id,entry_ordinal,object_ordinal,sha256,length)
   SELECT ?,entry_ordinal,object_ordinal,sha256,length FROM outbox_entry_objects WHERE capture_id=?`, id, original)
		require.NoError(t, err)
		_, err = tx.Exec(`INSERT INTO backfill_members(run_id,provider,configured_root_id,source_key,ordinal,capture_id,status)
   SELECT run_id,provider,configured_root_id,?,?,?,'pending' FROM backfill_members WHERE capture_id=?`, name, i+1, id, original)
		require.NoError(t, err)
	}
	_, err = tx.Exec(`UPDATE outbox_objects SET ref_count=?`, count)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
}

func TestBackfillAuditorBoundsTraversalAndResumeCandidates(t *testing.T) {
	for _, count := range []int{100, 10000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			root := t.TempDir()
			for i := range count {
				require.NoError(t, os.WriteFile(filepath.Join(root, fmt.Sprintf("%05d.jsonl", i)), []byte("x"), 0o600))
			}
			checkpointPath := filepath.Join(t.TempDir(), "checkpoint.db")
			store, err := rawcheckpoint.Open(t.Context(), checkpointPath)
			require.NoError(t, err)
			defer store.Close()
			require.NoError(t, store.SetDevice(t.Context(), "device-a"))
			configured, err := store.ResolveConfiguredRoot(t.Context(), parser.AgentClaude, root)
			require.NoError(t, err)
			spec := rawcheckpoint.BackfillRunSpec{RunID: "run-a", DeviceID: "device-a", Destination: "https://ingest.example", Providers: []parser.AgentType{parser.AgentClaude}, Roots: []rawcheckpoint.BackfillSelection{{Provider: parser.AgentClaude, ConfiguredRootID: configured.ID}}}
			_, err = store.BeginBackfill(t.Context(), spec)
			require.NoError(t, err)
			provider := &measuredBackfillProvider{auditProvider: newAuditProvider(root)}
			_, err = rawcapture.New(store).CaptureForBackfill(t.Context(), provider, parser.SourceRef{Provider: parser.AgentClaude, Key: "00000.jsonl", DisplayPath: filepath.Join(root, "00000.jsonl")}, spec.RunID)
			require.NoError(t, err)
			seedBackfillMembers(t, store, checkpointPath, spec, count)
			for _, batch := range []int{1, 512} {
				t.Run(fmt.Sprint(batch), func(t *testing.T) {
					runtime.GC()
					var before, running runtime.MemStats
					runtime.ReadMemStats(&before)
					a := NewBackfillAuditor(store, rawcapture.New(store), batch, spec.RunID)
					for range 10 {
						plans := provider.planCalls
						physical := int(provider.steps.Load())
						result, err := a.AuditProvider(t.Context(), provider)
						require.NoError(t, err)
						require.LessOrEqual(t, result.Candidates, batch)
						require.LessOrEqual(t, provider.planCalls-plans, batch)
						require.LessOrEqual(t, int(provider.steps.Load())-physical, batch+1)
						require.Zero(t, result.Captured, "bound sources must never recapture")
						if result.PassFinished {
							break
						}
					}
					runtime.GC()
					runtime.ReadMemStats(&running)
					require.Less(t, int64(running.HeapAlloc)-int64(before.HeapAlloc), int64(4<<20), "suspended streaming discovery must not retain the full source set")
					a.Close()
					require.Empty(t, a.scans)
					p, err := store.BackfillProgress(t.Context(), spec.RunID)
					require.NoError(t, err)
					require.Equal(t, int64(count), p.Captured)
					usage, err := store.OutboxUsage(t.Context())
					require.NoError(t, err)
					require.Zero(t, usage.ReservedBytes)
				})
			}
		})
	}
}

type incompleteBackfillProvider struct{ *auditProvider }

func (p *incompleteBackfillProvider) DiscoverRawCaptureSourcesEach(ctx context.Context, _ func(parser.SourceRef) error) (bool, error) {
	p.streamCalls++
	return false, parser.ReportRawCaptureDiscoveryProgress(ctx)
}
func TestBackfillTerminalEmptyIncompleteStreamStops(t *testing.T) {
	root := t.TempDir()
	store, spec := backfillFixture(t, root)
	provider := &incompleteBackfillProvider{newAuditProvider(root)}
	p, err := RunBackfill(t.Context(), store, rawcapture.New(store), rawupload.New(store, &recordingRawUploadTransport{}, "device-a"), BackfillOptions{Spec: spec, Providers: []parser.Provider{provider}, BatchSize: 1})
	require.Error(t, err)
	require.False(t, p.Complete)
	require.Zero(t, p.Captured)
	require.Equal(t, 1, provider.streamCalls)
	require.NotEmpty(t, p.Failures)
}

func TestBackfillFreshBatchesKeepTraversalAndSpoolBounded(t *testing.T) {
	for _, count := range []int{100, 10000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			root := t.TempDir()
			for i := range count {
				require.NoError(t, os.WriteFile(filepath.Join(root, fmt.Sprintf("%05d.jsonl", i)), []byte("x"), 0o600))
			}
			store, spec := backfillFixture(t, root)
			_, err := store.BeginBackfill(t.Context(), spec)
			require.NoError(t, err)
			provider := &measuredBackfillProvider{auditProvider: newAuditProvider(root)}
			a := NewBackfillAuditor(store, rawcapture.New(store), 1, spec.RunID)
			defer a.Close()
			u := rawupload.New(store, &recordingRawUploadTransport{}, "device-a")
			for range 12 {
				before := int(provider.steps.Load())
				plans := provider.planCalls
				batch, err := a.AuditProvider(t.Context(), provider)
				require.NoError(t, err)
				require.LessOrEqual(t, batch.Candidates, 1)
				require.LessOrEqual(t, batch.Captured, 1)
				require.LessOrEqual(t, int(provider.steps.Load())-before, 2)
				require.LessOrEqual(t, provider.planCalls-plans, 3)
				usage, err := store.OutboxUsage(t.Context())
				require.NoError(t, err)
				require.LessOrEqual(t, usage.UsedBytes, int64(1793))
				require.Zero(t, usage.ReservedBytes)
				_, _, err = u.UploadNextForBackfill(t.Context(), spec.RunID)
				require.NoError(t, err)
			}
		})
	}
}

func TestBackfillUsesConfiguredRootsForRealCodexProvider(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	require.NoError(t, os.Mkdir(root, 0o700))
	store, err := rawcheckpoint.Open(t.Context(), filepath.Join(t.TempDir(), "checkpoint.db"))
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.SetDevice(t.Context(), "device-a"))
	configured, err := store.ResolveConfiguredRoot(t.Context(), parser.AgentCodex, root)
	require.NoError(t, err)
	spec := rawcheckpoint.BackfillRunSpec{RunID: "run-a", DeviceID: "device-a", Destination: "https://ingest.example", Providers: []parser.AgentType{parser.AgentCodex}, Roots: []rawcheckpoint.BackfillSelection{{Provider: parser.AgentCodex, ConfiguredRootID: configured.ID}}}
	provider, ok := parser.NewProvider(parser.AgentCodex, parser.ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	p, err := RunBackfill(t.Context(), store, rawcapture.New(store), rawupload.New(store, &recordingRawUploadTransport{}, "device-a"), BackfillOptions{Spec: spec, Providers: []parser.Provider{provider}, BatchSize: 1})
	require.NoError(t, err, "progress: %+v", p)
	require.True(t, p.Complete)
	require.Zero(t, p.Captured)
}

func TestBackfillRealClaudeCapturesRawBytes(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	require.NoError(t, os.Mkdir(project, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(project, "session.jsonl"), []byte("synthetic raw bytes\n"), 0o600))
	store, spec := backfillFixture(t, root)
	provider, ok := parser.NewProvider(parser.AgentClaude, parser.ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	transport := newBackfillCustody(t)
	p, err := RunBackfill(t.Context(), store, rawcapture.New(store), rawupload.New(store, transport, "device-a"), BackfillOptions{Spec: spec, Providers: []parser.Provider{provider}, BatchSize: 1})
	require.NoError(t, err)
	require.True(t, p.Complete)
	require.Equal(t, int64(1), p.Captured)
}

// The stream may advance its next physical step after yielding a candidate.
// Count that boundary atomically; ordinary auditProvider counters are only safe
// to inspect once its streaming goroutine has joined.
type measuredBackfillProvider struct {
	*auditProvider
	steps atomic.Int64
}

func (p *measuredBackfillProvider) DiscoverRawCaptureSourcesEach(ctx context.Context, yield func(parser.SourceRef) error) (bool, error) {
	return p.auditProvider.DiscoverRawCaptureSourcesEach(parser.WithRawCaptureDiscoveryProgress(ctx, func() error {
		p.steps.Add(1)
		return parser.ReportRawCaptureDiscoveryProgress(ctx)
	}), yield)
}

type unsupportedCandidateProvider struct{ *auditProvider }

func (p *unsupportedCandidateProvider) Capabilities() parser.Capabilities {
	caps := p.auditProvider.Capabilities()
	if p.planCalls > 0 {
		caps.RawCapture.Support = parser.CapabilityUnsupported
	}
	return caps
}
func TestBackfillUnsupportedCandidateIsDurablyIncomplete(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.jsonl"), []byte("x"), 0o600))
	store, spec := backfillFixture(t, root)
	provider := &unsupportedCandidateProvider{newAuditProvider(root)}
	p, err := RunBackfill(t.Context(), store, rawcapture.New(store), rawupload.New(store, &recordingRawUploadTransport{}, "device-a"), BackfillOptions{Spec: spec, Providers: []parser.Provider{provider}, BatchSize: 1})
	require.Error(t, err)
	require.False(t, p.Complete)
	require.Equal(t, int64(1), p.Failures["unsupported"])
	require.Equal(t, 1, provider.streamCalls)
}

func TestBackfillRecoveryCannotTurnLostCaptureIntoCompletion(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.jsonl"), []byte("x"), 0o600))
	store, spec := backfillFixture(t, root)
	_, err := store.BeginBackfill(t.Context(), spec)
	require.NoError(t, err)
	captured, err := rawcapture.New(store).CaptureForBackfill(t.Context(), newAuditProvider(root), parser.SourceRef{Provider: parser.AgentClaude, Key: "a.jsonl", DisplayPath: filepath.Join(root, "a.jsonl")}, spec.RunID)
	require.NoError(t, err)
	generation, found, err := store.NextGeneration(t.Context())
	require.NoError(t, err)
	require.True(t, found)
	require.NoError(t, os.Remove(store.ObjectPath(generation.Entries[0].Objects[0])))
	_, err = store.Recover(t.Context())
	require.NoError(t, err)
	p, err := store.BackfillProgress(t.Context(), spec.RunID)
	require.NoError(t, err)
	require.False(t, p.Complete)
	require.Equal(t, int64(1), p.Pending)
	require.Equal(t, int64(1), p.Failures["capture_lost"])
	member, found, err := store.BackfillSource(t.Context(), spec.RunID, captured.Source)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "invalidated", member.Status)
	_, err = store.CompleteBackfill(t.Context(), spec.RunID)
	require.ErrorIs(t, err, rawcheckpoint.ErrBackfillIncomplete)
}

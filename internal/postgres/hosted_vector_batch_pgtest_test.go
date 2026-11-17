//go:build pgtest

package postgres

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/storage"
	"go.kenn.io/kit/embedclient"
	kitvec "go.kenn.io/kit/vector"
)

func hostedVectorBatchRecovery(t *testing.T) {
	f := newHostedFixture(t, "tenant-batch")
	_, err := f.admin.ExecContext(t.Context(), `CREATE EXTENSION IF NOT EXISTS vector SCHEMA public`)
	require.NoError(t, err)
	gen := storage.VectorGenerationInfo{Fingerprint: "batch-fixture", Model: "fixture", Dimension: 2}
	require.NoError(t, ProvisionHostedVector(t.Context(), f.admin, f.schema, f.tenant, gen))
	id, _, ok, err := LookupVectorGeneration(t.Context(), f.runtime, gen.Fingerprint)
	require.NoError(t, err)
	require.True(t, ok)
	_, err = f.admin.ExecContext(t.Context(), `GRANT SELECT,INSERT,DELETE ON `+vectorChunkTable(id)+` TO "`+f.role+`"`)
	require.NoError(t, err)
	for i := range 18 {
		session := fmt.Sprintf("batch-%02d", i)
		_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent,provenance_kind,raw_content_revision,is_automated) VALUES($1,'p','m','codex','raw','r1',$2)`, session, i == 17)
		require.NoError(t, err)
		_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO messages(session_id,ordinal,role,content) VALUES($1,0,'user',$1)`, session)
		require.NoError(t, err)
	}
	calls := map[string]int{}
	failing := true
	encode := func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i, text := range texts {
			calls[text]++
			if failing && strings.Contains(text, "batch-00") {
				return nil, &embedclient.APIError{StatusCode: 503, Reason: embedclient.ReasonUnavailable}
			}
			out[i] = []float32{1, 0}
		}
		return out, nil
	}
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO vector_push_state(generation_id,session_id,doc_agg_hash) VALUES($1,'orphan','old')`, id)
	require.NoError(t, err)
	err = EmbedBatch(t.Context(), f.runtime, gen, encode, 128, 1, false)
	require.ErrorContains(t, err, "batch-00")
	require.ErrorContains(t, err, "HTTP 503 (unavailable)")
	for i := range 17 {
		session := fmt.Sprintf("batch-%02d", i)
		assert.Equal(t, 1, calls[session])
		var stamped bool
		require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM vector_push_state WHERE generation_id=$1 AND session_id=$2)`, id, session).Scan(&stamped))
		assert.Equal(t, i != 0, stamped, session)
	}
	var orphan bool
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM vector_push_state WHERE session_id='orphan')`).Scan(&orphan))
	assert.False(t, orphan)
	failing = false
	require.NoError(t, EmbedBatch(t.Context(), f.runtime, gen, encode, 128, 1, false))
	for i := range 17 {
		want := 1
		if i == 0 {
			want = 2
		}
		assert.Equal(t, want, calls[fmt.Sprintf("batch-%02d", i)])
	}
	require.NoError(t, EmbedBatch(t.Context(), f.runtime, gen, encode, 128, 1, true))
	assert.Equal(t, 1, calls["batch-17"])
	require.NoError(t, EmbedBatch(t.Context(), f.runtime, gen, encode, 128, 1, false))
	var count int
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM vector_documents WHERE session_id='batch-17'`).Scan(&count))
	assert.Zero(t, count)
	// A projection that changes during encoding must not get a stale stamp.
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE sessions SET raw_content_revision='r2' WHERE id='batch-00'`)
	require.NoError(t, err)
	racing := func(ctx context.Context, texts []string) ([][]float32, error) {
		_, err := f.runtime.ExecContext(ctx, `UPDATE sessions SET raw_content_revision='r3' WHERE id='batch-00'`)
		if err != nil {
			return nil, err
		}
		return [][]float32{{1, 0}}, nil
	}
	require.NoError(t, EmbedBatch(t.Context(), f.runtime, gen, racing, 128, 1, false))
	var stamp string
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT doc_agg_hash FROM vector_push_state WHERE generation_id=$1 AND session_id='batch-00'`, id).Scan(&stamp))
	assert.Equal(t, "r1|false", stamp)
	require.NoError(t, EmbedBatch(t.Context(), f.runtime, gen, encode, 128, 1, false))
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT doc_agg_hash FROM vector_push_state WHERE generation_id=$1 AND session_id='batch-00'`, id).Scan(&stamp))
	assert.Equal(t, "r3|false", stamp)
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE sessions SET raw_content_revision='r4' WHERE id='batch-00'`)
	require.NoError(t, err)
	restore := func(ctx context.Context, texts []string) ([][]float32, error) {
		_, err := f.runtime.ExecContext(ctx, `DELETE FROM sessions WHERE id='batch-00'; INSERT INTO sessions(id,project,machine,agent,provenance_kind,raw_content_revision) VALUES('batch-00','p','m','codex','raw','r5'); INSERT INTO messages(session_id,ordinal,role,content) VALUES('batch-00',0,'user','restored')`)
		if err != nil {
			return nil, err
		}
		return [][]float32{{1, 0}}, nil
	}
	require.NoError(t, EmbedBatch(t.Context(), f.runtime, gen, restore, 128, 1, false))
	require.NoError(t, EmbedBatch(t.Context(), f.runtime, gen, encode, 128, 1, false))
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT content FROM vector_documents WHERE session_id='batch-00'`).Scan(&stamp))
	assert.Equal(t, "restored", stamp)
}

func TestHostedVectorFailureDiagnosticsAndCancellation(t *testing.T) {
	f := newHostedFixture(t, "tenant-failures")
	_, err := f.admin.ExecContext(t.Context(), `CREATE EXTENSION IF NOT EXISTS vector SCHEMA public`)
	require.NoError(t, err)
	gen := storage.VectorGenerationInfo{Fingerprint: "failures-fixture", Model: "fixture", Dimension: 2}
	require.NoError(t, ProvisionHostedVector(t.Context(), f.admin, f.schema, f.tenant, gen))
	id, _, ok, err := LookupVectorGeneration(t.Context(), f.runtime, gen.Fingerprint)
	require.NoError(t, err)
	require.True(t, ok)
	_, err = f.admin.ExecContext(t.Context(), `GRANT SELECT,INSERT,DELETE ON `+vectorChunkTable(id)+` TO "`+f.role+`"`)
	require.NoError(t, err)
	for i := range 6 {
		session := fmt.Sprintf("fail-%02d", i)
		_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent,provenance_kind,raw_content_revision) VALUES($1,'p','m','codex','raw','r1')`, session)
		require.NoError(t, err)
		_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO messages(session_id,ordinal,role,content) VALUES($1,0,'user',$1)`, session)
		require.NoError(t, err)
	}
	encode := func(context.Context, []string) ([][]float32, error) {
		return nil, &embedclient.APIError{StatusCode: 503, Reason: embedclient.ReasonUnavailable}
	}
	err = EmbedBatch(t.Context(), f.runtime, gen, encode, 128, 1, false)
	require.ErrorContains(t, err, "6 hosted sessions failed embedding")
	for i := range 6 {
		id := fmt.Sprintf("fail-%02d", i)
		if i < 3 {
			assert.ErrorContains(t, err, id+": embedding endpoint returned HTTP 503 (unavailable)")
		} else {
			assert.NotContains(t, err.Error(), id)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	encode = func(context.Context, []string) ([][]float32, error) {
		calls++
		cancel()
		return nil, context.Canceled
	}
	err = EmbedBatch(ctx, f.runtime, gen, encode, 128, 1, false)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, calls)
}

func TestHostedVectorPartialRetry(t *testing.T) {
	f := newHostedFixture(t, "tenant-partial")
	_, err := f.admin.ExecContext(t.Context(), `CREATE EXTENSION IF NOT EXISTS vector SCHEMA public`)
	require.NoError(t, err)
	gen := storage.VectorGenerationInfo{Fingerprint: "partial-fixture", Model: "fixture", Dimension: 2}
	require.NoError(t, ProvisionHostedVector(t.Context(), f.admin, f.schema, f.tenant, gen))
	id, _, ok, err := LookupVectorGeneration(t.Context(), f.runtime, gen.Fingerprint)
	require.NoError(t, err)
	require.True(t, ok)
	_, err = f.admin.ExecContext(t.Context(), `GRANT SELECT,INSERT,DELETE ON `+vectorChunkTable(id)+` TO "`+f.role+`"`)
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent,provenance_kind,raw_content_revision) VALUES('session','p','m','codex','raw','rev'); INSERT INTO messages(session_id,ordinal,role,content) VALUES('session',0,'user','first'),('session',1,'user','poison'),('session',2,'user','last')`)
	require.NoError(t, err)
	var calls []string
	reject := true
	encode := func(_ context.Context, texts []string) ([][]float32, error) {
		calls = append(calls, texts...)
		if reject && slices.Contains(texts, "poison") {
			return nil, &embedclient.APIError{StatusCode: 400, Reason: embedclient.ReasonInputTooLong}
		}
		out := make([][]float32, len(texts))
		for i := range out {
			out[i] = []float32{1, 0}
		}
		return out, nil
	}
	require.ErrorContains(t, EmbedBatch(t.Context(), f.runtime, gen, encode, 128, 1, false, kitvec.WithBatchSize(8)), "rejected")
	var stamp string
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT doc_agg_hash FROM vector_push_state WHERE generation_id=$1 AND session_id='session'`, id).Scan(&stamp))
	assert.Equal(t, "partial|rev|false", stamp)
	var count int
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM vector_documents WHERE session_id='session'`).Scan(&count))
	assert.Equal(t, 2, count)
	calls = nil
	require.Error(t, EmbedBatch(t.Context(), f.runtime, gen, encode, 128, 1, false, kitvec.WithBatchSize(8)))
	assert.Equal(t, []string{"poison"}, calls)
	_, err = f.runtime.ExecContext(t.Context(), `DELETE FROM `+vectorChunkTable(id)+` WHERE doc_key=(SELECT doc_key FROM vector_documents WHERE session_id='session' AND content='first')`)
	require.NoError(t, err)
	calls = nil
	require.Error(t, EmbedBatch(t.Context(), f.runtime, gen, encode, 128, 1, false, kitvec.WithBatchSize(8)))
	assert.Contains(t, calls, "first")
	assert.NotContains(t, calls, "last")
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent,provenance_kind,raw_content_revision) VALUES('session-new','p','m','codex','raw','rev2'); INSERT INTO messages(session_id,ordinal,role,content) VALUES('session-new',0,'user','first'),('session-new',1,'user','last')`)
	require.NoError(t, err)
	calls = nil
	require.Error(t, EmbedBatch(t.Context(), f.runtime, gen, encode, 128, 1, false, kitvec.WithBatchSize(8)))
	assert.Contains(t, calls, "first")
	assert.Contains(t, calls, "last")
	other := storage.VectorGenerationInfo{Fingerprint: "partial-other-generation", Model: "fixture", Dimension: 2}
	require.NoError(t, ProvisionHostedVector(t.Context(), f.admin, f.schema, f.tenant, other))
	otherID, _, ok, err := LookupVectorGeneration(t.Context(), f.runtime, other.Fingerprint)
	require.NoError(t, err)
	require.True(t, ok)
	_, err = f.admin.ExecContext(t.Context(), `GRANT SELECT,INSERT,DELETE ON `+vectorChunkTable(otherID)+` TO "`+f.role+`"`)
	require.NoError(t, err)
	calls = nil
	require.Error(t, EmbedBatch(t.Context(), f.runtime, other, encode, 128, 1, false, kitvec.WithBatchSize(8)))
	assert.Contains(t, calls, "first")
	assert.Contains(t, calls, "last")
	calls = nil
	require.Error(t, EmbedBatch(t.Context(), f.runtime, gen, encode, 128, 1, true, kitvec.WithBatchSize(8)))
	assert.Contains(t, calls, "first")
	assert.Contains(t, calls, "last")
	calls = nil
	reject = false
	require.NoError(t, EmbedBatch(t.Context(), f.runtime, gen, encode, 128, 1, true, kitvec.WithBatchSize(8)))
	assert.Equal(t, []string{"poison"}, calls)
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT doc_agg_hash FROM vector_push_state WHERE generation_id=$1 AND session_id='session'`, id).Scan(&stamp))
	assert.Equal(t, "rev|true", stamp)
	calls = nil
	require.NoError(t, EmbedBatch(t.Context(), f.runtime, gen, encode, 128, 1, true, kitvec.WithBatchSize(8)))
	assert.Empty(t, calls)
}

func TestHostedVectorOrphanRestoreRetries(t *testing.T) {
	f := newHostedFixture(t, "tenant-restore")
	_, err := f.admin.ExecContext(t.Context(), `CREATE EXTENSION IF NOT EXISTS vector SCHEMA public`)
	require.NoError(t, err)
	gen := storage.VectorGenerationInfo{Fingerprint: "restore-fixture", Model: "fixture", Dimension: 2}
	require.NoError(t, ProvisionHostedVector(t.Context(), f.admin, f.schema, f.tenant, gen))
	id, _, ok, err := LookupVectorGeneration(t.Context(), f.runtime, gen.Fingerprint)
	require.NoError(t, err)
	require.True(t, ok)
	_, err = f.admin.ExecContext(t.Context(), `GRANT SELECT,INSERT,DELETE ON `+vectorChunkTable(id)+` TO "`+f.role+`"`)
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent,provenance_kind,raw_content_revision) VALUES('restore','p','m','codex','raw','rev'); INSERT INTO messages(session_id,ordinal,role,content) VALUES('restore',0,'user','restore content')`)
	require.NoError(t, err)
	calls := 0
	encode := func(_ context.Context, texts []string) ([][]float32, error) {
		calls++
		out := make([][]float32, len(texts))
		for i := range out {
			out[i] = []float32{1, 0}
		}
		return out, nil
	}
	require.NoError(t, EmbedBatch(t.Context(), f.runtime, gen, encode, 128, 1, false))
	_, err = f.runtime.ExecContext(t.Context(), `DELETE FROM sessions WHERE id='restore'`)
	require.NoError(t, err)
	gate, err := f.runtime.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = gate.Rollback() }()
	var stamp string
	require.NoError(t, gate.QueryRowContext(t.Context(), `SELECT doc_agg_hash FROM vector_push_state WHERE generation_id=$1 AND session_id='restore' FOR UPDATE`, id).Scan(&stamp))
	done := make(chan error, 1)
	go func() { done <- clearHostedOrphan(t.Context(), f.runtime, "restore") }()
	require.Eventually(t, func() bool {
		var blocked bool
		err := f.admin.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'DELETE FROM vector_push_state%')`).Scan(&blocked)
		return err == nil && blocked
	}, 30*time.Second, time.Second)
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent,provenance_kind,raw_content_revision) VALUES('restore','p','m','codex','raw','rev'); INSERT INTO messages(session_id,ordinal,role,content) VALUES('restore',0,'user','restore content')`)
	require.NoError(t, err)
	require.NoError(t, EmbedBatch(t.Context(), f.runtime, gen, encode, 128, 1, false))
	assert.Equal(t, 1, calls)
	require.NoError(t, gate.Rollback())
	require.NoError(t, <-done)
	require.NoError(t, EmbedBatch(t.Context(), f.runtime, gen, encode, 128, 1, false))
	assert.Equal(t, 2, calls)
}

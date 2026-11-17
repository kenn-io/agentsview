//go:build pgtest

package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/storage"
	"go.kenn.io/kit/embedclient"
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
				return nil, &embedclient.APIError{StatusCode: 503}
			}
			out[i] = []float32{1, 0}
		}
		return out, nil
	}
	retry := NewHostedEmbeddingBackoff()
	err = retry.EmbedBatch(t.Context(), f.runtime, gen, encode, 128, false)
	batchErr, ok := errors.AsType[*HostedEmbeddingError](err)
	require.True(t, ok)
	assert.Equal(t, "embedding endpoint returned HTTP 503", batchErr.Cause)
	assert.Equal(t, 1, calls["batch-16"], "failure must not prevent later pages")
	assert.Zero(t, calls["batch-17"], "automated session is outside the scope")
	failing = false
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent,provenance_kind,raw_content_revision) VALUES('batch-new','p','m','codex','raw','r1'); INSERT INTO messages(session_id,ordinal,role,content) VALUES('batch-new',0,'user','batch-new')`)
	require.NoError(t, err)
	require.Error(t, retry.EmbedBatch(t.Context(), f.runtime, gen, encode, 128, false))
	assert.Equal(t, 1, calls["batch-00"], "backoff skips a failed session")
	assert.Equal(t, 1, calls["batch-new"], "new sessions proceed during backoff")
	retry.pending["batch-00"].next = time.Time{}
	require.NoError(t, retry.EmbedBatch(t.Context(), f.runtime, gen, encode, 128, false))
	assert.Equal(t, 2, calls["batch-00"])
	assert.Equal(t, 1, calls["batch-16"])
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE sessions SET raw_content_revision='r1b' WHERE id='batch-00'`)
	require.NoError(t, err)
	failing = true
	require.Error(t, retry.EmbedBatch(t.Context(), f.runtime, gen, encode, 128, false))
	failing = false
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE sessions SET raw_content_revision='r1c' WHERE id='batch-00'`)
	require.NoError(t, err)
	require.NoError(t, retry.EmbedBatch(t.Context(), f.runtime, gen, encode, 128, false))
	assert.Equal(t, 4, calls["batch-00"], "a new revision bypasses the previous backoff")
	require.NoError(t, EmbedBatch(t.Context(), f.runtime, gen, encode, 128, true))
	assert.Equal(t, 1, calls["batch-17"])
	require.NoError(t, EmbedBatch(t.Context(), f.runtime, gen, encode, 128, false))
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
	require.Error(t, EmbedBatch(t.Context(), f.runtime, gen, racing, 128, false))
	var stamp string
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT doc_agg_hash FROM vector_push_state WHERE generation_id=$1 AND session_id='batch-00'`, id).Scan(&stamp))
	assert.Equal(t, "r1c|false", stamp)
	require.NoError(t, EmbedBatch(t.Context(), f.runtime, gen, encode, 128, false))
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
	require.Error(t, EmbedBatch(t.Context(), f.runtime, gen, restore, 128, false))
	require.NoError(t, EmbedBatch(t.Context(), f.runtime, gen, encode, 128, false))
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT content FROM vector_documents WHERE session_id='batch-00'`).Scan(&stamp))
	assert.Equal(t, "restored", stamp)
}

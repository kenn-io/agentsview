//go:build pgtest

package postgres

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
	"go.kenn.io/kit/embedclient"
)

func TestHostedVectorGenerationSwitch(t *testing.T) {
	f := newHostedFixture(t, "tenant-layout")
	_, err := f.admin.ExecContext(t.Context(), `CREATE EXTENSION IF NOT EXISTS vector SCHEMA public`)
	require.NoError(t, err)
	a := storage.VectorGenerationInfo{Fingerprint: "layout-a", Model: "fixture", Dimension: 2}
	b := storage.VectorGenerationInfo{Fingerprint: "layout-b", Model: "fixture", Dimension: 2}
	var aID int64
	for _, gen := range []storage.VectorGenerationInfo{a, b} {
		require.NoError(t, ProvisionHostedVector(t.Context(), f.admin, f.schema, f.tenant, gen))
		id, _, ok, err := LookupVectorGeneration(t.Context(), f.runtime, gen.Fingerprint)
		require.NoError(t, err)
		require.True(t, ok)
		_, err = f.admin.ExecContext(t.Context(), `GRANT SELECT,INSERT,DELETE ON `+vectorChunkTable(id)+` TO "`+f.role+`"`)
		require.NoError(t, err)
		if gen.Fingerprint == a.Fingerprint {
			aID = id
		}
	}
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent,provenance_kind,raw_content_revision,message_count,user_message_count) VALUES('layout','p','m','codex','raw','rev',4,1),('unchanged','p','m','codex','raw','rev',1,1),('automated','p','m','codex','raw','rev',2,2); UPDATE sessions SET is_automated=true WHERE id='automated'; INSERT INTO messages(session_id,ordinal,role,content) VALUES('layout',0,'user','question'),('layout',1,'assistant','aaaaaaaaaa'),('layout',2,'assistant','bbbbbbbbbb'),('layout',3,'assistant','cccccccccc'),('unchanged',0,'user','stable'),('automated',0,'user','first automated'),('automated',1,'user','second automated')`)
	require.NoError(t, err)
	encode := func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i := range out {
			out[i] = []float32{1, 0}
		}
		return out, nil
	}
	store := &Store{pg: f.runtime}
	store.SetVectorSearcher(NewVectorSearcher(f.runtime, aID, 2, 100, func(context.Context, string) ([]float32, error) { return []float32{1, 0}, nil }))
	searchAutomated := func() db.ContentSearchPage {
		page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: "search", Mode: "semantic", SessionID: "automated", IncludeOneShot: true, IncludeAutomated: true, Limit: 20})
		require.NoError(t, err)
		return page
	}
	require.NoError(t, EmbedBatch(t.Context(), f.runtime, a, encode, 100, 1, true))
	require.Len(t, searchAutomated().Matches, 2)
	var key, original, changed string
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT doc_key,content FROM vector_documents WHERE session_id='layout' AND ordinal=1`).Scan(&key, &original))
	require.NoError(t, EmbedBatch(t.Context(), f.runtime, b, encode, 3, 1, false))
	pageAfterB, err := store.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: "search", Mode: "semantic", SessionID: "layout", IncludeOneShot: true, Limit: 20})
	require.NoError(t, err)
	var afterB []string
	for _, match := range pageAfterB.Matches {
		afterB = append(afterB, match.Snippet)
	}
	assert.ElementsMatch(t, []string{"question"}, afterB)
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT content FROM vector_documents WHERE doc_key=$1`, key).Scan(&changed))
	require.NotEqual(t, original, changed)
	var stamped bool
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM vector_push_state WHERE generation_id=$1 AND session_id='layout')`, aID).Scan(&stamped))
	require.False(t, stamped)
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM vector_push_state WHERE generation_id=$1 AND session_id='unchanged')`, aID).Scan(&stamped))
	require.True(t, stamped)
	require.NoError(t, EmbedBatch(t.Context(), f.runtime, a, encode, 100, 1, true))
	require.Len(t, searchAutomated().Matches, 2)
	page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: "search", Mode: "semantic", IncludeOneShot: true, Limit: 20})
	require.NoError(t, err)
	var snippets []string
	for _, match := range page.Matches {
		snippets = append(snippets, match.Snippet)
	}
	assert.ElementsMatch(t, []string{"question", "bbbbbbbbbb", "stable"}, snippets)
}

func TestHostedVectorRestoreRaces(t *testing.T) {
	f := newHostedFixture(t, "tenant-scan-restore")
	_, err := f.admin.ExecContext(t.Context(), `CREATE EXTENSION IF NOT EXISTS vector SCHEMA public`)
	require.NoError(t, err)
	gen := storage.VectorGenerationInfo{Fingerprint: "scan-restore", Model: "fixture", Dimension: 2}
	require.NoError(t, ProvisionHostedVector(t.Context(), f.admin, f.schema, f.tenant, gen))
	id, _, ok, err := LookupVectorGeneration(t.Context(), f.runtime, gen.Fingerprint)
	require.NoError(t, err)
	require.True(t, ok)
	_, err = f.admin.ExecContext(t.Context(), `GRANT SELECT,INSERT,DELETE ON `+vectorChunkTable(id)+` TO "`+f.role+`"`)
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent,provenance_kind,raw_content_revision) VALUES('first','p','m','codex','raw','rev'),('restore','p','m','codex','raw','rev'); INSERT INTO messages(session_id,ordinal,role,content) VALUES('first',0,'user','first content'),('restore',0,'user','restore content')`)
	require.NoError(t, err)
	err = EmbedBatch(t.Context(), f.runtime, gen, func(_ context.Context, _ []string) ([][]float32, error) {
		_, err := f.runtime.ExecContext(t.Context(), `DELETE FROM sessions WHERE id='restore'`)
		if err != nil {
			return nil, err
		}
		return nil, errors.New("fixture failure")
	}, 128, 1, false)
	require.ErrorContains(t, err, "1 hosted sessions failed embedding")
	var stamped bool
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM vector_push_state WHERE generation_id=$1 AND session_id='restore')`, id).Scan(&stamped))
	assert.False(t, stamped)
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent,provenance_kind,raw_content_revision) VALUES('empty','p','m','codex','raw','rev')`)
	require.NoError(t, err)
	require.NoError(t, EmbedBatch(t.Context(), f.runtime, gen, func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i := range out {
			out[i] = []float32{1, 0}
		}
		return out, nil
	}, 128, 1, false))
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM vector_push_state WHERE generation_id=$1 AND session_id='empty')`, id).Scan(&stamped))
	assert.True(t, stamped)

	// Orphan cleanup can race a restore after it observes a missing session.
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent,provenance_kind,raw_content_revision) VALUES('orphan-restore','p','m','codex','raw','rev'); INSERT INTO messages(session_id,ordinal,role,content) VALUES('orphan-restore',0,'user','restore content')`)
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
	_, err = f.runtime.ExecContext(t.Context(), `DELETE FROM sessions WHERE id='orphan-restore'`)
	require.NoError(t, err)
	gate, err := f.runtime.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = gate.Rollback() }()
	var stamp string
	require.NoError(t, gate.QueryRowContext(t.Context(), `SELECT doc_agg_hash FROM vector_push_state WHERE generation_id=$1 AND session_id='orphan-restore' FOR UPDATE`, id).Scan(&stamp))
	orphanDone := make(chan error, 1)
	go func() { orphanDone <- clearHostedOrphan(t.Context(), f.runtime, "orphan-restore") }()
	require.Eventually(t, func() bool {
		var blocked bool
		err := f.admin.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'DELETE FROM vector_push_state%')`).Scan(&blocked)
		return err == nil && blocked
	}, 30*time.Second, time.Second)
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent,provenance_kind,raw_content_revision) VALUES('orphan-restore','p','m','codex','raw','rev'); INSERT INTO messages(session_id,ordinal,role,content) VALUES('orphan-restore',0,'user','restore content')`)
	require.NoError(t, err)
	require.NoError(t, EmbedBatch(t.Context(), f.runtime, gen, encode, 128, 1, false))
	assert.Equal(t, 1, calls)
	require.NoError(t, gate.Rollback())
	require.NoError(t, <-orphanDone)
	require.NoError(t, EmbedBatch(t.Context(), f.runtime, gen, encode, 128, 1, false))
	assert.Equal(t, 2, calls)
}

func TestHostedVectorStopsPassOnEndpointFailure(t *testing.T) {
	f := newHostedFixture(t, "tenant-endpoint-failure")
	_, err := f.admin.ExecContext(t.Context(), `CREATE EXTENSION IF NOT EXISTS vector SCHEMA public`)
	require.NoError(t, err)
	gen := storage.VectorGenerationInfo{Fingerprint: "endpoint-failure", Model: "fixture", Dimension: 2}
	require.NoError(t, ProvisionHostedVector(t.Context(), f.admin, f.schema, f.tenant, gen))
	id, _, ok, err := LookupVectorGeneration(t.Context(), f.runtime, gen.Fingerprint)
	require.NoError(t, err)
	require.True(t, ok)
	_, err = f.admin.ExecContext(t.Context(), `GRANT SELECT,INSERT,DELETE ON `+vectorChunkTable(id)+` TO "`+f.role+`"`)
	require.NoError(t, err)
	for i, tc := range []struct {
		name string
		err  error
	}{
		{"server error", &embedclient.APIError{StatusCode: 503, Reason: embedclient.ReasonUnavailable}},
		{"HTTP timeout", &embedclient.APIError{StatusCode: 408, Reason: embedclient.ReasonUnavailable}},
		{"request timeout", context.DeadlineExceeded},
		{"network timeout", &net.DNSError{IsTimeout: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first, later := fmt.Sprintf("a%d", i), fmt.Sprintf("b%d", i)
			_, err := f.runtime.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent,provenance_kind,raw_content_revision) VALUES($1,'p','m','codex','raw','rev'),($2,'p','m','codex','raw','rev')`, first, later)
			require.NoError(t, err)
			_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO messages(session_id,ordinal,role,content) VALUES($1,0,'user','first'),($2,0,'user','later')`, first, later)
			require.NoError(t, err)
			var calls []string
			failing := true
			encode := func(_ context.Context, texts []string) ([][]float32, error) {
				calls = append(calls, texts...)
				if failing && slices.Contains(texts, "first") {
					return nil, tc.err
				}
				out := make([][]float32, len(texts))
				for i := range out {
					out[i] = []float32{1, 0}
				}
				return out, nil
			}
			require.Error(t, EmbedBatch(t.Context(), f.runtime, gen, encode, 128, 1, false))
			assert.Equal(t, []string{"first"}, calls)
			calls = nil
			failing = false
			require.NoError(t, EmbedBatch(t.Context(), f.runtime, gen, encode, 128, 1, false))
			assert.Equal(t, []string{"first", "later"}, calls)
		})
	}
}

func TestHostedVectorBatchRecovery(t *testing.T) {
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
	for i := range 17 {
		session := fmt.Sprintf("batch-%02d", i)
		_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent,provenance_kind,raw_content_revision) VALUES($1,'p','m','codex','raw','r1')`, session)
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
				return nil, &embedclient.APIError{StatusCode: 429, Reason: embedclient.ReasonRateLimited}
			}
			out[i] = []float32{1, 0}
		}
		return out, nil
	}
	oldGen := storage.VectorGenerationInfo{Fingerprint: "batch-old", Model: "fixture", Dimension: 2}
	require.NoError(t, ProvisionHostedVector(t.Context(), f.admin, f.schema, f.tenant, oldGen))
	oldID, _, ok, err := LookupVectorGeneration(t.Context(), f.runtime, oldGen.Fingerprint)
	require.NoError(t, err)
	require.True(t, ok)
	_, err = f.admin.ExecContext(t.Context(), `GRANT SELECT,DELETE ON `+vectorChunkTable(oldID)+` TO "`+f.role+`"`)
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO vector_push_state(generation_id,session_id,doc_agg_hash) VALUES($1,'old-orphan','old')`, oldID)
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO vector_push_state(generation_id,session_id,doc_agg_hash) VALUES($1,'orphan','old')`, id)
	require.NoError(t, err)
	err = EmbedBatch(t.Context(), f.runtime, gen, encode, 128, 1, false)
	require.Error(t, err)
	for i := range 17 {
		session := fmt.Sprintf("batch-%02d", i)
		assert.Equal(t, 1, calls[session])
		var stamped bool
		require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM vector_push_state WHERE generation_id=$1 AND session_id=$2)`, id, session).Scan(&stamped))
		assert.Equal(t, i != 0, stamped, session)
	}
	var orphan bool
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM vector_push_state WHERE session_id IN ('orphan','old-orphan'))`).Scan(&orphan))
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
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE sessions SET raw_content_revision='r2' WHERE id IN ('batch-00','batch-01','batch-02','batch-03','batch-04','batch-05'); UPDATE messages SET content=content || '-edited' WHERE session_id IN ('batch-00','batch-01','batch-02','batch-03','batch-04','batch-05')`)
	require.NoError(t, err)
	encode = func(context.Context, []string) ([][]float32, error) {
		return nil, &embedclient.APIError{StatusCode: 429, Reason: embedclient.ReasonRateLimited}
	}
	err = EmbedBatch(t.Context(), f.runtime, gen, encode, 128, 1, false)
	require.ErrorContains(t, err, "6 hosted sessions failed embedding")
	for i := range 6 {
		id := fmt.Sprintf("batch-%02d", i)
		if i < 3 {
			assert.ErrorContains(t, err, id)
		} else {
			assert.NotContains(t, err.Error(), id)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cancelCalls := 0
	encode = func(context.Context, []string) ([][]float32, error) {
		cancelCalls++
		cancel()
		return nil, context.Canceled
	}
	err = EmbedBatch(ctx, f.runtime, gen, encode, 128, 1, false)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, cancelCalls)
}

func TestHostedVectorCoverageIsGenerationSpecific(t *testing.T) {
	f := newHostedFixture(t, "tenant-partial")
	_, err := f.admin.ExecContext(t.Context(), `CREATE EXTENSION IF NOT EXISTS vector SCHEMA public`)
	require.NoError(t, err)
	gen := storage.VectorGenerationInfo{Fingerprint: "partial-fixture", Model: "fixture", Dimension: 2}
	other := storage.VectorGenerationInfo{Fingerprint: "partial-other-generation", Model: "fixture", Dimension: 2}
	for _, g := range []storage.VectorGenerationInfo{gen, other} {
		require.NoError(t, ProvisionHostedVector(t.Context(), f.admin, f.schema, f.tenant, g))
		id, _, ok, err := LookupVectorGeneration(t.Context(), f.runtime, g.Fingerprint)
		require.NoError(t, err)
		require.True(t, ok)
		_, err = f.admin.ExecContext(t.Context(), `GRANT SELECT,INSERT,DELETE ON `+vectorChunkTable(id)+` TO "`+f.role+`"`)
		require.NoError(t, err)
	}
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent,provenance_kind,raw_content_revision) VALUES('session','p','m','codex','raw','rev'); INSERT INTO messages(session_id,ordinal,role,content) VALUES('session',0,'user','first'),('session',1,'user','last')`)
	require.NoError(t, err)
	var calls []string
	encode := func(_ context.Context, texts []string) ([][]float32, error) {
		calls = append(calls, texts...)
		out := make([][]float32, len(texts))
		for i := range out {
			out[i] = []float32{1, 0}
		}
		return out, nil
	}
	require.NoError(t, EmbedBatch(t.Context(), f.runtime, gen, encode, 128, 1, false))
	calls = nil
	require.NoError(t, EmbedBatch(t.Context(), f.runtime, other, encode, 128, 1, false))
	assert.Equal(t, []string{"first", "last"}, calls)
}

func TestHostedVectorSpoolCleanupOnFailures(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	f := newHostedFixture(t, "tenant-spool-failure")
	_, err := f.admin.ExecContext(t.Context(), `CREATE EXTENSION IF NOT EXISTS vector SCHEMA public`)
	require.NoError(t, err)
	gen := storage.VectorGenerationInfo{Fingerprint: "spool-failure", Model: "fixture", Dimension: 2}
	require.NoError(t, ProvisionHostedVector(t.Context(), f.admin, f.schema, f.tenant, gen))
	id, _, ok, err := LookupVectorGeneration(t.Context(), f.runtime, gen.Fingerprint)
	require.NoError(t, err)
	require.True(t, ok)
	_, err = f.admin.ExecContext(t.Context(), `GRANT SELECT,INSERT,DELETE ON `+vectorChunkTable(id)+` TO "`+f.role+`"`)
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent,provenance_kind,raw_content_revision) VALUES('session','p','m','codex','raw','rev'); INSERT INTO messages(session_id,ordinal,role,content) VALUES('session',0,'user',repeat('content ',600))`)
	require.NoError(t, err)
	encode := func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i := range out {
			out[i] = []float32{1, 0}
		}
		return out, nil
	}
	require.NoError(t, EmbedBatch(t.Context(), f.runtime, gen, encode, 20, 1, false))
	_, err = f.admin.ExecContext(t.Context(), `REVOKE INSERT ON `+vectorChunkTable(id)+` FROM "`+f.role+`"`)
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE sessions SET raw_content_revision='rev2' WHERE id='session'; UPDATE messages SET content=repeat('changed ',600) WHERE session_id='session' AND ordinal=0`)
	require.NoError(t, err)
	require.ErrorContains(t, EmbedBatch(t.Context(), f.runtime, gen, encode, 20, 1, true), "permission denied")
	var stamp string
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT doc_agg_hash FROM vector_push_state WHERE generation_id=$1 AND session_id='session'`, id).Scan(&stamp))
	assert.Equal(t, "rev|false", stamp)
	var count int
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM `+vectorChunkTable(id)).Scan(&count))
	assert.Greater(t, count, 200)
	files, err := filepath.Glob(filepath.Join(dir, "agentsview-vectors-*"))
	require.NoError(t, err)
	assert.Empty(t, files)
	t.Setenv("TMPDIR", filepath.Join(dir, "missing"))
	require.ErrorContains(t, EmbedBatch(t.Context(), f.runtime, gen, encode, 20, 1, true), "create vector spool")
}

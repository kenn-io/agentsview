package sessionmaterialize

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCodexLineageIncludesParentThreadAndCompressedPage(t *testing.T) {
	const parent = "11111111-1111-1111-1111-111111111111"
	const thread = "22222222-2222-2222-2222-222222222222"
	const page = "33333333-3333-3333-3333-333333333333"
	parentName := "rollout-2026-01-01T01-00-00-" + parent + ".jsonl"
	firstName := "rollout-2026-01-02T01-00-00-" + thread + ".jsonl"
	newName := "rollout-2026-01-03T01-00-00-" + thread + "_" + page + ".jsonl.zst"
	parentBytes := []byte(fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"history_mode":"paginated"}}`+"\n", parent))
	firstBytes := []byte(fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"history_mode":"paginated","history_base":{"thread_id":%q,"end_ordinal_exclusive":1,"end_byte_offset":1}}}`+"\n", thread, parent))
	last := []byte(fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"history_mode":"paginated","history_base":{"thread_id":%q,"end_ordinal_exclusive":2,"end_byte_offset":1}}}`+"\n", thread, thread))
	encoder, err := zstd.NewWriter(nil)
	require.NoError(t, err)
	compressed := encoder.EncodeAll(last, nil)
	encoder.Close()
	files := map[string][]byte{parentName: parentBytes, firstName: firstBytes, newName: compressed}
	read := func(_ context.Context, name string, limit int64) ([]byte, error) {
		data := files[name]
		if int64(len(data)) > limit {
			return nil, errors.New("limit")
		}
		return data, nil
	}
	names := []string{parentName, firstName, newName}
	pages, err := codexLineage(t.Context(), thread, names, read)
	require.NoError(t, err)
	require.Len(t, pages, 3)
	assert.Equal(t, []string{parentName, firstName, newName}, []string{pages[0].name, pages[1].name, pages[2].name})
	assert.Equal(t, compressed, pages[2].data)
	plainName := strings.TrimSuffix(newName, ".zst")
	files[plainName] = last
	selected, err := codexLineage(t.Context(), thread, append(names, plainName), read)
	require.NoError(t, err)
	assert.Equal(t, plainName, selected[len(selected)-1].name)
	// Native ranks prefer live even when archived is plain and live is compressed.
	live := "sessions/" + newName
	archive := "archived_sessions/" + plainName
	files[live] = compressed
	files[archive] = []byte("stale corrupt copy")
	selected, err = codexLineage(t.Context(), thread, []string{parentName, firstName, archive, live}, read)
	require.NoError(t, err)
	assert.Equal(t, live, selected[len(selected)-1].name)
	duplicate := "sessions/other/" + newName
	files[duplicate] = compressed
	_, err = codexLineage(t.Context(), thread, []string{parentName, firstName, live, duplicate}, read)
	require.ErrorContains(t, err, "ambiguous")
	files[live] = []byte("corrupt")
	files[archive] = last
	_, err = codexLineage(t.Context(), thread, []string{parentName, firstName, live, archive}, read)
	require.ErrorContains(t, err, "metadata")
	delete(files, live)
	delete(files, archive)
	delete(files, duplicate)
	_, err = codexLineage(t.Context(), thread, append(names, "rollout-invalid-"+thread+".jsonl"), read)
	require.NoError(t, err)
	_, err = codexLineage(t.Context(), thread, []string{"rollout-invalid-" + thread + ".jsonl"}, read)
	require.ErrorContains(t, err, "date")
	files[firstName] = []byte(fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"history_mode":"paginated","history_base":{"thread_id":%q,"end_ordinal_exclusive":1,"end_byte_offset":9999}}}`+"\n", thread, parent))
	_, err = codexLineage(t.Context(), thread, names, read)
	require.ErrorContains(t, err, "cutoff")
	files[firstName] = firstBytes
	files[parentName] = []byte(fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q}}`+"\n", parent))
	_, err = codexLineage(t.Context(), thread, names, read)
	require.ErrorContains(t, err, "cutoff")
	files[parentName] = parentBytes
	unrelated := "rollout-2026-01-01T00-00-00-44444444-4444-4444-4444-444444444444.jsonl"
	_, err = codexLineage(t.Context(), thread, append(names, unrelated, "other/"+unrelated), read)
	require.NoError(t, err)
	_, err = codexLineage(t.Context(), thread, names[1:], read)
	require.ErrorContains(t, err, "missing Codex ancestor")
	files[parentName] = []byte(fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"history_mode":"paginated","history_base":{"thread_id":%q,"end_ordinal_exclusive":1,"end_byte_offset":1}}}`+"\n", parent, page))
	_, err = codexLineage(t.Context(), thread, names, read)
	require.ErrorContains(t, err, "cycle")
}

func TestCodexLegacyFilenameAndInvalidLineage(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	name := "rollout-20240115-" + id + ".jsonl"
	date, err := codexDate(name)
	require.NoError(t, err)
	assert.Equal(t, "2024/01/15", date)
	date, err = codexDate("2024/01/16/rollout-invalid-" + id + ".jsonl")
	require.NoError(t, err)
	assert.Equal(t, "2024/01/16", date)
	_, err = codexDate("rollout-invalid-" + id + ".jsonl")
	require.ErrorContains(t, err, "date")
	_, err = inspectCodexPage(t.Context(), name, []byte(`{"type":"session_meta","payload":{"id":"22222222-2222-2222-2222-222222222222"}}`))
	require.ErrorContains(t, err, "identity")
	data := []byte(fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q}}`+"\n", id))
	page, err := inspectCodexPage(t.Context(), name, data)
	require.NoError(t, err)
	assert.Equal(t, data, page.data)
	revert := "22222222-2222-4222-8222-222222222222"
	revertName := "rollout-2024-01-15T00-00-00-" + id + "_" + revert + ".jsonl"
	legacy := []byte(fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"session_id":%q}}`, revert, id))
	_, err = inspectCodexPage(t.Context(), revertName, legacy)
	require.NoError(t, err)
	for _, mode := range []string{`null`, `""`, `"future"`, `1`} {
		_, err := inspectCodexPage(t.Context(), name, []byte(fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"history_mode":%s}}`, id, mode)))
		require.ErrorContains(t, err, "history mode")
	}
	_, err = inspectCodexPage(t.Context(), "rollout-20240115-"+id+"_"+revert+".jsonl", legacy)
	require.ErrorContains(t, err, "canonical")
	paginated := []byte(fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"session_id":%q,"history_mode":"paginated"}}`, revert, id))
	_, err = codexLineage(t.Context(), id, []string{revertName}, func(context.Context, string, int64) ([]byte, error) { return paginated, nil })
	require.ErrorContains(t, err, "identity")
	canonical := "rollout-2024-01-15T00-00-00-" + id + ".jsonl"
	pages, err := codexLineage(t.Context(), id, []string{name, canonical}, func(context.Context, string, int64) ([]byte, error) { return data, nil })
	require.NoError(t, err)
	assert.Equal(t, canonical, pages[0].name)
	later := "rollout-2024-01-15T03-00-00-" + id + ".jsonl"
	pages, err = codexLineage(t.Context(), id, []string{canonical, later}, func(context.Context, string, int64) ([]byte, error) { return data, nil })
	require.NoError(t, err)
	assert.Equal(t, later, pages[0].name)
	tied := "rollout-2024-01-15T00-00-00-" + id + "_" + revert + ".jsonl"
	pages, err = codexLineage(t.Context(), id, []string{canonical, tied}, func(context.Context, string, int64) ([]byte, error) { return data, nil })
	require.NoError(t, err)
	assert.Equal(t, tied, pages[0].name)
}

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

func TestCodexLineageRejectsWrongIdentityAndCutoff(t *testing.T) {
	const id = "11111111-1111-1111-1111-111111111111"
	name := "rollout-2026-01-01T00-00-00-" + id + ".jsonl"
	_, err := inspectCodexPage(t.Context(), name, []byte(`{"type":"session_meta","payload":{"id":"22222222-2222-2222-2222-222222222222"}}`))
	require.ErrorContains(t, err, "identity")
	date, err := codexDate(name)
	require.NoError(t, err)
	assert.Equal(t, "2026/01/01", date)
	_, err = codexDate(strings.Replace(name, "01-01T", "99-01T", 1))
	require.ErrorContains(t, err, "date")
}

func TestCodexLegacyFilenameAndInvalidLineage(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	name := "2024/01/15/rollout-20240115-" + id + ".jsonl"
	date, err := codexDate(name)
	require.NoError(t, err)
	assert.Equal(t, "2024/01/15", date)
	data := []byte(fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q}}`+"\n", id))
	page, err := inspectCodexPage(t.Context(), name, data)
	require.NoError(t, err)
	assert.Equal(t, data, page.data)
	revert := "22222222-2222-4222-8222-222222222222"
	revertName := "rollout-20240115-" + id + "_" + revert + ".jsonl"
	legacy := []byte(fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"session_id":%q}}`, revert, id))
	_, err = inspectCodexPage(t.Context(), revertName, legacy)
	require.NoError(t, err)
	_, err = codexLineage(t.Context(), id, []string{name, "elsewhere/" + name}, func(context.Context, string, int64) ([]byte, error) { return data, nil })
	require.ErrorContains(t, err, "ambiguous")
}

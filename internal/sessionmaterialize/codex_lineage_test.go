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
	firstBytes := []byte(fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"history_mode":"paginated","history_base":{"thread_id":%q,"end_ordinal_exclusive":1,"end_byte_offset":%d}}}`+"\n", thread, parent, len(parentBytes)))
	last := []byte(fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"history_mode":"paginated","history_base":{"thread_id":%q,"end_ordinal_exclusive":1,"end_byte_offset":%d}}}`+"\n", thread, thread, len(firstBytes)))
	encoder, err := zstd.NewWriter(nil)
	require.NoError(t, err)
	compressed := encoder.EncodeAll(last, nil)
	compressedCorrupt := encoder.EncodeAll([]byte("corrupt\n"), nil)
	encoder.Close()
	plain := strings.TrimSuffix(newName, ".zst")
	live, archive := "sessions/"+newName, "archived_sessions/"+strings.Replace(plain, "2026-01-03", "2026-01-04", 1)
	unrelated := "rollout-2026-01-01T00-00-00-44444444-4444-4444-4444-444444444444.jsonl"
	type fixture struct {
		names    []string
		files    map[string][]byte
		archived map[string]bool
	}
	for _, tc := range []struct {
		name          string
		change        func(*fixture)
		want, failure string
	}{
		{"compressed chain", nil, newName, ""},
		{"plain before compressed", func(f *fixture) { f.names = append(f.names, plain); f.files[plain] = last }, plain, ""},
		{"live before archive", func(f *fixture) {
			f.names = []string{parentName, firstName, archive, live}
			f.files[live] = compressed
			f.files[archive] = []byte("corrupt")
			f.archived[archive] = true
		}, live, ""},
		{"equal rank refuses", func(f *fixture) {
			duplicate := "sessions/other/" + newName
			f.names = []string{parentName, firstName, live, duplicate}
			f.files[live] = compressed
			f.files[duplicate] = compressed
		}, "", "ambiguous"},
		{"corrupt preferred refuses", func(f *fixture) {
			f.names = []string{parentName, firstName, live, archive}
			f.files[live] = compressedCorrupt
			f.files[archive] = last
			f.archived[archive] = true
		}, "", "metadata"},
		{"invalid date ignored", func(f *fixture) { f.names = append(f.names, "rollout-invalid-"+thread+".jsonl") }, newName, ""},
		{"invalid date alone", func(f *fixture) { f.names = []string{"rollout-invalid-" + thread + ".jsonl"} }, "", "date"},
		{"cutoff exceeds decoded size", func(f *fixture) {
			f.files[firstName] = []byte(fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"history_mode":"paginated","history_base":{"thread_id":%q,"end_ordinal_exclusive":1,"end_byte_offset":9999}}}`+"\n", thread, parent))
		}, "", "incomplete or incompatible codex history"},
		{"ancestor not paginated", func(f *fixture) {
			f.files[parentName] = []byte(fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q}}`, parent) + strings.Repeat(" ", len(parentBytes)) + "\n")
		}, "", "incomplete or incompatible codex history"},
		{"unrelated twins", func(f *fixture) { f.names = append(f.names, unrelated, "other/"+unrelated) }, newName, ""},
		{"missing ancestor", func(f *fixture) { f.names = f.names[1:] }, "", "missing Codex ancestor"},
		{"cycle", func(f *fixture) {
			f.files[parentName] = []byte(fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"history_mode":"paginated","history_base":{"thread_id":%q,"end_ordinal_exclusive":1,"end_byte_offset":1}}}`+"\n", parent, page))
		}, "", "loop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fixture{names: []string{parentName, firstName, newName}, files: map[string][]byte{parentName: parentBytes, firstName: firstBytes, newName: compressed}, archived: map[string]bool{}}
			if tc.change != nil {
				tc.change(&f)
			}
			read := func(_ context.Context, name string, limit int64) ([]byte, error) {
				data := f.files[name]
				if int64(len(data)) > limit {
					return nil, errors.New("limit")
				}
				return data, nil
			}
			pages, err := codexLineage(t.Context(), thread, f.names, nil, f.archived, read, nil)
			if tc.failure != "" {
				require.ErrorContains(t, err, tc.failure)
				return
			}
			require.NoError(t, err)
			require.Len(t, pages, 3)
			assert.Equal(t, []string{parentName, firstName, tc.want}, []string{pages[0].name, pages[1].name, pages[2].name})
			if tc.name == "compressed chain" {
				assert.Equal(t, compressed, pages[2].data)
			}
		})
	}
}

func TestCodexLegacyFilenameAndInvalidLineage(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	name := "rollout-20240115-" + id + ".jsonl"
	date, _, err := codexDate(name)
	require.NoError(t, err)
	assert.Equal(t, "2024/01/15", date)
	date, _, err = codexDate("2024/01/16/rollout-invalid-" + id + ".jsonl")
	require.NoError(t, err)
	assert.Equal(t, "2024/01/16", date)
	_, _, err = codexDate("rollout-invalid-" + id + ".jsonl")
	require.ErrorContains(t, err, "date")
	_, err = inspectCodexPage(t.Context(), name, []byte(`{"type":"session_meta","payload":{"id":"22222222-2222-2222-2222-222222222222"}}`))
	require.ErrorContains(t, err, "identity")
	data := []byte(fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q}}`+"\n", id))
	page, err := inspectCodexPage(t.Context(), name, data)
	require.NoError(t, err)
	assert.Equal(t, data, page.data)
	revert := "22222222-2222-4222-8222-222222222222"
	revertName := "rollout-2024-01-15T00-00-00-" + id + "_" + revert + ".jsonl"
	legacy := []byte(fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"session_id":%q}}`+"\n", revert, id))
	_, err = inspectCodexPage(t.Context(), revertName, legacy)
	require.NoError(t, err)
	wrongSession := []byte(fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"session_id":%q}}`+"\n", revert, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"))
	_, err = inspectCodexPage(t.Context(), revertName, wrongSession)
	require.ErrorContains(t, err, "identity")
	for _, mode := range []string{`null`, `""`, `"future"`, `1`} {
		_, err := inspectCodexPage(t.Context(), name, []byte(fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"history_mode":%s}}`, id, mode)))
		require.ErrorContains(t, err, "history mode")
	}
	_, err = inspectCodexPage(t.Context(), "rollout-20240115-"+id+"_"+revert+".jsonl", legacy)
	require.ErrorContains(t, err, "timestamped filenames")
	paginated := []byte(fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"session_id":%q,"history_mode":"paginated"}}`+"\n", revert, id))
	_, err = codexLineage(t.Context(), id, []string{revertName}, nil, nil, func(context.Context, string, int64) ([]byte, error) { return paginated, nil }, nil)
	require.ErrorContains(t, err, "identity")
	canonical := "rollout-2024-01-15T00-00-00-" + id + "_" + revert + ".jsonl"
	later := "rollout-2024-01-15T03-00-00-" + id + "_33333333-3333-4333-8333-333333333333.jsonl"
	tied := "rollout-2024-01-15T00-00-00-" + id + "_33333333-3333-4333-8333-333333333333.jsonl"
	for _, tc := range []struct {
		name  string
		names []string
		want  string
	}{
		{"canonical over legacy", []string{name, canonical}, canonical},
		{"later timestamp", []string{canonical, later}, later},
		{"tied timestamp", []string{canonical, tied}, tied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pages, err := codexLineage(t.Context(), id, tc.names, nil, nil, func(context.Context, string, int64) ([]byte, error) { return data, nil }, nil)
			require.NoError(t, err)
			require.Len(t, pages, 1)
			assert.Equal(t, tc.want, pages[0].name)
		})
	}
}

func TestCodexLineageCompanionAuthority(t *testing.T) {
	const parent = "11111111-1111-4111-8111-111111111111"
	const thread = "22222222-2222-4222-8222-222222222222"
	parentName := "rollout-2024-01-15T00-00-00-" + parent + ".jsonl"
	pageName := "rollout-2026-01-01T00-00-00-" + thread + ".jsonl"
	parentBytes := []byte(fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"history_mode":"paginated"}}`+"\n", parent))
	pageBytes := []byte(fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"history_mode":"paginated","history_base":{"thread_id":%q,"end_ordinal_exclusive":1,"end_byte_offset":%d}}}`+"\n", thread, parent, len(parentBytes)))
	for _, tc := range []struct {
		name       string
		roots      []string
		companions []string
		failure    string
	}{
		{"companion supplies ancestor", []string{pageName}, []string{parentName}, ""},
		{"root wins", []string{pageName, parentName}, []string{"other/" + parentName}, ""},
		{"ambiguous root refuses", []string{pageName, parentName, "root/" + parentName}, []string{"other/" + parentName}, "ambiguous"},
		{"newer requested companion ignored", []string{pageName}, []string{parentName, "rollout-2027-01-01T00-00-00-" + thread + ".jsonl"}, ""},
		{"plain companion wins", []string{pageName}, []string{parentName + ".zst", parentName}, ""},
		{"equal companion rank refuses", []string{pageName}, []string{parentName, "other/" + parentName}, "ambiguous"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pages, err := codexLineage(t.Context(), thread, tc.roots, tc.companions, nil, func(_ context.Context, name string, _ int64) ([]byte, error) {
				if name == pageName {
					return pageBytes, nil
				}
				if name == parentName {
					return parentBytes, nil
				}
				return []byte("corrupt alternative"), nil
			}, func(context.Context, string, string) ([]string, error) {
				assert.Fail(t, "resolver called for an available rollout")
				return nil, nil
			})
			if tc.failure != "" {
				require.ErrorContains(t, err, tc.failure)
				return
			}
			require.NoError(t, err)
			require.Len(t, pages, 2)
			assert.Equal(t, parentName, pages[0].name)
			assert.Equal(t, pageName, pages[1].name)
		})
	}
}

func TestCodexLineageLazyAncestry(t *testing.T) {
	const parent = "11111111-1111-4111-8111-111111111111"
	const thread = "22222222-2222-4222-8222-222222222222"
	parentName := "rollout-2024-01-15T00-00-00-" + parent + ".jsonl"
	pageName := "rollout-2026-01-01T00-00-00-" + thread + ".jsonl"
	parentBytes := []byte(fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"history_mode":"paginated"}}`+"\n", parent))
	pageBytes := []byte(fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"history_mode":"paginated","history_base":{"thread_id":%q,"end_ordinal_exclusive":1,"end_byte_offset":%d}}}`+"\n", thread, parent, len(parentBytes)))
	read := func(_ context.Context, name string, _ int64) ([]byte, error) {
		if name == pageName {
			return pageBytes, nil
		}
		return parentBytes, nil
	}
	for _, tc := range []struct {
		name                        string
		roots, companions, returned []string
		failure                     string
		calls                       int
	}{
		{"lazy lookup", []string{pageName}, nil, []string{parentName}, "", 1},
		{"missing", []string{pageName}, nil, nil, "missing Codex ancestor", 1},
		{"ambiguous", []string{pageName}, nil, []string{parentName, "other/" + parentName}, "ambiguous Codex rollout", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			pages, err := codexLineage(t.Context(), thread, tc.roots, tc.companions, nil, read, func(_ context.Context, from, rollout string) ([]string, error) {
				calls++
				assert.Equal(t, pageName, from)
				assert.Equal(t, parent, rollout)
				return tc.returned, nil
			})
			assert.Equal(t, tc.calls, calls)
			if tc.failure != "" {
				require.ErrorContains(t, err, tc.failure)
				return
			}
			require.NoError(t, err)
			require.Len(t, pages, 2)
			assert.Equal(t, parentName, pages[0].name)
		})
	}
	want := errors.New("catalog failure")
	_, err := codexLineage(t.Context(), thread, []string{pageName}, nil, nil, read, func(context.Context, string, string) ([]string, error) { return nil, want })
	require.ErrorIs(t, err, want)
	cycleBytes := []byte(fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"history_mode":"paginated","history_base":{"thread_id":%q,"end_ordinal_exclusive":1,"end_byte_offset":1}}}`+"\n", parent, parent))
	_, err = codexLineage(t.Context(), thread, []string{pageName}, nil, nil, func(_ context.Context, name string, _ int64) ([]byte, error) {
		if name == pageName {
			return pageBytes, nil
		}
		return cycleBytes, nil
	}, func(context.Context, string, string) ([]string, error) { return []string{parentName}, nil })
	require.ErrorContains(t, err, "codex history contains a loop")
}

package db

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These searches exercise the store's choice of query preparation against the
// real optional index. Chinese segmentation must not turn kana or Hangul terms
// into unordered character matches.
func TestCJKFTSJapaneseAndKoreanSearch(t *testing.T) {
	if !simpleFTSRuntimeConfig.available() {
		t.Skip("simple FTS5 runtime is not installed for this test process")
	}
	for _, tc := range []struct {
		name  string
		query string
		match string
		miss  string
	}{
		{"hiragana order", "かな", "かなを探します。", "なかを探します。"},
		{"katakana adjacency", "カタカナ", "カタカナを探します。", "カナとカタを探します。"},
		{"kanji and kana", "検索方法を", "検索方法を説明します。", "方法を変えて検索します。"},
		{"quoted kanji", `"検索方法"`, "検索方法を説明します。", "方法を変えて検索します。"},
		{"single kana", "ぬ", "いぬを探します。", "ねこを探します。"},
		{"hangul adjacency", "검색", "검색합니다.", "색상 검토입니다."},
		{"hangul order", "검색", "검색합니다.", "색검합니다."},
		{"single hangul syllable", "검", "검색합니다.", "색상입니다."},
		{"japanese with latin", "SQLite 検索します", "SQLiteで検索します。", "SQLiteで検索し、別の作業をします。"},
		{"korean with latin", "SQLite 검색", "SQLite로 검색합니다.", "SQLite 색상 검토입니다."},
		{"separate japanese terms", "かな カタカナ", "カタカナとかなを探します。", "カタカナを探します。"},
		{"separate korean terms", "검색 기능", "기능을 추가해 검색합니다.", "검색합니다."},
		{"quoted japanese phrase", `"かな カタカナ"`, "かな カタカナを探します。", "カタカナとかなを探します。"},
		{"quoted korean phrase", `"검색 기능"`, "검색 기능을 추가합니다.", "기능을 추가해 검색합니다."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			seedSearchSession(t, d, "match", "proj", [][2]string{{"user", tc.match}})
			seedSearchSession(t, d, "miss", "proj", [][2]string{{"user", tc.miss}})

			results, err := d.Search(context.Background(), SearchFilter{Query: tc.query, Limit: 20})
			require.NoError(t, err)
			require.Len(t, results.Results, 1, "session search must preserve term adjacency and order")
			assert.Equal(t, "match", results.Results[0].SessionID)

			content, err := d.SearchContent(context.Background(), ContentSearchFilter{
				Pattern: tc.query,
				Mode:    "fts",
				Sources: []string{"messages"},
				Limit:   20,
			})
			require.NoError(t, err)
			require.Len(t, content.Matches, 1, "content search must use the same CJK query preparation")
			assert.Equal(t, "match", content.Matches[0].SessionID)
		})
	}
}

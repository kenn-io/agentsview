package parser

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaudeChannelMessages(t *testing.T) {
	content := loadFixture(t, "claude/channel_messages.jsonl")
	for _, incremental := range []bool{false, true} {
		t.Run(strconv.FormatBool(incremental), func(t *testing.T) {
			var messages []ParsedMessage
			if incremental {
				path := createTestFile(t, "channel.jsonl", content)
				offset := strings.Index(content, `{"parentUuid":"aaaaaaaa-0000-4000-8000-000000000011"`)
				require.Positive(t, offset)
				var err error
				messages, _, _, _, err = claudeParseSessionFrom(path, int64(offset), claudeIncrementalScan{startOrdinal: 2, lastEntryUUID: "aaaaaaaa-0000-4000-8000-000000000011", stored: claudeStoredIdentity{entrypoint: "cli"}})
				require.NoError(t, err)
				require.Len(t, messages, 2)
			} else {
				session, full := runClaudeParserTest(t, "channel.jsonl", content)
				assert.Equal(t, 2, session.UserMessageCount)
				require.Len(t, full, 4)
				messages = full[2:]
				assert.Equal(t, 2, messages[0].Ordinal)
			}
			assert.Equal(t, RoleUser, messages[0].Role)
			assert.False(t, messages[0].IsSystem)
			assert.Contains(t, messages[0].Content, "what changed in the last commit?")
			assert.Contains(t, messages[0].Content, `<channel source="telegram"`)
			assert.Equal(t, "The last commit renamed the config loader.", messages[1].Content)
		})
	}
}

func TestClaudeChannelPreviewEnvelopes(t *testing.T) {
	for _, tt := range []struct {
		name, content, preview string
		typed                  bool
	}{
		{name: "quoted angle", content: `<channel source="telegram" user="a > b">question</channel>`, preview: "question"},
		{name: "no attributes", content: `<channel>question</channel>`, preview: "question"},
		{name: "whitespace", content: " \n<channel source=\"telegram\">\n question\n</channel>\n ", preview: "question"},
		{name: "adjacent", content: `<channel>first</channel>` + "\n" + `<channel source="telegram">second</channel>`, preview: "first second"},
		{name: "malformed", content: `<channel source="telegram>question</channel>`, preview: `<channel source="telegram>question</channel>`},
		{name: "trailing text", content: `<channel>question</channel> trailing`, preview: `<channel>question</channel> trailing`},
		{name: "literal body", content: `<channel>a < b & c</channel>`, preview: "a < b & c"},
		{name: "truncation", content: `<channel source="telegram">` + strings.Repeat("question ", 50) + `</channel>`, preview: strings.Repeat("question ", 33) + "que..."},
		{name: "typed", content: `<channel source="telegram">literal text</channel>`, preview: `<channel source="telegram">literal text</channel>`, typed: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			metadata := `"isMeta":true,"origin":{"kind":"channel"},`
			if tt.typed {
				metadata = ""
			}
			content := fmt.Sprintf(`{"type":"user","uuid":"c1",%s"message":{"content":%q}}`+"\n", metadata, tt.content)
			session, _ := runClaudeParserTest(t, "preview.jsonl", content)
			assert.Equal(t, tt.preview, session.FirstMessage)
		})
	}
}

func TestClaudeChannelRewindKeepsRetryIdentity(t *testing.T) {
	content := loadFixture(t, "claude/channel_rewind.jsonl")
	path := createTestFile(t, "rewind.jsonl", content)
	results, err := parseClaudeSession(path, "project", "local")
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, "rewind", results[0].Session.ID)
	require.Len(t, results[0].Messages, 2)
	assert.Equal(t, "try again", results[0].Messages[1].Content)
	assert.Equal(t, 2, results[0].Session.UserMessageCount)
	assert.Equal(t, "rewind-c0", results[1].Session.ID)
	assert.Equal(t, "rewind", results[1].Session.ParentSessionID)
	assert.Equal(t, RelFork, results[1].Session.RelationshipType)
	assert.Equal(t, 4, results[1].Session.UserMessageCount)
	require.Len(t, results[1].Messages, 8)
	assert.Equal(t, "answer 3", results[1].Messages[7].Content)
}

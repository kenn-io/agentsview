package db

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
)

func nativeImageMessage(sessionID string) Message {
	image := testInlineImageContent()
	return Message{
		SessionID: sessionID, Role: "assistant", Content: "answer", ThinkingText: "plan", HasThinking: true,
		ToolResultText: image + "\n界", ContentLength: 200,
		ContentLayout: &parser.ContentLayout{Version: 1, Blocks: []parser.ContentBlock{
			{Kind: "tool_result", End: len(image)},
			{Kind: "text", End: 6},
			{Kind: "tool_result", Start: len(image) + 1, End: len(image) + 4},
			{Kind: "thinking", End: 4},
		}},
	}
}

func assertNativeImageBody(t *testing.T, message Message) string {
	t.Helper()
	assert.Equal(t, "answer", message.Content)
	assert.Equal(t, "plan", message.ThinkingText)
	assert.Equal(t, 200, message.ContentLength)
	require.NotNil(t, message.ContentLayout)
	assert.Equal(t, 1, message.ContentLayout.Version)
	require.Len(t, message.ContentLayout.Blocks, 4)
	blocks := message.ContentLayout.Blocks
	assert.Equal(t, parser.ContentBlock{Kind: "text", End: 6}, blocks[1])
	assert.Equal(t, parser.ContentBlock{Kind: "thinking", End: 4}, blocks[3])
	assert.Equal(t, "tool_result", blocks[0].Kind)
	assert.Equal(t, "tool_result", blocks[2].Kind)
	assert.Zero(t, blocks[0].Start)
	require.GreaterOrEqual(t, blocks[0].End, 0)
	require.LessOrEqual(t, blocks[0].End, len(message.ToolResultText))
	assert.Equal(t, blocks[0].End+1, blocks[2].Start)
	assert.Equal(t, len(message.ToolResultText), blocks[2].End)
	require.GreaterOrEqual(t, blocks[2].Start, 0)
	require.LessOrEqual(t, blocks[2].Start, blocks[2].End)
	require.LessOrEqual(t, blocks[2].End, len(message.ToolResultText))
	assert.Equal(t, "界", message.ToolResultText[blocks[2].Start:blocks[2].End])
	return message.ToolResultText[:blocks[0].End]
}

func TestNativeStandaloneImageWritePolicies(t *testing.T) {
	for _, policy := range []config.ToolResultImages{config.ToolResultImagesKeep, config.ToolResultImagesDrop, config.ToolResultImagesOffload} {
		t.Run(string(policy), func(t *testing.T) {
			d := testDB(t)
			d.SetToolResultImages(policy)
			d.SetAssetsDir(t.TempDir())
			insertSession(t, d, "native-images", "project")
			require.NoError(t, d.InsertMessages(t.Context(), []Message{nativeImageMessage("native-images")}))
			got, err := d.GetAllMessages(t.Context(), "native-images")
			require.NoError(t, err)
			require.Len(t, got, 1)
			image := assertNativeImageBody(t, got[0])
			switch policy {
			case config.ToolResultImagesKeep:
				assert.Equal(t, testInlineImageContent(), image)
			case config.ToolResultImagesDrop:
				assert.NotContains(t, image, "data:image/")
				assert.Contains(t, image, `"type":"agentsview_image"`)
				assert.Contains(t, image, `"text":"before"`)
				assert.Contains(t, image, `"text":"after"`)
			case config.ToolResultImagesOffload:
				assertOffloadedImage(t, image, d.AssetsDir())
			}
		})
	}
}

func TestNativeStandaloneImageMaintenance(t *testing.T) {
	for _, command := range []string{"strip", "migrate"} {
		t.Run(command, func(t *testing.T) {
			d := testDB(t)
			seedArtifactOrigin(t, d)
			assetsDir := t.TempDir()
			insertSession(t, d, "native-maintenance", "project")
			require.NoError(t, d.InsertMessages(t.Context(), []Message{
				nativeImageMessage("native-maintenance"),
				{
					SessionID: "native-maintenance", Ordinal: 1, Role: "assistant", Content: "safe",
					ThinkingText: "plan AKIA7QHWN2DKR4FYPLJM 界", ToolResultText: "output AKIA7QHWN2DKR4FYPLJM 界",
					ToolCalls: []ToolCall{{ToolName: "Bash", Category: "Bash", Rendering: "invoke AKIA7QHWN2DKR4FYPLJM 界"}},
				},
			}))
			preview, err := d.PreviewStripToolImages(t.Context(), StripImagesFilter{})
			if command == "migrate" {
				preview, err = d.PreviewMigrateToolImages(t.Context(), StripImagesFilter{})
			}
			require.NoError(t, err)
			assert.Equal(t, 1, preview.Changed)
			assert.Equal(t, int64(1), preview.Payloads)
			assert.Equal(t, int64(3), preview.DecodedBytes)
			var report StripImagesReport
			if command == "strip" {
				report, err = d.StripToolImages(t.Context(), StripImagesFilter{})
			} else {
				report, err = d.MigrateToolImages(t.Context(), StripImagesFilter{}, realPut(assetsDir))
			}
			require.NoError(t, err)
			assert.Equal(t, 1, report.Changed)
			got, err := d.GetAllMessages(t.Context(), "native-maintenance")
			require.NoError(t, err)
			require.Len(t, got, 2)
			image := assertNativeImageBody(t, got[0])
			assert.NotContains(t, image, "data:image/")
			assert.Contains(t, image, `"type":"agentsview_image"`)
			if command == "migrate" {
				assertOffloadedImage(t, image, assetsDir)
				entries, err := os.ReadDir(assetsDir)
				require.NoError(t, err)
				require.Len(t, entries, 1)
				body, err := os.ReadFile(filepath.Join(assetsDir, entries[0].Name()))
				require.NoError(t, err)
				assert.Equal(t, []byte{0, 1, 2}, body)
			}
			findings, err := d.SessionSecretFindings(t.Context(), "native-maintenance")
			require.NoError(t, err)
			var definite []SecretFinding
			for _, finding := range findings {
				if finding.Confidence == "definite" {
					definite = append(definite, finding)
				}
			}
			require.Len(t, definite, 3)
			var locations []string
			for _, finding := range definite {
				assert.Equal(t, "aws-access-key", finding.RuleName)
				locations = append(locations, finding.LocationKind)
				assert.Equal(t, 1, finding.MessageOrdinal)
				text, ok, err := d.SecretFindingSource(t.Context(), finding)
				require.NoError(t, err)
				require.True(t, ok)
				require.LessOrEqual(t, finding.MatchEnd, len(text))
				assert.Equal(t, "AKIA7QHWN2DKR4FYPLJM", text[finding.MatchStart:finding.MatchEnd])
			}
			assert.ElementsMatch(t, []string{"thinking", "tool_output", "tool_rendering"}, locations)
			preview, err = d.PreviewStripToolImages(t.Context(), StripImagesFilter{})
			if command == "migrate" {
				preview, err = d.PreviewMigrateToolImages(t.Context(), StripImagesFilter{})
			}
			require.NoError(t, err)
			assert.Zero(t, preview.Changed)
		})
	}
}

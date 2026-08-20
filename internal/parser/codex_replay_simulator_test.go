package parser

import (
	"bufio"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/pricing"
)

func TestCodexCapturedForkReplayTotals(t *testing.T) {
	root := os.Getenv("AGENTSVIEW_CODEX_REPLAY_ROOT")
	if root == "" {
		t.Skip("set AGENTSVIEW_CODEX_REPLAY_ROOT to run captured fork replay")
	}

	files, err := filepath.Glob(filepath.Join(root, "*.jsonl"))
	require.NoError(t, err)
	require.Len(t, files, 6)
	children := capturedCodexForkChildren(t, files)
	require.Len(t, children, 5)

	provider := newCodexTestProvider(t, root)
	var totalOutput int
	var totalMessages int
	var totalInput int
	var totalCacheRead int
	var totalCost money.Money
	rows := make([]export.EffectivePricingRow, 0, len(pricing.FallbackPricing()))
	for _, row := range pricing.FallbackPricing() {
		bands := make([]export.PricingBand, len(row.Bands))
		for i, band := range row.Bands {
			bands[i] = export.PricingBand{
				AboveInputTokens:  band.AboveInputTokens,
				InputPerMTok:      band.InputPerMTok,
				OutputPerMTok:     band.OutputPerMTok,
				CacheWritePerMTok: band.CacheCreationPerMTok,
				CacheReadPerMTok:  band.CacheReadPerMTok,
			}
		}
		rows = append(rows, export.EffectivePricingRow{
			ModelPattern: row.ModelPattern,
			Rates: export.ModelRates{
				InputPerMTok:      row.InputPerMTok,
				OutputPerMTok:     row.OutputPerMTok,
				CacheWritePerMTok: row.CacheCreationPerMTok,
				CacheReadPerMTok:  row.CacheReadPerMTok,
				Bands:             bands,
			},
		})
	}
	resolver := export.NewPricingResolver(rows)
	for _, path := range children {
		sess, messages, parseErr := provider.parseSession(path, "local", false)
		require.NoError(t, parseErr)
		for _, message := range messages {
			totalOutput += message.OutputTokens
			if len(message.TokenUsage) != 0 {
				var usage struct {
					Input      int `json:"input_tokens"`
					Output     int `json:"output_tokens"`
					CacheRead  int `json:"cache_read_input_tokens"`
					CacheWrite int `json:"cache_creation_input_tokens"`
				}
				require.NoError(t, json.Unmarshal(message.TokenUsage, &usage))
				totalInput += usage.Input
				totalCacheRead += usage.CacheRead
				lookup := resolver.Lookup(message.Model)
				require.True(t, lookup.OK, "pricing %s", message.Model)
				cost, costErr := lookup.Rates.CostForTokens(
					usage.Input, usage.Output, 0, usage.CacheWrite, usage.CacheRead,
				)
				require.NoError(t, costErr)
				totalCost, costErr = money.Add(totalCost, cost)
				require.NoError(t, costErr)
			}
		}
		totalMessages += len(messages)
		t.Logf("%s: messages=%d output=%d first=%q", filepath.Base(path),
			len(messages), sess.TotalOutputTokens, sess.FirstMessage)
	}

	assert.Equal(t, 96_476, totalOutput)
	assert.Equal(t, 907_715, totalInput)
	assert.Equal(t, 15_941_888, totalCacheRead)
	assert.Equal(t, 171, totalMessages)
	assert.Equal(t, money.Money{Microdollars: 15_403_799}, totalCost)
}

func capturedCodexForkChildren(t *testing.T, files []string) []string {
	t.Helper()
	var children []string
	for _, sourcePath := range files {
		data, err := os.ReadFile(sourcePath)
		require.NoError(t, err)
		firstLine, _, _ := strings.Cut(string(data), "\n")
		if gjson.Get(firstLine, "payload.forked_from_id").Str != "" {
			children = append(children, sourcePath)
		}
	}
	return children
}

func TestCodexCapturedForkLineReplayTotals(t *testing.T) {
	sourceRoot := os.Getenv("AGENTSVIEW_CODEX_REPLAY_ROOT")
	if sourceRoot == "" {
		t.Skip("set AGENTSVIEW_CODEX_REPLAY_ROOT to run captured fork replay")
	}
	files, err := filepath.Glob(filepath.Join(sourceRoot, "*.jsonl"))
	require.NoError(t, err)
	require.Len(t, files, 6)

	replayRoot := t.TempDir()
	children := capturedCodexForkChildren(t, files)
	require.Len(t, children, 5)
	for _, sourcePath := range files {
		data, readErr := os.ReadFile(sourcePath)
		require.NoError(t, readErr)
		targetPath := filepath.Join(replayRoot, filepath.Base(sourcePath))
		firstLine, _, _ := strings.Cut(string(data), "\n")
		if gjson.Get(firstLine, "payload.forked_from_id").Str == "" {
			require.NoError(t, os.WriteFile(targetPath, data, 0o600))
		}
	}

	provider := newCodexTestProvider(t, replayRoot)
	var totalOutput int
	var totalMessages int
	for _, sourcePath := range children {
		targetPath := filepath.Join(replayRoot, filepath.Base(sourcePath))
		source, openErr := os.Open(sourcePath)
		require.NoError(t, openErr)
		scanner := bufio.NewScanner(source)
		scanner.Buffer(make([]byte, 0, 64*1024), maxLineSize)

		var messages []ParsedMessage
		var offset int64
		for scanner.Scan() {
			line := append(append([]byte(nil), scanner.Bytes()...), '\n')
			file, fileErr := os.OpenFile(
				targetPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600,
			)
			require.NoError(t, fileErr)
			_, fileErr = file.Write(line)
			require.NoError(t, fileErr)
			require.NoError(t, file.Close())

			if offset == 0 {
				_, messages, fileErr = provider.parseSession(
					targetPath, "local", false,
				)
				require.NoError(t, fileErr)
				offset = int64(len(line))
				continue
			}

			result, parseErr := provider.parseSessionFromDetailed(
				targetPath, offset, len(messages), false,
			)
			if IsIncrementalFullParseFallback(parseErr) {
				_, messages, parseErr = provider.parseSession(
					targetPath, "local", false,
				)
				require.NoError(t, parseErr)
				offset += int64(len(line))
				continue
			}
			require.NoError(t, parseErr)
			messages = append(messages, result.messages...)
			offset += result.consumedBytes
			provider.cursorCache.Put(
				targetPath, offset, result.inode, result.device, result.cursor,
			)
		}
		require.NoError(t, scanner.Err())
		require.NoError(t, source.Close())
		for _, message := range messages {
			totalOutput += message.OutputTokens
		}
		totalMessages += len(messages)
	}

	assert.Equal(t, 96_476, totalOutput)
	assert.Equal(t, 171, totalMessages)
}

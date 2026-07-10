package parser

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/testjsonl"
)

const (
	codexBenchmarkUUID       = "019eb791-cf7d-75c1-8439-9ed74c122b00"
	codexBenchmarkPriorTurns = 1500
	codexBenchmarkTailUser   = "Tail request: summarize the finished benchmark evidence."
	codexBenchmarkTailAgent  = "Tail response: cursor parsing read only this appended turn."
)

var codexBenchmarkOutcomeSink IncrementalOutcome

// BenchmarkCodexIncrementalCursor measures the same appended tail after either
// reconstructing the cursor from a large persisted prefix or finding the exact
// committed prefix cursor in the factory-owned cache.
func BenchmarkCodexIncrementalCursor(b *testing.B) {
	b.StopTimer()
	ctx := context.Background()
	root, path, prefix, tail, startOrdinal := writeCodexBenchmarkTranscript(b)
	cfg := ProviderConfig{Roots: []string{root}, Machine: "benchmark-host"}

	registeredFactory, ok := ProviderFactoryByType(AgentCodex)
	require.True(b, ok)
	def := registeredFactory.Definition()
	newProvider := func() Provider {
		return newCodexProviderFactory(def).NewProvider(cfg)
	}

	timedWarmProvider := newProvider()
	source, found, err := timedWarmProvider.FindSource(ctx, FindSourceRequest{
		FullSessionID: "codex:" + codexBenchmarkUUID,
	})
	require.NoError(b, err)
	require.True(b, found)

	prefixFingerprint, err := timedWarmProvider.Fingerprint(ctx, source)
	require.NoError(b, err)
	assert.Equal(b, int64(len(prefix)), prefixFingerprint.Size)
	full, err := timedWarmProvider.Parse(ctx, ParseRequest{
		Source:      source,
		Fingerprint: prefixFingerprint,
	})
	require.NoError(b, err)
	require.Len(b, full.Results, 1)
	assert.Len(b, full.Results[0].Result.Messages, startOrdinal)
	timedConcrete, ok := timedWarmProvider.(*codexProvider)
	require.True(b, ok)
	_, timedPrefixSeeded := timedConcrete.cursorCache.Get(
		path,
		prefixFingerprint.Size,
		prefixFingerprint.Inode,
		prefixFingerprint.Device,
	)
	require.True(b, timedPrefixSeeded)

	// Validate warm output through a separately seeded provider so the timed
	// provider cannot acquire its prefix cursor from a preflight tail parse.
	validationWarmProvider := newProvider()
	_, err = validationWarmProvider.Parse(ctx, ParseRequest{
		Source:      source,
		Fingerprint: prefixFingerprint,
	})
	require.NoError(b, err)

	appendCodexBenchmarkTail(b, path, tail)
	currentFingerprint, err := timedWarmProvider.Fingerprint(ctx, source)
	require.NoError(b, err)
	assert.Equal(b, int64(len(prefix)+len(tail)), currentFingerprint.Size)
	req := IncrementalRequest{
		Source:       source,
		Fingerprint:  currentFingerprint,
		SessionID:    "codex:" + codexBenchmarkUUID,
		Offset:       int64(len(prefix)),
		StartOrdinal: startOrdinal,
	}

	coldValidationProvider := newProvider()
	coldOutcome, coldStatus, err := coldValidationProvider.ParseIncremental(
		ctx, req,
	)
	requireCodexBenchmarkOutcome(
		b, coldOutcome, coldStatus, err, startOrdinal, int64(len(tail)),
	)
	warmOutcome, warmStatus, err := validationWarmProvider.ParseIncremental(
		ctx, req,
	)
	requireCodexBenchmarkOutcome(
		b, warmOutcome, warmStatus, err, startOrdinal, int64(len(tail)),
	)

	b.Run("cold_prefix_seed", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(prefix)))
		b.ResetTimer()
		for b.Loop() {
			// A new factory is required here: the cursor cache belongs to the
			// factory and a reused factory would turn this into a warm parse.
			provider := newProvider()
			outcome, status, err := provider.ParseIncremental(ctx, req)
			if !codexBenchmarkOutcomeValid(
				outcome, status, err, startOrdinal, int64(len(tail)),
			) {
				b.StopTimer()
				requireCodexBenchmarkOutcome(
					b, outcome, status, err, startOrdinal, int64(len(tail)),
				)
				b.StartTimer()
			}
			codexBenchmarkOutcomeSink = outcome
		}
	})

	b.Run("warm_cursor", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(tail)))
		b.ResetTimer()
		for b.Loop() {
			outcome, status, err := timedWarmProvider.ParseIncremental(ctx, req)
			if !codexBenchmarkOutcomeValid(
				outcome, status, err, startOrdinal, int64(len(tail)),
			) {
				b.StopTimer()
				requireCodexBenchmarkOutcome(
					b, outcome, status, err, startOrdinal, int64(len(tail)),
				)
				b.StartTimer()
			}
			codexBenchmarkOutcomeSink = outcome
		}
	})
}

func writeCodexBenchmarkTranscript(
	b *testing.B,
) (root, path, prefix, tail string, startOrdinal int) {
	b.Helper()
	root = filepath.Join(b.TempDir(), "sessions")
	path = filepath.Join(
		root,
		"2026",
		"07",
		"10",
		"rollout-2026-07-10T07-12-15-"+codexBenchmarkUUID+".jsonl",
	)
	require.NoError(b, os.MkdirAll(filepath.Dir(path), 0o755))

	fixture := testjsonl.NewSessionBuilder().
		AddCodexMeta(
			"2026-07-10T07:00:00Z",
			codexBenchmarkUUID,
			"/workspace/project-a",
			"codex_cli_rs",
		).
		AddRaw(testjsonl.CodexTurnContextJSON(
			"gpt-5.4", "2026-07-10T07:00:01Z",
		)).
		AddCodexMessage(
			"2026-07-10T07:00:02Z",
			"user",
			"Initial request: inspect the project and make a careful change.",
		).
		AddCodexMessage(
			"2026-07-10T07:00:03Z",
			"assistant",
			"Initial response: I will inspect the relevant code and tests.",
		)
	contextPayload := strings.Repeat(
		"Retain concrete code, test, and validation context. ", 8,
	)
	for i := range codexBenchmarkPriorTurns {
		turn := strconv.Itoa(i)
		fixture.AddCodexMessage(
			"2026-07-10T07:01:00Z",
			"user",
			"Prior turn "+turn+" request: continue the implementation. "+
				contextPayload,
		)
		fixture.AddCodexMessage(
			"2026-07-10T07:01:01Z",
			"assistant",
			"Prior turn "+turn+" response: applied the next bounded change. "+
				contextPayload,
		)
	}
	prefix = fixture.String()
	startOrdinal = 2 + 2*codexBenchmarkPriorTurns
	tail = testjsonl.JoinJSONL(
		testjsonl.CodexMsgJSON(
			"user", codexBenchmarkTailUser, "2026-07-10T07:12:15Z",
		),
		testjsonl.CodexMsgJSON(
			"assistant", codexBenchmarkTailAgent, "2026-07-10T07:12:16Z",
		),
	)
	require.NoError(b, os.WriteFile(path, []byte(prefix), 0o644))
	return root, path, prefix, tail, startOrdinal
}

func appendCodexBenchmarkTail(b *testing.B, path, tail string) {
	b.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(b, err)
	_, err = f.WriteString(tail)
	require.NoError(b, err)
	require.NoError(b, f.Close())
}

func requireCodexBenchmarkOutcome(
	b *testing.B,
	outcome IncrementalOutcome,
	status IncrementalStatus,
	err error,
	startOrdinal int,
	tailBytes int64,
) {
	b.Helper()
	require.NoError(b, err)
	require.Equal(b, IncrementalApplied, status)
	require.Len(b, outcome.Messages, 2)
	assert.Equal(b, "codex:"+codexBenchmarkUUID, outcome.SessionID)
	assert.Equal(b, 2, outcome.MessageCount)
	assert.Equal(b, 1, outcome.UserMessageCount)
	assert.Equal(b, tailBytes, outcome.ConsumedBytes)
	assert.Equal(b, RoleUser, outcome.Messages[0].Role)
	assert.Equal(b, codexBenchmarkTailUser, outcome.Messages[0].Content)
	assert.Equal(b, startOrdinal, outcome.Messages[0].Ordinal)
	assert.Equal(b, RoleAssistant, outcome.Messages[1].Role)
	assert.Equal(b, codexBenchmarkTailAgent, outcome.Messages[1].Content)
	assert.Equal(b, startOrdinal+1, outcome.Messages[1].Ordinal)
}

func codexBenchmarkOutcomeValid(
	outcome IncrementalOutcome,
	status IncrementalStatus,
	err error,
	startOrdinal int,
	tailBytes int64,
) bool {
	return err == nil &&
		status == IncrementalApplied &&
		outcome.SessionID == "codex:"+codexBenchmarkUUID &&
		outcome.MessageCount == 2 &&
		outcome.UserMessageCount == 1 &&
		outcome.ConsumedBytes == tailBytes &&
		len(outcome.Messages) == 2 &&
		outcome.Messages[0].Role == RoleUser &&
		outcome.Messages[0].Content == codexBenchmarkTailUser &&
		outcome.Messages[0].Ordinal == startOrdinal &&
		outcome.Messages[1].Role == RoleAssistant &&
		outcome.Messages[1].Content == codexBenchmarkTailAgent &&
		outcome.Messages[1].Ordinal == startOrdinal+1
}

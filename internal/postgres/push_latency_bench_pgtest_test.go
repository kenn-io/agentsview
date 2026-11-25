//go:build pgtest

package postgres

import (
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

// latencyProxy forwards TCP connections to PostgreSQL. It holds each
// client-to-server chunk for delay before forwarding it, so every round trip
// costs at least delay, and it counts a round trip each time the client starts
// sending after the server last spoke. This file uses only APIs that exist on
// the base revision, so the benchmark can run unchanged before and after.
type latencyProxy struct {
	listener   net.Listener
	upstream   string
	delay      time.Duration
	roundTrips atomic.Int64
	wg         sync.WaitGroup
}

func startLatencyProxy(tb testing.TB, pgURL string, delay time.Duration) *latencyProxy {
	tb.Helper()
	parsed, err := url.Parse(pgURL)
	require.NoError(tb, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(tb, err)
	proxy := &latencyProxy{listener: listener, upstream: parsed.Host, delay: delay}
	proxy.wg.Go(proxy.accept)
	tb.Cleanup(func() {
		_ = listener.Close()
		proxy.wg.Wait()
	})
	return proxy
}

func (p *latencyProxy) accept() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		server, err := net.Dial("tcp", p.upstream)
		if err != nil {
			_ = client.Close()
			continue
		}
		p.wg.Go(func() { p.serve(client, server) })
	}
}

type delayedChunk struct {
	due  time.Time
	data []byte
}

func (p *latencyProxy) serve(client, server net.Conn) {
	var serverSpoke atomic.Bool
	serverSpoke.Store(true)
	chunks := make(chan delayedChunk, 4096)
	closeBoth := func() {
		_ = client.Close()
		_ = server.Close()
	}
	var inner sync.WaitGroup
	inner.Go(func() {
		defer close(chunks)
		buf := make([]byte, 64<<10)
		for {
			n, err := client.Read(buf)
			if n > 0 {
				if serverSpoke.Swap(false) {
					p.roundTrips.Add(1)
				}
				chunks <- delayedChunk{due: time.Now().Add(p.delay), data: append([]byte(nil), buf[:n]...)}
			}
			if err != nil {
				return
			}
		}
	})
	inner.Go(func() {
		defer closeBoth()
		for chunk := range chunks {
			time.Sleep(time.Until(chunk.due))
			if _, err := server.Write(chunk.data); err != nil {
				return
			}
		}
	})
	inner.Go(func() {
		defer closeBoth()
		buf := make([]byte, 64<<10)
		for {
			n, err := server.Read(buf)
			if n > 0 {
				serverSpoke.Store(true)
				if _, werr := client.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	})
	inner.Wait()
}

func (p *latencyProxy) url(tb testing.TB, pgURL string) string {
	tb.Helper()
	parsed, err := url.Parse(pgURL)
	require.NoError(tb, err)
	parsed.Host = p.listener.Addr().String()
	return parsed.String()
}

// seedLatencyFixture writes sessions shaped like an ordinary coding transcript:
// alternating user and assistant messages, a tool call with a result on every
// assistant message, and one usage event per session.
func seedLatencyFixture(tb testing.TB, local *db.DB, prefix string, sessions, messages int) {
	tb.Helper()
	started := "2026-10-01T09:00:00Z"
	for session := range sessions {
		id := fmt.Sprintf("%s-%d", prefix, session)
		row := db.Session{ID: id, Machine: "latency", Project: fmt.Sprintf("project-%d", session%10), Agent: "claude", StartedAt: &started, CreatedAt: started, MessageCount: messages, UserMessageCount: (messages + 1) / 2}
		require.NoError(tb, local.UpsertSession(tb.Context(), row))
		batch := make([]db.Message, messages)
		for ordinal := range messages {
			msg := db.Message{SessionID: id, Ordinal: ordinal, Role: "user", Timestamp: started, Content: strings.Repeat("Inspect the query and explain its plan. ", 10)}
			if ordinal%2 == 1 {
				msg.Role = "assistant"
				msg.Model = "model-a"
				msg.HasToolUse = true
				msg.TokenUsage = []byte(`{"input_tokens":1200,"output_tokens":300}`)
				result := "query.go\nplan.go"
				msg.ToolCalls = []db.ToolCall{{ToolName: "Bash", Category: "Bash", ToolUseID: fmt.Sprintf("%s-tool-%d", id, ordinal), InputJSON: `{"command":"ls"}`, ResultContent: result, ResultContentLength: len(result), ResultEvents: []db.ToolResultEvent{{Source: "tool_result", Status: "completed", Content: result, ContentLength: len(result), Timestamp: started}}}}
			}
			batch[ordinal] = msg
		}
		require.NoError(tb, local.InsertMessages(tb.Context(), batch))
		require.NoError(tb, local.ReplaceSessionUsageEvents(tb.Context(), id, []db.UsageEvent{
			{SessionID: id, Source: "provider", Model: "model-a", InputTokens: 1200, OutputTokens: 300, OccurredAt: started, DedupKey: "event-a"},
		}))
	}
}

type latencyPushRun struct {
	result     storage.PushResult
	roundTrips int64
	elapsed    time.Duration
}

// firstPushThroughProxy pushes a freshly seeded archive into a new schema
// through proxy and reports the push alone, without schema setup.
func firstPushThroughProxy(tb testing.TB, pgURL, schema string, proxy *latencyProxy, sessions, messages int) latencyPushRun {
	tb.Helper()
	local, err := db.Open(tb.Context(), filepath.Join(tb.TempDir(), "archive.db"))
	require.NoError(tb, err)
	tb.Cleanup(func() { require.NoError(tb, local.Close()) })
	seedLatencyFixture(tb, local, "latency-session", sessions, messages)

	syncer, err := New(tb.Context(), proxy.url(tb, pgURL), schema, local, "latency", true, storage.PusherOptions{})
	require.NoError(tb, err)
	tb.Cleanup(func() { require.NoError(tb, syncer.Close()) })
	require.NoError(tb, syncer.EnsureSchema(tb.Context()))

	before := proxy.roundTrips.Load()
	start := time.Now()
	result, err := syncer.Push(tb.Context(), true, nil)
	elapsed := time.Since(start)
	require.NoError(tb, err)
	require.Zero(tb, result.Errors)
	require.Equal(tb, sessions, result.SessionsPushed)
	require.Equal(tb, sessions*messages, result.MessagesPushed)
	return latencyPushRun{result: result, roundTrips: proxy.roundTrips.Load() - before, elapsed: elapsed}
}

func dropLatencySchema(tb testing.TB, pgURL, schema string) {
	tb.Helper()
	admin, err := sql.Open("pgx", pgURL)
	require.NoError(tb, err)
	defer admin.Close()
	_, err = admin.ExecContext(tb.Context(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	require.NoError(tb, err)
}

// BenchmarkPGPushFirstLoadLatency measures a first push of new sessions over
// a link that adds latency to every round trip. Run it with -benchtime=1x;
// a higher count repeats the whole first load.
func BenchmarkPGPushFirstLoadLatency(b *testing.B) {
	const sessions, messages = 100, 20
	for _, delay := range []time.Duration{20 * time.Millisecond, 100 * time.Millisecond} {
		b.Run(fmt.Sprintf("latency=%s", delay), func(b *testing.B) {
			pgURL := testPGURL(b)
			proxy := startLatencyProxy(b, pgURL, delay)
			var trips int64
			var elapsed time.Duration
			b.ResetTimer()
			for iteration := range b.N {
				b.StopTimer()
				schema := fmt.Sprintf("push_latency_%d", iteration)
				dropLatencySchema(b, pgURL, schema)
				run := firstPushThroughProxy(b, pgURL, schema, proxy, sessions, messages)
				trips += run.roundTrips
				elapsed += run.elapsed
				dropLatencySchema(b, pgURL, schema)
			}
			b.ReportMetric(0, "ns/op")
			b.ReportMetric(float64(trips)/float64(b.N*sessions), "round-trips/session")
			b.ReportMetric(float64(b.N*sessions)/elapsed.Minutes(), "sessions/min")
			b.ReportMetric(elapsed.Seconds()/float64(b.N), "push-s/op")
		})
	}
}

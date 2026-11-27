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
	mu         sync.Mutex
	conns      []net.Conn
}

func startLatencyProxy(tb testing.TB, pgURL string, delay time.Duration) *latencyProxy {
	tb.Helper()
	parsed, err := url.Parse(pgURL)
	require.NoError(tb, err)
	if parsed.Hostname() == "" || parsed.Query().Has("host") || parsed.Query().Has("port") {
		tb.Skip("the latency proxy needs the server address in the URL authority")
	}
	if mode := parsed.Query().Get("sslmode"); mode == "verify-ca" || mode == "verify-full" {
		// Clients reach the proxy by its loopback address, so certificate
		// checks against the server's name would fail.
		tb.Skip("the latency proxy cannot preserve TLS server verification")
	}
	port := parsed.Port()
	if port == "" {
		port = "5432"
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(tb, err)
	proxy := &latencyProxy{listener: listener, upstream: net.JoinHostPort(parsed.Hostname(), port), delay: delay}
	proxy.wg.Go(proxy.accept)
	tb.Cleanup(func() {
		_ = listener.Close()
		// Close relayed sockets too, so a stalled write cannot block cleanup.
		proxy.mu.Lock()
		for _, conn := range proxy.conns {
			_ = conn.Close()
		}
		proxy.mu.Unlock()
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
		server, err := net.DialTimeout("tcp", p.upstream, 10*time.Second)
		if err != nil {
			_ = client.Close()
			continue
		}
		p.mu.Lock()
		p.conns = append(p.conns, client, server)
		p.mu.Unlock()
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
	done := make(chan struct{})
	var closeOnce sync.Once
	closeBoth := func() {
		closeOnce.Do(func() { close(done) })
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
				select {
				case chunks <- delayedChunk{due: time.Now().Add(p.delay), data: append([]byte(nil), buf[:n]...)}:
				case <-done:
					return
				}
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

// pushNewSessionsThroughProxy pushes the given number of new sessions from a
// freshly seeded archive into a new schema through proxy and reports the push
// alone, without schema setup. With warm set, a one-session push runs first,
// so the measured push excludes the work every target's first push does once,
// such as publishing model pricing.
func pushNewSessionsThroughProxy(tb testing.TB, pgURL, schema string, proxy *latencyProxy, sessions, messages int, warm bool) latencyPushRun {
	tb.Helper()
	// Close per call, so repeated iterations do not hold archives and pools.
	local, err := db.Open(tb.Context(), filepath.Join(tb.TempDir(), "archive.db"))
	require.NoError(tb, err)
	defer func() { require.NoError(tb, local.Close()) }()

	syncer, err := New(tb.Context(), proxy.url(tb, pgURL), schema, local, "latency", true, storage.PusherOptions{})
	require.NoError(tb, err)
	defer func() { require.NoError(tb, syncer.Close()) }()
	require.NoError(tb, syncer.EnsureSchema(tb.Context()))
	if warm {
		seedLatencyFixture(tb, local, "latency-warm", 1, 2)
		_, err := syncer.Push(tb.Context(), false, nil)
		require.NoError(tb, err)
	}
	seedLatencyFixture(tb, local, "latency-session", sessions, messages)

	before := proxy.roundTrips.Load()
	start := time.Now()
	result, err := syncer.Push(tb.Context(), false, nil)
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

// BenchmarkPGPushFirstLoadLatency measures pushing new sessions over a link
// that adds latency to every round trip: "first" is a target's first push,
// and "warm" pushes the same sessions after a one-session push, so it measures
// only the session writes. Run it with -benchtime=1x; a higher count repeats
// the whole load.
func BenchmarkPGPushFirstLoadLatency(b *testing.B) {
	const sessions, messages = 100, 20
	for _, delay := range []time.Duration{20 * time.Millisecond, 100 * time.Millisecond} {
		for _, warm := range []bool{false, true} {
			push := "first"
			if warm {
				push = "warm"
			}
			b.Run(fmt.Sprintf("latency=%s/push=%s", delay, push), func(b *testing.B) {
				pgURL := testPGURL(b)
				proxy := startLatencyProxy(b, pgURL, delay)
				// The timer also covers seeding, so calibration sees each slow
				// iteration; the push alone is reported as push-s/op.
				var trips int64
				var elapsed time.Duration
				for iteration := range b.N {
					schema := fmt.Sprintf("push_latency_%d", iteration)
					dropLatencySchema(b, pgURL, schema)
					run := pushNewSessionsThroughProxy(b, pgURL, schema, proxy, sessions, messages, warm)
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
}

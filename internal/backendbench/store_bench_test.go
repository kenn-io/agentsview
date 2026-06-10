//go:build benchdb

package backendbench

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/duckdb"
	"go.kenn.io/agentsview/internal/postgres"
)

const (
	defaultBenchmarkSessionCount       = 1000
	defaultBenchmarkMessagesPerSession = 64

	benchmarkMachine = "bench-machine"
	benchmarkSchema  = "agentsview_bench"
	benchmarkQuery   = "needle"
)

type benchmarkStore struct {
	name  string
	store db.Store
}

type benchmarkFixture struct {
	sessionCount       int
	messagesPerSession int
}

func BenchmarkStoreBackends(b *testing.B) {
	ctx := context.Background()
	fixture := benchmarkFixtureFromEnv(b)
	b.Logf(
		"benchmark fixture: %d sessions, %d messages/session, %d total messages",
		fixture.sessionCount,
		fixture.messagesPerSession,
		fixture.sessionCount*fixture.messagesPerSession,
	)

	stores := setupBenchmarkStores(ctx, b, fixture)
	targetSessionID := sessionID(fixture.sessionCount / 2)

	benchmarks := []struct {
		name string
		run  func(*testing.B, context.Context, db.Store)
	}{
		{
			name: "ListSessions",
			run: func(b *testing.B, ctx context.Context, store db.Store) {
				for range b.N {
					page, err := store.ListSessions(ctx, db.SessionFilter{
						Limit: 50,
					})
					if err != nil {
						b.Fatal(err)
					}
					if len(page.Sessions) == 0 {
						b.Fatal("expected sessions")
					}
				}
			},
		},
		{
			name: "SidebarSessionIndex",
			run: func(b *testing.B, ctx context.Context, store db.Store) {
				for range b.N {
					index, err := store.GetSidebarSessionIndex(ctx, db.SessionFilter{
						IncludeChildren: true,
					})
					if err != nil {
						b.Fatal(err)
					}
					if len(index.Sessions) == 0 {
						b.Fatal("expected sidebar rows")
					}
				}
			},
		},
		{
			name: "Search",
			run: func(b *testing.B, ctx context.Context, store db.Store) {
				for range b.N {
					page, err := store.Search(ctx, db.SearchFilter{
						Query: benchmarkQuery,
						Limit: 25,
					})
					if err != nil {
						b.Fatal(err)
					}
					if len(page.Results) == 0 {
						b.Fatal("expected search results")
					}
				}
			},
		},
		{
			name: "GetAllMessages",
			run: func(b *testing.B, ctx context.Context, store db.Store) {
				for range b.N {
					messages, err := store.GetAllMessages(ctx, targetSessionID)
					if err != nil {
						b.Fatal(err)
					}
					if len(messages) != fixture.messagesPerSession {
						b.Fatalf("expected %d messages, got %d",
							fixture.messagesPerSession, len(messages))
					}
				}
			},
		},
		{
			name: "AnalyticsSummary",
			run: func(b *testing.B, ctx context.Context, store db.Store) {
				for range b.N {
					summary, err := store.GetAnalyticsSummary(ctx, db.AnalyticsFilter{
						From:     "2026-01-01",
						To:       "2026-12-31",
						Timezone: "UTC",
					})
					if err != nil {
						b.Fatal(err)
					}
					if summary.TotalSessions == 0 {
						b.Fatal("expected analytics sessions")
					}
				}
			},
		},
		{
			name: "DailyUsage",
			run: func(b *testing.B, ctx context.Context, store db.Store) {
				for range b.N {
					usage, err := store.GetDailyUsage(ctx, db.UsageFilter{
						From:     "2026-01-01",
						To:       "2026-12-31",
						Timezone: "UTC",
					})
					if err != nil {
						b.Fatal(err)
					}
					if len(usage.Daily) == 0 {
						b.Fatal("expected usage days")
					}
				}
			},
		},
	}

	for _, bench := range benchmarks {
		b.Run(bench.name, func(b *testing.B) {
			for _, backend := range stores {
				b.Run(backend.name, func(b *testing.B) {
					b.ReportAllocs()
					b.ResetTimer()
					bench.run(b, ctx, backend.store)
				})
			}
		})
	}
}

func benchmarkFixtureFromEnv(b *testing.B) benchmarkFixture {
	b.Helper()

	return benchmarkFixture{
		sessionCount: positiveIntFromEnv(
			b,
			"AGENTSVIEW_BENCH_SESSIONS",
			defaultBenchmarkSessionCount,
		),
		messagesPerSession: positiveIntFromEnv(
			b,
			"AGENTSVIEW_BENCH_MESSAGES_PER_SESSION",
			defaultBenchmarkMessagesPerSession,
		),
	}
}

func positiveIntFromEnv(b *testing.B, key string, fallback int) int {
	b.Helper()

	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		b.Fatalf("%s must be a positive integer, got %q", key, raw)
	}
	return value
}

func setupBenchmarkStores(ctx context.Context, b *testing.B, fixture benchmarkFixture) []benchmarkStore {
	b.Helper()

	local := openSQLiteStore(b)
	seedBenchmarkFixture(b, local, fixture)

	duck := openDuckDBStore(ctx, b, local)
	pg := openPostgresStore(ctx, b, local)

	return []benchmarkStore{
		{name: "sqlite", store: local},
		{name: "duckdb", store: duck},
		{name: "postgres", store: pg},
	}
}

func openSQLiteStore(b *testing.B) *db.DB {
	b.Helper()

	path := filepath.Join(b.TempDir(), "sessions.db")
	store, err := db.Open(path)
	if err != nil {
		b.Fatalf("open sqlite store: %v", err)
	}
	b.Cleanup(func() {
		if err := store.Close(); err != nil {
			b.Errorf("close sqlite store: %v", err)
		}
	})
	return store
}

func openDuckDBStore(ctx context.Context, b *testing.B, local *db.DB) db.Store {
	b.Helper()

	syncer, err := duckdb.New(
		filepath.Join(b.TempDir(), "sessions.duckdb"),
		local,
		benchmarkMachine,
		duckdb.SyncOptions{},
	)
	if err != nil {
		b.Fatalf("open duckdb sync: %v", err)
	}
	b.Cleanup(func() {
		if err := syncer.Close(); err != nil {
			b.Errorf("close duckdb sync: %v", err)
		}
	})

	result, err := syncer.Push(ctx, true, nil)
	if err != nil {
		b.Fatalf("push duckdb fixture: %v", err)
	}
	if result.SessionsPushed == 0 || result.MessagesPushed == 0 {
		b.Fatalf("duckdb fixture push wrote no rows: %+v", result)
	}

	return duckdb.NewStoreFromDB(syncer.DB())
}

func openPostgresStore(ctx context.Context, b *testing.B, local *db.DB) db.Store {
	b.Helper()

	container, err := tcpostgres.Run(
		ctx,
		"postgres:17-alpine",
		tcpostgres.WithDatabase("agentsview_bench"),
		tcpostgres.WithUsername("agentsview_bench"),
		tcpostgres.WithPassword("agentsview_bench"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(2*time.Minute),
		),
	)
	if err != nil {
		b.Fatalf("start postgres container: %v", err)
	}
	b.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := container.Terminate(
			stopCtx,
			testcontainers.StopTimeout(10*time.Second),
		); err != nil {
			b.Errorf("terminate postgres container: %v", err)
		}
	})

	pgURL, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		b.Fatalf("postgres connection string: %v", err)
	}

	syncer, err := postgres.New(
		pgURL,
		benchmarkSchema,
		local,
		benchmarkMachine,
		true,
		postgres.SyncOptions{},
	)
	if err != nil {
		b.Fatalf("open postgres sync: %v", err)
	}
	b.Cleanup(func() {
		if err := syncer.Close(); err != nil {
			b.Errorf("close postgres sync: %v", err)
		}
	})

	result, err := syncer.Push(ctx, true, nil)
	if err != nil {
		b.Fatalf("push postgres fixture: %v", err)
	}
	if result.SessionsPushed == 0 || result.MessagesPushed == 0 {
		b.Fatalf("postgres fixture push wrote no rows: %+v", result)
	}

	store, err := postgres.NewStore(pgURL, benchmarkSchema, true)
	if err != nil {
		b.Fatalf("open postgres store: %v", err)
	}
	b.Cleanup(func() {
		if err := store.Close(); err != nil {
			b.Errorf("close postgres store: %v", err)
		}
	})
	return store
}

func seedBenchmarkFixture(b *testing.B, store *db.DB, fixture benchmarkFixture) {
	b.Helper()

	if err := store.UpsertModelPricing([]db.ModelPricing{
		{
			ModelPattern:         "claude-bench-*",
			InputPerMTok:         3,
			OutputPerMTok:        15,
			CacheCreationPerMTok: 3.75,
			CacheReadPerMTok:     0.30,
		},
	}); err != nil {
		b.Fatalf("seed pricing: %v", err)
	}

	start := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	for i := range fixture.sessionCount {
		id := sessionID(i)
		project := fmt.Sprintf("project-%02d", i%11)
		agent := "codex"
		if i%3 == 1 {
			agent = "claude"
		} else if i%3 == 2 {
			agent = "cursor"
		}
		startedAt := start.Add(time.Duration(i*37) * time.Minute)
		endedAt := startedAt.Add(time.Duration(fixture.messagesPerSession*3) * time.Minute)
		firstMessage := fmt.Sprintf(
			"Benchmark prompt for %s with %s marker",
			project,
			benchmarkQuery,
		)
		sessionName := fmt.Sprintf("Benchmark %03d", i)
		healthScore := 65 + i%35
		healthGrade := "B"
		if healthScore >= 90 {
			healthGrade = "A"
		} else if healthScore < 75 {
			healthGrade = "C"
		}
		termination := "clean"
		filePath := filepath.Join("/tmp/agentsview-bench", id+".jsonl")
		fileSize := int64(16_384 + i*127)
		fileMtime := endedAt.UnixNano()
		contextPressure := float64(fixture.messagesPerSession*250+i%1000) / 200000

		session := db.Session{
			ID:                     id,
			Project:                project,
			Machine:                benchmarkMachine,
			Agent:                  agent,
			FirstMessage:           &firstMessage,
			SessionName:            &sessionName,
			StartedAt:              strPtr(startedAt.Format(time.RFC3339)),
			EndedAt:                strPtr(endedAt.Format(time.RFC3339)),
			MessageCount:           fixture.messagesPerSession,
			UserMessageCount:       fixture.messagesPerSession / 2,
			TotalOutputTokens:      4200 + i*9,
			PeakContextTokens:      24000 + i*17,
			HasTotalOutputTokens:   true,
			HasPeakContextTokens:   true,
			ToolFailureSignalCount: i % 5,
			ToolRetryCount:         i % 3,
			EditChurnCount:         i % 7,
			ConsecutiveFailureMax:  i % 4,
			Outcome:                []string{"completed", "blocked", "in_progress"}[i%3],
			OutcomeConfidence:      []string{"high", "medium"}[i%2],
			EndedWithRole:          []string{"assistant", "user"}[i%2],
			FinalFailureStreak:     i % 4,
			CompactionCount:        i % 4,
			MidTaskCompactionCount: i % 2,
			ContextPressureMax:     &contextPressure,
			HealthScore:            &healthScore,
			HealthGrade:            &healthGrade,
			HasContextData:         true,
			Cwd:                    filepath.Join("/workspace", project),
			GitBranch:              fmt.Sprintf("bench/branch-%02d", i%13),
			SourceVersion:          "backendbench",
			TerminationStatus:      &termination,
			FilePath:               &filePath,
			FileSize:               &fileSize,
			FileMtime:              &fileMtime,
			CreatedAt:              startedAt.Add(-time.Minute).Format(time.RFC3339),
		}
		if err := store.UpsertSession(session); err != nil {
			b.Fatalf("seed session %s: %v", id, err)
		}

		messages := make([]db.Message, 0, fixture.messagesPerSession)
		for ordinal := range fixture.messagesPerSession {
			timestamp := startedAt.Add(time.Duration(ordinal*3) * time.Minute)
			role := "assistant"
			if ordinal%2 == 0 {
				role = "user"
			}
			content := fmt.Sprintf(
				"%s message %02d for %s in %s with routine benchmark content",
				role,
				ordinal,
				id,
				project,
			)
			if ordinal%11 == 0 {
				content += " " + benchmarkQuery + " query payload"
			}
			contextTokens := 12000 + i*17 + ordinal*211
			outputTokens := 80 + ordinal*7 + i%41
			usage := tokenUsage{
				InputTokens:              900 + ordinal*13,
				OutputTokens:             outputTokens,
				CacheCreationInputTokens: 40 + i%11,
				CacheReadInputTokens:     120 + ordinal,
			}
			usageJSON, err := json.Marshal(usage)
			if err != nil {
				b.Fatalf("marshal token usage: %v", err)
			}
			messages = append(messages, db.Message{
				SessionID:        id,
				Ordinal:          ordinal,
				Role:             role,
				Content:          content,
				Timestamp:        timestamp.Format(time.RFC3339),
				ContentLength:    len(content),
				Model:            "claude-bench-sonnet",
				TokenUsage:       usageJSON,
				ContextTokens:    contextTokens,
				OutputTokens:     outputTokens,
				HasContextTokens: true,
				HasOutputTokens:  true,
				ClaudeMessageID:  fmt.Sprintf("%s-%02d", id, ordinal),
				ClaudeRequestID:  fmt.Sprintf("request-%03d-%02d", i, ordinal),
			})
		}
		if err := store.InsertMessages(messages); err != nil {
			b.Fatalf("seed messages for %s: %v", id, err)
		}
	}
}

func sessionID(i int) string {
	return fmt.Sprintf("bench-session-%04d", i)
}

func strPtr(s string) *string {
	return &s
}

type tokenUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

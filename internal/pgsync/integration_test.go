//go:build pgtest

package pgsync

import (
	"context"
	"testing"
	"time"

	"github.com/wesm/agentsview/internal/db"
)

func TestPGConnectivity(t *testing.T) {
	pgURL := testPGURL(t)

	local := testDB(t)
	ps, err := New(pgURL, local, "connectivity-test-machine", time.Hour, true)
	if err != nil {
		t.Fatalf("creating pgsync: %v", err)
	}
	defer ps.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := ps.EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	status, err := ps.Status(ctx)
	if err != nil {
		t.Fatalf("get status: %v", err)
	}

	t.Logf("PG Sync Status: %+v", status)
}

func TestPGPushCycle(t *testing.T) {
	pgURL := testPGURL(t)

	// Clean up schema before starting
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(pgURL, local, "machine-a", time.Hour, true)
	if err != nil {
		t.Fatalf("creating pgsync: %v", err)
	}
	defer ps.Close()

	ctx := context.Background()
	if err := ps.EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	// Create a session and message
	started := time.Now().UTC().Format(time.RFC3339)
	firstMsg := "hello from pg"
	sess := db.Session{
		ID:           "pg-sess-001",
		Project:      "pg-project",
		Machine:      "local",
		Agent:        "test-agent",
		FirstMessage: &firstMsg,
		StartedAt:    &started,
		MessageCount: 1,
	}
	if err := local.UpsertSession(sess); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	if err := local.InsertMessages([]db.Message{{
		SessionID: "pg-sess-001",
		Ordinal:   0,
		Role:      "user",
		Content:   firstMsg,
	}}); err != nil {
		t.Fatalf("insert message: %v", err)
	}

	// Push
	pushResult, err := ps.Push(ctx, false)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if pushResult.SessionsPushed != 1 || pushResult.MessagesPushed != 1 {
		t.Fatalf("pushed %d sessions, %d messages; want 1/1",
			pushResult.SessionsPushed, pushResult.MessagesPushed)
	}

	// Verify via status
	status, err := ps.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.PGSessions != 1 {
		t.Errorf("pg sessions = %d, want 1", status.PGSessions)
	}
	if status.PGMessages != 1 {
		t.Errorf("pg messages = %d, want 1", status.PGMessages)
	}
}

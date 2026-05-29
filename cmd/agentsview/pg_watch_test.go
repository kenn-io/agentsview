package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/postgres"
)

func pgConfigForTest(projects, exclude []string) config.PGConfig {
	return config.PGConfig{Projects: projects, ExcludeProjects: exclude}
}

func TestResolvePushProjects(t *testing.T) {
	tests := []struct {
		name        string
		pgProjects  []string
		pgExclude   []string
		cfg         PGPushConfig
		wantInclude []string
		wantExclude []string
		wantErr     bool
	}{
		{
			name:        "config include used when no flags",
			pgProjects:  []string{"a", "b"},
			wantInclude: []string{"a", "b"},
		},
		{
			name:        "flag include overrides config exclude",
			pgExclude:   []string{"x"},
			cfg:         PGPushConfig{ProjectsFlag: "a,b"},
			wantInclude: []string{"a", "b"},
		},
		{
			name:       "all-projects clears both",
			pgProjects: []string{"a"},
			cfg:        PGPushConfig{AllProjects: true},
		},
		{
			name:    "both flags is an error",
			cfg:     PGPushConfig{ProjectsFlag: "a", ExcludeProjects: "b"},
			wantErr: true,
		},
		{
			name:    "all-projects with include is an error",
			cfg:     PGPushConfig{AllProjects: true, ProjectsFlag: "a"},
			wantErr: true,
		},
		{
			name:       "config has both projects and exclude is an error",
			pgProjects: []string{"a"},
			pgExclude:  []string{"x"},
			wantErr:    true,
		},
		{
			name:    "all-projects with exclude is an error",
			cfg:     PGPushConfig{AllProjects: true, ExcludeProjects: "x"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pg := pgConfigForTest(tt.pgProjects, tt.pgExclude)
			inc, exc, err := resolvePushProjects(pg, tt.cfg)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !equalStrings(inc, tt.wantInclude) {
				t.Errorf("include = %v, want %v", inc, tt.wantInclude)
			}
			if !equalStrings(exc, tt.wantExclude) {
				t.Errorf("exclude = %v, want %v", exc, tt.wantExclude)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// fakeTarget is a test double for pgTarget.
type fakeTarget struct {
	ensureErr  error
	pushErr    error
	pushResult postgres.PushResult
	pushes     int
	closed     int
}

func (f *fakeTarget) EnsureSchema(context.Context) error { return f.ensureErr }
func (f *fakeTarget) Push(
	context.Context, bool, func(postgres.PushProgress),
) (postgres.PushResult, error) {
	f.pushes++
	return f.pushResult, f.pushErr
}
func (f *fakeTarget) Close() error { f.closed++; return nil }

func TestPgPusher_ConnectsOnceAndReuses(t *testing.T) {
	target := &fakeTarget{}
	connects := 0
	p := &pgPusher{
		localSync: func(context.Context) error { return nil },
		connect: func() (pgTarget, error) {
			connects++
			return target, nil
		},
	}
	if err := p.push(context.Background(), reasonChange, false); err != nil {
		t.Fatalf("push 1: %v", err)
	}
	if err := p.push(context.Background(), reasonChange, false); err != nil {
		t.Fatalf("push 2: %v", err)
	}
	if connects != 1 {
		t.Fatalf("connects = %d, want 1 (connection reused)", connects)
	}
	if target.pushes != 2 {
		t.Fatalf("pushes = %d, want 2", target.pushes)
	}
}

func TestPgPusher_ReconnectsAfterPushError(t *testing.T) {
	connects := 0
	targets := []*fakeTarget{{pushErr: errors.New("conn reset")}, {}}
	p := &pgPusher{
		localSync: func(context.Context) error { return nil },
		connect: func() (pgTarget, error) {
			t := targets[connects]
			connects++
			return t, nil
		},
	}
	if err := p.push(context.Background(), reasonChange, false); err == nil {
		t.Fatal("expected first push to error")
	}
	if targets[0].closed != 1 {
		t.Fatal("errored target should have been closed")
	}
	if err := p.push(context.Background(), reasonChange, false); err != nil {
		t.Fatalf("second push should reconnect and succeed: %v", err)
	}
	if connects != 2 {
		t.Fatalf("connects = %d, want 2 (reconnect after error)", connects)
	}
}

func TestPgPusher_ConnectErrorSurfaced(t *testing.T) {
	p := &pgPusher{
		localSync: func(context.Context) error { return nil },
		connect: func() (pgTarget, error) {
			return nil, errors.New("dial timeout")
		},
	}
	if err := p.push(context.Background(), reasonChange, false); err == nil {
		t.Fatal("expected connect error to surface")
	}
}

func TestPgPusher_LocalSyncErrorSkipsConnect(t *testing.T) {
	connects := 0
	p := &pgPusher{
		localSync: func(context.Context) error { return errors.New("disk") },
		connect: func() (pgTarget, error) {
			connects++
			return &fakeTarget{}, nil
		},
	}
	if err := p.push(context.Background(), reasonChange, false); err == nil {
		t.Fatal("expected local sync error")
	}
	if connects != 0 {
		t.Fatal("connect should not run when local sync fails")
	}
}

func TestPgPusher_ReconnectsAfterEnsureSchemaError(t *testing.T) {
	connects := 0
	targets := []*fakeTarget{{ensureErr: errors.New("schema down")}, {}}
	p := &pgPusher{
		localSync: func(context.Context) error { return nil },
		connect: func() (pgTarget, error) {
			tgt := targets[connects]
			connects++
			return tgt, nil
		},
	}
	if err := p.push(context.Background(), reasonChange, false); err == nil {
		t.Fatal("expected first push to error on ensure schema")
	}
	if targets[0].closed != 1 {
		t.Fatal("errored target should have been closed")
	}
	if err := p.push(context.Background(), reasonChange, false); err != nil {
		t.Fatalf("second push should reconnect and succeed: %v", err)
	}
	if connects != 2 {
		t.Fatalf("connects = %d, want 2 (reconnect after ensure schema error)", connects)
	}
}

func TestPgPusher_LogsPartialPushErrors(t *testing.T) {
	target := &fakeTarget{
		pushResult: postgres.PushResult{
			SessionsPushed: 3,
			MessagesPushed: 9,
			Errors:         2,
		},
	}
	var logs bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prev) })

	p := &pgPusher{
		localSync: func(context.Context) error { return nil },
		connect: func() (pgTarget, error) {
			return target, nil
		},
	}
	require.NoError(t, p.push(context.Background(), reasonChange, false))

	got := logs.String()
	assert.Contains(t, got, "pushed 3 sessions, 9 messages, 2 errors")
	assert.Contains(t, got, "2 session(s) failed to push; will retry")
	assert.Contains(t, got, "change")
}

func TestResolveWatchTargets_ErrorsOnEmptyURL(t *testing.T) {
	appCfg := config.Config{} // no PG URL
	_, _, _, err := resolveWatchTargets(appCfg, PGPushConfig{})
	if err == nil {
		t.Fatal("expected error when url not configured")
	}
}

func TestResolveWatchTargets_ResolvesProjects(t *testing.T) {
	appCfg := config.Config{
		PG: config.PGConfig{
			URL:         "postgres://u:p@localhost:5432/db?sslmode=disable",
			MachineName: "box1",
		},
	}
	pg, inc, _, err := resolveWatchTargets(
		appCfg, PGPushConfig{ProjectsFlag: "a,b"},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pg.URL == "" {
		t.Fatal("expected resolved URL")
	}
	if !equalStrings(inc, []string{"a", "b"}) {
		t.Fatalf("include = %v, want [a b]", inc)
	}
}

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/postgres"
)

func TestPGEmbeddingRecoveryCommandContract(t *testing.T) {
	cmd := newPGEmbeddingsCommand()
	retry, _, err := cmd.Find([]string{"retry-failed"})
	require.NoError(t, err)
	assert.Equal(t, "retry-failed [target]", retry.Use)
	assert.Equal(t, "0", retry.Flag("generation").DefValue)
	assert.Equal(t, "64", retry.Flag("batch-size").DefValue)
	assert.NotNil(t, retry.Flag("format"))
	assert.NotNil(t, retry.Flag("json"))
}

func TestPGEmbeddingRecoveryValidatesBeforeConfig(t *testing.T) {
	blockedDataDir := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(blockedDataDir, []byte("sentinel"), 0o600))
	t.Setenv("AGENTSVIEW_DATA_DIR", blockedDataDir)

	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing generation", args: []string{"retry-failed", "runtime"}, want: `required flag(s) "generation" not set`},
		{name: "zero generation", args: []string{"retry-failed", "runtime", "--generation", "0"}, want: "--generation must be a positive integer"},
		{name: "negative generation", args: []string{"retry-failed", "runtime", "--generation=-1"}, want: "--generation must be a positive integer"},
		{name: "zero batch", args: []string{"retry-failed", "runtime", "--generation", "1", "--batch-size", "0"}, want: "--batch-size must be between 1 and 256"},
		{name: "negative batch", args: []string{"retry-failed", "runtime", "--generation", "1", "--batch-size=-1"}, want: "--batch-size must be between 1 and 256"},
		{name: "oversized batch", args: []string{"retry-failed", "runtime", "--generation", "1", "--batch-size", "257"}, want: "--batch-size must be between 1 and 256"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newPGEmbeddingsCommand()
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			cmd.SetArgs(tt.args)
			err := cmd.Execute()
			require.EqualError(t, err, tt.want)
			assert.NotContains(t, err.Error(), "not-a-directory")
		})
	}
}

func TestPGEmbeddingRecoveryFormatsAggregateOnly(t *testing.T) {
	result := postgres.HostedEmbeddingRetryResult{GenerationID: 7, Examined: 3, Retried: 2, Skipped: 1}
	for _, tc := range []struct {
		name, format, want string
	}{
		{name: "json", format: "json", want: "{\"generation_id\":7,\"examined\":3,\"retried\":2,\"skipped\":1}\n"},
		{name: "human", format: "human", want: "Generation 7: examined=3 retried=2 skipped=1\nSkipped failures remain unchanged and await normal worker reconciliation.\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			require.NoError(t, writePGEmbeddingRetryResult(&output, result, tc.format == "json"))
			assert.Equal(t, tc.want, output.String())
		})
	}
}

func TestPGEmbeddingRecoverySanitizesConnectionFailure(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	configBody := `default_pg = "runtime"

[pg.runtime]
url = "postgres://synthetic_user:boundary_password@127.0.0.1:1/synthetic?sslmode=disable&application_name=boundary_session"
schema = "synthetic_schema"
raw_tenant = "synthetic_tenant"
`
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "config.toml"), []byte(configBody), 0o600))

	cmd := newPGEmbeddingsCommand()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"retry-failed", "runtime", "--generation", "1"})
	err := cmd.Execute()
	require.EqualError(t, err, "opening selected hosted embedding runtime target failed")
	for _, forbidden := range []string{"boundary_password", "boundary_session", "synthetic_user", "127.0.0.1"} {
		assert.NotContains(t, fmt.Sprint(err), forbidden)
	}
}

func TestPGEmbeddingRecoveryCancelsBlockedHostedOpen(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, listener.Close()) })
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- conn
		}
	}()

	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	configBody := fmt.Sprintf(`default_pg = "runtime"

[pg.runtime]
url = "postgres://synthetic@%s/database?sslmode=disable"
schema = "synthetic_schema"
raw_tenant = "synthetic_tenant"
`, listener.Addr().String())
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "config.toml"), []byte(configBody), 0o600))

	ctx, cancel := context.WithCancel(t.Context())
	cmd := newPGEmbeddingsCommand()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetContext(ctx)
	cmd.SetArgs([]string{"retry-failed", "runtime", "--generation", "1"})
	done := make(chan error, 1)
	go func() { done <- cmd.Execute() }()
	var peer net.Conn
	select {
	case peer = <-accepted:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "hosted open did not reach the synthetic PostgreSQL endpoint")
	}
	cancel()

	var commandErr error
	cancelledPromptly := true
	select {
	case commandErr = <-done:
	case <-time.After(500 * time.Millisecond):
		cancelledPromptly = false
		require.NoError(t, peer.Close())
		peer = nil
		commandErr = <-done
	}
	if peer != nil {
		require.NoError(t, peer.Close())
	}
	assert.True(t, cancelledPromptly, "caller cancellation must stop the blocked initial hosted connection")
	require.EqualError(t, commandErr, "opening selected hosted embedding runtime target failed")
}

func TestPGEmbeddingRecoveryMapsSafeStoreErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "generation", err: fmt.Errorf("database detail: %w", postgres.ErrHostedEmbeddingGenerationNotServiced), want: "hosted embedding generation is not active or desired"},
		{name: "index", err: fmt.Errorf("database detail: %w", postgres.ErrHostedEmbeddingRecoveryIndexUnavailable), want: "hosted embedding recovery index unavailable; with the owner role, reprovision the current desired instance (or the active instance if no desired generation exists); provisioning reselects it"},
		{name: "query", err: errors.New("permission denied on boundary_source"), want: "retrying failed hosted embeddings failed"},
		{name: "deadline", err: fmt.Errorf("database detail: %w", context.DeadlineExceeded), want: "hosted embedding recovery timed out; retry with a smaller --batch-size"},
		{name: "canceled", err: fmt.Errorf("database detail: %w", context.Canceled), want: "hosted embedding recovery canceled"},
		{name: "index deadline", err: fmt.Errorf("database detail: %w: %w", postgres.ErrHostedEmbeddingRecoveryIndexUnavailable, context.DeadlineExceeded), want: "hosted embedding recovery timed out; retry with a smaller --batch-size"},
		{name: "index canceled", err: fmt.Errorf("database detail: %w: %w", postgres.ErrHostedEmbeddingRecoveryIndexUnavailable, context.Canceled), want: "hosted embedding recovery canceled"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mapped := pgEmbeddingRetryBoundaryError(tt.err)
			require.EqualError(t, mapped, tt.want)
			assert.NotContains(t, mapped.Error(), "database detail")
			assert.NotContains(t, mapped.Error(), "boundary_source")
		})
	}
}

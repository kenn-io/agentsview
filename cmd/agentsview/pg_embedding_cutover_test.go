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
)

func TestPGEmbeddingActivateCommandContract(t *testing.T) {
	cmd := newPGEmbeddingsCommand()
	activate, _, err := cmd.Find([]string{"activate"})
	require.NoError(t, err)
	require.Equal(t, "activate [target]", activate.Use)
	assert.Equal(t, "0", activate.Flag("generation").DefValue)
	assert.NotNil(t, activate.Flag("format"))
	assert.NotNil(t, activate.Flag("json"))
}

func TestPGEmbeddingActivateFormatsAggregateOnly(t *testing.T) {
	result := pgEmbeddingActivateResultDTO{GenerationID: 7, Activated: true}
	for _, tc := range []struct {
		name, format, want string
	}{
		{name: "json", format: "json", want: "{\"generation_id\":7,\"activated\":true}\n"},
		{name: "human", format: "human", want: "Generation 7 activated.\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			require.NoError(t, writePGEmbeddingActivateResult(&output, result, tc.format == "json"))
			assert.Equal(t, tc.want, output.String())
		})
	}
}

func TestPGEmbeddingActivateMapsSafeErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{name: "query", err: errors.New("permission denied on boundary_source"), want: "activating hosted embedding generation failed"},
		{name: "deadline", err: fmt.Errorf("database detail: %w", context.DeadlineExceeded), want: "hosted embedding activation timed out"},
		{name: "canceled", err: fmt.Errorf("database detail: %w", context.Canceled), want: "hosted embedding activation canceled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mapped := pgEmbeddingActivateBoundaryError(tc.err)
			require.EqualError(t, mapped, tc.want)
			assert.NotContains(t, mapped.Error(), "database detail")
			assert.NotContains(t, mapped.Error(), "boundary_source")
		})
	}
}

func TestPGEmbeddingActivateCancelsBlockedHostedOpen(t *testing.T) {
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

[hosted_embeddings.profiles.current.embeddings]
model = "synthetic"
dimension = 3
max_input_chars = 100
default_server = "primary"

[hosted_embeddings.profiles.current.embeddings.servers.primary]
endpoint = "https://encoder.example.test/v1"
`, listener.Addr().String())
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "config.toml"), []byte(configBody), 0o600))

	ctx, cancel := context.WithCancel(t.Context())
	cmd := newPGEmbeddingsCommand()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetContext(ctx)
	cmd.SetArgs([]string{"activate", "runtime", "--generation", "1"})
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
	require.EqualError(t, commandErr, "hosted embedding activation canceled")
}

func TestPGEmbeddingActivateSanitizesConnectionFailure(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	configBody := `default_pg = "runtime"

[pg.runtime]
url = "postgres://synthetic_user:boundary_password@127.0.0.1:1/synthetic?sslmode=disable&application_name=boundary_session"
schema = "synthetic_schema"
raw_tenant = "synthetic_tenant"

[hosted_embeddings.profiles.current.embeddings]
model = "synthetic"
dimension = 3
max_input_chars = 100
default_server = "primary"

[hosted_embeddings.profiles.current.embeddings.servers.primary]
endpoint = "https://encoder.example.test/v1"
`
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "config.toml"), []byte(configBody), 0o600))

	cmd := newPGEmbeddingsCommand()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"activate", "runtime", "--generation", "1"})
	err := cmd.Execute()
	require.EqualError(t, err, "opening selected hosted embedding runtime target failed")
	for _, forbidden := range []string{"boundary_password", "boundary_session", "synthetic_user", "127.0.0.1"} {
		assert.NotContains(t, fmt.Sprint(err), forbidden)
	}
}

func TestPGEmbeddingCutoverValidatesBeforeConfig(t *testing.T) {
	blockedDataDir := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(blockedDataDir, []byte("sentinel"), 0o600))
	t.Setenv("AGENTSVIEW_DATA_DIR", blockedDataDir)

	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing generation", args: []string{"activate", "runtime"}, want: `required flag(s) "generation" not set`},
		{name: "zero generation", args: []string{"activate", "runtime", "--generation", "0"}, want: "--generation must be a positive integer"},
		{name: "negative generation", args: []string{"activate", "runtime", "--generation=-1"}, want: "--generation must be a positive integer"},
		{name: "too many targets", args: []string{"activate", "runtime", "extra", "--generation", "1"}, want: "accepts at most 1 arg(s), received 2"},
		{name: "unknown mode", args: []string{"provision", "owner", "--profile", "current", "--runtime-role", "runtime", "--instance-key", "initial", "--activation-mode", "invalid"}, want: "--activation-mode must be automatic or manual"},
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

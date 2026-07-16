//go:build duckdbtest && !(windows && arm64)

package duckdb

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestQuackClientURLFormAttachBehavior pins the upstream Quack extension
// behavior that ValidateQuackClientURL relies on: the extension accepts the
// native quack:HOST:PORT authority but rejects URL-scheme (http:// / https://)
// forms at ATTACH time with "Invalid Port". agentsview rejects the URL-scheme
// forms up front (see ValidateQuackClientURL) precisely because the extension
// cannot parse them; if a future Quack version starts accepting them, this
// test flags that the validator can be revisited.
func TestQuackClientURLFormAttachBehavior(t *testing.T) {
	ctx := context.Background()
	port := freeTCPPort(t)
	nativeURI := "quack:127.0.0.1:" + port
	const token = "agentsview-duckdbtest-token-urlform"

	path := filepath.Join(t.TempDir(), "agentsview-quack-urlform.duckdb")
	openQuackMirrorServer(t, ctx, path, nativeURI, token)

	client, err := sql.Open("duckdb", "")
	require.NoError(t, err, "open client DuckDB")
	client.SetMaxOpenConns(1)
	client.SetMaxIdleConns(1)
	t.Cleanup(func() {
		require.NoError(t, client.Close(), "close client DuckDB")
	})
	_, err = client.ExecContext(ctx, "LOAD quack")
	require.NoError(t, err, "load quack extension in client")

	attach := func(alias, uri string) error {
		_, err := client.ExecContext(ctx, fmt.Sprintf(
			`ATTACH '%s' AS %s (TOKEN '%s')`, uri, alias, token,
		))
		return err
	}

	// Native HOST:PORT form is the working, documented client URL.
	require.NoError(t, attach("native_db", nativeURI),
		"native quack:HOST:PORT form must attach")

	// URL-scheme forms are rejected by the extension at ATTACH time. This is
	// the failure ValidateQuackClientURL pre-empts.
	httpErr := attach("http_db", "quack:http://127.0.0.1:"+port)
	require.Error(t, httpErr, "http:// client url form must fail at ATTACH")
	assert.Contains(t, httpErr.Error(), "Invalid Port")

	httpsErr := attach("https_db", "quack:https://127.0.0.1:"+port)
	require.Error(t, httpsErr, "https:// client url form must fail at ATTACH")
	assert.Contains(t, httpsErr.Error(), "Invalid Port")
}

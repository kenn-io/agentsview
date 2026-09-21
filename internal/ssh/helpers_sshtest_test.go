//go:build sshtest

package ssh

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func testSSHHost(t *testing.T) string {
	t.Helper()
	h := os.Getenv("TEST_SSH_HOST")
	if h == "" {
		h = "localhost"
	}
	return h
}

func testSSHPort(t *testing.T) int {
	t.Helper()
	p := os.Getenv("TEST_SSH_PORT")
	if p == "" {
		p = "2222"
	}
	port, err := strconv.Atoi(p)
	require.NoError(t, err, "invalid TEST_SSH_PORT")
	return port
}

func testSSHUser(t *testing.T) string {
	t.Helper()
	u := os.Getenv("TEST_SSH_USER")
	if u == "" {
		u = "testuser"
	}
	return u
}

func testSSHKeyFile(t *testing.T) string {
	t.Helper()
	k := os.Getenv("TEST_SSH_KEY")
	if k == "" {
		t.Fatal(
			"TEST_SSH_KEY must point to the private key file",
		)
	}
	return k
}

func testSSHOpts(t *testing.T) []string {
	t.Helper()
	keyFile := testSSHKeyFile(t)
	return []string{
		"-i", keyFile,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
	}
}

func testDB(t *testing.T) *db.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	database, err := db.Open(t.Context(), dbPath)
	require.NoError(t, err, "opening test db")
	t.Cleanup(func() { database.Close() })
	return database
}

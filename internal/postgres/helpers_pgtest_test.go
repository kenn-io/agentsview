//go:build pgtest

package postgres

import (
	"os"
	"testing"
)

func testPGURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("TEST_PG_URL")
	if url == "" {
		t.Skip("TEST_PG_URL not set; skipping PG tests")
	}
	return url
}

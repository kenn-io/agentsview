package postgres

import (
	"os"
	"runtime"
	"testing"
)

func requireHegel(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "darwin" && runtime.GOARCH == "amd64" && os.Getenv("HEGEL_LIBHEGEL_PATH") == "" {
		t.Skip("Hegel requires HEGEL_LIBHEGEL_PATH on darwin/amd64")
	}
}

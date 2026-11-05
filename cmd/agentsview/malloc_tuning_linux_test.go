//go:build linux && cgo

package main

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	if os.Getenv("AVTEST_MALLOC_ARENA_CHILD") != "1" {
		return
	}
	ready := make(chan error, 16)
	release := make(chan struct{})
	var workers sync.WaitGroup
	for range 16 {
		workers.Go(func() {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			db, err := sql.Open("sqlite3", ":memory:")
			if err != nil {
				ready <- err
				return
			}
			defer db.Close()
			var length int
			err = db.QueryRow("SELECT length(zeroblob(65536))").Scan(&length) //nolint:noctx // The child owns its lifetime and exits directly.
			ready <- err
			<-release
		})
	}
	for range 16 {
		if err := <-ready; err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	count := mallocArenaCount()
	close(release)
	workers.Wait()
	fmt.Println(count)
	os.Exit(0)
}

func TestMallocConstructorArenaCap(t *testing.T) {
	binary, err := os.Executable()
	require.NoError(t, err)
	for _, tt := range []struct {
		name   string
		env    []string
		capped bool
	}{
		{name: "constructor caps arenas for every process", capped: true},
		{name: "empty override counts as unset", env: []string{"MALLOC_ARENA_MAX="}, capped: true},
		{name: "operator override wins", env: []string{"MALLOC_ARENA_MAX=8"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), binary)
			cmd.Env = append([]string{"AVTEST_MALLOC_ARENA_CHILD=1"}, tt.env...)
			output, err := cmd.CombinedOutput()
			require.NoError(t, err, "%s", output)
			count, err := strconv.Atoi(strings.TrimSpace(string(output)))
			require.NoError(t, err)
			if count == -1 {
				t.Skip("glibc arena introspection unavailable")
			}
			require.Positive(t, count)
			if tt.capped {
				assert.LessOrEqual(t, count, 2)
			} else {
				assert.Greater(t, count, 2)
			}
		})
	}
}

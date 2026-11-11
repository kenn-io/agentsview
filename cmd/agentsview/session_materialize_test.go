package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionMaterializeRejectsRemoteBackends(t *testing.T) {
	for _, flag := range []string{"--server=http://127.0.0.1:1", "--pg"} {
		t.Run(flag, func(t *testing.T) {
			_, err := executeCommand(newRootCommand(), "session", "materialize", "fixture", "--cwd", t.TempDir(), flag)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "local archive")
		})
	}
}

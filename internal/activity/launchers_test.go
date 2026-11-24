package activity

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveLaunchers(t *testing.T) {
	var calls [][]string
	got, err := ResolveLaunchers(t.Context(), []string{"b", "missing", "b"}, func(_ context.Context, ids []string) ([]Launcher, error) {
		calls = append(calls, ids)
		switch ids[0] {
		case "b":
			return []Launcher{{ID: "b", IsSubagent: true, ParentSessionID: "a"}}, nil
		case "a":
			return []Launcher{{ID: "a", IsSubagent: true, ParentSessionID: "b"}}, nil
		}
		return nil, nil
	})
	require.NoError(t, err)
	assert.Equal(t, [][]string{{"b", "missing"}, {"a"}}, calls)
	assert.Equal(t, []string{"a", "b", "missing"}, got.Examined)
	require.Len(t, got.Rows, 2)
}

func TestResolveLaunchersCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	called := false
	_, err := ResolveLaunchers(ctx, []string{"parent"}, func(context.Context, []string) ([]Launcher, error) {
		called = true
		return nil, nil
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, called)
}

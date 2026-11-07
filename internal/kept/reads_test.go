package kept_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/kept"
)

func TestReadsRetention(t *testing.T) {
	var reads kept.Reads[string]
	loads := 0
	load := func(context.Context) (string, error) { loads++; return "finished", nil }
	for range 2 {
		got, err := reads.Do(t.Context(), "selection", "v1", load)
		require.NoError(t, err)
		require.Equal(t, "finished", got)
	}
	require.Equal(t, 1, loads)
	_, err := reads.Do(t.Context(), "selection", "v2", load)
	require.NoError(t, err)
	require.Equal(t, 2, loads)
	reads.Clear()
	_, err = reads.Do(t.Context(), "selection", "v2", load)
	require.NoError(t, err)
	require.Equal(t, 3, loads)
}

func TestReadsExplicitSizeIncludesHiddenFields(t *testing.T) {
	type value struct {
		Hidden string `json:"-"`
	}
	reads := kept.Reads[value]{Size: func(v value) int64 { return int64(len(v.Hidden)) }}
	loads := 0
	for range 2 {
		got, err := reads.Do(t.Context(), "selection", "v1", func(context.Context) (value, error) {
			loads++
			return value{Hidden: strings.Repeat("x", (64<<20)+1)}, nil
		})
		require.NoError(t, err)
		require.Len(t, got.Hidden, (64<<20)+1)
	}
	require.Equal(t, 2, loads)
}

func TestReadsUnkept(t *testing.T) {
	for _, tc := range []struct {
		name, slot, version, value string
		err                        error
	}{
		{name: "error", slot: "s", version: "v", err: errors.New("read failed")},
		{name: "oversize", slot: "s", version: "v", value: strings.Repeat("x", 64<<20)},
		{name: "empty slot", version: "v", value: "finished"},
		{name: "empty version", slot: "s", value: "finished"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reads kept.Reads[string]
			loads := 0
			for range 2 {
				got, err := reads.Do(t.Context(), tc.slot, tc.version, func(context.Context) (string, error) { loads++; return tc.value, tc.err })
				require.ErrorIs(t, err, tc.err)
				require.Equal(t, tc.value, got)
			}
			require.Equal(t, 2, loads)
		})
	}
}

func TestReadsConcurrentCancellation(t *testing.T) {
	for _, cancelBoth := range []bool{false, true} {
		t.Run(map[bool]string{false: "one leaves", true: "both leave"}[cancelBoth], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var reads kept.Reads[int]
				var loads atomic.Int32
				release, canceled := make(chan struct{}), make(chan struct{})
				load := func(ctx context.Context) (int, error) {
					loads.Add(1)
					select {
					case <-release:
						return 500, nil
					case <-ctx.Done():
						close(canceled)
						return 0, ctx.Err()
					}
				}
				first, cancelFirst := context.WithCancel(t.Context())
				defer cancelFirst()
				second, cancelSecond := context.WithCancel(t.Context())
				defer cancelSecond()
				type result struct {
					value int
					err   error
				}
				done := make(chan result, 2)
				go func() { v, err := reads.Do(first, "s", "v1", load); done <- result{v, err} }()
				synctest.Wait()
				go func() { v, err := reads.Do(second, "s", "v1", load); done <- result{v, err} }()
				synctest.Wait()
				cancelFirst()
				require.ErrorIs(t, (<-done).err, context.Canceled)
				if cancelBoth {
					cancelSecond()
					require.ErrorIs(t, (<-done).err, context.Canceled)
					<-canceled
				} else {
					close(release)
					got := <-done
					require.NoError(t, got.err)
					require.Equal(t, 500, got.value)
				}
				require.Equal(t, int32(1), loads.Load())
			})
		})
	}
}

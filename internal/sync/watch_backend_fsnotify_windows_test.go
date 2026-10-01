package sync

import (
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const expectedWindowsBufferBytes = 16 << 10

func fsnotifyBufferAllocations(t *testing.T) (bytes, objects int64, found bool) {
	t.Helper()
	// Two collections publish completed allocations to the memory profile.
	runtime.GC()
	runtime.GC()
	n, _ := runtime.MemProfile(nil, true)
	var records []runtime.MemProfileRecord
	for {
		records = make([]runtime.MemProfileRecord, n)
		var ok bool
		n, ok = runtime.MemProfile(records, true)
		if ok {
			break
		}
	}
	for _, record := range records[:n] {
		frames := runtime.CallersFrames(record.Stack())
		for {
			frame, more := frames.Next()
			if frame.Function == "github.com/fsnotify/fsnotify.(*readDirChangesW).addWatch" &&
				record.AllocObjects > 0 && record.AllocBytes/record.AllocObjects >= expectedWindowsBufferBytes {
				bytes += record.AllocBytes
				objects += record.AllocObjects
				found = true
				break
			}
			if !more {
				break
			}
		}
	}
	return bytes, objects, found
}

func TestFSNotifyBackendWindowsBufferAllocation(t *testing.T) {
	oldRate := runtime.MemProfileRate
	runtime.MemProfileRate = 1
	t.Cleanup(func() { runtime.MemProfileRate = oldRate })
	for _, route := range []string{"recursive", "shallow", "runtime", "reinstalled"} {
		t.Run(route, func(t *testing.T) {
			backend := testFSNotifyBackend(t)
			root := t.TempDir()
			path := root
			switch route {
			case "runtime":
				require.NoError(t, backend.AddRecursive(root, math.MaxInt).Err)
				path = filepath.Join(root, "child")
				require.NoError(t, os.Mkdir(path, 0o755))
			case "reinstalled":
				require.NoError(t, backend.AddShallow(root))
				require.NoError(t, backend.watchOps.Remove(root))
			}
			beforeBytes, beforeObjects, _ := fsnotifyBufferAllocations(t)
			switch route {
			case "recursive":
				require.NoError(t, backend.AddRecursive(root, math.MaxInt).Err)
			case "shallow":
				require.NoError(t, backend.AddShallow(root))
			case "runtime":
				itemType, excluded := backend.watchCreatedPath(path)
				require.Equal(t, backendItemDirectory, itemType)
				require.False(t, excluded)
			case "reinstalled":
				backend.reinstallShallowWatches()
			}
			afterBytes, afterObjects, found := fsnotifyBufferAllocations(t)
			runtime.KeepAlive(backend)
			require.True(t, found, "missing pinned fsnotify buffer allocation record")
			assert.Equal(t, int64(1), afterObjects-beforeObjects, "native buffer allocations")
			assert.Equal(t, int64(expectedWindowsBufferBytes), afterBytes-beforeBytes, "native buffer bytes")
			t.Logf("buffer allocation: %d objects, %d bytes", afterObjects-beforeObjects, afterBytes-beforeBytes)
		})
	}
}

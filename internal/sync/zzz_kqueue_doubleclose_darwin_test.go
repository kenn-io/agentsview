//go:build darwin

package sync

// TEMPORARY diagnostic: checks whether fsnotify's kqueue reader closes file
// descriptors it does not own when Remove races a directory rescan.

import (
	"fmt"
	"os"
	"path/filepath"
	gosync "sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

func TestZZZKqueueDoubleClose(t *testing.T) {
	deadline := time.Now().Add(4 * time.Minute)
	for iter := 0; time.Now().Before(deadline); iter++ {
		root := t.TempDir()
		child := filepath.Join(root, "child")
		if err := os.Mkdir(child, 0o755); err != nil {
			t.Fatal(err)
		}
		for i := range 20 {
			if err := os.WriteFile(filepath.Join(child, fmt.Sprintf("f%d", i)), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		w, err := fsnotify.NewWatcher()
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			for range w.Events {
			}
		}()
		go func() {
			for range w.Errors {
			}
		}()
		if err := w.Add(root); err != nil {
			t.Fatal(err)
		}
		if err := w.Add(child); err != nil {
			t.Fatal(err)
		}

		var wg gosync.WaitGroup
		stop := make(chan struct{})
		var sentinels []*os.File
		var lost []string
		check := func(f *os.File) {
			if _, err := f.Stat(); err != nil {
				lost = append(lost, fmt.Sprintf("fd %d: %v", f.Fd(), err))
			}
			_ = f.Close()
		}
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				_ = os.WriteFile(filepath.Join(child, fmt.Sprintf("n%d", i)), nil, 0o644)
				_ = os.WriteFile(filepath.Join(root, fmt.Sprintf("r%d", i)), nil, 0o644)
			}
		}()
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				r, wr, err := os.Pipe()
				if err != nil {
					return
				}
				sentinels = append(sentinels, r, wr)
				if len(sentinels) > 128 {
					check(sentinels[0])
					sentinels = sentinels[1:]
				}
			}
		}()
		time.Sleep(2 * time.Millisecond) //nolint:kennlint // deadlinetest: diagnostic lets the writer race Remove
		_ = w.Remove(child)
		_ = w.Remove(root)
		close(stop)
		wg.Wait()
		_ = w.Close()

		for _, f := range sentinels {
			check(f)
		}
		if len(lost) > 0 {
			t.Fatalf("iteration %d: fsnotify closed descriptors it did not own: %v", iter, lost)
		}
		if iter%200 == 0 {
			t.Logf("iteration %d ok", iter)
		}
	}
}

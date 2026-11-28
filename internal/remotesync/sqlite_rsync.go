package remotesync

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// SQLiteRsyncSource is a Source that can name a remote SQLite file as a
// sqlite3_rsync origin ("host:/path"). The SSH source implements it.
type SQLiteRsyncSource interface {
	SQLiteRsyncOrigin(remotePath string) string
}

// sqlite3RsyncPath finds sqlite3_rsync unless AGENTSVIEW_NO_SQLITE3_RSYNC
// is set. It is a var so tests can point it at a fake.
var sqlite3RsyncPath = func() (string, error) {
	if os.Getenv("AGENTSVIEW_NO_SQLITE3_RSYNC") != "" {
		return "", exec.ErrNotFound
	}
	return exec.LookPath("sqlite3_rsync")
}

// rsyncSQLiteSnapshots refreshes changed SQLite snapshot targets (Hermes
// state.db and friends) in the mirror with sqlite3_rsync, which sends only
// changed pages, instead of re-shipping the whole database in the archive.
// It returns the fetch paths still to download; any path it could not
// rsync stays in that list, so a failure falls back to the archive.
func (hs RemoteSync) rsyncSQLiteSnapshots(
	ctx context.Context, targets TargetSet, fetch []string,
	manifest Manifest, mirrorRoot string,
) []string {
	src, ok := hs.Source.(SQLiteRsyncSource)
	if !ok || len(fetch) == 0 {
		return fetch
	}
	bin, err := sqlite3RsyncPath()
	if err != nil {
		return fetch
	}
	snapshots := sqliteSnapshotTargets(targets)
	entries := make(map[string]ManifestEntry, len(manifest.Files))
	for _, entry := range manifest.Files {
		entries[filepath.Clean(entry.Path)] = entry
	}
	kept := make([]string, 0, len(fetch))
	for _, remote := range fetch {
		clean := filepath.Clean(remote)
		_, snapshot := snapshots[clean]
		entry, listed := entries[clean]
		if !snapshot || !listed {
			kept = append(kept, remote)
			continue
		}
		local, err := safeRemappedRemotePath(mirrorRoot, remote)
		if err == nil {
			err = rsyncSQLiteFile(ctx, bin, src.SQLiteRsyncOrigin(clean), local, entry)
		}
		if err != nil {
			log.Printf("remote sync %s: sqlite3_rsync %s failed, using archive: %v", hs.Host, remote, err)
			kept = append(kept, remote)
			continue
		}
		log.Printf("remote sync %s: sqlite3_rsync refreshed %s (changed pages only)", hs.Host, remote)
	}
	return kept
}

func rsyncSQLiteFile(ctx context.Context, bin, origin, local string, entry ManifestEntry) error {
	if err := os.MkdirAll(filepath.Dir(local), 0o700); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, bin, origin, local)
	cmd.WaitDelay = 10 * time.Second
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, tail(out, 512))
	}
	// Stamp the replica with the manifest identity so MirrorDiff sees it as
	// current. If the origin moved on mid-copy the size differs; leave the
	// mtime alone then, so the next sync refreshes it again.
	info, err := os.Stat(local)
	if err != nil {
		return err
	}
	if info.Size() == entry.Size {
		mtime := time.Unix(0, entry.MtimeNS)
		return os.Chtimes(local, mtime, mtime)
	}
	return nil
}

func tail(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}

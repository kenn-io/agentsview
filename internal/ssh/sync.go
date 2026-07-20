package ssh

import (
	"context"
	"fmt"
	"os"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/remotesync"
	"go.kenn.io/agentsview/internal/sync"
)

// SyncStats summarizes the outcome of a remote sync run.
type SyncStats = remotesync.SyncStats

// RemoteSync orchestrates pulling session data from a remote
// host over SSH, parsing it, and writing it to the local DB.
//
// SSH remote sync is a deprecated compatibility transport that receives only
// critical fixes. New configurations should use HTTP remote sync.
type RemoteSync struct {
	Host                    string
	User                    string
	Port                    int
	Full                    bool
	DB                      *db.DB
	SSHOpts                 []string // extra args passed to ssh (e.g. -i keyfile)
	BlockedResultCategories []string
	Progress                sync.ProgressFunc
}

// Run executes the full remote sync flow: resolve dirs,
// download via tar, then delegate to sync.Engine for
// discovery, parsing, and writing.
func (rs *RemoteSync) Run(
	ctx context.Context,
) (SyncStats, error) {
	var stats SyncStats

	rs.reportProgress("Resolving agent directories on " + rs.Host)
	fmt.Printf(
		"Resolving agent directories on %s...\n", rs.Host,
	)
	dirs, files, extraFiles, err := resolveDirs(
		ctx, rs.Host, rs.User, rs.Port, rs.SSHOpts,
	)
	if err != nil {
		return stats, fmt.Errorf(
			"resolve dirs on %s: %w", rs.Host, err,
		)
	}
	if len(dirs) == 0 {
		rs.reportProgress("No agent directories found on " + rs.Host)
		fmt.Printf("No agent directories found on %s\n", rs.Host)
		return stats, nil
	}

	rs.reportProgress(fmt.Sprintf(
		"Downloading session data from %s (%d agents)",
		rs.Host, len(dirs),
	))
	fmt.Printf(
		"Downloading session data from %s (%d agents)...\n",
		rs.Host, len(dirs),
	)
	tmpDir, err := downloadAndExtract(
		ctx, rs.Host, rs.User, rs.Port, rs.SSHOpts, dirs, files, extraFiles,
	)
	if err != nil {
		return stats, fmt.Errorf(
			"download from %s: %w", rs.Host, err,
		)
	}
	defer os.RemoveAll(tmpDir)
	rs.reportProgress("Download complete")
	fmt.Printf("Download complete.\n")

	t0 := time.Now()
	lastPrint := t0
	var lastProgress sync.Progress
	progress := func(p sync.Progress) {
		if p.Detail == "" {
			p.Detail = "Processing sessions from " + rs.Host
		}
		if rs.Progress != nil {
			rs.Progress(p)
		}
		lastProgress = p
		now := time.Now()
		if now.Sub(lastPrint) < 500*time.Millisecond {
			return
		}
		lastPrint = now
		elapsed := now.Sub(t0).Truncate(time.Millisecond)
		fmt.Printf(
			"\r  %d/%d sessions (%s)...",
			p.SessionsDone, p.SessionsTotal, elapsed,
		)
	}
	fmt.Printf("Processing sessions...\n")
	stats, err = remotesync.Importer{
		Host:                    rs.Host,
		Full:                    rs.Full,
		DB:                      rs.DB,
		BlockedResultCategories: rs.BlockedResultCategories,
		Progress:                progress,
	}.ImportExtracted(ctx, remotesync.TargetSet{
		Dirs:       dirs,
		Files:      files,
		ExtraFiles: extraFiles,
	}, tmpDir)
	if lastProgress.SessionsTotal > 0 {
		elapsed := time.Since(t0).Truncate(time.Millisecond)
		fmt.Printf(
			"\r  %d/%d sessions (%s)   \n",
			lastProgress.SessionsDone,
			lastProgress.SessionsTotal, elapsed,
		)
	}
	if err != nil {
		return stats, err
	}

	fmt.Printf(
		"Synced %d sessions from %s",
		stats.SessionsSynced, rs.Host,
	)
	if stats.Skipped > 0 {
		fmt.Printf(" (%d unchanged)", stats.Skipped)
	}
	if stats.Failed > 0 {
		fmt.Printf(" (%d failed)", stats.Failed)
	}
	fmt.Println()
	rs.reportProgress(remoteSyncSummary(rs.Host, stats))
	return stats, nil
}

func (rs *RemoteSync) reportProgress(detail string) {
	if rs.Progress == nil {
		return
	}
	rs.Progress(sync.Progress{Detail: detail})
}

func remoteSyncSummary(host string, stats SyncStats) string {
	summary := fmt.Sprintf("Synced %d sessions from %s", stats.SessionsSynced, host)
	if stats.Skipped > 0 {
		summary += fmt.Sprintf(" (%d unchanged)", stats.Skipped)
	}
	if stats.Failed > 0 {
		summary += fmt.Sprintf(" (%d failed)", stats.Failed)
	}
	return summary
}

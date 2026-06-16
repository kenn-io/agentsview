package ssh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"go.kenn.io/agentsview/internal/parser"
)

// buildTarCommand generates the remote tar command for the given
// agent directories and extra files. Uses -C / so paths are relative
// to root. Strips leading / from each path and shell-quotes it. The
// extra files are resolved to exist on the remote (see
// buildResolveScript), so tar does not fail on a missing path.
func buildTarCommand(
	dirs map[parser.AgentType][]string,
	extraFiles []string,
) string {
	var paths []string
	for _, agentDirs := range dirs {
		for _, d := range agentDirs {
			paths = append(paths, shellQuote(strings.TrimPrefix(d, "/")))
		}
	}
	for _, f := range extraFiles {
		paths = append(paths, shellQuote(strings.TrimPrefix(f, "/")))
	}
	return "tar cf - -C / -- " + strings.Join(paths, " ")
}

// shellQuote wraps s in single quotes, escaping any embedded
// single quotes. Safe for passing paths through sh -c.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// downloadAndExtract tars remote agent dirs and extracts to a local
// temp dir. Returns the temp dir path; caller must clean up.
func downloadAndExtract(
	ctx context.Context,
	host, user string, port int, sshOpts []string,
	dirs map[parser.AgentType][]string,
	extraFiles []string,
) (string, error) {
	tarCmd := buildTarCommand(dirs, extraFiles)
	stdout, cleanup, err := runSSHStream(
		ctx, host, user, port, sshOpts, tarCmd,
	)
	if err != nil {
		return "", fmt.Errorf("download: %w", err)
	}

	tmpDir, err := os.MkdirTemp("", "agentsview-ssh-*")
	if err != nil {
		stdout.Close()
		_ = cleanup()
		return "", fmt.Errorf("create temp dir: %w", err)
	}

	// Wrap stdout with a progress counter so the user
	// can see data flowing during the transfer.
	pr := &progressReader{r: stdout}
	done := make(chan struct{})
	go pr.printLoop(done)

	skipped, extractErr := extractTarStream(ctx, pr, tmpDir)
	close(done)
	pr.printFinal()

	if extractErr != nil {
		stdout.Close()
		os.RemoveAll(tmpDir)
		_ = cleanup()
		return "", fmt.Errorf("extract tar: %w", extractErr)
	}
	if skipped > 0 {
		fmt.Printf(
			"  Skipped %d self-referential hardlink(s).\n",
			skipped,
		)
	}

	// stdout is consumed by the extractor; close it so the SSH
	// process can exit cleanly. A non-zero remote tar exit is
	// fatal unless its stderr shows only benign warnings (files
	// changing or vanishing as the remote read them).
	stdout.Close()
	if err := cleanup(); err != nil {
		if !remoteTarStderrBenign(err) {
			os.RemoveAll(tmpDir)
			return "", fmt.Errorf("ssh tar: %w", err)
		}
		fmt.Printf(
			"  Remote tar reported benign warnings; continuing.\n",
		)
	}
	return tmpDir, nil
}

// remapToRemotePath converts a temp-dir path back to the original
// remote path. Strips the temp dir prefix so the remainder is the
// absolute path as it existed on the remote host.
//
// Example:
//
//	tempDir="/tmp/sync-123"
//	localPath="/tmp/sync-123/home/wes/.claude/foo.jsonl"
//	result="/home/wes/.claude/foo.jsonl"
func remapToRemotePath(tempDir, remoteDir, localPath string) string {
	_ = remoteDir // reserved for future use; tar -C / preserves full paths
	rel, err := filepath.Rel(tempDir, localPath)
	if err != nil {
		return localPath
	}
	return "/" + filepath.ToSlash(rel)
}

// progressReader wraps a reader and tracks bytes read.
type progressReader struct {
	r     io.Reader
	bytes atomic.Int64
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.r.Read(p)
	pr.bytes.Add(int64(n))
	return n, err
}

func (pr *progressReader) printLoop(done <-chan struct{}) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			fmt.Printf(
				"\r  Received %s...",
				formatBytes(pr.bytes.Load()),
			)
		}
	}
}

func (pr *progressReader) printFinal() {
	fmt.Printf(
		"\r  Received %s   \n",
		formatBytes(pr.bytes.Load()),
	)
}

func formatBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d bytes", b)
	}
}

// remappedDir returns the temp-dir equivalent of a remote dir.
//
// Example:
//
//	tempDir="/tmp/sync-123"
//	remoteDir="/home/wes/.claude"
//	result="/tmp/sync-123/home/wes/.claude"
func remappedDir(tempDir, remoteDir string) string {
	return filepath.Join(
		tempDir, strings.TrimPrefix(remoteDir, "/"),
	)
}

// benignRemoteTarPrimary are remote tar (creation-side) stderr
// messages we treat as non-fatal: a file mutated or vanished while it
// was being archived. The resulting archive is still well-formed, and
// the local extractor independently validates its integrity. Stored
// lowercase; matched case-insensitively against a lowercased line.
var benignRemoteTarPrimary = []string{
	"file changed as we read it",
	"file removed before we read it",
}

// benignRemoteTarFallout are the summary lines tar prints after a
// non-zero exit. They are tolerated only alongside a primary benign
// warning, never on their own. Stored lowercase (see above).
var benignRemoteTarFallout = []string{
	"exiting with failure status due to previous errors", // GNU tar
	"error exit delayed from previous errors",            // bsdtar
}

// remoteTarStderrBenign reports whether a non-nil cleanup() error from
// the remote tar stream is safe to ignore. It is fail-closed: it
// returns true only for a *commandError whose every stderr line is a
// known-benign warning and which includes at least one primary
// warning. Truncation, corrupt archives, permission errors, and
// SSH-level failures are never benign, so they can never be persisted
// to the skip cache as a successful sync.
func remoteTarStderrBenign(err error) bool {
	var ce *commandError
	if !errors.As(err, &ce) {
		return false
	}
	sawPrimary := false
	for line := range strings.SplitSeq(ce.Stderr, "\n") {
		// Lowercase for case-insensitive matching: GNU tar is
		// inconsistent about capitalization (create.c emits
		// "File removed before we read it" with a capital F but
		// "file changed as we read it" lowercase).
		line = strings.ToLower(
			strings.TrimRight(strings.TrimSpace(line), ". "),
		)
		switch {
		case line == "":
			continue
		case hasBenignPrimary(line):
			sawPrimary = true
		case hasBenignFallout(line):
			// Summary line: tolerated only as attached fallout.
		default:
			return false
		}
	}
	return sawPrimary
}

// hasBenignPrimary reports whether line is a per-file remote tar
// warning about a file mutating or vanishing mid-archive. tar formats
// these as "<path>: <message>", so the phrase is matched as a suffix
// after the ": " separator. Matching it anywhere in the line would let
// a benign phrase embedded in a file path mask a real error reported
// for that same path (e.g. ".../file changed as we read it: Cannot
// open: Permission denied").
func hasBenignPrimary(line string) bool {
	for _, phrase := range benignRemoteTarPrimary {
		if strings.HasSuffix(line, ": "+phrase) {
			return true
		}
	}
	return false
}

// hasBenignFallout reports whether line is a tar end-of-run summary,
// which tar prints with no leading path.
func hasBenignFallout(line string) bool {
	for _, phrase := range benignRemoteTarFallout {
		if strings.HasSuffix(line, phrase) {
			return true
		}
	}
	return false
}

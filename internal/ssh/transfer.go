package ssh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/remotesync"
)

// buildTarCommand generates the remote shell script for the given
// agent directories, agent-scoped files, and extra files. Uses -C /
// so paths are relative to root, and feeds paths to tar over stdin
// instead of expanding them as tar argv. The script itself is sent to
// the remote shell over stdin, so a large file-scoped Windsurf export
// does not consume ssh/exec argument space.
func buildTarCommand(
	dirs map[parser.AgentType][]string,
	files map[parser.AgentType][]string,
	extraFiles []string,
) string {
	var paths []string
	for agent, agentDirs := range dirs {
		if _, fileScoped := files[agent]; fileScoped {
			continue
		}
		for _, d := range agentDirs {
			if path := tarListPath(d); path != "" {
				paths = append(paths, shellQuote(path))
			}
		}
	}
	for _, agentFiles := range files {
		for _, f := range agentFiles {
			if path := tarListPath(f); path != "" {
				paths = append(paths, shellQuote(path))
			}
		}
	}
	for _, f := range extraFiles {
		if path := tarListPath(f); path != "" {
			paths = append(paths, shellQuote(path))
		}
	}
	var b strings.Builder
	b.WriteString("set -e\n")
	b.WriteString("av_emit_tar_path() { [ -e \"/$1\" ] || return 0; printf '%s\\n' \"$1\"; }\n")
	b.WriteString("{\n")
	b.WriteString(":\n")
	for _, path := range paths {
		b.WriteString("av_emit_tar_path ")
		b.WriteString(path)
		b.WriteByte('\n')
	}
	b.WriteString("} | tar cf - -C / -T -\n")
	return b.String()
}

func tarListPath(path string) string {
	if strings.ContainsAny(path, "\x00\n\r") {
		return ""
	}
	rel := strings.TrimPrefix(path, "/")
	if rel == "" || rel == "." {
		return ""
	}
	if strings.HasPrefix(rel, "./") {
		return rel
	}
	return "./" + rel
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
	files map[parser.AgentType][]string,
	extraFiles []string,
) (string, error) {
	tarCmd := buildTarCommand(dirs, files, extraFiles)
	stdout, cleanup, err := runSSHScriptStream(
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

	skipped, extractErr := remotesync.ExtractTarStream(ctx, pr, tmpDir)
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
	return remotesync.RemapToRemotePath(tempDir, remoteDir, localPath)
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
	return remotesync.RemappedDir(tempDir, remoteDir)
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

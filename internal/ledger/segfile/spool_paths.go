package segfile

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const spoolClaimPrefix = ".agentsview-ingest-claim-"

// openSpoolRoot rejects a symlink or non-directory final component, then
// pins and rechecks the directory identity before returning its handle.
func openSpoolRoot(path string) (*os.Root, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return nil, fmt.Errorf("spool path %q must be a real directory", path)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	current, err := os.Lstat(path)
	if err != nil || current.Mode()&os.ModeSymlink != 0 || !current.IsDir() ||
		!os.SameFile(before, opened) || !os.SameFile(opened, current) {
		_ = root.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("spool path %q changed while it was being opened", path)
	}
	return root, nil
}

func ensureSpoolRoot(path string) (*os.Root, error) {
	_, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(path, 0o755); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	return openSpoolRoot(path)
}

// openSpoolChild pins a real child directory beneath parent. Lstat plus the
// identity checks reject both existing symlinks and a directory replaced
// while it is being opened. The returned Root stays attached to that
// directory if its name is later replaced.
func openSpoolChild(parent *os.Root, name string, create bool) (*os.Root, error) {
	if create {
		err := parent.Mkdir(name, 0o755)
		if err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
	}

	before, err := parent.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) && !create {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return nil, fmt.Errorf("spool entry %q must be a real directory", name)
	}

	child, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err := child.Stat(".")
	if err != nil {
		_ = child.Close()
		return nil, err
	}
	current, err := parent.Lstat(name)
	if err != nil || current.Mode()&os.ModeSymlink != 0 || !current.IsDir() ||
		!os.SameFile(before, opened) || !os.SameFile(opened, current) {
		_ = child.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("spool entry %q changed while it was being opened", name)
	}
	return child, nil
}

// readSpoolFile opens a regular directory entry relative to a pinned spool
// directory. Root.Open confines symlink traversal to the directory; Lstat and
// file identity checks reject symlink entries, including swaps during open.
func readSpoolFile(root *os.Root, name string) ([]byte, error) {
	before, err := root.Lstat(name)
	if err != nil {
		return nil, fmt.Errorf("I/O error: %w", err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, fmt.Errorf("I/O error: spool entry %q is not a regular file", name)
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, fmt.Errorf("I/O error: %w", err)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("I/O error: %w", err)
	}
	current, err := root.Lstat(name)
	if err != nil || current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() ||
		!os.SameFile(before, opened) || !os.SameFile(opened, current) {
		if err != nil {
			return nil, fmt.Errorf("I/O error: %w", err)
		}
		return nil, fmt.Errorf("I/O error: spool entry %q changed while it was being opened", name)
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("I/O error: %w", err)
	}
	return b, nil
}

func spoolFileExists(root *os.Root, name string) (bool, error) {
	if root == nil {
		return false, nil
	}
	_, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func claimSpoolFile(root *os.Root, name string) (string, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return "", fmt.Errorf("ledger error: I/O error: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("ledger error: I/O error: spool entry %q is not a regular file", name)
	}
	for range tmpAttempts {
		token := make([]byte, 16)
		if _, err := rand.Read(token); err != nil {
			return "", fmt.Errorf("ledger error: I/O error: %w", err)
		}
		claim := spoolClaimPrefix + name + ".claim-" + hex.EncodeToString(token)
		if _, err := root.Lstat(claim); err == nil {
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("ledger error: I/O error: %w", err)
		}
		if err := root.Rename(name, claim); err != nil {
			return "", fmt.Errorf("ledger error: I/O error: %w", err)
		}
		return claim, nil
	}
	return "", fmt.Errorf("could not create a unique ingest claim after %d attempts", tmpAttempts)
}

func originalForSpoolClaim(claim string) (string, bool) {
	if !strings.HasPrefix(claim, spoolClaimPrefix) {
		return "", false
	}
	tail := strings.TrimPrefix(claim, spoolClaimPrefix)
	marker := strings.LastIndex(tail, ".claim-")
	if marker <= 0 {
		return "", false
	}
	original, token := tail[:marker], tail[marker+len(".claim-"):]
	decoded, err := hex.DecodeString(token)
	if err != nil || len(decoded) != 16 || filepath.Base(original) != original ||
		original == "." || original == ".." || !hasRustJSONExtension(original) {
		return "", false
	}
	return original, true
}

// restoreSpoolClaim puts an unprocessed claim back under its public filename
// without replacing a file a producer may have published since the claim.
func restoreSpoolClaim(root *os.Root, claim, original string) error {
	info, err := root.Lstat(claim)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := root.Readlink(claim)
		if err != nil {
			return err
		}
		if err := root.Symlink(target, original); err != nil {
			return fmt.Errorf("cannot restore symlink claim because %s already exists; claim preserved: %w", original, err)
		}
		return root.Remove(claim)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("claim %s is not a regular file; claim preserved", claim)
	}
	data, err := readSpoolFile(root, claim)
	if err != nil {
		return err
	}
	if err := writeNoClobberInRoot(root, original, data); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("cannot restore claim %s; claim preserved: %w", claim, err)
		}
		current, readErr := readSpoolFile(root, original)
		if readErr != nil {
			return fmt.Errorf("incoming file %s exists and cannot be compared; claim preserved: %w", original, readErr)
		}
		if !bytes.Equal(data, current) {
			return fmt.Errorf("incoming file %s has different content; claim preserved at %s", original, claim)
		}
	}
	return root.Remove(claim)
}

func recoverSpoolClaims(root *os.Root, report *IngestReport) error {
	f, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("I/O error: %w", err)
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return fmt.Errorf("I/O error: %w", err)
	}
	for _, entry := range entries {
		claim := entry.Name()
		if !strings.HasPrefix(claim, spoolClaimPrefix) {
			continue
		}
		original, ok := originalForSpoolClaim(claim)
		if !ok {
			report.Failed = append(report.Failed, IngestFailure{
				File: claim, Err: "invalid ingest claim name; left for manual recovery",
			})
			continue
		}
		if err := restoreSpoolClaim(root, claim, original); err != nil {
			report.Failed = append(report.Failed, IngestFailure{File: claim, Err: err.Error()})
		}
	}
	return nil
}

func syncSpoolDirBestEffort(root *os.Root) {
	if root == nil || runtime.GOOS == "windows" {
		return
	}
	f, err := root.Open(".")
	if err == nil {
		if err := f.Sync(); err != nil {
			log.Printf("ledger: failed to fsync directory %s: %v", root.Name(), err)
		}
		_ = f.Close()
		return
	}
	log.Printf("ledger: failed to fsync directory %s: %v", root.Name(), err)
}

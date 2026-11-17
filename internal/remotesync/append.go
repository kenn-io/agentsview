package remotesync

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

// Agent transcripts are append-only, so a changed file usually holds its old
// bytes unchanged at its start. A delta request offers the mirror copy's
// length and the hash of its last appendTailBytes; a remote copy that is
// longer and ends that prefix with the same bytes streams only the bytes past
// it. Matching the tail rather than the whole prefix keeps the remote's read
// cost bounded, which matches the size-and-mtime trust the mirror comparison
// already uses.
const (
	appendTailBytes = 64 << 10
	// appendOffsetPAXKey marks a tar entry whose body continues the mirror
	// copy at the recorded byte offset instead of replacing it.
	appendOffsetPAXKey = "AGENTSVIEW.append_offset"
)

// tailSHA256 hashes the at most appendTailBytes bytes of r before offset.
func tailSHA256(r io.ReaderAt, offset int64) (string, error) {
	start := max(offset-appendTailBytes, 0)
	hash := sha256.New()
	if _, err := io.Copy(hash, io.NewSectionReader(r, start, offset-start)); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// mirrorDeltaAppends offers an append base for each fetched file the mirror
// already holds as a non-empty regular file. A file it cannot read is simply
// fetched whole.
func mirrorDeltaAppends(mirrorRoot string, fetch []string) []DeltaAppend {
	var appends []DeltaAppend
	for _, remotePath := range fetch {
		local, err := safeRemappedRemotePath(mirrorRoot, remotePath)
		if err != nil {
			continue
		}
		base, ok := mirrorAppendBase(local)
		if !ok {
			continue
		}
		base.Path = remotePath
		appends = append(appends, base)
	}
	return appends
}

func mirrorAppendBase(local string) (DeltaAppend, bool) {
	file, err := os.Open(local)
	if err != nil {
		return DeltaAppend{}, false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return DeltaAppend{}, false
	}
	tail, err := tailSHA256(file, info.Size())
	if err != nil {
		return DeltaAppend{}, false
	}
	return DeltaAppend{Offset: info.Size(), TailSHA256: tail}, true
}

// appendBasesByPath indexes appends by cleaned path for the archive writer.
func appendBasesByPath(appends []DeltaAppend) map[string]DeltaAppend {
	if len(appends) == 0 {
		return nil
	}
	bases := make(map[string]DeltaAppend, len(appends))
	for _, base := range appends {
		bases[filepath.Clean(base.Path)] = base
	}
	return bases
}

// writeArchiveAppend writes only the bytes path gained past base.Offset. It
// reports false, having written nothing, when the file is not longer than the
// base or its bytes before the offset differ; the caller then sends the whole
// file.
func writeArchiveAppend(tw *tar.Writer, path string, base DeltaAppend) (bool, error) {
	if base.Offset <= 0 {
		return false, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return false, nil
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= base.Offset {
		return false, nil
	}
	tail, err := tailSHA256(file, base.Offset)
	if err != nil {
		return false, fmt.Errorf("read archive file %q: %w", path, err)
	}
	if tail != base.TailSHA256 {
		return false, nil
	}
	name, err := safeRemotePathArchiveName(path)
	if err != nil {
		return false, err
	}
	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return false, err
	}
	hdr.Name = name
	hdr.Size = info.Size() - base.Offset
	hdr.Format = tar.FormatPAX
	hdr.PAXRecords = map[string]string{
		appendOffsetPAXKey: strconv.FormatInt(base.Offset, 10),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return false, err
	}
	copied, err := io.Copy(tw, io.NewSectionReader(file, base.Offset, hdr.Size))
	if err != nil {
		return false, err
	}
	if copied != hdr.Size {
		return false, fmt.Errorf(
			"copy archive file %q: expected %d bytes, copied %d",
			path, hdr.Size, copied,
		)
	}
	return true, nil
}

// writeAppended continues the mirror copy at target with an appended entry.
// The copy must still have the length the request offered; on any failure
// it is cut back to that length, so the next comparison fetches it again.
func writeAppended(target string, tr io.Reader, hdr *tar.Header, rawOffset string) error {
	offset, err := strconv.ParseInt(rawOffset, 10, 64)
	if err != nil || offset <= 0 {
		return fmt.Errorf("invalid append offset %q for %q", rawOffset, hdr.Name)
	}
	f, err := os.OpenFile(target, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open %q for append: %w", hdr.Name, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("stat %q: %w", hdr.Name, err)
	}
	if info.Size() != offset {
		// The mirror is locked for the sync, so this means the request and
		// the mirror disagree. Leave the copy alone rather than cut it.
		_ = f.Close()
		return errors.New("append base for " + hdr.Name + " no longer matches the mirror copy")
	}
	writeErr := appendAt(f, tr, offset, hdr)
	closeErr := f.Close()
	if writeErr == nil && closeErr != nil {
		writeErr = fmt.Errorf("write %q: %w", hdr.Name, closeErr)
	}
	if writeErr != nil {
		_ = os.Truncate(target, offset)
		return writeErr
	}
	// Restore the remote mtime, as writeRegular does, so the next mirror
	// comparison sees the file as unchanged.
	if !hdr.ModTime.IsZero() {
		_ = os.Chtimes(target, hdr.ModTime, hdr.ModTime)
	}
	return nil
}

func appendAt(f *os.File, tr io.Reader, offset int64, hdr *tar.Header) error {
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return fmt.Errorf("seek %q: %w", hdr.Name, err)
	}
	n, err := io.Copy(f, tr)
	if err != nil {
		return fmt.Errorf("write %q: %w", hdr.Name, err)
	}
	if n != hdr.Size {
		return fmt.Errorf("short write %q: got %d of %d bytes", hdr.Name, n, hdr.Size)
	}
	return nil
}

// filesWithoutAppends returns the fetched files that were not offered for
// append.
func filesWithoutAppends(fetch []string, appends []DeltaAppend) []string {
	if len(appends) == 0 {
		return fetch
	}
	offered := make(map[string]struct{}, len(appends))
	for _, base := range appends {
		offered[base.Path] = struct{}{}
	}
	rest := make([]string, 0, len(fetch)-len(appends))
	for _, path := range fetch {
		if _, ok := offered[path]; !ok {
			rest = append(rest, path)
		}
	}
	return rest
}

// omittedAppends returns the offered files the archive did not include.
func omittedAppends(appends []DeltaAppend, extracted map[string]struct{}) []string {
	var omitted []string
	for _, base := range appends {
		name, err := safeRemotePathArchiveName(base.Path)
		if err != nil {
			continue
		}
		if _, ok := extracted[filepath.ToSlash(filepath.Clean(name))]; !ok {
			omitted = append(omitted, base.Path)
		}
	}
	return omitted
}

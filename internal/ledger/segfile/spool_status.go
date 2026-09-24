package segfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// SpoolStatus renders the observable part of one zone's status line
// (spool.rs:585-652): "incoming={n} processed={n} cursor[{source}]={v}".
// healthy is false when a directory or entry cannot be read, the cursor is
// corrupt, or the source is unknown; status then exits nonzero.
func SpoolStatus(spoolDir, cursorDir, zone, source string) (string, bool) {
	incoming, incomingOK := countSpoolDir(filepath.Join(spoolDir, SpoolIncomingDir))
	processed, processedOK := countSpoolDir(filepath.Join(spoolDir, SpoolProcessedDir))
	host, cursor, cursorOK := "unknown", "cannot determine own ledger source — pass --source", false
	if source != "" {
		host = source
		seq, present, valid := readCursor(CursorPath(cursorDir, zone, source))
		switch {
		case present && !valid:
			cursor, cursorOK = "unreadable/corrupt (emit will restart from 0)", false
		default:
			cursor, cursorOK = strconv.FormatUint(seq, 10), true
		}
	}
	line := fmt.Sprintf("incoming=%s processed=%s cursor[%s]=%s", incoming, processed, host, cursor)
	return line, incomingOK && processedOK && cursorOK
}

// countSpoolDir counts *.json entries. A missing directory is an honest 0;
// any other failure is never shown as zero (spool.rs:585-610).
func countSpoolDir(dir string) (string, bool) {
	f, err := os.Open(dir)
	if errors.Is(err, os.ErrNotExist) {
		return "0", true
	}
	if err != nil {
		return "unreadable", false
	}
	defer f.Close()
	entries, readErr := f.ReadDir(-1)
	if readErr != nil && len(entries) == 0 {
		return "unreadable", false
	}
	n := 0
	for _, e := range entries {
		if hasRustJSONExtension(e.Name()) {
			n++
		}
	}
	if readErr != nil {
		return fmt.Sprintf("%d (unreadable entries: 1)", n), false
	}
	return strconv.Itoa(n), true
}

// Package segfile reads and writes jilog-format segment directories
// (`<ledger_path>/segments/{source}-{seq:06}.json`). In agentsview the
// database rows are the authority (spec D19); files are the import,
// export and interop format. Ports jilog ledger-core segment.rs:153-384
// and store.rs:178-276.
package segfile

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/ledger"
)

// Entry is one listed segment file.
type Entry struct {
	Source string
	Seq    uint64
	Path   string
}

// Store is one segments directory.
type Store struct{ Dir string }

// List ports list_segments_with_errors (store.rs:218-276): only *.json,
// file-sync conflict copies (*.sync-conflict-*) skipped with a warning, unparseable names and
// unreadable entries reported as listing errors, a missing directory is
// empty, and entries sorted by (source, seq) rather than by name.
func (s Store) List() (segs []Entry, listingErrors []string) {
	dirents, err := os.ReadDir(s.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		listingErrors = append(listingErrors, fmt.Sprintf("unreadable directory entry in %s: %v", s.Dir, err))
	}
	for _, de := range dirents {
		name := de.Name()
		if filepath.Ext(name) != ".json" {
			continue
		}
		path := filepath.Join(s.Dir, name)
		if strings.Contains(name, ".sync-conflict-") {
			log.Printf("ledger: ignoring file-sync conflict copy %s in segments/ — inspect and remove it manually", path)
			continue
		}
		source, seq, ok := ledger.ParseFilename(name)
		if !ok {
			listingErrors = append(listingErrors,
				"segment filename does not parse as {source}-{seq}.json: "+path)
			continue
		}
		segs = append(segs, Entry{Source: source, Seq: seq, Path: path})
	}
	slices.SortFunc(segs, func(a, b Entry) int {
		return cmp.Or(cmp.Compare(a.Source, b.Source), cmp.Compare(a.Seq, b.Seq))
	})
	return segs, listingErrors
}

// Read ports read_segment (store.rs:178-182). It does not verify.
func (s Store) Read(source string, seq uint64) (ledger.Segment, error) {
	if !ledger.ValidSourceName(source) {
		return ledger.Segment{}, fmt.Errorf("invalid segment source %q: must match ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$", source)
	}
	return ReadFile(filepath.Join(s.Dir, fmt.Sprintf("%s-%06d.json", source, seq)))
}

// ReadFile ports read_from_file (segment.rs:409-414): parse, no verify.
func ReadFile(path string) (ledger.Segment, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return ledger.Segment{}, err
	}
	return ledger.ParseSegmentFile(b)
}

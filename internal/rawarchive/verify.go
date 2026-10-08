package rawarchive

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/rawsync"
)

// Verify reads every inventoried object and every accepted manifest. A vault ID
// or bounded blob sampler cannot establish that an entire recovery point exists.
func (a *Archive) Verify(ctx context.Context) (Report, error) {
	var report Report
	roots, err := a.roots(ctx)
	if err != nil {
		return report, err
	}
	report.Roots = len(roots)
	seen := make(map[rawsync.ObjectRef]bool)
	verify := func(ref rawsync.ObjectRef) error {
		if seen[ref] {
			return nil
		}
		if _, err := a.objects.CopyObject(ctx, a.tenant, ref, io.Discard); err != nil {
			return err
		}
		seen[ref] = true
		return nil
	}
	var after int64
	for {
		files, err := a.database.ListRawArchiveFiles(ctx, after, pageSize)
		if err != nil {
			return report, err
		}
		for _, file := range files {
			if _, ok := roots[file.RootID]; !ok {
				return report, errors.New("inventory file has no original root")
			}
			if err := verify(rawsync.ObjectRef{SHA256: file.SHA256, Length: file.Size}); err != nil {
				return report, fmt.Errorf("verifying inventoried object %s: %w", file.SHA256, err)
			}
			report.Files++
			report.Bytes += file.Size
			if !file.Covered {
				report.Supplemental++
			}
			after = file.ID
		}
		a.report(fmt.Sprintf("Verified %d files (%d bytes)", report.Files, report.Bytes))
		if len(files) < pageSize {
			break
		}
	}
	err = a.sourcePages(ctx, func(source db.RawArchiveSource) error {
		manifest, err := a.canonical(ctx, source, roots)
		if err != nil {
			return err
		}
		info, reader, err := a.objects.OpenManifest(ctx, manifest.Identity, manifest.ManifestID)
		if err != nil {
			return err
		}
		raw, readErr := io.ReadAll(io.LimitReader(reader, int64(len(source.CanonicalJSON))+1))
		verifyErr := reader.Verify()
		closeErr := reader.Close()
		if err := errors.Join(readErr, verifyErr, closeErr); err != nil {
			return err
		}
		if info.Ref.Length != int64(len(raw)) || !bytes.Equal(raw, source.CanonicalJSON) {
			return errors.New("vault manifest differs from acceptance record")
		}
		for _, ref := range manifest.Objects {
			if err := verify(ref); err != nil {
				return err
			}
		}
		report.Sources++
		if source.ProcessingVersion != "" && source.ParseError == "" {
			report.Parsed++
		}
		return nil
	})
	return report, err
}

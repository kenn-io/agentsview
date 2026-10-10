package rawarchive

import (
	"bytes"
	"context"

	"go.kenn.io/agentsview/internal/db"
)

func (a *Archive) retainCaptureEvidence(ctx context.Context, spec ImportSpec) error {
	rootID := archiveRootID(spec.DeviceID, "files", "capture-evidence")
	if err := a.database.RegisterRawArchiveRoot(ctx, db.RawArchiveRoot{ID: rootID, ConfiguredRootID: "capture-evidence", DeviceID: spec.DeviceID, Machine: spec.Machine, Provider: "files", OriginalPath: "capture-evidence"}); err != nil {
		return err
	}
	for name, data := range map[string][]byte{"capture.json": spec.capture.data, "inventory.json": spec.inventoryData} {
		ref, err := hashReader(ctx, bytes.NewReader(data))
		if err != nil {
			return err
		}
		if _, err := a.objects.PutObject(ctx, a.tenant, ref, bytes.NewReader(data)); err != nil {
			return err
		}
		if err := a.database.RecordRawArchiveFile(ctx, db.RawArchiveFile{RootID: rootID, Path: spec.capture.CaptureID + "/" + name, SHA256: ref.SHA256, Size: ref.Length}); err != nil {
			return err
		}
	}
	return nil
}

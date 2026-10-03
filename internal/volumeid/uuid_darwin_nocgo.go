//go:build darwin && !cgo

package volumeid

// mountedVolumeUUID reports no UUID without cgo, so every volume is identified
// by its mount point. Release builds on macOS use cgo.
func mountedVolumeUUID(string) ([16]byte, uuidAnswer) { return [16]byte{}, uuidNone }

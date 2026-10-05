//go:build windows

package db

import (
	"fmt"
	"os"

	"go.kenn.io/kit/atomicfile"
)

// replaceInstalledFile moves the old target to a failed-target name first,
// which startup recovery and reinstateSidelinedOriginal know how to restore.
// If installing the candidate fails, put the old target back before returning
// the error.
func replaceInstalledFile(source, target string) error {
	failed := target + ".failed"
	// The sideline path is scratch space that is cleared below. A caller
	// handing it in as the source would have its file deleted; reinstating a
	// sidelined original goes through reinstateSidelinedOriginal instead.
	if source == failed {
		return fmt.Errorf("replace installed file: source %s is the sideline scratch path", source)
	}
	if err := removeIfExists(failed); err != nil {
		return err
	}
	oldMoved := false
	if _, err := os.Stat(target); err == nil {
		if err := atomicfile.Replace(target, failed); err != nil {
			return err
		}
		oldMoved = true
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := atomicfile.Replace(source, target); err != nil {
		if oldMoved {
			_ = atomicfile.Replace(failed, target)
		}
		return err
	}
	if oldMoved {
		if err := removeIfExists(failed); err != nil {
			return err
		}
	}
	return nil
}

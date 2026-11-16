package rawcapture

import "os"

// CheckpointFileIdentity identifies a file across restarts on its volume.
func CheckpointFileIdentity(file *os.File, info os.FileInfo) string {
	return checkpointFileIdentity(file, info)
}

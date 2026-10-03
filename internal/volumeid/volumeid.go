// Package volumeid turns the device number in a file's stat into a value that
// identifies the same volume across reboots.
//
// Sync records a file's inode and device so a replaced file is never appended
// to from a stale offset. That needs the device half to mean "this volume" for
// as long as the archive lives. On Linux st_dev does. On macOS it does not: a
// volume's st_dev is assigned when it is mounted, and after a system update the
// data volume can come back with a different number while every file keeps its
// inode, which made every session look replaced.
package volumeid

// DeviceNumber is any integer type a platform declares st_dev as: a signed
// 32-bit integer on macOS, an unsigned 64-bit one on Linux, and an unsigned
// 32-bit one on a few others.
type DeviceNumber interface {
	~int32 | ~uint32 | ~int64 | ~uint64
}

// Stable returns a device value for dev that does not change when the volume
// is remounted. On platforms where st_dev is already stable it returns dev. It
// takes st_dev as the platform declares it, so no caller has to convert it.
func Stable[D DeviceNumber](dev D) uint64 { return stable(uint64(dev)) }

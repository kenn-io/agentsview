//go:build darwin && cgo

package volumeid

/*
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <sys/attr.h>
#include <unistd.h>

// The reply to the request below: its length, which attributes came back,
// then the volume UUID.
struct volumeid_reply {
	uint32_t length;
	attribute_set_t returned;
	uuid_t uuid;
} __attribute__((aligned(4), packed));

// volumeid_uuid copies the UUID of the volume mounted at path into out and
// returns 1. It returns 0 when the volume reports no UUID, and -1 when the
// call fails, which the caller must not mistake for a volume without one.
static int volumeid_uuid(const char *path, unsigned char out[16]) {
	struct attrlist request;
	struct volumeid_reply reply;
	memset(&request, 0, sizeof(request));
	request.bitmapcount = ATTR_BIT_MAP_COUNT;
	request.commonattr = ATTR_CMN_RETURNED_ATTRS;
	request.volattr = ATTR_VOL_INFO | ATTR_VOL_UUID;
	if (getattrlist(path, &request, &reply, sizeof(reply), FSOPT_NOFOLLOW) != 0) {
		return -1;
	}
	if (!(reply.returned.volattr & ATTR_VOL_UUID)) {
		return 0;
	}
	memcpy(out, reply.uuid, 16);
	return 1;
}
*/
import "C"

import "unsafe"

// mountedVolumeUUID returns the UUID of the volume mounted at mount, read with
// getattrlist(2). golang.org/x/sys does not wrap that call, and macOS builds
// already need cgo for SQLite.
func mountedVolumeUUID(mount string) ([16]byte, uuidAnswer) {
	path := C.CString(mount)
	defer C.free(unsafe.Pointer(path))
	var out [16]C.uchar
	switch C.volumeid_uuid(path, &out[0]) {
	case 1:
	case 0:
		return [16]byte{}, uuidNone
	default:
		return [16]byte{}, uuidFailed
	}
	var uuid [16]byte
	for i, b := range out {
		uuid[i] = byte(b)
	}
	if uuid == ([16]byte{}) {
		return [16]byte{}, uuidNone
	}
	return uuid, uuidFound
}

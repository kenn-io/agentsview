//go:build darwin && cgo

package requestsign

/*
#include <errno.h>
#include <membership.h>
#include <string.h>
#include <sys/acl.h>
#include <unistd.h>

static int av_signing_acl_is_private(int fd) {
	acl_t acl = acl_get_fd_np(fd, ACL_TYPE_EXTENDED);
	if (acl == NULL) {
		// ENOENT means the open file has no extended ACL.
		return errno == ENOENT;
	}

	uuid_t current_uuid;
	uuid_t root_uuid;
	if (mbr_uid_to_uuid(geteuid(), current_uuid) != 0 ||
		mbr_uid_to_uuid(0, root_uuid) != 0) {
		acl_free(acl);
		return 0;
	}

	acl_entry_t entry;
	int entry_id = ACL_FIRST_ENTRY;
	while (acl_get_entry(acl, entry_id, &entry) == 0) {
		entry_id = ACL_NEXT_ENTRY;
		acl_tag_t tag;
		if (acl_get_tag_type(entry, &tag) != 0) {
			acl_free(acl);
			return 0;
		}
		if (tag == ACL_EXTENDED_DENY) {
			continue;
		}
		if (tag != ACL_EXTENDED_ALLOW) {
			acl_free(acl);
			return 0;
		}
		uuid_t *principal = acl_get_qualifier(entry);
		if (principal == NULL) {
			acl_free(acl);
			return 0;
		}
		int trusted = memcmp(principal, current_uuid, sizeof(uuid_t)) == 0 ||
			memcmp(principal, root_uuid, sizeof(uuid_t)) == 0;
		acl_free(principal);
		if (!trusted) {
			acl_free(acl);
			return 0;
		}
	}
	// Darwin reports EINVAL after the final entry, including an empty ACL.
	int complete = errno == EINVAL;
	acl_free(acl);
	return complete;
}
*/
import "C"

import "os"

func hasPrivateFileACL(file *os.File) bool {
	return C.av_signing_acl_is_private(C.int(file.Fd())) == 1
}

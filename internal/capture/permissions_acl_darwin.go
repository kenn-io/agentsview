//go:build darwin && cgo

package capture

/*
#include <errno.h>
#include <fcntl.h>
#include <membership.h>
#include <stdlib.h>
#include <string.h>
#include <sys/acl.h>
#include <sys/types.h>
#include <unistd.h>

static int av_acl_has_dangerous_permission(acl_permset_t permissions) {
	acl_perm_t dangerous[] = {
		ACL_READ_DATA, ACL_WRITE_DATA, ACL_EXECUTE, ACL_DELETE,
		ACL_APPEND_DATA, ACL_DELETE_CHILD, ACL_READ_ATTRIBUTES,
		ACL_WRITE_ATTRIBUTES, ACL_READ_EXTATTRIBUTES,
		ACL_WRITE_EXTATTRIBUTES, ACL_READ_SECURITY, ACL_WRITE_SECURITY,
		ACL_CHANGE_OWNER,
	};
	size_t count = sizeof(dangerous) / sizeof(dangerous[0]);
	for (size_t i = 0; i < count; i++) {
		if (acl_get_perm_np(permissions, dangerous[i]) == 1) {
			return 1;
		}
	}
	return 0;
}

// Returns 0 when the ACL is safe, 1 for an untrusted allow entry, or a
// negative errno value when the ACL cannot be inspected.
static int av_validate_parent_acl(const char *path, uid_t current_uid) {
	int fd = open(path, O_RDONLY | O_DIRECTORY | O_NOFOLLOW);
	if (fd < 0) {
		return -errno;
	}
	acl_t acl = acl_get_fd_np(fd, ACL_TYPE_EXTENDED);
	if (acl == NULL) {
		int saved = errno;
		close(fd);
		if (saved == ENOENT || saved == EOPNOTSUPP) {
			return 0;
		}
		return -saved;
	}

	uuid_t current_uuid;
	uuid_t root_uuid;
	if (mbr_uid_to_uuid(current_uid, current_uuid) != 0 ||
		mbr_uid_to_uuid(0, root_uuid) != 0) {
		acl_free(acl);
		close(fd);
		return -EINVAL;
	}

	acl_entry_t entry;
	int entry_id = ACL_FIRST_ENTRY;
	while (acl_get_entry(acl, entry_id, &entry) == 0) {
		entry_id = ACL_NEXT_ENTRY;
		acl_tag_t tag;
		if (acl_get_tag_type(entry, &tag) != 0) {
			int saved = errno;
			acl_free(acl);
			close(fd);
			return -saved;
		}
		if (tag != ACL_EXTENDED_ALLOW) {
			continue;
		}
		uuid_t *principal = acl_get_qualifier(entry);
		if (principal == NULL) {
			int saved = errno;
			acl_free(acl);
			close(fd);
			return -saved;
		}
		int trusted = memcmp(principal, current_uuid, sizeof(uuid_t)) == 0 ||
			memcmp(principal, root_uuid, sizeof(uuid_t)) == 0;
		acl_free(principal);
		if (trusted) {
			continue;
		}
		acl_permset_t permissions;
		if (acl_get_permset(entry, &permissions) != 0) {
			int saved = errno;
			acl_free(acl);
			close(fd);
			return -saved;
		}
		if (av_acl_has_dangerous_permission(permissions)) {
			acl_free(acl);
			close(fd);
			return 1;
		}
	}
	acl_free(acl);
	close(fd);
	return 0;
}

static int av_clear_extended_acl(const char *path) {
	int fd = open(path, O_RDONLY | O_DIRECTORY | O_NOFOLLOW);
	if (fd < 0) {
		return -errno;
	}
	acl_t empty = acl_init(0);
	if (empty == NULL) {
		close(fd);
		return -errno;
	}
	if (acl_set_fd_np(fd, empty, ACL_TYPE_EXTENDED) != 0) {
		int saved = errno;
		acl_free(empty);
		close(fd);
		if (saved == EOPNOTSUPP) {
			return 0;
		}
		return -saved;
	}
	acl_free(empty);
	close(fd);
	return 0;
}
*/
import "C"

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

func verifyCaptureParentACL(path string) error {
	cPath := C.CString(path)
	defer C.free(unsafe.Pointer(cPath))
	status := int(C.av_validate_parent_acl(cPath, C.uid_t(os.Geteuid())))
	switch status {
	case 0:
		return nil
	case 1:
		return fmt.Errorf("capture parent %q grants access through an extended ACL", path)
	default:
		return fmt.Errorf("inspecting capture parent ACL %q: %w", path, syscall.Errno(-status))
	}
}

func secureCaptureDirectoryACL(path string) error {
	cPath := C.CString(path)
	defer C.free(unsafe.Pointer(cPath))
	status := int(C.av_clear_extended_acl(cPath))
	if status < 0 {
		return fmt.Errorf("clearing capture directory ACL: %w", syscall.Errno(-status))
	}
	return verifyCaptureParentACL(path)
}

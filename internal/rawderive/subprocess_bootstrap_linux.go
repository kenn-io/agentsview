//go:build linux && cgo && (amd64 || arm64)

package rawderive

/*
#define _GNU_SOURCE
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <sys/syscall.h>
#include <limits.h>

// Run before Go initializes its own poll descriptors. Plain exec preserves
// deliberately non-CLOEXEC descriptors; closing later would also close Go's.
static int raw_parser_fds_closed = 0;
static int raw_parser_fd_bootstrap_completed(void) { return raw_parser_fds_closed; }

__attribute__((constructor)) static void raw_parser_close_inherited_fds(void) {
    const char *mode = getenv("AGENTSVIEW_RAW_PARSER_CHILD");
    if (mode == NULL || strcmp(mode, "1") != 0) return;
    if (syscall(SYS_close_range, 3U, UINT_MAX, 0U) != 0) _exit(78);
    raw_parser_fds_closed = 1;
}
*/
import "C"

const parserFDBootstrapAvailable = true

func parserFDBootstrapCompleted() bool { return C.raw_parser_fd_bootstrap_completed() == 1 }

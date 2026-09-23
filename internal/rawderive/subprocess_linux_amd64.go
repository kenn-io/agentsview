package rawderive

import "golang.org/x/sys/unix"

const parserAuditArch = unix.AUDIT_ARCH_X86_64

var parserArchSyscalls = []uint32{unix.SYS_OPEN, unix.SYS_STAT, unix.SYS_LSTAT, unix.SYS_READLINK, unix.SYS_ACCESS, unix.SYS_EPOLL_WAIT, unix.SYS_POLL, unix.SYS_SELECT, unix.SYS_ARCH_PRCTL, unix.SYS_DUP2}

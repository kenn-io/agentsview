//go:build linux && (amd64 || arm64)

package rawderive

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

type boundParserImage struct {
	file   *os.File
	path   string
	digest ParityDigest
}

func (i *boundParserImage) identity() ParityDigest { return i.digest }

func openBoundParserImage() (*boundParserImage, error) {
	path, err := os.Executable()
	if err != nil {
		return nil, ErrSandboxUnavailable
	}
	return openBoundParserImageAt("/proc/self/exe", path)
}

func openBoundParserImageAt(imagePath, pathname string) (*boundParserImage, error) {
	file, err := os.Open(imagePath)
	if err != nil {
		return nil, ErrSandboxUnavailable
	}
	digest, err := digestParserImage(file)
	if err != nil {
		file.Close()
		return nil, ErrSandboxUnavailable
	}
	pathDigest, err := digestParserImagePath(pathname)
	if err != nil || pathDigest != digest {
		file.Close()
		return nil, fmt.Errorf("%w: executable pathname does not match running image", ErrSandboxUnavailable)
	}
	return &boundParserImage{file: file, path: pathname, digest: digest}, nil
}

func digestParserImage(reader io.ReadSeeker) (ParityDigest, error) {
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		return ParityDigest{}, err
	}
	h := sha256.New()
	if _, err := io.Copy(h, reader); err != nil {
		return ParityDigest{}, err
	}
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		return ParityDigest{}, err
	}
	var digest ParityDigest
	copy(digest[:], h.Sum(nil))
	return digest, nil
}

func digestParserImagePath(path string) (ParityDigest, error) {
	file, err := os.Open(path)
	if err != nil {
		return ParityDigest{}, err
	}
	defer file.Close()
	return digestParserImage(file)
}

func (i *boundParserImage) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "/proc/self/fd/3", args...)
	cmd.ExtraFiles = []*os.File{i.file}
	return cmd
}

func (i *boundParserImage) revalidate() error {
	digest, err := digestParserImagePath(i.path)
	if err != nil || digest != i.digest {
		return fmt.Errorf("%w: executable pathname changed after parity binding", ErrSandboxUnavailable)
	}
	return nil
}

func (i *boundParserImage) close() error { return i.file.Close() }

func configureParserNamespace(cmd *exec.Cmd) error {
	if !parserFDBootstrapAvailable {
		return ErrSandboxUnavailable
	}
	cmd.Env = append(cmd.Env, "AGENTSVIEW_RAW_PARSER_CHILD=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:                 unix.CLONE_NEWUSER | unix.CLONE_NEWNS | unix.CLONE_NEWNET,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Geteuid(), Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getegid(), Size: 1}},
		GidMappingsEnableSetgroups: false, Setpgid: true, Pdeathsig: syscall.SIGKILL,
	}
	return nil
}
func isolateParser(source, jail string) error {
	if !filepath.IsAbs(source) || !filepath.IsAbs(jail) || source == jail {
		return ErrSandboxUnavailable
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return err
	}
	// Bind the jail first: covering it later hides its source submount.
	if err := unix.Mount(jail, jail, "", unix.MS_BIND, ""); err != nil {
		return err
	}
	target := filepath.Join(jail, "source")
	if err := unix.Mount(source, target, "", unix.MS_BIND, ""); err != nil {
		return err
	}
	flags := uintptr(unix.MS_BIND | unix.MS_REMOUNT | unix.MS_RDONLY | unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC)
	if err := unix.Mount("", target, "", flags, ""); err != nil {
		return err
	}
	if err := unix.Mount("", jail, "", flags, ""); err != nil {
		return err
	}
	if err := unix.Chroot(jail); err != nil {
		return err
	}
	if err := unix.Chdir("/"); err != nil {
		return err
	}
	for resource, limit := range map[int]uint64{unix.RLIMIT_AS: 2 << 30, unix.RLIMIT_DATA: 512 << 20, unix.RLIMIT_CPU: 30, unix.RLIMIT_CORE: 0, unix.RLIMIT_NOFILE: 64, unix.RLIMIT_FSIZE: 0} {
		if err := unix.Setrlimit(resource, &unix.Rlimit{Cur: limit, Max: limit}); err != nil {
			return err
		}
	}
	return installParserSeccomp()
}
func installParserSeccomp() error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	// seccomp_data: nr@0, arch@4, args@16. x32 is rejected even on amd64.
	f := []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 4},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: parserAuditArch, Jt: 1},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_KILL_PROCESS},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
		{Code: unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K, K: 0x40000000, Jf: 1},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_KILL_PROCESS},
	}
	ret := func(nr uint32, action uint32) {
		f = append(f, unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: nr, Jf: 1}, unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: action})
	}
	ret(unix.SYS_CLONE3, unix.SECCOMP_RET_ERRNO|uint32(unix.ENOSYS))
	// clone is permitted solely for runtime threads, with no namespace or
	// process creation flags. Reject high bits as well as unfamiliar low bits.
	cloneFlags := uint32(unix.CLONE_VM | unix.CLONE_FS | unix.CLONE_FILES | unix.CLONE_SIGHAND | unix.CLONE_THREAD | unix.CLONE_SYSVSEM | unix.CLONE_SETTLS | unix.CLONE_PARENT_SETTID | unix.CLONE_CHILD_CLEARTID | unix.CLONE_CHILD_SETTID | unix.CLONE_DETACHED)
	f = append(f,
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.SYS_CLONE, Jf: 9},
		unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 20},
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: 0, Jf: 5},
		unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 16},
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K, K: ^cloneFlags, Jt: 3},
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K, K: unix.CLONE_THREAD, Jf: 2},
		unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
		unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
		unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
		unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
	)
	for _, nr := range []uint32{unix.SYS_KILL, unix.SYS_TGKILL} {
		f = append(f,
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: nr, Jf: 4},
			unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 16},
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: uint32(os.Getpid()), Jf: 1},
			unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
			unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
		)
	}
	allowed := []uint32{unix.SYS_READ, unix.SYS_WRITE, unix.SYS_READV, unix.SYS_WRITEV, unix.SYS_PREAD64, unix.SYS_CLOSE, unix.SYS_LSEEK, unix.SYS_FSTAT, unix.SYS_NEWFSTATAT, unix.SYS_STATX, unix.SYS_GETDENTS64, unix.SYS_READLINKAT, unix.SYS_OPENAT, unix.SYS_FACCESSAT, unix.SYS_FACCESSAT2, unix.SYS_FCNTL, unix.SYS_FLOCK, unix.SYS_MMAP, unix.SYS_MPROTECT, unix.SYS_MUNMAP, unix.SYS_MADVISE, unix.SYS_MREMAP, unix.SYS_BRK, unix.SYS_FUTEX, unix.SYS_SCHED_YIELD, unix.SYS_SCHED_GETAFFINITY, unix.SYS_CLOCK_GETTIME, unix.SYS_CLOCK_NANOSLEEP, unix.SYS_NANOSLEEP, unix.SYS_GETTIMEOFDAY, unix.SYS_RT_SIGACTION, unix.SYS_RT_SIGPROCMASK, unix.SYS_RT_SIGRETURN, unix.SYS_SIGALTSTACK, unix.SYS_RT_SIGSUSPEND, unix.SYS_RESTART_SYSCALL, unix.SYS_GETPID, unix.SYS_GETPPID, unix.SYS_GETTID, unix.SYS_GETUID, unix.SYS_GETEUID, unix.SYS_GETGID, unix.SYS_GETEGID, unix.SYS_GETRANDOM, unix.SYS_EPOLL_CREATE1, unix.SYS_EPOLL_CTL, unix.SYS_EPOLL_PWAIT, unix.SYS_EVENTFD2, unix.SYS_PIPE2, unix.SYS_DUP, unix.SYS_DUP3, unix.SYS_SET_ROBUST_LIST, unix.SYS_RSEQ, unix.SYS_SET_TID_ADDRESS, unix.SYS_GETCWD, unix.SYS_UNAME, unix.SYS_EXIT, unix.SYS_EXIT_GROUP}
	for _, nr := range append(allowed, parserArchSyscalls...) {
		ret(nr, unix.SECCOMP_RET_ALLOW)
	}
	f = append(f, unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)})
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return err
	}
	program := unix.SockFprog{Len: uint16(len(f)), Filter: &f[0]}
	result, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&program)))
	runtime.KeepAlive(f)
	if errno != 0 {
		return errno
	}
	if result != 0 {
		return ErrSandboxUnavailable
	}
	return nil
}

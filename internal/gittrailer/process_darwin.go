package gittrailer

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func processIdentity(pid uint32) (string, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", int(pid))
	if err != nil {
		return "", err
	}
	start := info.Proc.P_starttime
	return fmt.Sprintf("%d/%d/%d", pid, start.Sec, start.Usec), nil
}

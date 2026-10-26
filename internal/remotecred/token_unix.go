//go:build !windows

package remotecred

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func openToken(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	var st unix.Stat_t
	if err = unix.Fstat(fd, &st); err != nil {
		f.Close()
		return nil, err
	}
	if err := validateTokenStat(&st); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func validateTokenStat(st *unix.Stat_t) error {
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Uid != uint32(os.Geteuid()) || st.Mode&0o7777&^0o600 != 0 {
		return errors.New("token must be a regular file owned by the effective user with mode 0600 or stricter")
	}
	return nil
}

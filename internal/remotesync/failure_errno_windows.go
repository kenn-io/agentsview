//go:build windows

package remotesync

import "golang.org/x/sys/windows"

// Windows sockets return WSA error codes. syscall.ECONNREFUSED and its
// relatives are values Go invents on Windows, and errors.Is never maps a
// WSA code to them.
var (
	platformConnRefusedErrnos = []error{windows.WSAECONNREFUSED}
	platformUnreachableErrnos = []error{
		windows.WSAECONNRESET,
		windows.WSAECONNABORTED,
		windows.WSAETIMEDOUT,
		windows.WSAEHOSTUNREACH,
		windows.WSAENETUNREACH,
	}
)

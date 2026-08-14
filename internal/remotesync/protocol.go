package remotesync

import (
	"fmt"
	"net/http"
	"strconv"
)

type IncompatibleProtocolError struct {
	Got  string
	Want string
}

func (e *IncompatibleProtocolError) Error() string {
	return fmt.Sprintf("remote sync protocol version %s is incompatible with %s", e.Got, e.Want)
}

const (
	ProtocolVersion = 1
	ProtocolHeader  = "X-AgentsView-Remote-Sync-Version"
)

func SetProtocolHeader(header http.Header) {
	header.Set(ProtocolHeader, strconv.Itoa(ProtocolVersion))
}

func ValidateProtocolHeader(header http.Header) error {
	got := header.Get(ProtocolHeader)
	want := strconv.Itoa(ProtocolVersion)
	if got != want {
		if got == "" {
			got = "missing"
		}
		return &IncompatibleProtocolError{Got: got, Want: want}
	}
	return nil
}

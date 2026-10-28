package server

// Tests load IANA timezone names. Embed the zone database because Windows
// has none and -trimpath test binaries cannot read the copy in GOROOT.
import _ "time/tzdata"

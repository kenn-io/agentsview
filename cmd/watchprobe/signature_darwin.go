package main

import (
	"encoding/binary"

	"golang.org/x/sys/unix"
)

func freshSignature(path string) (signature, error) {
	var s unix.Stat_t
	if err := unix.Lstat(path, &s); err != nil {
		return signature{}, err
	}
	id := [16]byte{}
	binary.LittleEndian.PutUint64(id[:8], s.Ino)
	return signature{Size: s.Size, MtimeNS: s.Mtim.Nano(), ChangeNS: s.Ctim.Nano(), Volume: uint64(s.Dev), Identity: id, IdentityKnown: true, ChangeKnown: true}, nil
}

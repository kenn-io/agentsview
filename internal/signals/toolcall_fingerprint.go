package signals

import (
	"fmt"
	"hash/fnv"
)

// Fingerprint hashes the call's tool name and input with 32-bit FNV-1a over
// their UTF-8 bytes. A jump compares it with the call at its target, since
// some agents derive tool IDs from the message position and a rewrite can
// reuse one for another call. The frontend's toolCallFingerprint must match.
func (r ToolCallRow) Fingerprint() string {
	h := fnv.New32a()
	h.Write([]byte(r.ToolName))
	h.Write([]byte{0})
	h.Write([]byte(r.InputJSON))
	return fmt.Sprintf("%08x", h.Sum32())
}

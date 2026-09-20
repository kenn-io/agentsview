package secrets

import (
	"strings"
	"testing"
)

// benchCorpus mimics agent-session content: prose, code with assignments,
// JSON/tool output with colons, and high-entropy tokens (hashes, base64).
// The "=" / ":" everywhere is what exercises the high-entropy-assignment
// rule, the hot path during sync-time scanning.
var benchCorpus = strings.Repeat(`The sync engine writes each session after parsing.
commit := "a036b7b47fe2ec3cd35271bf37f2babd406dce97"
rulesVersion = "5872f62c14874e8b2ac4bd8e586638cfb37a843eb85b0e4c272eb47668dc9354"
{"path": "/Users/x/code/agentsview/internal/sync/engine.go", "size": 48213}
func computeSignalsFromMessages(s db.Session, msgs []db.Message) Update {
	hash := sha256.Sum256(data) // returns a 32-byte digest
	token = "ghp_xxxxNOTAREALTOKENxxxxxxxxxxxxxxxxxxxx"
}
Here is a normal paragraph of assistant prose explaining the change in
plain English, with no assignments or secrets, the kind of text that makes
up a large fraction of message content in a typical coding session.
base64blob: aGVsbG8gd29ybGQgdGhpcyBpcyBub3QgYSBzZWNyZXQgYnV0IGxvb2tzIHJhbmRvbQ==
`, 40)

func BenchmarkScan(b *testing.B) {
	b.SetBytes(int64(len(benchCorpus)))
	b.ReportAllocs()
	for range b.N {
		_ = Scan(benchCorpus)
	}
}

// BenchmarkScanDefinite measures throughput of the inline-sync path, which
// runs only the definite (cheap, specific-prefilter) rules and skips the
// expensive high-entropy/jwt/basic-auth candidate rules.
func BenchmarkScanDefinite(b *testing.B) {
	b.SetBytes(int64(len(benchCorpus)))
	b.ReportAllocs()
	for range b.N {
		_ = ScanDefinite(benchCorpus)
	}
}

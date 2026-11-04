package testutil

import (
	"os"
	"runtime"
	"testing"
)

// WildcardListenEnv opts a test run in to (or out of) the tests that bind a
// wildcard address.
const WildcardListenEnv = "AGENTSVIEW_TEST_WILDCARD_LISTEN"

// WildcardListenAllowed reports whether this run may bind wildcard addresses
// (0.0.0.0 / [::]). It is enabled everywhere except local Windows runs, where
// such a bind makes Windows Defender Firewall ask whether the test binary may
// communicate on public and private networks. CI runners (CI is set) have no
// such prompt, so they always run these tests. Set WildcardListenEnv to 1 to
// force it on, or to 0 to force it off.
func WildcardListenAllowed() bool {
	switch os.Getenv(WildcardListenEnv) {
	case "1":
		return true
	case "0":
		return false
	}
	return runtime.GOOS != "windows" || os.Getenv("CI") != ""
}

// RequireWildcardListen skips tb unless this run may bind a wildcard address.
//
// Only a handful of tests need one: they cover bind semantics that are
// specific to 0.0.0.0 / [::] (cross-family port collisions, the published
// discovery URL for a wildcard listener), so they cannot simply move to
// 127.0.0.1 the way every other listener in the suite does. Loopback listens
// never prompt the firewall; wildcard listens do, and because `go test`
// rebuilds <pkg>.test.exe into a fresh temporary directory on every run while
// Windows Defender Firewall records its decision per program path, the prompt
// comes back every single run. Skipping these few tests on local Windows runs
// keeps `go test ./...` prompt-free there while CI still runs them everywhere.
func RequireWildcardListen(tb testing.TB) {
	tb.Helper()
	if WildcardListenAllowed() {
		return
	}
	tb.Skipf(
		"wildcard listen disabled on %s to avoid the Windows Defender "+
			"Firewall prompt for the test binary; set %s=1 to run this test",
		runtime.GOOS, WildcardListenEnv,
	)
}

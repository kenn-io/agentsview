//go:build !linux || (!amd64 && !arm64)

package rawderive

import "os/exec"

func configureParserNamespace(*exec.Cmd) error { return ErrSandboxUnavailable }
func isolateParser(string, string) error       { return ErrSandboxUnavailable }

func parserFDBootstrapCompleted() bool { return false }

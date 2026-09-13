//go:build !linux || (!amd64 && !arm64)

package rawderive

import (
	"context"
	"os/exec"
)

type boundParserImage struct{}

func (i *boundParserImage) identity() ParityDigest { return ParityDigest{} }

func openBoundParserImage() (*boundParserImage, error) { return nil, ErrSandboxUnavailable }

func (i *boundParserImage) command(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "", args...)
}

func (i *boundParserImage) revalidate() error { return ErrSandboxUnavailable }
func (i *boundParserImage) close() error      { return nil }

func configureParserNamespace(*exec.Cmd) error { return ErrSandboxUnavailable }
func isolateParser(string, string) error       { return ErrSandboxUnavailable }

func parserFDBootstrapCompleted() bool { return false }

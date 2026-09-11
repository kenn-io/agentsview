//go:build linux && !cgo && (amd64 || arm64)

package rawderive

const parserFDBootstrapAvailable = false

func parserFDBootstrapCompleted() bool { return false }

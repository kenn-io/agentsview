//go:build !darwin

package volumeid

func stable(dev uint64) uint64 { return dev }

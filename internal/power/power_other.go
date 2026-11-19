//go:build !linux && !windows && (!darwin || !cgo)

package power

func onBattery() bool {
	return false
}

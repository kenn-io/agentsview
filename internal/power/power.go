// Package power reports the machine's current power source.
package power

// OnBattery reports whether the machine is running on battery power.
// Unknown power sources and probe errors return false.
func OnBattery() bool {
	return onBattery()
}

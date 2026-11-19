//go:build darwin && cgo

package power

/*
#cgo LDFLAGS: -framework IOKit -framework CoreFoundation
#include <IOKit/ps/IOPowerSources.h>
#include <IOKit/ps/IOPSKeys.h>

static int onBattery(void) {
	CFTypeRef info = IOPSCopyPowerSourcesInfo();
	if (info == NULL) return 0;
	CFStringRef source = IOPSGetProvidingPowerSourceType(info);
	int result = source != NULL && CFEqual(source, CFSTR(kIOPSBatteryPowerValue));
	CFRelease(info);
	return result;
}
*/
import "C"

func onBattery() bool {
	return C.onBattery() != 0
}

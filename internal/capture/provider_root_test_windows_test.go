//go:build windows

package capture

func secureTestProviderRoot(providerRoot string) {
	if err := secureCaptureDirectory(providerRoot); err != nil {
		panic("securing Windows test provider root: " + err.Error())
	}
}

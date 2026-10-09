//go:build !windows

package chromehost

func RegisteredManifest(home string) (string, error) {
	return ManifestPath("", home), nil
}

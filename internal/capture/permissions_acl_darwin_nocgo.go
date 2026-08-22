//go:build darwin && !cgo

package capture

import "errors"

func verifyCaptureParentACL(string) error {
	return errors.New("capture directory ACL validation requires a CGO-enabled build")
}

func secureCaptureDirectoryACL(string) error {
	return errors.New("capture directory ACL protection requires a CGO-enabled build")
}

//go:build !darwin && !windows

package capture

func verifyCaptureParentACL(string) error { return nil }

func secureCaptureDirectoryACL(string) error { return nil }

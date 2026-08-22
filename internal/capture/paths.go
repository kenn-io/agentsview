package capture

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

func validateResultPath(captureDir, resultPath string) (string, error) {
	if captureDir == "" {
		return "", errors.New("capture directory is required")
	}
	if resultPath == "-" {
		return resultPath, nil
	}
	absResult, err := filepath.Abs(resultPath)
	if err != nil {
		return "", fmt.Errorf("resolving result path: %w", err)
	}
	if err := validateExistingResult(resultPath); err != nil {
		return "", err
	}
	resolvedCapture, err := resolveProspectivePath(captureDir)
	if err != nil {
		return "", fmt.Errorf("resolving capture directory: %w", err)
	}
	resolvedResult, err := resolveProspectivePath(resultPath)
	if err != nil {
		return "", fmt.Errorf("resolving result path: %w", err)
	}
	inside, err := pathWithin(resolvedCapture, resolvedResult)
	if err != nil {
		return "", err
	}
	if inside {
		return "", errors.New("result path must be outside the capture directory")
	}
	return filepath.Clean(absResult), nil
}

func validateProviderRootPath(captureDir, providerRoot string) error {
	resolvedCapture, err := resolveProspectivePath(captureDir)
	if err != nil {
		return fmt.Errorf("resolving capture directory: %w", err)
	}
	resolvedRoot, err := resolveProspectivePath(providerRoot)
	if err != nil {
		return fmt.Errorf("resolving provider root: %w", err)
	}
	rootInsideCapture, err := pathWithin(resolvedCapture, resolvedRoot)
	if err != nil {
		return err
	}
	captureInsideRoot, err := pathWithin(resolvedRoot, resolvedCapture)
	if err != nil {
		return err
	}
	if rootInsideCapture || captureInsideRoot {
		return errors.New("capture directory and provider root must not overlap")
	}
	return nil
}

func validateResultOutsideProviderRoot(providerRoot, resultPath string) error {
	if resultPath == "-" {
		return nil
	}
	resolvedRoot, err := resolveProspectivePath(providerRoot)
	if err != nil {
		return fmt.Errorf("resolving provider root: %w", err)
	}
	resolvedResult, err := resolveProspectivePath(resultPath)
	if err != nil {
		return fmt.Errorf("resolving result path: %w", err)
	}
	inside, err := pathWithin(resolvedRoot, resolvedResult)
	if err != nil {
		return err
	}
	if inside {
		return errors.New("result path must be outside the provider root")
	}
	return nil
}

func writeCaptureResult(
	state *captureState,
	resultPath string,
	stdout ioWriter,
	data []byte,
) error {
	validated, err := validateResultPath(state.dir, resultPath)
	if err != nil {
		return err
	}
	return writeResult(validated, stdout, data)
}

func invalidateResult(path string) error {
	if err := validateExistingResult(path); err != nil {
		return err
	}
	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("rechecking existing result: %w", err)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("invalidating existing result: %w", err)
	}
	return nil
}

func validateExistingResult(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking existing result: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("existing result path is not a regular file")
	}
	return nil
}

func resolveProspectivePath(path string) (string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	current := absPath
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for _, part := range slices.Backward(suffix) {
				resolved = filepath.Join(resolved, part)
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return filepath.Clean(absPath), nil
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

func pathWithin(parent, candidate string) (bool, error) {
	if !strings.EqualFold(
		filepath.VolumeName(parent), filepath.VolumeName(candidate),
	) {
		return false, nil
	}
	return pathWithinVolume(parent, candidate)
}

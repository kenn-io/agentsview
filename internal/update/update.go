package update

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

const (
	// githubLatestReleaseURL is the HTML endpoint that 302-redirects to
	// /releases/tag/<tag>. Unlike api.github.com it is not rate-limited
	// at 60 req/hr per IP for unauthenticated callers.
	githubLatestReleaseURL    = "https://github.com/kenn-io/agentsview/releases/latest"
	githubReleaseDownloadBase = "https://github.com/kenn-io/agentsview/releases/download"
	updateUserAgent           = "agentsview-update"
	cacheFileName             = "update_check.json"
	cacheDuration             = 1 * time.Hour
	devCacheDuration          = 15 * time.Minute
)

// UpdateInfo contains information about an available update.
type UpdateInfo struct {
	CurrentVersion string
	LatestVersion  string
	DownloadURL    string
	AssetName      string
	Size           int64
	Checksum       string
	IsDevBuild     bool
	// cacheOnly is set when the info came from cache and lacks
	// download metadata. The caller must re-fetch for installs.
	cacheOnly bool
}

// NeedsRefetch returns true when the info came from cache
// and lacks the download URL/checksum needed for an install.
func (u *UpdateInfo) NeedsRefetch() bool {
	return u.cacheOnly
}

type cachedCheck struct {
	CheckedAt time.Time `json:"checked_at"`
	Version   string    `json:"version"`
}

// CheckForUpdate checks if a newer version is available.
// Uses a 1-hour cache to avoid hitting the GitHub API often.
func CheckForUpdate(
	currentVersion string,
	forceCheck bool,
	cacheDir string,
) (*UpdateInfo, error) {
	cleanVersion := strings.TrimPrefix(currentVersion, "v")
	isDevBuild := IsDevBuildVersion(cleanVersion)

	if !forceCheck {
		if info, done := checkCache(
			currentVersion, cleanVersion, isDevBuild, cacheDir,
		); done {
			return info, nil
		}
	}

	tag, err := resolveLatestTag(githubLatestReleaseURL)
	if err != nil {
		return nil, fmt.Errorf("check for updates: %w", err)
	}

	saveCache(tag, cacheDir)

	latestVersion := strings.TrimPrefix(tag, "v")

	if !isDevBuild && !isNewer(latestVersion, cleanVersion) {
		return nil, nil
	}

	ext := ".tar.gz"
	if runtime.GOOS == "windows" {
		ext = ".zip"
	}
	assetName := fmt.Sprintf(
		"agentsview_%s_%s_%s%s",
		latestVersion, runtime.GOOS, runtime.GOARCH, ext,
	)
	downloadURL := fmt.Sprintf(
		"%s/%s/%s", githubReleaseDownloadBase, tag, assetName,
	)
	checksumsURL := fmt.Sprintf(
		"%s/%s/SHA256SUMS", githubReleaseDownloadBase, tag,
	)

	// HEAD the asset to confirm it exists for this platform. The previous
	// API-based code returned "no release asset for OS/ARCH" up front; now
	// that we construct the URL ourselves, we have to verify it resolves.
	size, err := fetchContentLength(downloadURL)
	if err != nil {
		return nil, fmt.Errorf(
			"no release asset for %s/%s: %w",
			runtime.GOOS, runtime.GOARCH, err,
		)
	}

	checksum, _ := fetchChecksumFromFile(checksumsURL, assetName)

	return &UpdateInfo{
		CurrentVersion: currentVersion,
		LatestVersion:  tag,
		DownloadURL:    downloadURL,
		AssetName:      assetName,
		Size:           size,
		Checksum:       checksum,
		IsDevBuild:     isDevBuild,
	}, nil
}

// PerformUpdate downloads and installs the update.
func PerformUpdate(
	info *UpdateInfo,
	progressFn func(downloaded, total int64),
) error {
	if info.Checksum == "" {
		return fmt.Errorf(
			"no checksum for %s - refusing unverified binary",
			info.AssetName,
		)
	}

	fmt.Printf("Downloading %s...\n", info.AssetName)
	tempDir, err := os.MkdirTemp("", "agentsview-update-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tempDir)

	archivePath := filepath.Join(tempDir, info.AssetName)
	downloadChecksum, err := downloadFile(
		info.DownloadURL, archivePath, info.Size, progressFn,
	)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}

	if progressFn != nil {
		fmt.Println()
	}
	fmt.Println("Verifying and installing...")
	if err := installFromArchive(
		archivePath, info.Checksum, downloadChecksum,
	); err != nil {
		return err
	}
	fmt.Println("Update complete.")
	return nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func installFromArchive(
	archivePath, expectedChecksum, precomputedChecksum string,
) error {
	currentExe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find current executable: %w", err)
	}
	currentExe, err = filepath.EvalSymlinks(currentExe)
	if err != nil {
		return fmt.Errorf("resolve symlinks: %w", err)
	}
	binDir := filepath.Dir(currentExe)
	binaryName := "agentsview"
	if runtime.GOOS == "windows" {
		binaryName = "agentsview.exe"
	}
	dstPath := filepath.Join(binDir, binaryName)

	return installFromArchiveTo(
		archivePath, expectedChecksum, dstPath,
		precomputedChecksum,
	)
}

func installFromArchiveTo(
	archivePath, expectedChecksum, dstPath string,
	precomputedChecksum string,
) error {
	if expectedChecksum == "" {
		return fmt.Errorf(
			"empty checksum - refusing unverified binary",
		)
	}

	checksum := precomputedChecksum
	if checksum == "" {
		var err error
		checksum, err = hashFile(archivePath)
		if err != nil {
			return fmt.Errorf("hash archive: %w", err)
		}
	}

	if !strings.EqualFold(checksum, expectedChecksum) {
		return fmt.Errorf(
			"checksum mismatch: expected %s, got %s",
			expectedChecksum, checksum,
		)
	}

	extractDir, err := os.MkdirTemp("", "agentsview-extract-*")
	if err != nil {
		return fmt.Errorf("create extract dir: %w", err)
	}
	defer os.RemoveAll(extractDir)

	if strings.HasSuffix(archivePath, ".zip") {
		if err := extractZip(archivePath, extractDir); err != nil {
			return fmt.Errorf("extract: %w", err)
		}
	} else {
		if err := extractTarGz(archivePath, extractDir); err != nil {
			return fmt.Errorf("extract: %w", err)
		}
	}

	binaryName := "agentsview"
	if runtime.GOOS == "windows" {
		binaryName = "agentsview.exe"
	}
	srcPath := filepath.Join(extractDir, binaryName)
	if _, err := os.Stat(srcPath); os.IsNotExist(err) {
		return fmt.Errorf(
			"binary %s not found in archive", binaryName,
		)
	}

	return installBinaryTo(srcPath, dstPath)
}

// installBinaryTo replaces the binary at dstPath with the one
// at srcPath. The new binary is staged in a sibling tmp file
// with the executable mode bit set, then renamed into place.
//
// On Unix os.Rename atomically replaces dstPath in a single
// syscall, so concurrent readers always see one of the two
// binaries — never a missing or partial file. On Windows the
// existing binary must be moved aside first because os.Rename
// cannot replace a running executable; this leaves dstPath
// briefly missing between the two renames.
func installBinaryTo(srcPath, dstPath string) error {
	backupPath := dstPath + ".old"
	tmpPath := dstPath + ".new"

	// Clean up leftovers from a prior failed update so they
	// don't interfere with the renames below.
	os.Remove(backupPath)
	os.Remove(tmpPath)

	installed := false
	defer func() {
		if !installed {
			os.Remove(tmpPath)
		}
	}()

	// Stage the new binary at tmpPath with executable mode set
	// BEFORE touching the live binary at dstPath.
	if err := copyFile(srcPath, tmpPath); err != nil {
		return fmt.Errorf("install: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return fmt.Errorf("chmod: %w", err)
	}

	movedAside := false
	if runtime.GOOS == "windows" {
		aside, err := movePreviousAside(dstPath, backupPath)
		if err != nil {
			return err
		}
		movedAside = aside
	}

	if err := os.Rename(tmpPath, dstPath); err != nil {
		if movedAside {
			if rbErr := os.Rename(backupPath, dstPath); rbErr != nil {
				return fmt.Errorf(
					"install: %w (rollback also failed: %v)",
					err, rbErr,
				)
			}
		}
		return fmt.Errorf("install: %w", err)
	}

	installed = true
	os.Remove(backupPath)
	return nil
}

// movePreviousAside renames an existing dstPath to backupPath.
// Used on Windows where os.Rename cannot replace a running
// executable. Returns true if dstPath was moved.
func movePreviousAside(dstPath, backupPath string) (bool, error) {
	if _, err := os.Stat(dstPath); err != nil {
		return false, nil
	}
	if err := os.Rename(dstPath, backupPath); err != nil {
		return false, fmt.Errorf("backup: %w", err)
	}
	return true, nil
}

// resolveLatestTag follows the /releases/latest 302 redirect to
// /releases/tag/<tag> and returns the tag. Using the HTML endpoint
// avoids api.github.com's 60-req/hr unauthenticated rate limit.
func resolveLatestTag(url string) (string, error) {
	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", updateUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 300 || resp.StatusCode >= 400 {
		return "", fmt.Errorf(
			"expected redirect from %s, got %s", url, resp.Status,
		)
	}

	loc, err := resp.Location()
	if err != nil {
		return "", fmt.Errorf("read Location header: %w", err)
	}

	const marker = "/releases/tag/"
	idx := strings.Index(loc.Path, marker)
	if idx < 0 {
		return "", fmt.Errorf(
			"unexpected redirect target %q", loc.String(),
		)
	}
	tag := loc.Path[idx+len(marker):]
	if tag == "" {
		return "", fmt.Errorf(
			"empty tag in redirect target %q", loc.String(),
		)
	}
	return tag, nil
}

// fetchContentLength does a HEAD request and returns the Content-Length
// of the eventual asset (following redirects to the S3 backend).
// Returns 0 if the size can't be determined; callers degrade gracefully.
func fetchContentLength(url string) (int64, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest("HEAD", url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", updateUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf(
			"HEAD %s returned %s", url, resp.Status,
		)
	}
	if resp.ContentLength < 0 {
		return 0, nil
	}
	return resp.ContentLength, nil
}

func downloadFile(
	url, dest string,
	totalSize int64,
	progressFn func(downloaded, total int64),
) (string, error) {
	resp, err := http.Get(url) //nolint:gosec
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download failed: %s", resp.Status)
	}

	out, err := os.Create(dest)
	if err != nil {
		return "", err
	}
	defer out.Close()

	hasher := sha256.New()
	writer := io.MultiWriter(out, hasher)

	var downloaded int64
	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			_, writeErr := writer.Write(buf[:n])
			if writeErr != nil {
				return "", writeErr
			}
			downloaded += int64(n)
			if progressFn != nil {
				progressFn(downloaded, totalSize)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return "", readErr
		}
	}

	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func extractTarGz(archivePath, destDir string) error {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}

	absDestDir, err := filepath.Abs(destDir)
	if err != nil {
		return fmt.Errorf("resolve dest dir: %w", err)
	}

	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()

	gzr, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	for {
		header, headerErr := tr.Next()
		if headerErr == io.EOF {
			break
		}
		if headerErr != nil {
			return headerErr
		}

		target, targetErr := sanitizePath(absDestDir, header.Name)
		if targetErr != nil {
			return fmt.Errorf(
				"invalid tar entry %q: %w",
				header.Name, targetErr,
			)
		}

		if header.Typeflag == tar.TypeSymlink ||
			header.Typeflag == tar.TypeLink {
			continue
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(
				filepath.Dir(target), 0o755,
			); err != nil {
				return err
			}
			outFile, createErr := os.Create(target)
			if createErr != nil {
				return createErr
			}
			if _, err := io.Copy(outFile, tr); err != nil {
				outFile.Close()
				return err
			}
			outFile.Close()
			if err := os.Chmod(
				target, os.FileMode(header.Mode),
			); err != nil {
				return err
			}
		}
	}
	return nil
}

// sanitizePath validates a path to prevent directory traversal.
func sanitizePath(destDir, name string) (string, error) {
	if strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("absolute path not allowed")
	}

	cleanName := filepath.Clean(name)
	if filepath.IsAbs(cleanName) {
		return "", fmt.Errorf("absolute path not allowed")
	}
	if strings.HasPrefix(cleanName, "..") ||
		strings.Contains(
			cleanName, string(filepath.Separator)+"..",
		) {
		return "", fmt.Errorf("path traversal not allowed")
	}

	target := filepath.Join(destDir, cleanName)
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	absDestDir, err := filepath.Abs(destDir)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(
		absTarget, absDestDir+string(filepath.Separator),
	) && absTarget != absDestDir {
		return "", fmt.Errorf("path escapes destination directory")
	}
	return target, nil
}

func extractZip(archivePath, destDir string) error {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}

	absDestDir, err := filepath.Abs(destDir)
	if err != nil {
		return fmt.Errorf("resolve dest dir: %w", err)
	}

	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer r.Close()

	for _, f := range r.File {
		target, targetErr := sanitizePath(absDestDir, f.Name)
		if targetErr != nil {
			return fmt.Errorf(
				"invalid zip entry %q: %w",
				f.Name, targetErr,
			)
		}

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}

		if err := os.MkdirAll(
			filepath.Dir(target), 0o755,
		); err != nil {
			return err
		}

		rc, openErr := f.Open()
		if openErr != nil {
			return openErr
		}

		outFile, createErr := os.Create(target)
		if createErr != nil {
			rc.Close()
			return createErr
		}

		_, copyErr := io.Copy(outFile, rc)
		closeErr := outFile.Close()
		rc.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

func fetchChecksumFromFile(
	url, assetName string,
) (string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url) //nolint:gosec
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf(
			"failed to fetch checksums: %s", resp.Status,
		)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return extractChecksum(string(body), assetName), nil
}

func extractChecksum(releaseBody, assetName string) string {
	lines := strings.Split(releaseBody, "\n")
	re := regexp.MustCompile(`(?i)[a-f0-9]{64}`)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			fname := strings.TrimPrefix(fields[1], "*")
			if fname == assetName {
				if match := re.FindString(fields[0]); match != "" {
					return strings.ToLower(match)
				}
			}
		}
	}
	return ""
}

func loadCache(cacheDir string) (*cachedCheck, error) {
	cachePath := filepath.Join(cacheDir, cacheFileName)
	data, err := os.ReadFile(cachePath)
	if err != nil {
		return nil, err
	}
	var cached cachedCheck
	if err := json.Unmarshal(data, &cached); err != nil {
		return nil, err
	}
	return &cached, nil
}

func checkCache(
	currentVersion, cleanVersion string,
	isDevBuild bool,
	cacheDir string,
) (*UpdateInfo, bool) {
	cached, err := loadCache(cacheDir)
	if err != nil {
		return nil, false
	}

	cacheWindow := cacheDuration
	if isDevBuild {
		cacheWindow = devCacheDuration
	}

	if time.Since(cached.CheckedAt) >= cacheWindow {
		return nil, false
	}

	latestVersion := strings.TrimPrefix(cached.Version, "v")

	if isDevBuild {
		// Cache only records the version, not full asset metadata.
		// Return a cacheOnly UpdateInfo so the caller can display
		// version info for --check, but re-fetches with full
		// download metadata when an install is needed.
		return &UpdateInfo{
			CurrentVersion: currentVersion,
			LatestVersion:  cached.Version,
			IsDevBuild:     true,
			cacheOnly:      true,
		}, true
	}

	if !isNewer(latestVersion, cleanVersion) {
		return nil, true
	}

	return nil, false
}

func saveCache(version, cacheDir string) {
	cached := cachedCheck{
		CheckedAt: time.Now(),
		Version:   version,
	}
	data, err := json.Marshal(cached)
	if err != nil {
		return
	}
	cachePath := filepath.Join(cacheDir, cacheFileName)
	_ = os.MkdirAll(filepath.Dir(cachePath), 0o755)
	_ = os.WriteFile(cachePath, data, 0o600)
}

func extractBaseSemver(v string) string {
	v = strings.TrimPrefix(v, "v")
	if len(v) == 0 || v[0] < '0' || v[0] > '9' {
		return ""
	}
	if !strings.Contains(v, ".") {
		return ""
	}
	if idx := strings.Index(v, "-"); idx > 0 {
		v = v[:idx]
	}
	return v
}

var gitDescribePattern = regexp.MustCompile(
	`-\d+-g[0-9a-f]+(-dirty)?$`,
)

// IsDevBuildVersion returns true if the version is a dev build.
func IsDevBuildVersion(v string) bool {
	v = strings.TrimPrefix(v, "v")
	if extractBaseSemver(v) == "" {
		return true
	}
	return gitDescribePattern.MatchString(v)
}

// IsNewer reports whether v1 is a semver release newer than v2. Dev builds
// and non-semver strings are not considered newer.
func IsNewer(v1, v2 string) bool {
	base1 := extractBaseSemver(v1)
	base2 := extractBaseSemver(v2)
	if base1 == "" || base2 == "" {
		return false
	}
	sv1 := normalizeSemver(v1)
	sv2 := normalizeSemver(v2)
	return semver.Compare(sv1, sv2) > 0
}

func isNewer(v1, v2 string) bool {
	return IsNewer(v1, v2)
}

var prereleaseNumericPattern = regexp.MustCompile(
	`^([A-Za-z]+)(\d+)$`,
)

func normalizeSemver(v string) string {
	v = strings.TrimPrefix(v, "v")
	if gitDescribePattern.MatchString(v) {
		v = gitDescribePattern.ReplaceAllString(v, "")
	}
	if idx := strings.Index(v, "-"); idx > 0 {
		base := v[:idx]
		prerelease := v[idx+1:]
		prerelease = normalizePrereleaseIdentifiers(prerelease)
		v = base + "-" + prerelease
	}
	return "v" + v
}

func normalizePrereleaseIdentifiers(prerelease string) string {
	parts := strings.Split(prerelease, ".")
	var result []string
	for _, part := range parts {
		matches := prereleaseNumericPattern.FindStringSubmatch(part)
		if matches != nil {
			letters, digits := matches[1], matches[2]
			if len(digits) > 1 && digits[0] == '0' {
				result = append(result, part)
			} else {
				result = append(result, letters, digits)
			}
		} else {
			result = append(result, part)
		}
	}
	return strings.Join(result, ".")
}

// FormatSize formats bytes as a human-readable string.
func FormatSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf(
		"%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp],
	)
}

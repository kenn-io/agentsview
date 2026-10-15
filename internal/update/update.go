package update

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"go.kenn.io/kit/atomicfile"
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
	// rawBinary is set for rolling builds, which publish the bare
	// executable instead of an archive.
	rawBinary bool
	// cacheOnly is set when the info lacks download metadata because
	// it came from cache or a display-only rolling check. The caller
	// must re-fetch for installs.
	cacheOnly bool
}

// NeedsRefetch returns true when the info is display-only and
// lacks the download URL/checksum needed for an install.
func (u *UpdateInfo) NeedsRefetch() bool {
	return u.cacheOnly
}

type cachedCheck struct {
	CheckedAt time.Time `json:"checked_at"`
	Version   string    `json:"version"`
}

// CheckForUpdate checks if a newer version is available.
// Uses a 1-hour cache to avoid hitting the GitHub API often.
// Rolling builds check their own repository's rolling release and
// cache for 15 minutes; see checkRollingUpdate.
func CheckForUpdate(ctx context.Context,
	currentVersion string,
	forceCheck bool,
	cacheDir string,
) (*UpdateInfo, error) {
	if rollingRepo != "" {
		return checkRollingUpdate(ctx,
			rollingDownloadRoot(rollingRepo),
			currentVersion, forceCheck, cacheDir,
		)
	}

	cleanVersion := strings.TrimPrefix(currentVersion, "v")
	isDevBuild := IsDevBuildVersion(cleanVersion)

	if !forceCheck {
		if info, done := checkCache(
			currentVersion, cleanVersion, isDevBuild, cacheDir,
		); done {
			return info, nil
		}
	}

	tag, err := resolveLatestTag(ctx, githubLatestReleaseURL)
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
	size, err := fetchContentLength(ctx, downloadURL)
	if err != nil {
		return nil, fmt.Errorf(
			"no release asset for %s/%s: %w",
			runtime.GOOS, runtime.GOARCH, err,
		)
	}

	checksum, _ := fetchChecksumFromFile(ctx, checksumsURL, assetName)

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
func PerformUpdate(ctx context.Context,
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
	downloadChecksum, err := downloadFile(ctx,
		info.DownloadURL, archivePath, info.Size, progressFn,
	)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}

	if progressFn != nil {
		fmt.Println()
	}
	fmt.Println("Verifying and installing...")
	dstPath, err := installTarget(info.rawBinary)
	if err != nil {
		return err
	}
	install := installFromArchiveTo
	if info.rawBinary {
		install = installRawBinaryTo
	}
	if err := install(
		archivePath, info.Checksum, dstPath, downloadChecksum,
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

// executablePath locates the running program. Tests replace it.
var executablePath = os.Executable

// binaryName is the file name of the agentsview executable that
// release archives contain and archive updates install.
func binaryName() string {
	if runtime.GOOS == "windows" {
		return "agentsview.exe"
	}
	return "agentsview"
}

// installTarget returns the file an update replaces. Archive updates
// install the agentsview binary beside the running executable. Raw
// binary updates replace the running executable itself, whatever its
// name, so the next run and a restarted daemon use the new build.
func installTarget(rawBinary bool) (string, error) {
	currentExe, err := executablePath()
	if err != nil {
		return "", fmt.Errorf("find current executable: %w", err)
	}
	currentExe, err = filepath.EvalSymlinks(currentExe)
	if err != nil {
		return "", fmt.Errorf("resolve symlinks: %w", err)
	}
	if rawBinary {
		return currentExe, nil
	}
	return filepath.Join(filepath.Dir(currentExe), binaryName()), nil
}

// verifyChecksum checks path against expectedChecksum. A non-empty
// precomputedChecksum is trusted instead of rehashing the file.
func verifyChecksum(
	path, expectedChecksum, precomputedChecksum string,
) error {
	if expectedChecksum == "" {
		return errors.New("empty checksum - refusing unverified binary")
	}

	checksum := precomputedChecksum
	if checksum == "" {
		var err error
		checksum, err = hashFile(path)
		if err != nil {
			return fmt.Errorf("hash download: %w", err)
		}
	}

	if !strings.EqualFold(checksum, expectedChecksum) {
		return fmt.Errorf(
			"checksum mismatch: expected %s, got %s",
			expectedChecksum, checksum,
		)
	}
	return nil
}

func installFromArchiveTo(
	archivePath, expectedChecksum, dstPath string,
	precomputedChecksum string,
) error {
	if err := verifyChecksum(
		archivePath, expectedChecksum, precomputedChecksum,
	); err != nil {
		return err
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

	srcPath := filepath.Join(extractDir, binaryName())
	if _, err := os.Stat(srcPath); os.IsNotExist(err) {
		return fmt.Errorf(
			"binary %s not found in archive", binaryName(),
		)
	}

	return installBinaryTo(srcPath, dstPath)
}

// installBinaryTo replaces the binary at dstPath with the one
// at srcPath. The new binary is staged in a sibling tmp file
// with the executable mode bit set, then renamed into place.
//
// On Unix atomicfile.Replace atomically replaces dstPath in a
// single syscall, so concurrent readers always see one of the two
// binaries — never a missing or partial file. On Windows the
// existing binary must be moved aside first because Replace
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

	if err := atomicfile.Replace(tmpPath, dstPath); err != nil {
		if movedAside {
			if rbErr := atomicfile.Replace(backupPath, dstPath); rbErr != nil {
				return fmt.Errorf(
					"install: %w (rollback also failed: %w)",
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
// Used on Windows where atomicfile.Replace cannot replace a running
// executable. Returns true if dstPath was moved.
func movePreviousAside(dstPath, backupPath string) (bool, error) {
	if _, err := os.Stat(dstPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("checking installed executable: %w", err)
	}
	if err := atomicfile.Replace(dstPath, backupPath); err != nil {
		return false, fmt.Errorf("backup: %w", err)
	}
	return true, nil
}

// resolveLatestTag follows the /releases/latest 302 redirect to
// /releases/tag/<tag> and returns the tag. Using the HTML endpoint
// avoids api.github.com's 60-req/hr unauthenticated rate limit.
func resolveLatestTag(ctx context.Context, url string) (string, error) {
	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
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
func fetchContentLength(ctx context.Context, url string) (int64, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
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

func downloadFile(ctx context.Context,
	url, dest string,
	totalSize int64,
	progressFn func(downloaded, total int64),
) (string, error) {
	client := &http.Client{Timeout: 10 * time.Minute}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download failed: %s", resp.Status)
	}
	// Report the start, so a progress display times the transfer from
	// the response rather than from the first 32 KB.
	if progressFn != nil {
		progressFn(0, totalSize)
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
		return "", errors.New("absolute path not allowed")
	}

	cleanName := filepath.Clean(name)
	if filepath.IsAbs(cleanName) {
		return "", errors.New("absolute path not allowed")
	}
	if strings.HasPrefix(cleanName, "..") ||
		strings.Contains(
			cleanName, string(filepath.Separator)+"..",
		) {
		return "", errors.New("path traversal not allowed")
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
		return "", errors.New("path escapes destination directory")
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

// maxChecksumsSize bounds a SHA256SUMS download.
const maxChecksumsSize = 1 << 20

func fetchChecksumFromFile(ctx context.Context,
	url, assetName string,
) (string, error) {
	body, err := fetchSmallAsset(ctx, url, maxChecksumsSize)
	if err != nil {
		return "", fmt.Errorf("failed to fetch checksums: %w", err)
	}
	return extractChecksum(body, assetName), nil
}

// fetchSmallAsset GETs a small text release asset, such as VERSION,
// BUILD_TAG, or SHA256SUMS. It fails on any status other than 200 and
// on bodies longer than limit bytes.
func fetchSmallAsset(ctx context.Context,
	url string, limit int64,
) (string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", updateUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", url, err)
	}
	if int64(len(body)) > limit {
		return "", fmt.Errorf("GET %s: body exceeds %d bytes", url, limit)
	}
	return string(body), nil
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
	return loadCacheFile(cacheDir, cacheFileName)
}

func loadCacheFile(cacheDir, name string) (*cachedCheck, error) {
	data, err := os.ReadFile(filepath.Join(cacheDir, name))
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
	saveCacheFile(version, cacheDir, cacheFileName)
}

func saveCacheFile(version, cacheDir, name string) {
	cached := cachedCheck{
		CheckedAt: time.Now(),
		Version:   version,
	}
	data, err := json.Marshal(cached)
	if err != nil {
		return
	}
	cachePath := filepath.Join(cacheDir, name)
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

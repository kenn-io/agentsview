package sshremote

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"go.kenn.io/agentsview/internal/remotesync"
)

// remoteBinDir is expanded by the remote shell.
const remoteBinDir = `$HOME/.cache/agentsview/bin`

// identityPattern keeps an identity a single safe word inside the remote
// scripts' double-quoted paths.
var identityPattern = regexp.MustCompile(`^[0-9A-Za-z._+-]+$`)

type remoteBinary struct {
	// shellPath is a shell word naming the executable on the remote.
	shellPath string
	// present is a shell test that succeeds while the executable exists.
	present string
}

func cachedBinary(id string) remoteBinary {
	path := `"` + remoteBinDir + "/" + id + `/agentsview"`
	return remoteBinary{shellPath: path, present: "[ -e " + path + " ]"}
}

var pathBinary = remoteBinary{
	shellPath: "agentsview",
	present:   "command -v agentsview >/dev/null 2>&1",
}

func requireIdentity(id string) error {
	if !identityPattern.MatchString(id) {
		return fmt.Errorf("invalid remote binary identity %q", id)
	}
	return nil
}

// pathProbeVersions are the `agentsview version` outputs, minus the
// "agentsview " prefix, of builds that reject an unknown command. Builds
// before v0.23.0 run the server for any unknown arguments, so the probe
// would never return on them. Every release prints its version and exits
// for `version`, which makes that the one safe question to ask first.
// Builds whose version is not a release number, such as "dev" or the bare
// commit hash a clone without tags reports, are skipped because their age is
// unknown. Hex hashes contain no dot, so every pattern requires one.
const pathProbeVersions = `v0.2[3-9]*|0.2[3-9]*|v0.[3-9][0-9]*|0.[3-9][0-9]*|` +
	`v0.[1-9][0-9][0-9]*|0.[1-9][0-9][0-9]*|v[1-9]*.*|[1-9]*.*`

// probeScript reports the platform, probes each cached identity (touching
// .last-used for every one that answers), then probes agentsview on PATH
// when its version is new enough to answer safely. Each report line ends
// with a separate echo so no format string needs a backslash.
func probeScript(ids []string) (string, error) {
	for _, id := range ids {
		if err := requireIdentity(id); err != nil {
			return "", err
		}
	}
	script := strings.Join([]string{
		`bin="` + remoteBinDir + `"`,
		`echo "os=$(uname -s)"`,
		`echo "arch=$(uname -m)"`,
		// A refreshed identity is safe from retention for 30 days; one that
		// cannot be refreshed may be removed, so it is a miss.
		`for id in ` + strings.Join(ids, " ") + `; do ` +
			`if [ -x "$bin/$id/agentsview" ] && ` +
			`out=$("$bin/$id/agentsview" remote-sync probe 2>/dev/null) && ` +
			`touch "$bin/$id/.last-used" 2>/dev/null; then ` +
			`printf "cached %s %s" "$id" "$out"; echo; fi; done`,
		`if command -v agentsview >/dev/null 2>&1; then ` +
			`v=$(agentsview version </dev/null 2>/dev/null) || v=""; ` +
			`case "${v#agentsview }" in ` + pathProbeVersions + `) ` +
			`if out=$(agentsview remote-sync probe </dev/null 2>/dev/null); then ` +
			`printf "path %s" "$out"; echo; fi;; esac; fi`,
	}, "; ")
	return script, validateScript(script)
}

// installScript reads a gzip tarball on stdin and installs its agentsview as
// identity id, then applies retention. Renaming the file (not a directory)
// keeps concurrent installs of one identity harmless. Retention is best
// effort: once the binary is in place, a directory another install removed
// first, or one the user cannot delete, must not fail the install.
func installScript(id string) (string, error) {
	if err := requireIdentity(id); err != nil {
		return "", err
	}
	script := strings.Join([]string{
		`set -eu`,
		`bin="` + remoteBinDir + `"`,
		`id="` + id + `"`,
		`mkdir -p "$bin"`,
		`tmp=$(mktemp -d "$bin/.tmp-XXXXXX")`,
		`cleanup() { rm -rf "$tmp"; }`,
		`trap cleanup EXIT`,
		`tar -xzf - -C "$tmp"`,
		`[ -f "$tmp/agentsview" ] || ` +
			`{ echo "archive has no agentsview executable" >&2; exit 3; }`,
		`chmod 755 "$tmp/agentsview"`,
		`"$tmp/agentsview" remote-sync probe >/dev/null`,
		`mkdir -p "$bin/$id"`,
		`mv -f "$tmp/agentsview" "$bin/$id/agentsview"`,
		`touch "$bin/$id/.last-used"`,
		`find "$bin" -mindepth 1 -maxdepth 1 -type d -name ".tmp-*" -mtime +0 ` +
			`-exec rm -rf {} + || true`,
		`for d in "$bin"/*; do [ -d "$d" ] || continue; [ "$d" = "$bin/$id" ] && continue; ` +
			`m="$d"; [ -f "$d/.last-used" ] && m="$d/.last-used"; ` +
			`if [ -n "$(find "$m" -prune -mtime +30)" ]; then rm -rf "$d"; fi; done || true`,
	}, "; ")
	return script, validateScript(script)
}

type probeReport struct {
	unameS, unameM string
	cached         map[string]ProbeResult
	path           *ProbeResult
}

// platform maps the remote uname output to a release platform. Only an
// upload needs it, so an unsupported host can still use its own agentsview.
func (r probeReport) platform() (platform, error) {
	return parsePlatform(r.unameS, r.unameM)
}

// parseProbe reads the probe script's output. Unknown lines, such as text a
// shell start-up file printed, are ignored.
func parseProbe(out []byte) (probeReport, error) {
	report := probeReport{cached: map[string]ProbeResult{}}
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "os="):
			report.unameS = strings.TrimPrefix(line, "os=")
		case strings.HasPrefix(line, "arch="):
			report.unameM = strings.TrimPrefix(line, "arch=")
		case strings.HasPrefix(line, "cached "):
			id, payload, ok := strings.Cut(strings.TrimPrefix(line, "cached "), " ")
			var result ProbeResult
			if ok && json.Unmarshal([]byte(payload), &result) == nil {
				report.cached[id] = result
			}
		case strings.HasPrefix(line, "path "):
			var result ProbeResult
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "path ")), &result) == nil {
				report.path = &result
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return probeReport{}, fmt.Errorf("read remote probe output: %w", err)
	}
	return report, nil
}

// Provisioner finds or installs a usable agentsview on one remote host.
type Provisioner struct {
	Runner  Runner
	Build   LocalBuild
	DataDir string
	// Host names the remote in progress messages.
	Host string
	// Progress, when set, receives a detail line when an upload starts. An
	// upload can move tens of megabytes before the sync shows other output.
	Progress func(detail string)
	releases releaseFetcher
}

// Ensure returns the remote binary to run for this sync.
func (p Provisioner) Ensure(ctx context.Context) (remoteBinary, error) {
	releaseID, isRelease := p.Build.ReleaseID()
	contentID, err := p.Build.ContentID()
	if err != nil {
		return remoteBinary{}, &ProvisionError{Err: err}
	}
	ids := []string{contentID}
	if isRelease {
		ids = []string{releaseID, contentID}
	}
	script, err := probeScript(ids)
	if err != nil {
		return remoteBinary{}, err
	}
	out, err := p.Runner.Output(ctx, script, nil)
	if err != nil {
		return remoteBinary{}, fmt.Errorf("probe remote agentsview: %w", err)
	}
	report, err := parseProbe(out)
	if err != nil {
		return remoteBinary{}, err
	}
	for _, id := range ids {
		result, ok := report.cached[id]
		if ok && result.ProtocolVersion == remotesync.ProtocolVersion {
			return cachedBinary(id), nil
		}
	}
	if report.path != nil && report.path.ProtocolVersion == remotesync.ProtocolVersion {
		return pathBinary, nil
	}
	remote, err := report.platform()
	if err != nil {
		return remoteBinary{}, &ProvisionError{Err: err}
	}
	return p.install(ctx, remote, releaseID, isRelease, contentID)
}

func (p Provisioner) install(
	ctx context.Context, remote platform, releaseID string, isRelease bool, contentID string,
) (remoteBinary, error) {
	if isRelease {
		fetcher := p.releases
		if fetcher.CacheDir == "" {
			fetcher.CacheDir = filepath.Join(p.DataDir, "ssh-binaries")
		}
		tarball, err := fetcher.Fetch(ctx, p.Build.releaseVersion(), remote)
		switch {
		case err == nil:
			return p.installTarball(ctx, releaseID, tarball)
		case !errors.Is(err, errReleaseNotFound):
			return remoteBinary{}, &ProvisionError{Err: err}
		}
	}
	if remote != localPlatform() {
		return remoteBinary{}, &ProvisionError{Err: fmt.Errorf(
			"no agentsview release matches this build (%s) for remote platform %s; "+
				"install agentsview on the remote or sync from a release build",
			p.Build.Version, remote)}
	}
	return p.installSelf(ctx, contentID)
}

// installTarball streams a verified release tarball to the install script.
func (p Provisioner) installTarball(ctx context.Context, id, path string) (remoteBinary, error) {
	f, err := os.Open(path)
	if err != nil {
		return remoteBinary{}, &ProvisionError{Err: fmt.Errorf("open release tarball: %w", err)}
	}
	defer f.Close()
	return p.runInstall(ctx, id, f)
}

// installSelf packs the local executable as a gzip tarball and streams it to
// the install script.
func (p Provisioner) installSelf(ctx context.Context, id string) (remoteBinary, error) {
	pr, pw := io.Pipe()
	packed := make(chan error, 1)
	go func() {
		err := packExecutable(pw, p.Build.Executable)
		_ = pw.CloseWithError(err)
		packed <- err
	}()
	bin, err := p.runInstall(ctx, id, pr)
	// ssh may stop reading before the end, for example when it fails.
	// Closing the reader releases a packer blocked on a write.
	_ = pr.Close()
	if packErr := <-packed; packErr != nil && !errors.Is(packErr, io.ErrClosedPipe) {
		return remoteBinary{}, &ProvisionError{Err: packErr}
	}
	return bin, err
}

func (p Provisioner) runInstall(
	ctx context.Context, id string, tarball io.Reader,
) (remoteBinary, error) {
	script, err := installScript(id)
	if err != nil {
		return remoteBinary{}, err
	}
	if p.Progress != nil {
		p.Progress("Installing agentsview on " + p.Host)
	}
	if _, err := p.Runner.Output(ctx, script, tarball); err != nil {
		return remoteBinary{}, fmt.Errorf("install agentsview %s on remote: %w", id, err)
	}
	return cachedBinary(id), nil
}

// packExecutable writes a gzip tar holding the file at path as agentsview.
func packExecutable(w io.Writer, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open local executable: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat local executable: %w", err)
	}
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name:     "agentsview",
		Mode:     0o755,
		Size:     info.Size(),
		ModTime:  info.ModTime(),
		Typeflag: tar.TypeReg,
	}); err != nil {
		return fmt.Errorf("pack local executable: %w", err)
	}
	if _, err := io.Copy(tw, f); err != nil {
		return fmt.Errorf("pack local executable: %w", err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("pack local executable: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("pack local executable: %w", err)
	}
	return nil
}

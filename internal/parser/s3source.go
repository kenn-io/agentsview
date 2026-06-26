// ABOUTME: Reads Claude/Codex session JSONL directly from an S3-compatible
// ABOUTME: object store (AWS S3, MinIO, Aliyun OSS, R2, ...) — pure Go, no cgo.
package parser

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Object carries the durable source metadata needed for sync skip checks.
type S3Object struct {
	URI          string
	Size         int64
	LastModified time.Time
	Fingerprint  string
}

var (
	listS3Objects = listS3
	fetchS3Object = fetchS3ObjectDefault
	statS3Object  = statS3ObjectDefault
)

// s3Client builds an S3-compatible client from standard env vars:
//
//	AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_REGION
//	AWS_S3_ENDPOINT  — host of an S3-compatible endpoint (e.g.
//	                   "oss-cn-shenzhen.aliyuncs.com"); empty = AWS S3.
//	                   An "http://" prefix selects insecure transport for
//	                   loopback endpoints, or with explicit unsafe opt-in.
//
// Returning an error here means an s3:// source simply yields nothing,
// so a misconfigured store never aborts the local sync.
func s3Client() (*minio.Client, error) {
	endpoint, secure, err := s3EndpointConfig(os.Getenv("AWS_S3_ENDPOINT"))
	if err != nil {
		return nil, err
	}
	return minio.New(endpoint, &minio.Options{
		Creds:  s3Credentials(),
		Secure: secure,
		Region: os.Getenv("AWS_REGION"),
	})
}

func s3EndpointConfig(raw string) (endpoint string, secure bool, err error) {
	endpoint = strings.TrimSpace(raw)
	secure = true
	switch {
	case endpoint == "":
		return "s3.amazonaws.com", true, nil
	case strings.HasPrefix(endpoint, "http://"):
		secure, endpoint = false, strings.TrimPrefix(endpoint, "http://")
	case strings.HasPrefix(endpoint, "https://"):
		endpoint = strings.TrimPrefix(endpoint, "https://")
	case strings.Contains(endpoint, "://"):
		return "", false, fmt.Errorf("unsupported S3 endpoint scheme: %q", raw)
	}

	if !secure && !isLoopbackS3Endpoint(endpoint) &&
		!allowUnsafeS3Endpoint() {
		return "", false, fmt.Errorf(
			"insecure S3 endpoint %q is only allowed for loopback hosts; "+
				"set AGENTSVIEW_ALLOW_INSECURE_S3_ENDPOINT=true to override",
			raw,
		)
	}
	return endpoint, secure, nil
}

func allowUnsafeS3Endpoint() bool {
	switch strings.ToLower(os.Getenv("AGENTSVIEW_ALLOW_INSECURE_S3_ENDPOINT")) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

func isLoopbackS3Endpoint(endpoint string) bool {
	host := endpoint
	if before, _, ok := strings.Cut(host, "/"); ok {
		host = before
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(strings.ToLower(host), "[]")
	if host == "localhost" {
		return true
	}
	if before, _, ok := strings.Cut(host, "%"); ok {
		host = before
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func s3Credentials() *credentials.Credentials {
	return credentials.NewStaticV4(
		os.Getenv("AWS_ACCESS_KEY_ID"),
		os.Getenv("AWS_SECRET_ACCESS_KEY"),
		os.Getenv("AWS_SESSION_TOKEN"),
	)
}

// parseS3URI splits s3://bucket/key into (bucket, key).
func parseS3URI(uri string) (bucket, key string) {
	rest := strings.TrimPrefix(uri, "s3://")
	if before, after, ok := strings.Cut(rest, "/"); ok {
		return before, after
	}
	return rest, ""
}

// listS3 lists every object under an s3://bucket/prefix, returning each
// object's full s3:// URI plus source metadata.
func listS3(uri string) ([]S3Object, error) {
	cl, err := s3Client()
	if err != nil {
		return nil, err
	}
	bucket, prefix := parseS3URI(uri)
	if prefix != "" {
		prefix = strings.TrimSuffix(prefix, "/") + "/"
	}
	var out []S3Object
	for o := range cl.ListObjects(context.Background(), bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	}) {
		if o.Err != nil {
			return nil, o.Err
		}
		out = append(out, S3Object{
			URI:          "s3://" + bucket + "/" + o.Key,
			Size:         o.Size,
			LastModified: o.LastModified,
			Fingerprint:  s3ObjectFingerprint("s3://"+bucket+"/"+o.Key, o),
		})
	}
	return out, nil
}

// FetchS3Object opens one s3://bucket/key object for streaming reads. The
// caller owns the returned reader and must close it. minio fetches lazily,
// so transport errors surface on the first Read rather than here.
func FetchS3Object(uri string) (io.ReadCloser, error) {
	return fetchS3Object(uri)
}

func fetchS3ObjectDefault(uri string) (io.ReadCloser, error) {
	cl, err := s3Client()
	if err != nil {
		return nil, err
	}
	bucket, key := parseS3URI(uri)
	return cl.GetObject(context.Background(), bucket, key, minio.GetObjectOptions{})
}

// StatS3Object returns durable object metadata for an s3:// URI.
func StatS3Object(uri string) (S3Object, error) {
	return statS3Object(uri)
}

// StatClaudeS3Session returns metadata for a Claude transcript plus matching
// tool-result sidecars that can change parsed content without changing JSONL.
func StatClaudeS3Session(uri string) (S3Object, error) {
	obj, err := statS3Object(uri)
	if err != nil {
		return S3Object{}, err
	}
	return foldClaudeS3SidecarMetadata(obj, func(root string) []S3Object {
		objects, err := listS3Objects(root)
		if err != nil {
			return nil
		}
		return objects
	}), nil
}

// StatCodexS3Session returns metadata for a Codex rollout object.
func StatCodexS3Session(uri string) (S3Object, error) {
	return statS3Object(uri)
}

func statS3ObjectDefault(uri string) (S3Object, error) {
	cl, err := s3Client()
	if err != nil {
		return S3Object{}, err
	}
	bucket, key := parseS3URI(uri)
	info, err := cl.StatObject(
		context.Background(), bucket, key, minio.StatObjectOptions{},
	)
	if err != nil {
		return S3Object{}, err
	}
	return S3Object{
		URI:          uri,
		Size:         info.Size,
		LastModified: info.LastModified,
		Fingerprint:  s3ObjectFingerprint(uri, info),
	}, nil
}

func s3RelativePath(root, uri string) (string, bool) {
	prefix := strings.TrimSuffix(root, "/") + "/"
	rel := strings.TrimPrefix(uri, prefix)
	return rel, rel != uri
}

// s3MachineFromRoot derives the source machine from an s3:// session root laid
// out as .../<machine>/raw/<provider>, i.e. the path segment immediately
// preceding the "raw/<provider>" boundary. provider is the agent's path segment
// (string(Agent)), so the rule generalizes to any agent that adopts the same
// layout rather than being limited to Claude/Codex. Returns "" when not found,
// so callers fall back to the agentsview host machine name. This mirrors the
// host prefix that SSH remote sync attaches to pulled sessions.
func s3MachineFromRoot(root, provider string) string {
	// segs[0] is the bucket, so "raw" must be at index >= 2 for the
	// preceding segment to be a machine directory rather than the bucket.
	segs := strings.Split(strings.TrimPrefix(root, "s3://"), "/")
	for i := len(segs) - 2; i > 1; i-- {
		if segs[i] == "raw" && segs[i+1] == provider {
			return segs[i-1]
		}
	}
	return ""
}

func foldClaudeS3SidecarMetadata(
	obj S3Object, list func(root string) []S3Object,
) S3Object {
	for _, root := range claudeS3SidecarRoots(obj.URI) {
		for _, sidecar := range list(root) {
			obj = foldS3ObjectMetadata(obj, sidecar)
		}
	}
	return obj
}

func claudeS3SidecarRoots(uri string) []string {
	sessionPath := strings.TrimSuffix(uri, ".jsonl")
	if sessionPath == "" || sessionPath == uri {
		return nil
	}
	roots := []string{sessionPath + "/tool-results"}
	if strings.HasPrefix(pathBase(sessionPath), "agent-") {
		if idx := strings.LastIndex(sessionPath, "/subagents/"); idx > 0 {
			roots = append(roots, sessionPath[:idx]+"/tool-results")
		}
	}
	return roots
}

func pathBase(p string) string {
	if idx := strings.LastIndex(p, "/"); idx >= 0 {
		return p[idx+1:]
	}
	return p
}

func foldS3ObjectMetadata(obj, extra S3Object) S3Object {
	obj.Size += extra.Size
	if extra.LastModified.After(obj.LastModified) {
		obj.LastModified = extra.LastModified
	}
	obj.Fingerprint = combineS3Fingerprints(
		obj.Fingerprint, extra.Fingerprint,
	)
	return obj
}

func s3ObjectFingerprint(uri string, info minio.ObjectInfo) string {
	parts := []string{
		"etag=" + strings.Trim(info.ETag, `"`),
		"version=" + info.VersionID,
		"crc32=" + info.ChecksumCRC32,
		"crc32c=" + info.ChecksumCRC32C,
		"sha1=" + info.ChecksumSHA1,
		"sha256=" + info.ChecksumSHA256,
		"crc64nvme=" + info.ChecksumCRC64NVME,
		"md5=" + info.ChecksumMD5,
		"sha512=" + info.ChecksumSHA512,
		"xxhash64=" + info.ChecksumXXHash64,
		"xxhash3=" + info.ChecksumXXHash3,
		"xxhash128=" + info.ChecksumXXHash128,
	}
	nonEmpty := parts[:0]
	for _, part := range parts {
		if !strings.HasSuffix(part, "=") {
			nonEmpty = append(nonEmpty, part)
		}
	}
	if len(nonEmpty) == 0 {
		return ""
	}
	sort.Strings(nonEmpty)
	return combineS3Fingerprints(uri + "\x00" + strings.Join(nonEmpty, "\x00"))
}

func combineS3Fingerprints(values ...string) string {
	const prefix = "s3-meta:"
	const sep = "\x1e"
	var entries []string
	for _, value := range values {
		if value == "" {
			continue
		}
		value = strings.TrimPrefix(value, prefix)
		for entry := range strings.SplitSeq(value, sep) {
			if entry != "" {
				entries = append(entries, entry)
			}
		}
	}
	if len(entries) == 0 {
		return ""
	}
	sort.Strings(entries)
	entries = slices.Compact(entries)
	return prefix + strings.Join(entries, sep)
}

// S3DiscoveredSource is the Opaque payload an S3-aware source set attaches to a
// discovered s3:// SourceRef. It carries the durable object metadata the sync
// engine threads back into the DiscoveredFile so S3 freshness, dedup, mtime
// cutoff, and machine-ID namespacing operate on a provider-discovered S3 source
// exactly as they did when discovery emitted these fields directly. Providers
// read local files and cannot Fingerprint an s3:// URI, so the engine routes
// s3:// objects to the dedicated S3 sync path: it re-stats and fetches the
// object itself, then parses the fetched temp file through provider.Parse via a
// MaterializedFileSource. The metadata threaded here is what lets the
// incremental cutoff and skip checks run without performing that fetch.
type S3DiscoveredSource struct {
	URI         string
	Project     string
	Machine     string
	Size        int64
	MtimeNS     int64
	Fingerprint string
}

// s3SourceRefFromDiscoveredFile builds the SourceRef for an s3:// session object
// enumerated by a source set's discovery. The s3 URI is the stable identity
// across Key, DisplayPath, and FingerprintKey, and the durable object metadata
// rides in the Opaque payload for the engine to thread into the DiscoveredFile.
func s3SourceRefFromDiscoveredFile(file DiscoveredFile) SourceRef {
	return SourceRef{
		Provider:       file.Agent,
		Key:            file.Path,
		DisplayPath:    file.Path,
		FingerprintKey: file.Path,
		ProjectHint:    file.Project,
		Opaque: S3DiscoveredSource{
			URI:         file.Path,
			Project:     file.Project,
			Machine:     file.Machine,
			Size:        file.SourceSize,
			MtimeNS:     file.SourceMtime,
			Fingerprint: file.SourceFingerprint,
		},
	}
}

// s3SessionScanner configures the shared S3 discovery scan over a session root
// laid out as .../<machine>/raw/<provider>. The scan lists every object under
// the root, derives the source machine from that layout, and emits a
// DiscoveredFile for each object Keep accepts. Keep and Project receive both the
// raw relative path and its pre-split segments so a provider expresses its
// selection and project rules without re-splitting. Sidecars, when set, returns
// the companion objects whose size/mtime/fingerprint fold into the session's
// freshness identity (Claude tool-results); providers without sidecars leave it
// nil, and providers that derive the project from session content leave Project
// nil.
type s3SessionScanner struct {
	Agent    AgentType
	Keep     func(rel string, segs []string) bool
	Project  func(rel string, segs []string) string
	Sidecars func(uri string, all []S3Object) []S3Object
}

// s3PrefixScan is the shared S3 discovery body for the
// .../<machine>/raw/<provider> layout. discoverClaudeS3 and discoverCodexS3 are
// thin configurations of it, and any JSONL provider whose sessions land under
// the same layout can reuse it by supplying its own Keep/Project predicates.
func s3PrefixScan(root string, scan s3SessionScanner) []DiscoveredFile {
	objects, err := listS3Objects(root)
	if err != nil {
		return nil
	}
	machine := s3MachineFromRoot(root, string(scan.Agent))
	var out []DiscoveredFile
	for _, obj := range objects {
		rel, ok := s3RelativePath(root, obj.URI)
		if !ok {
			continue
		}
		segs := strings.Split(rel, "/")
		if !scan.Keep(rel, segs) {
			continue
		}
		source := obj
		if scan.Sidecars != nil {
			for _, sidecar := range scan.Sidecars(obj.URI, objects) {
				source = foldS3ObjectMetadata(source, sidecar)
			}
		}
		project := ""
		if scan.Project != nil {
			project = scan.Project(rel, segs)
		}
		out = append(out, DiscoveredFile{
			Path:              obj.URI,
			Project:           project,
			Agent:             scan.Agent,
			Machine:           machine,
			SourceSize:        source.Size,
			SourceMtime:       source.LastModified.UnixNano(),
			SourceFingerprint: source.Fingerprint,
		})
	}
	return out
}

// discoverClaudeS3 lists Claude session JSONL under an s3:// projects root,
// mirroring DiscoverClaudeProjects' selection rules:
//   - top-level <project>/<uuid>.jsonl   (skip names starting "agent-")
//   - subagents .../subagents/.../agent-*.jsonl
//
// Project is the first path segment under the root (e.g. "-home-user-proj").
func discoverClaudeS3(root string) []DiscoveredFile {
	return s3PrefixScan(root, s3SessionScanner{
		Agent:    AgentClaude,
		Keep:     keepClaudeS3Session,
		Project:  func(_ string, segs []string) string { return segs[0] },
		Sidecars: claudeS3SidecarObjects,
	})
}

// keepClaudeS3Session selects Claude transcript objects: a top-level
// <project>/<uuid>.jsonl (excluding agent-* names and any subagents path), or a
// subagent under .../subagents/.../agent-*.jsonl.
func keepClaudeS3Session(rel string, segs []string) bool {
	if !strings.HasSuffix(rel, ".jsonl") || len(segs) < 2 {
		return false
	}
	base := segs[len(segs)-1]
	if len(segs) >= 4 && segs[2] == "subagents" {
		return strings.HasPrefix(base, "agent-")
	}
	return len(segs) == 2 && !strings.HasPrefix(base, "agent-") &&
		!slices.Contains(segs, "subagents")
}

// claudeS3SidecarObjects returns the tool-results sidecar objects (under the
// session's tool-results prefix, plus the parent session's for subagents) whose
// metadata folds into the transcript's freshness identity. It filters the bulk
// listing rather than re-listing per prefix, since the scan already holds every
// object under the root.
func claudeS3SidecarObjects(uri string, all []S3Object) []S3Object {
	var matched []S3Object
	for _, sidecarRoot := range claudeS3SidecarRoots(uri) {
		prefix := strings.TrimSuffix(sidecarRoot, "/") + "/"
		for _, candidate := range all {
			if strings.HasPrefix(candidate.URI, prefix) {
				matched = append(matched, candidate)
			}
		}
	}
	return matched
}

// discoverCodexS3 lists Codex rollout-*.jsonl under an s3:// sessions root
// (any depth — Codex nests under 2026/MM/DD/). Project is derived from
// session content, so it is left empty here, as in the local path.
func discoverCodexS3(root string) []DiscoveredFile {
	return s3PrefixScan(root, s3SessionScanner{
		Agent: AgentCodex,
		Keep: func(_ string, segs []string) bool {
			return isCodexSessionFilename(segs[len(segs)-1])
		},
	})
}

// CodexS3SessionIndexURI returns the session_index.jsonl URI adjacent to the
// configured Codex sessions root represented by a rollout URI.
func CodexS3SessionIndexURI(sessionURI string) (string, bool) {
	if !strings.HasPrefix(sessionURI, "s3://") {
		return "", false
	}
	trimmed := strings.TrimPrefix(sessionURI, "s3://")
	parts := strings.Split(trimmed, "/")
	if len(parts) < 2 || !isCodexSessionFilename(parts[len(parts)-1]) {
		return "", false
	}

	for i := len(parts) - 3; i >= 1; i-- {
		if parts[i] == "raw" && parts[i+1] == "codex" {
			rootEnd := i + 2
			if rootEnd < len(parts)-1 &&
				(parts[rootEnd] == "sessions" ||
					parts[rootEnd] == "archived_sessions") {
				return s3URIWithLast(parts[:rootEnd], CodexSessionIndexFilename), true
			}
			return s3URIWithLast(parts[:i+1], CodexSessionIndexFilename), true
		}
	}

	for i := len(parts) - 2; i >= 1; i-- {
		if parts[i] == "sessions" || parts[i] == "archived_sessions" {
			return s3URIWithLast(parts[:i], CodexSessionIndexFilename), true
		}
	}

	sessionRootEnd := len(parts) - 1
	if len(parts) >= 5 &&
		IsDigits(parts[len(parts)-4]) &&
		IsDigits(parts[len(parts)-3]) &&
		IsDigits(parts[len(parts)-2]) {
		sessionRootEnd = len(parts) - 4
	}
	if sessionRootEnd <= 0 {
		return "", false
	}
	parent := parts[:sessionRootEnd]
	if len(parent) > 1 {
		parent = parent[:len(parent)-1]
	}
	return s3URIWithLast(parent, CodexSessionIndexFilename), true
}

func s3URIWithLast(parts []string, last string) string {
	out := make([]string, 0, len(parts)+1)
	out = append(out, parts...)
	out = append(out, last)
	return "s3://" + strings.Join(out, "/")
}

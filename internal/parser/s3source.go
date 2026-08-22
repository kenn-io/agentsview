// ABOUTME: Reads session JSONL directly from an S3-compatible
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
func s3SourceRefFromDiscoveredFile(root string, file DiscoveredFile) SourceRef {
	return SourceRef{
		Provider:       file.Agent,
		ConfiguredRoot: root,
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

// S3SessionScanner configures the shared S3 discovery scan over a session root
// laid out as .../<machine>/raw/<provider>. The scan lists every object under
// the root, derives the source machine from that layout, and emits a
// DiscoveredFile for each object Keep accepts. Keep and Project receive both the
// raw relative path and its pre-split segments so a provider expresses its
// selection and project rules without re-splitting. Sidecars, when set, returns
// the companion objects whose size/mtime/fingerprint fold into the session's
// freshness identity; providers without sidecars leave it nil, and providers
// that derive the project from session content leave Project nil.
type S3SessionScanner struct {
	Agent    AgentType
	Keep     func(rel string, segs []string) bool
	Project  func(rel string, segs []string) string
	Sidecars func(uri string, all []S3Object) []S3Object
}

// s3PrefixScan is the shared S3 discovery body for the
// .../<machine>/raw/<provider> layout. Providers reuse it by supplying an
// S3SessionScanner with their own Keep/Project predicates.
func s3PrefixScan(root string, scan S3SessionScanner) []DiscoveredFile {
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

func s3URIWithLast(parts []string, last string) string {
	out := make([]string, 0, len(parts)+1)
	out = append(out, parts...)
	out = append(out, last)
	return "s3://" + strings.Join(out, "/")
}

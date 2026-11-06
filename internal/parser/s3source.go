// ABOUTME: Reads session JSONL directly from an S3-compatible
// ABOUTME: object store (AWS S3, MinIO, Aliyun OSS, R2, ...) — pure Go, no cgo.
package parser

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// S3Object carries the durable source metadata needed for sync skip checks.
type S3Object struct {
	URI          string
	Size         int64
	LastModified time.Time
	Fingerprint  string
}

var (
	listS3Objects    = listS3
	fetchS3Object    = fetchS3ObjectDefault
	statS3Object     = statS3ObjectDefault
	s3EndpointRegion = regexp.MustCompile(`^s3(?:-fips)?(?:\.dualstack)?[.-]([a-z]+(?:-[a-z]+)+-[0-9]+)\.amazonaws\.com(?:\.cn)?$`)
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
func s3Client(ctx context.Context, bucket string) (*s3.Client, error) {
	endpoint, secure, err := s3EndpointConfig(os.Getenv("AWS_S3_ENDPOINT"))
	if err != nil {
		return nil, err
	}
	scheme := "https://"
	if !secure {
		scheme = "http://"
	}
	u, err := url.Parse(scheme + endpoint)
	if err != nil || u.Hostname() == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("invalid S3 endpoint: %q", endpoint)
	}
	region := os.Getenv("AWS_REGION")
	if region == "" {
		if match := s3EndpointRegion.FindStringSubmatch(u.Hostname()); match != nil {
			region = match[1]
		}
	}
	discoverRegion := region == ""
	if region == "" {
		region = "us-east-1"
	}
	// Keep credentials explicit and leave checksum-mode defaults unset.
	cl := s3.NewFromConfig(aws.Config{
		Region: region, Credentials: s3Credentials(),
	}, func(opts *s3.Options) {
		// AWS, Google, and Aliyun use virtual-hosted buckets. Other custom
		// stores keep path-style addressing.
		if endpoint != "s3.amazonaws.com" {
			opts.BaseEndpoint = new(scheme + endpoint)
			opts.UsePathStyle = !strings.HasSuffix(u.Hostname(), ".amazonaws.com") &&
				!strings.HasSuffix(u.Hostname(), ".amazonaws.com.cn") &&
				!strings.HasSuffix(u.Hostname(), ".aliyuncs.com") &&
				u.Hostname() != "storage.googleapis.com"
		}
	})
	if discoverRegion {
		// Keep bucket-region discovery for operators who omit AWS_REGION.
		location, err := cl.GetBucketLocation(ctx, &s3.GetBucketLocationInput{Bucket: new(bucket)})
		if err != nil {
			apiErr, hasAPI := errors.AsType[smithy.APIError](err)
			resp, hasResponse := errors.AsType[*awshttp.ResponseError](err)
			if !hasAPI || !hasResponse {
				return nil, err
			}
			switch apiErr.ErrorCode() {
			case "AccessDenied", "AuthorizationHeaderMalformed", "InvalidRegion":
				region = resp.Response.Header.Get("X-Amz-Bucket-Region")
			case "NotImplemented":
				switch resp.Response.Header.Get("Server") {
				case "cloudflare":
					region = "us-east-1"
				case "AmazonSnowball":
					region = "snowball"
				default:
					return nil, err
				}
			default:
				return nil, err
			}
		} else {
			region = strings.TrimSpace(string(location.LocationConstraint))
		}
		switch region {
		case "":
			region = "us-east-1"
		case "EU":
			region = "eu-west-1"
		}
		opts := cl.Options()
		opts.Region = region
		cl = s3.New(opts)
	}
	return cl, nil
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

func s3Credentials() aws.CredentialsProvider {
	// Incomplete explicit credentials previously selected anonymous reads.
	if os.Getenv("AWS_ACCESS_KEY_ID") == "" || os.Getenv("AWS_SECRET_ACCESS_KEY") == "" {
		return aws.AnonymousCredentials{}
	}
	return credentials.NewStaticCredentialsProvider(
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
	bucket, prefix := parseS3URI(uri)
	cl, err := s3Client(context.Background(), bucket)
	if err != nil {
		return nil, err
	}
	if prefix != "" {
		prefix = strings.TrimSuffix(prefix, "/") + "/"
	}
	var out []S3Object
	pages := s3.NewListObjectsV2Paginator(cl, &s3.ListObjectsV2Input{
		Bucket: new(bucket), Prefix: new(prefix), EncodingType: types.EncodingTypeUrl,
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(context.Background())
		if err != nil {
			return nil, err
		}
		if aws.ToBool(page.IsTruncated) && aws.ToString(page.NextContinuationToken) == "" {
			return nil, errors.New("truncated S3 listing has no continuation token")
		}
		for _, o := range page.Contents {
			key := aws.ToString(o.Key)
			if page.EncodingType == types.EncodingTypeUrl {
				key, err = url.QueryUnescape(key)
				if err != nil {
					return nil, err
				}
			}
			uri := "s3://" + bucket + "/" + key
			out = append(out, S3Object{
				URI:          uri,
				Size:         aws.ToInt64(o.Size),
				LastModified: aws.ToTime(o.LastModified).Truncate(time.Millisecond),
				Fingerprint:  s3ObjectFingerprint(uri, &s3.HeadObjectOutput{ETag: o.ETag}),
			})
		}
	}
	return out, nil
}

// FetchS3Object opens one s3://bucket/key object for streaming reads. The
// caller owns the returned reader and must close it. Initial request errors
// return here; errors while streaming the response surface on Read.
func FetchS3Object(uri string) (io.ReadCloser, error) {
	return fetchS3Object(uri)
}

func fetchS3ObjectDefault(uri string) (io.ReadCloser, error) {
	bucket, key := parseS3URI(uri)
	cl, err := s3Client(context.Background(), bucket)
	if err != nil {
		return nil, err
	}
	obj, err := cl.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: new(bucket), Key: new(key),
	})
	if err != nil {
		return nil, err
	}
	return obj.Body, nil
}

// StatS3Object returns durable object metadata for an s3:// URI.
func StatS3Object(uri string) (S3Object, error) {
	return statS3Object(uri)
}

func statS3ObjectDefault(uri string) (S3Object, error) {
	bucket, key := parseS3URI(uri)
	cl, err := s3Client(context.Background(), bucket)
	if err != nil {
		return S3Object{}, err
	}
	info, err := cl.HeadObject(context.Background(), &s3.HeadObjectInput{
		Bucket: new(bucket), Key: new(key),
	})
	if err != nil {
		return S3Object{}, err
	}
	return S3Object{
		URI:          uri,
		Size:         aws.ToInt64(info.ContentLength),
		LastModified: aws.ToTime(info.LastModified),
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
// host prefix that remote sync attaches to pulled sessions.
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
	if _, after, ok := strings.CutLast(p, "/"); ok {
		return after
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

func s3ObjectFingerprint(uri string, info *s3.HeadObjectOutput) string {
	parts := []string{
		"etag=" + strings.Trim(aws.ToString(info.ETag), `"`),
		"version=" + aws.ToString(info.VersionId),
		"crc32=" + aws.ToString(info.ChecksumCRC32),
		"crc32c=" + aws.ToString(info.ChecksumCRC32C),
		"sha1=" + aws.ToString(info.ChecksumSHA1),
		"sha256=" + aws.ToString(info.ChecksumSHA256),
		"crc64nvme=" + aws.ToString(info.ChecksumCRC64NVME),
		"md5=" + aws.ToString(info.ChecksumMD5),
		"sha512=" + aws.ToString(info.ChecksumSHA512),
		"xxhash64=" + aws.ToString(info.ChecksumXXHASH64),
		"xxhash3=" + aws.ToString(info.ChecksumXXHASH3),
		"xxhash128=" + aws.ToString(info.ChecksumXXHASH128),
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
	URI               string
	Project           string
	Machine           string
	Size              int64
	MtimeNS           int64
	Fingerprint       string
	TranscriptSize    int64
	TranscriptMtimeNS int64
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
			URI:               file.Path,
			Project:           file.Project,
			Machine:           file.Machine,
			Size:              file.SourceSize,
			MtimeNS:           file.SourceMtime,
			Fingerprint:       file.SourceFingerprint,
			TranscriptSize:    file.TranscriptSize,
			TranscriptMtimeNS: file.TranscriptMtime,
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
			TranscriptSize:    obj.Size,
			TranscriptMtime:   obj.LastModified.UnixNano(),
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

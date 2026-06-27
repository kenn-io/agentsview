package artifact

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// emptyPayloadSHA256 is the hex SHA-256 of the empty string, the payload hash
// SigV4 uses for requests without a body (GET and ListObjectsV2).
const emptyPayloadSHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

var errObjectStore = errors.New("artifact object store request failed")

// IsObjectTarget reports whether target is an S3-compatible object-store URL
// rather than a local folder or HTTP peer target.
func IsObjectTarget(target string) bool {
	return strings.HasPrefix(target, "s3://")
}

// ObjectStoreOptions configures the S3-compatible object-store transport. It
// carries static credentials plus the addressing details needed to talk to AWS
// S3 or a compatible service such as MinIO or Backblaze B2.
type ObjectStoreOptions struct {
	// Endpoint is the service base URL. Empty means real AWS S3, addressed in
	// virtual-host style at https://s3.<region>.amazonaws.com. A non-empty value
	// (host or full URL) forces path-style addressing.
	Endpoint        string
	Region          string
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	// PathStyle forces path-style addressing (/<bucket>/<key>) instead of
	// virtual-host addressing (<bucket>.host/<key>). It is implied by a custom
	// Endpoint.
	PathStyle bool
}

// ObjectStoreOptionsFromEnv reads object-store credentials and addressing from
// the environment, preferring agentsview-specific variables over the standard
// AWS ones for region and endpoint. Region defaults to us-east-1, and a custom
// endpoint forces path-style addressing because MinIO, B2, and local test
// servers do not support virtual-host buckets.
func ObjectStoreOptionsFromEnv() ObjectStoreOptions {
	region := os.Getenv("AGENTSVIEW_S3_REGION")
	if region == "" {
		region = os.Getenv("AWS_REGION")
	}
	if region == "" {
		region = "us-east-1"
	}
	endpoint := os.Getenv("AGENTSVIEW_S3_ENDPOINT")
	pathStyle := os.Getenv("AGENTSVIEW_S3_PATH_STYLE") == "true"
	if endpoint != "" {
		pathStyle = true
	}
	return ObjectStoreOptions{
		Endpoint:        endpoint,
		Region:          region,
		AccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		SessionToken:    os.Getenv("AWS_SESSION_TOKEN"),
		PathStyle:       pathStyle,
	}
}

// s3Transport exchanges artifacts with an S3-compatible bucket using the same
// set-union semantics as the folder and HTTP peer transports: list every object
// under the prefix once, fetch remote-only artifacts into the local store, and
// upload local-only artifacts to the bucket. Content addressing makes "does the
// bucket have X" a name-set comparison, so exchange is stateless and idempotent.
type s3Transport struct {
	bucket    string
	prefix    string // no leading or trailing slash; may be empty
	endpoint  *url.URL
	pathStyle bool
	opts      ObjectStoreOptions
	client    *http.Client
}

func newObjectTransport(target string, opts ObjectStoreOptions) (*s3Transport, error) {
	if !IsObjectTarget(target) {
		return nil, fmt.Errorf("object store target must be s3://: %q", target)
	}
	rest := strings.TrimPrefix(target, "s3://")
	bucket, prefix, _ := strings.Cut(rest, "/")
	prefix = strings.Trim(prefix, "/")
	if bucket == "" {
		return nil, fmt.Errorf("object store target is missing a bucket: %q", target)
	}
	if opts.AccessKeyID == "" || opts.SecretAccessKey == "" {
		return nil, errors.New("object store target requires AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY")
	}
	region := opts.Region
	if region == "" {
		region = "us-east-1"
		opts.Region = region
	}
	var endpoint *url.URL
	if opts.Endpoint == "" {
		endpoint = &url.URL{Scheme: "https", Host: "s3." + region + ".amazonaws.com"}
	} else {
		raw := opts.Endpoint
		if !strings.Contains(raw, "://") {
			raw = "https://" + raw
		}
		u, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("parsing object store endpoint %q: %w", opts.Endpoint, err)
		}
		if u.Host == "" {
			return nil, fmt.Errorf("object store endpoint is missing a host: %q", opts.Endpoint)
		}
		endpoint = &url.URL{Scheme: u.Scheme, Host: u.Host}
		opts.PathStyle = true
	}
	return &s3Transport{
		bucket:    bucket,
		prefix:    prefix,
		endpoint:  endpoint,
		pathStyle: opts.PathStyle,
		opts:      opts,
		client:    &http.Client{Timeout: httpTransportTimeout},
	}, nil
}

// Prepare verifies the bucket is reachable and the credentials are accepted
// before the local export runs, so a wrong endpoint or key fails fast.
func (t *s3Transport) Prepare(_ string) error {
	if _, err := t.listPage(context.Background(), "", 1); err != nil {
		return fmt.Errorf("connecting to object store: %w", err)
	}
	return nil
}

func (t *s3Transport) Exchange(ctx context.Context, localRoot string) error {
	remote, err := t.listRemote(ctx)
	if err != nil {
		return fmt.Errorf("listing object store artifacts: %w", err)
	}
	if err := t.pull(ctx, localRoot, remote); err != nil {
		return fmt.Errorf("fetching artifacts from object store: %w", err)
	}
	if err := t.push(ctx, localRoot, remote); err != nil {
		return fmt.Errorf("publishing artifacts to object store: %w", err)
	}
	return nil
}

// pull fetches every artifact the bucket holds that is missing locally.
func (t *s3Transport) pull(ctx context.Context, localRoot string, remote map[string]OriginArtifactIndex) error {
	origins := make([]string, 0, len(remote))
	for origin := range remote {
		origins = append(origins, origin)
	}
	sort.Strings(origins)
	for _, origin := range origins {
		local, err := ListArtifacts(localRoot, origin)
		if err != nil {
			return err
		}
		for _, item := range missingItems(remote[origin], local) {
			data, err := t.getObject(ctx, t.objectKey(origin, item.kind, item.name))
			if err != nil {
				return err
			}
			if _, err := WriteArtifact(localRoot, origin, item.kind, item.name, data); err != nil {
				return err
			}
		}
	}
	return nil
}

// push uploads every local artifact the bucket is missing.
func (t *s3Transport) push(ctx context.Context, localRoot string, remote map[string]OriginArtifactIndex) error {
	origins, err := ListOrigins(localRoot)
	if err != nil {
		return err
	}
	for _, origin := range origins {
		local, err := ListArtifacts(localRoot, origin)
		if err != nil {
			return err
		}
		for _, item := range missingItems(local, remote[origin]) {
			art, err := ReadArtifact(localRoot, origin, item.kind, item.name)
			if err != nil {
				return err
			}
			if err := t.putObject(ctx, t.objectKey(origin, item.kind, item.name), art.Data); err != nil {
				return err
			}
		}
	}
	return nil
}

// listRemote walks the whole prefix once via paginated ListObjectsV2 and groups
// the valid artifact keys into a per-origin index, the enumeration both pull and
// push compare against.
func (t *s3Transport) listRemote(ctx context.Context) (map[string]OriginArtifactIndex, error) {
	indexes := map[string]*OriginArtifactIndex{}
	token := ""
	for {
		result, err := t.listPage(ctx, token, 0)
		if err != nil {
			return nil, err
		}
		for _, c := range result.Contents {
			t.indexKey(indexes, c.Key)
		}
		if !result.IsTruncated || result.NextContinuationToken == "" {
			break
		}
		token = result.NextContinuationToken
	}
	out := make(map[string]OriginArtifactIndex, len(indexes))
	for origin, idx := range indexes {
		out[origin] = *idx
	}
	return out, nil
}

// indexKey parses one object key into origin/kind/name and records it in the
// per-origin index, skipping keys that do not have exactly three path segments
// under the prefix or whose origin, kind, and name fail the same validation
// ListArtifacts applies locally.
func (t *s3Transport) indexKey(indexes map[string]*OriginArtifactIndex, key string) {
	rel := key
	if t.prefix != "" {
		p := t.prefix + "/"
		if !strings.HasPrefix(key, p) {
			return
		}
		rel = key[len(p):]
	}
	parts := strings.Split(rel, "/")
	if len(parts) != 3 {
		return
	}
	origin, kind, name := parts[0], parts[1], parts[2]
	if validateOriginID(origin) != nil {
		return
	}
	if !validArtifactKindName(kind, name) {
		return
	}
	idx := indexes[origin]
	if idx == nil {
		idx = &OriginArtifactIndex{Origin: origin}
		indexes[origin] = idx
	}
	switch kind {
	case KindCheckpoints:
		idx.Checkpoints = append(idx.Checkpoints, name)
	case KindManifests:
		idx.Manifests = append(idx.Manifests, name)
	case KindSegments:
		idx.Segments = append(idx.Segments, name)
	case KindMeta:
		idx.Meta = append(idx.Meta, name)
	case KindRaw:
		idx.Raw = append(idx.Raw, name)
	}
}

// objectKey builds the bucket key for one artifact, joining the optional prefix
// with origin/kind/name.
func (t *s3Transport) objectKey(origin, kind, name string) string {
	parts := make([]string, 0, 4)
	if t.prefix != "" {
		parts = append(parts, t.prefix)
	}
	parts = append(parts, origin, kind, name)
	return strings.Join(parts, "/")
}

// listBucketResult is the subset of the ListObjectsV2 response we consume.
type listBucketResult struct {
	XMLName               xml.Name `xml:"ListBucketResult"`
	IsTruncated           bool     `xml:"IsTruncated"`
	NextContinuationToken string   `xml:"NextContinuationToken"`
	Contents              []struct {
		Key string `xml:"Key"`
	} `xml:"Contents"`
}

// listPage performs one ListObjectsV2 request. A maxKeys of 0 leaves the limit
// to the server default; the reachability check in Prepare uses 1.
func (t *s3Transport) listPage(ctx context.Context, token string, maxKeys int) (listBucketResult, error) {
	q := url.Values{}
	q.Set("list-type", "2")
	if t.prefix != "" {
		q.Set("prefix", t.prefix+"/")
	}
	if token != "" {
		q.Set("continuation-token", token)
	}
	if maxKeys > 0 {
		q.Set("max-keys", strconv.Itoa(maxKeys))
	}
	req, err := t.newRequest(ctx, http.MethodGet, "", q, nil)
	if err != nil {
		return listBucketResult{}, err
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return listBucketResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return listBucketResult{}, t.statusError(resp)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return listBucketResult{}, err
	}
	var result listBucketResult
	if err := xml.Unmarshal(body, &result); err != nil {
		return listBucketResult{}, fmt.Errorf("decoding object store listing: %w", err)
	}
	return result, nil
}

func (t *s3Transport) getObject(ctx context.Context, key string) ([]byte, error) {
	req, err := t.newRequest(ctx, http.MethodGet, key, nil, nil)
	if err != nil {
		return nil, err
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, t.statusError(resp)
	}
	return io.ReadAll(resp.Body)
}

func (t *s3Transport) putObject(ctx context.Context, key string, data []byte) error {
	req, err := t.newRequest(ctx, http.MethodPut, key, nil, data)
	if err != nil {
		return err
	}
	// Write-once: refuse to overwrite an existing object. Artifacts are
	// immutable and content-addressed, so a collision means either a harmless
	// re-upload of identical bytes or a genuine origin-ID conflict that must be
	// surfaced, not silently merged.
	req.Header.Set("If-None-Match", "*")
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	case http.StatusPreconditionFailed:
		_, _ = io.Copy(io.Discard, resp.Body)
		return t.reconcileExistingObject(ctx, key, data)
	default:
		return t.statusError(resp)
	}
}

// reconcileExistingObject handles a write-once collision: identical content is
// an accepted duplicate, differing content is a conflict.
func (t *s3Transport) reconcileExistingObject(ctx context.Context, key string, data []byte) error {
	existing, err := t.getObject(ctx, key)
	if err != nil {
		return fmt.Errorf("comparing conflicting object %s: %w", key, err)
	}
	if bytes.Equal(existing, data) {
		return nil
	}
	return fmt.Errorf("%w: object %s already exists with different content", errObjectStore, key)
}

// newRequest builds and signs one object-store request. An empty key targets the
// bucket itself (used for listing). A nil body sends no payload and signs the
// empty-payload hash; a non-nil body signs its real SHA-256.
func (t *s3Transport) newRequest(ctx context.Context, method, key string, query url.Values, body []byte) (*http.Request, error) {
	host := t.endpoint.Host
	var rawPath string
	if t.pathStyle {
		rawPath = "/" + t.bucket
		if key != "" {
			rawPath += "/" + key
		}
	} else {
		host = t.bucket + "." + t.endpoint.Host
		rawPath = "/" + key
	}
	u := &url.URL{
		Scheme:   t.endpoint.Scheme,
		Host:     host,
		Path:     rawPath,
		RawPath:  s3EncodePath(rawPath),
		RawQuery: canonicalQueryString(query),
	}
	var reader io.Reader
	payloadHash := emptyPayloadSHA256
	if body != nil {
		reader = bytes.NewReader(body)
		payloadHash = hashHex(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	signRequest(req, payloadHash, t.opts, time.Now())
	return req, nil
}

func (t *s3Transport) statusError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, httpTransportMaxErrLen))
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		return fmt.Errorf("%w: %s", errObjectStore, resp.Status)
	}
	return fmt.Errorf("%w: %s: %s", errObjectStore, resp.Status, msg)
}

// signRequest applies AWS Signature Version 4 for the "s3" service to req,
// setting the x-amz-date, x-amz-content-sha256, optional x-amz-security-token,
// and Authorization headers. payloadSHA256Hex must be the hex SHA-256 of the
// request body (the empty-payload hash for bodyless requests).
func signRequest(req *http.Request, payloadSHA256Hex string, opts ObjectStoreOptions, now time.Time) {
	now = now.UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadSHA256Hex)
	if opts.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", opts.SessionToken)
	}

	// Sign the host header plus every x-amz-* header we set. They are already in
	// lowercase canonical form, so we sort by name to build the canonical block.
	type header struct{ name, value string }
	headers := []header{
		{"host", req.URL.Host},
		{"x-amz-content-sha256", payloadSHA256Hex},
		{"x-amz-date", amzDate},
	}
	if opts.SessionToken != "" {
		headers = append(headers, header{"x-amz-security-token", opts.SessionToken})
	}
	sort.Slice(headers, func(i, j int) bool { return headers[i].name < headers[j].name })

	var canonicalHeaders strings.Builder
	signedNames := make([]string, 0, len(headers))
	for _, h := range headers {
		canonicalHeaders.WriteString(h.name)
		canonicalHeaders.WriteByte(':')
		canonicalHeaders.WriteString(strings.TrimSpace(h.value))
		canonicalHeaders.WriteByte('\n')
		signedNames = append(signedNames, h.name)
	}
	signedHeaders := strings.Join(signedNames, ";")

	canonicalRequest := strings.Join([]string{
		req.Method,
		req.URL.EscapedPath(),
		req.URL.RawQuery,
		canonicalHeaders.String(),
		signedHeaders,
		payloadSHA256Hex,
	}, "\n")

	scope := dateStamp + "/" + opts.Region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hashHex([]byte(canonicalRequest)),
	}, "\n")

	signingKey := sigV4SigningKey(opts.SecretAccessKey, dateStamp, opts.Region, "s3")
	signature := hex.EncodeToString(hmacSHA256(signingKey, stringToSign))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		opts.AccessKeyID, scope, signedHeaders, signature,
	))
}

func sigV4SigningKey(secret, dateStamp, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), dateStamp)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	return hmacSHA256(kService, "aws4_request")
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// canonicalQueryString renders query parameters in the sorted, S3-encoded form
// the SigV4 canonical request requires.
func canonicalQueryString(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(q))
	for _, k := range keys {
		values := append([]string(nil), q[k]...)
		sort.Strings(values)
		for _, v := range values {
			parts = append(parts, s3URIEncode(k)+"="+s3URIEncode(v))
		}
	}
	return strings.Join(parts, "&")
}

// s3EncodePath URI-encodes a path, encoding each segment per S3 rules while
// preserving the '/' separators.
func s3EncodePath(path string) string {
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		segments[i] = s3URIEncode(seg)
	}
	return strings.Join(segments, "/")
}

// s3URIEncode percent-encodes s following the AWS SigV4 rules: only the
// unreserved characters A-Z a-z 0-9 - _ . ~ pass through unencoded, and every
// other byte is encoded as uppercase %XX (notably space becomes %20, not '+').
func s3URIEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		const hexDigits = "0123456789ABCDEF"
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&0xf])
	}
	return b.String()
}

package artifact

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const emptyPayloadSHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

var errObjectStore = errors.New("artifact object store request failed")

func IsObjectTarget(target string) bool { return strings.HasPrefix(target, "s3://") }

type ObjectStoreOptions struct {
	Endpoint              string
	Region                string
	AccessKeyID           string
	SecretAccessKey       string
	SessionToken          string
	AllowInsecureEndpoint bool
	PathStyle             bool
}

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
	allowInsecure := false
	switch strings.ToLower(strings.TrimSpace(os.Getenv("AGENTSVIEW_ALLOW_INSECURE_S3_ENDPOINT"))) {
	case "1", "true", "yes":
		allowInsecure = true
	}
	if endpoint != "" {
		pathStyle = true
	}
	return ObjectStoreOptions{
		Endpoint: endpoint, Region: region,
		AccessKeyID:           os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretAccessKey:       os.Getenv("AWS_SECRET_ACCESS_KEY"),
		SessionToken:          os.Getenv("AWS_SESSION_TOKEN"),
		AllowInsecureEndpoint: allowInsecure,
		PathStyle:             pathStyle,
	}
}

type s3Transport struct {
	bucket    string
	prefix    string
	endpoint  *url.URL
	pathStyle bool
	opts      ObjectStoreOptions
	client    *http.Client
}

func newObjectTransport(target string, opts ObjectStoreOptions) (*s3Transport, error) {
	if !IsObjectTarget(target) {
		return nil, errors.New("object store target must use s3://")
	}
	parsed, err := url.Parse(target)
	if err != nil || parsed == nil {
		return nil, errors.New("object store target is invalid")
	}
	if parsed.Host == "" {
		return nil, errors.New("object store target is missing a bucket")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("object store target must not contain credentials, query, or fragment")
	}
	rest := strings.TrimPrefix(target, "s3://")
	bucket, prefix, _ := strings.Cut(rest, "/")
	prefix = strings.Trim(prefix, "/")
	if bucket == "" {
		return nil, errors.New("object store target is missing a bucket")
	}
	if opts.AccessKeyID == "" || opts.SecretAccessKey == "" {
		return nil, errors.New("object store target requires AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY")
	}
	if opts.Region == "" {
		opts.Region = "us-east-1"
	}
	var endpoint *url.URL
	if opts.Endpoint == "" {
		endpoint = &url.URL{Scheme: "https", Host: "s3." + opts.Region + ".amazonaws.com"}
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
		scheme := strings.ToLower(u.Scheme)
		switch scheme {
		case "https":
		case "http":
			if !opts.AllowInsecureEndpoint && !isLoopbackEndpointHost(u.Hostname()) {
				return nil, fmt.Errorf("insecure S3 endpoint %q requires HTTPS or AGENTSVIEW_ALLOW_INSECURE_S3_ENDPOINT", opts.Endpoint)
			}
		default:
			return nil, fmt.Errorf("object store endpoint %q uses unsupported scheme %q; only http and https are allowed", opts.Endpoint, u.Scheme)
		}
		endpoint = &url.URL{Scheme: scheme, Host: u.Host}
		opts.PathStyle = true
	}
	return &s3Transport{
		bucket: bucket, prefix: prefix, endpoint: endpoint,
		pathStyle: opts.PathStyle, opts: opts,
		client: &http.Client{
			Timeout: httpTransportTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func isLoopbackEndpointHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if address, _, ok := strings.Cut(host, "%"); ok {
		host = address
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (t *s3Transport) Prepare(ctx context.Context, _ ArtifactStore) error {
	if err := requireTransportContext(ctx); err != nil {
		return err
	}
	if _, err := t.listPage(ctx, t.prefixWithSlash(), "", "", 1); err != nil {
		return fmt.Errorf("connecting to object store: %w", err)
	}
	return nil
}

func (t *s3Transport) Exchange(ctx context.Context, local ArtifactStore) (retErr error) {
	if err := validateTransportStore(ctx, local); err != nil {
		return err
	}
	defer func() {
		if retErr == nil {
			NotifyArtifactBatch(ctx, local)
		}
	}()
	remoteIt := &s3OriginIterator{transport: t}
	localIt := &storeOriginIterator{store: local}
	defer func() { retErr = errors.Join(retErr, localIt.Close()) }()
	remoteOrigin, remoteOK, err := remoteIt.Next(ctx)
	if err != nil {
		return fmt.Errorf("listing object store origins: %w", err)
	}
	localOrigin, localOK, err := localIt.Next(ctx)
	if err != nil {
		return fmt.Errorf("listing local artifact origins: %w", err)
	}
	for remoteOK || localOK {
		var origin string
		switch {
		case !localOK || (remoteOK && remoteOrigin < localOrigin):
			origin = remoteOrigin
			remoteOrigin, remoteOK, err = remoteIt.Next(ctx)
		case !remoteOK || localOrigin < remoteOrigin:
			origin = localOrigin
			localOrigin, localOK, err = localIt.Next(ctx)
		default:
			origin = remoteOrigin
			remoteOrigin, remoteOK, err = remoteIt.Next(ctx)
			if err == nil {
				localOrigin, localOK, err = localIt.Next(ctx)
			}
		}
		if err != nil {
			return fmt.Errorf("advancing object store origins: %w", err)
		}
		if err := t.exchangeOrigin(ctx, local, origin); err != nil {
			return err
		}
	}
	return nil
}

func (t *s3Transport) exchangeOrigin(
	ctx context.Context, local ArtifactStore, origin string,
) error {
	for _, kind := range transportKinds {
		if err := t.exchangeKind(ctx, local, origin, kind); err != nil {
			return fmt.Errorf("exchanging %s artifacts for %s: %w", kind, origin, err)
		}
	}
	return nil
}

func (t *s3Transport) exchangeKind(
	ctx context.Context, local ArtifactStore, origin string, kind Kind,
) (retErr error) {
	localIt := &singleKindStoreIterator{store: local, origin: origin, kind: kind}
	remoteIt := &s3WireIterator{transport: t, origin: origin, kind: kind}
	defer func() { retErr = errors.Join(retErr, localIt.Close()) }()
	localEntry, localOK, err := localIt.Next(ctx)
	if err != nil {
		return err
	}
	remoteWire, remoteOK, err := remoteIt.Next(ctx)
	if err != nil {
		return err
	}
	for localOK || remoteOK {
		if err := ctx.Err(); err != nil {
			return err
		}
		var localWire WireRef
		if localOK {
			localWire, err = ToWireRef(localEntry.Ref)
			if err != nil {
				return err
			}
		}
		switch {
		case !localOK || (remoteOK && remoteWire.Name < localWire.Name):
			if err := t.receiveObject(ctx, local, remoteWire); err != nil {
				return err
			}
			remoteWire, remoteOK, err = remoteIt.Next(ctx)
			if err != nil {
				return err
			}
		case !remoteOK || localWire.Name < remoteWire.Name:
			if err := t.putEntry(ctx, local, localEntry); err != nil {
				return err
			}
			localEntry, localOK, err = localIt.Next(ctx)
			if err != nil {
				return err
			}
		default:
			repaired, err := repairQueuedTransportArtifact(ctx, local, remoteWire,
				func(consume func(io.Reader) error) error {
					return t.withObject(ctx,
						t.objectKey(remoteWire.Origin, string(remoteWire.Kind), remoteWire.Name),
						consume)
				})
			if err != nil {
				return err
			}
			if !repaired && kind == KindCheckpoints {
				if err := t.compareCheckpoint(ctx, local, localEntry, remoteWire); err != nil {
					return err
				}
			}
			localEntry, localOK, err = localIt.Next(ctx)
			if err != nil {
				return err
			}
			remoteWire, remoteOK, err = remoteIt.Next(ctx)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (t *s3Transport) prefixWithSlash() string {
	if t.prefix == "" {
		return ""
	}
	return t.prefix + "/"
}

func (t *s3Transport) receiveObject(
	ctx context.Context, local ArtifactStore, wire WireRef,
) error {
	key := t.objectKey(wire.Origin, string(wire.Kind), wire.Name)
	return t.withObject(ctx, key, func(body io.Reader) error {
		_, err := CreateFromWire(ctx, local, wire, body, transportWireLimits(wire.Kind))
		if !errors.Is(err, ErrArtifactInvalid) && !errors.Is(err, ErrArtifactCorrupt) {
			return err
		}
		log.Printf("artifact: detected corrupt artifact %s/%s/%s in object store: %v",
			wire.Origin, wire.Kind, wire.Name, err)
		if deleteErr := t.deleteObject(ctx, key); deleteErr != nil {
			log.Printf("artifact: deleting corrupt object %s: %v", key, deleteErr)
		}
		return nil
	})
}

func (t *s3Transport) compareCheckpoint(
	ctx context.Context, store ArtifactStore, local Entry, wire WireRef,
) error {
	return t.withObject(ctx, t.objectKey(wire.Origin, string(wire.Kind), wire.Name), func(body io.Reader) error {
		return compareOrRepairCheckpoint(ctx, store, local, wire, body,
			"remote", wire.Origin, local.Ref.Name)
	})
}

func (t *s3Transport) putEntry(
	ctx context.Context, local ArtifactStore, entry Entry,
) (retErr error) {
	spool, size, payloadHash, err := spoolWireArtifact(ctx, local, entry)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, closeAndRemoveTransportSpool(spool)) }()
	wire, err := ToWireRef(entry.Ref)
	if err != nil {
		return err
	}
	return t.putObject(ctx, t.objectKey(wire.Origin, string(wire.Kind), wire.Name), spool, size, payloadHash)
}

func (t *s3Transport) objectKey(origin, kind, name string) string {
	parts := make([]string, 0, 4)
	if t.prefix != "" {
		parts = append(parts, t.prefix)
	}
	return strings.Join(append(parts, origin, kind, name), "/")
}

type listBucketResult struct {
	XMLName               xml.Name `xml:"ListBucketResult"`
	IsTruncated           bool     `xml:"IsTruncated"`
	NextContinuationToken string   `xml:"NextContinuationToken"`
	Contents              []struct {
		Key string `xml:"Key"`
	} `xml:"Contents"`
	CommonPrefixes []struct {
		Prefix string `xml:"Prefix"`
	} `xml:"CommonPrefixes"`
}

func (t *s3Transport) listPage(
	ctx context.Context, prefix, delimiter, token string, maxKeys int,
) (listBucketResult, error) {
	q := url.Values{}
	q.Set("list-type", "2")
	if prefix != "" {
		q.Set("prefix", prefix)
	}
	if delimiter != "" {
		q.Set("delimiter", delimiter)
	}
	if token != "" {
		q.Set("continuation-token", token)
	}
	if maxKeys > 0 {
		q.Set("max-keys", strconv.Itoa(maxKeys))
	}
	req, err := t.newRequest(ctx, http.MethodGet, "", q, nil, 0, emptyPayloadSHA256)
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
	body, err := readTransportPage(resp.Body)
	if err != nil {
		return listBucketResult{}, err
	}
	var result listBucketResult
	if err := xml.Unmarshal(body, &result); err != nil {
		return listBucketResult{}, fmt.Errorf("decoding object store listing: %w", err)
	}
	if maxKeys > 0 && len(result.Contents)+len(result.CommonPrefixes) > maxKeys {
		return listBucketResult{}, fmt.Errorf("%w: object store page exceeds requested max-keys", ErrArtifactInvalid)
	}
	if result.IsTruncated && result.NextContinuationToken == "" {
		return listBucketResult{}, fmt.Errorf("%w: truncated object store page is missing a continuation token", ErrArtifactInvalid)
	}
	for index := 1; index < len(result.Contents); index++ {
		if result.Contents[index-1].Key >= result.Contents[index].Key {
			return listBucketResult{}, fmt.Errorf("%w: object store keys are not strictly increasing", ErrArtifactInvalid)
		}
	}
	for index := 1; index < len(result.CommonPrefixes); index++ {
		if result.CommonPrefixes[index-1].Prefix >= result.CommonPrefixes[index].Prefix {
			return listBucketResult{}, fmt.Errorf("%w: object store prefixes are not strictly increasing", ErrArtifactInvalid)
		}
	}
	return result, nil
}

func (t *s3Transport) withObject(
	ctx context.Context, key string, consume func(io.Reader) error,
) error {
	req, err := t.newRequest(ctx, http.MethodGet, key, nil, nil, 0, emptyPayloadSHA256)
	if err != nil {
		return err
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrArtifactNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return t.statusError(resp)
	}
	return consume(resp.Body)
}

func (t *s3Transport) putObject(
	ctx context.Context,
	key string,
	body io.ReadSeeker,
	size int64,
	payloadHash string,
) error {
	req, err := t.newRequest(ctx, http.MethodPut, key, nil, body, size, payloadHash)
	if err != nil {
		return err
	}
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
		return t.reconcileExistingObject(ctx, key, size, payloadHash)
	default:
		return t.statusError(resp)
	}
}

func (t *s3Transport) reconcileExistingObject(
	ctx context.Context, key string, size int64, payloadHash string,
) error {
	return t.withObject(ctx, key, func(existing io.Reader) error {
		hasher := sha256.New()
		read, err := io.Copy(hasher, &wireContextReader{ctx: ctx, reader: existing})
		if err != nil {
			return fmt.Errorf("comparing conflicting object %s: %w", key, err)
		}
		if read == size && hex.EncodeToString(hasher.Sum(nil)) == payloadHash {
			return nil
		}
		return fmt.Errorf("%w: object %s already exists with different content", errObjectStore, key)
	})
}

func (t *s3Transport) deleteObject(ctx context.Context, key string) error {
	if t.endpoint.Scheme != "https" {
		return fmt.Errorf("refusing to delete object through insecure S3 endpoint %q", t.endpoint.String())
	}
	req, err := t.newRequest(ctx, http.MethodDelete, key, nil, nil, 0, emptyPayloadSHA256)
	if err != nil {
		return err
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent, http.StatusNotFound:
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	default:
		return t.statusError(resp)
	}
}

func (t *s3Transport) newRequest(
	ctx context.Context,
	method, key string,
	query url.Values,
	body io.ReadSeeker,
	size int64,
	payloadHash string,
) (*http.Request, error) {
	host := t.endpoint.Host
	rawPath := "/" + key
	if t.pathStyle {
		rawPath = "/" + t.bucket
		if key != "" {
			rawPath += "/" + key
		}
	} else {
		host = t.bucket + "." + t.endpoint.Host
	}
	u := &url.URL{Scheme: t.endpoint.Scheme, Host: host, Path: rawPath, RawPath: s3EncodePath(rawPath), RawQuery: canonicalQueryString(query)}
	var reader io.Reader
	var readerAt io.ReaderAt
	if body != nil {
		readerAt = bodyReaderAt(body)
		reader = io.NewSectionReader(readerAt, 0, size)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = size
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(io.NewSectionReader(readerAt, 0, size)), nil
		}
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

type singleKindStoreIterator struct {
	store  ArtifactStore
	origin string
	kind   Kind
	cursor Cursor
	items  []Entry
	next   int
	done   bool
	guard  boundedCursorCycleGuard
}

func (i *singleKindStoreIterator) Next(ctx context.Context) (Entry, bool, error) {
	for {
		if i.next < len(i.items) {
			entry := i.items[i.next]
			i.next++
			return entry, true, nil
		}
		if i.done {
			return Entry{}, false, nil
		}
		page, err := i.store.List(ctx, i.origin, i.kind, i.cursor, transportPageSize)
		if err != nil {
			return Entry{}, false, err
		}
		i.items, i.next, i.cursor = page.Items, 0, page.Next
		if i.cursor == "" {
			i.done = true
		} else if i.guard.Observe(i.cursor) {
			return Entry{}, false, errors.New("artifact list cursor cycle")
		}
	}
}

func (i *singleKindStoreIterator) Close() error {
	return releaseArtifactCursor(i.store, &i.cursor)
}

type s3OriginIterator struct {
	transport  *s3Transport
	token      string
	items      []string
	next       int
	done       bool
	guard      boundedCursorCycleGuard
	lastPrefix string
}

func (i *s3OriginIterator) Next(ctx context.Context) (string, bool, error) {
	for {
		if i.next < len(i.items) {
			origin := i.items[i.next]
			i.next++
			return origin, true, nil
		}
		if i.done {
			return "", false, nil
		}
		base := i.transport.prefixWithSlash()
		page, err := i.transport.listPage(ctx, base, "/", i.token, transportPageSize)
		if err != nil {
			return "", false, err
		}
		i.items = i.items[:0]
		for _, common := range page.CommonPrefixes {
			if !strings.HasPrefix(common.Prefix, base) ||
				!strings.HasSuffix(common.Prefix, "/") ||
				common.Prefix <= i.lastPrefix {
				return "", false, fmt.Errorf("%w: object store origin prefixes are malformed or not strictly increasing", ErrArtifactInvalid)
			}
			i.lastPrefix = common.Prefix
			rel := strings.TrimSuffix(strings.TrimPrefix(common.Prefix, base), "/")
			if strings.Contains(rel, "/") || validateOriginID(rel) != nil {
				return "", false, fmt.Errorf("%w: malformed artifact origin prefix %q", ErrArtifactInvalid, common.Prefix)
			}
			i.items = append(i.items, rel)
		}
		i.next = 0
		if !page.IsTruncated {
			i.done = true
		} else {
			if i.guard.Observe(Cursor(page.NextContinuationToken)) {
				return "", false, errors.New("object store continuation token cycle")
			}
			i.token = page.NextContinuationToken
		}
	}
}

type s3WireIterator struct {
	transport *s3Transport
	origin    string
	kind      Kind
	token     string
	items     []WireRef
	next      int
	done      bool
	guard     boundedCursorCycleGuard
	lastKey   string
}

func (i *s3WireIterator) Next(ctx context.Context) (WireRef, bool, error) {
	for {
		if i.next < len(i.items) {
			item := i.items[i.next]
			i.next++
			return item, true, nil
		}
		if i.done {
			return WireRef{}, false, nil
		}
		prefix := i.transport.objectKey(i.origin, string(i.kind), "")
		page, err := i.transport.listPage(ctx, prefix, "", i.token, transportPageSize)
		if err != nil {
			return WireRef{}, false, err
		}
		i.items = i.items[:0]
		for _, content := range page.Contents {
			if !strings.HasPrefix(content.Key, prefix) || content.Key <= i.lastKey {
				return WireRef{}, false, fmt.Errorf("%w: object store keys are not strictly increasing within the requested prefix", ErrArtifactInvalid)
			}
			i.lastKey = content.Key
			name := strings.TrimPrefix(content.Key, prefix)
			if strings.Contains(name, "/") || name == "" {
				return WireRef{}, false, fmt.Errorf("%w: malformed artifact object key %q", ErrArtifactInvalid, content.Key)
			}
			ref, err := FromWireRef(i.origin, i.kind, name)
			if err != nil {
				return WireRef{}, false, fmt.Errorf("%w: malformed artifact object key %q: %v", ErrArtifactInvalid, content.Key, err)
			}
			wire, err := ToWireRef(ref)
			if err != nil {
				return WireRef{}, false, err
			}
			i.items = append(i.items, wire)
		}
		i.next = 0
		if !page.IsTruncated {
			i.done = true
		} else {
			if i.guard.Observe(Cursor(page.NextContinuationToken)) {
				return WireRef{}, false, errors.New("object store continuation token cycle")
			}
			i.token = page.NextContinuationToken
		}
	}
}

func signRequest(req *http.Request, payloadSHA256Hex string, opts ObjectStoreOptions, now time.Time) {
	now = now.UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadSHA256Hex)
	if opts.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", opts.SessionToken)
	}
	type header struct{ name, value string }
	headers := []header{{"host", req.URL.Host}, {"x-amz-content-sha256", payloadSHA256Hex}, {"x-amz-date", amzDate}}
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
	canonicalRequest := strings.Join([]string{req.Method, req.URL.EscapedPath(), req.URL.RawQuery, canonicalHeaders.String(), signedHeaders, payloadSHA256Hex}, "\n")
	scope := dateStamp + "/" + opts.Region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, scope, hashHex([]byte(canonicalRequest))}, "\n")
	signingKey := sigV4SigningKey(opts.SecretAccessKey, dateStamp, opts.Region, "s3")
	signature := hex.EncodeToString(hmacSHA256(signingKey, stringToSign))
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s", opts.AccessKeyID, scope, signedHeaders, signature))
}

func sigV4SigningKey(secret, dateStamp, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), dateStamp)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	return hmacSHA256(kService, "aws4_request")
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	_, _ = h.Write([]byte(data))
	return h.Sum(nil)
}

func canonicalQueryString(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for key := range q {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(q))
	for _, key := range keys {
		values := append([]string(nil), q[key]...)
		sort.Strings(values)
		for _, value := range values {
			parts = append(parts, s3URIEncode(key)+"="+s3URIEncode(value))
		}
	}
	return strings.Join(parts, "&")
}

func s3EncodePath(path string) string {
	segments := strings.Split(path, "/")
	for index, segment := range segments {
		segments[index] = s3URIEncode(segment)
	}
	return strings.Join(segments, "/")
}

func s3URIEncode(value string) string {
	var builder strings.Builder
	for index := 0; index < len(value); index++ {
		char := value[index]
		if (char >= 'A' && char <= 'Z') || (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '-' || char == '_' || char == '.' || char == '~' {
			builder.WriteByte(char)
			continue
		}
		builder.WriteByte('%')
		const digits = "0123456789ABCDEF"
		builder.WriteByte(digits[char>>4])
		builder.WriteByte(digits[char&0xf])
	}
	return builder.String()
}

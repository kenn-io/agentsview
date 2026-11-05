package parser

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestS3TransportListPagination(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("Authorization"), "empty explicit credentials must allow anonymous ingest")
		assert.Equal(t, "url", r.URL.Query().Get("encoding-type"))
		assert.Equal(t, "host/raw/codex/", r.URL.Query().Get("prefix"))
		assert.Empty(t, r.URL.Query().Get("delimiter"), "nested objects must be listed")
		switch r.URL.Query().Get("continuation-token") {
		case "":
			_, _ = io.WriteString(w, `<ListBucketResult><EncodingType>url</EncodingType><IsTruncated>true</IsTruncated><NextContinuationToken>next+page</NextContinuationToken><Contents><Key>host/raw/codex/nested%01%20%2B%25/one.jsonl</Key><Size>3</Size><LastModified>2026-01-02T03:04:05.123456Z</LastModified><ETag>&quot;first&quot;</ETag></Contents></ListBucketResult>`)
		case "next+page":
			_, _ = io.WriteString(w, `<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>host/raw/codex/two.jsonl</Key><Size>4</Size><LastModified>2026-01-02T03:04:06Z</LastModified><ETag>&quot;second&quot;</ETag></Contents></ListBucketResult>`)
		default:
			assert.Fail(t, "unexpected continuation token", "%s", r.URL.RawQuery)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	t.Setenv("AWS_S3_ENDPOINT", server.URL)
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_SESSION_TOKEN", "")

	objects, err := listS3("s3://bucket/host/raw/codex/")
	require.NoError(t, err)
	assert.Equal(t, []S3Object{
		{
			URI: "s3://bucket/host/raw/codex/nested\x01 +%/one.jsonl", Size: 3,
			LastModified: time.Date(2026, 1, 2, 3, 4, 5, 123000000, time.UTC),
			Fingerprint:  "s3-meta:s3://bucket/host/raw/codex/nested\x01 +%/one.jsonl\x00etag=first",
		},
		{
			URI: "s3://bucket/host/raw/codex/two.jsonl", Size: 4,
			LastModified: time.Date(2026, 1, 2, 3, 4, 6, 0, time.UTC),
			Fingerprint:  "s3-meta:s3://bucket/host/raw/codex/two.jsonl\x00etag=second",
		},
	}, objects)
}

func TestS3TransportReadStat(t *testing.T) {
	const uri = "s3://bucket/proj:name/session name%.jsonl"
	version := "v1"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "3")
		w.Header().Set("Last-Modified", "Fri, 02 Jan 2026 03:04:05 GMT")
		w.Header().Set("ETag", `"etag"`)
		w.Header().Set("X-Amz-Version-Id", version)
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, "abc")
		} else {
			w.Header().Set("X-Amz-Checksum-Sha256", "checksum")
		}
	}))
	defer server.Close()
	t.Setenv("AWS_S3_ENDPOINT", server.URL)
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "access-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret-key")

	reader, err := FetchS3Object(uri)
	require.NoError(t, err)
	data, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	assert.Equal(t, "abc", string(data))

	obj, err := StatS3Object(uri)
	require.NoError(t, err)
	assert.Equal(t, S3Object{
		URI: uri, Size: 3, LastModified: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Fingerprint: "s3-meta:" + uri + "\x00etag=etag\x00sha256=checksum\x00version=v1",
	}, obj)
	version = "v2"
	changed, err := StatS3Object(uri)
	require.NoError(t, err)
	assert.NotEqual(t, obj.Fingerprint, changed.Fingerprint, "version alone must invalidate a same-size, same-mtime object")
}

func TestS3TransportRejectsIncompleteListing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `<ListBucketResult><IsTruncated>true</IsTruncated><Contents><Key>prefix/session.jsonl</Key><Size>3</Size></Contents></ListBucketResult>`)
	}))
	defer server.Close()
	t.Setenv("AWS_S3_ENDPOINT", server.URL)
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "access-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret-key")

	objects, err := listS3("s3://bucket/prefix")
	require.Error(t, err, "a truncated page without a continuation token must not silently hide sessions")
	assert.Nil(t, objects)
}

func TestS3TransportDiscoversRegion(t *testing.T) {
	for _, tc := range []struct {
		name, location, region, code, hint string
	}{
		{name: "regional bucket", location: "eu-west-2", region: "eu-west-2"},
		{name: "legacy EU location", location: "EU", region: "eu-west-1"},
		{name: "region hint", code: "AuthorizationHeaderMalformed", hint: "eu-west-2", region: "eu-west-2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Has("location") {
					if tc.code != "" {
						w.Header().Set("X-Amz-Bucket-Region", tc.hint)
						w.WriteHeader(http.StatusForbidden)
						_, _ = fmt.Fprintf(w, "<Error><Code>%s</Code></Error>", tc.code)
						return
					}
					_, _ = fmt.Fprintf(w, "<LocationConstraint>%s</LocationConstraint>", tc.location)
					return
				}
				assert.Contains(t, r.Header.Get("Authorization"), "/"+tc.region+"/s3/aws4_request")
				_, _ = io.WriteString(w, `<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>`)
			}))
			defer server.Close()
			t.Setenv("AWS_S3_ENDPOINT", server.URL)
			t.Setenv("AWS_REGION", "")
			t.Setenv("AWS_ACCESS_KEY_ID", "access-key")
			t.Setenv("AWS_SECRET_ACCESS_KEY", "secret-key")

			_, err := listS3("s3://bucket/prefix")
			require.NoError(t, err)
		})
	}
}

func TestS3ClientEndpointAddressing(t *testing.T) {
	for _, tc := range []struct {
		endpoint, host, path, region string
	}{
		{"oss-cn-shenzhen.aliyuncs.com", "bucket.oss-cn-shenzhen.aliyuncs.com", "/", "us-east-1"},
		{"account.r2.cloudflarestorage.com", "account.r2.cloudflarestorage.com", "/bucket", "us-east-1"},
		{"s3.eu-west-2.amazonaws.com", "bucket.s3.eu-west-2.amazonaws.com", "/", ""},
	} {
		t.Run(tc.endpoint, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, tc.host, r.Host)
				assert.Equal(t, tc.path, r.URL.Path)
				if tc.region == "" {
					assert.Contains(t, r.Header.Get("Authorization"), "/eu-west-2/s3/aws4_request")
				}
				_, _ = io.WriteString(w, `<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>`)
			}))
			defer server.Close()
			t.Setenv("AWS_S3_ENDPOINT", "http://"+tc.endpoint)
			t.Setenv("AGENTSVIEW_ALLOW_INSECURE_S3_ENDPOINT", "true")
			t.Setenv("AWS_REGION", tc.region)
			t.Setenv("AWS_ACCESS_KEY_ID", "access-key")
			t.Setenv("AWS_SECRET_ACCESS_KEY", "secret-key")
			// Known regional endpoints must work without a location lookup.
			ctx := t.Context()
			if tc.region == "" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			cl, err := s3Client(ctx, "bucket")
			require.NoError(t, err)
			// Route these synthetic provider names to the local fixture only.
			transport := server.Client().Transport.(*http.Transport).Clone()
			defer transport.CloseIdleConnections()
			transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
			}
			_, err = cl.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{Bucket: new("bucket")}, func(opts *s3.Options) {
				opts.HTTPClient = &http.Client{Transport: transport}
			})
			require.NoError(t, err)
		})
	}
}

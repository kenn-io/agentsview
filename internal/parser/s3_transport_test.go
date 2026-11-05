package parser

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestS3TransportListPagination(t *testing.T) {
	pages := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/bucket", strings.TrimSuffix(r.URL.Path, "/"))
		assert.Equal(t, "2", r.URL.Query().Get("list-type"))
		assert.Equal(t, "url", r.URL.Query().Get("encoding-type"))
		assert.Equal(t, "host/raw/codex/", r.URL.Query().Get("prefix"))
		assert.Empty(t, r.URL.Query().Get("delimiter"), "nested objects must be listed")
		assert.Contains(t, r.Header.Get("Authorization"), "Credential=access-key/")
		assert.Contains(t, r.Header.Get("Authorization"), "/test-region/s3/aws4_request")
		assert.Equal(t, "session-token", r.Header.Get("X-Amz-Security-Token"))
		pages++
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
	t.Setenv("AWS_REGION", "test-region")
	t.Setenv("AWS_ACCESS_KEY_ID", "access-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret-key")
	t.Setenv("AWS_SESSION_TOKEN", "session-token")

	objects, err := listS3("s3://bucket/host/raw/codex/")
	require.NoError(t, err)
	assert.Equal(t, 2, pages)
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
		assert.Equal(t, "/bucket/proj:name/session name%.jsonl", r.URL.Path)
		assert.Empty(t, r.Header.Get("If-Match"), "session objects are mutable")
		assert.Empty(t, r.Header.Get("X-Amz-Checksum-Mode"), "reads must not require extra KMS checksum permissions")
		w.Header().Set("Content-Length", "3")
		w.Header().Set("Last-Modified", "Fri, 02 Jan 2026 03:04:05 GMT")
		w.Header().Set("ETag", `"etag"`)
		w.Header().Set("X-Amz-Version-Id", version)
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, "abc")
		} else {
			assert.Equal(t, http.MethodHead, r.Method)
			for _, algorithm := range []string{"crc32", "crc32c", "crc64nvme", "md5", "sha1", "sha256", "sha512", "xxhash64", "xxhash3", "xxhash128"} {
				w.Header().Set("X-Amz-Checksum-"+algorithm, algorithm+"-value")
			}
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
		Fingerprint: "s3-meta:" + uri + "\x00crc32=crc32-value\x00crc32c=crc32c-value\x00crc64nvme=crc64nvme-value\x00etag=etag\x00md5=md5-value\x00sha1=sha1-value\x00sha256=sha256-value\x00sha512=sha512-value\x00version=v1\x00xxhash128=xxhash128-value\x00xxhash3=xxhash3-value\x00xxhash64=xxhash64-value",
	}, obj)
	unchanged, err := StatS3Object(uri)
	require.NoError(t, err)
	assert.Equal(t, obj, unchanged)
	version = "v2"
	changed, err := StatS3Object(uri)
	require.NoError(t, err)
	assert.NotEqual(t, obj.Fingerprint, changed.Fingerprint, "version alone must invalidate a same-size, same-mtime object")
}

func TestS3TransportErrors(t *testing.T) {
	for _, code := range []string{"NoSuchKey", "NoSuchBucket", "AccessDenied"} {
		t.Run(code, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/xml")
				status := http.StatusNotFound
				if code == "AccessDenied" {
					status = http.StatusForbidden
				}
				w.WriteHeader(status)
				_, _ = fmt.Fprintf(w, "<Error><Code>%s</Code><Message>synthetic error</Message></Error>", code)
			}))
			defer server.Close()
			t.Setenv("AWS_S3_ENDPOINT", server.URL)
			t.Setenv("AWS_REGION", "us-east-1")
			t.Setenv("AWS_ACCESS_KEY_ID", "access-key")
			t.Setenv("AWS_SECRET_ACCESS_KEY", "secret-key")

			reader, err := FetchS3Object("s3://bucket/session.jsonl")
			require.Error(t, err, "AWS GetObject reports initial errors before returning a reader")
			assert.Nil(t, reader)
			var apiErr smithy.APIError
			require.ErrorAs(t, err, &apiErr)
			assert.Equal(t, code, apiErr.ErrorCode())

			_, err = StatS3Object("s3://bucket/session.jsonl")
			require.ErrorAs(t, err, &apiErr)
			if code == "AccessDenied" {
				assert.Equal(t, "Forbidden", apiErr.ErrorCode())
			} else {
				// HEAD has no XML body, so the SDK exposes a generic 404.
				assert.Equal(t, "NotFound", apiErr.ErrorCode())
			}

			_, err = listS3("s3://bucket/prefix")
			require.ErrorAs(t, err, &apiErr)
			assert.Equal(t, code, apiErr.ErrorCode())
		})
	}
}

func TestS3TransportReadFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "10")
		_, _ = io.WriteString(w, "short")
	}))
	defer server.Close()
	t.Setenv("AWS_S3_ENDPOINT", server.URL)
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "access-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret-key")

	reader, err := FetchS3Object("s3://bucket/session.jsonl")
	require.NoError(t, err)
	defer reader.Close()
	_, err = io.ReadAll(reader)
	assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
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
		{name: "default location", region: "us-east-1"},
		{name: "location denied", code: "AccessDenied", region: "us-east-1"},
		{name: "region hint", code: "AuthorizationHeaderMalformed", hint: "eu-west-2", region: "eu-west-2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			locations := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Has("location") {
					locations++
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
			assert.Equal(t, 1, locations)
		})
	}
}

func TestS3ClientEndpointAddressing(t *testing.T) {
	for _, tc := range []struct {
		endpoint, host, path, region string
	}{
		{"oss-cn-shenzhen.aliyuncs.com", "bucket.oss-cn-shenzhen.aliyuncs.com", "/", "us-east-1"},
		{"storage.googleapis.com", "bucket.storage.googleapis.com", "/", "us-east-1"},
		{"account.r2.cloudflarestorage.com", "account.r2.cloudflarestorage.com", "/bucket", "us-east-1"},
		{"s3.eu-west-2.amazonaws.com", "bucket.s3.eu-west-2.amazonaws.com", "/", ""},
		{"s3.dualstack.eu-west-2.amazonaws.com", "bucket.s3.dualstack.eu-west-2.amazonaws.com", "/", ""},
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
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
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

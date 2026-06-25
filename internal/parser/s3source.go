// ABOUTME: Reads Claude/Codex session JSONL directly from an S3-compatible
// ABOUTME: object store (AWS S3, MinIO, Aliyun OSS, R2, ...) — pure Go, no cgo.
package parser

import (
	"context"
	"io"
	"os"
	"slices"
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
//	                   An "http://" prefix selects insecure transport.
//
// Returning an error here means an s3:// source simply yields nothing,
// so a misconfigured store never aborts the local sync.
func s3Client() (*minio.Client, error) {
	endpoint := os.Getenv("AWS_S3_ENDPOINT")
	secure := true
	switch {
	case endpoint == "":
		endpoint = "s3.amazonaws.com"
	case strings.HasPrefix(endpoint, "http://"):
		secure, endpoint = false, strings.TrimPrefix(endpoint, "http://")
	case strings.HasPrefix(endpoint, "https://"):
		endpoint = strings.TrimPrefix(endpoint, "https://")
	}
	return minio.New(endpoint, &minio.Options{
		Creds:  s3Credentials(),
		Secure: secure,
		Region: os.Getenv("AWS_REGION"),
	})
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

// StatCodexS3Session returns metadata for a Codex rollout plus the adjacent
// session_index.jsonl title index when it exists.
func StatCodexS3Session(uri string) (S3Object, error) {
	obj, err := statS3Object(uri)
	if err != nil {
		return S3Object{}, err
	}
	indexURI, ok := CodexS3SessionIndexURI(uri)
	if !ok {
		return obj, nil
	}
	index, err := statS3Object(indexURI)
	if err != nil {
		return obj, nil
	}
	return foldS3ObjectMetadata(obj, index), nil
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
	}, nil
}

func s3RelativePath(root, uri string) (string, bool) {
	prefix := strings.TrimSuffix(root, "/") + "/"
	rel := strings.TrimPrefix(uri, prefix)
	return rel, rel != uri
}

// s3MachineFromRoot derives the source machine from an s3:// session root
// laid out as .../<machine>/raw/<claude|codex>, i.e. the path segment
// immediately preceding the agent raw segment. Returns "" when not found, so
// callers fall back to the agentsview host machine name. This mirrors the host
// prefix that SSH remote sync attaches to pulled sessions.
func s3MachineFromRoot(root string) string {
	// segs[0] is the bucket, so "raw" must be at index >= 2 for the
	// preceding segment to be a machine directory rather than the bucket.
	segs := strings.Split(strings.TrimPrefix(root, "s3://"), "/")
	for i := len(segs) - 2; i > 1; i-- {
		if segs[i] == "raw" && isS3AgentRootSegment(segs[i+1]) {
			return segs[i-1]
		}
	}
	return ""
}

func isS3AgentRootSegment(seg string) bool {
	return seg == "claude" || seg == "codex"
}

func foldClaudeS3SidecarMetadata(
	obj S3Object, list func(root string) []S3Object,
) S3Object {
	for _, root := range claudeS3SidecarRoots(obj.URI) {
		for _, sidecar := range list(root) {
			obj.Size += sidecar.Size
			if sidecar.LastModified.After(obj.LastModified) {
				obj.LastModified = sidecar.LastModified
			}
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
	return obj
}

// discoverClaudeS3 lists Claude session JSONL under an s3:// projects root,
// mirroring DiscoverClaudeProjects' selection rules:
//   - top-level <project>/<uuid>.jsonl   (skip names starting "agent-")
//   - subagents .../subagents/.../agent-*.jsonl
//
// Project is the first path segment under the root (e.g. "-home-user-proj").
func discoverClaudeS3(root string) []DiscoveredFile {
	objects, err := listS3Objects(root)
	if err != nil {
		return nil
	}
	machine := s3MachineFromRoot(root)
	var out []DiscoveredFile
	for _, obj := range objects {
		rel, ok := s3RelativePath(root, obj.URI)
		if !ok || !strings.HasSuffix(rel, ".jsonl") {
			continue
		}
		segs := strings.Split(rel, "/")
		if len(segs) < 2 {
			continue
		}
		project := segs[0]
		base := segs[len(segs)-1]
		isSubagent := len(segs) >= 4 && segs[2] == "subagents"
		if isSubagent {
			if !strings.HasPrefix(base, "agent-") {
				continue
			}
		} else if len(segs) != 2 || strings.HasPrefix(base, "agent-") ||
			slices.Contains(segs, "subagents") {
			continue
		}
		source := foldClaudeS3SidecarMetadata(
			obj, func(sidecarRoot string) []S3Object {
				prefix := strings.TrimSuffix(sidecarRoot, "/") + "/"
				var matched []S3Object
				for _, candidate := range objects {
					if strings.HasPrefix(candidate.URI, prefix) {
						matched = append(matched, candidate)
					}
				}
				return matched
			},
		)
		out = append(out, DiscoveredFile{
			Path:        obj.URI,
			Project:     project,
			Agent:       AgentClaude,
			Machine:     machine,
			SourceSize:  source.Size,
			SourceMtime: source.LastModified.UnixNano(),
		})
	}
	return out
}

// discoverCodexS3 lists Codex rollout-*.jsonl under an s3:// sessions root
// (any depth — Codex nests under 2026/MM/DD/). Project is derived from
// session content, so it is left empty here, as in the local path.
func discoverCodexS3(root string) []DiscoveredFile {
	objects, err := listS3Objects(root)
	if err != nil {
		return nil
	}
	machine := s3MachineFromRoot(root)
	var out []DiscoveredFile
	indexCache := make(map[string]S3Object)
	for _, obj := range objects {
		rel, ok := s3RelativePath(root, obj.URI)
		if !ok {
			continue
		}
		base := rel[strings.LastIndex(rel, "/")+1:]
		if !isCodexSessionFilename(base) {
			continue
		}
		source := foldCodexS3IndexMetadata(obj, indexCache)
		out = append(out, DiscoveredFile{
			Path:        obj.URI,
			Agent:       AgentCodex,
			Machine:     machine,
			SourceSize:  source.Size,
			SourceMtime: source.LastModified.UnixNano(),
		})
	}
	return out
}

func foldCodexS3IndexMetadata(
	obj S3Object, indexCache map[string]S3Object,
) S3Object {
	indexURI, ok := CodexS3SessionIndexURI(obj.URI)
	if !ok {
		return obj
	}
	index, ok := indexCache[indexURI]
	if !ok {
		var err error
		index, err = statS3Object(indexURI)
		if err != nil {
			return obj
		}
		indexCache[indexURI] = index
	}
	return foldS3ObjectMetadata(obj, index)
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

	for i := 1; i < len(parts)-1; i++ {
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

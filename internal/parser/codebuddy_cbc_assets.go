package parser

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"go.kenn.io/agentsview/internal/assets"
)

// maxCodeBuddyImageBytes bounds one attached image read into the asset
// store, matching the largest image the CodeBuddy clients accept.
const maxCodeBuddyImageBytes = 64 << 20

// codebuddyBlobAssets resolves CBC image references against the CodeBuddy
// home's content-addressed blob store, and a converted conversation's
// codebuddy-asset:// images against its 4.x assets directory. Only
// <home>/blobs/<prefix>/<sha>.<ext> and plain file names inside that assets
// directory are read; absolute paths recorded in transcripts are never opened.
type codebuddyBlobAssets struct {
	home         string
	assetsDir    string
	legacyAssets string
	// readOnly computes references and reads verified copies in assetsDir
	// without saving images there, for report-only parses.
	readOnly bool
	// storeFailed records an image that was read but could not be saved in
	// the asset store, so the parse is retried once the store is writable.
	storeFailed *atomic.Bool
}

// codebuddyImageOutcome tells apart why an attached image has no link: its
// original is gone, or the asset store could not save it. Only a failed save
// is worth retrying while the transcript is unchanged.
type codebuddyImageOutcome uint8

const (
	codebuddyImageMissing codebuddyImageOutcome = iota
	codebuddyImageSaved
	codebuddyImageStoreFailed
)

// ImageReference returns the asset reference for one attached image. Without
// an asset directory, or in read-only mode, it still computes the
// content-addressed reference so the parse stays deterministic; writing
// happens only when assetsDir is set and the parse may write.
// An image the store cannot save reports false and is recorded in
// storeFailed.
func (a codebuddyBlobAssets) ImageReference(ref codebuddyImageRef) (string, bool) {
	asset, outcome := a.resolve(ref)
	if outcome == codebuddyImageStoreFailed && a.storeFailed != nil {
		a.storeFailed.Store(true)
	}
	return asset, outcome == codebuddyImageSaved
}

func (a codebuddyBlobAssets) resolve(ref codebuddyImageRef) (string, codebuddyImageOutcome) {
	body, mediaType, ok := a.read(ref)
	if !ok {
		if asset, ok := a.storedBlob(ref); ok {
			return asset, codebuddyImageSaved
		}
		return "", codebuddyImageMissing
	}
	if a.assetsDir == "" || a.readOnly {
		asset, err := assets.Reference(mediaType, body)
		if err != nil {
			return "", codebuddyImageMissing
		}
		return asset, codebuddyImageSaved
	}
	asset, _, err := assets.Put(a.assetsDir, mediaType, body)
	if err != nil {
		return "", codebuddyImageStoreFailed
	}
	return asset, codebuddyImageSaved
}

// storedBlob finds the archived copy of a blob-store image whose original is
// gone. CodeBuddy names each blob by the SHA-256 of its bytes, which is also
// the asset store's object name, so a copy saved by an earlier import is
// found and verified by that digest. An image never archived stays missing.
func (a codebuddyBlobAssets) storedBlob(ref codebuddyImageRef) (string, bool) {
	blob, ok := codebuddyBlobID(ref.blobID)
	if !ok || a.assetsDir == "" {
		return "", false
	}
	root, err := os.OpenRoot(a.assetsDir)
	if err != nil {
		return "", false
	}
	defer root.Close()
	for _, ext := range codebuddyBlobExtensions(ref.mime) {
		if ext == ".jpeg" {
			// The asset store saves JPEG images as .jpg.
			continue
		}
		if assets.IsCompleteObject(root, blob+ext, -1, blob) {
			return "asset://" + blob + ext, true
		}
	}
	return "", false
}

// codebuddyBlobID returns a reference's blob id when it names a blob-store
// object: 64 lowercase hex digits.
func codebuddyBlobID(id string) (string, bool) {
	blob := strings.ToLower(id)
	if len(blob) != 64 || strings.Trim(blob, "0123456789abcdef") != "" {
		return "", false
	}
	return blob, true
}

func (a codebuddyBlobAssets) read(ref codebuddyImageRef) ([]byte, string, bool) {
	if strings.HasPrefix(ref.dataURL, "data:") {
		return codebuddyDecodeDataURL(ref.dataURL)
	}
	if name, ok := strings.CutPrefix(ref.assetURI, "codebuddy-asset://assets/"); ok {
		return a.readLegacyAsset(name)
	}
	blob, ok := codebuddyBlobID(ref.blobID)
	if !ok || a.home == "" {
		return nil, "", false
	}
	dir := filepath.Join(a.home, "blobs", blob[:2])
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, "", false
	}
	defer root.Close()
	for _, ext := range codebuddyBlobExtensions(ref.mime) {
		if body, mediaType, ok := codebuddyReadImage(root, blob+ext); ok {
			return body, mediaType, true
		}
	}
	return nil, "", false
}

// readLegacyAsset reads one file by plain name from a converted
// conversation's 4.x assets directory.
func (a codebuddyBlobAssets) readLegacyAsset(name string) ([]byte, string, bool) {
	if a.legacyAssets == "" || name == "" || !filepath.IsLocal(name) ||
		strings.ContainsAny(name, "/\\") {
		return nil, "", false
	}
	root, err := os.OpenRoot(a.legacyAssets)
	if err != nil {
		return nil, "", false
	}
	defer root.Close()
	return codebuddyReadImage(root, name)
}

// codebuddyReadImage reads a bounded image file inside root and reports its
// detected media type.
func codebuddyReadImage(root *os.Root, name string) ([]byte, string, bool) {
	f, err := root.Open(name)
	if err != nil {
		return nil, "", false
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, maxCodeBuddyImageBytes+1))
	if err != nil || len(body) > maxCodeBuddyImageBytes {
		return nil, "", false
	}
	mediaType := http.DetectContentType(body)
	if _, ok := assets.ExtForMediaType(mediaType); !ok {
		return nil, "", false
	}
	return body, mediaType, true
}

// codebuddyBlobExtensions lists the stored extensions to try, preferring the
// one the reference's MIME type names.
func codebuddyBlobExtensions(mime string) []string {
	all := []string{".png", ".jpg", ".jpeg", ".webp", ".gif"}
	preferred := map[string]string{
		"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp",
		"image/gif": ".gif",
	}[strings.ToLower(mime)]
	if preferred == "" {
		return all
	}
	out := []string{preferred}
	for _, ext := range all {
		if ext != preferred {
			out = append(out, ext)
		}
	}
	return out
}

// CodeBuddyReferencedBlobs lists the blob-store files the CBC transcripts
// under projectsRoot reference, so remote sync can carry the images a
// transcript shows without copying the whole store.
func CodeBuddyReferencedBlobs(ctx context.Context, projectsRoot string) ([]string, error) {
	home := filepath.Dir(filepath.Clean(projectsRoot))
	set := newCodeBuddyCBCSourceSet([]string{projectsRoot}, "")
	seen := make(map[string]struct{})
	var out []string
	err := set.DiscoverEach(ctx, func(source SourceRef) error {
		_, path, ok := set.pathFromSource(source)
		if !ok {
			return nil
		}
		ids, err := codebuddyTranscriptBlobIDs(ctx, path)
		if err != nil {
			return err
		}
		for _, blob := range ids {
			if _, dup := seen[blob]; dup {
				continue
			}
			seen[blob] = struct{}{}
			matches, _ := filepath.Glob(filepath.Join(home, "blobs", blob[:2], blob+".*"))
			for _, match := range matches {
				if IsRegularFile(match) {
					out = append(out, match)
				}
			}
		}
		return nil
	})
	slices.Sort(out)
	return out, err
}

var codebuddyBlobRefCache = struct {
	sync.Mutex
	byPath map[string]codebuddyBlobRefs
}{byPath: make(map[string]codebuddyBlobRefs)}

type codebuddyBlobRefs struct {
	size, mtime, ctime int64
	inode              uint64
	ids                []string
}

// codebuddyTranscriptBlobIDs lists the blob ids one transcript references,
// reusing the previous read while the file's size, modification time, change
// time, and inode are unchanged. A rewrite that restores the modification
// time still moves the change time. Without a change time or inode the
// transcript is read every time. A transcript removed mid-scan references
// nothing.
func codebuddyTranscriptBlobIDs(ctx context.Context, path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil //nolint:nilerr // A removed transcript references nothing.
	}
	ctime, verified := codexIndexChangeTime(path, info)
	inode, _ := sourceFileIdentityForPath(path, info)
	key := codebuddyBlobRefs{
		size: info.Size(), mtime: info.ModTime().UnixNano(), ctime: ctime, inode: inode,
	}
	cacheable := verified && inode != 0
	if cacheable {
		codebuddyBlobRefCache.Lock()
		cached, ok := codebuddyBlobRefCache.byPath[path]
		codebuddyBlobRefCache.Unlock()
		if ok && cached.size == key.size && cached.mtime == key.mtime &&
			cached.ctime == key.ctime && cached.inode == key.inode {
			return cached.ids, nil
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil //nolint:nilerr // A removed transcript references nothing.
	}
	defer f.Close()
	lr := newLineReaderContext(ctx, f, maxLineSize)
	defer releaseLineReader(lr)
	var ids []string
	for {
		line, ok := lr.next()
		if !ok {
			break
		}
		if !strings.Contains(line, "image_blob_ref") {
			continue
		}
		for _, m := range codebuddyBlobIDRe.FindAllStringSubmatch(line, -1) {
			if !slices.Contains(ids, m[1]) {
				ids = append(ids, m[1])
			}
		}
	}
	if err := lr.Err(); err != nil {
		return nil, err
	}
	if !cacheable {
		return ids, nil
	}
	key.ids = ids
	codebuddyBlobRefCache.Lock()
	if len(codebuddyBlobRefCache.byPath) >= 1<<16 {
		clear(codebuddyBlobRefCache.byPath)
	}
	codebuddyBlobRefCache.byPath[path] = key
	codebuddyBlobRefCache.Unlock()
	return ids, nil
}

var codebuddyBlobIDRe = regexp.MustCompile(`"blob_id"\s*:\s*"([0-9a-f]{64})"`)

// codebuddyTranscriptBlobPaths lists every blob-store path a transcript's
// images can be read from: each referenced blob under each extension the
// image reader tries. Their availability decides whether an image shows or
// stays a placeholder, so the freshness checks follow these paths.
func codebuddyTranscriptBlobPaths(ctx context.Context, home, path string) ([]string, error) {
	ids, err := codebuddyTranscriptBlobIDs(ctx, path)
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	exts := codebuddyBlobExtensions("")
	paths := make([]string, 0, len(ids)*len(exts))
	for _, id := range ids {
		for _, ext := range exts {
			paths = append(paths, filepath.Join(home, "blobs", id[:2], id+ext))
		}
	}
	return paths, nil
}

func codebuddyDecodeDataURL(url string) ([]byte, string, bool) {
	header, payload, ok := strings.Cut(strings.TrimPrefix(url, "data:"), ",")
	if !ok || !strings.HasSuffix(header, ";base64") {
		return nil, "", false
	}
	if base64.StdEncoding.DecodedLen(len(payload)) > maxCodeBuddyImageBytes {
		return nil, "", false
	}
	body, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, "", false
	}
	mediaType := http.DetectContentType(body)
	if _, ok := assets.ExtForMediaType(mediaType); !ok {
		return nil, "", false
	}
	return body, mediaType, true
}
